// Package handler 提供 HTTP 请求处理器，包括路由、静态文件服务和零拷贝传输。
//
// 该文件包含路由器相关的核心逻辑，包括：
//   - HTTP 方法路由注册（包括任意方法）
//   - 路由器创建和处理器获取
//
// 主要用途：
//
//	用于管理 HTTP 请求的路由分发，将请求路径映射到对应的处理器。
//
// 注意事项：
//   - 底层使用 fasthttp/router 实现
//   - 所有路由方法均为并发安全
//
// 作者：xfy
package handler

import (
	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

// Router HTTP 请求路由器。
//
// 封装 fasthttp/router，提供简洁的路由注册接口。
// 支持按具体方法或任意 HTTP 方法注册路由。
type Router struct {
	// router 底层 fasthttp 路由器实例
	router *router.Router
}

// NewRouter 创建路由器。
//
// 初始化并返回一个新的 Router 实例，底层使用 fasthttp/router 实现。
//
// 返回值：
//   - *Router: 新创建的路由器实例
//
// 使用示例：
//
//	r := handler.NewRouter()
//	r.GET("/api", myHandler)
func NewRouter() *Router {
	return &Router{
		router: router.New(),
	}
}

// GET 注册 GET 路由。
//
// 将指定路径映射到对应的处理器函数。
//
// 参数：
//   - path: 路由路径，支持参数化路径如 /user/{id}
//   - handler: 请求处理函数
func (r *Router) GET(path string, handler fasthttp.RequestHandler) {
	r.router.GET(path, handler)
}

// POST 注册 POST 路由。
//
// 将指定路径映射到对应的处理器函数。
//
// 参数：
//   - path: 路由路径，支持参数化路径
//   - handler: 请求处理函数
func (r *Router) POST(path string, handler fasthttp.RequestHandler) {
	r.router.POST(path, handler)
}

// PUT 注册 PUT 路由。
//
// 将指定路径映射到对应的处理器函数。
//
// 参数：
//   - path: 路由路径
//   - handler: 请求处理函数
func (r *Router) PUT(path string, handler fasthttp.RequestHandler) {
	r.router.PUT(path, handler)
}

// DELETE 注册 DELETE 路由。
//
// 将指定路径映射到对应的处理器函数。
//
// 参数：
//   - path: 路由路径
//   - handler: 请求处理函数
func (r *Router) DELETE(path string, handler fasthttp.RequestHandler) {
	r.router.DELETE(path, handler)
}

// HEAD 注册 HEAD 路由。
//
// 将指定路径映射到对应的处理器函数。
//
// 参数：
//   - path: 路由路径
//   - handler: 请求处理函数
func (r *Router) HEAD(path string, handler fasthttp.RequestHandler) {
	r.router.HEAD(path, handler)
}

// ANY 注册匹配任意 HTTP 方法的路由。
//
// 用于反向代理等需要保留并转发客户端原始方法的处理器。
//
// 参数：
//   - path: 路由路径
//   - handler: 请求处理函数
func (r *Router) ANY(path string, handler fasthttp.RequestHandler) {
	r.router.ANY(path, handler)
}

// Handler 返回路由处理器。
//
// 获取可用于 fasthttp.Server 的请求处理器。
// 该处理器会根据注册的路由规则分发请求。
//
// 返回值：
//   - fasthttp.RequestHandler: 请求处理器函数
//
// 使用示例：
//
//	server := &fasthttp.Server{
//	    Handler: r.Handler(),
//	}
func (r *Router) Handler() fasthttp.RequestHandler {
	return r.router.Handler
}
