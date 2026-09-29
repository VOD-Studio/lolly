// Package server 将容器发现快照接入 HTTP 服务器的稳定请求处理器。
//
// 动态路由通过原子替换不可变快照发布，请求处理中不持锁；静态 server_name 始终优先。
//
// 作者：xfy
package server

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
	containerdiscovery "rua.plus/lolly/internal/discovery/container"
	"rua.plus/lolly/internal/hostmatch"
	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/netutil"
	"rua.plus/lolly/internal/proxy"
	"rua.plus/lolly/internal/ssl"
)

// containerRouteHandler 保存一条已构建完成的动态代理路由。
type containerRouteHandler struct {
	handler fasthttp.RequestHandler
	path    string
	method  string
	secure  bool
}

// containerRouteContextKey 避免快照切换时同一请求前后读取到不同路由表。
type containerRouteContextKey struct{}

// builtContainerRoute 关联请求处理器与其代理生命周期。
type builtContainerRoute struct {
	handler fasthttp.RequestHandler
	proxy   *proxy.Proxy
}

// containerRouteTable 是一次发布后不再修改的主机路由表。
type containerRouteTable struct {
	hosts   map[string][]containerRouteHandler
	proxies []*proxy.Proxy
}

// close 关闭表中不再接收新请求的代理空闲连接和后台任务。
func (t *containerRouteTable) close() {
	if t == nil {
		return
	}
	for _, p := range t.proxies {
		p.Close()
	}
}

// containerTLSBinding 关联一个 HTTPS 模板的签发与 SNI 管理器。
type containerTLSBinding struct {
	acme *ssl.ACMEManager
	sni  *ssl.SNIManager
}

// containerRouter 为一个 HTTP 或 HTTPS 模板监听组提供稳定处理器。
type containerRouter struct {
	config      *config.Config
	staticHosts *hostmatch.Matcher[struct{}]
	table       atomic.Pointer[containerRouteTable]
	snapshot    containerdiscovery.Snapshot
	published   bool
	handler     fasthttp.RequestHandler
	listen      string
	tls         bool
}

// newContainerRouter 创建绑定模板监听组的动态路由器。
func newContainerRouter(cfg *config.Config, tlsEnabled bool, listen string, staticNames []string) *containerRouter {
	router := &containerRouter{config: cfg, staticHosts: hostmatch.New[struct{}](), listen: listen, tls: tlsEnabled}
	for _, name := range staticNames {
		_ = router.staticHosts.Add(name, struct{}{})
	}
	router.table.Store(&containerRouteTable{hosts: map[string][]containerRouteHandler{}})
	return router
}

// publish 构建完整的新表后一次替换，旧请求可继续安全使用旧表。
func (r *containerRouter) publish(snapshot containerdiscovery.Snapshot) {
	current := r.table.Load()
	if current != nil && r.published && reflect.DeepEqual(r.snapshot, snapshot) {
		return
	}
	table := &containerRouteTable{hosts: make(map[string][]containerRouteHandler)}
	for _, route := range snapshot.Routes {
		if !r.acceptPort(route) {
			continue
		}
		handler := r.buildRoute(route)
		if handler == nil {
			continue
		}
		table.proxies = append(table.proxies, handler.proxy)
		table.hosts[strings.ToLower(route.Host)] = append(table.hosts[strings.ToLower(route.Host)], containerRouteHandler{
			handler: handler.handler,
			path:    normalizedContainerPath(route.Path),
			method:  strings.ToLower(route.HTTPSMethod),
			secure:  r.config.ContainerDiscovery.HTTPSServer != "" && containsFold(route.ACMEHosts, route.Host),
		})
	}
	for host := range table.hosts {
		routes := table.hosts[host]
		for left := 0; left < len(routes); left++ {
			for right := left + 1; right < len(routes); right++ {
				if len(routes[right].path) > len(routes[left].path) {
					routes[left], routes[right] = routes[right], routes[left]
				}
			}
		}
		table.hosts[host] = routes
	}
	for _, issue := range snapshot.Issues {
		logging.Warn().Str("container", issue.ContainerID).Msg(issue.Message)
	}
	r.snapshot = snapshot
	r.published = true
	old := r.table.Swap(table)
	old.close()
}

