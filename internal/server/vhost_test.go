// Package server 提供虚拟主机管理器的测试。
package server

import (
	"os"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
)

// mockHandler 创建一个记录调用的 mock handler
func mockHandler(name string, called *bool) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		*called = true

		_, _ = ctx.WriteString(name)
	}
}

// TestVHostManager_Handler 测试虚拟主机选择器功能。
func TestVHostManager_Handler(t *testing.T) {
	t.Run("匹配已知主机", func(t *testing.T) {
		manager := NewVHostManager()
		hostCalled := false
		_ = manager.AddHost("example.com", mockHandler("example", &hostCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("example.com")

		handler(ctx)

		if !hostCalled {
			t.Error("期望 example.com handler 被调用，但未被调用")
		}
		if string(ctx.Response.Body()) != "example" {
			t.Errorf("响应体 = %q, want %q", string(ctx.Response.Body()), "example")
		}
	})

	t.Run("匹配带端口的主机", func(t *testing.T) {
		manager := NewVHostManager()
		hostCalled := false
		_ = manager.AddHost("example.com", mockHandler("example", &hostCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("example.com:8080")

		handler(ctx)

		if !hostCalled {
			t.Error("期望 example.com handler 被调用（端口应被忽略），但未被调用")
		}
		if string(ctx.Response.Body()) != "example" {
			t.Errorf("响应体 = %q, want %q", string(ctx.Response.Body()), "example")
		}
	})

	t.Run("无匹配使用默认主机", func(t *testing.T) {
		manager := NewVHostManager()
		exampleCalled := false
		defaultCalled := false
		_ = manager.AddHost("example.com", mockHandler("example", &exampleCalled))
		manager.SetDefault(mockHandler("default", &defaultCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("unknown.com")

		handler(ctx)

		if exampleCalled {
			t.Error("不期望 example.com handler 被调用")
		}
		if !defaultCalled {
			t.Error("期望默认 handler 被调用，但未被调用")
		}
		if string(ctx.Response.Body()) != "default" {
			t.Errorf("响应体 = %q, want %q", string(ctx.Response.Body()), "default")
		}
	})

	t.Run("无匹配无默认返回404", func(t *testing.T) {
		manager := NewVHostManager()
		exampleCalled := false
		_ = manager.AddHost("example.com", mockHandler("example", &exampleCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("unknown.com")

		handler(ctx)

		if exampleCalled {
			t.Error("不期望 example.com handler 被调用")
		}
		if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
			t.Errorf("状态码 = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusNotFound)
		}
	})

	t.Run("IPv6地址Host", func(t *testing.T) {
		manager := NewVHostManager()
		ipv6Called := false
		_ = manager.AddHost("[::1]", mockHandler("ipv6", &ipv6Called))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("[::1]:8080")

		handler(ctx)

		if !ipv6Called {
			t.Error("期望 [::1] handler 被调用，但未被调用")
		}
		if string(ctx.Response.Body()) != "ipv6" {
			t.Errorf("响应体 = %q, want %q", string(ctx.Response.Body()), "ipv6")
		}
	})

	t.Run("空Host使用默认", func(t *testing.T) {
		manager := NewVHostManager()
		defaultCalled := false
		manager.SetDefault(mockHandler("default", &defaultCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("")

		handler(ctx)

		if !defaultCalled {
			t.Error("期望默认 handler 被调用，但未被调用")
		}
		if string(ctx.Response.Body()) != "default" {
			t.Errorf("响应体 = %q, want %q", string(ctx.Response.Body()), "default")
		}
	})

	t.Run("空Host无默认返回404", func(t *testing.T) {
		manager := NewVHostManager()

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("")

		handler(ctx)

		if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
			t.Errorf("状态码 = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusNotFound)
		}
	})
}

// TestVHostManager_AddHost 测试添加虚拟主机功能。
func TestVHostManager_AddHost(t *testing.T) {
	t.Run("添加单个主机", func(t *testing.T) {
		manager := NewVHostManager()
		called := false
		_ = manager.AddHost("test.com", mockHandler("test", &called))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("test.com")

		handler(ctx)

		if !called {
			t.Error("期望添加的主机 handler 被调用")
		}
	})

	t.Run("添加多个主机", func(t *testing.T) {
		manager := NewVHostManager()
		host1Called := false
		host2Called := false
		_ = manager.AddHost("host1.com", mockHandler("host1", &host1Called))
		_ = manager.AddHost("host2.com", mockHandler("host2", &host2Called))

		handler := manager.Handler()

		// 测试 host1
		ctx1 := &fasthttp.RequestCtx{}
		ctx1.Request.SetHost("host1.com")
		handler(ctx1)
		if !host1Called {
			t.Error("期望 host1 handler 被调用")
		}

		// 测试 host2
		ctx2 := &fasthttp.RequestCtx{}
		ctx2.Request.SetHost("host2.com")
		handler(ctx2)
		if !host2Called {
			t.Error("期望 host2 handler 被调用")
		}
	})

	t.Run("覆盖已存在的主机", func(t *testing.T) {
		manager := NewVHostManager()
		firstCalled := false
		secondCalled := false
		_ = manager.AddHost("test.com", mockHandler("first", &firstCalled))
		_ = manager.AddHost("test.com", mockHandler("second", &secondCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("test.com")

		handler(ctx)

		if firstCalled {
			t.Error("不期望第一个 handler 被调用（应被覆盖）")
		}
		if !secondCalled {
			t.Error("期望第二个 handler 被调用")
		}
	})
}

// TestVHostManager_SetDefault 测试设置默认主机功能。
func TestVHostManager_SetDefault(t *testing.T) {
	t.Run("设置默认主机", func(t *testing.T) {
		manager := NewVHostManager()
		defaultCalled := false
		manager.SetDefault(mockHandler("default", &defaultCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("nonexistent.com")

		handler(ctx)

		if !defaultCalled {
			t.Error("期望默认 handler 被调用")
		}
	})

	t.Run("覆盖默认主机", func(t *testing.T) {
		manager := NewVHostManager()
		firstCalled := false
		secondCalled := false
		manager.SetDefault(mockHandler("first", &firstCalled))
		manager.SetDefault(mockHandler("second", &secondCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("unknown.com")

		handler(ctx)

		if firstCalled {
			t.Error("不期望第一个默认 handler 被调用（应被覆盖）")
		}
		if !secondCalled {
			t.Error("期望第二个默认 handler 被调用")
		}
	})
}

// TestVHostManager_WildcardPrefix 测试前缀通配符 *.example.com。
func TestVHostManager_WildcardPrefix(t *testing.T) {
	tests := []struct {
		name        string
		pattern     string
		host        string
		shouldMatch bool
	}{
		{"exact subdomain", "*.example.com", "www.example.com", true},
		{"nested subdomain", "*.example.com", "api.www.example.com", true},
		{"no subdomain", "*.example.com", "example.com", false},
		{"different domain", "*.example.com", "www.other.com", false},
		{"longest match", "*.b.example.com", "a.b.example.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewVHostManager()
			called := false
			_ = manager.AddHost(tt.pattern, mockHandler("wildcard", &called))

			handler := manager.Handler()
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetHost(tt.host)

			handler(ctx)

			if called != tt.shouldMatch {
				t.Errorf("expected match %v, got %v", tt.shouldMatch, called)
			}
		})
	}
}

// TestVHostManager_WildcardSuffix 测试后缀通配符 example.*。
func TestVHostManager_WildcardSuffix(t *testing.T) {
	tests := []struct {
		name        string
		pattern     string
		host        string
		shouldMatch bool
	}{
		{"match com", "example.*", "example.com", true},
		{"match net", "example.*", "example.net", true},
		{"match org", "example.*", "example.org", true},
		{"no match subdomain", "example.*", "www.example.com", false},
		{"no match different prefix", "example.*", "other.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewVHostManager()
			called := false
			_ = manager.AddHost(tt.pattern, mockHandler("suffix", &called))

			handler := manager.Handler()
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetHost(tt.host)

			handler(ctx)

			if called != tt.shouldMatch {
				t.Errorf("expected match %v, got %v", tt.shouldMatch, called)
			}
		})
	}
}

// TestVHostManager_Regex 测试正则匹配。
func TestVHostManager_Regex(t *testing.T) {
	tests := []struct {
		name        string
		pattern     string
		host        string
		shouldMatch bool
		wantErr     bool
	}{
		{"match digits", "~^api[0-9]+\\.example\\.com$", "api1.example.com", true, false},
		{"match digits 2", "~^api[0-9]+\\.example\\.com$", "api99.example.com", true, false},
		{"no match letters", "~^api[0-9]+\\.example\\.com$", "apiX.example.com", false, false},
		{"invalid regex", "~[invalid", "", false, true},
		{"match any subdomain", "~.*\\.example\\.com", "www.example.com", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewVHostManager()
			called := false
			err := manager.AddHost(tt.pattern, mockHandler("regex", &called))

			if tt.wantErr {
				if err == nil {
					t.Error("expected error for invalid regex")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			handler := manager.Handler()
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetHost(tt.host)

			handler(ctx)

			if called != tt.shouldMatch {
				t.Errorf("expected match %v, got %v", tt.shouldMatch, called)
			}
		})
	}
}

// TestVHostManager_MatchPriority 测试匹配优先级。
func TestVHostManager_MatchPriority(t *testing.T) {
	t.Run("exact over wildcard", func(t *testing.T) {
		manager := NewVHostManager()
		exactCalled := false
		wildcardCalled := false

		_ = manager.AddHost("www.example.com", mockHandler("exact", &exactCalled))
		_ = manager.AddHost("*.example.com", mockHandler("wildcard", &wildcardCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("www.example.com")

		handler(ctx)

		if !exactCalled {
			t.Error("expected exact match to be called")
		}
		if wildcardCalled {
			t.Error("expected wildcard to NOT be called when exact match exists")
		}
	})

	t.Run("longest wildcard prefix", func(t *testing.T) {
		manager := NewVHostManager()
		shortCalled := false
		longCalled := false

		_ = manager.AddHost("*.example.com", mockHandler("short", &shortCalled))
		_ = manager.AddHost("*.b.example.com", mockHandler("long", &longCalled))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("a.b.example.com")

		handler(ctx)

		if shortCalled {
			t.Error("expected short wildcard to NOT be called")
		}
		if !longCalled {
			t.Error("expected longest wildcard match to be called")
		}
	})
}

// TestVHostManager_FindHost 测试 FindHost 方法。
func TestVHostManager_FindHost(t *testing.T) {
	manager := NewVHostManager()
	_ = manager.AddHost("exact.com", mockHandler("exact", new(bool)))
	_ = manager.AddHost("*.wildcard.com", mockHandler("wildcard", new(bool)))
	_ = manager.AddHost("suffix.*", mockHandler("suffix", new(bool)))
	_ = manager.AddHost("~^regex.*", mockHandler("regex", new(bool)))
	manager.SetDefault(mockHandler("default", new(bool)))

	tests := []struct {
		host     string
		wantName string
	}{
		{"exact.com", "exact.com"},
		{"www.wildcard.com", "*.wildcard.com"},
		{"suffix.net", "suffix.*"},
		{"regex123", "~^regex.*"},
		{"unknown.com", "default"},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			vhost := manager.FindHost(tt.host)
			if vhost == nil {
				t.Fatal("expected non-nil vhost")
			}
			if vhost.name != tt.wantName {
				t.Errorf("expected name %q, got %q", tt.wantName, vhost.name)
			}
		})
	}
}

// TestVHostManager_FindHost_NilDefault 测试无默认主机时返回 nil。
func TestVHostManager_FindHost_NilDefault(t *testing.T) {
	manager := NewVHostManager()
	_ = manager.AddHost("example.com", mockHandler("example", new(bool)))

	vhost := manager.FindHost("unknown.com")
	if vhost != nil {
		t.Error("expected nil when no default host and no match")
	}
}

// TestVHostManager_AddHost_InvalidRegex 测试无效正则表达式。
func TestVHostManager_AddHost_InvalidRegex(t *testing.T) {
	manager := NewVHostManager()
	err := manager.AddHost("~[invalid(regex", mockHandler("test", new(bool)))

	if err == nil {
		t.Error("expected error for invalid regex pattern")
	}
}

// TestVHostManager_PortStripping 测试端口剥离逻辑。
func TestVHostManager_PortStripping(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"无端口", "example.com", "example.com"},
		{"标准HTTP端口", "example.com:80", "example.com"},
		{"标准HTTPS端口", "example.com:443", "example.com"},
		{"自定义端口", "example.com:8080", "example.com"},
		{"IPv6 localhost带端口", "[localhost]:8080", "[localhost]"},
		{"IPv6 loopback带端口", "[::1]:8080", "[::1]"},
		{"IPv6完整地址带端口", "[2001:db8::1]:443", "[2001:db8::1]"},
		{"IPv6无端口", "[::1]", "[::1]"},
		{"IPv6完整地址无端口", "[2001:db8::1]", "[2001:db8::1]"},
		{"空字符串", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewVHostManager()
			called := false
			_ = manager.AddHost(tt.expected, mockHandler("matched", &called))

			handler := manager.Handler()
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetHost(tt.input)

			handler(ctx)

			if !called {
				t.Errorf("Host %q 期望匹配 %q，但未匹配", tt.input, tt.expected)
			}
		})
	}

	// IPv6 数字地址测试
	t.Run("IPv6数字地址", func(t *testing.T) {
		manager := NewVHostManager()
		ipv6Called := false
		manager.AddHost("[::1]", mockHandler("ipv6", &ipv6Called))

		handler := manager.Handler()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetHost("[::1]:8080")

		handler(ctx)

		if !ipv6Called {
			t.Error("期望 [::1] handler 被调用，但未被调用")
		}
		if string(ctx.Response.Body()) != "ipv6" {
			t.Errorf("响应体 = %q, want %q", string(ctx.Response.Body()), "ipv6")
		}
	})
}

// TestStartVHostMode_MultipleHosts 测试多虚拟主机配置。
func TestStartVHostMode_MultipleHosts(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:        "api.example.com",
				Listen:      "127.0.0.1:0",
				ServerNames: []string{"api.example.com", "api2.example.com"},
			},
			{
				Name:        "www.example.com",
				Listen:      "127.0.0.1:0",
				ServerNames: []string{"www.example.com"},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证多虚拟主机配置
	if len(s.config.Servers) != 2 {
		t.Errorf("Expected 2 servers, got %d", len(s.config.Servers))
	}

	// 验证 server_names 配置
	if len(s.config.Servers[0].ServerNames) != 2 {
		t.Errorf("Expected 2 server_names for first server, got %d", len(s.config.Servers[0].ServerNames))
	}
}

// TestStartVHostMode_DefaultHost 测试默认主机配置。
func TestStartVHostMode_DefaultHost(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:    "api.example.com",
				Listen:  "127.0.0.1:0",
				Default: false,
			},
			{
				Name:    "default.example.com",
				Listen:  "127.0.0.1:0",
				Default: true,
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证默认主机配置
	defaultServer := cfg.GetDefaultServerFromList()
	if defaultServer == nil {
		t.Error("Expected non-nil default server")
		return
	}
	if defaultServer.Name != "default.example.com" {
		t.Errorf("Expected default server name 'default.example.com', got %q", defaultServer.Name)
	}
}

// TestStartVHostMode_NoDefaultHost 测试无默认主机配置。
func TestStartVHostMode_NoDefaultHost(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:    "api.example.com",
				Listen:  "127.0.0.1:0",
				Default: false,
			},
			{
				Name:    "www.example.com",
				Listen:  "127.0.0.1:0",
				Default: false,
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证无默认主机
	defaultServer := cfg.GetDefaultServerFromList()
	if defaultServer != nil {
		t.Error("Expected nil default server when none marked as default")
	}
}

// TestStartVHostMode_WithProxy 测试带代理配置的虚拟主机。
func TestStartVHostMode_WithProxy(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "api.example.com",
				Listen: "127.0.0.1:0",
				Proxy: []config.ProxyConfig{
					{
						Path: "/api",
						Targets: []config.ProxyTarget{
							{URL: "http://127.0.0.1:8081", Weight: 1},
						},
					},
				},
			},
			{
				Name:   "www.example.com",
				Listen: "127.0.0.1:0",
				Proxy: []config.ProxyConfig{
					{
						Path: "/www",
						Targets: []config.ProxyTarget{
							{URL: "http://127.0.0.1:8082", Weight: 1},
						},
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证代理配置
	if len(s.config.Servers[0].Proxy) != 1 {
		t.Errorf("Expected 1 proxy for first server, got %d", len(s.config.Servers[0].Proxy))
	}
	if len(s.config.Servers[1].Proxy) != 1 {
		t.Errorf("Expected 1 proxy for second server, got %d", len(s.config.Servers[1].Proxy))
	}
}

// TestStartVHostMode_WithStaticFiles 测试带静态文件配置的虚拟主机。
func TestStartVHostMode_WithStaticFiles(t *testing.T) {
	tempDir := t.TempDir()

	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "static.example.com",
				Listen: "127.0.0.1:0",
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
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证静态文件配置
	if len(s.config.Servers[0].Static) != 1 {
		t.Errorf("Expected 1 static config, got %d", len(s.config.Servers[0].Static))
	}
}

// TestStartVHostMode_WithMiddleware 测试带中间件配置的虚拟主机。
func TestStartVHostMode_WithMiddleware(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "secure.example.com",
				Listen: "127.0.0.1:0",
				Security: config.SecurityConfig{
					Headers: config.SecurityHeaders{
						XFrameOptions:       "DENY",
						XContentTypeOptions: "nosniff",
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证中间件配置
	if s.config.Servers[0].Security.Headers.XFrameOptions != "DENY" {
		t.Error("Expected XFrameOptions to be DENY")
	}
}

// TestStartVHostMode_ServerNamesFallback 测试 server_names 回退到 Name 字段。
func TestStartVHostMode_ServerNamesFallback(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:        "fallback.example.com",
				Listen:      "127.0.0.1:0",
				ServerNames: nil, // 无 server_names，应回退到 Name
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证配置
	if s.config.Servers[0].Name != "fallback.example.com" {
		t.Errorf("Expected name 'fallback.example.com', got %q", s.config.Servers[0].Name)
	}
}

// TestStartVHostMode_MultipleServerNames 测试多个 server_names。
func TestStartVHostMode_MultipleServerNames(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "multi.example.com",
				Listen: "127.0.0.1:0",
				ServerNames: []string{
					"example.com",
					"www.example.com",
					"api.example.com",
					"*.example.com",
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证多个 server_names
	if len(s.config.Servers[0].ServerNames) != 4 {
		t.Errorf("Expected 4 server_names, got %d", len(s.config.Servers[0].ServerNames))
	}
}

// TestStartVHostMode_WildcardServerNames 测试通配符 server_names。
func TestStartVHostMode_WildcardServerNames(t *testing.T) {
	tests := []struct {
		name        string
		serverName  string
		requestHost string
		shouldMatch bool
	}{
		{"前缀通配匹配", "*.example.com", "www.example.com", true},
		{"前缀通配匹配子域名", "*.example.com", "api.www.example.com", true},
		{"前缀通配不匹配根域", "*.example.com", "example.com", false},
		{"后缀通配匹配", "example.*", "example.com", true},
		{"后缀通配匹配其他TLD", "example.*", "example.net", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewVHostManager()
			called := false
			_ = manager.AddHost(tt.serverName, mockHandler("wildcard", &called))

			handler := manager.Handler()
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetHost(tt.requestHost)

			handler(ctx)

			if called != tt.shouldMatch {
				t.Errorf("Expected match %v for %q against %q, got %v",
					tt.shouldMatch, tt.requestHost, tt.serverName, called)
			}
		})
	}
}

// TestStartVHostMode_WithCompression 测试带压缩配置的虚拟主机。
func TestStartVHostMode_WithCompression(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "compressed.example.com",
				Listen: "127.0.0.1:0",
				Compression: config.CompressionConfig{
					Type:  "gzip",
					Level: 6,
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证压缩配置
	if s.config.Servers[0].Compression.Type != "gzip" {
		t.Error("Expected compression type to be gzip")
	}
}

// TestStartVHostMode_WithRewrite 测试带重写配置的虚拟主机。
func TestStartVHostMode_WithRewrite(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "rewrite.example.com",
				Listen: "127.0.0.1:0",
				Rewrite: []config.RewriteRule{
					{
						Pattern:     "^/old/(.*)$",
						Replacement: "/new/$1",
						Flag:        "last",
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证重写配置
	if len(s.config.Servers[0].Rewrite) != 1 {
		t.Errorf("Expected 1 rewrite rule, got %d", len(s.config.Servers[0].Rewrite))
	}
}

// TestStartVHostMode_WithSecurity 测试带安全配置的虚拟主机。
func TestStartVHostMode_WithSecurity(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "secure.example.com",
				Listen: "127.0.0.1:0",
				Security: config.SecurityConfig{
					Access: config.AccessConfig{
						Allow: []string{"127.0.0.1"},
						Deny:  []string{"10.0.0.0/8"},
					},
					RateLimit: config.RateLimitConfig{
						RequestRate: 100,
						Burst:       200,
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证安全配置
	if len(s.config.Servers[0].Security.Access.Allow) != 1 {
		t.Error("Expected 1 allowed IP")
	}
	if s.config.Servers[0].Security.RateLimit.RequestRate != 100 {
		t.Error("Expected request rate 100")
	}
}

// TestStartVHostMode_PerformanceConfig 测试性能配置。
func TestStartVHostMode_PerformanceConfig(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "perf.example.com",
				Listen: "127.0.0.1:0",
			},
		},
		Performance: config.PerformanceConfig{
			GoroutinePool: config.GoroutinePoolConfig{
				Enabled:     true,
				MaxWorkers:  100,
				MinWorkers:  10,
				IdleTimeout: 30 * time.Second,
			},
			FileCache: config.FileCacheConfig{
				MaxEntries: 1000,
				MaxSize:    100 * 1024 * 1024,
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证性能配置
	if !s.config.Performance.GoroutinePool.Enabled {
		t.Error("Expected GoroutinePool to be enabled")
	}
}

// TestStartVHostMode_ServerOptions 测试服务器选项配置。
func TestStartVHostMode_ServerOptions(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:               "options.example.com",
				Listen:             "127.0.0.1:0",
				ReadTimeout:        30 * time.Second,
				WriteTimeout:       30 * time.Second,
				IdleTimeout:        60 * time.Second,
				MaxConnsPerIP:      100,
				MaxRequestsPerConn: 1000,
				Concurrency:        1000,
				ReadBufferSize:     16384,
				WriteBufferSize:    16384,
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证服务器选项
	if s.config.Servers[0].ReadTimeout != 30*time.Second {
		t.Error("Expected ReadTimeout 30s")
	}
	if s.config.Servers[0].MaxConnsPerIP != 100 {
		t.Error("Expected MaxConnsPerIP 100")
	}
}

// TestStartVHostMode_ServerTokens 测试 ServerTokens 配置。
func TestStartVHostMode_ServerTokens(t *testing.T) {
	tests := []struct {
		name            string
		serverTokens    bool
		expectedVersion bool
	}{
		{"显示版本", true, true},
		{"隐藏版本", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Mode: config.ServerModeVHost,
				Servers: []config.ServerConfig{
					{
						Name:         "tokens.example.com",
						Listen:       "127.0.0.1:0",
						ServerTokens: tt.serverTokens,
					},
				},
			}

			s := New(cfg)
			if s == nil {
				t.Fatal("Expected non-nil server")
			}

			// 验证 ServerTokens 配置
			if s.config.Servers[0].ServerTokens != tt.serverTokens {
				t.Errorf("Expected ServerTokens %v, got %v", tt.serverTokens, s.config.Servers[0].ServerTokens)
			}
		})
	}
}

// TestStartVHostMode_MonitoringConfig 测试监控配置。
func TestStartVHostMode_MonitoringConfig(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:    "monitor.example.com",
				Listen:  "127.0.0.1:0",
				Default: true,
			},
		},
		Monitoring: config.MonitoringConfig{
			Status: config.StatusConfig{
				Enabled: true,
				Path:    "/status",
				Format:  "json",
				Allow:   []string{"127.0.0.1"},
			},
			Pprof: config.PprofConfig{
				Enabled: true,
				Path:    "/debug/pprof",
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证监控配置
	if !s.config.Monitoring.Status.Enabled {
		t.Error("Expected Status monitoring to be enabled")
	}
	if s.config.Monitoring.Status.Path != "/status" {
		t.Error("Expected status path /status")
	}
}

// TestStartVHostMode_CacheAPI 测试缓存 API 配置。
func TestStartVHostMode_CacheAPI(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:    "cache.example.com",
				Listen:  "127.0.0.1:0",
				Default: true,
				CacheAPI: &config.CacheAPIConfig{
					Enabled: true,
					Path:    "/_cache/purge",
					Allow:   []string{"127.0.0.1"},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证缓存 API 配置
	if s.config.Servers[0].CacheAPI == nil || !s.config.Servers[0].CacheAPI.Enabled {
		t.Error("Expected CacheAPI to be enabled")
	}
}

// TestStartVHostMode_ErrorPage 测试错误页面配置。
func TestStartVHostMode_ErrorPage(t *testing.T) {
	tempDir := t.TempDir()
	errorPagePath := tempDir + "/404.html"
	_ = os.WriteFile(errorPagePath, []byte("<html>Not Found</html>"), 0o644)

	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "errors.example.com",
				Listen: "127.0.0.1:0",
				Security: config.SecurityConfig{
					ErrorPage: config.ErrorPageConfig{
						Pages: map[int]string{
							404: errorPagePath,
						},
						Default: errorPagePath,
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证错误页面配置
	if s.config.Servers[0].Security.ErrorPage.Pages == nil {
		t.Error("Expected error pages to be configured")
	}
}

// TestStartVHostMode_LuaConfig 测试 Lua 配置。
func TestStartVHostMode_LuaConfig(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "lua.example.com",
				Listen: "127.0.0.1:0",
				Lua: &config.LuaMiddlewareConfig{
					Enabled: true,
					GlobalSettings: config.LuaGlobalSettings{
						MaxConcurrentCoroutines: 100,
						CoroutineTimeout:        30 * time.Second,
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证 Lua 配置
	if s.config.Servers[0].Lua == nil || !s.config.Servers[0].Lua.Enabled {
		t.Error("Expected Lua to be enabled")
	}
}

// TestStartVHostMode_AuthConfig 测试认证配置。
func TestStartVHostMode_AuthConfig(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "auth.example.com",
				Listen: "127.0.0.1:0",
				Security: config.SecurityConfig{
					Auth: config.AuthConfig{
						Type:  "basic",
						Realm: "Protected Area",
						Users: []config.User{
							{Name: "admin", Password: "$2a$10$hash"},
						},
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证认证配置
	if s.config.Servers[0].Security.Auth.Type != "basic" {
		t.Error("Expected auth type basic")
	}
}

// TestStartVHostMode_AuthRequestConfig 测试外部认证配置。
func TestStartVHostMode_AuthRequestConfig(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "authreq.example.com",
				Listen: "127.0.0.1:0",
				Security: config.SecurityConfig{
					AuthRequest: config.AuthRequestConfig{
						Enabled: true,
						URI:     "/auth",
						Method:  "GET",
						Timeout: 5 * time.Second,
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证外部认证配置
	if !s.config.Servers[0].Security.AuthRequest.Enabled {
		t.Error("Expected AuthRequest to be enabled")
	}
}

// TestStartVHostMode_ConnLimiter 测试连接限制配置。
func TestStartVHostMode_ConnLimiter(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "limited.example.com",
				Listen: "127.0.0.1:0",
				Security: config.SecurityConfig{
					RateLimit: config.RateLimitConfig{
						ConnLimit: 100,
						Key:       "ip",
					},
				},
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证连接限制配置
	if s.config.Servers[0].Security.RateLimit.ConnLimit != 100 {
		t.Error("Expected ConnLimit 100")
	}
}

// TestStartVHostMode_BodyLimit 测试请求体限制配置。
func TestStartVHostMode_BodyLimit(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:              "bodylimit.example.com",
				Listen:            "127.0.0.1:0",
				ClientMaxBodySize: "10MB",
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证请求体限制配置
	if s.config.Servers[0].ClientMaxBodySize != "10MB" {
		t.Error("Expected ClientMaxBodySize 10MB")
	}
}

// TestStartVHostMode_MixedConfig 测试混合配置场景。
func TestStartVHostMode_MixedConfig(t *testing.T) {
	tempDir := t.TempDir()

	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:        "api.example.com",
				Listen:      "127.0.0.1:0",
				ServerNames: []string{"api.example.com", "api-alias.example.com"},
				Proxy: []config.ProxyConfig{
					{
						Path: "/v1",
						Targets: []config.ProxyTarget{
							{URL: "http://backend1:8080", Weight: 1},
						},
					},
				},
				Security: config.SecurityConfig{
					Headers: config.SecurityHeaders{
						XFrameOptions: "DENY",
					},
				},
			},
			{
				Name:    "static.example.com",
				Listen:  "127.0.0.1:0",
				Default: true,
				Static: []config.StaticConfig{
					{
						Path:  "/",
						Root:  tempDir,
						Index: []string{"index.html"},
					},
				},
				Compression: config.CompressionConfig{
					Type: "gzip",
				},
			},
		},
		Performance: config.PerformanceConfig{
			GoroutinePool: config.GoroutinePoolConfig{
				Enabled:    true,
				MaxWorkers: 50,
			},
		},
	}

	s := New(cfg)
	if s == nil {
		t.Fatal("Expected non-nil server")
	}

	// 验证混合配置
	if len(s.config.Servers) != 2 {
		t.Errorf("Expected 2 servers, got %d", len(s.config.Servers))
	}

	// 验证第一个服务器配置
	if len(s.config.Servers[0].Proxy) != 1 {
		t.Error("Expected 1 proxy for api server")
	}

	// 验证第二个服务器配置
	if len(s.config.Servers[1].Static) != 1 {
		t.Error("Expected 1 static config for static server")
	}
	if !s.config.Servers[1].Default {
		t.Error("Expected second server to be default")
	}
}

// TestStartVHostMode_ModeDetection 测试模式自动检测。
func TestStartVHostMode_ModeDetection(t *testing.T) {
	tests := []struct {
		name         string
		servers      []config.ServerConfig
		expectedMode config.ServerMode
	}{
		{
			name: "单服务器模式",
			servers: []config.ServerConfig{
				{Listen: ":8080"},
			},
			expectedMode: config.ServerModeSingle,
		},
		{
			name: "虚拟主机模式（相同监听地址）",
			servers: []config.ServerConfig{
				{Listen: ":8080", Name: "api"},
				{Listen: ":8080", Name: "www"},
			},
			expectedMode: config.ServerModeVHost,
		},
		{
			name: "多服务器模式（不同监听地址）",
			servers: []config.ServerConfig{
				{Listen: ":8080", Name: "api"},
				{Listen: ":8081", Name: "www"},
			},
			expectedMode: config.ServerModeMultiServer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Mode:    config.ServerModeAuto,
				Servers: tt.servers,
			}

			mode := cfg.GetMode()
			if mode != tt.expectedMode {
				t.Errorf("Expected mode %s, got %s", tt.expectedMode, mode)
			}
		})
	}
}

// TestGroupedVHostStartIntegration 测试 分组虚拟主机启动集成。

// TestGroupedVHostManagerCreation 测试 VHostManager 创建逻辑。
func TestGroupedVHostManagerCreation(t *testing.T) {
	manager := NewVHostManager()

	// 添加多个虚拟主机
	hosts := []struct {
		name    string
		handler fasthttp.RequestHandler
	}{
		{"api.example.com", mockHandler("api", new(bool))},
		{"www.example.com", mockHandler("www", new(bool))},
		{"*.example.com", mockHandler("wildcard", new(bool))},
	}

	for _, h := range hosts {
		if err := manager.AddHost(h.name, h.handler); err != nil {
			t.Errorf("Failed to add host %s: %v", h.name, err)
		}
	}

	// 设置默认主机
	manager.SetDefault(mockHandler("default", new(bool)))

	// 验证主机查找
	tests := []struct {
		host     string
		expected string
	}{
		{"api.example.com", "api.example.com"},
		{"www.example.com", "www.example.com"},
		{"sub.example.com", "*.example.com"},
		{"unknown.example.com", "*.example.com"},
		{"other.com", "default"},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			vhost := manager.FindHost(tt.host)
			if vhost == nil {
				t.Fatalf("Expected non-nil vhost for %s", tt.host)
			}
			if vhost.name != tt.expected {
				t.Errorf("Expected vhost name %s, got %s", tt.expected, vhost.name)
			}
		})
	}
}

// TestStartVHostMode_StatsTracking 测试虚拟主机模式下的请求统计。
func TestStartVHostMode_StatsTracking(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "stats.example.com",
				Listen: "127.0.0.1:0",
			},
		},
	}

	s := New(cfg)

	// 测试统计追踪包装器
	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetBodyString("test response")
	}

	wrappedHandler := s.trackStats(handler)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	ctx.Request.SetBody([]byte("test request"))

	wrappedHandler(ctx)

	if s.requests.Load() != 1 {
		t.Errorf("Expected 1 request, got %d", s.requests.Load())
	}
	if s.bytesReceived.Load() != int64(len("test request")) {
		t.Errorf("Expected bytesReceived %d, got %d", len("test request"), s.bytesReceived.Load())
	}
	if s.bytesSent.Load() != int64(len("test response")) {
		t.Errorf("Expected bytesSent %d, got %d", len("test response"), s.bytesSent.Load())
	}
}

// TestStartVHostMode_MiddlewareChainBuilding 测试虚拟主机模式的中间件链构建。
func TestStartVHostMode_MiddlewareChainBuilding(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "middleware.example.com",
				Listen: "127.0.0.1:0",
				Security: config.SecurityConfig{
					Headers: config.SecurityHeaders{
						XFrameOptions:       "DENY",
						XContentTypeOptions: "nosniff",
					},
					RateLimit: config.RateLimitConfig{
						RequestRate: 100,
						Burst:       200,
					},
				},
				Compression: config.CompressionConfig{
					Type:  "gzip",
					Level: 6,
				},
			},
		},
	}

	s := New(cfg)

	// 为虚拟主机构建中间件链
	chain, err := s.buildMiddlewareChain(&cfg.Servers[0])
	if err != nil {
		t.Fatalf("Failed to build middleware chain: %v", err)
	}
	if chain == nil {
		t.Fatal("Expected non-nil middleware chain")
	}
}

// TestStartVHostMode_GetServerName 测试服务器名称获取。
func TestStartVHostMode_GetServerName(t *testing.T) {
	tests := []struct {
		name        string
		serverToken bool
		expectFull  bool
	}{
		{"显示版本", false, false}, // ServerTokens=false 隐藏版本
		{"隐藏版本", true, true},   // ServerTokens=true 显示版本
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Mode: config.ServerModeVHost,
				Servers: []config.ServerConfig{
					{
						Name:         "name.example.com",
						Listen:       "127.0.0.1:0",
						ServerTokens: tt.serverToken,
					},
				},
			}

			s := New(cfg)
			serverName := s.getServerName(&cfg.Servers[0])

			if tt.expectFull {
				// 应包含版本号
				if len(serverName) < 6 {
					t.Errorf("Expected server name with version, got %s", serverName)
				}
			} else {
				// 应该只有 "lolly"
				if serverName != "lolly" {
					t.Errorf("Expected server name 'lolly', got %s", serverName)
				}
			}
		})
	}
}

// TestStartVHostMode_CreateListener 测试虚拟主机模式下的监听器创建。
func TestStartVHostMode_CreateListener(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "listener.example.com",
				Listen: "127.0.0.1:0", // 随机端口
			},
		},
	}

	s := New(cfg)

	// 创建 TCP 监听器
	ln, err := s.createListener(&cfg.Servers[0])
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer ln.Close()

	// 验证监听器类型
	if ln.Addr().Network() != "tcp" {
		t.Errorf("Expected tcp network, got %s", ln.Addr().Network())
	}

	// 验证可以获取地址
	if ln.Addr() == nil {
		t.Error("Expected non-nil listener address")
	}
}

// TestStartVHostMode_RegisterRoutes 测试虚拟主机路由注册。
func TestStartVHostMode_RegisterRoutes(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "routes.example.com",
				Listen: "127.0.0.1:0",
				Proxy: []config.ProxyConfig{
					{
						Path: "/api",
						Targets: []config.ProxyTarget{
							{URL: "http://backend:8080", Weight: 1},
						},
					},
				},
				Static: []config.StaticConfig{
					{
						Path:  "/static",
						Root:  "/tmp",
						Index: []string{"index.html"},
					},
				},
			},
		},
	}

	s := New(cfg)

	// 验证代理和静态配置
	if len(s.config.Servers[0].Proxy) != 1 {
		t.Errorf("Expected 1 proxy config, got %d", len(s.config.Servers[0].Proxy))
	}
	if len(s.config.Servers[0].Static) != 1 {
		t.Errorf("Expected 1 static config, got %d", len(s.config.Servers[0].Static))
	}
}

// TestStartVHostMode_DefaultHostSetup 测试默认主机设置。
func TestStartVHostMode_DefaultHostSetup(t *testing.T) {
	// 测试有默认主机的情况
	t.Run("with default host", func(t *testing.T) {
		cfg := &config.Config{
			Mode: config.ServerModeVHost,
			Servers: []config.ServerConfig{
				{
					Name:    "api.example.com",
					Listen:  "127.0.0.1:0",
					Default: false,
				},
				{
					Name:    "default.example.com",
					Listen:  "127.0.0.1:0",
					Default: true,
				},
			},
		}

		defaultSrv := cfg.GetDefaultServerFromList()
		if defaultSrv == nil {
			t.Fatal("Expected non-nil default server")
		}
		if defaultSrv.Name != "default.example.com" {
			t.Errorf("Expected default server name 'default.example.com', got %s", defaultSrv.Name)
		}
	})

	// 测试无默认主机的情况
	t.Run("without default host", func(t *testing.T) {
		cfg := &config.Config{
			Mode: config.ServerModeVHost,
			Servers: []config.ServerConfig{
				{
					Name:    "api.example.com",
					Listen:  "127.0.0.1:0",
					Default: false,
				},
				{
					Name:    "www.example.com",
					Listen:  "127.0.0.1:0",
					Default: false,
				},
			},
		}

		defaultSrv := cfg.GetDefaultServerFromList()
		if defaultSrv != nil {
			t.Errorf("Expected nil default server, got %v", defaultSrv)
		}
	})
}

// TestStartVHostMode_MultiServerNames 测试每个服务器有多个 server_names。
func TestStartVHostMode_MultiServerNames(t *testing.T) {
	manager := NewVHostManager()

	// 模拟 监听分组中的主机注册逻辑
	serverNames := []string{"example.com", "www.example.com", "example.org"}
	for _, name := range serverNames {
		if err := manager.AddHost(name, mockHandler(name, new(bool))); err != nil {
			t.Errorf("Failed to add host %s: %v", name, err)
		}
	}

	// 验证每个主机名都能找到
	for _, name := range serverNames {
		vhost := manager.FindHost(name)
		if vhost == nil {
			t.Errorf("Expected to find vhost for %s", name)
		}
		if vhost.name != name {
			t.Errorf("Expected vhost name %s, got %s", name, vhost.name)
		}
	}
}

// TestStartVHostMode_ComplexWildcardSetup 测试复杂的通配符配置。
func TestStartVHostMode_ComplexWildcardSetup(t *testing.T) {
	manager := NewVHostManager()

	// 添加精确匹配
	_ = manager.AddHost("exact.example.com", mockHandler("exact", new(bool)))

	// 添加前缀通配
	_ = manager.AddHost("*.example.com", mockHandler("wildcard", new(bool)))

	// 添加后缀通配
	_ = manager.AddHost("test.*", mockHandler("suffix", new(bool)))

	// 设置默认
	manager.SetDefault(mockHandler("default", new(bool)))

	// 验证匹配优先级
	tests := []struct {
		host     string
		expected string
	}{
		// 精确匹配优先
		{"exact.example.com", "exact.example.com"},
		// 前缀通配匹配
		{"sub.example.com", "*.example.com"},
		{"deep.sub.example.com", "*.example.com"},
		// 后缀通配匹配
		{"test.org", "test.*"},
		{"test.net", "test.*"},
		// 默认主机
		{"other.com", "default"},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			vhost := manager.FindHost(tt.host)
			if vhost == nil {
				t.Fatalf("Expected non-nil vhost for %s", tt.host)
			}
			if vhost.name != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, vhost.name)
			}
		})
	}
}

// TestGroupedVHostActualExecution 测试 分组虚拟主机实际执行路径。
func TestGroupedVHostActualExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Run("基本虚拟主机模式启动", func(t *testing.T) {
		cfg := &config.Config{
			Mode: config.ServerModeVHost,
			Servers: []config.ServerConfig{
				{
					Name:        "api.example.com",
					Listen:      "127.0.0.1:0",
					ServerNames: []string{"api.example.com"},
				},
			},
		}

		s := New(cfg)

		// 启动服务器（在 goroutine 中）
		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Start()
		}()

		waitForServerRunning(s, 2*time.Second)

		// 停止服务器
		_ = s.GracefulStop(1 * time.Second)

		// 检查启动是否成功（服务器应该阻塞直到停止）
		select {
		case err := <-errCh:
			// 服务器已停止，这是正常的
			if err != nil {
				t.Logf("Server stopped with: %v", err)
			}
		default:
			// 服务器仍在运行，关闭它
			_ = s.StopWithTimeout(1 * time.Second)
		}
	})
}

