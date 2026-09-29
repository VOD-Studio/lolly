// Package server 验证容器发现路由在服务器边界上的动态行为。
//
// 测试覆盖快照发布、主机与最长路径匹配、路由移除、HTTPS 策略、目标路径改写和静态主机优先级。
//
// 作者：xfy
package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
	containerdiscovery "rua.plus/lolly/internal/discovery/container"
	"rua.plus/lolly/internal/middleware"
)

// countingMiddleware 记录请求穿过中间件链的次数。
type countingMiddleware struct {
	count int
}

// Name 返回测试中间件名称。
func (m *countingMiddleware) Name() string { return "counting" }

// Process 在调用下游前递增计数。
func (m *countingMiddleware) Process(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		m.count++
		next(ctx)
	}
}

// TestContainerRouterPublishesMatchesAndRemovesRoutes 验证快照替换是原子的且最长路径优先。
func TestContainerRouterPublishesMatchesAndRemovesRoutes(t *testing.T) {
	backend := startContainerBackend(t)
	router := newContainerRouter(&config.Config{}, false, ":80", nil)
	router.publish(containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{
		containerRoute("dynamic.example", "/", "/root", backend),
		containerRoute("dynamic.example", "/api", "/v1", backend),
	}})

	ctx := newContainerRequest("dynamic.example", "/apiary")
	if !router.serve(ctx) || string(ctx.Response.Body()) != "/root/apiary" {
		t.Fatalf("路径前缀不应跨路径段匹配：%q", ctx.Response.Body())
	}

	ctx = newContainerRequest("dynamic.example", "/api/items?x=1")
	if !router.serve(ctx) {
		t.Fatal("动态路由未匹配")
	}
	if got := string(ctx.Response.Body()); got != "/v1/items?x=1" {
		t.Fatalf("最长路径或目标路径改写错误：%q", got)
	}
	if got := string(ctx.RequestURI()); got != "/api/items?x=1" {
		t.Fatalf("代理后未恢复请求 URI：%q", got)
	}

	router.publish(containerdiscovery.Snapshot{})
	if router.serve(newContainerRequest("dynamic.example", "/api/items")) {
		t.Fatal("已移除路由仍然匹配")
	}
}

// TestContainerRouterPreservesURIWithoutDestination 验证空目标路径完整保留原始 URI。
func TestContainerRouterPreservesURIWithoutDestination(t *testing.T) {
	backend := startContainerBackend(t)
	router := newContainerRouter(&config.Config{}, false, ":80", nil)
	router.publish(containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{
		containerRoute("dynamic.example", "/api", "", backend),
	}})

	ctx := newContainerRequest("dynamic.example", "/api/items?x=1&x=2")
	if !router.serve(ctx) {
		t.Fatal("动态路由未匹配")
	}
	if got := string(ctx.Response.Body()); got != "/api/items?x=1&x=2" {
		t.Fatalf("空目标路径未保留完整 URI：%q", got)
	}
}

// TestContainerRouterMiddlewareRunsOnce 验证动态请求继承模板链，静态回退不重复执行。
func TestContainerRouterMiddlewareRunsOnce(t *testing.T) {
	backend := startContainerBackend(t)
	counter := &countingMiddleware{}
	router := newContainerRouter(&config.Config{}, false, ":80", nil)
	router.handler = middleware.NewChain(counter).Apply(router.serveMatched)
	router.publish(containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{
		containerRoute("dynamic.example", "/", "", backend),
	}})
	static := middleware.NewChain(counter).Apply(func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("static") })
	handler := router.wrap(static)

	handler(newContainerRequest("dynamic.example", "/dynamic"))
	if counter.count != 1 {
		t.Fatalf("动态请求中间件执行次数 = %d，期望 1", counter.count)
	}
	handler(newContainerRequest("static.example", "/static"))
	if counter.count != 2 {
		t.Fatalf("静态回退中间件累计执行次数 = %d，期望 2", counter.count)
	}
}

// TestPublishContainerSnapshotUnchangedKeepsRouteTable 验证服务器发布相同快照时不重建代理。
func TestPublishContainerSnapshotUnchangedKeepsRouteTable(t *testing.T) {
	backend := startContainerBackend(t)
	srv := New(&config.Config{})
	router := newContainerRouter(srv.config, false, ":80", nil)
	srv.containerRouters = []*containerRouter{router}
	snapshot := containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{
		containerRoute("dynamic.example", "/", "", backend),
	}}

	srv.publishContainerSnapshot(snapshot)
	first := router.table.Load()
	srv.publishContainerSnapshot(snapshot)
	if router.table.Load() != first {
		t.Fatal("服务器不应向路由器重复发布相同快照")
	}
	t.Cleanup(func() { router.table.Load().close() })
}

