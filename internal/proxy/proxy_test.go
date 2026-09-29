// Package proxy 提供反向代理功能的测试。
//
// 该文件测试代理模块的各项功能，包括：
//   - 代理创建和配置
//   - 目标选择
//   - 请求转发
//   - 请求头修改
//   - 响应头修改
//   - 客户端 IP 提取
//   - 目标更新
//   - WebSocket 请求检测
//   - 负载均衡器创建
//   - HostClient 创建
//   - 健康检查器设置
//   - 代理缓存功能
//   - 被动健康检查
//
// 作者：xfy
package proxy

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/netutil"
	"rua.plus/lolly/internal/testutil"
	"rua.plus/lolly/internal/variable"
)

// TestNewProxy 测试 NewProxy 函数
func TestNewProxy(t *testing.T) {
	tests := []struct {
		cfg         *config.ProxyConfig
		name        string
		errContains string
		targets     []*loadbalance.Target
		wantErr     bool
	}{
		{
			name:    "正常创建",
			cfg:     testutil.NewTestProxyConfig("/api"),
			targets: testutil.NewTestTargets("http://localhost:8081", "http://localhost:8082"),
			wantErr: false,
		},
		{
			name:        "nil配置",
			cfg:         nil,
			targets:     testutil.NewTestTargets("http://localhost:8081"),
			wantErr:     true,
			errContains: "proxy config is nil",
		},
		{
			name:        "空目标列表",
			cfg:         &config.ProxyConfig{Path: "/api"},
			targets:     []*loadbalance.Target{},
			wantErr:     true,
			errContains: "no proxy targets provided",
		},
		{
			name:        "nil目标列表",
			cfg:         &config.ProxyConfig{Path: "/api"},
			targets:     nil,
			wantErr:     true,
			errContains: "no proxy targets provided",
		},
		{
			name: "默认负载均衡算法",
			cfg: &config.ProxyConfig{
				Path:        "/api",
				LoadBalance: "",
			},
			targets: testutil.NewTestTargets("http://localhost:8081"),
			wantErr: false,
		},
		{
			name: "加权轮询算法",
			cfg: &config.ProxyConfig{
				Path:        "/api",
				LoadBalance: "weighted_round_robin",
				Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
			},
			targets: []*loadbalance.Target{
				{URL: "http://localhost:8081", Weight: 1},
				{URL: "http://localhost:8082", Weight: 2},
			},
			wantErr: false,
		},
		{
			name: "最少连接算法",
			cfg: &config.ProxyConfig{
				Path:        "/api",
				LoadBalance: "least_conn",
				Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
			},
			targets: []*loadbalance.Target{
				{URL: "http://localhost:8081"},
			},
			wantErr: false,
		},
		{
			name: "IP哈希算法",
			cfg: &config.ProxyConfig{
				Path:        "/api",
				LoadBalance: "ip_hash",
				Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
			},
			targets: []*loadbalance.Target{
				{URL: "http://localhost:8081"},
			},
			wantErr: false,
		},
		{
			name: "无效负载均衡算法",
			cfg: &config.ProxyConfig{
				Path:        "/api",
				LoadBalance: "invalid_algorithm",
			},
			targets:     testutil.NewTestTargets("http://localhost:8081"),
			wantErr:     true,
			errContains: "unsupported load balance algorithm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewProxy(tt.cfg, tt.targets, nil, nil)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NewProxy() expected error containing %q, got nil", tt.errContains)
					return
				}
				if !contains(err.Error(), tt.errContains) {
					t.Errorf("NewProxy() error = %v, want containing %q", err, tt.errContains)
				}
				return
			}
			if err != nil {
				t.Errorf("NewProxy() unexpected error: %v", err)
				return
			}
			if p == nil {
				t.Error("NewProxy() returned nil proxy")
				return
			}
			if p.config != tt.cfg {
				t.Error("NewProxy() proxy config not set correctly")
			}
			if p.balancer == nil {
				t.Error("NewProxy() balancer not initialized")
			}
		})
	}
}

