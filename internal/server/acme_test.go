// Package server 提供 HTTP 服务器功能的测试。
//
// 该文件测试 ACME 自动证书在服务端的集成逻辑，包括：
//   - 启动阶段 ACME 管理器的创建与静态证书优先
//   - http-01 挑战处理器的构造与域名分派
//   - 挑战路由在 LocationEngine / Router 上的注册
//   - 申请域名的推导优先级
//
// 注意：测试不发起真实 ACME 网络请求。
//
// 作者：xfy
package server

import (
	"testing"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/handler"
	"rua.plus/lolly/internal/matcher"
)

// acmeServerConfig 构造一个启用 ACME（无静态证书）的服务器配置。
func acmeServerConfig(t *testing.T, challenge string, hosts ...string) config.ServerConfig {
	t.Helper()
	return config.ServerConfig{
		Listen: ":443",
		Name:   "example.com",
		SSL: config.SSLConfig{
			ACME: config.ACMEConfig{
				Enabled:   true,
				StatePath: t.TempDir(),
				Challenge: challenge,
				Hosts:     hosts,
			},
		},
	}
}

// TestInitACMEManagers 验证启动阶段为启用 ACME 的服务器创建管理器。
func TestInitACMEManagers(t *testing.T) {
	cfg := &config.Config{
		Servers: []config.ServerConfig{acmeServerConfig(t, "")},
	}
	s := New(cfg)

	if err := s.initACMEManagers(); err != nil {
		t.Fatalf("initACMEManagers() error = %v", err)
	}
	if len(s.acmeManagers) != 1 {
		t.Fatalf("acmeManagers len = %d, want 1", len(s.acmeManagers))
	}
	if s.acmeManagers[0] == nil {
		t.Fatal("acmeManagers[0] = nil, want manager")
	}
	if s.acmeManagerAt(0) == nil {
		t.Error("acmeManagerAt(0) = nil, want manager")
	}
	if s.acmeManagerAt(5) != nil {
		t.Error("acmeManagerAt(out-of-range) should be nil")
	}
}

// TestInitACMEManagers_StaticCertWins 验证配置静态证书时跳过 ACME。
func TestInitACMEManagers_StaticCertWins(t *testing.T) {
	srv := acmeServerConfig(t, "")
	srv.SSL.Cert = "/tmp/cert.pem"
	srv.SSL.Key = "/tmp/key.pem"

	cfg := &config.Config{Servers: []config.ServerConfig{srv}}
	s := New(cfg)

	if err := s.initACMEManagers(); err != nil {
		t.Fatalf("initACMEManagers() error = %v", err)
	}
	if s.acmeManagers[0] != nil {
		t.Error("acmeManagers[0] should be nil when static cert is configured")
	}
}

// TestAcmeChallengeHandler 验证挑战处理器按挑战类型构造与域名分派。
func TestAcmeChallengeHandler(t *testing.T) {
	// tls-alpn-01 无需 HTTP 挑战处理器
	tlsSrv := acmeServerConfig(t, config.ACMEChallengeTLSALPN01, "example.com")
	s := New(&config.Config{Servers: []config.ServerConfig{tlsSrv}})
	if err := s.initACMEManagers(); err != nil {
		t.Fatalf("initACMEManagers() error = %v", err)
	}
	if h := s.acmeChallengeHandler(); h != nil {
		t.Error("acmeChallengeHandler() should be nil for tls-alpn-01")
	}

	// http-01 需要处理器
	httpSrv := acmeServerConfig(t, config.ACMEChallengeHTTP01, "example.com")
	s2 := New(&config.Config{Servers: []config.ServerConfig{httpSrv}})
	if err := s2.initACMEManagers(); err != nil {
		t.Fatalf("initACMEManagers() error = %v", err)
	}
	h := s2.acmeChallengeHandler()
	if h == nil {
		t.Fatal("acmeChallengeHandler() = nil, want handler for http-01")
	}

	// 命中白名单但无对应令牌 → autocert 返回 404
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	ctx.Request.SetRequestURI(acmeChallengePath + "token")
	ctx.Request.Header.SetHost("example.com")
	h(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown token", ctx.Response.StatusCode())
	}

	// 未命中白名单 → 分派不到任何管理器，返回 404
	ctx2 := &fasthttp.RequestCtx{}
	ctx2.Init(&fasthttp.Request{}, nil, nil)
	ctx2.Request.SetRequestURI(acmeChallengePath + "token")
	ctx2.Request.Header.SetHost("evil.example.net")
	h(ctx2)
	if ctx2.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404 for host outside whitelist", ctx2.Response.StatusCode())
	}
}

