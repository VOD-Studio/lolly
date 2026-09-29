// Package handler 提供路由器功能的测试。
//
// 该文件测试路由器模块的各项功能，包括：
//   - GET 路由注册
//   - POST 路由注册
//   - PUT 路由注册
//   - DELETE 路由注册
//   - HEAD 路由注册
//   - 任意 HTTP 方法路由注册
//   - 多方法路由区分
//   - 多路由注册
//   - 未匹配路由处理
//
// 作者：xfy
package handler

import (
	"testing"

	"github.com/valyala/fasthttp"
)

// TestRouterGET 测试 GET 路由注册。
func TestRouterGET(t *testing.T) {
	r := NewRouter()

	var called bool
	handler := func(ctx *fasthttp.RequestCtx) {
		called = true
		_, _ = ctx.WriteString("GET response")
	}

	r.GET("/test", handler)

	// 模拟 GET 请求
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/test")

	r.Handler()(&ctx)

	if !called {
		t.Error("GET handler 未被调用")
	}
	if string(ctx.Response.Body()) != "GET response" {
		t.Errorf("响应体 = %q, want %q", string(ctx.Response.Body()), "GET response")
	}
}

// TestRouterPOST 测试 POST 路由注册。
func TestRouterPOST(t *testing.T) {
	r := NewRouter()

	var called bool
	handler := func(ctx *fasthttp.RequestCtx) {
		called = true
		_, _ = ctx.WriteString("POST response")
	}

	r.POST("/submit", handler)

	// 模拟 POST 请求
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/submit")

	r.Handler()(&ctx)

	if !called {
		t.Error("POST handler 未被调用")
	}
	if string(ctx.Response.Body()) != "POST response" {
		t.Errorf("响应体 = %q, want %q", string(ctx.Response.Body()), "POST response")
	}
}

// TestRouterMultipleMethods 测试同路径不同方法的区分。
func TestRouterMultipleMethods(t *testing.T) {
	r := NewRouter()

	var getCalled, postCalled bool

	r.GET("/api", func(ctx *fasthttp.RequestCtx) {
		getCalled = true
		_, _ = ctx.WriteString("GET api")
	})

	r.POST("/api", func(ctx *fasthttp.RequestCtx) {
		postCalled = true
		_, _ = ctx.WriteString("POST api")
	})

	// 测试 GET 请求
	var getCtx fasthttp.RequestCtx
	getCtx.Request.Header.SetMethod("GET")
	getCtx.Request.SetRequestURI("/api")

	r.Handler()(&getCtx)

	if !getCalled {
		t.Error("GET handler 未被调用")
	}
	if postCalled {
		t.Error("POST handler 不应被调用")
	}
	if string(getCtx.Response.Body()) != "GET api" {
		t.Errorf("GET 响应体 = %q, want %q", string(getCtx.Response.Body()), "GET api")
	}

	// 重置并测试 POST 请求
	var postCtx fasthttp.RequestCtx
	postCtx.Request.Header.SetMethod("POST")
	postCtx.Request.SetRequestURI("/api")

	r.Handler()(&postCtx)

	if !postCalled {
		t.Error("POST handler 未被调用")
	}
	if string(postCtx.Response.Body()) != "POST api" {
		t.Errorf("POST 响应体 = %q, want %q", string(postCtx.Response.Body()), "POST api")
	}
}

// TestRouterHandlerNotNil 测试 Handler() 返回非 nil。
func TestRouterHandlerNotNil(t *testing.T) {
	r := NewRouter()

	handler := r.Handler()
	if handler == nil {
		t.Error("Handler() 返回 nil, want non-nil")
	}
}

// TestRouterMultipleRoutes 测试多路由注册。
func TestRouterMultipleRoutes(t *testing.T) {
	r := NewRouter()

	routes := map[string]string{
		"/users":    "users handler",
		"/products": "products handler",
		"/orders":   "orders handler",
	}

	for path, response := range routes {
		r.GET(path, func(ctx *fasthttp.RequestCtx) {
			_, _ = ctx.WriteString(response)
		})
	}

	for path, expected := range routes {
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod("GET")
		ctx.Request.SetRequestURI(path)

		r.Handler()(&ctx)

		if string(ctx.Response.Body()) != expected {
			t.Errorf("路径 %s 响应体 = %q, want %q", path, string(ctx.Response.Body()), expected)
		}
	}
}

// TestRouterPUT 测试 PUT 路由注册。
func TestRouterPUT(t *testing.T) {
	r := NewRouter()

	var called bool
	r.PUT("/update", func(ctx *fasthttp.RequestCtx) {
		called = true
		_, _ = ctx.WriteString("PUT response")
	})

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("PUT")
	ctx.Request.SetRequestURI("/update")

	r.Handler()(&ctx)

	if !called {
		t.Error("PUT handler 未被调用")
	}
}

// TestRouterDELETE 测试 DELETE 路由注册。
func TestRouterDELETE(t *testing.T) {
	r := NewRouter()

	var called bool
	r.DELETE("/remove", func(ctx *fasthttp.RequestCtx) {
		called = true
		_, _ = ctx.WriteString("DELETE response")
	})

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("DELETE")
	ctx.Request.SetRequestURI("/remove")

	r.Handler()(&ctx)

	if !called {
		t.Error("DELETE handler 未被调用")
	}
}

// TestRouterHEAD 测试 HEAD 路由注册。
func TestRouterHEAD(t *testing.T) {
	r := NewRouter()

	var called bool
	r.HEAD("/info", func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.Response.Header.Set("Content-Length", "100")
	})

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("HEAD")
	ctx.Request.SetRequestURI("/info")

	r.Handler()(&ctx)

	if !called {
		t.Error("HEAD handler 未被调用")
	}
}

// TestRouterANY 测试任意 HTTP 方法路由注册。
func TestRouterANY(t *testing.T) {
	r := NewRouter()
	r.ANY("/proxy", func(ctx *fasthttp.RequestCtx) {
		_, _ = ctx.Write(ctx.Method())
	})

	for _, method := range []string{"GET", "PATCH", "OPTIONS", "PROPFIND"} {
		t.Run(method, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.SetMethod(method)
			ctx.Request.SetRequestURI("/proxy")

			r.Handler()(&ctx)

			if got := string(ctx.Response.Body()); got != method {
				t.Errorf("响应体 = %q, want %q", got, method)
			}
		})
	}
}

// TestRouterNotFound 测试未匹配路由的处理。
func TestRouterNotFound(t *testing.T) {
	r := NewRouter()

	r.GET("/exists", func(ctx *fasthttp.RequestCtx) {
		_, _ = ctx.WriteString("found")
	})

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/notexists")

	r.Handler()(&ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("状态码 = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusNotFound)
	}
}