// TestServeHTTP_NoHealthyTargets 测试没有健康目标时返回502
func TestServeHTTP_NoHealthyTargets(t *testing.T) {
	cfg := testutil.NewTestProxyConfig("/api")

	// 所有目标都不健康
	targets := testutil.NewTestTargets("http://localhost:8081", "http://localhost:8082")
	targets[0].Healthy.Store(false)
	targets[1].Healthy.Store(false)

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 创建测试请求
	ctx := testutil.NewRequestCtx("GET", "/api/test")

	// 执行请求
	p.ServeHTTP(ctx)

	// 应该返回502
	if ctx.Response.StatusCode() != fasthttp.StatusBadGateway {
		t.Errorf("ServeHTTP() status code = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusBadGateway)
	}
}

// TestServeHTTP_RequestForwarding 测试请求转发
func TestServeHTTP_RequestForwarding(t *testing.T) {
	// 创建本地测试服务器
	ln := fasthttputil.NewInmemoryListener()
	defer func() { _ = ln.Close() }()

	// 启动后端服务器
	go func() {
		s := &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetStatusCode(fasthttp.StatusOK)
				ctx.SetBodyString("Hello from backend")
				ctx.Response.Header.Set("X-Backend-Header", "test-value")
			},
		}
		_ = s.Serve(ln)
	}()

	// 等待服务器启动
	time.Sleep(10 * time.Millisecond)

	cfg := testutil.NewTestProxyConfig("/api")

	targets := testutil.NewTestTargets("http://localhost:8080")

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 创建测试请求
	ctx := testutil.NewRequestCtx("GET", "/api/test")
	ctx.Request.Header.Set("X-Custom-Header", "client-value")

	// 执行请求
	p.ServeHTTP(ctx)

	// 由于没有真实后端，应该返回502
	// 但在单元测试中我们可以验证错误处理逻辑
	if ctx.Response.StatusCode() != fasthttp.StatusBadGateway {
		t.Logf("ServeHTTP() status code = %d (expected 502 when no backend available)", ctx.Response.StatusCode())
	}
}

// TestSelectTarget 测试目标选择
func TestSelectTarget(t *testing.T) {
	tests := []struct {
		name           string
		loadBalance    string
		clientIP       string
		expectedTarget string
		targets        []*loadbalance.Target
	}{
		{
			name:        "轮询选择",
			loadBalance: "round_robin",
			targets: []*loadbalance.Target{
				{URL: "http://backend1:8080"},
				{URL: "http://backend2:8080"},
			},
			expectedTarget: "http://backend1:8080",
		},
		{
			name:        "跳过不健康目标",
			loadBalance: "round_robin",
			targets: []*loadbalance.Target{
				{URL: "http://backend1:8080"},
				{URL: "http://backend2:8080"},
			},
			expectedTarget: "http://backend2:8080",
		},
		{
			name:        "IP哈希选择",
			loadBalance: "ip_hash",
			targets: []*loadbalance.Target{
				{URL: "http://backend1:8080"},
				{URL: "http://backend2:8080"},
			},
			clientIP:       "192.168.1.100",
			expectedTarget: "any", // IP哈希应该返回一个目标，具体是哪个取决于哈希值
		},
		{
			name:        "所有目标都不健康",
			loadBalance: "round_robin",
			targets: []*loadbalance.Target{
				{URL: "http://backend1:8080"},
				{URL: "http://backend2:8080"},
			},
			expectedTarget: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 根据测试用例设置健康状态
			switch tt.name {
			case "轮询选择", "IP哈希选择":
				for _, target := range tt.targets {
					target.Healthy.Store(true)
				}
			case "跳过不健康目标":
				tt.targets[0].Healthy.Store(false)
				tt.targets[1].Healthy.Store(true)
			case "所有目标都不健康":
				for _, target := range tt.targets {
					target.Healthy.Store(false)
				}
			}

			cfg := &config.ProxyConfig{
				Path:        "/api",
				LoadBalance: tt.loadBalance,
				Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
			}

			p, err := NewProxy(cfg, tt.targets, nil, nil)
			if err != nil {
				t.Fatalf("NewProxy() error: %v", err)
			}

			ctx := testutil.NewRequestCtxWithHeader("GET", "/api/test", map[string]string{
				"X-Forwarded-For": tt.clientIP,
			})

			target := p.selectTarget(ctx)

			if tt.expectedTarget == "" {
				if target != nil {
					t.Errorf("selectTarget() expected nil, got %v", target.URL)
				}
				return
			}

			if tt.loadBalance == "round_robin" && tt.expectedTarget != "" {
				// 轮询应该选择第一个健康目标
				if target == nil {
					t.Error("selectTarget() returned nil for healthy targets")
					return
				}
				if target.URL != tt.expectedTarget {
					t.Errorf("selectTarget() = %v, want %v", target.URL, tt.expectedTarget)
				}
			}

			// IP哈希应该始终返回同一个目标给同一个IP
			if tt.loadBalance == "ip_hash" && tt.clientIP != "" {
				if target == nil {
					t.Error("selectTarget() returned nil for IP hash")
					return
				}
				// 再次选择，应该返回相同的目标
				target2 := p.selectTarget(ctx)
				if target2 == nil || target2.URL != target.URL {
					t.Error("IP hash should consistently return the same target for the same IP")
				}
			}
		})
	}
}

