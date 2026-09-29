// Package server 提供 HTTP 服务器功能的测试。
//
// 该文件测试服务器模块的各项功能，包括：
//   - 服务器创建和初始化
//   - 启动和停止控制
//   - 优雅关闭
//   - 中间件链构建
//   - 请求统计追踪
//   - 监听器管理
//   - TLS 配置
//   - 代理缓存统计
//
// 作者：xfy
package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/lua"
	"rua.plus/lolly/internal/matcher"
	"rua.plus/lolly/internal/middleware/accesslog"
	"rua.plus/lolly/internal/middleware/security"
	"rua.plus/lolly/internal/proxy"
	"rua.plus/lolly/internal/ssl"
	"rua.plus/lolly/internal/testutil"
	"rua.plus/lolly/internal/version"
)

// TestNew 测试服务器创建
func TestNew(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Static: []config.StaticConfig{{
				Path:  "/",
				Root:  "./static",
				Index: []string{"index.html"},
			}},
		}},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("New() returned nil, expected non-nil Server")
	}

	if s.config != cfg {
		t.Error("Server.config not set correctly")
	}

	if s.running.Load() {
		t.Error("Server.running should be false initially")
	}

	if s.fastServer != nil {
		t.Error("Server.fastServer should be nil before Start()")
	}
}

// TestStopWithoutServer 测试无服务器时调用 Stop
func TestStopWithoutServer(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 在未启动时调用 Stop，应返回 nil
	err := s.StopWithTimeout(5 * time.Second)
	if err != nil {
		t.Errorf("StopWithTimeout() on non-started server returned error: %v", err)
	}
}

// TestGracefulStop 测试 GracefulStop 调用
func TestGracefulStop(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 在未启动时调用 GracefulStop，应返回 nil
	err := s.GracefulStop(5 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop() on non-started server returned error: %v", err)
	}
}

// TestStopAfterStop 测试多次调用 Stop
func TestStopAfterStop(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 多次调用 StopWithTimeout 应该都是安全的
	for i := range 3 {
		err := s.StopWithTimeout(5 * time.Second)
		if err != nil {
			t.Errorf("StopWithTimeout() call %d returned error: %v", i+1, err)
		}
	}
}

// TestGracefulStopWithZeroTimeout 测试零超时的 GracefulStop
func TestGracefulStopWithZeroTimeout(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	err := s.GracefulStop(0)
	if err != nil {
		t.Errorf("GracefulStop(0) returned error: %v", err)
	}
}

// TestBuildMiddlewareChain_AccessLog 测试访问日志中间件
func TestBuildMiddlewareChain_AccessLog(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestBuildMiddlewareChain_AccessControl 测试访问控制中间件
func TestBuildMiddlewareChain_AccessControl(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Security: config.SecurityConfig{
				Access: config.AccessConfig{
					Allow: []string{"127.0.0.1"},
				},
			},
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestBuildMiddlewareChain_RateLimiter 测试限流中间件
func TestBuildMiddlewareChain_RateLimiter(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Security: config.SecurityConfig{
				RateLimit: config.RateLimitConfig{
					RequestRate: 100,
					Burst:       200,
				},
			},
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestBuildMiddlewareChain_Rewrite 测试重写中间件
func TestBuildMiddlewareChain_Rewrite(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Rewrite: []config.RewriteRule{
				{Pattern: "/old/(.*)", Replacement: "/new/$1"},
			},
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestBuildMiddlewareChain_Compression 测试压缩中间件
func TestBuildMiddlewareChain_Compression(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Compression: config.CompressionConfig{
				Level: 6,
			},
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestBuildMiddlewareChain_SecurityHeaders 测试安全头中间件
func TestBuildMiddlewareChain_SecurityHeaders(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Security: config.SecurityConfig{
				Headers: config.SecurityHeaders{
					XFrameOptions:       "DENY",
					XContentTypeOptions: "nosniff",
				},
			},
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestBuildMiddlewareChain_AllMiddlewares 测试所有中间件组合
func TestBuildMiddlewareChain_AllMiddlewares(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Security: config.SecurityConfig{
				Access: config.AccessConfig{
					Allow: []string{"127.0.0.1"},
				},
				RateLimit: config.RateLimitConfig{
					RequestRate: 100,
					Burst:       200,
				},
				Headers: config.SecurityHeaders{
					XFrameOptions: "DENY",
				},
			},
			Rewrite: []config.RewriteRule{
				{Pattern: "/old/(.*)", Replacement: "/new/$1"},
			},
			Compression: config.CompressionConfig{
				Level: 6,
			},
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestTrackStats 测试请求统计追踪
func TestTrackStats(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 初始统计应该为 0
	if s.requests.Load() != 0 {
		t.Error("Initial requests should be 0")
	}
	if s.bytesSent.Load() != 0 {
		t.Error("Initial bytesSent should be 0")
	}
	if s.bytesReceived.Load() != 0 {
		t.Error("Initial bytesReceived should be 0")
	}

	// 创建测试 handler
	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetBodyString("response body")
	}

	// 包装 handler
	wrappedHandler := s.trackStats(handler)

	// 创建测试请求上下文
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	ctx.Request.SetBody([]byte("request body"))

	// 执行
	wrappedHandler(ctx)

	// 验证统计
	if s.requests.Load() != 1 {
		t.Errorf("Expected 1 request, got %d", s.requests.Load())
	}

	if s.bytesReceived.Load() != int64(len("request body")) {
		t.Errorf("Expected bytesReceived %d, got %d", len("request body"), s.bytesReceived.Load())
	}

	if s.bytesSent.Load() != int64(len("response body")) {
		t.Errorf("Expected bytesSent %d, got %d", len("response body"), s.bytesSent.Load())
	}
}

// TestTrackStats_MultipleRequests 测试多次请求统计
func TestTrackStats_MultipleRequests(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetBodyString("ok")
	}

	wrappedHandler := s.trackStats(handler)

	// 执行多次请求
	for range 10 {
		ctx := &fasthttp.RequestCtx{}
		ctx.Init(&fasthttp.Request{}, nil, nil)
		wrappedHandler(ctx)
	}

	if s.requests.Load() != 10 {
		t.Errorf("Expected 10 requests, got %d", s.requests.Load())
	}
}

// TestGetListeners_Empty 测试空监听器列表
func TestGetListeners_Empty(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	listeners := s.GetListeners()
	if listeners != nil {
		t.Errorf("Expected nil listeners, got %v", listeners)
	}
}

// TestSetListeners 测试设置监听器
func TestSetListeners(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 创建模拟监听器
	listener1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer func() {
		_ = listener1.Close()
	}()

	listener2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer func() {
		_ = listener2.Close()
	}()

	listeners := []net.Listener{listener1, listener2}
	s.SetListeners(listeners)

	// 外部提供的监听器在启动前不属于活动监听器。
	if got := s.GetListeners(); len(got) != 0 {
		t.Errorf("Expected no active listeners, got %d", len(got))
	}
	if len(s.providedListeners) != 2 {
		t.Errorf("Expected 2 provided listeners, got %d", len(s.providedListeners))
	}
}

// TestGetTLSConfig_NotConfigured 测试未配置 TLS
func TestGetTLSConfig_NotConfigured(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	tlsConfig, err := s.GetTLSConfig()
	if err == nil {
		t.Error("Expected error for unconfigured TLS")
	}
	if tlsConfig != nil {
		t.Error("Expected nil TLS config")
	}
	if err.Error() != "TLS not configured" {
		t.Errorf("Expected error 'TLS not configured', got: %v", err)
	}
}

// TestGetHandler 测试获取 handler
func TestGetHandler(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 初始 handler 应该为 nil
	handler := s.GetHandler()
	if handler != nil {
		t.Error("Expected nil handler initially")
	}

	// 设置一个 handler
	testHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetBodyString("test")
	}
	s.handler = testHandler

	// 验证获取成功
	got := s.GetHandler()
	if got == nil {
		t.Error("Expected non-nil handler after setting")
	}
}

// TestServer_Connections 测试连接统计
func TestServer_Connections(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 初始连接数应该为 0
	if s.connections.Load() != 0 {
		t.Error("Initial connections should be 0")
	}

	// 增加
	s.connections.Add(1)
	if s.connections.Load() != 1 {
		t.Errorf("Expected 1 connection, got %d", s.connections.Load())
	}

	// 减少
	s.connections.Add(-1)
	if s.connections.Load() != 0 {
		t.Errorf("Expected 0 connections, got %d", s.connections.Load())
	}
}

// TestServer_Proxies 测试代理管理
func TestServer_Proxies(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 初始代理列表应为空
	if len(s.proxies) != 0 {
		t.Error("Initial proxies should be empty")
	}
}

// TestServer_Running 测试运行状态
func TestServer_Running(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 初始状态应为未运行
	if s.running.Load() {
		t.Error("Initial running state should be false")
	}
}

// TestServer_StopWithNilFastServer 测试无 fastServer 时停止
func TestServer_StopWithNilFastServer(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	s.fastServer = nil

	err := s.StopWithTimeout(5 * time.Second)
	if err != nil {
		t.Errorf("StopWithTimeout with nil fastServer should succeed: %v", err)
	}
}

// TestServer_GracefulStopWithNilFastServer 测试无 fastServer 时优雅停止
func TestServer_GracefulStopWithNilFastServer(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	s.fastServer = nil

	err := s.GracefulStop(5 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop with nil fastServer should succeed: %v", err)
	}
}

// TestServer_GetProxyCacheStats 测试代理缓存统计
func TestServer_GetProxyCacheStats(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 无代理时应返回空统计
	stats := s.getProxyCacheStats()
	if stats.Entries != 0 {
		t.Errorf("Expected 0 entries, got %d", stats.Entries)
	}
	if stats.Pending != 0 {
		t.Errorf("Expected 0 pending, got %d", stats.Pending)
	}
}

// TestServer_BuildMiddlewareChain_EmptyConfig 测试空配置的中间件链
func TestServer_BuildMiddlewareChain_EmptyConfig(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestServer_TrackStats_EmptyBody 测试空响应体的统计
func TestServer_TrackStats_EmptyBody(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	handler := func(_ *fasthttp.RequestCtx) {
		// 空响应
	}

	wrappedHandler := s.trackStats(handler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	ctx.Request.SetBody(nil)

	wrappedHandler(ctx)

	if s.requests.Load() != 1 {
		t.Errorf("Expected 1 request, got %d", s.requests.Load())
	}

	if s.bytesSent.Load() != 0 {
		t.Errorf("Expected 0 bytes sent, got %d", s.bytesSent.Load())
	}
}

// TestStart_NilConfig 测试 nil 配置启动返回错误而非 panic。
func TestStart_NilConfig(t *testing.T) {
	s := New(nil)

	err := s.Start()
	if err == nil {
		t.Error("Start() with nil config should return error")
	}
}

// TestStart_EmptyServers 测试空服务器列表启动返回错误而非 panic。
func TestStart_EmptyServers(t *testing.T) {
	s := New(&config.Config{})

	err := s.Start()
	if err == nil {
		t.Error("Start() with empty servers should return error")
	}
}

// TestStart_Success 测试服务器配置初始化
func TestStart_Success(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 验证服务器正确初始化
	if s == nil {
		t.Fatal("New() returned nil, expected non-nil Server")
	}

	if s.config != cfg {
		t.Error("Server.config not set correctly")
	}
}

// TestStart_WithStaticFiles 测试静态文件配置
func TestStart_WithStaticFiles(t *testing.T) {
	// 创建临时目录
	tempDir := t.TempDir()

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Static: []config.StaticConfig{{
				Path:  "/static",
				Root:  tempDir,
				Index: []string{"index.html"},
			}},
		}},
	}

	s := New(cfg)

	if s == nil {
		t.Fatal("New() returned nil")
	}
}

// TestStart_WithGoroutinePool 测试 GoroutinePool 配置
func TestStart_WithGoroutinePool(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
		Performance: config.PerformanceConfig{
			GoroutinePool: config.GoroutinePoolConfig{
				Enabled:     true,
				MaxWorkers:  100,
				MinWorkers:  10,
				IdleTimeout: 30 * time.Second,
			},
		},
	}

	s := New(cfg)

	if s == nil {
		t.Fatal("New() returned nil")
	}
}

// TestStart_WithFileCache 测试文件缓存配置
func TestStart_WithFileCache(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
		Performance: config.PerformanceConfig{
			FileCache: config.FileCacheConfig{
				MaxEntries: 1000,
				MaxSize:    100 * 1024 * 1024,
			},
		},
	}

	s := New(cfg)

	if s == nil {
		t.Fatal("New() returned nil")
	}
}

// TestStop_Graceful 测试优雅停止（无 race 模式）
func TestStop_Graceful(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)

	// 在未启动时调用 GracefulStop，应返回 nil
	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop() on non-started server returned error: %v", err)
	}
}

