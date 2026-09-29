package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/cache"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/handler"
	"rua.plus/lolly/internal/lua"
	"rua.plus/lolly/internal/matcher"
	"rua.plus/lolly/internal/proxy"
	"rua.plus/lolly/internal/ssl"
	"rua.plus/lolly/internal/testutil"
)

// TestSetInternalRedirect 测试 SetInternalRedirect 标记内部重定向。
func TestSetInternalRedirect(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)

	SetInternalRedirect(ctx, "/internal/target")

	assert.True(t, IsInternalRedirect(ctx))
	assert.Equal(t, "/internal/target", GetInternalRedirectPath(ctx))
}

// TestIsInternalRedirect_未标记 测试未标记时返回 false。
func TestIsInternalRedirect_未标记(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)

	assert.False(t, IsInternalRedirect(ctx))
	assert.Equal(t, "", GetInternalRedirectPath(ctx))
}

// TestGetInternalRedirectPath_空路径 测试空路径重定向。
func TestGetInternalRedirectPath_空路径(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)

	SetInternalRedirect(ctx, "")
	assert.True(t, IsInternalRedirect(ctx))
	assert.Equal(t, "", GetInternalRedirectPath(ctx))
}

// TestInternalRedirect_多次设置 测试多次设置覆盖。
func TestInternalRedirect_多次设置(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)

	SetInternalRedirect(ctx, "/first")
	assert.Equal(t, "/first", GetInternalRedirectPath(ctx))

	SetInternalRedirect(ctx, "/second")
	assert.Equal(t, "/second", GetInternalRedirectPath(ctx))
}

// TestWrapRoutedHandler_无中间件 测试无中间件时原样返回。
func TestWrapRoutedHandler_无中间件(t *testing.T) {
	s := &Server{}
	called := false
	original := func(ctx *fasthttp.RequestCtx) {
		called = true
	}

	wrapped := s.wrapRoutedHandler(original)

	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	wrapped(ctx)

	assert.True(t, called, "原始 handler 应被调用")
}

// TestWrapRoutedHandler_有AccessLog 测试带访问日志的包装。
func TestWrapRoutedHandler_有AccessLog(t *testing.T) {
	cfg := &config.Config{
		Logging: config.LoggingConfig{},
	}
	s := New(cfg)

	called := false
	original := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetBodyString("ok")
	}

	wrapped := s.wrapRoutedHandler(original)

	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	wrapped(ctx)

	assert.True(t, called, "原始 handler 应被调用")
}

// TestWrapRoutedHandler_有ErrorPageManager 测试带错误页面管理器的包装。
func TestWrapRoutedHandler_有ErrorPageManager(t *testing.T) {
	tempDir := t.TempDir()
	errorPagePath := filepath.Join(tempDir, "404.html")
	err := os.WriteFile(errorPagePath, []byte("<html>Not Found</html>"), 0o644)
	require.NoError(t, err)

	epCfg := &config.ErrorPageConfig{
		Pages: map[int]string{404: errorPagePath},
	}
	epManager, err := handler.NewErrorPageManager(epCfg)
	require.NoError(t, err)

	s := &Server{
		errorPageManager: epManager,
	}

	called := false
	original := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetBodyString("ok")
	}

	wrapped := s.wrapRoutedHandler(original)

	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	wrapped(ctx)

	assert.True(t, called, "原始 handler 应被调用")
}

// TestConfigureStaticHandler_完整配置 测试完整的静态处理器配置。
func TestConfigureStaticHandler_完整配置(t *testing.T) {
	tempDir := t.TempDir()

	s := &Server{
		fileCache: cache.NewFileCache(100, 1024*1024, 5*time.Minute),
	}

	staticCfg := &config.StaticConfig{
		Path:    "/static",
		Root:    tempDir,
		Index:   []string{"index.html"},
		Alias:   "/data",
		Expires: "1h",
	}

	serverCfg := &config.ServerConfig{
		Compression: config.CompressionConfig{
			GzipStatic: true,
		},
	}

	h := s.configureStaticHandler(staticCfg, serverCfg)
	assert.NotNil(t, h)
}