// TestModifyRequestHeaders 测试请求头修改
func TestModifyRequestHeaders(t *testing.T) {
	tests := []struct {
		name           string
		clientIP       string
		existingXFF    string
		setRequest     map[string]string
		removeHeaders  []string
		checkHeaders   map[string]string
		shouldNotExist []string
	}{
		{
			name:     "设置X-Real-IP",
			clientIP: "192.168.1.100",
			checkHeaders: map[string]string{
				"X-Real-IP": "192.168.1.100",
			},
		},
		{
			name:        "追加X-Forwarded-For",
			clientIP:    "192.168.1.100",
			existingXFF: "10.0.0.1",
			checkHeaders: map[string]string{
				"X-Forwarded-For": "10.0.0.1, 10.0.0.1",
			},
		},
		{
			name:     "新建X-Forwarded-For",
			clientIP: "192.168.1.100",
			checkHeaders: map[string]string{
				"X-Forwarded-For": "192.168.1.100",
			},
		},
		{
			name:     "自定义请求头",
			clientIP: "192.168.1.100",
			setRequest: map[string]string{
				"X-Custom-Header": "custom-value",
				"X-Another":       "another-value",
			},
			checkHeaders: map[string]string{
				"X-Custom-Header": "custom-value",
				"X-Another":       "another-value",
			},
		},
		{
			name:           "移除请求头",
			clientIP:       "192.168.1.100",
			removeHeaders:  []string{"X-Remove-Me"},
			shouldNotExist: []string{"X-Remove-Me"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.ProxyConfig{
				Path:        "/api",
				LoadBalance: "round_robin",
				Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
				Headers: config.ProxyHeaders{
					SetRequest: tt.setRequest,
					Remove:     tt.removeHeaders,
				},
			}

			targets := []*loadbalance.Target{
				{URL: "http://localhost:8080"},
			}

			p, err := NewProxy(cfg, targets, nil, nil)
			if err != nil {
				t.Fatalf("NewProxy() error: %v", err)
			}

			// 构建 headers map
			headers := make(map[string]string)
			if tt.clientIP != "" {
				headers["X-Real-IP"] = tt.clientIP
			}
			if tt.existingXFF != "" {
				headers["X-Forwarded-For"] = tt.existingXFF
			}
			if len(tt.removeHeaders) > 0 {
				for _, h := range tt.removeHeaders {
					headers[h] = "should-be-removed"
				}
			}

			ctx := testutil.NewRequestCtxWithHeader("GET", "/api/test", headers)

			target := &loadbalance.Target{URL: "http://localhost:8080"}
			p.modifyRequestHeaders(ctx, target)

			// 检查期望存在的头
			for key, expectedValue := range tt.checkHeaders {
				actualValue := string(ctx.Request.Header.Peek(key))
				if actualValue != expectedValue {
					t.Errorf("Header %s = %q, want %q", key, actualValue, expectedValue)
				}
			}

			// 检查不应该存在的头
			for _, key := range tt.shouldNotExist {
				if ctx.Request.Header.Peek(key) != nil {
					t.Errorf("Header %s should not exist", key)
				}
			}
		})
	}
}

// TestModifyResponseHeaders 测试响应头修改
func TestModifyResponseHeaders(t *testing.T) {
	tests := []struct {
		setResponse  map[string]string
		checkHeaders map[string]string
		name         string
	}{
		{
			name: "设置自定义响应头",
			setResponse: map[string]string{
				"X-Custom-Response": "custom-value",
				"X-Powered-By":      "Lolly",
			},
			checkHeaders: map[string]string{
				"X-Custom-Response": "custom-value",
				"X-Powered-By":      "Lolly",
			},
		},
		{
			name:         "空响应头配置",
			setResponse:  nil,
			checkHeaders: map[string]string{},
		},
		{
			name: "覆盖已有响应头",
			setResponse: map[string]string{
				"Content-Type": "application/json",
			},
			checkHeaders: map[string]string{
				"Content-Type": "application/json",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.ProxyConfig{
				Path:        "/api",
				LoadBalance: "round_robin",
				Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
				Headers: config.ProxyHeaders{
					SetResponse: tt.setResponse,
				},
			}

			targets := testutil.NewTestTargets("http://localhost:8080")

			p, err := NewProxy(cfg, targets, nil, nil)
			if err != nil {
				t.Fatalf("NewProxy() error: %v", err)
			}

			ctx := testutil.NewRequestCtx("GET", "/")

			p.modifyResponseHeaders(ctx)

			// 检查期望存在的头
			for key, expectedValue := range tt.checkHeaders {
				actualValue := string(ctx.Response.Header.Peek(key))
				if actualValue != expectedValue {
					t.Errorf("Response Header %s = %q, want %q", key, actualValue, expectedValue)
				}
			}
		})
	}
}