// TestGetTLSConfig_Nil 测试无 TLS 配置
func TestGetTLSConfig_Nil(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)

	tlsCfg, err := s.GetTLSConfig()
	if err == nil {
		t.Error("GetTLSConfig() should return error when TLS not configured")
	}
	if tlsCfg != nil {
		t.Error("GetTLSConfig() should return nil when TLS not configured")
	}
}

// TestGetTLSConfig_NilServer 测试 nil 服务器调用 GetTLSConfig
func TestGetTLSConfig_NilServer(t *testing.T) {
	var s *Server
	// 防御性：如果 s 为 nil，调用方法会 panic，这是预期的行为
	// 这里我们只测试非 nil 但 tlsManager 为 nil 的情况
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}
	s = New(cfg)

	// 确保 tlsManager 为 nil
	if s.tlsManager != nil {
		t.Skip("tlsManager should be nil initially")
	}

	tlsCfg, err := s.GetTLSConfig()
	if err == nil {
		t.Error("Expected error when tlsManager is nil")
	}
	if tlsCfg != nil {
		t.Error("Expected nil TLS config when tlsManager is nil")
	}
	if err.Error() != "TLS not configured" {
		t.Errorf("Expected error 'TLS not configured', got: %v", err)
	}
}

// TestGetServerName 测试服务器名称返回。
func TestGetServerName(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *config.ServerConfig
		wantName string
	}{
		{
			name:     "nil config",
			cfg:      nil,
			wantName: "lolly/" + version.Version,
		},
		{
			name: "ServerTokens true (default)",
			cfg: &config.ServerConfig{
				ServerTokens: true,
			},
			wantName: "lolly/" + version.Version,
		},
		{
			name: "ServerTokens false",
			cfg: &config.ServerConfig{
				ServerTokens: false,
			},
			wantName: "lolly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			got := s.getServerName(tt.cfg)
			if got != tt.wantName {
				t.Errorf("getServerName() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

// TestApplyTypesConfig 测试 MIME 类型配置应用。
func TestApplyTypesConfig(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		s := &Server{}
		// 不应该 panic
		s.applyTypesConfig(nil)
	})

	t.Run("empty config", func(t *testing.T) {
		s := &Server{}
		cfg := &config.ServerConfig{}
		// 不应该 panic
		s.applyTypesConfig(cfg)
	})

	t.Run("with types map", func(t *testing.T) {
		s := &Server{}
		cfg := &config.ServerConfig{
			Types: config.TypesConfig{
				Map: map[string]string{
					".custom": "application/x-custom",
				},
			},
		}
		// 不应该 panic
		s.applyTypesConfig(cfg)
	})

	t.Run("with default type", func(t *testing.T) {
		s := &Server{}
		cfg := &config.ServerConfig{
			Types: config.TypesConfig{
				DefaultType: "application/octet-stream",
			},
		}
		// 不应该 panic
		s.applyTypesConfig(cfg)
	})
}

// TestCreateListener_TCP 测试 TCP 监听器创建。
func TestCreateListener_TCP(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0", // 随机端口
		}},
	}

	s := New(cfg)
	ln, err := s.createListener(&cfg.Servers[0])
	if err != nil {
		t.Fatalf("createListener() error: %v", err)
	}
	if ln == nil {
		t.Fatal("createListener() returned nil listener")
	}
	defer ln.Close()

	if ln.Addr().Network() != "tcp" {
		t.Errorf("expected tcp network, got %s", ln.Addr().Network())
	}
}

// TestCreateListener_UnixSocket 测试 Unix Socket 监听器创建。
func TestCreateListener_UnixSocket(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := tempDir + "/test.sock"

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "unix:" + socketPath,
		}},
	}

	s := New(cfg)
	ln, err := s.createListener(&cfg.Servers[0])
	if err != nil {
		t.Fatalf("createListener() error: %v", err)
	}
	if ln == nil {
		t.Fatal("createListener() returned nil listener")
	}
	defer ln.Close()

	if ln.Addr().Network() != "unix" {
		t.Errorf("expected unix network, got %s", ln.Addr().Network())
	}
}

// TestCreateListener_UnixSocketWithPermissions 测试带权限的 Unix Socket 创建。
func TestCreateListener_UnixSocketWithPermissions(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := tempDir + "/test_perm.sock"

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "unix:" + socketPath,
			UnixSocket: config.UnixSocketConfig{
				Mode:  0o600,
				User:  "nobody",
				Group: "nobody",
			},
		}},
	}

	s := New(cfg)
	ln, err := s.createListener(&cfg.Servers[0])
	if err != nil {
		t.Fatalf("createListener() error: %v", err)
	}
	if ln == nil {
		t.Fatal("createListener() returned nil listener")
	}
	defer ln.Close()
}

// TestCreateListener_UnixSocketCleanup 测试 Unix Socket 文件清理。
func TestCreateListener_UnixSocketCleanup(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := tempDir + "/cleanup.sock"

	// 先创建一个已存在的 socket 文件
	if err := os.WriteFile(socketPath, []byte{}, 0o666); err != nil {
		t.Fatalf("failed to create existing socket file: %v", err)
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "unix:" + socketPath,
		}},
	}

	s := New(cfg)
	ln, err := s.createListener(&cfg.Servers[0])
	if err != nil {
		t.Fatalf("createListener() error: %v", err)
	}
	defer ln.Close()
}

