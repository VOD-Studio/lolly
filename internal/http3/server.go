// Package http3 提供 HTTP/3 (QUIC) 协议支持。
//
// 该文件包含 HTTP/3 服务器的核心实现，包括：
//   - 基于 quic-go 的 HTTP/3 服务器
//   - 支持 0-RTT 连接
//   - 优雅关闭支持
//   - 与现有 fasthttp handler 的集成
//
// 主要用途：
//
//	用于提供 HTTP/3 协议支持，提升网站性能和用户体验。
//
// 作者：xfy
package http3

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	quichttp3 "github.com/quic-go/quic-go/http3"
	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/logging"
)

const (
	defaultHTTP3Listen = ":443"
)

// Server HTTP/3 服务器。
//
// 使用 QUIC 协议提供 HTTP/3 服务，与现有的 TCP 服务器并行运行。
type Server struct {
	// config HTTP/3 配置
	config *config.HTTP3Config

	// http3Server HTTP/3 服务器实例
	http3Server *quichttp3.Server

	// handler fasthttp 请求处理器
	handler fasthttp.RequestHandler

	// adapter 请求适配器
	adapter *Adapter

	// tlsConfig TLS 配置
	tlsConfig *tls.Config

	// listener QUIC 监听器
	listener *quic.EarlyListener

	// running 服务器运行状态
	running atomic.Bool

	// mu 读写锁
	mu sync.RWMutex

	// maxBodySize 请求体大小限制
	maxBodySize int64

	// streamRequestBody 是否以流式方式把请求体注入 fasthttp.Request
	streamRequestBody bool
}

// NewServer 创建 HTTP/3 服务器。
//
// 参数：
//   - cfg: HTTP/3 配置
//   - handler: fasthttp 请求处理器
//   - tlsConfig: TLS 配置（必须）
//
// 返回值：
//   - *Server: HTTP/3 服务器实例
//   - error: 配置无效时返回错误
func NewServer(cfg *config.HTTP3Config, handler fasthttp.RequestHandler, tlsConfig *tls.Config) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("http3 config is nil")
	}

	if handler == nil {
		return nil, fmt.Errorf("handler is nil")
	}

	if tlsConfig == nil {
		return nil, fmt.Errorf("tls config is required for HTTP/3")
	}

	adapter := NewAdapter()
	adapter.MaxBodySize = cfg.MaxBodySize
	adapter.StreamEnabled = cfg.StreamRequestBody

	return &Server{
		config:            cfg,
		handler:           handler,
		adapter:           adapter,
		tlsConfig:         tlsConfig,
		maxBodySize:       cfg.MaxBodySize,
		streamRequestBody: cfg.StreamRequestBody,
	}, nil
}

// Start 启动 HTTP/3 服务器。
//
// 创建 UDP 监听器并开始接受 QUIC 连接。
//
// 返回值：
//   - error: 启动失败时返回错误
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running.Load() {
		return fmt.Errorf("server already running")
	}

	// 创建 QUIC 配置
	quicConfig := &quic.Config{
		MaxIncomingStreams: int64(s.config.MaxStreams),
		MaxIdleTimeout:     s.config.IdleTimeout,
		KeepAlivePeriod:    30 * time.Second,
		Allow0RTT:          s.config.Enable0RTT,
	}

	// 如果启用了 0-RTT，输出安全警告
	if s.config.Enable0RTT {
		logging.Warn().
			Msg("HTTP/3 0-RTT is enabled. " +
				"For 0-RTT to work, TLS session tickets must be configured " +
				"(TLSConfig.ClientSessionCache and TLSConfig.SessionTicketKey). " +
				"See documentation for details.")
	}

	// 设置默认值
	if quicConfig.MaxIncomingStreams == 0 {
		quicConfig.MaxIncomingStreams = 100
	}
	if quicConfig.MaxIdleTimeout == 0 {
		quicConfig.MaxIdleTimeout = 30 * time.Second
	}

	// 创建 UDP 监听器
	listenAddr := s.config.Listen
	if listenAddr == "" {
		listenAddr = defaultHTTP3Listen
	}

	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("failed to listen UDP: %w", err)
	}

	// 创建 QUIC 监听器
	s.listener, err = quic.ListenEarly(udpConn, s.tlsConfig, quicConfig)
	if err != nil {
		_ = udpConn.Close()
		return fmt.Errorf("failed to listen QUIC: %w", err)
	}

	// 创建 HTTP/3 服务器
	s.http3Server = &quichttp3.Server{
		Handler: s.adapter.Wrap(s.handler),
	}

	s.running.Store(true)

	logging.Info().
		Str("listen", listenAddr).
		Bool("0rtt", s.config.Enable0RTT).
		Msg("HTTP/3 server started")

	http3Server := s.http3Server
	listener := s.listener
	go func() {
		if err := http3Server.ServeListener(listener); err != nil {
			if s.running.Load() {
				logging.Error().Err(err).Msg("HTTP/3 server error")
			}
		}
	}()

	return nil
}

// Stop 停止 HTTP/3 服务器。
//
// 优雅关闭服务器，等待现有连接完成。
//
// 返回值：
//   - error: 关闭失败时返回错误
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running.Load() {
		return nil
	}

	s.running.Store(false)

	if s.http3Server != nil {
		if err := s.http3Server.Close(); err != nil {
			logging.Error().Err(err).Msg("HTTP/3 server close error")
		}
	}

	logging.Info().Msg("HTTP/3 server stopped")
	return nil
}
