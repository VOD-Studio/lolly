// Package server 提供 HTTP 服务器的核心实现，支持单服务器、虚拟主机和多服务器三种运行模式。
//
// 包含路由器相关的核心逻辑，用于管理 HTTP 请求的路由分发。
//
// 作者：xfy
package server

import (
	"strings"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/handler"
	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/lua"
	"rua.plus/lolly/internal/matcher"
	"rua.plus/lolly/internal/middleware"
	"rua.plus/lolly/internal/middleware/errorintercept"
	"rua.plus/lolly/internal/proxy"
)

// createProxyForConfig 创建代理实例并配置健康检查。
// 返回创建的代理实例，如果创建失败则返回 nil。
func (s *Server) createProxyForConfig(proxyCfg *config.ProxyConfig) *proxy.Proxy {
	// 转换目标
	targets := make([]*loadbalance.Target, len(proxyCfg.Targets))
	for j, t := range proxyCfg.Targets {
		failTimeout := t.FailTimeout
		if t.MaxFails > 0 && failTimeout == 0 {
			failTimeout = 10 * time.Second
		}
		targets[j] = loadbalance.NewTargetFromConfig(
			t.URL, t.Weight,
			int64(t.MaxConns), int64(t.MaxFails), failTimeout,
			t.Backup, t.Down, t.ProxyURI,
		)
	}

	// 传递 Transport 配置和 Lua 引擎
	p, err := proxy.NewProxy(proxyCfg, targets, &s.config.Performance.Transport, s.luaEngine)
	if err != nil {
		logging.Error().Msg("Failed to create proxy: " + err.Error())
		return nil
	}

	// 设置 DNS 解析器（如果已配置）
	if s.resolver != nil {
		p.SetResolver(s.resolver)
		if err := p.Start(); err != nil {
			logging.Error().Err(err).Msg("Failed to start proxy")
		}
	}

	// 启动健康检查
	if proxyCfg.HealthCheck.Interval > 0 {
		hc := proxy.NewHealthChecker(targets, &proxyCfg.HealthCheck)
		hc.Start()
		s.proxiesMu.Lock()
		s.healthCheckers = append(s.healthCheckers, hc)
		s.proxiesMu.Unlock()
		p.SetHealthChecker(hc)
	}

	s.proxiesMu.Lock()
	s.proxies = append(s.proxies, p)
	s.proxiesMu.Unlock()

	return p
}

// registerRoute 根据位置类型注册路由
func (s *Server) registerRoute(
	locType string,
	path string,
	handler fasthttp.RequestHandler,
	internal bool,
	source string,
) error {
	var err error
	switch locType {
	case matcher.LocationTypeExact:
		err = s.locationEngine.AddExact(path, handler, internal)
	case matcher.LocationTypePrefixPriority:
		err = s.locationEngine.AddPrefixPriority(path, handler, internal)
	case matcher.LocationTypeRegex:
		err = s.locationEngine.AddRegex(path, handler, false, internal)
	case matcher.LocationTypeRegexCaseless:
		err = s.locationEngine.AddRegex(path, handler, true, internal)
	case matcher.LocationTypeNamed:
		err = s.locationEngine.AddNamed(path, handler)
	default:
		err = s.locationEngine.AddPrefix(path, handler, internal)
	}
	if err != nil {
		return s.handleRegistrationError(source, path, err)
	}
	return nil
}

// registerProxyRoutesWithLocationEngine 使用 LocationEngine 注册代理路由。
//
// 根据配置为 LocationEngine 注册代理路径，创建代理处理器和健康检查器。
// 支持通过 LocationType 配置不同的匹配方式。
func (s *Server) registerProxyRoutesWithLocationEngine(serverCfg *config.ServerConfig) error {
	for i := range serverCfg.Proxy {
		proxyCfg := &serverCfg.Proxy[i]

		p := s.createProxyForConfig(proxyCfg)
		if p == nil {
			continue
		}

		locType := proxyCfg.LocationType
		if locType == "" {
			locType = matcher.LocationTypePrefix
		}

		path := proxyCfg.Path
		if locType == matcher.LocationTypeNamed && proxyCfg.LocationName != "" {
			path = "@" + proxyCfg.LocationName
		}

		if err := s.registerRoute(locType, path, p.ServeHTTP, proxyCfg.Internal, "proxy"); err != nil {
			return err
		}
	}
	return nil
}