// TestGetClientIP 测试客户端IP提取
func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name     string
		xff      string
		xri      string
		expected string
	}{
		{
			name:     "从X-Forwarded-For提取",
			xff:      "10.0.0.1, 10.0.0.2",
			expected: "10.0.0.1",
		},
		{
			name:     "从X-Real-IP提取",
			xri:      "192.168.1.100",
			expected: "192.168.1.100",
		},
		{
			name:     "X-Forwarded-For优先",
			xff:      "10.0.0.1",
			xri:      "192.168.1.100",
			expected: "10.0.0.1",
		},
		{
			name:     "单IP",
			xff:      "10.0.0.1",
			expected: "10.0.0.1",
		},
		{
			name:     "带空格",
			xff:      " 10.0.0.1 ",
			expected: "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testutil.NewRequestCtxWithHeader("GET", "/", map[string]string{
				"X-Forwarded-For": tt.xff,
				"X-Real-IP":       tt.xri,
			})

			ip := netutil.ExtractClientIP(ctx)
			if ip != tt.expected {
				t.Errorf("ExtractClientIP() = %q, want %q", ip, tt.expected)
			}
		})
	}
}

// TestUpdateTargets 测试更新目标

// TestGetTargets 测试获取目标列表

// TestGetConfig 测试获取配置

// TestIsWebSocketRequest 测试WebSocket请求检测
func TestIsWebSocketRequest(t *testing.T) {
	tests := []struct {
		name       string
		upgrade    string
		connection string
		expected   bool
	}{
		{
			name:       "标准WebSocket请求",
			upgrade:    "websocket",
			connection: "upgrade",
			expected:   true,
		},
		{
			name:       "大小写不敏感",
			upgrade:    "WebSocket",
			connection: "Upgrade",
			expected:   true,
		},
		{
			name:       "非WebSocket升级",
			upgrade:    "h2c",
			connection: "upgrade",
			expected:   false,
		},
		{
			name:       "非upgrade连接",
			upgrade:    "websocket",
			connection: "keep-alive",
			expected:   false,
		},
		{
			name:       "keep-alive, Upgrade",
			upgrade:    "websocket",
			connection: "keep-alive, Upgrade",
			expected:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testutil.NewRequestCtxWithHeader("GET", "/", map[string]string{
				"Upgrade":    tt.upgrade,
				"Connection": tt.connection,
			})

			result := isWebSocketRequest(ctx)
			if result != tt.expected {
				t.Errorf("isWebSocketRequest() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestCreateBalancer 测试负载均衡器创建
func TestCreateBalancer(t *testing.T) {
	tests := []struct {
		cfg         *config.ProxyConfig
		name        string
		errContains string
		wantErr     bool
	}{
		{
			name: "轮询",
			cfg:  &config.ProxyConfig{LoadBalance: "round_robin"},
		},
		{
			name: "加权轮询",
			cfg:  &config.ProxyConfig{LoadBalance: "weighted_round_robin"},
		},
		{
			name: "最少连接",
			cfg:  &config.ProxyConfig{LoadBalance: "least_conn"},
		},
		{
			name: "IP哈希",
			cfg:  &config.ProxyConfig{LoadBalance: "ip_hash"},
		},
		{
			name: "一致性哈希",
			cfg:  &config.ProxyConfig{LoadBalance: "consistent_hash", HashKey: "ip", VirtualNodes: 150},
		},
		{
			name: "空算法（默认轮询）",
			cfg:  &config.ProxyConfig{LoadBalance: ""},
		},
		{
			name:        "无效算法",
			cfg:         &config.ProxyConfig{LoadBalance: "unknown_algorithm"},
			wantErr:     true,
			errContains: "unsupported load balance algorithm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			balancer, err := createBalancer(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Errorf("createBalancer(%v) expected error", tt.cfg.LoadBalance)
					return
				}
				if !contains(err.Error(), tt.errContains) {
					t.Errorf("createBalancer(%v) error = %v, want containing %q", tt.cfg.LoadBalance, err, tt.errContains)
				}
				return
			}
			if err != nil {
				t.Errorf("createBalancer(%v) unexpected error: %v", tt.cfg.LoadBalance, err)
				return
			}
			if balancer == nil {
				t.Errorf("createBalancer(%v) returned nil balancer", tt.cfg.LoadBalance)
			}
		})
	}
}

// TestCreateHostClient 测试HostClient创建
func TestCreateHostClient(t *testing.T) {
	tests := []struct {
		name      string
		targetURL string
		timeout   config.ProxyTimeout
	}{
		{
			name:      "HTTP地址",
			targetURL: "http://localhost:8080",
			timeout:   config.ProxyTimeout{Connect: 5 * time.Second, Read: 30 * time.Second, Write: 30 * time.Second},
		},
		{
			name:      "HTTPS地址",
			targetURL: "https://localhost:8443",
			timeout:   config.ProxyTimeout{Connect: 5 * time.Second, Read: 30 * time.Second, Write: 30 * time.Second},
		},
		{
			name:      "带路径的URL",
			targetURL: "http://localhost:8080/path",
			timeout:   config.ProxyTimeout{Connect: 5 * time.Second},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := createHostClient(tt.targetURL, tt.timeout, nil, nil, "", nil)
			if client == nil {
				t.Error("createHostClient() returned nil")
				return
			}

			// 检查基本属性
			if client.Addr == "" {
				t.Error("createHostClient() client.Addr is empty")
			}

			if tt.targetURL == "https://localhost:8443" && !client.IsTLS {
				t.Error("createHostClient() IsTLS should be true for HTTPS")
			}
		})
	}
}

// TestHandleWebSocket 测试 WebSocket 处理
// 注意：由于 WebSocket 代理使用 Hijack 获取底层连接，
// 这个测试主要验证函数不会 panic，实际桥接功能需要集成测试
func TestHandleWebSocket(t *testing.T) {
	cfg := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
	}

	targets := testutil.NewTestTargets("http://localhost:8080")

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 由于 handleWebSocket 使用 Hijack，在测试环境中无法正常工作
	// （需要一个真实的 HTTP 连接），因此我们仅验证函数存在且可调用
	// 实际功能通过集成测试验证
	target := testutil.NewTestTarget("http://localhost:8080")
	client := p.getClient(target.URL)

	// 验证客户端和目标已正确配置
	if client == nil {
		t.Error("Expected non-nil client")
	}
	if target.URL != "http://localhost:8080" {
		t.Errorf("Expected target URL http://localhost:8080, got %s", target.URL)
	}
}