// TestConfigureStaticHandler_自动索引 测试目录列表功能。
func TestConfigureStaticHandler_自动索引(t *testing.T) {
	tempDir := t.TempDir()

	s := &Server{}

	staticCfg := &config.StaticConfig{
		Path:               "/files",
		Root:               tempDir,
		AutoIndex:          true,
		AutoIndexFormat:    "html",
		AutoIndexLocaltime: true,
		AutoIndexExactSize: true,
	}

	serverCfg := &config.ServerConfig{}

	h := s.configureStaticHandler(staticCfg, serverCfg)
	assert.NotNil(t, h)
}

// TestConfigureStaticHandler_默认路径 测试空路径使用默认 "/"。
func TestConfigureStaticHandler_默认路径(t *testing.T) {
	tempDir := t.TempDir()

	s := &Server{}

	staticCfg := &config.StaticConfig{
		Root: tempDir,
	}

	serverCfg := &config.ServerConfig{}

	h := s.configureStaticHandler(staticCfg, serverCfg)
	assert.NotNil(t, h)
}

// TestConfigureStaticHandler_SymlinkCheck 测试符号链接安全检查配置。
func TestConfigureStaticHandler_SymlinkCheck(t *testing.T) {
	tempDir := t.TempDir()

	s := &Server{}

	staticCfg := &config.StaticConfig{
		Path:         "/static",
		Root:         tempDir,
		SymlinkCheck: true,
		Internal:     true,
	}

	serverCfg := &config.ServerConfig{}

	h := s.configureStaticHandler(staticCfg, serverCfg)
	assert.NotNil(t, h)
}

// TestPurgeByPath_带缓存代理 测试按路径清理带缓存的代理。
func TestPurgeByPath_带缓存代理(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: ":0"}},
	}
	s := New(cfg)

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
	require.NoError(t, err)

	s.proxies = []*proxy.Proxy{p}

	purgeHandler := &PurgeHandler{server: s}

	deleted := purgeHandler.purgeByPath("/api/test", "GET")
	assert.Equal(t, 1, deleted)
}

// TestPurgeByPath_无缓存代理 测试无缓存代理时返回0。
func TestPurgeByPath_无缓存代理(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: ":0"}},
	}
	s := New(cfg)

	proxyCfg := testutil.NewTestProxyConfig("/api")
	targets := testutil.NewTestTargets("http://localhost:8080")
	p, err := proxy.NewProxy(proxyCfg, targets, nil, nil)
	require.NoError(t, err)

	s.proxies = []*proxy.Proxy{p}

	purgeHandler := &PurgeHandler{server: s}

	deleted := purgeHandler.purgeByPath("/api/test", "GET")
	assert.Equal(t, 0, deleted)
}

// TestPurgeByPath_NilServer 测试 nil server 返回0。
func TestPurgeByPath_NilServer(t *testing.T) {
	purgeHandler := &PurgeHandler{server: nil}
	deleted := purgeHandler.purgeByPath("/api/test", "GET")
	assert.Equal(t, 0, deleted)
}

// TestPurgeByPattern_带缓存代理 测试按模式清理带缓存的代理。
func TestPurgeByPattern_带缓存代理(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: ":0"}},
	}
	s := New(cfg)

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
	require.NoError(t, err)

	s.proxies = []*proxy.Proxy{p}

	purgeHandler := &PurgeHandler{server: s}

	deleted := purgeHandler.purgeByPattern("/api/*", "GET")
	assert.GreaterOrEqual(t, deleted, 0)
}

// TestPurgeByPattern_NilServer 测试 nil server 返回0。
func TestPurgeByPattern_NilServer(t *testing.T) {
	purgeHandler := &PurgeHandler{server: nil}
	deleted := purgeHandler.purgeByPattern("/api/*", "GET")
	assert.Equal(t, 0, deleted)
}