// configureStaticHandler 配置静态文件处理器。
// 返回配置好的 StaticHandler，由调用者执行路由注册。
func (s *Server) configureStaticHandler(static *config.StaticConfig, cfg *config.ServerConfig) *handler.StaticHandler {
	path := static.Path
	if path == "" {
		path = "/"
	}

	staticHandler := handler.NewStaticHandler(
		static.Root,
		path,
		static.Index,
		true, // useSendfile
	)
	// 设置 alias（与 root 互斥）
	if static.Alias != "" {
		staticHandler.SetAlias(static.Alias)
	}
	if s.fileCache != nil {
		staticHandler.SetFileCache(s.fileCache)
	}
	// 始终启用 fileInfoCache 以减少 os.Stat 调用
	// 默认 TTL 2s，在静态文件修改可见性和性能间取得平衡
	staticHandler.SetCacheTTL(2 * time.Second)
	if cfg.Compression.GzipStatic {
		// extensions: 源文件类型，为空使用默认值
		// GzipStaticExtensions: 预压缩文件扩展名（如 .br, .gz）
		staticHandler.SetGzipStatic(true, nil, cfg.Compression.GzipStaticExtensions)
	}

	// 设置符号链接安全检查
	staticHandler.SetSymlinkCheck(static.SymlinkCheck)

	// 设置 internal 限制
	staticHandler.SetInternal(static.Internal)

	// 设置缓存过期时间
	if static.Expires != "" {
		staticHandler.SetExpires(static.Expires)
	}

	// 设置目录列表
	if static.AutoIndex {
		staticHandler.SetAutoIndex(
			static.AutoIndex,
			static.AutoIndexFormat,
			static.AutoIndexLocaltime,
			static.AutoIndexExactSize,
		)
	}

	return staticHandler
}

// registerStaticHandlersWithLocationEngine 使用 LocationEngine 注册静态文件处理器。
func (s *Server) registerStaticHandlersWithLocationEngine(cfg *config.ServerConfig) error {
	for _, static := range cfg.Static {
		staticHandler := s.configureStaticHandler(&static, cfg)
		path := static.Path
		if path == "" {
			path = "/"
		}

		locType := static.LocationType
		if locType == "" {
			locType = matcher.LocationTypePrefix
		}

		if err := s.registerRoute(locType, path, staticHandler.Handle, static.Internal, "static"); err != nil {
			return err
		}
	}
	return nil
}

// registerProxyRoutes 注册代理路由。
//
// 根据配置为路由器注册代理路径，创建代理处理器和健康检查器。
// 支持任意 HTTP 方法，并将客户端方法原样交给代理处理器。
//
// 参数：
//   - router: 路由器实例，用于注册路由规则
//   - serverCfg: 服务器配置，包含代理目标、负载均衡、健康检查等设置
//
// 注意事项：
//   - 代理目标初始状态默认为健康
//   - 健康检查根据配置自动启动
func (s *Server) registerProxyRoutes(router *handler.Router, serverCfg *config.ServerConfig) {
	for i := range serverCfg.Proxy {
		proxyCfg := &serverCfg.Proxy[i]

		p := s.createProxyForConfig(proxyCfg)
		if p == nil {
			continue
		}

		// 使用前缀匹配（通配符）注册代理路由
		// path: / 匹配所有子路径如 /sorry/index
		// path: /api/ 匹配 /api/* 所有子路径
		routePath := proxyCfg.Path
		// 确保通配符路由格式正确
		if !strings.HasSuffix(routePath, "/") && routePath != "/" {
			routePath += "/"
		}
		wildcardPath := routePath + "{path:*}"
		router.ANY(wildcardPath, p.ServeHTTP)
	}
}

// registerStaticHandlers 注册静态文件处理器。
//
// 为路由器注册静态文件服务，支持多个静态目录、文件缓存和预压缩文件。
//
// 参数：
//   - router: 路由器实例，用于注册路由规则
//   - cfg: 服务器配置，包含静态文件和压缩设置
func (s *Server) registerStaticHandlers(router *handler.Router, cfg *config.ServerConfig) {
	for _, static := range cfg.Static {
		staticHandler := s.configureStaticHandler(&static, cfg)
		path := static.Path
		if path == "" {
			path = "/"
		}

		// 设置 try_files 配置
		if len(static.TryFiles) > 0 {
			// 注意：tryFilesPass 需要路由器支持，当前实现传入 nil
			// 如果 tryFilesPass 为 true，需要额外处理
			staticHandler.SetTryFiles(static.TryFiles, static.TryFilesPass, router)
		}

		// 注册路由：确保路径以 / 结尾
		routePath := path
		if !strings.HasSuffix(routePath, "/") {
			routePath += "/"
		}
		router.GET(routePath+"{filepath:*}", staticHandler.Handle)
		router.HEAD(routePath+"{filepath:*}", staticHandler.Handle)
	}
}