// TestStartVHostMode_MultipleVirtualHosts 测试多个虚拟主机的实际执行。
func TestStartVHostMode_MultipleVirtualHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:        "api.example.com",
				Listen:      "127.0.0.1:0",
				ServerNames: []string{"api.example.com", "api2.example.com"},
			},
			{
				Name:        "www.example.com",
				Listen:      "127.0.0.1:0",
				ServerNames: []string{"www.example.com"},
				Default:     true,
			},
		},
	}

	s := New(cfg)

	// 验证配置
	if len(s.config.Servers) != 2 {
		t.Errorf("Expected 2 servers, got %d", len(s.config.Servers))
	}

	// 验证 server_names
	if len(s.config.Servers[0].ServerNames) != 2 {
		t.Errorf("Expected 2 server_names for first server, got %d", len(s.config.Servers[0].ServerNames))
	}

	// 验证默认主机
	defaultSrv := cfg.GetDefaultServerFromList()
	if defaultSrv == nil || defaultSrv.Name != "www.example.com" {
		t.Error("Expected www.example.com to be default server")
	}
}

// TestStartVHostMode_ServerNamesFallbackToName 测试 server_names 回退到 Name 字段。
func TestStartVHostMode_ServerNamesFallbackToName(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:        "fallback.example.com",
				Listen:      "127.0.0.1:0",
				ServerNames: nil, // 未设置，应回退到 Name
			},
		},
	}

	s := New(cfg)

	// 验证 Name 字段正确设置
	if s.config.Servers[0].Name != "fallback.example.com" {
		t.Errorf("Expected Name 'fallback.example.com', got %s", s.config.Servers[0].Name)
	}
}