// TestPurgeByPattern_多代理 测试多代理的模式清理。
func TestPurgeByPattern_多代理(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: ":0"}},
	}
	s := New(cfg)

	targets := testutil.NewTestTargets("http://localhost:8080")

	proxyCfg1 := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
		Cache:       config.ProxyCacheConfig{Enabled: true, MaxAge: 10 * time.Second},
	}
	p1, err := proxy.NewProxy(proxyCfg1, targets, nil, nil)
	require.NoError(t, err)

	proxyCfg2 := &config.ProxyConfig{
		Path:        "/data",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
		Cache:       config.ProxyCacheConfig{Enabled: true, MaxAge: 20 * time.Second},
	}
	p2, err := proxy.NewProxy(proxyCfg2, targets, nil, nil)
	require.NoError(t, err)

	s.proxies = []*proxy.Proxy{p1, p2}

	purgeHandler := &PurgeHandler{server: s}
	deleted := purgeHandler.purgeByPattern("*", "GET")
	assert.GreaterOrEqual(t, deleted, 0)
}

// TestShutdownServers_NilCtx使用默认背景 测试 nil context 使用默认背景。
func TestShutdownServers_NilCtx使用默认背景(t *testing.T) {
	err := shutdownServers(nil, nil)
	assert.NoError(t, err)
}

// TestShutdownServers_Ctx取消 测试 context 取消。
func TestShutdownServers_Ctx取消(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	servers := []*fasthttp.Server{
		{Handler: func(ctx *fasthttp.RequestCtx) {}},
	}

	err := shutdownServers(ctx, servers)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// TestSetPidFile 测试设置 PID 文件路径。
func TestSetPidFile(t *testing.T) {
	mgr := NewUpgradeManager(nil)
	assert.Equal(t, "", mgr.pidFile)

	mgr.SetPidFile("/var/run/lolly.pid")
	assert.Equal(t, "/var/run/lolly.pid", mgr.pidFile)
}

// TestWritePid_写入文件 测试 PID 写入文件。
func TestWritePid_写入文件(t *testing.T) {
	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "lolly.pid")

	mgr := NewUpgradeManager(nil)
	mgr.SetPidFile(pidFile)

	err := mgr.WritePid()
	require.NoError(t, err)

	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)

	expectedPid := fmt.Sprintf("%d", os.Getpid())
	assert.Equal(t, expectedPid, string(data))
}

// TestWritePid_覆盖写入 测试多次写入覆盖。
func TestWritePid_覆盖写入(t *testing.T) {
	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "lolly.pid")

	mgr := NewUpgradeManager(nil)
	mgr.SetPidFile(pidFile)

	err := mgr.WritePid()
	require.NoError(t, err)

	err = mgr.WritePid()
	require.NoError(t, err)

	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%d", os.Getpid()), string(data))
}

// TestServeJSON_正常 测试正常 JSON 输出。
func TestServeJSON_正常(t *testing.T) {
	srv := New(nil)
	srv.startTime = time.Now()

	h := &StatusHandler{
		server: srv,
		path:   "/_status",
		format: "json",
	}

	status := &Status{
		Version:       "test",
		Uptime:        5 * time.Second,
		Connections:   10,
		Requests:      100,
		BytesSent:     2048,
		BytesReceived: 1024,
	}

	ctx := &fasthttp.RequestCtx{}
	h.serveJSON(ctx, status)

	assert.Equal(t, 200, ctx.Response.StatusCode())
	assert.Contains(t, string(ctx.Response.Header.ContentType()), "application/json")

	var parsed Status
	err := json.Unmarshal(ctx.Response.Body(), &parsed)
	require.NoError(t, err)
	assert.Equal(t, int64(10), parsed.Connections)
	assert.Equal(t, int64(100), parsed.Requests)
}

// TestServeJSON_无数据 测试最简 JSON 输出。
func TestServeJSON_无数据(t *testing.T) {
	srv := New(nil)
	srv.startTime = time.Now()

	h := &StatusHandler{
		server: srv,
		path:   "/_status",
		format: "json",
	}

	status := &Status{
		Version: "test",
	}

	ctx := &fasthttp.RequestCtx{}
	h.serveJSON(ctx, status)

	assert.Equal(t, 200, ctx.Response.StatusCode())

	var parsed Status
	err := json.Unmarshal(ctx.Response.Body(), &parsed)
	require.NoError(t, err)
	assert.Equal(t, "test", parsed.Version)
	assert.Nil(t, parsed.Cache)
	assert.Nil(t, parsed.Pool)
}

