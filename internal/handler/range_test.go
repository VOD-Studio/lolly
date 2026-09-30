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
	"bufio"
	"bytes"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestStaticRange_HeadIgnoresRange HEAD 必须忽略 Range（RFC 9110 14.2：Range 仅对 GET 定义），
// 返回 200 与完整资源的元数据，而不是 206。
func TestStaticRange_HeadIgnoresRange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), rangeFixture(200), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, false)
	ctx := rangeGet(t, h, "HEAD", "/f.txt", map[string]string{"Range": "bytes=0-4"})
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, want 200", ctx.Response.StatusCode())
	}
	if cr := ctx.Response.Header.Peek("Content-Range"); len(cr) != 0 {
		t.Errorf("Content-Range = %q, want empty", cr)
	}
	if got := string(ctx.Response.Header.Peek("Accept-Ranges")); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := headContentLength(t, ctx); got != 200 {
		t.Errorf("Content-Length = %d, want 200", got)
	}
}

// TestStaticRange_HeadOutOfRange HEAD 携带越界 Range 时同样忽略，返回 200 而不是 416。
func TestStaticRange_HeadOutOfRange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), rangeFixture(200), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, false)
	ctx := rangeGet(t, h, "HEAD", "/f.txt", map[string]string{"Range": "bytes=9999-"})
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, want 200 (not 416)", ctx.Response.StatusCode())
	}
	if cr := ctx.Response.Header.Peek("Content-Range"); len(cr) != 0 {
		t.Errorf("Content-Range = %q, want empty", cr)
	}
	if got := headContentLength(t, ctx); got != 200 {
		t.Errorf("Content-Length = %d, want 200", got)
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

// newCompressedStatic 构造 压缩中间件 + 静态处理器 的请求处理链，并写入一个 n 字节文本文件。
func newCompressedStatic(t *testing.T, n int, gzipStatic bool) (fasthttp.RequestHandler, []byte) {
	t.Helper()
	root := t.TempDir()
	data := rangeFixture(n)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, false)
	if gzipStatic {
		if err := os.WriteFile(filepath.Join(root, "f.txt.gz"), []byte("FAKE-GZIP-CONTENT"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.SetGzipStatic(true, []string{".txt"}, []string{".gz"})
	}
	mw, err := compression.New(&config.CompressionConfig{Type: "gzip", Level: 6, MinSize: 10, Types: []string{"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	return mw.Process(h.Handle), data
}

func doChain(handler fasthttp.RequestHandler, method string, hdr map[string]string) *fasthttp.RequestCtx {
	ctx := testutil.NewRequestCtx(method, "/f.txt")
	for k, v := range hdr {
		ctx.Request.Header.Set(k, v)
	}
	handler(ctx)
	return ctx
}

// TestStaticETag_DifferentPerEncoding 不同内容编码的表示携带不同的强 ETag。
func TestStaticETag_DifferentPerEncoding(t *testing.T) {
	for _, gzipStatic := range []bool{false, true} {
		name := "动态gzip"
		if gzipStatic {
			name = "预压缩gz"
		}
		t.Run(name, func(t *testing.T) {
			handler, _ := newCompressedStatic(t, 300, gzipStatic)
			id := doChain(handler, "GET", nil)
			gz := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip"})
			if string(gz.Response.Header.Peek("Content-Encoding")) != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", gz.Response.Header.Peek("Content-Encoding"))
			}
			idTag := string(id.Response.Header.Peek("ETag"))
			gzTag := string(gz.Response.Header.Peek("ETag"))
			if idTag == "" || gzTag == "" {
				t.Fatalf("missing ETag: identity=%q gzip=%q", idTag, gzTag)
			}
			if idTag == gzTag {
				t.Fatalf("gzip and identity share ETag %q", idTag)
			}
			if gzTag[0] != '"' || gzTag[len(gzTag)-1] != '"' || gzTag != idTag[:len(idTag)-1]+`-gzip"` {
				t.Errorf("gzip ETag = %q, want strong tag %q", gzTag, idTag[:len(idTag)-1]+`-gzip"`)
			}
		})
	}
}

// TestStaticIfRange_CompressedETagFallsBackToFull 客户端用压缩响应的 ETag 做 If-Range 时，
// 不能得到未压缩字节的 206，而必须回退完整 200（避免拼接出损坏的文件）。
func TestStaticIfRange_CompressedETagFallsBackToFull(t *testing.T) {
	for _, gzipStatic := range []bool{false, true} {
		name := "动态gzip"
		if gzipStatic {
			name = "预压缩gz"
		}
		t.Run(name, func(t *testing.T) {
			handler, data := newCompressedStatic(t, 300, gzipStatic)
			gz := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip"})
			gzTag := string(gz.Response.Header.Peek("ETag"))

			ctx := doChain(handler, "GET", map[string]string{"Range": "bytes=0-4", "If-Range": gzTag})
			if ctx.Response.StatusCode() != fasthttp.StatusOK {
				t.Fatalf("status = %d, want 200", ctx.Response.StatusCode())
			}
			if !bytes.Equal(ctx.Response.Body(), data) {
				t.Errorf("body len = %d, want full %d bytes", len(ctx.Response.Body()), len(data))
			}
			if cr := ctx.Response.Header.Peek("Content-Range"); len(cr) != 0 {
				t.Errorf("Content-Range = %q, want empty", cr)
			}

			// 带 Accept-Encoding 的续传请求同样回退到完整 200
			ctx = doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-4", "If-Range": gzTag})
			if ctx.Response.StatusCode() != fasthttp.StatusOK {
				t.Errorf("with Accept-Encoding: status = %d, want 200", ctx.Response.StatusCode())
			}
		})
	}
}

// TestStaticIfRange_IdentityETagPartial identity 表示的 ETag 通过 If-Range，返回 206。
func TestStaticIfRange_IdentityETagPartial(t *testing.T) {
	handler, data := newCompressedStatic(t, 300, true)
	idTag := string(doChain(handler, "GET", nil).Response.Header.Peek("ETag"))
	ctx := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-4", "If-Range": idTag})
	checkPartial(t, ctx, data[:5], "bytes 0-4/300")
	if got := string(ctx.Response.Header.Peek("ETag")); got != idTag {
		t.Errorf("206 ETag = %q, want identity %q", got, idTag)
	}
}

// encStaticOpts 配置 newEncStatic 构造的“静态处理器 + 压缩中间件”链。
type encStaticOpts struct {
	size       int      // f.txt 大小
	body       []byte   // 非空时作为 f.txt 内容（覆盖 size 生成的可识别文本）
	sendfile   bool     // 启用 sendfile（配合 >= MinSendfileSize 的文件覆盖零拷贝路径）
	gz, br     bool     // 是否提供预压缩 f.txt.gz / f.txt.br
	algorithm  string   // 压缩中间件类型："gzip"（默认）或 "both"
	minSize    int      // 压缩中间件 min_size（默认 10）
	types      []string // 可压缩 MIME（默认 text/plain）
	fileCache  bool     // 启用文件缓存（覆盖 tryServeFromFileCache 路径）
	noCompress bool     // 链上不放压缩中间件
}

// newEncStatic 构造静态处理器（可选预压缩文件、文件缓存）并包上压缩中间件。
func newEncStatic(t *testing.T, o encStaticOpts) fasthttp.RequestHandler {
	t.Helper()
	root := t.TempDir()
	content := o.body
	if content == nil {
		content = rangeFixture(o.size)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, o.sendfile)
	var exts []string
	if o.gz {
		if err := os.WriteFile(filepath.Join(root, "f.txt.gz"), []byte("FAKE-GZIP-CONTENT"), 0o644); err != nil {
			t.Fatal(err)
		}
		exts = append(exts, ".gz")
	}
	if o.br {
		if err := os.WriteFile(filepath.Join(root, "f.txt.br"), []byte("FAKE-BR-CONTENT"), 0o644); err != nil {
			t.Fatal(err)
		}
		exts = append([]string{".br"}, exts...)
	}
	if len(exts) > 0 {
		h.SetGzipStatic(true, []string{".txt"}, exts)
	}
	if o.fileCache {
		h.SetFileCache(cache.NewFileCache(100, 1<<20, time.Minute))
		h.SetCacheTTL(time.Minute)
	}
	if o.noCompress {
		return h.Handle
	}
	if o.algorithm == "" {
		o.algorithm = "gzip"
	}
	if o.minSize == 0 {
		o.minSize = 10
	}
	if o.types == nil {
		o.types = []string{"text/plain"}
	}
	mw, err := compression.New(&config.CompressionConfig{Type: o.algorithm, Level: 6, MinSize: o.minSize, Types: o.types})
	if err != nil {
		t.Fatal(err)
	}
	return mw.Process(h.Handle)
}

// TestStaticIfNoneMatch_EncodingMatrix If-None-Match 只与本次响应实际选用的表示的 ETag 比较：
//
//	identity 请求 + identity 标签 -> 304（回显 identity ETag）
//	identity 请求 + 压缩标签      -> 200（identity 正文，ETag 为 identity）
//	压缩请求     + 压缩标签      -> 304（回显压缩 ETag）
//	压缩请求     + identity 标签 -> 200（压缩正文，ETag 为压缩变体）
func TestStaticIfNoneMatch_EncodingMatrix(t *testing.T) {
	cases := []struct {
		name string
		opts encStaticOpts
		enc  string // 压缩请求的 Accept-Encoding
		want string // 期望的 Content-Encoding
	}{
		{"动态gzip", encStaticOpts{size: 300}, "gzip", "gzip"},
		{"动态gzip/文件缓存", encStaticOpts{size: 300, fileCache: true}, "gzip", "gzip"},
		{"动态br", encStaticOpts{size: 300, algorithm: "both"}, "br, gzip", "br"},
		{"预压缩gz", encStaticOpts{size: 300, gz: true}, "gzip", "gzip"},
		{"预压缩br", encStaticOpts{size: 300, gz: true, br: true}, "br, gzip", "br"},
		{"预压缩gz/文件缓存", encStaticOpts{size: 300, gz: true, fileCache: true}, "gzip", "gzip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newEncStatic(t, tc.opts)
			// 文件缓存路径需要先预热一次，后续请求才会走 tryServeFromFileCache。
			doChain(handler, "GET", nil)

			id := doChain(handler, "GET", nil)
			idTag := string(id.Response.Header.Peek("ETag"))
			encHdr := map[string]string{"Accept-Encoding": tc.enc}
			ec := doChain(handler, "GET", encHdr)
			encTag := string(ec.Response.Header.Peek("ETag"))
			if got := string(ec.Response.Header.Peek("Content-Encoding")); got != tc.want {
				t.Fatalf("Content-Encoding = %q, want %q", got, tc.want)
			}
			if idTag == "" || encTag == "" || idTag == encTag {
				t.Fatalf("ETag 应存在且不同: identity=%q encoded=%q", idTag, encTag)
			}
			wantEncTag := idTag[:len(idTag)-1] + "-" + tc.want + `"`
			if encTag != wantEncTag {
				t.Fatalf("encoded ETag = %q, want %q", encTag, wantEncTag)
			}

			with := func(hdr map[string]string, inm string) *fasthttp.RequestCtx {
				h := map[string]string{"If-None-Match": inm}
				maps.Copy(h, hdr)
				return doChain(handler, "GET", h)
			}
			check := func(name string, ctx *fasthttp.RequestCtx, status int, wantTag, wantCE string) {
				t.Helper()
				if got := ctx.Response.StatusCode(); got != status {
					t.Errorf("%s: status = %d, want %d", name, got, status)
				}
				if got := string(ctx.Response.Header.Peek("ETag")); got != wantTag {
					t.Errorf("%s: ETag = %q, want %q", name, got, wantTag)
				}
				if got := string(ctx.Response.Header.Peek("Content-Encoding")); status == fasthttp.StatusOK && got != wantCE {
					t.Errorf("%s: Content-Encoding = %q, want %q", name, got, wantCE)
				}
				if status == fasthttp.StatusNotModified && len(ctx.Response.Body()) != 0 {
					t.Errorf("%s: 304 不应有正文", name)
				}
			}
			check("identity请求+identity标签", with(nil, idTag), fasthttp.StatusNotModified, idTag, "")
			check("identity请求+压缩标签", with(nil, encTag), fasthttp.StatusOK, idTag, "")
			check("压缩请求+压缩标签", with(encHdr, encTag), fasthttp.StatusNotModified, encTag, "")
			check("压缩请求+identity标签", with(encHdr, idTag), fasthttp.StatusOK, encTag, tc.want)
			// 标签列表：命中其中任一个即可（弱比较忽略 W/ 前缀）
			check("压缩请求+列表含压缩标签", with(encHdr, idTag+", "+encTag), fasthttp.StatusNotModified, encTag, "")
			check("压缩请求+弱前缀压缩标签", with(encHdr, "W/"+encTag), fasthttp.StatusNotModified, encTag, "")
			check("identity请求+无关标签", with(nil, `"other"`), fasthttp.StatusOK, idTag, "")
		})
	}
}

// TestStaticIfNoneMatch_ReviewScenario 复现评审场景：请求 1 带 gzip 得到 "abc-gzip"，
// 请求 2 不接受 gzip 却携带 If-None-Match: "abc-gzip"，必须得到 200、identity 正文与 ETag "abc"。
func TestStaticIfNoneMatch_ReviewScenario(t *testing.T) {
	for _, gz := range []bool{false, true} {
		name := "动态gzip"
		if gz {
			name = "预压缩gz"
		}
		t.Run(name, func(t *testing.T) {
			handler := newEncStatic(t, encStaticOpts{size: 300, gz: gz})
			r1 := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip"})
			gzTag := string(r1.Response.Header.Peek("ETag"))
			if string(r1.Response.Header.Peek("Content-Encoding")) != "gzip" || gzTag == "" {
				t.Fatalf("第一次请求应为 gzip 且带 ETag, got CE=%q ETag=%q", r1.Response.Header.Peek("Content-Encoding"), gzTag)
			}
			r2 := doChain(handler, "GET", map[string]string{"If-None-Match": gzTag})
			if r2.Response.StatusCode() != fasthttp.StatusOK {
				t.Fatalf("status = %d, want 200", r2.Response.StatusCode())
			}
			if len(r2.Response.Body()) != 300 || len(r2.Response.Header.Peek("Content-Encoding")) != 0 {
				t.Errorf("应返回 identity 正文, len=%d CE=%q", len(r2.Response.Body()), r2.Response.Header.Peek("Content-Encoding"))
			}
			if got, want := string(r2.Response.Header.Peek("ETag")), strings.TrimSuffix(gzTag, `-gzip"`)+`"`; got != want {
				t.Errorf("ETag = %q, want identity %q", got, want)
			}
		})
	}
}

// TestStaticIfNoneMatch_UncompressedRepresentation 响应不会被压缩时（未协商、体积小于 min_size、
// 非可压缩 MIME、无压缩中间件、Range 生效），表示就是 identity，压缩标签不匹配而 identity 标签匹配。
func TestStaticIfNoneMatch_UncompressedRepresentation(t *testing.T) {
	cases := []struct {
		name string
		opts encStaticOpts
		hdr  map[string]string
	}{
		{"小于min_size", encStaticOpts{size: 300, minSize: 1000}, map[string]string{"Accept-Encoding": "gzip"}},
		{"非可压缩MIME", encStaticOpts{size: 300, types: []string{"text/html"}}, map[string]string{"Accept-Encoding": "gzip"}},
		{"无压缩中间件", encStaticOpts{size: 300, noCompress: true}, map[string]string{"Accept-Encoding": "gzip"}},
		{"HEAD之外的Range生效", encStaticOpts{size: 300}, map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-4"}},
		{"预压缩存在但Range生效", encStaticOpts{size: 300, gz: true}, map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newEncStatic(t, tc.opts)
			plain := doChain(handler, "GET", tc.hdr)
			idTag := string(plain.Response.Header.Peek("ETag"))
			if idTag == "" || strings.Contains(idTag, "-gzip") {
				t.Fatalf("ETag = %q, want identity", idTag)
			}
			gzTag := idTag[:len(idTag)-1] + `-gzip"`

			hdr := maps.Clone(tc.hdr)
			hdr["If-None-Match"] = gzTag
			ctx := doChain(handler, "GET", hdr)
			if ctx.Response.StatusCode() == fasthttp.StatusNotModified {
				t.Errorf("gzip 标签不应匹配 identity 表示, got 304")
			}

			hdr["If-None-Match"] = idTag
			ctx = doChain(handler, "GET", hdr)
			if ctx.Response.StatusCode() != fasthttp.StatusNotModified {
				t.Errorf("identity 标签: status = %d, want 304", ctx.Response.StatusCode())
			}
			if got := string(ctx.Response.Header.Peek("ETag")); got != idTag {
				t.Errorf("304 ETag = %q, want %q", got, idTag)
			}
		})
	}
}

// incompressibleBody 生成 n 字节确定性伪随机内容：gzip/brotli 压缩后体积反而变大，
// 压缩中间件会放弃压缩并回退 identity。
func incompressibleBody(n int) []byte {
	b := make([]byte, n)
	x := uint64(0x9E3779B97F4A7C15)
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = byte(x >> 24)
	}
	return b
}

// TestStaticIfNoneMatch_CompressionFallback 压缩后体积不减小时中间件放弃压缩并回退 identity，
// 此时响应带 identity ETag、无 Content-Encoding；客户端拿该 ETag 再验证必须得到 304（并回显同一 ETag），
// 而不是因处理器预测了 "-gzip" 变体而一直 200。
func TestStaticIfNoneMatch_CompressionFallback(t *testing.T) {
	cases := []struct {
		name string
		opts encStaticOpts
		enc  string
	}{
		{"动态gzip", encStaticOpts{body: incompressibleBody(300), minSize: 10}, "gzip"},
		{"动态gzip/文件缓存", encStaticOpts{body: incompressibleBody(300), minSize: 10, fileCache: true}, "gzip"},
		{"动态br", encStaticOpts{body: incompressibleBody(300), minSize: 10, algorithm: "both"}, "br, gzip"},
		{"动态gzip/sendfile", encStaticOpts{body: incompressibleBody(MinSendfileSize + 100), minSize: 10, sendfile: true}, "gzip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newEncStatic(t, tc.opts)
			doChain(handler, "GET", nil) // 预热文件缓存
			hdr := map[string]string{"Accept-Encoding": tc.enc}

			r1 := doChain(handler, "GET", hdr)
			idTag := string(r1.Response.Header.Peek("ETag"))
			if r1.Response.StatusCode() != fasthttp.StatusOK {
				t.Fatalf("首次请求 status = %d, want 200", r1.Response.StatusCode())
			}
			if ce := string(r1.Response.Header.Peek("Content-Encoding")); ce != "" {
				t.Fatalf("不可压缩内容应回退 identity, Content-Encoding = %q", ce)
			}
			if idTag == "" || strings.Contains(idTag, "-gzip") || strings.Contains(idTag, "-br") {
				t.Fatalf("回退响应应携带 identity ETag, got %q", idTag)
			}
			if len(r1.Response.Body()) != len(tc.opts.body) {
				t.Fatalf("回退响应体长度 = %d, want %d", len(r1.Response.Body()), len(tc.opts.body))
			}

			// 用刚收到的 ETag 再验证（同样带 Accept-Encoding）-> 304，回显同一 ETag。
			revHdr := map[string]string{"Accept-Encoding": tc.enc, "If-None-Match": idTag}
			r2 := doChain(handler, "GET", revHdr)
			if r2.Response.StatusCode() != fasthttp.StatusNotModified {
				t.Fatalf("再验证 status = %d, want 304", r2.Response.StatusCode())
			}
			if got := string(r2.Response.Header.Peek("ETag")); got != idTag {
				t.Errorf("304 ETag = %q, want %q", got, idTag)
			}
			if len(r2.Response.Body()) != 0 || len(r2.Response.Header.Peek("Content-Encoding")) != 0 {
				t.Errorf("304 不应有正文/Content-Encoding: body=%d CE=%q", len(r2.Response.Body()), r2.Response.Header.Peek("Content-Encoding"))
			}

			// 不带 Accept-Encoding 的请求：表示是 identity，压缩变体标签不匹配 -> 200。
			gzTag := idTag[:len(idTag)-1] + `-gzip"`
			r3 := doChain(handler, "GET", map[string]string{"If-None-Match": gzTag})
			if r3.Response.StatusCode() != fasthttp.StatusOK || string(r3.Response.Header.Peek("ETag")) != idTag {
				t.Errorf("identity 请求 + 压缩标签: status=%d ETag=%q, want 200/%q", r3.Response.StatusCode(), r3.Response.Header.Peek("ETag"), idTag)
			}

			// 标签列表含 identity 标签同样命中。
			r4 := doChain(handler, "GET", map[string]string{"Accept-Encoding": tc.enc, "If-None-Match": gzTag + ", W/" + idTag})
			if r4.Response.StatusCode() != fasthttp.StatusNotModified {
				t.Errorf("列表含 identity 标签: status = %d, want 304", r4.Response.StatusCode())
			}
		})
	}
}

// TestStaticIfNoneMatch_CompressibleStillDistinct 可压缩内容（压缩确实发生）时，
// 处理器不得因回退兼容而让 identity 标签匹配：gzip 请求 + identity 标签仍为 200。
func TestStaticIfNoneMatch_CompressibleStillDistinct(t *testing.T) {
	handler := newEncStatic(t, encStaticOpts{size: 300})
	idTag := string(doChain(handler, "GET", nil).Response.Header.Peek("ETag"))
	ctx := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": idTag})
	if ctx.Response.StatusCode() != fasthttp.StatusOK || string(ctx.Response.Header.Peek("Content-Encoding")) != "gzip" {
		t.Fatalf("status=%d CE=%q, want 200/gzip", ctx.Response.StatusCode(), ctx.Response.Header.Peek("Content-Encoding"))
	}
}

// headContentLength 将响应序列化后解析 Content-Length（HEAD 响应的长度在写出时才确定）。
func headContentLength(t *testing.T, ctx *fasthttp.RequestCtx) int {
	t.Helper()
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := ctx.Response.Write(bw); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	var resp fasthttp.Response
	resp.SkipBody = true
	if err := resp.Read(bufio.NewReader(&buf)); err != nil {
		t.Fatal(err)
	}
	return resp.Header.ContentLength()
}
