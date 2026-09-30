// Package handler 提供静态文件 Range 请求支持的测试。
//
// 覆盖：
//   - Range 头解析（起止、开放结尾、后缀、越界、非法、多区间）
//   - 内存读取 / 文件缓存 / sendfile 三条响应路径下的 206 与 416
//   - If-Range、HEAD、预压缩文件跳过、压缩中间件不压缩 206
//   - 真实 TCP 连接下 sendfile 区间的头部与内容
//
// 作者：xfy
package handler

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/cache"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/middleware/compression"
	"rua.plus/lolly/internal/testutil"
)

func TestParseByteRange(t *testing.T) {
	const size = 100
	tests := []struct {
		name       string
		header     string
		size       int64
		wantState  rangeState
		start, len int64
	}{
		{"起止", "bytes=0-4", size, rangePartial, 0, 5},
		{"单字节", "bytes=5-5", size, rangePartial, 5, 1},
		{"开放结尾", "bytes=90-", size, rangePartial, 90, 10},
		{"结尾越界截断", "bytes=90-999", size, rangePartial, 90, 10},
		{"整个文件", "bytes=0-", size, rangePartial, 0, 100},
		{"后缀", "bytes=-5", size, rangePartial, 95, 5},
		{"后缀大于文件", "bytes=-500", size, rangePartial, 0, 100},
		{"后缀0不可满足", "bytes=-0", size, rangeUnsatisfiable, 0, 0},
		{"起点等于size不可满足", "bytes=100-", size, rangeUnsatisfiable, 0, 0},
		{"起点越界不可满足", "bytes=200-300", size, rangeUnsatisfiable, 0, 0},
		{"空文件不可满足", "bytes=0-4", 0, rangeUnsatisfiable, 0, 0},
		{"空文件后缀不可满足", "bytes=-5", 0, rangeUnsatisfiable, 0, 0},
		{"数值溢出", "bytes=0-99999999999999999999999", size, rangePartial, 0, 100},
		{"单位大小写", "Bytes=0-4", size, rangePartial, 0, 5},
		{"空白", "bytes= 1 - 3 ", size, rangePartial, 1, 3},
		{"start>end忽略", "bytes=5-2", size, rangeNone, 0, 0},
		{"多区间忽略", "bytes=0-1,3-4", size, rangeNone, 0, 0},
		{"非bytes单位忽略", "items=0-4", size, rangeNone, 0, 0},
		{"无等号忽略", "bytes", size, rangeNone, 0, 0},
		{"无横线忽略", "bytes=5", size, rangeNone, 0, 0},
		{"空规格忽略", "bytes=", size, rangeNone, 0, 0},
		{"仅横线忽略", "bytes=-", size, rangeNone, 0, 0},
		{"非数字忽略", "bytes=a-b", size, rangeNone, 0, 0},
		{"负号忽略", "bytes=--5", size, rangeNone, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, length, state := parseByteRange(tt.header, tt.size)
			if state != tt.wantState {
				t.Fatalf("state = %v, want %v", state, tt.wantState)
			}
			if state == rangePartial && (start != tt.start || length != tt.len) {
				t.Errorf("start,len = %d,%d, want %d,%d", start, length, tt.start, tt.len)
			}
		})
	}
}