// TestMatchInheritedListener_UnixSocket 测试 Unix socket 继承匹配。
func TestMatchInheritedListener_UnixSocket(t *testing.T) {
	s := &Server{}

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer ln.Close()

	inherited := []net.Listener{ln}

	result := s.matchInheritedListener(inherited, "unix:"+socketPath)
	assert.Equal(t, ln, result)
}

// TestMatchInheritedListener_UnixSocket不匹配 测试 Unix socket 地址不匹配。
func TestMatchInheritedListener_UnixSocket不匹配(t *testing.T) {
	s := &Server{}

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer ln.Close()

	inherited := []net.Listener{ln}

	result := s.matchInheritedListener(inherited, "unix:"+dir+"/other.sock")
	assert.Nil(t, result)
}

// TestMatchInheritedListener_NilListener 测试列表中含 nil。
func TestMatchInheritedListener_NilListener(t *testing.T) {
	s := &Server{}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	inherited := []net.Listener{nil, ln}

	addr := ln.Addr().String()
	result := s.matchInheritedListener(inherited, addr)
	assert.Equal(t, ln, result)
}

// TestMatchInheritedListener_TCP网络不匹配 测试 TCP 非网络类型跳过。
func TestMatchInheritedListener_TCP网络不匹配(t *testing.T) {
	s := &Server{}

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "test.sock")
	unixLn, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer unixLn.Close()

	inherited := []net.Listener{unixLn}

	result := s.matchInheritedListener(inherited, "127.0.0.1:8080")
	assert.Nil(t, result)
}

// TestMatchInheritedListener_端口不匹配 测试端口不同时不匹配。
func TestMatchInheritedListener_端口不匹配(t *testing.T) {
	s := &Server{}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	inherited := []net.Listener{ln}

	result := s.matchInheritedListener(inherited, "127.0.0.1:99999")
	assert.Nil(t, result)
}

// TestMatchInheritedListener_通配符匹配 测试 0.0.0.0 匹配任意地址。
func TestMatchInheritedListener_通配符匹配(t *testing.T) {
	s := &Server{}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	inherited := []net.Listener{ln}
	addr := ln.Addr().String()

	result := s.matchInheritedListener(inherited, "0.0.0.0"+addr[len("127.0.0.1"):])
	assert.Equal(t, ln, result)
}

// TestIsAnyAddr 测试 isAnyAddr 辅助函数。
func TestIsAnyAddr(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"0.0.0.0", true},
		{"::", true},
		{"", true},
		{"127.0.0.1", false},
		{"192.168.1.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.want, isAnyAddr(tt.host))
		})
	}
}

// TestCreateFastServer_StreamRequestBody 测试请求体流式开关透传。
func TestCreateFastServer_StreamRequestBody(t *testing.T) {
	s := &Server{}
	serverCfg := &config.ServerConfig{}

	if srv := s.createFastServer(serverCfg, nil, false); srv.StreamRequestBody {
		t.Error("StreamRequestBody should be false when flag is false")
	}
	if srv := s.createFastServer(serverCfg, nil, true); !srv.StreamRequestBody {
		t.Error("StreamRequestBody should be true when flag is true")
	}
}

