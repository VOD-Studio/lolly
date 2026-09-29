// Package http2 提供 HTTP/2 协议支持。
//
// 该文件包含 HTTP/2 服务器的核心实现，包括：
//   - 基于 golang.org/x/net/http2 的 HTTP/2 服务器
//   - ALPN 协议协商支持
//   - 明文 h2c 连接的嗅探与回退（见 h2c.go）
//   - 与现有 fasthttp handler 的集成
//   - 优雅关闭支持
//
// 主要用途：
//
//	用于在现有 TCP 监听器上提供 HTTP/2 协议支持：TLS 监听器按 ALPN 协商，
//	明文监听器按 HTTP/2 连接前导嗅探，未命中的连接回退到 HTTP/1.1。
//
// 作者：xfy
package http2

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/logging"
)

// Server HTTP/2 服务器。
//
// 包装 golang.org/x/net/http2 服务器，提供与 fasthttp handler 的集成。
//
// 两种驱动方式互斥：
//   - Serve(ln)：自带 Accept 循环，TLS 走 ALPN、明文走前导嗅探，HTTP/1.1
//     回退逐连接调用 fasthttp.ServeConn；
//   - Wrap(ln)：只嗅探 h2c 连接，HTTP/1.1 连接交回外层的 fasthttp.Serve，
//     从而复用 fasthttp 的监听器与连接生命周期。
type Server struct {
	listener                net.Listener
	http2Server             *http2.Server
	config                  *config.HTTP2Config
	tlsConfig               *tls.Config
	pool                    *connectionPool
	handler                 fasthttp.RequestHandler
	stopChan                chan struct{}
	connWg                  sync.WaitGroup
	GracefulShutdownTimeout time.Duration
	maxBodySize             int64
	streamRequestBody       bool
	sniffTimeout            time.Duration
	maxConns                int
	activeConns             atomic.Int64
	ctx                     context.Context
	cancel                  context.CancelFunc
	mu                      sync.RWMutex
	running                 bool
}

// Option Server 的可选配置。
//
// 与 ssl.NewTLSManager 一致采用函数式选项，避免为少量参数继续膨胀
// NewServer 的位置参数列表。
type Option func(*Server)

// WithMaxConcurrentConns 限制 Wrap 模式（明文 h2c 嗅探）下的并发连接数。
//
// h2c 连接由嗅探器直接交给 ServeConn，不经过 fasthttp 的 Accept 循环，
// 因此不受 fasthttp 的 Concurrency 约束；这里显式把同一上限套到 HTTP/2
// 连接上，避免嗅探分派成为无界的 goroutine 来源。Serve 模式自带独立的
// Accept 循环，连接数由调用方通过停止监听器控制，不受该选项影响。
//
// 参数：
//   - n: 最大并发连接数，<=0 表示不限制
//
// 返回值：
//   - Option: 应用该配置的选项
func WithMaxConcurrentConns(n int) Option {
	return func(s *Server) { s.maxConns = n }
}

// NewServer 创建 HTTP/2 服务器。
//
// 参数：
//   - cfg: HTTP/2 配置
//   - handler: fasthttp 请求处理器
//   - tlsConfig: TLS 配置（可选，但推荐用于 ALPN 协商）
//   - opts: 可选配置
//
// 返回值：
//   - *Server: HTTP/2 服务器实例
//   - error: 配置无效时返回错误
func NewServer(cfg *config.HTTP2Config, handler fasthttp.RequestHandler, tlsConfig *tls.Config, opts ...Option) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("http2 config is nil")
	}

	if handler == nil {
		return nil, fmt.Errorf("handler is nil")
	}

	// 创建 HTTP/2 服务器
	h2s := newHTTP2Server(cfg)

	gracefulTimeout := cfg.GracefulShutdownTimeout
	if gracefulTimeout <= 0 {
		gracefulTimeout = 30 * time.Second
	}

	// ctx 被取消时 x/net/http2 会收尾对应连接，Stop 借此收敛在役 h2c 连接。
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		stopChan:                make(chan struct{}),
		http2Server:             h2s,
		config:                  cfg,
		tlsConfig:               tlsConfig,
		handler:                 handler,
		pool:                    newConnectionPool(),
		GracefulShutdownTimeout: gracefulTimeout,
		maxBodySize:             cfg.MaxBodySize,
		streamRequestBody:       cfg.StreamRequestBody,
		sniffTimeout:            defaultH2CSniffTimeout,
		ctx:                     ctx,
		cancel:                  cancel,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}

	return s, nil
}