// TestContainerRouterUnchangedSnapshotKeepsRouteTable 验证周期同步相同内容时不重建代理。
func TestContainerRouterUnchangedSnapshotKeepsRouteTable(t *testing.T) {
	backend := startContainerBackend(t)
	router := newContainerRouter(&config.Config{}, false, ":80", nil)
	snapshot := containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{
		containerRoute("dynamic.example", "/", "", backend),
	}}
	router.publish(snapshot)
	first := router.table.Load()
	router.publish(snapshot)
	if router.table.Load() != first {
		t.Fatal("内容不变的快照不应替换路由表")
	}
}

// TestContainerRouterRedirectAndProtocolMethods 验证 HTTP/HTTPS 按 HTTPS_METHOD 分流并给挑战请求保留静态处理机会。
func TestContainerRouterRedirectAndProtocolMethods(t *testing.T) {
	backend := startContainerBackend(t)
	routes := []containerdiscovery.Route{
		containerRoute("redirect.example", "/", "/", backend),
		containerRoute("plain.example", "/", "/", backend),
		containerRoute("http-only.example", "/", "/", backend),
		containerRoute("https-only.example", "/", "/", backend),
	}
	routes[0].HTTPSMethod = "redirect"
	routes[0].ACMEHosts = []string{"redirect.example"}
	routes[1].HTTPSMethod = "noredirect"
	routes[2].HTTPSMethod = "nohttps"
	routes[3].HTTPSMethod = "nohttp"

	discoveryConfig := &config.Config{ContainerDiscovery: config.ContainerDiscoveryConfig{HTTPSServer: "https"}}
	httpRouter := newContainerRouter(discoveryConfig, false, ":80", nil)
	httpsRouter := newContainerRouter(discoveryConfig, true, ":443", nil)
	httpRouter.publish(containerdiscovery.Snapshot{Routes: routes})
	httpsRouter.publish(containerdiscovery.Snapshot{Routes: routes})

	redirect := newContainerRequest("redirect.example", "/hello?x=1")
	if !httpRouter.serve(redirect) || redirect.Response.StatusCode() != fasthttp.StatusMovedPermanently || string(redirect.Response.Header.Peek("Location")) != "https://redirect.example/hello?x=1" {
		t.Fatalf("HTTP 重定向错误：status=%d location=%q", redirect.Response.StatusCode(), redirect.Response.Header.Peek("Location"))
	}
	if httpRouter.serve(newContainerRequest("redirect.example", acmeChallengePath+"token")) {
		t.Fatal("ACME challenge 不应被动态重定向截获")
	}
	withoutCertificate := newContainerRequest("plain.example", "/without-certificate")
	routes[1].HTTPSMethod = "redirect"
	httpRouter.publish(containerdiscovery.Snapshot{Routes: routes})
	if !httpRouter.serve(withoutCertificate) || withoutCertificate.Response.StatusCode() == fasthttp.StatusMovedPermanently {
		t.Fatal("未声明 ACME_HOST 的容器不应重定向到不可用的 HTTPS")
	}
	routes[1].HTTPSMethod = "noredirect"
	httpRouter.publish(containerdiscovery.Snapshot{Routes: routes})
	if !httpRouter.serve(newContainerRequest("plain.example", "/")) || !httpRouter.serve(newContainerRequest("http-only.example", "/")) {
		t.Fatal("noredirect/nohttps 应在 HTTP 代理")
	}
	noHTTP := newContainerRequest("https-only.example", "/")
	if !httpRouter.serve(noHTTP) || noHTTP.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Fatal("nohttp 应拒绝 HTTP 访问")
	}
	noHTTPS := newContainerRequest("http-only.example", "/")
	if !httpsRouter.serve(noHTTPS) || noHTTPS.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Fatal("nohttps 应拒绝 HTTPS 访问")
	}
	if !httpsRouter.serve(newContainerRequest("redirect.example", "/")) || !httpsRouter.serve(newContainerRequest("https-only.example", "/")) {
		t.Fatal("除 nohttps 外均应在 HTTPS 代理")
	}
}

// TestApplyContainerDiscoveryACME 验证全局非空字段在管理器创建前覆盖 HTTPS 模板。
func TestApplyContainerDiscoveryACME(t *testing.T) {
	cfg := &config.Config{
		ContainerDiscovery: config.ContainerDiscoveryConfig{
			Enabled: true, HTTPSServer: "https-template",
			ACME: config.ContainerDiscoveryACMEConfig{Email: "global@example", Directory: "https://ca.example/directory", StatePath: t.TempDir(), Challenge: config.ACMEChallengeHTTP01},
		},
		Servers: []config.ServerConfig{{Name: "https-template", SSL: config.SSLConfig{ACME: config.ACMEConfig{Enabled: true, Email: "template@example"}}}},
	}
	srv := New(cfg)
	srv.applyContainerDiscoveryACME()
	got := cfg.Servers[0].SSL.ACME
	if !got.AllowDynamicHosts || got.Email != "global@example" || got.Directory != "https://ca.example/directory" || got.StatePath != cfg.ContainerDiscovery.ACME.StatePath || got.Challenge != config.ACMEChallengeHTTP01 {
		t.Fatalf("全局 ACME 未完整应用到模板: %+v", got)
	}

	cfg.ContainerDiscovery.ACME.Email = ""
	cfg.Servers[0].SSL.ACME.Email = ""
	srv.containerSnapshot = containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{{ACMEEmail: "container@example"}}}
	srv.applyContainerDiscoveryACME()
	if cfg.Servers[0].SSL.ACME.Email != "container@example" {
		t.Fatalf("未采用首次发现的容器邮箱: %q", cfg.Servers[0].SSL.ACME.Email)
	}
}