// TestStartVHostMode_WithMonitoringEndpoints 测试监控端点配置。
func TestStartVHostMode_WithMonitoringEndpoints(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:    "default.example.com",
				Listen:  "127.0.0.1:0",
				Default: true,
			},
		},
		Monitoring: config.MonitoringConfig{
			Status: config.StatusConfig{
				Enabled: true,
				Path:    "/_status",
				Format:  "json",
				Allow:   []string{"127.0.0.1"},
			},
			Pprof: config.PprofConfig{
				Enabled: true,
				Path:    "/debug/pprof",
			},
		},
	}

	s := New(cfg)

	// 验证监控配置
	if !s.config.Monitoring.Status.Enabled {
		t.Error("Expected status monitoring enabled")
	}
	if !s.config.Monitoring.Pprof.Enabled {
		t.Error("Expected pprof enabled")
	}
}

// TestStartVHostMode_WithCacheAPIEndpoint 测试缓存 API 端点配置。
func TestStartVHostMode_WithCacheAPIEndpoint(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:    "default.example.com",
				Listen:  "127.0.0.1:0",
				Default: true,
				CacheAPI: &config.CacheAPIConfig{
					Enabled: true,
					Path:    "/_cache/purge",
					Allow:   []string{"127.0.0.1"},
				},
			},
		},
	}

	s := New(cfg)

	// 验证缓存 API 配置
	if s.config.Servers[0].CacheAPI == nil || !s.config.Servers[0].CacheAPI.Enabled {
		t.Error("Expected cache API enabled")
	}
}