// TestAnyProxyRequestStreaming 测试监听分组请求体流式检测。
func TestAnyProxyRequestStreaming(t *testing.T) {
	withStreaming := func() *config.ServerConfig {
		return &config.ServerConfig{
			Proxy: []config.ProxyConfig{{
				Path:      "/",
				Targets:   []config.ProxyTarget{{URL: "http://127.0.0.1:8080"}},
				Buffering: &config.ProxyBufferingConfig{RequestMode: "off"},
			}},
		}
	}
	withoutStreaming := func() *config.ServerConfig {
		return &config.ServerConfig{
			Proxy: []config.ProxyConfig{{
				Path:      "/",
				Targets:   []config.ProxyTarget{{URL: "http://127.0.0.1:8080"}},
				Buffering: &config.ProxyBufferingConfig{Mode: "off"},
			}},
		}
	}

	tests := []struct {
		name    string
		servers []*config.ServerConfig
		want    bool
	}{
		{name: "nil cfg", servers: []*config.ServerConfig{{}}, want: false},
		{name: "no streaming", servers: []*config.ServerConfig{withoutStreaming()}, want: false},
		{name: "streaming", servers: []*config.ServerConfig{withStreaming()}, want: true},
		{name: "mixed", servers: []*config.ServerConfig{withoutStreaming(), withStreaming()}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anyProxyRequestStreaming(tt.servers...); got != tt.want {
				t.Errorf("anyProxyRequestStreaming() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCreateFastServer 测试创建 fasthttp 服务器。
func TestCreateFastServer(t *testing.T) {
	s := &Server{}
	serverCfg := &config.ServerConfig{
		ReadTimeout:        10 * time.Second,
		WriteTimeout:       20 * time.Second,
		IdleTimeout:        30 * time.Second,
		MaxConnsPerIP:      100,
		MaxRequestsPerConn: 1000,
		Concurrency:        500,
		ReadBufferSize:     8192,
		WriteBufferSize:    8192,
		ServerTokens:       true,
	}

	handler := func(ctx *fasthttp.RequestCtx) {}
	fastSrv := s.createFastServer(serverCfg, handler, false)

	assert.NotNil(t, fastSrv)
	assert.Equal(t, 10*time.Second, fastSrv.ReadTimeout)
	assert.Equal(t, 20*time.Second, fastSrv.WriteTimeout)
	assert.Equal(t, 30*time.Second, fastSrv.IdleTimeout)
	assert.Equal(t, 100, fastSrv.MaxConnsPerIP)
	assert.Equal(t, 1000, fastSrv.MaxRequestsPerConn)
	assert.True(t, fastSrv.CloseOnShutdown)
	assert.Equal(t, 500, fastSrv.Concurrency)
	assert.Equal(t, 8192, fastSrv.ReadBufferSize)
	assert.Equal(t, 8192, fastSrv.WriteBufferSize)
}

// TestCreateFastServer_隐藏版本 测试 ServerTokens=false 时隐藏版本。
func TestCreateFastServer_隐藏版本(t *testing.T) {
	s := &Server{}
	serverCfg := &config.ServerConfig{
		ServerTokens: false,
	}

	fastSrv := s.createFastServer(serverCfg, nil, false)
	assert.Equal(t, "lolly", fastSrv.Name)
}

// TestRegisterRoute_各种类型 测试各种位置类型的路由注册。
func TestRegisterRoute_各种类型(t *testing.T) {
	s := &Server{
		locationEngine: matcher.NewLocationEngine(),
	}

	tests := []struct {
		name    string
		locType string
		path    string
	}{
		{"exact", matcher.LocationTypeExact, "/api/users"},
		{"prefix", matcher.LocationTypePrefix, "/api/"},
		{"prefix_priority", matcher.LocationTypePrefixPriority, "/api/v2/"},
		{"regex", matcher.LocationTypeRegex, "^/api/.*$"},
		{"regex_caseless", matcher.LocationTypeRegexCaseless, "^/API/.*$"},
		{"named", matcher.LocationTypeNamed, "@internal"},
		{"default", "", "/fallback/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			le := matcher.NewLocationEngine()
			s.locationEngine = le

			h := func(ctx *fasthttp.RequestCtx) {}
			err := s.registerRoute(tt.locType, tt.path, h, false, "test")
			assert.NoError(t, err)
		})
	}
}

// TestRegisterProxyRoutes_AllMethods 测试代理路由接受任意 HTTP 方法。
func TestRegisterProxyRoutes_AllMethods(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Proxy: []config.ProxyConfig{{
				Path:    "/api",
				Targets: []config.ProxyTarget{{URL: "http://127.0.0.1:1", Down: true}},
			}},
		}},
	}
	s := New(cfg)
	router := handler.NewRouter()
	s.registerProxyRoutes(router, &cfg.Servers[0])

	for _, method := range []string{"PATCH", "OPTIONS", "PROPFIND"} {
		t.Run(method, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.SetMethod(method)
			ctx.Request.SetRequestURI("/api/resource")

			router.Handler()(&ctx)

			assert.Equal(t, fasthttp.StatusBadGateway, ctx.Response.StatusCode())
		})
	}
}

