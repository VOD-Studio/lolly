// Package compression 测试编码预测（PredictEncoding）与预压缩选择（SelectEncoding），
// 确认它们与 Process / ServeFile 的实际行为一致。
//
// 作者：xfy
package compression

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
)

func TestMiddlewarePredictEncoding_MatchesProcess(t *testing.T) {
	for _, typ := range []string{"gzip", "both"} {
		mw, err := New(&config.CompressionConfig{Type: typ, Level: 6, MinSize: 100, Types: []string{"text/plain"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, ae := range []string{"", "identity", "gzip", "br", "br, gzip", "deflate"} {
			for _, tc := range []struct {
				ct   string
				size int
			}{{"text/plain", 500}, {"text/plain", 99}, {"image/png", 500}} {
				body := make([]byte, tc.size)
				for i := range body {
					body[i] = 'a'
				}
				ctx := &fasthttp.RequestCtx{}
				if ae != "" {
					ctx.Request.Header.Set("Accept-Encoding", ae)
				}
				mw.Process(func(c *fasthttp.RequestCtx) {
					c.Response.Header.SetContentType(tc.ct)
					c.Response.SetBody(body)
				})(ctx)
				want := string(ctx.Response.Header.Peek("Content-Encoding"))
				got := mw.PredictEncoding([]byte(ae), []byte(tc.ct), tc.size)
				if got != want {
					t.Errorf("type=%s AE=%q ct=%s size=%d: PredictEncoding=%q, Process 实际=%q", typ, ae, tc.ct, tc.size, got, want)
				}
			}
		}
	}
}

func TestGzipStaticSelectEncoding_MatchesServeFile(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.js", "a.js.gz", "a.js.br", "b.js", "b.js.gz", "c.js", "d.png", "d.png.gz"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := NewGzipStatic(true, dir, nil, nil)
	for _, path := range []string{"a.js", "b.js", "c.js", "d.png"} {
		for _, ae := range []string{"", "gzip", "br", "br, gzip", "deflate"} {
			ctx := &fasthttp.RequestCtx{}
			if ae != "" {
				ctx.Request.Header.Set("Accept-Encoding", ae)
			}
			sel := g.SelectEncoding(ctx, path)
			if len(ctx.Response.Header.Peek("Content-Encoding")) != 0 {
				t.Fatalf("SelectEncoding 不应修改响应")
			}
			served := g.ServeFile(ctx, path)
			got := string(ctx.Response.Header.Peek("Content-Encoding"))
			if !served {
				got = ""
			}
			if sel != got {
				t.Errorf("%s AE=%q: SelectEncoding=%q, ServeFile 实际=%q", path, ae, sel, got)
			}
		}
	}
}

func TestMiddlewareCanFallBack(t *testing.T) {
	mw, err := New(&config.CompressionConfig{Type: "gzip", Level: 6, MinSize: 10, Types: []string{"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	if !mw.CanFallBack(300) || !mw.CanFallBack(streamingThreshold) {
		t.Errorf("缓冲路径（<= streamingThreshold）压缩可能被放弃")
	}
	if mw.CanFallBack(streamingThreshold + 1) {
		t.Errorf("流式路径（> streamingThreshold）不会回退")
	}
}

// TestMiddlewareDeferredRevalidation 下游推迟 If-None-Match 判定后，
// 压缩被放弃 -> 改写为 304；压缩发生 -> 保持 200；ETag 不一致 / 非 200 -> 不改写。
func TestMiddlewareDeferredRevalidation(t *testing.T) {
	mw, err := New(&config.CompressionConfig{Type: "gzip", Level: 6, MinSize: 10, Types: []string{"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 300)
	x := uint64(88172645463325252)
	for i := range random {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		random[i] = byte(x >> 24)
	}
	text := make([]byte, 300)
	for i := range text {
		text[i] = 'a'
	}
	const tag = `"abc"`
	run := func(body []byte, deferTag, respTag string, status int) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
		mw.Process(func(c *fasthttp.RequestCtx) {
			DeferRevalidation(c, deferTag)
			c.Response.SetStatusCode(status)
			c.Response.Header.SetContentType("text/plain")
			c.Response.Header.Set("ETag", respTag)
			c.Response.Header.Set("Last-Modified", "Wed, 30 Sep 2026 00:00:00 GMT")
			c.Response.SetBody(body)
		})(ctx)
		return ctx
	}

	ctx := run(random, tag, tag, 200)
	if ctx.Response.StatusCode() != 304 || len(ctx.Response.Body()) != 0 ||
		string(ctx.Response.Header.Peek("ETag")) != tag || len(ctx.Response.Header.Peek("Content-Encoding")) != 0 ||
		len(ctx.Response.Header.Peek("Last-Modified")) == 0 {
		t.Errorf("压缩放弃时应改写为 304 并保留 ETag/Last-Modified: %q", ctx.Response.Header.String())
	}
	ctx = run(text, tag, tag, 200)
	if ctx.Response.StatusCode() != 200 || string(ctx.Response.Header.Peek("Content-Encoding")) != "gzip" ||
		string(ctx.Response.Header.Peek("ETag")) != `"abc-gzip"` {
		t.Errorf("压缩发生时应保持 200 gzip: %d %q", ctx.Response.StatusCode(), ctx.Response.Header.String())
	}
	if ctx = run(random, `"other"`, tag, 200); ctx.Response.StatusCode() != 200 {
		t.Errorf("ETag 不一致不应改写")
	}
	if ctx = run(random, tag, tag, 404); ctx.Response.StatusCode() != 404 {
		t.Errorf("非 200 不应改写")
	}
}

// TestMiddlewareDeferredNotModified 与表示无关的条件（If-None-Match: * / 仅 If-Modified-Since）被推迟后：
// 压缩发生 -> 304 回显压缩变体 ETag；压缩被放弃 -> 304 回显 identity ETag；均无 Content-Encoding/正文；
// 未登记推迟 / 非 200 时不改写。
func TestMiddlewareDeferredNotModified(t *testing.T) {
	mw, err := New(&config.CompressionConfig{Type: "gzip", Level: 6, MinSize: 10, Types: []string{"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 300)
	x := uint64(88172645463325252)
	for i := range random {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		random[i] = byte(x >> 24)
	}
	text := make([]byte, 300)
	for i := range text {
		text[i] = 'a'
	}
	run := func(body []byte, deferNM bool, status int) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
		mw.Process(func(c *fasthttp.RequestCtx) {
			if deferNM {
				DeferNotModified(c)
			}
			c.Response.SetStatusCode(status)
			c.Response.Header.SetContentType("text/plain")
			c.Response.Header.Set("ETag", `"abc"`)
			c.Response.Header.Set("Last-Modified", "Wed, 30 Sep 2026 00:00:00 GMT")
			c.Response.Header.Set("Cache-Control", "public, max-age=60")
			c.Response.SetBody(body)
		})(ctx)
		return ctx
	}
	check := func(name string, ctx *fasthttp.RequestCtx, wantTag string) {
		t.Helper()
		if ctx.Response.StatusCode() != 304 || len(ctx.Response.Body()) != 0 ||
			string(ctx.Response.Header.Peek("ETag")) != wantTag ||
			len(ctx.Response.Header.Peek("Content-Encoding")) != 0 ||
			len(ctx.Response.Header.Peek("Last-Modified")) == 0 || len(ctx.Response.Header.Peek("Cache-Control")) == 0 {
			t.Errorf("%s: 期望 304 + ETag %s + 保留 Last-Modified/Cache-Control: %d %q", name, wantTag, ctx.Response.StatusCode(), ctx.Response.Header.String())
		}
	}
	check("压缩发生", run(text, true, 200), `"abc-gzip"`)
	check("压缩被放弃", run(random, true, 200), `"abc"`)
	if ctx := run(text, false, 200); ctx.Response.StatusCode() != 200 || string(ctx.Response.Header.Peek("Content-Encoding")) != "gzip" {
		t.Errorf("未登记推迟不应改写: %d", ctx.Response.StatusCode())
	}
	if ctx := run(random, true, 404); ctx.Response.StatusCode() != 404 {
		t.Errorf("非 200 不应改写: %d", ctx.Response.StatusCode())
	}
}