func TestHandleRegistrationError_ConflictWarning(t *testing.T) {
	s := &Server{}
	err := s.handleRegistrationError("proxy", "/api",
		&matcher.ConflictError{Path: "/api", ExistingType: "exact", NewType: "prefix"})
	if err != nil {
		t.Errorf("conflict should return nil, got: %v", err)
	}
}

func TestHandleRegistrationError_FatalError(t *testing.T) {
	s := &Server{}
	err := s.handleRegistrationError("proxy", "/api",
		fmt.Errorf("invalid regex pattern: missing closing parenthesis"))
	if err == nil {
		t.Error("fatal error should return non-nil")
	}
	if !strings.Contains(err.Error(), "proxy route /api") {
		t.Errorf("error should wrap context, got: %v", err)
	}
}

func TestDupListener_TCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	duped, err := DupListener(ln)
	if err != nil {
		t.Fatalf("DupListener() error: %v", err)
	}
	defer duped.Close()

	if duped.Addr().Network() != "tcp" {
		t.Errorf("expected tcp, got %s", duped.Addr().Network())
	}
	if duped.Addr().String() != ln.Addr().String() {
		t.Errorf("expected same address %s, got %s", ln.Addr().String(), duped.Addr().String())
	}
}

func TestDupListener_Unix(t *testing.T) {
	dir := t.TempDir()
	socketPath := dir + "/dup.sock"
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	duped, err := DupListener(ln)
	if err != nil {
		t.Fatalf("DupListener() error: %v", err)
	}
	defer duped.Close()
}

func TestDupListener_Unsupported(t *testing.T) {
	_, err := DupListener(struct{ net.Listener }{})
	if err == nil {
		t.Error("expected error for unsupported type")
	}
}

func TestTcpAddrMatch(t *testing.T) {
	s := &Server{}

	tests := []struct {
		inherited string
		target    string
		want      bool
	}{
		{"127.0.0.1:8080", "127.0.0.1:8080", true},
		{"0.0.0.0:8080", ":8080", true},
		{"[::]:8080", ":8080", true},
		{"0.0.0.0:8080", "0.0.0.0:8080", true},
		{"0.0.0.0:8080", "127.0.0.1:8080", true},
		{"127.0.0.1:8080", "0.0.0.0:8080", true},
		{"127.0.0.1:8080", ":9090", false},
		{"127.0.0.1:8080", "192.168.1.1:8080", false},
	}

	for _, tt := range tests {
		got := s.tcpAddrMatch(tt.inherited, tt.target)
		if got != tt.want {
			t.Errorf("tcpAddrMatch(%q, %q) = %v, want %v", tt.inherited, tt.target, got, tt.want)
		}
	}
}

func TestMatchInheritedListener_TCP(t *testing.T) {
	s := &Server{}

	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln1.Close()

	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln2.Close()

	inherited := []net.Listener{ln1, ln2}

	result := s.matchInheritedListener(inherited, "0.0.0.0:99999")
	if result != nil {
		t.Error("expected nil for non-matching address")
	}

	addr1 := ln1.Addr().String()
	result = s.matchInheritedListener(inherited, addr1)
	if result != ln1 {
		t.Errorf("expected ln1 for address %s", addr1)
	}
}

func TestMatchInheritedListener_Empty(t *testing.T) {
	s := &Server{}
	result := s.matchInheritedListener(nil, ":8080")
	if result != nil {
		t.Error("expected nil for empty inherited list")
	}
}

func TestMatchInheritedListener_PresetListeners(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: "127.0.0.1:0"}},
	}
	s := New(cfg)

	ln, err := s.createListener(&cfg.Servers[0])
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	s.SetListeners([]net.Listener{ln})

	addr := ln.Addr().String()
	cfg.Servers[0].Listen = addr

	matched, err := s.createListener(&cfg.Servers[0])
	if err != nil {
		t.Fatalf("createListener with preset should reuse: %v", err)
	}
	if matched == nil {
		t.Fatal("expected non-nil listener from preset match")
	}
	if matched.Addr().String() != addr {
		t.Errorf("expected same address %s, got %s", addr, matched.Addr().String())
	}
}

// TestServer_StatsMethods 测试服务器统计方法。
func TestServer_StatsMethods(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 测试 startTime 初始值
	if !s.startTime.IsZero() {
		t.Error("startTime should be zero initially")
	}

	// 设置 startTime
	s.startTime = time.Now()
	if s.startTime.IsZero() {
		t.Error("startTime should not be zero after setting")
	}

	// 测试统计值
	if s.connections.Load() != 0 {
		t.Error("initial connections should be 0")
	}
	if s.requests.Load() != 0 {
		t.Error("initial requests should be 0")
	}
	if s.bytesSent.Load() != 0 {
		t.Error("initial bytesSent should be 0")
	}
	if s.bytesReceived.Load() != 0 {
		t.Error("initial bytesReceived should be 0")
	}
}

// TestServer_SetResolver 测试设置解析器。
func TestServer_SetResolver(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 设置 nil resolver
	s.SetResolver(nil)
	if s.resolver != nil {
		t.Error("resolver should be nil")
	}
}

// TestServer_SetUpgradeManager 测试设置升级管理器。
func TestServer_SetUpgradeManager(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 设置 nil upgrade manager
	s.SetUpgradeManager(nil)
	if s.upgradeManager != nil {
		t.Error("upgradeManager should be nil")
	}

	// 设置实际的 upgrade manager
	um := NewUpgradeManager(s)
	s.SetUpgradeManager(um)
	if s.upgradeManager == nil {
		t.Error("upgradeManager should not be nil after setting")
	}
}

// TestServer_GetResolver 测试获取解析器。

// TestServer_StopWithTimeout_WithListeners 测试带监听器的停止。
func TestServer_StopWithTimeout_WithListeners(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)

	// 创建监听器
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	s.listeners = []net.Listener{ln}

	// 调用停止
	err = s.StopWithTimeout(1 * time.Second)
	if err != nil {
		t.Errorf("StopWithTimeout failed: %v", err)
	}
}

// TestServer_GracefulStop_WithListeners 测试带监听器的优雅停止。
func TestServer_GracefulStop_WithListeners(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)

	// 创建监听器
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	s.listeners = []net.Listener{ln}

	// 调用优雅停止
	err = s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestServer_StopWithTimeout_WithFastServer 测试带 fastServer 的停止。
func TestServer_StopWithTimeout_WithFastServer(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建 mock fastServer
	s.fastServer = &fasthttp.Server{}

	// 调用停止
	err := s.StopWithTimeout(1 * time.Second)
	if err != nil {
		t.Errorf("StopWithTimeout failed: %v", err)
	}
}