// TestRegisterACMEChallengeRouter 验证 http-01 挑战路由注册到 Router。
func TestRegisterACMEChallengeRouter(t *testing.T) {
	srv := acmeServerConfig(t, config.ACMEChallengeHTTP01, "example.com")
	s := New(&config.Config{Servers: []config.ServerConfig{srv}})
	if err := s.initACMEManagers(); err != nil {
		t.Fatalf("initACMEManagers() error = %v", err)
	}

	router := handler.NewRouter()
	s.registerACMEChallengeRouter(router)
	if router.Handler() == nil {
		t.Fatal("router handler is nil")
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	ctx.Request.SetRequestURI(acmeChallengePath + "token")
	ctx.Request.Header.SetHost("evil.example.net")
	router.Handler()(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404 for host outside whitelist", ctx.Response.StatusCode())
	}
}

// TestRegisterACMEChallengeLocation 验证 http-01 挑战路由注册到 LocationEngine。
func TestRegisterACMEChallengeLocation(t *testing.T) {
	srv := acmeServerConfig(t, config.ACMEChallengeHTTP01, "example.com")
	s := New(&config.Config{Servers: []config.ServerConfig{srv}})
	if err := s.initACMEManagers(); err != nil {
		t.Fatalf("initACMEManagers() error = %v", err)
	}

	engine := matcher.NewLocationEngine()
	if err := s.registerACMEChallengeLocation(engine); err != nil {
		t.Fatalf("registerACMEChallengeLocation() error = %v", err)
	}
	engine.MarkInitialized()

	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	ctx.Request.SetRequestURI(acmeChallengePath + "token")
	ctx.Request.Header.SetHost("evil.example.net")

	result := engine.Match(ctx.Path())
	if result == nil || result.Handler == nil {
		t.Fatal("no route matched for challenge path")
	}
	result.Handler(ctx)
	matcher.ReleaseMatchResult(result)
	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404 for host outside whitelist", ctx.Response.StatusCode())
	}
}

// TestAcmeHosts 验证申请域名推导优先级。
func TestAcmeHosts(t *testing.T) {
	// acme.hosts 优先
	hosts := acmeHosts(&config.ACMEConfig{Hosts: []string{"a.com"}}, []string{"b.com"}, "c.com")
	if len(hosts) != 1 || hosts[0] != "a.com" {
		t.Errorf("acmeHosts() = %v, want [a.com]", hosts)
	}

	// 其次 server_names
	hosts = acmeHosts(&config.ACMEConfig{}, []string{"b.com"}, "c.com")
	if len(hosts) != 1 || hosts[0] != "b.com" {
		t.Errorf("acmeHosts() = %v, want [b.com]", hosts)
	}

	// 最后回退到 name
	hosts = acmeHosts(&config.ACMEConfig{}, nil, "c.com")
	if len(hosts) != 1 || hosts[0] != "c.com" {
		t.Errorf("acmeHosts() = %v, want [c.com]", hosts)
	}

	// 都为空
	hosts = acmeHosts(&config.ACMEConfig{}, nil, "")
	if len(hosts) != 0 {
		t.Errorf("acmeHosts() = %v, want empty", hosts)
	}
}