// acceptPort 仅接收未声明外部端口或端口与模板监听端口一致的路由。
func (r *containerRouter) acceptPort(route containerdiscovery.Route) bool {
	port := route.ExternalHTTPPort
	if r.tls {
		port = route.ExternalHTTPSPort
	}
	if port == 0 || listenUsesPort(r.listen, strconv.Itoa(port)) {
		return true
	}
	logging.Warn().Str("host", route.Host).Int("external_port", port).Str("listen", r.listen).Msg("容器动态路由外部端口与模板监听端口不一致，已忽略")
	return false
}

// buildRoute 使用现有 Proxy 构造轮询后端，并在调用边界完成目标路径改写与恢复。
func (r *containerRouter) buildRoute(route containerdiscovery.Route) *builtContainerRoute {
	if len(route.Endpoints) == 0 {
		return nil
	}
	targets := make([]*loadbalance.Target, 0, len(route.Endpoints))
	for _, endpoint := range route.Endpoints {
		address := net.JoinHostPort(endpoint.Address, strconv.Itoa(endpoint.Port))
		targets = append(targets, loadbalance.NewTargetFromConfig(route.Protocol+"://"+address, 1, 0, 0, 0, false, false, ""))
	}
	proxyConfig := &config.ProxyConfig{Path: normalizedContainerPath(route.Path), LoadBalance: "round_robin"}
	p, err := proxy.NewProxy(proxyConfig, targets, &r.config.Performance.Transport, nil)
	if err != nil {
		logging.Warn().Err(err).Str("host", route.Host).Msg("创建容器动态代理失败")
		return nil
	}
	if route.Destination == "" {
		return &builtContainerRoute{handler: p.ServeHTTP, proxy: p}
	}
	path := normalizedContainerPath(route.Path)
	destination := normalizedContainerPath(route.Destination)
	handler := func(ctx *fasthttp.RequestCtx) {
		originalURI := append([]byte(nil), ctx.RequestURI()...)
		requestPath := string(ctx.Path())
		suffix := strings.TrimPrefix(requestPath, path)
		newPath := joinContainerDestination(destination, suffix)
		if query := ctx.QueryArgs().QueryString(); len(query) != 0 {
			newPath += "?" + string(query)
		}
		ctx.Request.SetRequestURI(newPath)
		defer ctx.Request.SetRequestURIBytes(originalURI)
		p.ServeHTTP(ctx)
	}
	return &builtContainerRoute{handler: handler, proxy: p}
}

// serve 尝试处理动态主机请求，返回是否已接管请求。
func (r *containerRouter) serve(ctx *fasthttp.RequestCtx) bool {
	route, ok := r.match(ctx)
	if !ok {
		return false
	}
	ctx.SetUserValue(containerRouteContextKey{}, route)
	defer ctx.RemoveUserValue(containerRouteContextKey{})
	if r.handler != nil {
		r.handler(ctx)
	} else {
		r.serveMatched(ctx)
	}
	return true
}

// match 返回当前快照中命中的动态路由，保证一次请求只选择一次快照。
func (r *containerRouter) match(ctx *fasthttp.RequestCtx) (containerRouteHandler, bool) {
	host := strings.ToLower(netutil.StripPort(string(ctx.Host())))
	if _, static := r.staticHosts.Find(host); static {
		return containerRouteHandler{}, false
	}
	path := string(ctx.Path())
	for _, route := range r.table.Load().hosts[host] {
		if !containerPathMatches(path, route.path) {
			continue
		}
		if r.tls || !route.secure || route.method != "redirect" && route.method != "" || !strings.HasPrefix(path, acmeChallengePath) {
			return route, true
		}
	}
	return containerRouteHandler{}, false
}

// serveMatched 处理已确认命中的动态请求。
func (r *containerRouter) serveMatched(ctx *fasthttp.RequestCtx) {
	route, ok := ctx.UserValue(containerRouteContextKey{}).(containerRouteHandler)
	if !ok {
		return
	}
	if r.tls {
		if route.method == "nohttps" {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString("Not Found")
			return
		}
		route.handler(ctx)
		return
	}
	switch route.method {
	case "nohttp":
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		ctx.SetBodyString("Not Found")
	case "noredirect", "nohttps":
		route.handler(ctx)
	default:
		if !route.secure {
			route.handler(ctx)
			return
		}
		ctx.Redirect("https://"+string(ctx.Host())+string(ctx.RequestURI()), fasthttp.StatusMovedPermanently)
	}
}