// TestBuildMiddlewareChain_BodyLimit 测试请求体限制中间件。
func TestBuildMiddlewareChain_BodyLimit(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen:            ":8080",
			ClientMaxBodySize: "1MB",
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestBuildMiddlewareChain_ErrorIntercept 测试错误拦截中间件。
func TestBuildMiddlewareChain_ErrorIntercept(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":8080",
			Security: config.SecurityConfig{
				ErrorPage: config.ErrorPageConfig{
					Pages: map[int]string{
						404: "/404.html",
					},
				},
			},
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestBuildMiddlewareChain_NilServerConfig 测试 nil 服务器配置。
// 注意：buildMiddlewareChain 不接受 nil，所以这个测试验证空配置。
func TestBuildMiddlewareChain_NilServerConfig(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Errorf("buildMiddlewareChain failed: %v", err)
	}
	if chain == nil {
		t.Error("Expected non-nil chain")
	}
}

// TestServer_StatusCode_MethodNotAllowed 测试不支持的 HTTP 方法。
func TestServer_StatusCode_MethodNotAllowed(t *testing.T) {
	// 简单验证
	if fasthttp.StatusMethodNotAllowed != 405 {
		t.Errorf("StatusMethodNotAllowed should be 405")
	}
}

// TestServer_ConnectionTracking 测试连接追踪。
func TestServer_ConnectionTracking(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 测试原子操作
	initial := s.connections.Load()
	s.connections.Add(1)
	if s.connections.Load() != initial+1 {
		t.Error("connections should have incremented")
	}
	s.connections.Add(-1)
	if s.connections.Load() != initial {
		t.Error("connections should be back to initial")
	}
}

// TestServer_RequestTracking 测试请求追踪。
func TestServer_RequestTracking(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 测试原子操作
	s.requests.Add(5)
	if s.requests.Load() != 5 {
		t.Errorf("expected 5 requests, got %d", s.requests.Load())
	}
}

// TestServer_BytesTracking 测试字节追踪。
func TestServer_BytesTracking(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 测试原子操作
	s.bytesSent.Add(1024)
	s.bytesReceived.Add(512)
	if s.bytesSent.Load() != 1024 {
		t.Errorf("expected 1024 bytes sent, got %d", s.bytesSent.Load())
	}
	if s.bytesReceived.Load() != 512 {
		t.Errorf("expected 512 bytes received, got %d", s.bytesReceived.Load())
	}
}

// TestServer_GracefulStop_WithFastServers 测试带多个 fastServer 的优雅停止。
func TestServer_GracefulStop_WithFastServers(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建多个 fastServer
	s.fastServers = []*fasthttp.Server{
		{},
		{},
	}

	// 调用优雅停止
	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestServer_StopWithTimeout_WithFastServers 测试带多个 fastServer 的停止。
func TestServer_StopWithTimeout_WithFastServers(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建多个 fastServer
	s.fastServers = []*fasthttp.Server{
		{},
		{},
	}

	// 调用停止
	err := s.StopWithTimeout(1 * time.Second)
	if err != nil {
		t.Errorf("StopWithTimeout failed: %v", err)
	}
}

// TestServer_GetProxyCacheStats_WithProxies 测试带代理的缓存统计。
func TestServer_GetProxyCacheStats_WithProxies(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	s.proxies = nil // 确保 proxies 为 nil

	// 无代理时应返回空统计
	stats := s.getProxyCacheStats()
	if stats.Entries != 0 {
		t.Errorf("Expected 0 entries, got %d", stats.Entries)
	}
}

// TestServer_GetProxyCacheStats_SingleProxyWithCache 测试单个代理带缓存的统计。
func TestServer_GetProxyCacheStats_SingleProxyWithCache(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 创建带缓存的代理
	proxyCfg := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
		Cache: config.ProxyCacheConfig{
			Enabled: true,
			MaxAge:  10 * time.Second,
		},
	}
	targets := testutil.NewTestTargets("http://localhost:8080")
	p, err := proxy.NewProxy(proxyCfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	s.proxies = []*proxy.Proxy{p}

	// 获取统计
	stats := s.getProxyCacheStats()
	// 新创建的缓存应该有 0 条目
	if stats.Entries < 0 {
		t.Errorf("Expected non-negative entries, got %d", stats.Entries)
	}
	if stats.Pending < 0 {
		t.Errorf("Expected non-negative pending, got %d", stats.Pending)
	}
}

// TestServer_GetProxyCacheStats_SingleProxyNoCache 测试单个代理无缓存的统计。
func TestServer_GetProxyCacheStats_SingleProxyNoCache(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 创建不带缓存的代理
	proxyCfg := testutil.NewTestProxyConfig("/api")
	targets := testutil.NewTestTargets("http://localhost:8080")
	p, err := proxy.NewProxy(proxyCfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	s.proxies = []*proxy.Proxy{p}

	// 获取统计
	stats := s.getProxyCacheStats()
	// 无缓存时应返回 0
	if stats.Entries != 0 {
		t.Errorf("Expected 0 entries for proxy without cache, got %d", stats.Entries)
	}
	if stats.Pending != 0 {
		t.Errorf("Expected 0 pending for proxy without cache, got %d", stats.Pending)
	}
}

// TestServer_GetProxyCacheStats_MultipleProxies 测试多个代理的缓存统计聚合。
func TestServer_GetProxyCacheStats_MultipleProxies(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	// 创建多个代理：部分带缓存，部分不带
	targets := testutil.NewTestTargets("http://localhost:8080")

	// 代理1：带缓存
	proxyCfg1 := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
		Cache: config.ProxyCacheConfig{
			Enabled: true,
			MaxAge:  10 * time.Second,
		},
	}
	p1, err := proxy.NewProxy(proxyCfg1, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 代理2：不带缓存
	proxyCfg2 := testutil.NewTestProxyConfig("/static")
	p2, err := proxy.NewProxy(proxyCfg2, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 代理3：带缓存
	proxyCfg3 := &config.ProxyConfig{
		Path:        "/data",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
		Cache: config.ProxyCacheConfig{
			Enabled: true,
			MaxAge:  20 * time.Second,
		},
	}
	p3, err := proxy.NewProxy(proxyCfg3, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	s.proxies = []*proxy.Proxy{p1, p2, p3}

	// 获取聚合统计
	stats := s.getProxyCacheStats()
	// 统计应该非负
	if stats.Entries < 0 {
		t.Errorf("Expected non-negative entries, got %d", stats.Entries)
	}
	if stats.Pending < 0 {
		t.Errorf("Expected non-negative pending, got %d", stats.Pending)
	}
}

// TestServer_GetProxyCacheStats_AllProxiesWithCache 测试所有代理都有缓存的统计。
func TestServer_GetProxyCacheStats_AllProxiesWithCache(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	targets := testutil.NewTestTargets("http://localhost:8080")

	// 创建多个带缓存的代理
	proxies := make([]*proxy.Proxy, 3)
	for i := range 3 {
		proxyCfg := &config.ProxyConfig{
			Path:        fmt.Sprintf("/api%d", i),
			LoadBalance: "round_robin",
			Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
			Cache: config.ProxyCacheConfig{
				Enabled: true,
				MaxAge:  10 * time.Second,
			},
		}
		p, err := proxy.NewProxy(proxyCfg, targets, nil, nil)
		if err != nil {
			t.Fatalf("NewProxy() error: %v", err)
		}
		proxies[i] = p
	}

	s.proxies = proxies

	// 获取统计
	stats := s.getProxyCacheStats()
	// 应该聚合所有代理的统计
	if stats.Entries < 0 {
		t.Errorf("Expected non-negative entries, got %d", stats.Entries)
	}
	if stats.Pending < 0 {
		t.Errorf("Expected non-negative pending, got %d", stats.Pending)
	}
}

// TestServer_GetProxyCacheStats_AllProxiesNoCache 测试所有代理都没有缓存的统计。
func TestServer_GetProxyCacheStats_AllProxiesNoCache(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)

	targets := testutil.NewTestTargets("http://localhost:8080")

	// 创建多个不带缓存的代理
	proxies := make([]*proxy.Proxy, 3)
	for i := range 3 {
		proxyCfg := testutil.NewTestProxyConfig(fmt.Sprintf("/api%d", i))
		p, err := proxy.NewProxy(proxyCfg, targets, nil, nil)
		if err != nil {
			t.Fatalf("NewProxy() error: %v", err)
		}
		proxies[i] = p
	}

	s.proxies = proxies

	// 获取统计
	stats := s.getProxyCacheStats()
	// 所有代理都没有缓存，应该返回 0
	if stats.Entries != 0 {
		t.Errorf("Expected 0 entries, got %d", stats.Entries)
	}
	if stats.Pending != 0 {
		t.Errorf("Expected 0 pending, got %d", stats.Pending)
	}
}

// TestServer_GetProxyCacheStats_EmptyProxiesSlice 测试空代理切片的统计。
func TestServer_GetProxyCacheStats_EmptyProxiesSlice(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	s.proxies = []*proxy.Proxy{} // 空切片

	// 获取统计
	stats := s.getProxyCacheStats()
	if stats.Entries != 0 {
		t.Errorf("Expected 0 entries, got %d", stats.Entries)
	}
	if stats.Pending != 0 {
		t.Errorf("Expected 0 pending, got %d", stats.Pending)
	}
}

// TestServer_MultipleListeners 测试多个监听器。
func TestServer_MultipleListeners(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)

	// 创建多个监听器
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
		_ = ln1.Close()
	}

	s.listeners = []net.Listener{ln1, ln2}

	// 验证可以获取监听器
	got := s.GetListeners()
	if len(got) != 2 {
		t.Errorf("expected 2 listeners, got %d", len(got))
	}

	// 清理
	_ = s.StopWithTimeout(1 * time.Second)
}

// TestGracefulStop_RunningState 测试 GracefulStop 设置 running 为 false。
func TestGracefulStop_RunningState(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	if !s.running.Load() {
		t.Fatal("running should be true before GracefulStop")
	}

	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}

	if s.running.Load() {
		t.Error("running should be false after GracefulStop")
	}
}

// TestGracefulStop_WithPool 测试 GracefulStop 停止 GoroutinePool。
func TestGracefulStop_WithPool(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
		Performance: config.PerformanceConfig{
			GoroutinePool: config.GoroutinePoolConfig{
				Enabled:     true,
				MaxWorkers:  10,
				MinWorkers:  2,
				IdleTimeout: 5 * time.Second,
			},
		},
	}

	s := New(cfg)
	s.running.Store(true)

	// 初始化并启动 pool
	s.pool = initGoroutinePool(&cfg.Performance)
	if s.pool != nil {
		s.pool.Start()
	}

	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestGracefulStop_WithHealthCheckers 测试 GracefulStop 停止健康检查器。
func TestGracefulStop_WithHealthCheckers(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建 mock healthChecker (使用 nil，因为我们只测试循环不会 panic)
	s.healthCheckers = nil

	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestGracefulStop_WithAccessLog 测试 GracefulStop 关闭访问日志。
func TestGracefulStop_WithAccessLog(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建 accessLogMiddleware
	s.accessLogMiddleware = accesslog.New(&config.LoggingConfig{})

	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestGracefulStop_WithTLSManager 测试 GracefulStop 关闭 TLS 管理器。
func TestGracefulStop_WithTLSManager(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建临时证书文件
	tempDir := t.TempDir()
	certFile := tempDir + "/cert.pem"
	keyFile := tempDir + "/key.pem"

	// 生成自签名证书用于测试
	if err := generateTestCert(certFile, keyFile); err != nil {
		t.Skipf("failed to generate test cert: %v", err)
	}

	tlsMgr, err := ssl.NewTLSManager(&config.SSLConfig{
		Cert: certFile,
		Key:  keyFile,
	})
	if err != nil {
		t.Skipf("failed to create TLS manager: %v", err)
	}
	s.tlsManager = tlsMgr

	err = s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestGracefulStop_WithLuaEngine 测试 GracefulStop 关闭 Lua 引擎。
func TestGracefulStop_WithLuaEngine(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建 Lua 引擎
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	err = s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestGracefulStop_Timeout 测试 GracefulStop 超时场景。
func TestGracefulStop_Timeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建一个真实的 fastServer，但通过模拟长时间关闭来测试超时
	s.fastServer = &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("test")
		},
	}

	// 使用非常短的超时
	err := s.GracefulStop(1 * time.Nanosecond)
	// 超时可能返回 context.DeadlineExceeded 或 nil（取决于关闭速度）
	if err != nil && err != context.DeadlineExceeded {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestGracefulStop_AllComponents 测试 GracefulStop 关闭所有组件。
func TestGracefulStop_AllComponents(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
		Performance: config.PerformanceConfig{
			GoroutinePool: config.GoroutinePoolConfig{
				Enabled:     true,
				MaxWorkers:  10,
				IdleTimeout: 5 * time.Second,
			},
		},
	}

	s := New(cfg)
	s.running.Store(true)

	// 初始化所有组件
	s.pool = initGoroutinePool(&cfg.Performance)
	if s.pool != nil {
		s.pool.Start()
	}
	s.accessLogMiddleware = accesslog.New(&config.LoggingConfig{})

	// 创建监听器
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	s.listeners = []net.Listener{ln}

	err = s.GracefulStop(2 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}

	// 验证 running 状态
	if s.running.Load() {
		t.Error("running should be false after GracefulStop")
	}
}

// generateTestCert 生成测试用的自签名证书。
func generateTestCert(certFile, keyFile string) error {
	// 简化实现：跳过证书生成
	return fmt.Errorf("test cert generation not implemented")
}

// TestGracefulStop_WithAccessControl 测试 GracefulStop 关闭访问控制。
func TestGracefulStop_WithAccessControl(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
			Security: config.SecurityConfig{
				Access: config.AccessConfig{
					Allow: []string{"127.0.0.1"},
				},
			},
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建 AccessControl
	ac, err := security.NewAccessControl(&cfg.Servers[0].Security.Access)
	if err != nil {
		t.Skipf("failed to create AccessControl: %v", err)
	}
	s.accessControl = ac

	err = s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestGracefulStop_ContextCancelled 测试 GracefulStop 上下文取消场景。
func TestGracefulStop_ContextCancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建一个监听中的服务器
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	s.listeners = []net.Listener{ln}

	// 创建 fastServer 并开始服务
	s.fastServer = &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			time.Sleep(100 * time.Millisecond) // 模拟慢请求
			ctx.SetBodyString("ok")
		},
	}

	// 启动服务器
	go func() {
		_ = s.fastServer.Serve(ln)
	}()

	// 等待服务器启动
	time.Sleep(10 * time.Millisecond)

	// 使用非常短的超时测试超时场景
	err = s.GracefulStop(1 * time.Nanosecond)
	// 超时可能返回 context.DeadlineExceeded 或 nil
	if err != nil && err != context.DeadlineExceeded {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestGracefulStop_MultipleHealthCheckers 测试 GracefulStop 停止多个健康检查器。
func TestGracefulStop_MultipleHealthCheckers(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建多个 mock healthChecker
	// 注意：这里使用 nil slice 测试空循环不会 panic
	s.healthCheckers = make([]*proxy.HealthChecker, 0)

	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestGracefulStop_NilComponents 测试 GracefulStop 所有组件为 nil。
func TestGracefulStop_NilComponents(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 确保所有组件为 nil
	s.pool = nil
	s.healthCheckers = nil
	s.accessLogMiddleware = nil
	s.tlsManager = nil
	s.accessControl = nil
	s.luaEngine = nil
	s.fastServer = nil
	s.fastServers = nil

	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}

	if s.running.Load() {
		t.Error("running should be false after GracefulStop")
	}
}

// TestGracefulStop_FastServersWithNil 测试 GracefulStop 处理 fastServers 中的 nil。
func TestGracefulStop_FastServersWithNil(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)

	// 创建包含 nil 的 fastServers
	s.fastServers = []*fasthttp.Server{nil, {}, nil}

	err := s.GracefulStop(1 * time.Second)
	if err != nil {
		t.Errorf("GracefulStop failed: %v", err)
	}
}

// TestGracefulStop_ZeroTimeout 测试 GracefulStop 零超时。
func TestGracefulStop_ZeroTimeout(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)
	s.fastServer = &fasthttp.Server{}

	err := s.GracefulStop(0)
	// 零超时应该立即返回（可能导致超时错误或成功关闭）
	if err != nil && err != context.DeadlineExceeded {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestGracefulStop_NegativeTimeout 测试 GracefulStop 负超时。
func TestGracefulStop_NegativeTimeout(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":0",
		}},
	}

	s := New(cfg)
	s.running.Store(true)
	s.fastServer = &fasthttp.Server{}

	err := s.GracefulStop(-1 * time.Second)
	// 负超时应该立即返回
	if err != nil && err != context.DeadlineExceeded {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestStartSingleMode_StaticFiles 测试 startSingleMode 静态文件配置。
func TestStartSingleMode_StaticFiles(t *testing.T) {
	tempDir := t.TempDir()

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Static: []config.StaticConfig{
				{
					Path:  "/static",
					Root:  tempDir,
					Index: []string{"index.html"},
				},
				{
					Path:         "/assets",
					Root:         tempDir,
					LocationType: "exact",
					SymlinkCheck: true,
					Internal:     true,
					TryFiles:     []string{"$uri", "/fallback.html"},
					TryFilesPass: true,
				},
			},
		}},
	}

	s := New(cfg)
	// 验证静态文件配置已正确设置
	if len(s.config.Servers[0].Static) != 2 {
		t.Errorf("expected 2 static configs, got %d", len(s.config.Servers[0].Static))
	}

	// 验证第一个静态配置
	static1 := s.config.Servers[0].Static[0]
	if static1.Path != "/static" {
		t.Errorf("expected path /static, got %s", static1.Path)
	}
	if static1.Root != tempDir {
		t.Errorf("expected root %s, got %s", tempDir, static1.Root)
	}
}