// Serve 在指定监听器上启动 HTTP/2 服务器。
//
// 该方法会处理 ALPN 协议协商，根据客户端支持的协议自动选择 HTTP/2 或 HTTP/1.1。
//
// 参数：
//   - ln: TCP 监听器
//
// 返回值：
//   - error: 启动失败时返回错误
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("server already running")
	}
	s.running = true
	s.listener = ln
	s.mu.Unlock()

	log := logging.Info()
	if s.config.Enabled {
		log.Str("protocol", "h2").
			Bool("h2c", s.config.H2CEnabled).
			Bool("push", s.config.PushEnabled).
			Int("max_streams", s.config.MaxConcurrentStreams).
			Int("max_header_size", s.config.MaxHeaderListSize).
			Str("idle_timeout", s.config.IdleTimeout.String()).
			Msg("HTTP/2 server started")
	}

	// 启动连接处理循环
	for {
		select {
		case <-s.stopChan:
			return nil
		default:
		}

		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.stopChan:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			logging.Error().Err(err).Msg("HTTP/2 accept error")
			continue
		}

		// 明文连接由 handleConnection 嗅探前导；未命中 h2c 时回退 HTTP/1.1，
		// 因此独立监听模式下同一端口可同时服务两种协议。
		s.connWg.Add(1)
		go s.handleConnection(conn)
	}
}

// handleConnection 处理单个连接。
//
// 根据连接类型（TLS 或明文）和 ALPN 协商结果，选择合适的协议处理。
// 明文连接按 HTTP/2 连接前导嗅探，未命中的按 HTTP/1.1 回退给 fasthttp。
func (s *Server) handleConnection(conn net.Conn) {
	key := conn.RemoteAddr().String()
	s.pool.add(key, conn)
	defer func() {
		s.pool.remove(key, conn)
		s.connWg.Done()
		if err := conn.Close(); err != nil {
			logging.Error().Err(err).Msg("HTTP/2 connection close error")
		}
	}()

	// 如果是 TLS 连接，检查 ALPN 协商结果
	if tlsConn, ok := conn.(*tls.Conn); ok {
		// 执行 TLS 握手
		if err := tlsConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			logging.Error().Err(err).Msg("HTTP/2 set read deadline error")
			return
		}

		if err := tlsConn.Handshake(); err != nil {
			logging.Error().Err(err).Msg("HTTP/2 TLS handshake error")
			return
		}

		if err := tlsConn.SetReadDeadline(time.Time{}); err != nil {
			logging.Error().Err(err).Msg("HTTP/2 clear read deadline error")
			return
		}

		// 检查 ALPN 协商结果
		state := tlsConn.ConnectionState()
		if len(state.NegotiatedProtocol) > 0 && state.NegotiatedProtocol != "h2" {
			// ALPN 协商结果为 http/1.1 或其他，使用 fasthttp 处理
			s.serveHTTP1(tlsConn)
			return
		}

		s.serveHTTP2(conn)
		return
	}

	// 明文连接：只有以 HTTP/2 前导开头才是 h2c，否则必须按 HTTP/1.1 处理，
	// 否则同一端口上的普通请求会被当成协议错误全部丢弃。
	isH2C, sniffed, err := sniffH2C(conn, s.sniffTimeout)
	if err != nil {
		logging.Error().Err(err).Msg("HTTP/2 (h2c) preface sniff error")
		return
	}
	if !isH2C {
		s.serveHTTP1(sniffed)
		return
	}

	s.serveHTTP2(sniffed)
}

// serveHTTP2 使用 HTTP/2 协议服务连接。
func (s *Server) serveHTTP2(conn net.Conn) {
	s.serveHTTP2WithUpgrade(conn, nil, nil)
}