// TestContainerDiscoveryRequiredInitialFailure 验证 required 模式首次连接失败会阻止启动初始化。
func TestContainerDiscoveryRequiredInitialFailure(t *testing.T) {
	cfg := &config.Config{ContainerDiscovery: config.ContainerDiscoveryConfig{
		Enabled: true, Required: true, Endpoint: "unix://" + filepath.Join(t.TempDir(), "missing.sock"),
	}}
	server := New(cfg)
	if err := server.initContainerDiscovery(); err == nil {
		t.Fatal("required=true 首次同步失败应返回错误")
	}
}

// TestPublishContainerSnapshotUpdatesDynamicACMEAndSNI 验证发布快照同步更新模板签发与握手白名单。
func TestPublishContainerSnapshotUpdatesDynamicACMEAndSNI(t *testing.T) {
	cfg := &config.Config{
		ContainerDiscovery: config.ContainerDiscoveryConfig{Enabled: true, HTTPSServer: "https-template"},
		Servers: []config.ServerConfig{
			{Name: "default", Listen: ":443", Default: true, SSL: config.SSLConfig{RejectHandshake: true}},
			{Name: "https-template", Listen: ":443", SSL: config.SSLConfig{ACME: config.ACMEConfig{Enabled: true, AllowDynamicHosts: true, StatePath: t.TempDir()}}},
		},
	}
	srv := New(cfg)
	if err := srv.initACMEManagers(); err != nil {
		t.Fatal(err)
	}
	fastSrv, _, err := srv.buildListenGroupServer(listenGroup{listen: ":443", indices: []int{0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.cleanupResources)

	route := containerdiscovery.Route{Host: "route.example", ACMEHosts: []string{"route.example", "cert.example"}}
	srv.publishContainerSnapshot(containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{route}})
	if !srv.acmeManagers[1].HasHost("route.example") || srv.acmeManagers[1].HasHost("cert.example") {
		t.Fatal("仅与 VIRTUAL_HOST 匹配的 ACMEHosts 才应进入动态签发白名单")
	}
	if _, err := fastSrv.TLSConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "route.example"}); err != nil {
		t.Fatalf("动态 SNI 未选择模板: %v", err)
	}

	route.ACMEHosts = nil
	srv.publishContainerSnapshot(containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{route}})
	if srv.acmeManagers[1].HasHost("route.example") {
		t.Fatal("未声明 ACME_HOST 的路由不应自动申请证书")
	}
	if _, err := fastSrv.TLSConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "route.example"}); err == nil {
		t.Fatal("未声明 ACME_HOST 时应恢复默认拒绝策略")
	}
	srv.publishContainerSnapshot(containerdiscovery.Snapshot{})
}

// TestContainerHandlerKeepsStaticServerNamePriority 验证静态 server_name 命中时不会落入动态路由。
func TestContainerHandlerKeepsStaticServerNamePriority(t *testing.T) {
	backend := startContainerBackend(t)
	router := newContainerRouter(&config.Config{}, false, ":80", []string{"same.example"})
	router.publish(containerdiscovery.Snapshot{Routes: []containerdiscovery.Route{containerRoute("same.example", "/", "/", backend)}})
	static := func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("static") }
	handler := router.wrap(static)

	ctx := newContainerRequest("same.example", "/")
	handler(ctx)
	if got := string(ctx.Response.Body()); got != "static" {
		t.Fatalf("静态主机未优先：%q", got)
	}
}

// containerRoute 创建指向测试后端的动态路由。
func containerRoute(host, path, destination string, backend net.Addr) containerdiscovery.Route {
	address, portText, _ := net.SplitHostPort(backend.String())
	var port int
	_, _ = fmt.Sscanf(portText, "%d", &port)
	return containerdiscovery.Route{
		Host: host, Path: path, Destination: destination, Protocol: "http", HTTPSMethod: "noredirect",
		Endpoints: []containerdiscovery.Endpoint{{ContainerID: "test", Address: address, Port: port}},
	}
}

// newContainerRequest 创建供动态路由直接调用的请求上下文。
func newContainerRequest(host, uri string) *fasthttp.RequestCtx {
	ctx := new(fasthttp.RequestCtx)
	ctx.Request.Header.SetHost(host)
	ctx.Request.SetRequestURI(uri)
	return ctx
}

// startContainerBackend 启动回显请求 URI 的本机后端。
func startContainerBackend(t *testing.T) net.Addr {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBody(ctx.RequestURI()) }}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		_ = listener.Close()
	})
	return listener.Addr()
}