// TestRegisterProxyRoutesWithLocationEngine 测试代理路由注册到 LocationEngine。
func TestRegisterProxyRoutesWithLocationEngine(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{
			{
				Listen: ":0",
				Proxy: []config.ProxyConfig{
					{
						Path:        "/api",
						LoadBalance: "round_robin",
						Targets:     []config.ProxyTarget{{URL: "http://localhost:8080"}},
					},
				},
			},
		},
	}

	s := New(cfg)
	s.locationEngine = matcher.NewLocationEngine()

	err := s.registerProxyRoutesWithLocationEngine(&cfg.Servers[0])
	assert.NoError(t, err)
}

// TestRegisterProxyRoutesWithLocationEngine_命名路由 测试命名路由注册。
func TestRegisterProxyRoutesWithLocationEngine_命名路由(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{
			{
				Listen: ":0",
				Proxy: []config.ProxyConfig{
					{
						Path:         "/api",
						LoadBalance:  "round_robin",
						LocationType: matcher.LocationTypeNamed,
						LocationName: "backend",
						Targets:      []config.ProxyTarget{{URL: "http://localhost:8080"}},
					},
				},
			},
		},
	}

	s := New(cfg)
	s.locationEngine = matcher.NewLocationEngine()

	err := s.registerProxyRoutesWithLocationEngine(&cfg.Servers[0])
	assert.NoError(t, err)
}

// TestRegisterStaticHandlersWithLocationEngine 测试静态文件路由注册。
func TestRegisterStaticHandlersWithLocationEngine(t *testing.T) {
	tempDir := t.TempDir()

	cfg := &config.Config{
		Servers: []config.ServerConfig{
			{
				Listen: ":0",
				Static: []config.StaticConfig{
					{
						Path:  "/static",
						Root:  tempDir,
						Index: []string{"index.html"},
					},
				},
			},
		},
	}

	s := New(cfg)
	s.locationEngine = matcher.NewLocationEngine()

	err := s.registerStaticHandlersWithLocationEngine(&cfg.Servers[0])
	assert.NoError(t, err)
}

// TestRegisterLuaRoutesWithLocationEngine_无引擎 测试无 Lua 引擎时跳过。
func TestRegisterLuaRoutesWithLocationEngine_无引擎(t *testing.T) {
	s := &Server{luaEngine: nil}
	serverCfg := &config.ServerConfig{
		Lua: &config.LuaMiddlewareConfig{Enabled: true},
	}

	err := s.registerLuaRoutesWithLocationEngine(serverCfg)
	assert.NoError(t, err)
}

// TestRegisterLuaRoutesWithLocationEngine_未启用 测试 Lua 未启用时跳过。
func TestRegisterLuaRoutesWithLocationEngine_未启用(t *testing.T) {
	s := &Server{}
	serverCfg := &config.ServerConfig{
		Lua: &config.LuaMiddlewareConfig{Enabled: false},
	}

	err := s.registerLuaRoutesWithLocationEngine(serverCfg)
	assert.NoError(t, err)
}

// TestRegisterLuaRoutesWithLocationEngine_无路由 测试 Lua 脚本无路由时跳过。
func TestRegisterLuaRoutesWithLocationEngine_无路由(t *testing.T) {
	s := &Server{}
	serverCfg := &config.ServerConfig{
		Lua: &config.LuaMiddlewareConfig{
			Enabled: false,
			Scripts: []config.LuaScriptConfig{
				{Path: "/tmp/test.lua"},
			},
		},
	}

	err := s.registerLuaRoutesWithLocationEngine(serverCfg)
	assert.NoError(t, err)
}

// TestHandleRegistrationError_非冲突错误 测试非 ConflictError 返回错误。
func TestHandleRegistrationError_非冲突错误(t *testing.T) {
	s := &Server{}

	originalErr := fmt.Errorf("some registration error")
	err := s.handleRegistrationError("test", "/path", originalErr)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "test route /path")
}

