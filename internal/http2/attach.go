// Package http2 提供 HTTP/2 与 fasthttp 的 ALPN 集成。
//
// 该文件实现 Attach：将 HTTP/2 处理挂载到 fasthttp.Server 的 ALPN "h2"
// 分派上，使启用 TLS 的 fasthttp.Server 在 ALPN 协商出 h2 时，把连接
// 交给 golang.org/x/net/http2 处理，HTTP/1.1 仍由 fasthttp 自身服务。
//
// 与 server.Server.Serve 的独立监听循环不同，Attach 复用 fasthttp 的
// 监听器与连接生命周期，避免两个 Accept 循环竞争同一监听器。
//
// 作者：xfy
package http2

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/logging"
)

// newHTTP2Server 根据配置构建 http2.Server，填充默认值。
//
// 默认值与 NewServer 保持一致，确保经 Attach 与经独立 Server 启动的
// HTTP/2 行为相同。
//
// 参数：
//   - cfg: HTTP/2 配置
//
// 返回值：
//   - *http2.Server: 配置好的 HTTP/2 服务器
func newHTTP2Server(cfg *config.HTTP2Config) *http2.Server {
	maxConcurrentStreams := cfg.MaxConcurrentStreams
	if maxConcurrentStreams <= 0 {
		maxConcurrentStreams = 250
	}

	maxHeaderListSize := cfg.MaxHeaderListSize
	if maxHeaderListSize <= 0 {
		maxHeaderListSize = 1048576 // 1MB
	}

	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Second
	}

	return &http2.Server{
		MaxConcurrentStreams: uint32(maxConcurrentStreams),
		IdleTimeout:          idleTimeout,
		MaxReadFrameSize:     uint32(maxHeaderListSize),
		//nolint:staticcheck // SA1019: NewWriteScheduler deprecated
		NewWriteScheduler: func() http2.WriteScheduler { return http2.NewPriorityWriteScheduler(nil) },
		CountError:        func(_ string) {},
	}
}

// Attach 将 HTTP/2 处理挂载到 fasthttp.Server 的 ALPN "h2" 分派上。
//
// 调用后，启用 TLS 的 fasthttp.Server 会通过 NextProto("h2", ...)
// 将协商出 h2 的连接交给 http2.Server.ServeConn 处理；HTTP/1.1
// 仍由 fasthttp 自身处理。必须在 fastSrv.ServeTLS 之前调用。
//
// 若 fastSrv 未配置 TLS、HTTP/2 未启用或 handler 为空，则直接返回。
//
// 参数：
//   - fastSrv: 目标 fasthttp.Server（需已设置 TLSConfig）
//   - cfg: HTTP/2 配置
//   - handler: fasthttp 请求处理器
func Attach(fastSrv *fasthttp.Server, cfg *config.HTTP2Config, handler fasthttp.RequestHandler) {
	if fastSrv == nil || fastSrv.TLSConfig == nil || cfg == nil || !cfg.Enabled || handler == nil {
		return
	}

	h2s := newHTTP2Server(cfg)
	maxBodySize := cfg.MaxBodySize
	streamEnabled := cfg.StreamRequestBody

	// fasthttp.ServeHandler 签名为 func(net.Conn) error；将连接交给
	// http2.Server.ServeConn，由其处理 HTTP/2 帧层与多路复用。
	serve := func(conn net.Conn) error {
		adapter := NewFastHTTPHandlerAdapter(handler)
		adapter.MaxBodySize = maxBodySize
		adapter.StreamEnabled = streamEnabled

		opts := &http2.ServeConnOpts{
			Context:    context.Background(),
			Handler:    adapter,
			BaseConfig: &http.Server{},
		}
		h2s.ServeConn(conn, opts)
		return nil
	}

	fastSrv.NextProto("h2", serve)

	logging.Info().
		Str("protocol", "h2").
		Int("max_concurrent_streams", cfg.MaxConcurrentStreams).
		Msg("HTTP/2 attached to fasthttp server via ALPN")
}
