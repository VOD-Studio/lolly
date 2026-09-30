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