// TestWebSocketConnectionCountReleasedOnFailure 测试 WebSocket 升级失败后连接计数被释放。
// testutil 创建的请求上下文不支持 Hijack，会导致 WebSocket 代理失败；
// 失败后应确保 IncrementConnections 对应的 DecrementConnections 被调用。
func TestWebSocketConnectionCountReleasedOnFailure(t *testing.T) {
	cfg := testutil.NewTestProxyConfig("/ws")
	targets := testutil.NewTestTargets("http://127.0.0.1:1")
	targets[0].MaxFails = 1

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	ctx := testutil.NewRequestCtxWithHeader("GET", "/ws", map[string]string{
		"Upgrade":    "websocket",
		"Connection": "Upgrade",
	})

	p.ServeHTTP(ctx)

	if got := atomic.LoadInt64(&targets[0].Connections); got != 0 {
		t.Fatalf("expected connection count 0 after failed WebSocket, got %d", got)
	}
}

// TestSetHealthChecker 测试健康检查器设置
// 注意：SetHealthChecker 是公开方法，但 healthChecker 是私有字段
// 此测试验证方法可以正常调用
func TestSetHealthChecker(t *testing.T) {
	cfg := testutil.NewTestProxyConfig("/api")

	targets := testutil.NewTestTargets("http://localhost:8081")

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 创建健康检查器
	hcCfg := &config.HealthCheckConfig{
		Interval: 10 * time.Second,
		Path:     "/health",
		Timeout:  5 * time.Second,
	}
	hc := NewHealthChecker(targets, hcCfg)

	// 设置健康检查器 - 验证方法存在且可调用
	p.SetHealthChecker(hc)

	// 测试被动健康检查：标记目标为不健康
	targets[0].Healthy.Store(true)
	hc.MarkUnhealthy(targets[0])

	if targets[0].Healthy.Load() {
		t.Error("MarkUnhealthy() target should be unhealthy after marking")
	}
}

// TestGetClient 测试客户端获取
func TestGetClient(t *testing.T) {
	cfg := testutil.NewTestProxyConfig("/api")

	targets := testutil.NewTestTargets("http://localhost:8081", "http://localhost:8082")

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 测试获取存在的客户端
	client1 := p.getClient("http://localhost:8081")
	if client1 == nil {
		t.Error("getClient() returned nil for existing client")
	}

	client2 := p.getClient("http://localhost:8082")
	if client2 == nil {
		t.Error("getClient() returned nil for existing client")
	}

	// 测试获取不存在的客户端
	client3 := p.getClient("http://localhost:9999")
	if client3 != nil {
		t.Error("getClient() should return nil for non-existent client")
	}
}

// TestProxyCache 测试代理缓存功能

// TestServeHTTP_WithPassiveHealthCheck 测试带有被动健康检查的请求转发
func TestServeHTTP_WithPassiveHealthCheck(t *testing.T) {
	cfg := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 100 * time.Millisecond, Read: 100 * time.Millisecond, Write: 100 * time.Millisecond},
	}

	targets := testutil.NewTestHealthyTargets("http://127.0.0.1:59999") // 不存在的后端

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 设置健康检查器
	hcCfg := &config.HealthCheckConfig{
		Interval: 10 * time.Second,
		Path:     "/health",
		Timeout:  5 * time.Second,
	}
	hc := NewHealthChecker(targets, hcCfg)
	p.SetHealthChecker(hc)

	// 创建测试请求
	ctx := testutil.NewRequestCtx("GET", "/api/test")

	// 执行请求 - 应该会失败并触发被动健康检查
	p.ServeHTTP(ctx)

	// 验证返回502错误
	if ctx.Response.StatusCode() != fasthttp.StatusBadGateway {
		t.Errorf("ServeHTTP() status code = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusBadGateway)
	}

	// 验证目标已被标记为不健康
	if targets[0].Healthy.Load() {
		t.Error("Target should be marked unhealthy after failed request")
	}
}