// wrap 在动态路由未接管时回退到原监听组处理器。
func (r *containerRouter) wrap(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		if !r.serve(ctx) {
			next(ctx)
		}
	}
}

// containsFold 判断字符串列表是否包含大小写不敏感的目标值。
func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

// containerPathMatches 按路径段边界执行动态前缀匹配。
func containerPathMatches(requestPath, routePath string) bool {
	if routePath == "/" || requestPath == routePath {
		return true
	}
	return strings.HasPrefix(requestPath, strings.TrimSuffix(routePath, "/")+"/")
}

// normalizedContainerPath 补全动态路径的开头斜杠。
func normalizedContainerPath(path string) string {
	if path == "" {
		return "/"
	}
	if path[0] != '/' {
		return "/" + path
	}
	return path
}

// joinContainerDestination 按代理路径替换语义连接目标前缀与剩余路径。
func joinContainerDestination(destination, suffix string) string {
	if destination == "/" {
		return "/" + strings.TrimPrefix(suffix, "/")
	}
	if suffix == "" {
		return destination
	}
	return strings.TrimSuffix(destination, "/") + "/" + strings.TrimPrefix(suffix, "/")
}

// applyContainerDiscoveryACME 在管理器创建前把全局账户参数应用到 HTTPS 模板。
//
// ACME 管理器的账户邮箱创建后不能在运行期切换；路由声明的不同邮箱仅告警。
func (s *Server) applyContainerDiscoveryACME() {
	cfg := &s.config.ContainerDiscovery
	if !cfg.Enabled || cfg.HTTPSServer == "" {
		return
	}
	for i := range s.config.Servers {
		srv := &s.config.Servers[i]
		if srv.Name != cfg.HTTPSServer {
			continue
		}
		global := cfg.ACME
		email := global.Email
		if email == "" && srv.SSL.ACME.Email == "" {
			email = firstContainerACMEEmail(s.containerSnapshot)
		}
		if email != "" {
			if srv.SSL.ACME.Email != "" && srv.SSL.ACME.Email != email {
				logging.Warn().Str("template_email", srv.SSL.ACME.Email).Str("discovery_email", email).Msg("容器发现 ACME 邮箱与 HTTPS 模板冲突，使用发现配置邮箱")
			}
			srv.SSL.ACME.Email = email
		}
		if global.Directory != "" {
			srv.SSL.ACME.Directory = global.Directory
		}
		if global.StatePath != "" {
			srv.SSL.ACME.StatePath = global.StatePath
		}
		if global.Challenge != "" {
			srv.SSL.ACME.Challenge = global.Challenge
		}
		srv.SSL.ACME.AllowDynamicHosts = true
		return
	}
}

// firstContainerACMEEmail 返回稳定排序快照中的首个容器证书邮箱。
func firstContainerACMEEmail(snapshot containerdiscovery.Snapshot) string {
	for _, route := range snapshot.Routes {
		if route.ACMEEmail != "" {
			return route.ACMEEmail
		}
	}
	return ""
}

// warnContainerRouteACMEEmails 提示路由邮箱不能替换已创建的 ACME 账户。
func (s *Server) warnContainerRouteACMEEmails(snapshot containerdiscovery.Snapshot) {
	account := s.containerACMEAccountEmail()
	for _, route := range snapshot.Routes {
		if route.ACMEEmail != "" && route.ACMEEmail != account {
			logging.Warn().Str("host", route.Host).Str("route_email", route.ACMEEmail).Str("account_email", account).Msg("路由 ACME 邮箱与已创建账户冲突，运行期不能切换账户邮箱")
		}
	}
}

// containerACMEAccountEmail 返回 HTTPS 模板实际采用的 ACME 账户邮箱。
func (s *Server) containerACMEAccountEmail() string {
	for i := range s.config.Servers {
		if s.config.Servers[i].Name == s.config.ContainerDiscovery.HTTPSServer {
			return s.config.Servers[i].SSL.ACME.Email
		}
	}
	return ""
}