// registerLuaRoutes 使用 Router 注册 Lua 路由。
//
// 遍历 Lua 配置中的脚本，为带有 Route 配置的脚本创建 LuaRouteHandler
// 并注册到 Router。
//
// 参数：
//   - router: 路由器实例
//   - serverCfg: 服务器配置
//
// 注意事项：
//   - 只有设置了 Route 字段的脚本才会被注册
//   - 支持 exact、prefix、regex 匹配类型（router 模式下统一使用路径匹配）
func (s *Server) registerLuaRoutes(router *handler.Router, serverCfg *config.ServerConfig) {
	if s.luaEngine == nil || serverCfg.Lua == nil || !serverCfg.Lua.Enabled {
		return
	}

	for _, script := range serverCfg.Lua.Scripts {
		if script.Route == "" {
			continue
		}

		timeout := script.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		luaHandler := lua.NewLuaRouteHandler(s.luaEngine, script.Path, timeout)
		handler := s.wrapRoutedHandler(luaHandler.ServeHTTP)

		// Router 模式下，根据 routeType 注册不同路由
		routePath := script.Route
		routeType := script.RouteType
		if routeType == "" {
			routeType = "prefix"
		}

		switch routeType {
		case "exact":
			// 精确匹配：只注册该路径
			router.GET(routePath, handler)
			router.POST(routePath, handler)
			router.PUT(routePath, handler)
			router.DELETE(routePath, handler)
			router.HEAD(routePath, handler)
		default:
			// 前缀匹配：注册带通配符的路径
			if !strings.HasSuffix(routePath, "/") && routePath != "/" {
				routePath += "/"
			}
			wildcardPath := routePath + "{path:*}"
			router.GET(wildcardPath, handler)
			router.POST(wildcardPath, handler)
			router.PUT(wildcardPath, handler)
			router.DELETE(wildcardPath, handler)
			router.HEAD(wildcardPath, handler)
		}
	}
}

// registerLuaRoutesWithLocationEngine 使用 LocationEngine 注册 Lua 路由。
//
// 遍历 Lua 配置中的脚本，为带有 Route 配置的脚本创建 LuaRouteHandler
// 并注册到 LocationEngine。
//
// 参数：
//   - serverCfg: 服务器配置，包含 Lua 脚本配置
//
// 注意事项：
//   - 只有设置了 Route 字段的脚本才会被注册
//   - 路由脚本不经过完整中间件链，只应用 accesslog 和 errorintercept
//   - 支持 exact、prefix、prefix_priority、regex、regex_caseless 匹配类型
func (s *Server) registerLuaRoutesWithLocationEngine(serverCfg *config.ServerConfig) error {
	if s.luaEngine == nil || serverCfg.Lua == nil || !serverCfg.Lua.Enabled {
		return nil
	}

	for _, script := range serverCfg.Lua.Scripts {
		if script.Route == "" {
			continue
		}

		timeout := script.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		luaHandler := lua.NewLuaRouteHandler(s.luaEngine, script.Path, timeout)
		handler := s.wrapRoutedHandler(luaHandler.ServeHTTP)

		routeType := script.RouteType
		if routeType == "" {
			routeType = matcher.LocationTypePrefix
		}

		if err := s.registerRoute(routeType, script.Route, handler, false, "lua"); err != nil {
			return err
		}
	}
	return nil
}

// wrapRoutedHandler 为路由处理器包装基础中间件链。
//
// 路由处理器（如 LuaRouteHandler）需要基础的访问日志和错误页面处理，
// 但不需要完整的中间件链（如认证、限流等）。
//
// 参数：
//   - handler: 原始请求处理器
//
// 返回值：
//   - fasthttp.RequestHandler: 包装后的处理器
func (s *Server) wrapRoutedHandler(handler fasthttp.RequestHandler) fasthttp.RequestHandler {
	var chain []middleware.Middleware

	if s.accessLogMiddleware != nil {
		chain = append(chain, s.accessLogMiddleware)
	}

	if s.errorPageManager != nil && s.errorPageManager.IsConfigured() {
		chain = append(chain, errorintercept.New(s.errorPageManager))
	}

	if len(chain) == 0 {
		return handler
	}
	return middleware.NewChain(chain...).Apply(handler)
}