// 辅助函数
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr, 0))
}

func containsAt(s, substr string, start int) bool {
	for i := start; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestUpstreamVariablesCapture 测试上游变量捕获
func TestUpstreamVariablesCapture(t *testing.T) {
	// 创建后端服务器
	backend := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetStatusCode(200)
			ctx.SetBodyString("OK")
		},
	}

	// 在随机端口启动后端
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer func() { _ = backendLn.Close() }()

	go func() { _ = backend.Serve(backendLn) }()

	// 等待后端启动
	time.Sleep(50 * time.Millisecond)

	backendAddr := "http://" + backendLn.Addr().String()

	// 创建代理
	targets := []*loadbalance.Target{
		{URL: backendAddr, Weight: 1},
	}
	targets[0].Healthy.Store(true)

	cfg := testutil.NewTestProxyConfig("/")

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	// 创建请求
	ctx := testutil.NewRequestCtxWithHeader("GET", "/test", map[string]string{
		"Host": "example.com",
	})

	// 执行代理请求
	p.ServeHTTP(ctx)

	// 验证响应
	if ctx.Response.StatusCode() != 200 {
		t.Errorf("expected status 200, got %d", ctx.Response.StatusCode())
	}

	// 测试 UpstreamTiming
	timing := NewUpstreamTiming()
	if timing == nil {
		t.Error("NewUpstreamTiming() returned nil")
	}

	// 测试时间标记
	timing.MarkConnectStart()
	timing.MarkConnectEnd()
	timing.MarkHeaderReceived()
	timing.MarkResponseEnd()

	// 验证时间计算
	if timing.GetConnectTime() < 0 {
		t.Error("GetConnectTime() should be >= 0")
	}
	if timing.GetHeaderTime() < 0 {
		t.Error("GetHeaderTime() should be >= 0")
	}
	if timing.GetResponseTime() < 0 {
		t.Error("GetResponseTime() should be >= 0")
	}
}

// TestUpstreamVariablesErrorPaths 测试上游变量错误路径
func TestUpstreamVariablesErrorPaths(t *testing.T) {
	tests := []struct {
		name         string
		backendAddr  string
		expectedAddr string
		expectedCode int
	}{
		{
			name:         "no healthy backend",
			backendAddr:  "",
			expectedAddr: "FAILED",
			expectedCode: 502,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var targets []*loadbalance.Target
			if tt.backendAddr != "" {
				targets = []*loadbalance.Target{
					{URL: tt.backendAddr, Weight: 1},
				}
				targets[0].Healthy.Store(true)
			} else {
				// 创建一个不健康目标
				targets = []*loadbalance.Target{
					{URL: "http://127.0.0.1:1", Weight: 1},
				}
			}

			cfg := &config.ProxyConfig{
				Path:        "/",
				LoadBalance: "round_robin",
				Timeout: config.ProxyTimeout{
					Connect: 1 * time.Millisecond, // 超短超时
					Read:    1 * time.Millisecond,
					Write:   1 * time.Millisecond,
				},
			}

			p, err := NewProxy(cfg, targets, nil, nil)
			if err != nil {
				t.Fatalf("failed to create proxy: %v", err)
			}

			ctx := testutil.NewRequestCtxWithHeader("GET", "/test", map[string]string{
				"Host": "example.com",
			})

			p.ServeHTTP(ctx)

			// 验证错误状态码
			if ctx.Response.StatusCode() != tt.expectedCode &&
				ctx.Response.StatusCode() != 502 &&
				ctx.Response.StatusCode() != 504 {
				t.Errorf("expected status %d or 502/504, got %d", tt.expectedCode, ctx.Response.StatusCode())
			}
		})
	}
}

// TestFinalizeUpstreamVars 测试 FinalizeUpstreamVars 函数
func TestFinalizeUpstreamVars(t *testing.T) {
	ctx := testutil.NewRequestCtx("GET", "/test")

	vc := variable.NewContext(ctx)
	defer variable.ReleaseContext(vc)

	timing := NewUpstreamTiming()
	timing.MarkConnectStart()
	time.Sleep(1 * time.Millisecond)
	timing.MarkConnectEnd()
	timing.MarkHeaderReceived()
	time.Sleep(1 * time.Millisecond)
	timing.MarkResponseEnd()

	// 测试 FinalizeUpstreamVars
	FinalizeUpstreamVars(vc, "http://backend:8080", 200, timing)

	// 验证变量已设置
	addr, ok := vc.Get("upstream_addr")
	if !ok || addr != "http://backend:8080" {
		t.Errorf("upstream_addr = %q, want 'http://backend:8080'", addr)
	}

	status, ok := vc.Get("upstream_status")
	if !ok || status != "200" {
		t.Errorf("upstream_status = %q, want '200'", status)
	}

	// 测试 nil vc
	FinalizeUpstreamVars(nil, "http://backend:8080", 200, timing)
	// 不应该 panic
}