// initContainerDiscovery 执行必需的首次同步并启动后台事件与周期同步。
func (s *Server) initContainerDiscovery() error {
	cfg := &s.config.ContainerDiscovery
	if !cfg.Enabled {
		return nil
	}
	watcher, err := containerdiscovery.New(containerdiscovery.Config{
		SocketPath:     strings.TrimPrefix(cfg.Endpoint, "unix://"),
		Network:        cfg.Network,
		ResyncInterval: cfg.ResyncInterval,
		RequestTimeout: cfg.RequestTimeout,
		Debounce:       cfg.Debounce,
	})
	if err != nil {
		return fmt.Errorf("创建容器发现观察器失败: %w", err)
	}
	snapshot, syncErr := watcher.Sync(context.Background())
	if syncErr != nil && cfg.Required {
		return fmt.Errorf("首次同步容器发现失败: %w", syncErr)
	}
	if syncErr != nil {
		logging.Warn().Err(syncErr).Msg("首次同步容器发现失败，将在后台重试")
	} else {
		s.publishContainerSnapshot(snapshot)
	}
	s.containerWatcher = watcher
	return nil
}

// startContainerDiscovery 在静态路由和 TLS 管理器完成构建后启动后台同步。
func (s *Server) startContainerDiscovery() {
	if s.containerWatcher == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.containerDiscoveryCancel = cancel
	watcher := s.containerWatcher
	s.containerDiscoveryWG.Add(1)
	go func() {
		defer s.containerDiscoveryWG.Done()
		if err := watcher.Start(ctx, s.publishContainerSnapshot); err != nil && ctx.Err() == nil {
			logging.Warn().Err(err).Msg("容器发现观察器已停止")
		}
	}()
}

// publishContainerSnapshot 将新快照同时发布到已安装的 HTTP 与 HTTPS 模板路由器。
func (s *Server) publishContainerSnapshot(snapshot containerdiscovery.Snapshot) {
	s.containerRoutersMu.Lock()
	defer s.containerRoutersMu.Unlock()
	if reflect.DeepEqual(s.containerSnapshot, snapshot) {
		return
	}
	s.containerSnapshot = snapshot
	s.warnContainerRouteACMEEmails(snapshot)
	for _, router := range s.containerRouters {
		router.publish(snapshot)
	}
	acmeHosts, sniHosts := containerTLSHosts(snapshot)
	for _, binding := range s.containerTLSBindings {
		if binding.acme != nil {
			binding.acme.SetDynamicHosts(acmeHosts)
		}
		if binding.sni != nil {
			binding.sni.SetDynamicHosts(sniHosts)
		}
	}
}

// containerTLSHosts 分别提取允许签发的 ACME 域名与可选择动态模板的 SNI 域名。
//
// 只有显式声明 LETSENCRYPT_HOST 或 ACME_HOST 的 HTTPS 路由才进入集合，
// 与 acme-companion 保持一致，避免仅凭 VIRTUAL_HOST 意外申请证书。
func containerTLSHosts(snapshot containerdiscovery.Snapshot) ([]string, []string) {
	var acmeHosts, sniHosts []string
	for _, route := range snapshot.Routes {
		if strings.EqualFold(route.HTTPSMethod, "nohttps") {
			continue
		}
		for _, host := range route.ACMEHosts {
			if strings.EqualFold(host, route.Host) {
				acmeHosts = append(acmeHosts, route.Host)
				sniHosts = append(sniHosts, route.Host)
				break
			}
		}
	}
	return acmeHosts, sniHosts
}

// containerRouterForGroup 返回当前监听组对应的动态模板路由器。
func (s *Server) containerRouterForGroup(group listenGroup) (*containerRouter, error) {
	cfg := &s.config.ContainerDiscovery
	if !cfg.Enabled {
		return nil, nil
	}
	for _, index := range group.indices {
		serverCfg := &s.config.Servers[index]
		if serverCfg.Name != cfg.HTTPServer && serverCfg.Name != cfg.HTTPSServer {
			continue
		}
		var names []string
		for _, staticIndex := range group.indices {
			names = append(names, s.config.Servers[staticIndex].EffectiveServerNames()...)
		}
		router := newContainerRouter(s.config, serverCfg.Name == cfg.HTTPSServer, group.listen, names)
		chain, err := s.buildMiddlewareChain(serverCfg)
		if err != nil {
			return nil, err
		}
		router.handler = chain.Apply(router.serveMatched)
		if s.pool != nil {
			router.handler = s.pool.WrapHandler(router.handler)
		}
		router.handler = s.trackStats(router.handler)
		s.containerRoutersMu.Lock()
		router.publish(s.containerSnapshot)
		s.containerRouters = append(s.containerRouters, router)
		s.containerRoutersMu.Unlock()
		return router, nil
	}
	return nil, nil
}