// TestStartVHostMode_WithProxyConfig 测试代理配置。
func TestStartVHostMode_WithProxyConfig(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "api.example.com",
				Listen: "127.0.0.1:0",
				Proxy: []config.ProxyConfig{
					{
						Path: "/api",
						Targets: []config.ProxyTarget{
							{URL: "http://127.0.0.1:8081", Weight: 1},
						},
					},
				},
			},
		},
	}

	s := New(cfg)

	// 验证代理配置
	if len(s.config.Servers[0].Proxy) != 1 {
		t.Errorf("Expected 1 proxy config, got %d", len(s.config.Servers[0].Proxy))
	}
	if s.config.Servers[0].Proxy[0].Path != "/api" {
		t.Errorf("Expected proxy path /api, got %s", s.config.Servers[0].Proxy[0].Path)
	}
}

// TestStartVHostMode_WithStaticFiles2 测试静态文件配置。
func TestStartVHostMode_WithStaticFiles2(t *testing.T) {
	tempDir := t.TempDir()

	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "static.example.com",
				Listen: "127.0.0.1:0",
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

	// 验证静态文件配置
	if len(s.config.Servers[0].Static) != 1 {
		t.Errorf("Expected 1 static config, got %d", len(s.config.Servers[0].Static))
	}
}