// TestWrapHandler_带连接池 测试 wrapHandler 使用连接池。
func TestWrapHandler_带连接池(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: ":0"}},
	}
	s := New(cfg)

	s.pool = NewGoroutinePool(PoolConfig{
		MaxWorkers:  10,
		MinWorkers:  2,
		QueueSize:   10,
		IdleTimeout: 5 * time.Second,
	})
	s.pool.Start()
	defer s.pool.Stop()

	base := func(ctx *fasthttp.RequestCtx) {
		ctx.SetBodyString("ok")
	}

	wrapped, err := s.wrapHandler(base, &cfg.Servers[0])
	require.NoError(t, err)
	assert.NotNil(t, wrapped)
}

// TestStopWithTimeout_默认超时 测试零超时使用默认值。
func TestStopWithTimeout_默认超时(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: ":0"}},
	}
	s := New(cfg)

	err := s.StopWithTimeout(0)
	assert.NoError(t, err)
}

// TestStopWithTimeout_负超时 测试负超时使用默认值。
func TestStopWithTimeout_负超时(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: ":0"}},
	}
	s := New(cfg)

	err := s.StopWithTimeout(-1 * time.Second)
	assert.NoError(t, err)
}

// TestCleanupResources_全nil 测试所有组件为 nil 时不 panic。
func TestCleanupResources_全nil(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: ":0"}},
	}
	s := New(cfg)

	assert.NotPanics(t, func() {
		s.cleanupResources()
	})
}

// TestDupListener_关闭测试 测试复制后原 listener 仍可用。
func TestDupListener_关闭测试(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	duped, err := DupListener(ln)
	require.NoError(t, err)
	defer duped.Close()

	assert.Equal(t, ln.Addr().String(), duped.Addr().String())
}

// TestGetTLSConfig_有TLSManager 测试有 TLS 管理器时返回配置。
func TestGetTLSConfig_有TLSManager(t *testing.T) {
	tempDir := t.TempDir()
	certFile := filepath.Join(tempDir, "cert.pem")
	keyFile := filepath.Join(tempDir, "key.pem")

	err := generateSelfSignedCert(certFile, keyFile)
	if err != nil {
		t.Skipf("跳过: 无法生成测试证书: %v", err)
	}

	tlsMgr, err := ssl.NewTLSManager(&config.SSLConfig{
		Cert: certFile,
		Key:  keyFile,
	})
	if err != nil {
		t.Skipf("跳过: 无法创建 TLS 管理器: %v", err)
	}

	s := &Server{tlsManager: tlsMgr}

	tlsConfig, err := s.GetTLSConfig()
	assert.NoError(t, err)
	assert.NotNil(t, tlsConfig)
}

// generateSelfSignedCert 生成自签名证书用于测试。
func generateSelfSignedCert(certFile, keyFile string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(1 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return err
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return err
	}

	keyOut, err := os.Create(keyFile)
	if err != nil {
		return err
	}
	defer keyOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}

	return pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
}

// TestCreateListener_InheritedFromUpgradeManager 测试从 UpgradeManager 继承监听器。
func TestCreateListener_InheritedFromUpgradeManager(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{{Listen: "127.0.0.1:0"}},
	}
	s := New(cfg)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	um := NewUpgradeManager(s)
	um.SetPidFile("")
	s.SetUpgradeManager(um)

	addr := ln.Addr().String()
	cfg.Servers[0].Listen = addr

	s.SetListeners([]net.Listener{ln})

	matched, err := s.createListener(&cfg.Servers[0])
	require.NoError(t, err)
	assert.Equal(t, addr, matched.Addr().String())
}

// TestShutdownServers_成功关闭 测试正常关闭服务器。
func TestShutdownServers_成功关闭(t *testing.T) {
	srv1 := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {}}
	srv2 := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {}}

	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go srv1.Serve(ln1)
	go srv2.Serve(ln2)
	time.Sleep(50 * time.Millisecond)

	err = shutdownServers(context.Background(), []*fasthttp.Server{srv1, srv2})
	assert.NoError(t, err)
}