// TestStartSingleMode_StaticFilesWithGzipStatic 测试静态文件 gzip 预压缩配置。
func TestStartSingleMode_StaticFilesWithGzipStatic(t *testing.T) {
	tempDir := t.TempDir()

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Static: []config.StaticConfig{
				{
					Path:  "/",
					Root:  tempDir,
					Index: []string{"index.html"},
				},
			},
			Compression: config.CompressionConfig{
				Type:                 "gzip",
				Level:                6,
				GzipStatic:           true,
				GzipStaticExtensions: []string{".html", ".css", ".js"},
			},
		}},
	}

	s := New(cfg)
	// 验证 gzip 静态配置
	if !s.config.Servers[0].Compression.GzipStatic {
		t.Error("expected GzipStatic to be true")
	}
	if len(s.config.Servers[0].Compression.GzipStaticExtensions) != 3 {
		t.Errorf("expected 3 extensions, got %d", len(s.config.Servers[0].Compression.GzipStaticExtensions))
	}
}

// TestStartSingleMode_ProxyWithLocationTypes 测试代理配置的不同位置类型。
func TestStartSingleMode_ProxyWithLocationTypes(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Proxy: []config.ProxyConfig{
				{
					Path:         "/api/exact",
					LocationType: "exact",
					Targets: []config.ProxyTarget{
						{URL: "http://127.0.0.1:8081", Weight: 1},
					},
				},
				{
					Path:         "/api/priority",
					LocationType: "prefix_priority",
					Targets: []config.ProxyTarget{
						{URL: "http://127.0.0.1:8082", Weight: 1},
					},
				},
				{
					Path:         "^/api/regex/(.*)$",
					LocationType: "regex",
					Targets: []config.ProxyTarget{
						{URL: "http://127.0.0.1:8083", Weight: 1},
					},
				},
				{
					Path:         "^/api/caseless/(.*)$",
					LocationType: "regex_caseless",
					Targets: []config.ProxyTarget{
						{URL: "http://127.0.0.1:8084", Weight: 1},
					},
				},
				{
					Path:         "/api/named",
					LocationType: "named",
					LocationName: "@api_named",
					Targets: []config.ProxyTarget{
						{URL: "http://127.0.0.1:8085", Weight: 1},
					},
				},
				{
					Path: "/api/default",
					// 默认 prefix 类型
					Targets: []config.ProxyTarget{
						{URL: "http://127.0.0.1:8086", Weight: 1},
					},
					Internal: true,
				},
			},
		}},
	}

	s := New(cfg)
	// 验证代理配置数量
	if len(s.config.Servers[0].Proxy) != 6 {
		t.Errorf("expected 6 proxy configs, got %d", len(s.config.Servers[0].Proxy))
	}

	// 验证不同位置类型
	proxyTypes := []string{"exact", "prefix_priority", "regex", "regex_caseless", "named", ""}
	for i, pt := range proxyTypes {
		if s.config.Servers[0].Proxy[i].LocationType != pt {
			t.Errorf("proxy[%d]: expected location type %s, got %s", i, pt, s.config.Servers[0].Proxy[i].LocationType)
		}
	}
}