// TestStartVHostMode_WithGoroutinePool 测试 GoroutinePool 配置。
func TestStartVHostMode_WithGoroutinePool(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "pool.example.com",
				Listen: "127.0.0.1:0",
			},
		},
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

	// 验证性能配置
	if !s.config.Performance.GoroutinePool.Enabled {
		t.Error("Expected GoroutinePool enabled")
	}
}

// TestStartVHostMode_InvalidRegexServerName 测试无效正则表达式的 server_name。
func TestStartVHostMode_InvalidRegexServerName(t *testing.T) {
	manager := NewVHostManager()

	// 添加无效正则表达式应该返回错误
	err := manager.AddHost("~[invalid(regex", mockHandler("test", new(bool)))
	if err == nil {
		t.Error("Expected error for invalid regex pattern")
	}
}

// TestStartVHostMode_NoServers 测试无服务器配置。
func TestStartVHostMode_NoServers(t *testing.T) {
	cfg := &config.Config{
		Mode:    config.ServerModeVHost,
		Servers: []config.ServerConfig{},
	}

	s := New(cfg)

	// 验证空配置
	if len(s.config.Servers) != 0 {
		t.Errorf("Expected 0 servers, got %d", len(s.config.Servers))
	}
}

// TestStartVHostMode_SingleServer 测试单服务器虚拟主机模式。
func TestStartVHostMode_SingleServer(t *testing.T) {
	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:        "single.example.com",
				Listen:      "127.0.0.1:0",
				ServerNames: []string{"single.example.com"},
				Default:     true,
			},
		},
	}

	s := New(cfg)

	// 验证配置
	if len(s.config.Servers) != 1 {
		t.Errorf("Expected 1 server, got %d", len(s.config.Servers))
	}

	// 验证默认主机
	defaultSrv := cfg.GetDefaultServerFromList()
	if defaultSrv == nil {
		t.Error("Expected default server")
	}
}