// rangeFixture 生成 n 字节的可识别内容（byte i = 'a' + i%26）。
func rangeFixture(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

// rangeGet 对 handler 发起带头部的 GET/HEAD 请求并返回上下文。
func rangeGet(t *testing.T, h *StaticHandler, method, path string, hdr map[string]string) *fasthttp.RequestCtx {
	t.Helper()
	ctx := testutil.NewRequestCtx(method, path)
	for k, v := range hdr {
		ctx.Request.Header.Set(k, v)
	}
	h.Handle(ctx)
	return ctx
}

// checkPartial 断言 206 响应的状态、头部与内容。
func checkPartial(t *testing.T, ctx *fasthttp.RequestCtx, want []byte, contentRange string) {
	t.Helper()
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusPartialContent {
		t.Fatalf("status = %d, want 206", got)
	}
	if got := string(ctx.Response.Header.Peek("Content-Range")); got != contentRange {
		t.Errorf("Content-Range = %q, want %q", got, contentRange)
	}
	if got := string(ctx.Response.Header.Peek("Accept-Ranges")); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if body := ctx.Response.Body(); !bytes.Equal(body, want) {
		t.Errorf("body = %q, want %q", body, want)
	}
	// Content-Length 在序列化时由 fasthttp 依据 body 生成，真实头部见 RealConn 测试。
}

// TestStaticRange_Paths 在内存、文件缓存、sendfile 三条路径上验证单区间语义。
func TestStaticRange_Paths(t *testing.T) {
	small := rangeFixture(200)
	large := rangeFixture(MinSendfileSize + 1000)

	tests := []struct {
		name   string
		file   string
		data   []byte
		build  func(root string) *StaticHandler
		warmup bool // 先请求一次以填充缓存
	}{
		{"内存读取", "s.txt", small, func(root string) *StaticHandler {
			return NewStaticHandler(root, "/", nil, false)
		}, false},
		{"文件缓存命中", "s.txt", small, func(root string) *StaticHandler {
			h := NewStaticHandler(root, "/", nil, false)
			h.SetFileCache(cache.NewFileCache(100, 1<<20, time.Minute))
			return h
		}, true},
		{"文件缓存命中+TTL", "s.txt", small, func(root string) *StaticHandler {
			h := NewStaticHandler(root, "/", nil, false)
			h.SetFileCache(cache.NewFileCache(100, 1<<20, time.Minute))
			h.SetCacheTTL(time.Minute)
			return h
		}, true},
		{"sendfile大文件", "l.txt", large, func(root string) *StaticHandler {
			return NewStaticHandler(root, "/", nil, true)
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tt.file), tt.data, 0o644); err != nil {
				t.Fatal(err)
			}
			h := tt.build(root)
			path := "/" + tt.file
			size := len(tt.data)
			if tt.warmup {
				rangeGet(t, h, "GET", path, nil)
			}
			sz := strconv.Itoa(size)

			t.Run("bytes=0-4", func(t *testing.T) {
				checkPartial(t, rangeGet(t, h, "GET", path, map[string]string{"Range": "bytes=0-4"}),
					tt.data[0:5], "bytes 0-4/"+sz)
			})
			t.Run("bytes=-5", func(t *testing.T) {
				checkPartial(t, rangeGet(t, h, "GET", path, map[string]string{"Range": "bytes=-5"}),
					tt.data[size-5:], "bytes "+strconv.Itoa(size-5)+"-"+strconv.Itoa(size-1)+"/"+sz)
			})
			t.Run("bytes=100-", func(t *testing.T) {
				checkPartial(t, rangeGet(t, h, "GET", path, map[string]string{"Range": "bytes=100-"}),
					tt.data[100:], "bytes 100-"+strconv.Itoa(size-1)+"/"+sz)
			})
			t.Run("越界", func(t *testing.T) {
				ctx := rangeGet(t, h, "GET", path, map[string]string{"Range": "bytes=" + sz + "-"})
				if got := ctx.Response.StatusCode(); got != fasthttp.StatusRequestedRangeNotSatisfiable {
					t.Fatalf("status = %d, want 416", got)
				}
				if got := string(ctx.Response.Header.Peek("Content-Range")); got != "bytes */"+sz {
					t.Errorf("Content-Range = %q, want %q", got, "bytes */"+sz)
				}
			})
			t.Run("无Range返回200与Accept-Ranges", func(t *testing.T) {
				ctx := rangeGet(t, h, "GET", path, nil)
				if ctx.Response.StatusCode() != fasthttp.StatusOK {
					t.Fatalf("status = %d, want 200", ctx.Response.StatusCode())
				}
				if got := string(ctx.Response.Header.Peek("Accept-Ranges")); got != "bytes" {
					t.Errorf("Accept-Ranges = %q, want bytes", got)
				}
				if !bytes.Equal(ctx.Response.Body(), tt.data) {
					t.Error("full body mismatch")
				}
			})
			t.Run("多区间与非法回退200", func(t *testing.T) {
				for _, v := range []string{"bytes=0-1,5-6", "bytes=9-3", "foo=1-2", "bytes=x-y"} {
					ctx := rangeGet(t, h, "GET", path, map[string]string{"Range": v})
					if ctx.Response.StatusCode() != fasthttp.StatusOK {
						t.Errorf("Range %q: status = %d, want 200", v, ctx.Response.StatusCode())
					}
					if !bytes.Equal(ctx.Response.Body(), tt.data) {
						t.Errorf("Range %q: want full body", v)
					}
				}
			})
			t.Run("POST忽略Range", func(t *testing.T) {
				ctx := rangeGet(t, h, "POST", path, map[string]string{"Range": "bytes=0-4"})
				if ctx.Response.StatusCode() == fasthttp.StatusPartialContent {
					t.Error("POST must not return 206")
				}
			})
		})
	}
}