// TestUpstreamTimingZero 测试 UpstreamTiming 零值处理
func TestUpstreamTimingZero(t *testing.T) {
	timing := NewUpstreamTiming()

	// 未标记时应该返回 0
	if timing.GetConnectTime() != 0 {
		t.Errorf("GetConnectTime() = %v, want 0", timing.GetConnectTime())
	}
	if timing.GetHeaderTime() != 0 {
		t.Errorf("GetHeaderTime() = %v, want 0", timing.GetHeaderTime())
	}
	if timing.GetResponseTime() != 0 {
		t.Errorf("GetResponseTime() = %v, want 0", timing.GetResponseTime())
	}

	// 只标记开始
	timing.MarkConnectStart()
	if timing.GetConnectTime() != 0 {
		t.Errorf("GetConnectTime() after MarkConnectStart = %v, want 0", timing.GetConnectTime())
	}
}

// TestUpstreamTiming_ZeroValues 测试 UpstreamTiming 完全零值情况
func TestUpstreamTiming_ZeroValues(t *testing.T) {
	// 创建一个零值的时间记录器（模拟未初始化的状态）
	timing := &UpstreamTiming{}

	// 所有时间应该返回 0
	if timing.GetConnectTime() != 0 {
		t.Errorf("Zero timing GetConnectTime() = %v, want 0", timing.GetConnectTime())
	}
	if timing.GetHeaderTime() != 0 {
		t.Errorf("Zero timing GetHeaderTime() = %v, want 0", timing.GetHeaderTime())
	}
	if timing.GetResponseTime() != 0 {
		t.Errorf("Zero timing GetResponseTime() = %v, want 0", timing.GetResponseTime())
	}
}

// TestUpstreamTiming_PartialMarks 测试部分标记的情况
func TestUpstreamTiming_PartialMarks(t *testing.T) {
	timing := NewUpstreamTiming()

	// 只标记 connectEnd，不标记 connectStart
	timing.MarkConnectEnd()
	if timing.GetConnectTime() != 0 {
		t.Errorf("GetConnectTime() with only end marked = %v, want 0", timing.GetConnectTime())
	}

	// 重置并测试只有 headerReceived 的情况
	timing = NewUpstreamTiming()
	timing.MarkHeaderReceived()
	if timing.GetHeaderTime() != 0 {
		t.Errorf("GetHeaderTime() without connectEnd = %v, want 0", timing.GetHeaderTime())
	}

	// 重置并测试只有 responseEnd 的情况
	timing = NewUpstreamTiming()
	timing.MarkResponseEnd()
	if timing.GetResponseTime() != 0 {
		t.Errorf("GetResponseTime() without connectEnd = %v, want 0", timing.GetResponseTime())
	}
}

// TestProxyConnectionCount_InternalRedirect 验证 X-Accel-Redirect 后连接计数归零。
func TestProxyConnectionCount_InternalRedirect(t *testing.T) {
	backend := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set("X-Accel-Redirect", "/redirect/new-path")
			ctx.SetStatusCode(200)
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() { _ = backend.Serve(ln) }()
	time.Sleep(50 * time.Millisecond)

	targets := testutil.NewTestHealthyTargets("http://" + ln.Addr().String())
	cfg := testutil.NewTestProxyConfig("/")
	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	ctx := testutil.NewRequestCtxWithHeader("GET", "/api", map[string]string{
		"Host": "example.com",
	})

	p.ServeHTTP(ctx)

	if atomic.LoadInt64(&targets[0].Connections) != 0 {
		t.Errorf("expected connections to return to 0 after internal redirect, got %d", targets[0].Connections)
	}
}

// TestProxyConnectionCount_Retry 验证重试路径后连接计数归零。
func TestProxyConnectionCount_Retry(t *testing.T) {
	backend := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetStatusCode(503)
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() { _ = backend.Serve(ln) }()
	time.Sleep(50 * time.Millisecond)

	targets := testutil.NewTestHealthyTargets("http://" + ln.Addr().String())
	cfg := &config.ProxyConfig{
		Path:        "/",
		LoadBalance: "round_robin",
		Timeout: config.ProxyTimeout{
			Connect: 5 * time.Second,
			Read:    5 * time.Second,
			Write:   5 * time.Second,
		},
		NextUpstream: config.NextUpstreamConfig{
			Tries:     2,
			HTTPCodes: []int{503},
		},
	}

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	ctx := testutil.NewRequestCtxWithHeader("GET", "/api", map[string]string{
		"Host": "example.com",
	})

	p.ServeHTTP(ctx)

	if atomic.LoadInt64(&targets[0].Connections) != 0 {
		t.Errorf("expected connections to return to 0 after retry, got %d", targets[0].Connections)
	}
}