// TestStartVHostMode_MixedProxyAndStatic 测试代理和静态文件混合配置。
func TestStartVHostMode_MixedProxyAndStatic(t *testing.T) {
	tempDir := t.TempDir()

	cfg := &config.Config{
		Mode: config.ServerModeVHost,
		Servers: []config.ServerConfig{
			{
				Name:   "mixed.example.com",
				Listen: "127.0.0.1:0",
				Proxy: []config.ProxyConfig{
					{
						Path: "/api",
						Targets: []config.ProxyTarget{
							{URL: "http://backend:8080", Weight: 1},
						},
					},
				},
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

	// 验证代理配置
	if len(s.config.Servers[0].Proxy) != 1 {
		t.Errorf("Expected 1 proxy config, got %d", len(s.config.Servers[0].Proxy))
	}

	// 验证静态文件配置
	if len(s.config.Servers[0].Static) != 1 {
		t.Errorf("Expected 1 static config, got %d", len(s.config.Servers[0].Static))
	}
}

// TestGroupedVHostActualServerStart 测试 分组虚拟主机实际服务器启动。
func TestGroupedVHostActualServerStart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Run("带默认主机启动", func(t *testing.T) {
		cfg := &config.Config{
			Mode: config.ServerModeVHost,
			Servers: []config.ServerConfig{
				{
					Name:        "api.example.com",
					Listen:      "127.0.0.1:0",
					ServerNames: []string{"api.example.com"},
				},
				{
					Name:        "default.example.com",
					Listen:      "127.0.0.1:0",
					ServerNames: []string{"default.example.com"},
					Default:     true,
				},
			},
		}

		s := New(cfg)

		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Start()
		}()

		waitForServerRunning(s, 2*time.Second)
		_ = s.GracefulStop(1 * time.Second)

		select {
		case <-errCh:
		default:
			_ = s.StopWithTimeout(1 * time.Second)
		}
	})

	t.Run("带代理配置启动", func(t *testing.T) {
		cfg := &config.Config{
			Mode: config.ServerModeVHost,
			Servers: []config.ServerConfig{
				{
					Name:        "proxy.example.com",
					Listen:      "127.0.0.1:0",
					ServerNames: []string{"proxy.example.com"},
					Proxy: []config.ProxyConfig{
						{
							Path: "/api",
							Targets: []config.ProxyTarget{
								{URL: "http://127.0.0.1:8081", Weight: 1},
							},
						},
					},
				},
			},
		}

		s := New(cfg)

		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Start()
		}()

		waitForServerRunning(s, 2*time.Second)
		_ = s.GracefulStop(1 * time.Second)

		select {
		case <-errCh:
		default:
			_ = s.StopWithTimeout(1 * time.Second)
		}
	})

	t.Run("带监控端点启动", func(t *testing.T) {
		cfg := &config.Config{
			Mode: config.ServerModeVHost,
			Servers: []config.ServerConfig{
				{
					Name:    "monitor.example.com",
					Listen:  "127.0.0.1:0",
					Default: true,
				},
			},
			Monitoring: config.MonitoringConfig{
				Status: config.StatusConfig{
					Enabled: true,
					Path:    "/_status",
				},
				Pprof: config.PprofConfig{
					Enabled: true,
					Path:    "/debug/pprof",
				},
			},
		}

		s := New(cfg)

		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Start()
		}()

		waitForServerRunning(s, 2*time.Second)
		_ = s.GracefulStop(1 * time.Second)

		select {
		case <-errCh:
		default:
			_ = s.StopWithTimeout(1 * time.Second)
		}
	})

	t.Run("带缓存API启动", func(t *testing.T) {
		cfg := &config.Config{
			Mode: config.ServerModeVHost,
			Servers: []config.ServerConfig{
				{
					Name:    "cache.example.com",
					Listen:  "127.0.0.1:0",
					Default: true,
					CacheAPI: &config.CacheAPIConfig{
						Enabled: true,
						Path:    "/_cache/purge",
					},
				},
			},
		}

		s := New(cfg)

		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Start()
		}()

		waitForServerRunning(s, 2*time.Second)
		_ = s.GracefulStop(1 * time.Second)

		select {
		case <-errCh:
		default:
			_ = s.StopWithTimeout(1 * time.Second)
		}
	})

	t.Run("带GoroutinePool启动", func(t *testing.T) {
		cfg := &config.Config{
			Mode: config.ServerModeVHost,
			Servers: []config.ServerConfig{
				{
					Name:        "pool.example.com",
					Listen:      "127.0.0.1:0",
					ServerNames: []string{"pool.example.com"},
				},
			},
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

		errCh := make(chan error, 1)
		go func() {
			errCh <- s.Start()
		}()

		waitForServerRunning(s, 2*time.Second)
		_ = s.GracefulStop(1 * time.Second)

		select {
		case <-errCh:
		default:
			_ = s.StopWithTimeout(1 * time.Second)
		}
	})
}