// serveHTTP2WithUpgrade 服务 HTTP/2 连接，可携带 h2c 升级请求与初始 SETTINGS。
//
// 参数：
//   - conn: HTTP/2 连接
//   - upgradeReq: 被升级的 HTTP/1.1 请求（按 RFC 7540 3.2 即流 1），无则传 nil
//   - settings: HTTP2-Settings 头部解码后的 SETTINGS 帧负载，无则传 nil
func (s *Server) serveHTTP2WithUpgrade(conn net.Conn, upgradeReq *http.Request, settings []byte) {
	adapter := NewFastHTTPHandlerAdapter(s.handler)
	adapter.MaxBodySize = s.maxBodySize
	adapter.StreamEnabled = s.streamRequestBody

	opts := &http2.ServeConnOpts{
		// 用 Server 的上下文：Stop 取消后 x/net/http2 会结束在役连接。
		Context:        s.connContext(),
		Handler:        adapter,
		BaseConfig:     &http.Server{},
		UpgradeRequest: upgradeReq,
		Settings:       settings,
	}

	s.http2Server.ServeConn(conn, opts)
}

// connContext 返回 ServeConn 使用的连接上下文。
//
// 独立 Server 未经 NewServer 构造（如测试里直接取零值）时回退到
// context.Background()，避免空上下文让 x/net/http2 直接拒绝服务连接。
//
// 返回值：
//   - context.Context: 可被 Stop 取消的连接上下文
func (s *Server) connContext() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// serveHTTP1 使用 HTTP/1.1 协议服务连接（回退到 fasthttp）。
func (s *Server) serveHTTP1(conn net.Conn) {
	// 创建一个简单的 fasthttp 服务器来处理单个连接
	server := &fasthttp.Server{
		Handler: s.handler,
	}

	// 使用 fasthttp 的连接处理
	if err := server.ServeConn(conn); err != nil {
		logging.Error().Err(err).Msg("HTTP/1.1 fallback serve error")
	}
}

// Stop 停止 HTTP/2 服务器。
//
// 优雅关闭服务器，等待现有连接完成。
//
// 返回值：
//   - error: 关闭失败时返回错误
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	s.running = false

	// 发送停止信号
	close(s.stopChan)

	// 取消连接上下文：x/net/http2 会在 ServeConn 内部监听 ctx.Done，
	// 借此结束仍活跃的 h2c 长连接，避免 Wait 一直等到空闲超时。
	if s.cancel != nil {
		s.cancel()
	}

	// 关闭监听器（Wrap 模式下监听器由外层 fasthttp 持有并关闭）
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			logging.Error().Err(err).Msg("HTTP/2 listener close error")
		}
	}

	// 关闭所有连接
	s.pool.closeAll()

	// 等待所有连接完成或超时
	done := make(chan struct{})
	go func() {
		s.connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logging.Info().Msg("HTTP/2 server stopped gracefully")
	case <-time.After(s.GracefulShutdownTimeout):
		logging.Warn().Msg("HTTP/2 server graceful shutdown timed out")
	}

	return nil
}

// connectionPool HTTP/2 连接池。
type connectionPool struct {
	conns map[string][]net.Conn
	mu    sync.RWMutex
}

// newConnectionPool 创建新的连接池。
func newConnectionPool() *connectionPool {
	return &connectionPool{
		conns: make(map[string][]net.Conn),
	}
}

// add 添加连接。
func (p *connectionPool) add(key string, conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[key] = append(p.conns[key], conn)
}

// remove 移除连接。
func (p *connectionPool) remove(key string, conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	conns := p.conns[key]
	for i, c := range conns {
		if c == conn {
			conns = append(conns[:i], conns[i+1:]...)
			if len(conns) == 0 {
				delete(p.conns, key)
			} else {
				p.conns[key] = conns
			}
			break
		}
	}
}

// closeAll 关闭所有连接。
func (p *connectionPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, conns := range p.conns {
		for _, conn := range conns {
			if err := conn.Close(); err != nil {
				// 忽略关闭错误，继续关闭其他连接
				continue
			}
		}
	}
	p.conns = make(map[string][]net.Conn)
}