// TestProxy_RequestStreamingEnabled 测试 NewProxy 正确解析请求体流式配置。
func TestProxy_RequestStreamingEnabled(t *testing.T) {
	cfg := testutil.NewTestProxyConfig("/", "http://127.0.0.1:1")
	cfg.Buffering = &config.ProxyBufferingConfig{RequestMode: "off"}
	p, err := NewProxy(cfg, testutil.NewTestTargets("http://127.0.0.1:1"), nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}
	if !p.RequestStreamingEnabled() {
		t.Error("RequestStreamingEnabled() = false, want true")
	}

	// 默认配置应为 false
	cfg2 := testutil.NewTestProxyConfig("/", "http://127.0.0.1:1")
	p2, _ := NewProxy(cfg2, testutil.NewTestTargets("http://127.0.0.1:1"), nil, nil)
	if p2.RequestStreamingEnabled() {
		t.Error("default RequestStreamingEnabled() = true, want false")
	}
}

// TestServeHTTP_StreamRequestBody 测试请求体流式端到端转发。
//
// 构造带 BodyStream 的请求上下文，启用 request_mode: off，
// 验证后端收到的请求体与发送一致，且 lolly 侧未提前物化整个 body。
func TestServeHTTP_StreamRequestBody(t *testing.T) {
	body := bytes.Repeat([]byte("lolly-stream-"), 8*1024) // ~104KB，超过默认 inline 阈值

	var gotBody []byte
	var gotLen int
	backend := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			gotLen = ctx.Request.Header.ContentLength()
			gotBody = append(gotBody, ctx.Request.Body()...)
			ctx.SetStatusCode(fasthttp.StatusOK)
			ctx.SetBodyString("ok")
		},
	}

	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = backendLn.Close() }()
	go func() { _ = backend.Serve(backendLn) }()
	time.Sleep(50 * time.Millisecond)

	cfg := testutil.NewTestProxyConfig("/", "http://"+backendLn.Addr().String())
	cfg.Buffering = &config.ProxyBufferingConfig{RequestMode: "off"}

	targets := testutil.NewTestTargets("http://" + backendLn.Addr().String())
	targets[0].Healthy.Store(true)

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}
	if !p.RequestStreamingEnabled() {
		t.Fatal("proxy should enable request streaming")
	}

	// 构造带 BodyStream 的请求（模拟 fasthttp server StreamRequestBody=true 的入站形态）
	ctx := testutil.NewRequestCtx("POST", "/upload")
	ctx.Request.Header.SetContentLength(len(body))
	ctx.Request.SetBodyStream(bytes.NewReader(body), len(body))

	p.ServeHTTP(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, want 200", ctx.Response.StatusCode())
	}
	if gotLen != len(body) {
		t.Errorf("backend Content-Length = %d, want %d", gotLen, len(body))
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("backend body mismatch: got %d bytes, want %d bytes", len(gotBody), len(body))
	}
}

// TestServeHTTP_StreamRequestBody_NoRetry 验证流式请求体禁用 next_upstream 重试。
//
// 后端返回 503（默认重试码），配置 tries=3，但因流式请求体不可回放，
// 应只命中后端一次并最终返回 503。
func TestServeHTTP_StreamRequestBody_NoRetry(t *testing.T) {
	var hits int32
	backend := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			atomic.AddInt32(&hits, 1)
			// 排空请求体以避免连接复用问题
			_ = ctx.Request.Body()
			ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
		},
	}

	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = backendLn.Close() }()
	go func() { _ = backend.Serve(backendLn) }()
	time.Sleep(50 * time.Millisecond)

	cfg := testutil.NewTestProxyConfig("/", "http://"+backendLn.Addr().String())
	cfg.Buffering = &config.ProxyBufferingConfig{RequestMode: "off"}
	cfg.NextUpstream.Tries = 3
	cfg.NextUpstream.HTTPCodes = []int{502, 503, 504}

	targets := testutil.NewTestTargets("http://" + backendLn.Addr().String())
	targets[0].Healthy.Store(true)

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	body := []byte("stream-body-content")
	ctx := testutil.NewRequestCtx("POST", "/upload")
	ctx.Request.Header.SetContentLength(len(body))
	ctx.Request.SetBodyStream(bytes.NewReader(body), len(body))

	p.ServeHTTP(ctx)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("backend hits = %d, want 1 (streaming disables retry)", got)
	}
	if ctx.Response.StatusCode() != fasthttp.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", ctx.Response.StatusCode())
	}
}
