// Package handler 提供“压缩回退 identity 后条件请求再验证”的补充测试。
//
// 覆盖（评审要求）：
//   - HEAD 请求：If-None-Match 命中 identity ETag -> 304，未命中 -> 200 仅头部
//   - 缓冲/流式压缩阈值边界（64KB 与 64KB+1）两侧的回退与再验证行为
//   - If-None-Match 变体（*、弱标签、标签列表、不匹配）与 If-Modified-Since 的优先级
//   - 重写后的 304 头部与 200 / 处理器直接产生的 304 一致
//   - sendfile 路径确实触发了 DeferRevalidation
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
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/cache"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/middleware/compression"
)

// fbModTime 是测试文件固定的修改时间，便于构造 If-Modified-Since。
var fbModTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// streamingLimit 通过 CanFallBack 探测压缩中间件缓冲/流式压缩的实际分界（streamingThreshold），
// 返回最大的可回退长度（<= 该值走缓冲压缩，可能回退；> 该值走流式压缩，不回退）。
// 不硬编码常量：分界改变时测试自动跟随。
func streamingLimit(t *testing.T) int {
	t.Helper()
	mw, err := compression.New(&config.CompressionConfig{Type: "gzip", Level: 6, MinSize: 10, Types: []string{"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := 0, 1<<30 // CanFallBack(lo) 为真，CanFallBack(hi) 为假
	if !mw.CanFallBack(lo) || mw.CanFallBack(hi) {
		t.Fatalf("CanFallBack 单调性假设不成立")
	}
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if mw.CanFallBack(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

// fbOpts 配置 newFallbackChain。
type fbOpts struct {
	body      []byte
	sendfile  bool
	fileCache bool
	expires   string
	// recorder 非空时，在静态处理器返回后（压缩中间件处理前）记录处理器留下的状态。
	recorder *fbRecord
}

// fbRecord 记录静态处理器返回时的观测值。
type fbRecord struct {
	deferredTag  string // ctx.UserValue(RevalidateKey)
	deferredNM   bool   // ctx.UserValue(NotModifiedKey)
	isBodyStream bool   // 响应体是否为流（sendfile 路径的标志）
	status       int
}

// newFallbackChain 构造 压缩中间件(gzip, min_size=10, text/plain) + 静态处理器，文件 f.txt 固定 mtime。
func newFallbackChain(t *testing.T, o fbOpts) fasthttp.RequestHandler {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "f.txt")
	if err := os.WriteFile(p, o.body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fbModTime, fbModTime); err != nil {
		t.Fatal(err)
	}
	h := NewStaticHandler(root, "/", nil, o.sendfile)
	if o.expires != "" {
		h.SetExpires(o.expires)
	}
	if o.fileCache {
		h.SetFileCache(cache.NewFileCache(100, 1<<20, time.Minute))
		h.SetCacheTTL(time.Minute)
	}
	mw, err := compression.New(&config.CompressionConfig{Type: "gzip", Level: 6, MinSize: 10, Types: []string{"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	inner := h.Handle
	if o.recorder != nil {
		inner = func(ctx *fasthttp.RequestCtx) {
			h.Handle(ctx)
			*o.recorder = fbRecord{
				deferredTag:  stringValue(ctx.UserValue(compression.RevalidateKey)),
				deferredNM:   boolValue(ctx.UserValue(compression.NotModifiedKey)),
				isBodyStream: ctx.Response.IsBodyStream(),
				status:       ctx.Response.StatusCode(),
			}
		}
	}
	return mw.Process(inner)
}

func stringValue(v any) string { s, _ := v.(string); return s }
func boolValue(v any) bool     { b, _ := v.(bool); return b }

// fbServe 在真实 TCP 连接上服务 handler，返回发请求的函数（禁用 net/http 的自动解压，
// Accept-Encoding 完全由测试指定），用于 HEAD 与头部序列化断言。
func fbServe(t *testing.T, handler fasthttp.RequestHandler) func(method string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &fasthttp.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	client := &http.Client{Transport: &http.Transport{DisableCompression: true, DisableKeepAlives: true}}
	return func(method string, hdr map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+ln.Addr().String()+"/f.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
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
}

func gunzipAll(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

// TestStaticIfNoneMatch_CompressionFallback_HEAD 回退场景（不可压缩正文 + 接受 gzip）下的 HEAD 请求：
// If-None-Match 命中 identity ETag -> 304；未命中 -> 200，仅头部、无正文、无 Content-Encoding。
func TestStaticIfNoneMatch_CompressionFallback_HEAD(t *testing.T) {
	body := incompressibleBody(300)
	do := fbServe(t, newFallbackChain(t, fbOpts{body: body}))
	ae := map[string]string{"Accept-Encoding": "gzip"}

	r0, b0 := do("GET", ae)
	idTag := r0.Header.Get("ETag")
	if r0.StatusCode != 200 || r0.Header.Get("Content-Encoding") != "" || idTag == "" || len(b0) != len(body) {
		t.Fatalf("GET 应回退 identity: status=%d CE=%q ETag=%q len=%d", r0.StatusCode, r0.Header.Get("Content-Encoding"), idTag, len(b0))
	}

	hdr := func(inm string) map[string]string {
		return map[string]string{"Accept-Encoding": "gzip", "If-None-Match": inm}
	}

	t.Run("HEAD命中identity标签得304", func(t *testing.T) {
		for _, inm := range []string{idTag, "W/" + idTag, `"x", ` + idTag, "*"} {
			resp, b := do("HEAD", hdr(inm))
			if resp.StatusCode != http.StatusNotModified {
				t.Errorf("If-None-Match=%q: status = %d, want 304", inm, resp.StatusCode)
			}
			if got := resp.Header.Get("ETag"); got != idTag {
				t.Errorf("If-None-Match=%q: ETag = %q, want %q", inm, got, idTag)
			}
			if resp.Header.Get("Content-Encoding") != "" || len(b) != 0 {
				t.Errorf("If-None-Match=%q: 304 不应有 Content-Encoding/正文", inm)
			}
		}
	})

	t.Run("HEAD未命中得200仅头部", func(t *testing.T) {
		resp, b := do("HEAD", hdr(`"other"`))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("ETag"); got != idTag {
			t.Errorf("ETag = %q, want identity %q", got, idTag)
		}
		if resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("回退后不应有 Content-Encoding, got %q", resp.Header.Get("Content-Encoding"))
		}
		if len(b) != 0 {
			t.Errorf("HEAD 不应有正文, got %d bytes", len(b))
		}
		if resp.Header.Get("Last-Modified") == "" {
			t.Errorf("200 应带 Last-Modified")
		}
	})
}

// TestStaticCompression_ThresholdBoundary 缓冲/流式压缩分界两侧（不可压缩正文）：
//
//	<= 阈值：缓冲压缩，压缩后变大 -> 回退 identity；identity ETag 再验证得 304
//	>  阈值：流式压缩不回退 -> gzip 表示与 "-gzip" ETag；该标签再验证得 304，identity 标签得 200
func TestStaticCompression_ThresholdBoundary(t *testing.T) {
	limit := streamingLimit(t)
	if limit < MinSendfileSize {
		t.Fatalf("测试假设阈值(%d) >= MinSendfileSize(%d)", limit, MinSendfileSize)
	}
	cases := []struct {
		name         string
		size         int
		wantFallback bool // 是否可回退（mayFallBack）
	}{
		{"阈值-1", limit - 1, true},
		{"恰好阈值", limit, true},
		{"阈值+1", limit + 1, false},
	}
	for _, sendfile := range []bool{false, true} {
		for _, tc := range cases {
			name := tc.name
			if sendfile {
				name += "/sendfile"
			}
			t.Run(name, func(t *testing.T) {
				body := incompressibleBody(tc.size)
				var rec fbRecord
				handler := newFallbackChain(t, fbOpts{body: body, sendfile: sendfile, recorder: &rec})
				ae := map[string]string{"Accept-Encoding": "gzip"}

				// identity 请求取得 identity ETag
				idTag := string(doChain(handler, "GET", nil).Response.Header.Peek("ETag"))
				gzTag := idTag[:len(idTag)-1] + `-gzip"`

				r1 := doChain(handler, "GET", ae)
				if r1.Response.StatusCode() != 200 {
					t.Fatalf("首次 status = %d", r1.Response.StatusCode())
				}
				ce := string(r1.Response.Header.Peek("Content-Encoding"))
				tag1 := string(r1.Response.Header.Peek("ETag"))
				if tc.wantFallback {
					if ce != "" || tag1 != idTag || len(r1.Response.Body()) != tc.size {
						t.Fatalf("<=阈值应回退 identity: CE=%q ETag=%q(want %q) len=%d", ce, tag1, idTag, len(r1.Response.Body()))
					}
					// 再验证：带 identity ETag -> 304，回显同一 ETag；此时处理器必须推迟了判定。
					r2 := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": idTag})
					if rec.deferredTag != idTag {
						t.Errorf("可回退侧应登记 DeferRevalidation(%q), got %q", idTag, rec.deferredTag)
					}
					if r2.Response.StatusCode() != 304 || string(r2.Response.Header.Peek("ETag")) != idTag {
						t.Errorf("回退 + identity 标签: status=%d ETag=%q, want 304/%q", r2.Response.StatusCode(), r2.Response.Header.Peek("ETag"), idTag)
					}
				} else {
					if ce != "gzip" || tag1 != gzTag {
						t.Fatalf(">阈值应流式 gzip: CE=%q ETag=%q(want %q)", ce, tag1, gzTag)
					}
					if got := gunzipAll(t, r1.Response.Body()); string(got) != string(body) {
						t.Errorf("流式 gzip 解压后内容不一致")
					}
					r2 := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": gzTag})
					if r2.Response.StatusCode() != 304 || string(r2.Response.Header.Peek("ETag")) != gzTag {
						t.Errorf("流式 + gzip 标签: status=%d ETag=%q, want 304/%q", r2.Response.StatusCode(), r2.Response.Header.Peek("ETag"), gzTag)
					}
					if rec.deferredTag != "" {
						t.Errorf("流式侧预测精确，不应登记推迟, got %q", rec.deferredTag)
					}
					r3 := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": idTag})
					if r3.Response.StatusCode() != 200 || string(r3.Response.Header.Peek("Content-Encoding")) != "gzip" ||
						string(r3.Response.Header.Peek("ETag")) != gzTag {
						t.Errorf("流式 + identity 标签: status=%d CE=%q ETag=%q, want 200/gzip/%q",
							r3.Response.StatusCode(), r3.Response.Header.Peek("Content-Encoding"), r3.Response.Header.Peek("ETag"), gzTag)
					}
				}
			})
		}
	}
}

// TestStaticIfNoneMatch_FallbackVariants 回退场景（300B 不可压缩，gzip 可接受）下 If-None-Match / If-Modified-Since 的各种写法。
func TestStaticIfNoneMatch_FallbackVariants(t *testing.T) {
	body := incompressibleBody(300)
	imsSame := fbModTime.Format(httpTimeFormat)
	imsNewer := fbModTime.Add(time.Hour).Format(httpTimeFormat)
	imsOlder := fbModTime.Add(-time.Hour).Format(httpTimeFormat)
	cases := []struct {
		name string
		inm  func(id string) string // 返回 If-None-Match（空表示不带）
		ims  string
		want int
	}{
		{"星号", func(string) string { return "*" }, "", 304},
		{"星号在列表中", func(id string) string { return `"nope", *` }, "", 304},
		{"强标签", func(id string) string { return id }, "", 304},
		{"弱标签W/", func(id string) string { return "W/" + id }, "", 304},
		{"列表含标签", func(id string) string { return `"a", "b", ` + id + `, "c"` }, "", 304},
		{"列表含弱标签", func(id string) string { return `"a",W/` + id }, "", 304},
		{"不匹配", func(string) string { return `"other"` }, "", 200},
		{"不匹配列表", func(string) string { return `"a", W/"b"` }, "", 200},
		{"INM不匹配时忽略较新IMS", func(string) string { return `"other"` }, imsNewer, 200},
		{"INM匹配时忽略较旧IMS", func(id string) string { return id }, imsOlder, 304},
		{"星号时忽略较旧IMS", func(string) string { return "*" }, imsOlder, 304},
		{"仅IMS等于Last-Modified", nil, imsSame, 304},
		{"仅IMS晚于Last-Modified", nil, imsNewer, 304},
		{"仅IMS早于Last-Modified", nil, imsOlder, 200},
		{"无条件头", nil, "", 200},
	}
	for _, fileCache := range []bool{false, true} {
		for _, tc := range cases {
			name := tc.name
			if fileCache {
				name += "/文件缓存"
			}
			t.Run(name, func(t *testing.T) {
				handler := newFallbackChain(t, fbOpts{body: body, fileCache: fileCache})
				doChain(handler, "GET", nil) // 预热文件缓存
				r0 := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip"})
				idTag := string(r0.Response.Header.Peek("ETag"))
				if string(r0.Response.Header.Peek("Content-Encoding")) != "" || idTag == "" {
					t.Fatalf("前置：应回退 identity, CE=%q ETag=%q", r0.Response.Header.Peek("Content-Encoding"), idTag)
				}
				hdr := map[string]string{"Accept-Encoding": "gzip"}
				if tc.inm != nil {
					hdr["If-None-Match"] = tc.inm(idTag)
				}
				if tc.ims != "" {
					hdr["If-Modified-Since"] = tc.ims
				}
				ctx := doChain(handler, "GET", hdr)
				if got := ctx.Response.StatusCode(); got != tc.want {
					t.Fatalf("status = %d, want %d (If-None-Match=%q If-Modified-Since=%q)", got, tc.want, hdr["If-None-Match"], tc.ims)
				}
				// 无论 200 还是 304，最终表示都是 identity：ETag 相同、无 Content-Encoding。
				if got := string(ctx.Response.Header.Peek("ETag")); got != idTag {
					t.Errorf("ETag = %q, want identity %q", got, idTag)
				}
				if ce := ctx.Response.Header.Peek("Content-Encoding"); len(ce) != 0 {
					t.Errorf("Content-Encoding = %q, want empty", ce)
				}
				if tc.want == 304 && len(ctx.Response.Body()) != 0 {
					t.Errorf("304 不应有正文")
				}
				if tc.want == 200 && len(ctx.Response.Body()) != len(body) {
					t.Errorf("200 正文长度 = %d, want %d", len(ctx.Response.Body()), len(body))
				}
			})
		}
	}
}

// TestStaticIfNoneMatch_WildcardWhenCompressed 可压缩正文（压缩确实发生）时，"*" / 仅 IMS 的 304 回显压缩变体 ETag，
// 与同请求 200 会携带的 ETag 一致，且无 Content-Encoding/正文。
func TestStaticIfNoneMatch_WildcardWhenCompressed(t *testing.T) {
	handler := newFallbackChain(t, fbOpts{body: rangeFixture(300)})
	idTag := string(doChain(handler, "GET", nil).Response.Header.Peek("ETag"))
	gzTag := idTag[:len(idTag)-1] + `-gzip"`
	for name, hdr := range map[string]map[string]string{
		"星号":     {"If-None-Match": "*"},
		"仅IMS":   {"If-Modified-Since": fbModTime.Format(httpTimeFormat)},
		"压缩标签":   {"If-None-Match": gzTag},
		"星号+IMS": {"If-None-Match": "*", "If-Modified-Since": fbModTime.Add(-time.Hour).Format(httpTimeFormat)},
	} {
		t.Run(name, func(t *testing.T) {
			hdr["Accept-Encoding"] = "gzip"
			ctx := doChain(handler, "GET", hdr)
			if ctx.Response.StatusCode() != 304 {
				t.Fatalf("status = %d, want 304", ctx.Response.StatusCode())
			}
			if got := string(ctx.Response.Header.Peek("ETag")); got != gzTag {
				t.Errorf("ETag = %q, want %q", got, gzTag)
			}
			if len(ctx.Response.Header.Peek("Content-Encoding")) != 0 || len(ctx.Response.Body()) != 0 {
				t.Errorf("304 不应有 Content-Encoding/正文")
			}
		})
	}
}

// TestStaticNotModified_HeadersConsistent 回退场景下由压缩中间件改写的 304 与 200、与处理器直接产生的 304
// 在 ETag / Last-Modified / Accept-Ranges / Cache-Control / Vary 上保持一致（RFC 9110 15.4.5），
// 且不含 Content-Encoding / Content-Length / 正文。
//
// 说明：动态压缩路径本身不会在 200 上设置 Vary（仅预压缩文件路径设置），所以此处断言的是“304 与 200 一致”，
// 而不是“必须有 Vary”。
func TestStaticNotModified_HeadersConsistent(t *testing.T) {
	handler := newFallbackChain(t, fbOpts{body: incompressibleBody(300), expires: "1h"})
	do := fbServe(t, handler)
	ae := map[string]string{"Accept-Encoding": "gzip"}

	r200, _ := do("GET", ae) // 回退 identity 的 200
	idTag := r200.Header.Get("ETag")
	rewritten, b := do("GET", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": idTag})
	direct, _ := do("GET", map[string]string{"If-None-Match": idTag}) // 无 AE：处理器直接判定的 304

	if rewritten.StatusCode != 304 || direct.StatusCode != 304 {
		t.Fatalf("status rewritten=%d direct=%d, want 304/304", rewritten.StatusCode, direct.StatusCode)
	}
	if len(b) != 0 {
		t.Errorf("304 不应有正文")
	}
	for _, name := range []string{"Content-Encoding", "Content-Length", "Content-Range"} {
		if v := rewritten.Header.Get(name); v != "" {
			t.Errorf("改写后的 304 不应有 %s, got %q", name, v)
		}
	}
	for _, name := range []string{"ETag", "Last-Modified", "Accept-Ranges", "Cache-Control", "Vary"} {
		if got, want := rewritten.Header.Get(name), r200.Header.Get(name); got != want {
			t.Errorf("%s: 改写的 304 = %q, 200 = %q", name, got, want)
		}
		if got, want := rewritten.Header.Get(name), direct.Header.Get(name); got != want {
			t.Errorf("%s: 改写的 304 = %q, 处理器直接 304 = %q", name, got, want)
		}
	}
	for _, name := range []string{"ETag", "Last-Modified", "Accept-Ranges", "Cache-Control"} {
		if rewritten.Header.Get(name) == "" {
			t.Errorf("改写后的 304 缺少 %s", name)
		}
	}
	if got, want := rewritten.Header.Get("Last-Modified"), fbModTime.Format(httpTimeFormat); got != want {
		t.Errorf("Last-Modified = %q, want %q", got, want)
	}
}

// TestStaticIfNoneMatch_SendfileTriggersDeferral 确认 sendfile 用例真的走了“推迟 + 回退”路径。
//
// 阈值关系：sendfile 要求文件 >= MinSendfileSize(8KB)；压缩中间件对 <= streamingThreshold(64KB) 的正文走缓冲压缩，
// 才可能因压缩变大而回退。所以有效区间是 [MinSendfileSize, streamingThreshold]，用例取区间中点。
// 文件 > streamingThreshold 时走流式压缩，不会回退（见 TestStaticCompression_ThresholdBoundary）。
//
// 观测：处理器返回时响应体是流（SetBodyStream，即 sendfile 路径）、已登记 DeferRevalidation；
// 最终响应无 Content-Encoding（回退 identity）；再验证得 304。
func TestStaticIfNoneMatch_SendfileTriggersDeferral(t *testing.T) {
	limit := streamingLimit(t)
	size := (MinSendfileSize + limit) / 2
	if size < MinSendfileSize || size > limit {
		t.Fatalf("size %d 不在 [%d, %d]", size, MinSendfileSize, limit)
	}
	body := incompressibleBody(size)
	var rec fbRecord
	handler := newFallbackChain(t, fbOpts{body: body, sendfile: true, recorder: &rec})

	r1 := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip"})
	idTag := string(r1.Response.Header.Peek("ETag"))
	if !rec.isBodyStream {
		t.Fatalf("处理器应走 sendfile（SetBodyStream）路径")
	}
	if len(r1.Response.Header.Peek("Content-Encoding")) != 0 || len(r1.Response.Body()) != size {
		t.Fatalf("应回退 identity: CE=%q len=%d want %d", r1.Response.Header.Peek("Content-Encoding"), len(r1.Response.Body()), size)
	}
	if rec.deferredTag != "" {
		t.Errorf("无 If-None-Match 时不应登记推迟, got %q", rec.deferredTag)
	}

	r2 := doChain(handler, "GET", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": idTag})
	if rec.deferredTag != idTag {
		t.Fatalf("应登记 DeferRevalidation(%q), got %q", idTag, rec.deferredTag)
	}
	if rec.status != 200 {
		t.Errorf("处理器返回时应仍是 200（判定被推迟）, got %d", rec.status)
	}
	if r2.Response.StatusCode() != 304 || string(r2.Response.Header.Peek("ETag")) != idTag ||
		len(r2.Response.Header.Peek("Content-Encoding")) != 0 || len(r2.Response.Body()) != 0 {
		t.Errorf("再验证: status=%d ETag=%q CE=%q body=%d, want 304/%q/空/0",
			r2.Response.StatusCode(), r2.Response.Header.Peek("ETag"), r2.Response.Header.Peek("Content-Encoding"), len(r2.Response.Body()), idTag)
	}
}