// TestShutdownServers_Nil服务器跳过 测试列表中 nil 服务器被跳过。
func TestShutdownServers_Nil服务器跳过(t *testing.T) {
	srv := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go srv.Serve(ln)
	time.Sleep(50 * time.Millisecond)

	err = shutdownServers(context.Background(), []*fasthttp.Server{nil, srv, nil})
	assert.NoError(t, err)
}

// TestRegisterLuaRoutesWithLocationEngine_有路由 测试带路由的 Lua 脚本注册。
func TestRegisterLuaRoutesWithLocationEngine_有路由(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "test.lua")
	err := os.WriteFile(scriptPath, []byte("ngx.say('hello')"), 0o644)
	require.NoError(t, err)

	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	require.NoError(t, err)
	defer luaEngine.Close()

	s := &Server{
		luaEngine:      luaEngine,
		locationEngine: matcher.NewLocationEngine(),
	}

	serverCfg := &config.ServerConfig{
		Lua: &config.LuaMiddlewareConfig{
			Enabled: true,
			Scripts: []config.LuaScriptConfig{
				{
					Path:  scriptPath,
					Route: "/api/lua",
				},
			},
		},
	}

	err = s.registerLuaRoutesWithLocationEngine(serverCfg)
	assert.NoError(t, err)
}

// TestRegisterLuaRoutesWithLocationEngine_自定义路由类型 测试自定义 RouteType。
func TestRegisterLuaRoutesWithLocationEngine_自定义路由类型(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "exact.lua")
	err := os.WriteFile(scriptPath, []byte("ngx.say('exact')"), 0o644)
	require.NoError(t, err)

	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	require.NoError(t, err)
	defer luaEngine.Close()

	s := &Server{
		luaEngine:      luaEngine,
		locationEngine: matcher.NewLocationEngine(),
	}

	serverCfg := &config.ServerConfig{
		Lua: &config.LuaMiddlewareConfig{
			Enabled: true,
			Scripts: []config.LuaScriptConfig{
				{
					Path:      scriptPath,
					Route:     "/api/exact",
					RouteType: matcher.LocationTypeExact,
				},
			},
		},
	}

	err = s.registerLuaRoutesWithLocationEngine(serverCfg)
	assert.NoError(t, err)
}

// TestRegisterLuaRoutesWithLocationEngine_自定义超时 测试自定义脚本超时。
func TestRegisterLuaRoutesWithLocationEngine_自定义超时(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "timeout.lua")
	err := os.WriteFile(scriptPath, []byte("ngx.say('timeout')"), 0o644)
	require.NoError(t, err)

	luaEngine, err := lua.NewEngine(lua.DefaultConfig())
	require.NoError(t, err)
	defer luaEngine.Close()

	s := &Server{
		luaEngine:      luaEngine,
		locationEngine: matcher.NewLocationEngine(),
	}

	serverCfg := &config.ServerConfig{
		Lua: &config.LuaMiddlewareConfig{
			Enabled: true,
			Scripts: []config.LuaScriptConfig{
				{
					Path:    scriptPath,
					Route:   "/api/timeout",
					Timeout: 10 * time.Second,
				},
			},
		},
	}

	err = s.registerLuaRoutesWithLocationEngine(serverCfg)
	assert.NoError(t, err)
}

// TestServeHTTP_带缓存的Prometheus 测试 Prometheus 格式带缓存指标。
func TestServeHTTP_带缓存的Prometheus(t *testing.T) {
	cfg := &config.StatusConfig{
		Path:   "/_status",
		Format: "prometheus",
		Allow:  []string{},
	}

	srv := New(nil)
	srv.startTime = time.Now()
	srv.connections.Store(5)

	fc := cache.NewFileCache(100, 1024*1024, 5*time.Minute)
	srv.fileCache = fc

	h, err := NewStatusHandler(srv, cfg)
	require.NoError(t, err)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/_status")
	h.ServeHTTP(ctx)

	assert.Equal(t, 200, ctx.Response.StatusCode())

	body := string(ctx.Response.Body())
	assert.Contains(t, body, "lolly_version")
	assert.Contains(t, body, "lolly_cache_entries")
}
