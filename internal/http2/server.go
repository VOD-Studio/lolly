// Package http2 提供 HTTP/2 协议支持。
//
// 该文件包含 HTTP/2 服务器的核心实现，包括：
//   - 基于 golang.org/x/net/http2 的 HTTP/2 服务器
//   - ALPN 协议协商支持
//   - 与现有 fasthttp handler 的集成
//   - 优雅关闭支持
//
// 主要用途：
//
//	用于在现有 TCP 监听器上提供 HTTP/2 协议支持，通过 ALPN 协商自动选择协议。
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
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/logging"
)

// Server HTTP/2 服务器。
//
// 包装 golang.org/x/net/http2 服务器，提供与 fasthttp handler 的集成。
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
	mu                      sync.RWMutex
	running                 bool
}

// NewServer 创建 HTTP/2 服务器。
//
// 参数：
//   - cfg: HTTP/2 配置
//   - handler: fasthttp 请求处理器
//   - tlsConfig: TLS 配置（可选，但推荐用于 ALPN 协商）
//
// 返回值：
//   - *Server: HTTP/2 服务器实例
//   - error: 配置无效时返回错误
func NewServer(cfg *config.HTTP2Config, handler fasthttp.RequestHandler, tlsConfig *tls.Config) (*Server, error) {
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

	return &Server{
		stopChan:                make(chan struct{}),
		http2Server:             h2s,
		config:                  cfg,
		tlsConfig:               tlsConfig,
		handler:                 handler,
		pool:                    newConnectionPool(),
		GracefulShutdownTimeout: gracefulTimeout,
		maxBodySize:             cfg.MaxBodySize,
		streamRequestBody:       cfg.StreamRequestBody,
	}, nil
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

		s.connWg.Add(1)
		go s.handleConnection(conn)
	}
}

// handleConnection 处理单个连接。
//
// 根据连接类型（TLS 或明文）和 ALPN 协商结果，选择合适的协议处理。
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
	}

	// 处理 HTTP/2 连接
	s.serveHTTP2(conn)
}

// serveHTTP2 使用 HTTP/2 协议服务连接。
func (s *Server) serveHTTP2(conn net.Conn) {
	adapter := NewFastHTTPHandlerAdapter(s.handler)
	adapter.MaxBodySize = s.maxBodySize
	adapter.StreamEnabled = s.streamRequestBody

	opts := &http2.ServeConnOpts{
		Context:    context.Background(),
		Handler:    adapter,
		BaseConfig: &http.Server{},
	}

	s.http2Server.ServeConn(conn, opts)
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

	// 关闭监听器
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