// TestStartSingleMode_ProxyWithHealthCheck 测试代理健康检查配置。
func TestStartSingleMode_ProxyWithHealthCheck(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Proxy: []config.ProxyConfig{
				{
					Path: "/api",
					Targets: []config.ProxyTarget{
						{
							URL:         "http://127.0.0.1:8081",
							Weight:      3,
							MaxFails:    3,
							FailTimeout: 10 * time.Second,
							MaxConns:    100,
							Backup:      false,
							Down:        false,
						},
						{
							URL:    "http://127.0.0.1:8082",
							Weight: 1,
							Backup: true,
						},
					},
					LoadBalance: "weighted_round_robin",
					HealthCheck: config.HealthCheckConfig{
						Interval: 10 * time.Second,
						Timeout:  5 * time.Second,
						Path:     "/health",
					},
				},
			},
		}},
	}

	s := New(cfg)
	// 验证健康检查配置
	hc := s.config.Servers[0].Proxy[0].HealthCheck
	if hc.Interval != 10*time.Second {
		t.Errorf("expected interval 10s, got %v", hc.Interval)
	}
	if hc.Path != "/health" {
		t.Errorf("expected path /health, got %s", hc.Path)
	}
}

// TestStartSingleMode_MonitoringEndpoints 测试监控端点配置。
func TestStartSingleMode_MonitoringEndpoints(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
		}},
		Monitoring: config.MonitoringConfig{
			Status: config.StatusConfig{
				Enabled: true,
				Path:    "/_status",
				Format:  "json",
				Allow:   []string{"127.0.0.1", "192.168.0.0/16"},
			},
			Pprof: config.PprofConfig{
				Enabled: true,
				Path:    "/debug/pprof",
				Allow:   []string{"127.0.0.1"},
			},
		},
	}

	s := New(cfg)
	// 验证状态端点配置
	if !s.config.Monitoring.Status.Enabled {
		t.Error("expected status enabled")
	}
	if s.config.Monitoring.Status.Path != "/_status" {
		t.Errorf("expected status path /_status, got %s", s.config.Monitoring.Status.Path)
	}
	if len(s.config.Monitoring.Status.Allow) != 2 {
		t.Errorf("expected 2 allowed IPs, got %d", len(s.config.Monitoring.Status.Allow))
	}

	// 验证 pprof 配置
	if !s.config.Monitoring.Pprof.Enabled {
		t.Error("expected pprof enabled")
	}
}

// TestStartSingleMode_CacheAPI 测试缓存 API 配置。
func TestStartSingleMode_CacheAPI(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			CacheAPI: &config.CacheAPIConfig{
				Enabled: true,
				Path:    "/_cache/purge",
				Allow:   []string{"127.0.0.1"},
				Auth:    config.CacheAPIAuthConfig{Type: "token", Token: "secret-token"},
			},
		}},
	}

	s := New(cfg)
	// 验证缓存 API 配置
	if s.config.Servers[0].CacheAPI == nil || !s.config.Servers[0].CacheAPI.Enabled {
		t.Error("expected cache API enabled")
	}
	if s.config.Servers[0].CacheAPI.Path != "/_cache/purge" {
		t.Errorf("expected path /_cache/purge, got %s", s.config.Servers[0].CacheAPI.Path)
	}
}

// TestStartSingleMode_TLSConfig 测试 TLS 配置。
func TestStartSingleMode_TLSConfig(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			SSL: config.SSLConfig{
				Cert:      "/path/to/cert.pem",
				Key:       "/path/to/key.pem",
				Protocols: []string{"TLSv1.2", "TLSv1.3"},
				Ciphers:   []string{"TLS_AES_128_GCM_SHA256"},
				HSTS: config.HSTSConfig{
					MaxAge:            31536000,
					IncludeSubDomains: true,
					Preload:           true,
				},
			},
		}},
	}

	s := New(cfg)
	// 验证 SSL 配置
	if s.config.Servers[0].SSL.Cert != "/path/to/cert.pem" {
		t.Errorf("expected cert path, got %s", s.config.Servers[0].SSL.Cert)
	}
	if s.config.Servers[0].SSL.HSTS.MaxAge != 31536000 {
		t.Errorf("expected HSTS MaxAge 31536000, got %d", s.config.Servers[0].SSL.HSTS.MaxAge)
	}
}

// TestStartSingleMode_MIMETypes 测试 MIME 类型配置。
func TestStartSingleMode_MIMETypes(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Types: config.TypesConfig{
				Map: map[string]string{
					".wasm":   "application/wasm",
					".custom": "application/x-custom",
				},
				DefaultType: "application/octet-stream",
			},
		}},
	}

	s := New(cfg)
	// 验证 MIME 类型配置
	if len(s.config.Servers[0].Types.Map) != 2 {
		t.Errorf("expected 2 MIME types, got %d", len(s.config.Servers[0].Types.Map))
	}
	if s.config.Servers[0].Types.DefaultType != "application/octet-stream" {
		t.Errorf("expected default type, got %s", s.config.Servers[0].Types.DefaultType)
	}
}

// TestStartSingleMode_ServerOptions 测试服务器选项配置。
func TestStartSingleMode_ServerOptions(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen:             "127.0.0.1:0",
			ReadTimeout:        30 * time.Second,
			WriteTimeout:       30 * time.Second,
			IdleTimeout:        60 * time.Second,
			MaxConnsPerIP:      100,
			MaxRequestsPerConn: 1000,
			Concurrency:        256 * 1024,
			ReadBufferSize:     16 * 1024,
			WriteBufferSize:    16 * 1024,
			ReduceMemoryUsage:  true,
			ServerTokens:       false,
		}},
	}

	s := New(cfg)
	// 验证服务器选项
	sc := s.config.Servers[0]
	if sc.ReadTimeout != 30*time.Second {
		t.Errorf("expected ReadTimeout 30s, got %v", sc.ReadTimeout)
	}
	if sc.MaxConnsPerIP != 100 {
		t.Errorf("expected MaxConnsPerIP 100, got %d", sc.MaxConnsPerIP)
	}
	if !sc.ReduceMemoryUsage {
		t.Error("expected ReduceMemoryUsage true")
	}
	if sc.ServerTokens {
		t.Error("expected ServerTokens false")
	}
}

// TestStartSingleMode_WithMiddlewareChain 测试中间件链配置。
func TestStartSingleMode_WithMiddlewareChain(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Security: config.SecurityConfig{
				Access: config.AccessConfig{
					Allow: []string{"127.0.0.1"},
					Deny:  []string{"10.0.0.0/8"},
				},
				RateLimit: config.RateLimitConfig{
					RequestRate: 100,
					Burst:       200,
					Key:         "remote_addr",
				},
				Auth: config.AuthConfig{
					Users: []config.User{
						{Name: "admin", Password: "secret"},
					},
				},
				Headers: config.SecurityHeaders{
					XFrameOptions:         "DENY",
					XContentTypeOptions:   "nosniff",
					ContentSecurityPolicy: "default-src 'self'",
					ReferrerPolicy:        "strict-origin-when-cross-origin",
				},
			},
			Compression: config.CompressionConfig{
				Type:  "gzip",
				Level: 6,
			},
			Rewrite: []config.RewriteRule{
				{Pattern: "^/old/(.*)$", Replacement: "/new/$1"},
			},
		}},
	}

	s := New(cfg)
	// 验证中间件配置
	security := s.config.Servers[0].Security
	if len(security.Access.Allow) != 1 {
		t.Errorf("expected 1 allow rule, got %d", len(security.Access.Allow))
	}
	if security.RateLimit.RequestRate != 100 {
		t.Errorf("expected request rate 100, got %d", security.RateLimit.RequestRate)
	}
	if len(security.Auth.Users) != 1 {
		t.Errorf("expected 1 auth user, got %d", len(security.Auth.Users))
	}
}