func TestStaticRange_IfRange(t *testing.T) {
	root := t.TempDir()
	data := rangeFixture(200)
	p := filepath.Join(root, "f.txt")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, false)

	first := rangeGet(t, h, "GET", "/f.txt", nil)
	etag := string(first.Response.Header.Peek("ETag"))
	lm := string(first.Response.Header.Peek("Last-Modified"))
	if etag == "" || lm == "" {
		t.Fatalf("missing validators: etag=%q lm=%q", etag, lm)
	}

	cases := []struct {
		name    string
		ifRange string
		partial bool
	}{
		{"ETag匹配", etag, true},
		{"ETag不匹配", `"other"`, false},
		{"弱ETag不匹配", "W/" + etag, false},
		{"日期匹配", lm, true},
		{"日期不匹配", "Mon, 02 Jan 2006 15:04:05 GMT", false},
		{"日期非法", "not a date", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := rangeGet(t, h, "GET", "/f.txt", map[string]string{"Range": "bytes=0-4", "If-Range": c.ifRange})
			if c.partial {
				checkPartial(t, ctx, data[:5], "bytes 0-4/200")
				return
			}
			if ctx.Response.StatusCode() != fasthttp.StatusOK || !bytes.Equal(ctx.Response.Body(), data) {
				t.Errorf("status = %d, want full 200", ctx.Response.StatusCode())
			}
		})
	}
}

func TestStaticRange_Head(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), rangeFixture(200), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, false)
	ctx := rangeGet(t, h, "HEAD", "/f.txt", map[string]string{"Range": "bytes=0-4"})
	if ctx.Response.StatusCode() != fasthttp.StatusPartialContent {
		t.Fatalf("status = %d, want 206", ctx.Response.StatusCode())
	}
	if got := string(ctx.Response.Header.Peek("Content-Range")); got != "bytes 0-4/200" {
		t.Errorf("Content-Range = %q", got)
	}
}

// TestStaticRange_SkipsPrecompressed 存在 Range 时不发送预压缩文件，返回原始内容区间。
func TestStaticRange_SkipsPrecompressed(t *testing.T) {
	root := t.TempDir()
	data := rangeFixture(200)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt.gz"), []byte("FAKE-GZIP-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, false)
	h.SetGzipStatic(true, []string{".txt"}, []string{".gz"})

	// 无 Range：仍然发送预压缩文件
	ctx := rangeGet(t, h, "GET", "/a.txt", map[string]string{"Accept-Encoding": "gzip"})
	if got := string(ctx.Response.Header.Peek("Content-Encoding")); got != "gzip" {
		t.Fatalf("baseline Content-Encoding = %q, want gzip (precompressed)", got)
	}

	// 有 Range：返回未压缩内容的区间
	ctx = rangeGet(t, h, "GET", "/a.txt", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-4"})
	checkPartial(t, ctx, data[:5], "bytes 0-4/200")
	if ce := ctx.Response.Header.Peek("Content-Encoding"); len(ce) != 0 {
		t.Errorf("Content-Encoding = %q, want empty for range response", ce)
	}
}