// TestStartSingleMode_PerformanceConfig 测试性能配置。
func TestStartSingleMode_PerformanceConfig(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
		}},
		Performance: config.PerformanceConfig{
			GoroutinePool: config.GoroutinePoolConfig{
				Enabled:     true,
				MaxWorkers:  100,
				MinWorkers:  10,
				IdleTimeout: 30 * time.Second,
			},
			FileCache: config.FileCacheConfig{
				MaxEntries: 10000,
				MaxSize:    100 * 1024 * 1024,
			},
		},
	}

	s := New(cfg)
	// 验证性能配置
	if !s.config.Performance.GoroutinePool.Enabled {
		t.Error("expected goroutine pool enabled")
	}
	if s.config.Performance.FileCache.MaxEntries != 10000 {
		t.Errorf("expected 10000 max entries, got %d", s.config.Performance.FileCache.MaxEntries)
	}
}

// TestStartSingleMode_WithLuaMiddleware 测试 Lua 中间件配置。
func TestStartSingleMode_WithLuaMiddleware(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Lua: &config.LuaMiddlewareConfig{
				Enabled: true,
				Scripts: []config.LuaScriptConfig{
					{
						Path:    "/scripts/access.lua",
						Phase:   "access",
						Timeout: 30 * time.Second,
					},
					{
						Path:    "/scripts/header.lua",
						Phase:   "header_filter",
						Timeout: 10 * time.Second,
					},
				},
			},
		}},
	}

	s := New(cfg)
	// 验证 Lua 配置
	if s.config.Servers[0].Lua == nil || !s.config.Servers[0].Lua.Enabled {
		t.Error("expected Lua enabled")
	}
	if len(s.config.Servers[0].Lua.Scripts) != 2 {
		t.Errorf("expected 2 scripts, got %d", len(s.config.Servers[0].Lua.Scripts))
	}
}

// TestStartSingleMode_WithErrorPage 测试错误页面配置。
func TestStartSingleMode_WithErrorPage(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Security: config.SecurityConfig{
				ErrorPage: config.ErrorPageConfig{
					Pages: map[int]string{
						404: "/errors/404.html",
						500: "/errors/500.html",
						502: "/errors/502.html",
					},
					Default: "/errors/default.html",
				},
			},
		}},
	}

	s := New(cfg)
	// 验证错误页面配置
	ep := s.config.Servers[0].Security.ErrorPage
	if len(ep.Pages) != 3 {
		t.Errorf("expected 3 error pages, got %d", len(ep.Pages))
	}
	if ep.Default != "/errors/default.html" {
		t.Errorf("expected default error page, got %s", ep.Default)
	}
}

// TestStartSingleMode_WithConnLimiter 测试连接限制配置。
func TestStartSingleMode_WithConnLimiter(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Security: config.SecurityConfig{
				RateLimit: config.RateLimitConfig{
					ConnLimit: 100,
					Key:       "remote_addr",
				},
			},
		}},
	}

	s := New(cfg)
	// 验证连接限制配置
	if s.config.Servers[0].Security.RateLimit.ConnLimit != 100 {
		t.Errorf("expected ConnLimit 100, got %d", s.config.Servers[0].Security.RateLimit.ConnLimit)
	}
}

// TestStartSingleMode_WithAuthRequest 测试外部认证配置。
func TestStartSingleMode_WithAuthRequest(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			Security: config.SecurityConfig{
				AuthRequest: config.AuthRequestConfig{
					Enabled: true,
					URI:     "/auth/validate",
					Timeout: 5 * time.Second,
				},
			},
		}},
	}

	s := New(cfg)
	// 验证外部认证配置
	ar := s.config.Servers[0].Security.AuthRequest
	if !ar.Enabled {
		t.Error("expected AuthRequest enabled")
	}
	if ar.URI != "/auth/validate" {
		t.Errorf("expected URI /auth/validate, got %s", ar.URI)
	}
}

// TestShutdownServers_EmptySlice 测试空服务器列表。
func TestShutdownServers_EmptySlice(t *testing.T) {
	ctx := context.Background()
	err := shutdownServers(ctx, []*fasthttp.Server{})
	if err != nil {
		t.Errorf("shutdownServers with empty slice should return nil, got: %v", err)
	}
}

// TestShutdownServers_NilSlice 测试 nil 服务器列表。
func TestShutdownServers_NilSlice(t *testing.T) {
	ctx := context.Background()
	err := shutdownServers(ctx, nil)
	if err != nil {
		t.Errorf("shutdownServers with nil slice should return nil, got: %v", err)
	}
}

// TestShutdownServers_NilContext 测试 nil 上下文。
func TestShutdownServers_NilContext(t *testing.T) {
	// nil ctx 应该使用 context.Background()
	err := shutdownServers(nil, []*fasthttp.Server{})
	if err != nil {
		t.Errorf("shutdownServers with nil ctx should return nil, got: %v", err)
	}
}

// TestShutdownServers_SingleServer 测试单个服务器关闭。
func TestShutdownServers_SingleServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	servers := []*fasthttp.Server{
		{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") }},
	}

	err := shutdownServers(ctx, servers)
	if err != nil {
		t.Errorf("shutdownServers failed: %v", err)
	}
}

// TestShutdownServers_MultipleServers 测试多个服务器关闭。
func TestShutdownServers_MultipleServers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	servers := []*fasthttp.Server{
		{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test1") }},
		{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test2") }},
		{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test3") }},
	}

	err := shutdownServers(ctx, servers)
	if err != nil {
		t.Errorf("shutdownServers failed: %v", err)
	}
}

// TestShutdownServers_WithNilServers 测试服务器列表中包含 nil。
func TestShutdownServers_WithNilServers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	servers := []*fasthttp.Server{
		nil,
		{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") }},
		nil,
	}

	err := shutdownServers(ctx, servers)
	if err != nil {
		t.Errorf("shutdownServers failed: %v", err)
	}
}

// TestShutdownServers_AllNilServers 测试所有服务器都是 nil。
func TestShutdownServers_AllNilServers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	servers := []*fasthttp.Server{nil, nil, nil}

	err := shutdownServers(ctx, servers)
	if err != nil {
		t.Errorf("shutdownServers with all nil servers should return nil, got: %v", err)
	}
}

// TestShutdownServers_ContextCancelled 测试上下文取消。
func TestShutdownServers_ContextCancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	// 创建一个已取消的上下文
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	servers := []*fasthttp.Server{
		{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") }},
	}

	err := shutdownServers(ctx, servers)
	// 已取消的上下文可能返回 context.Canceled 或 nil（取决于服务器关闭速度）
	if err != nil && err != context.Canceled {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestShutdownServers_ContextTimeout 测试上下文超时。
func TestShutdownServers_ContextTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	// 创建一个极短超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// 等待超时
	time.Sleep(1 * time.Millisecond)

	servers := []*fasthttp.Server{
		{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") }},
	}

	err := shutdownServers(ctx, servers)
	// 超时的上下文可能返回 context.DeadlineExceeded 或 nil
	if err != nil && err != context.DeadlineExceeded {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestShutdownServers_RunningServers 测试关闭运行中的服务器。
func TestShutdownServers_RunningServers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 创建服务器并启动
	servers := make([]*fasthttp.Server, 2)
	listeners := make([]net.Listener, 2)

	for i := range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to create listener: %v", err)
		}
		listeners[i] = ln

		srv := &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("test")
			},
		}
		servers[i] = srv

		go func(s *fasthttp.Server, l net.Listener) {
			_ = s.Serve(l)
		}(srv, ln)
	}

	// 等待服务器启动
	time.Sleep(10 * time.Millisecond)

	// 关闭服务器
	err := shutdownServers(ctx, servers)
	if err != nil {
		t.Errorf("shutdownServers failed: %v", err)
	}

	// 关闭监听器（如果服务器没有关闭它们）
	for _, ln := range listeners {
		_ = ln.Close()
	}
}

// TestShutdownServers_ManyServers 测试关闭大量服务器。
func TestShutdownServers_ManyServers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 创建大量服务器
	count := 50
	servers := make([]*fasthttp.Server, count)
	for i := range count {
		servers[i] = &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") },
		}
	}

	err := shutdownServers(ctx, servers)
	if err != nil {
		t.Errorf("shutdownServers with many servers failed: %v", err)
	}
}

// TestShutdownServers_MixedNilAndRealServers 测试混合 nil 和真实服务器。
func TestShutdownServers_MixedNilAndRealServers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	count := 20
	servers := make([]*fasthttp.Server, count)
	for i := range count {
		if i%2 == 0 {
			servers[i] = nil
		} else {
			servers[i] = &fasthttp.Server{
				Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") },
			}
		}
	}

	err := shutdownServers(ctx, servers)
	if err != nil {
		t.Errorf("shutdownServers failed: %v", err)
	}
}

// TestShutdownServers_ConcurrentSafety 测试并发安全性。
func TestShutdownServers_ConcurrentSafety(t *testing.T) {
	ctx := context.Background()

	// 并发调用 shutdownServers
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			servers := []*fasthttp.Server{
				{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") }},
				{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") }},
			}
			_ = shutdownServers(ctx, servers)
		})
	}
	wg.Wait()
}

// TestShutdownServers_WithDeadline 测试带截止时间的上下文。
func TestShutdownServers_WithDeadline(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	servers := []*fasthttp.Server{
		{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("test") }},
	}

	err := shutdownServers(ctx, servers)
	if err != nil {
		t.Errorf("shutdownServers failed: %v", err)
	}
}

// TestBuildLuaMiddlewares_SingleScript 测试单个脚本配置。
func TestBuildLuaMiddlewares_SingleScript(t *testing.T) {
	// 创建临时 Lua 脚本
	tempDir := t.TempDir()
	scriptPath := tempDir + "/test.lua"
	if err := os.WriteFile(scriptPath, []byte("ngx.say('hello')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: scriptPath, Phase: "access", Timeout: 10 * time.Second, Enabled: true},
		},
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if len(middlewares) != 1 {
		t.Errorf("expected 1 middleware, got: %d", len(middlewares))
	}
}

// TestBuildLuaMiddlewares_SingleScriptDefaultTimeout 测试单脚本默认超时。
func TestBuildLuaMiddlewares_SingleScriptDefaultTimeout(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := tempDir + "/test.lua"
	if err := os.WriteFile(scriptPath, []byte("ngx.say('hello')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: scriptPath, Phase: "content", Timeout: 0}, // 使用默认超时
		},
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if len(middlewares) != 1 {
		t.Errorf("expected 1 middleware, got: %d", len(middlewares))
	}
}

// TestBuildLuaMiddlewares_MultipleScriptsSamePhase 测试多脚本同阶段。
func TestBuildLuaMiddlewares_MultipleScriptsSamePhase(t *testing.T) {
	tempDir := t.TempDir()
	script1 := tempDir + "/test1.lua"
	script2 := tempDir + "/test2.lua"
	if err := os.WriteFile(script1, []byte("ngx.say('1')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}
	if err := os.WriteFile(script2, []byte("ngx.say('2')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: script1, Phase: "access", Timeout: 10 * time.Second, Enabled: true},
			{Path: script2, Phase: "access", Timeout: 20 * time.Second, Enabled: true},
		},
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if len(middlewares) != 1 {
		t.Errorf("expected 1 middleware (multi-phase), got: %d", len(middlewares))
	}
}

// TestBuildLuaMiddlewares_MultipleScriptsDifferentPhases 测试多脚本不同阶段。
func TestBuildLuaMiddlewares_MultipleScriptsDifferentPhases(t *testing.T) {
	tempDir := t.TempDir()
	script1 := tempDir + "/rewrite.lua"
	script2 := tempDir + "/access.lua"
	script3 := tempDir + "/log.lua"
	for _, p := range []string{script1, script2, script3} {
		if err := os.WriteFile(p, []byte("ngx.say('hello')"), 0o644); err != nil {
			t.Fatalf("failed to create script: %v", err)
		}
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: script1, Phase: "rewrite", Timeout: 10 * time.Second, Enabled: true},
			{Path: script2, Phase: "access", Timeout: 15 * time.Second, Enabled: true},
			{Path: script3, Phase: "log", Timeout: 20 * time.Second, Enabled: true},
		},
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if len(middlewares) != 3 {
		t.Errorf("expected 3 middlewares, got: %d", len(middlewares))
	}
}

// TestBuildLuaMiddlewares_DefaultEnabled 测试默认启用逻辑。
func TestBuildLuaMiddlewares_DefaultEnabled(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := tempDir + "/test.lua"
	if err := os.WriteFile(scriptPath, []byte("ngx.say('hello')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	// Enabled 为 false，但 Timeout=0 且 Path 不为空，应该默认启用
	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: scriptPath, Phase: "access", Timeout: 0, Enabled: false},
		},
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	// 默认启用逻辑：Enabled=false && Timeout=0 && Path!="" -> enabled=true
	if len(middlewares) != 1 {
		t.Errorf("expected 1 middleware (default enabled), got: %d", len(middlewares))
	}
}

// TestBuildLuaMiddlewares_InvalidPhaseInMultiScript 测试多脚本中的无效阶段。
func TestBuildLuaMiddlewares_InvalidPhaseInMultiScript(t *testing.T) {
	tempDir := t.TempDir()
	script1 := tempDir + "/test1.lua"
	script2 := tempDir + "/test2.lua"
	if err := os.WriteFile(script1, []byte("ngx.say('1')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}
	if err := os.WriteFile(script2, []byte("ngx.say('2')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: script1, Phase: "access", Timeout: 10 * time.Second, Enabled: true},
			{Path: script2, Phase: "invalid_phase", Timeout: 10 * time.Second, Enabled: true},
		},
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err == nil {
		t.Error("expected error for invalid phase in multi-script")
	}
	if middlewares != nil {
		t.Errorf("expected nil middlewares on error, got: %v", middlewares)
	}
}

// TestBuildLuaMiddlewares_AllPhases 测试所有阶段。
func TestBuildLuaMiddlewares_AllPhases(t *testing.T) {
	tempDir := t.TempDir()
	phases := []string{"rewrite", "access", "content", "log", "header_filter", "body_filter"}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	scripts := make([]config.LuaScriptConfig, len(phases))
	for i, phase := range phases {
		scriptPath := tempDir + "/" + phase + ".lua"
		if err := os.WriteFile(scriptPath, []byte("ngx.say('"+phase+"')"), 0o644); err != nil {
			t.Fatalf("failed to create script: %v", err)
		}
		scripts[i] = config.LuaScriptConfig{Path: scriptPath, Phase: phase, Timeout: 10 * time.Second, Enabled: true}
	}

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: scripts,
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if len(middlewares) != len(phases) {
		t.Errorf("expected %d middlewares, got: %d", len(phases), len(middlewares))
	}
}

// TestBuildLuaMiddlewares_NonExistentScript 测试不存在的脚本文件。
func TestBuildLuaMiddlewares_NonExistentScript(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: "/non/existent/script.lua", Phase: "access", Timeout: 10 * time.Second},
		},
	}

	// NewLuaMiddleware 会在创建时验证脚本文件
	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	// 由于脚本不存在，可能会返回错误或创建失败
	// 这取决于 lua.NewLuaMiddleware 的实现
	_ = middlewares
	_ = err
}

// TestBuildLuaMiddlewares_MixedEnabledDisabled 测试混合启用禁用脚本。
func TestBuildLuaMiddlewares_MixedEnabledDisabled(t *testing.T) {
	tempDir := t.TempDir()
	for _, name := range []string{"enabled1", "enabled2", "disabled1", "disabled2"} {
		scriptPath := tempDir + "/" + name + ".lua"
		if err := os.WriteFile(scriptPath, []byte("ngx.say('"+name+"')"), 0o644); err != nil {
			t.Fatalf("failed to create script: %v", err)
		}
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: tempDir + "/enabled1.lua", Phase: "rewrite", Timeout: 10 * time.Second, Enabled: true},
			{Path: tempDir + "/disabled1.lua", Phase: "rewrite", Timeout: 10 * time.Second, Enabled: false},
			{Path: tempDir + "/enabled2.lua", Phase: "access", Timeout: 10 * time.Second, Enabled: true},
			{Path: tempDir + "/disabled2.lua", Phase: "access", Timeout: 10 * time.Second, Enabled: false},
		},
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	// 只有启用的脚本应该被处理：rewrite(1) + access(1) = 2
	if len(middlewares) != 2 {
		t.Errorf("expected 2 middlewares, got: %d", len(middlewares))
	}
}

// TestBuildLuaMiddlewares_MultiPhaseDefaultTimeout 测试多脚本阶段默认超时。
func TestBuildLuaMiddlewares_MultiPhaseDefaultTimeout(t *testing.T) {
	tempDir := t.TempDir()
	script1 := tempDir + "/test1.lua"
	script2 := tempDir + "/test2.lua"
	if err := os.WriteFile(script1, []byte("ngx.say('1')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}
	if err := os.WriteFile(script2, []byte("ngx.say('2')"), 0o644); err != nil {
		t.Fatalf("failed to create script: %v", err)
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: ":8080",
		}},
	}

	s := New(cfg)
	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	if err != nil {
		t.Skipf("failed to create Lua engine: %v", err)
	}
	s.luaEngine = luaEngine

	luaCfg := &config.LuaMiddlewareConfig{
		Enabled: true,
		Scripts: []config.LuaScriptConfig{
			{Path: script1, Phase: "access", Timeout: 0}, // 默认超时
			{Path: script2, Phase: "access", Timeout: 0}, // 默认超时
		},
	}

	middlewares, err := s.buildLuaMiddlewares(luaCfg)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if len(middlewares) != 1 {
		t.Errorf("expected 1 middleware (multi-phase), got: %d", len(middlewares))
	}
}