// TestStaticRange_NotModifiedTakesPrecedence 条件请求命中时返回 304，而不是 206。
func TestStaticRange_NotModifiedTakesPrecedence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), rangeFixture(200), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, false)
	etag := string(rangeGet(t, h, "GET", "/f.txt", nil).Response.Header.Peek("ETag"))
	ctx := rangeGet(t, h, "GET", "/f.txt", map[string]string{"Range": "bytes=0-4", "If-None-Match": etag})
	if ctx.Response.StatusCode() != fasthttp.StatusNotModified {
		t.Errorf("status = %d, want 304", ctx.Response.StatusCode())
	}
}

// TestCompression_SkipsPartialContent 压缩中间件不压缩 206，且压缩 200 时撤销 Accept-Ranges。
func TestCompression_SkipsPartialContent(t *testing.T) {
	mw, err := compression.New(&config.CompressionConfig{Type: "gzip", Level: 6, MinSize: 10, Types: []string{"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("hello range "), 50)
	handler := mw.Process(func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.SetContentType("text/plain")
		ctx.Response.Header.Set("Accept-Ranges", "bytes")
		if string(ctx.Request.Header.Peek("Range")) != "" {
			ctx.Response.SetStatusCode(fasthttp.StatusPartialContent)
			ctx.Response.Header.Set("Content-Range", "bytes 0-"+strconv.Itoa(len(body)-1)+"/1000")
		}
		ctx.Response.SetBody(body)
	})

	ctx := testutil.NewRequestCtx("GET", "/")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")
	ctx.Request.Header.Set("Range", "bytes=0-599")
	handler(ctx)
	if ce := ctx.Response.Header.Peek("Content-Encoding"); len(ce) != 0 {
		t.Errorf("206 must not be compressed, got Content-Encoding %q", ce)
	}
	if !bytes.Equal(ctx.Response.Body(), body) {
		t.Error("206 body must be untouched")
	}

	ctx = testutil.NewRequestCtx("GET", "/")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")
	handler(ctx)
	if string(ctx.Response.Header.Peek("Content-Encoding")) != "gzip" {
		t.Fatal("200 should be gzip-compressed")
	}
	if ar := ctx.Response.Header.Peek("Accept-Ranges"); len(ar) != 0 {
		t.Errorf("compressed 200 should drop Accept-Ranges, got %q", ar)
	}
}

// TestStaticRange_RealConnSendfile 通过真实 TCP 连接验证 sendfile 区间响应
// 的头部与内容完整（Content-Length 与实际传输字节一致）。
func TestStaticRange_RealConnSendfile(t *testing.T) {
	root := t.TempDir()
	data := rangeFixture(MinSendfileSize * 3)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &fasthttp.Server{Handler: h.Handle}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Shutdown() }()

	get := func(rng string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/big.bin", nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp, b
	}

	resp, b := get("bytes=1000-1999")
	if resp.StatusCode != 206 || !bytes.Equal(b, data[1000:2000]) || resp.ContentLength != 1000 {
		t.Errorf("status=%d len=%d cl=%d", resp.StatusCode, len(b), resp.ContentLength)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 1000-1999/"+strconv.Itoa(len(data)) {
		t.Errorf("Content-Range = %q", got)
	}
	resp, b = get("bytes=-10")
	if resp.StatusCode != 206 || !bytes.Equal(b, data[len(data)-10:]) {
		t.Errorf("suffix: status=%d body=%q", resp.StatusCode, b)
	}
	resp, b = get("")
	if resp.StatusCode != 200 || !bytes.Equal(b, data) {
		t.Errorf("full: status=%d len=%d", resp.StatusCode, len(b))
	}
	resp, _ = get("bytes=99999999-")
	if resp.StatusCode != 416 {
		t.Errorf("unsatisfiable: status=%d, want 416", resp.StatusCode)
	}
}
