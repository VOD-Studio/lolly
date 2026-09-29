// Package server 提供 HTTP 服务器的核心实现，支持单服务器和虚拟主机两种运行模式。
//
// 该文件包含服务器相关的核心逻辑，包括：
//   - HTTP 服务器的创建和生命周期管理
//   - 中间件链的构建和应用
//   - 代理路由的注册和处理
//   - 静态文件服务的集成
//   - Goroutine 池的性能优化
//
// 主要用途：
//
//	用于启动和管理 HTTP 服务器，处理客户端请求并转发到上游服务或静态文件。
//
// 注意事项：
//   - 服务器支持优雅关闭和热升级
//   - 所有公开方法均为并发安全
//   - 使用前需确保配置已正确加载
//
// 作者：xfy
package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/cache"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/handler"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/lua"
	"rua.plus/lolly/internal/matcher"
	"rua.plus/lolly/internal/middleware/accesslog"
	"rua.plus/lolly/internal/middleware/security"
	"rua.plus/lolly/internal/mimeutil"
	"rua.plus/lolly/internal/proxy"
	"rua.plus/lolly/internal/resolver"
	"rua.plus/lolly/internal/ssl"
	"rua.plus/lolly/internal/version"
)

const networkTCP = "tcp"

// acmeChallengePath ACME http-01 挑战请求的路径前缀。
const acmeChallengePath = "/.well-known/acme-challenge/"

// Server HTTP 服务器，封装 fasthttp.Server 并提供中间件链和生命周期管理。
//
// 该结构体是服务器的核心实体，负责：
//   - 管理配置和 fasthttp.Server 实例
//   - 构建和应用中间件链
//   - 维护健康检查器和访问日志中间件
//   - 可选的 Goroutine 池和文件缓存
//
// 注意事项：
//   - 创建后需调用 Start 方法启动服务器
//   - 关闭时建议使用 GracefulStop 实现优雅关闭
type Server struct {
	handler             fasthttp.RequestHandler
	resolver            resolver.Resolver
	tlsManager          *ssl.TLSManager
	tlsManagers         []*ssl.TLSManager
	sniManagers         []*ssl.SNIManager
	acmeManagers        []*ssl.ACMEManager
	certMonitor         *ssl.CertMonitor
	tlsManagersMu       sync.Mutex
	accessLogMiddleware *accesslog.AccessLog
	luaEngine           *lua.LuaEngine
	accessControl       *security.AccessControl
	accessControls      []*security.AccessControl
	accessControlsMu    sync.Mutex
	rateLimiters        []*security.RateLimiter
	rateLimitersMu      sync.Mutex
	errorPageManager    *handler.ErrorPageManager
	fileCache           *cache.FileCache
	pool                *GoroutinePool
	upgradeManager      *UpgradeManager
	config              *config.Config
	fastServer          *fasthttp.Server
	fastServers         []*fasthttp.Server // 多监听器模式使用
	proxies             []*proxy.Proxy
	proxiesMu           sync.RWMutex
	providedListeners   []net.Listener
	listeners           []net.Listener
	healthCheckers      []*proxy.HealthChecker
	locationEngine      *matcher.LocationEngine
	startTime           time.Time
	connections         atomic.Int64
	requests            atomic.Int64
	bytesSent           atomic.Int64
	bytesReceived       atomic.Int64
	running             atomic.Bool
	cleanupOnce         sync.Once
}

// New 创建 HTTP 服务器实例。
//
// 根据提供的配置创建服务器对象，但不启动服务器。
// 服务器创建后需调用 Start 方法才能开始处理请求。
//
// 参数：
//   - cfg: 服务器配置对象，包含监听地址、代理、静态文件、安全等配置
//
// 返回值：
//   - *Server: 创建的服务器实例
func New(cfg *config.Config) *Server {
	s := &Server{config: cfg}
	if cfg != nil {
		s.accessLogMiddleware = accesslog.New(&cfg.Logging)
	}
	return s
}

// Running reports whether the server is currently running.
func (s *Server) Running() bool {
	return s.running.Load()
}

// trackRateLimiter 记录创建的令牌桶限流器，便于后续统一释放后台清理 goroutine。
func (s *Server) trackRateLimiter(rl *security.RateLimiter) {
	s.rateLimitersMu.Lock()
	defer s.rateLimitersMu.Unlock()
	s.rateLimiters = append(s.rateLimiters, rl)
}

// stopRateLimiters 停止所有已记录的令牌桶限流器。
func (s *Server) stopRateLimiters() {
	s.rateLimitersMu.Lock()
	defer s.rateLimitersMu.Unlock()
	for _, rl := range s.rateLimiters {
		rl.StopCleanup()
	}
	s.rateLimiters = s.rateLimiters[:0]
}

func (s *Server) handleRegistrationError(source, path string, err error) error {
	var ce *matcher.ConflictError
	if errors.As(err, &ce) {
		logging.Warn().Msgf("Route registration skipped (%s %s): %s", source, path, err)
		return nil
	}
	return fmt.Errorf("%s route %s: %w", source, path, err)
}

// getServerName 根据配置返回服务器名称。
//
// 当 ServerTokens 为 false 时隐藏版本号，仅返回 "lolly"。
// 默认（ServerTokens 为 true 或零值）返回完整版本信息。
//
// 参数：
//   - cfg: 服务器配置对象
//
// 返回值：
//   - string: 服务器名称
func (s *Server) getServerName(cfg *config.ServerConfig) string {
	if cfg != nil && !cfg.ServerTokens {
		return "lolly"
	}
	return "lolly/" + version.Version
}

// createFastServer 创建 fasthttp.Server 实例。
//
// 根据配置创建并配置 fasthttp.Server，包含所有通用设置。
//
// 参数：
//   - serverCfg: 服务器配置对象
//   - handler: 请求处理器
//
// 返回值：
//   - *fasthttp.Server: 配置好的 fasthttp.Server 实例
func (s *Server) createFastServer(serverCfg *config.ServerConfig, handler fasthttp.RequestHandler) *fasthttp.Server {
	return &fasthttp.Server{
		Name:               s.getServerName(serverCfg),
		Handler:            handler,
		ReadTimeout:        serverCfg.ReadTimeout,
		WriteTimeout:       serverCfg.WriteTimeout,
		IdleTimeout:        serverCfg.IdleTimeout,
		MaxConnsPerIP:      serverCfg.MaxConnsPerIP,
		MaxRequestsPerConn: serverCfg.MaxRequestsPerConn,
		CloseOnShutdown:    true,
		Concurrency:        serverCfg.Concurrency,
		ReadBufferSize:     serverCfg.ReadBufferSize,
		WriteBufferSize:    serverCfg.WriteBufferSize,
		ReduceMemoryUsage:  serverCfg.ReduceMemoryUsage,
	}
}

// applyTypesConfig 应用 MIME 类型配置。
//
// 根据配置设置自定义 MIME 类型映射和默认类型。
//
// 参数：
//   - cfg: 服务器配置对象
func (s *Server) applyTypesConfig(cfg *config.ServerConfig) {
	if cfg == nil {
		return
	}
	if len(cfg.Types.Map) > 0 {
		mimeutil.AddTypes(cfg.Types.Map)
	}
	if cfg.Types.DefaultType != "" {
		mimeutil.SetDefaultType(cfg.Types.DefaultType)
	}
}

// trackStats 包装处理器以统计请求数和数据传输量。
//
// 在每个请求处理前后更新统计字段：requests、bytesSent、bytesReceived。
//
// 参数：
//   - handler: 原始的请求处理器
//
// 返回值：
//   - fasthttp.RequestHandler: 包装后的处理器
func (s *Server) trackStats(handler fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		s.requests.Add(1)
		s.bytesReceived.Add(int64(len(ctx.Request.Body())))
		handler(ctx)
		s.bytesSent.Add(int64(len(ctx.Response.Body())))
	}
}

// GetListeners 获取服务器监听器列表。
//
// 返回当前服务器使用的监听器，用于热升级时传递给子进程。
//
// 返回值：
//   - []net.Listener: 监听器列表
func (s *Server) GetListeners() []net.Listener {
	return s.listeners
}

// SetListeners 设置服务器监听器列表。
//
// 用于热升级时，子进程从父进程继承监听器。
//
// 参数：
//   - listeners: 要设置的监听器列表
func (s *Server) SetListeners(listeners []net.Listener) {
	s.providedListeners = listeners
}

// SetUpgradeManager 设置升级管理器。
//
// 用于从外部（App 层）注入升级管理器，使服务器能够在
// createListener 中检查热升级状态和继承的监听器。
//
// 参数：
//   - mgr: 升级管理器实例
func (s *Server) SetUpgradeManager(mgr *UpgradeManager) {
	s.upgradeManager = mgr
}

// GetTLSConfig 获取 TLS 配置。
//
// 返回服务器的 TLS 配置，用于 HTTP/3 等需要 TLS 的协议。
//
// 返回值：
//   - *tls.Config: TLS 配置对象
//   - error: 未配置 TLS 或配置无效时返回错误
func (s *Server) GetTLSConfig() (*tls.Config, error) {
	if s.tlsManager == nil {
		return nil, fmt.Errorf("TLS not configured")
	}
	return s.tlsManager.GetTLSConfig(), nil
}

// GetHandler 获取请求处理器。
//
// 返回服务器的请求处理器，用于 HTTP/3 等需要复用处理器的场景。
//
// 返回值：
//   - fasthttp.RequestHandler: 请求处理器
func (s *Server) GetHandler() fasthttp.RequestHandler {
	return s.handler
}

// Start 启动 HTTP 服务器。
//
// 初始化日志系统、性能优化组件（Goroutine池、文件缓存），
// 根据配置选择单服务器模式或虚拟主机模式启动。
//
// 返回值：
//   - error: 启动过程中遇到的错误，如监听地址绑定失败
//
// 注意事项：
//   - 该方法会阻塞运行，直到服务器停止
//   - 调用前需确保配置已正确加载
//   - Goroutine池和文件缓存根据配置自动启用
func (s *Server) Start() error {
	err := s.start()
	if err != nil {
		s.cleanupResources()
	}
	return err
}

// start 执行服务器初始化与阻塞服务，失败清理由 Start 统一处理。
//
// 返回值：
//   - error: 初始化、绑定或服务阶段发生的错误
func (s *Server) start() error {
	if s.config == nil {
		return fmt.Errorf("server config is nil")
	}
	if len(s.config.Servers) == 0 {
		return fmt.Errorf("no servers configured")
	}
	logging.Init(s.config.Logging.Error.Level, s.config.Logging.Format)

	// 记录启动时间
	s.startTime = time.Now()

	// 初始化 ACME 自动证书管理器（须在注册路由与创建 TLS 之前）
	if initErr := s.initACMEManagers(); initErr != nil {
		return initErr
	}

	// 启动 ACME 证书到期监控
	s.startCertMonitor()

	// 初始化 GoroutinePool
	s.pool = initGoroutinePool(&s.config.Performance)

	// 初始化文件缓存
	s.fileCache = initFileCache(&s.config.Performance)

	// 初始化错误页面管理器
	if len(s.config.Servers) > 0 {
		var err error
		s.errorPageManager, err = initErrorPageManager(&s.config.Servers[0].Security.ErrorPage)
		if err != nil {
			return err
		}

		// 初始化 Lua 引擎
		s.luaEngine, err = initLuaEngine(s.config.Servers[0].Lua)
		if err != nil {
			return err
		}
	}

	// 单服务器保留 LocationEngine 路径，多条配置统一按 listen 分组。
	if len(s.config.Servers) == 1 {
		return s.startSingleMode()
	}
	return s.startMultiServerMode()
}

// createListener 根据配置创建监听器。
//
// 支持两种监听器格式：
//   - "unix:/path/to/socket" -> Unix domain socket
//   - ":8080" / "127.0.0.1:8080" -> TCP
//
// Unix socket 模式下会自动处理：
//   - 热升级时继承的监听器复用
//   - 旧 socket 文件清理
//   - socket 文件权限设置
//
// 参数：
//   - cfg: 服务器配置
//
// 返回值：
//   - net.Listener: 创建的监听器
//   - error: 创建失败时返回错误
func (s *Server) createListener(cfg *config.ServerConfig) (net.Listener, error) {
	listenAddr := cfg.Listen

	if s.upgradeManager != nil && s.upgradeManager.IsChild() {
		inherited, _ := s.upgradeManager.GetInheritedListeners()
		if ln := s.matchInheritedListener(inherited, listenAddr); ln != nil {
			return ln, nil
		}
	}

	if len(s.providedListeners) > 0 {
		if ln := s.matchInheritedListener(s.providedListeners, listenAddr); ln != nil {
			return ln, nil
		}
	}

	if strings.HasPrefix(listenAddr, "unix:") {
		socketPath := listenAddr[5:]

		if _, err := os.Stat(socketPath); err == nil {
			_ = os.Remove(socketPath)
		}

		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			return nil, fmt.Errorf("create unix socket failed: %w", err)
		}

		mode := 0o666
		if cfg.UnixSocket.Mode > 0 {
			mode = cfg.UnixSocket.Mode
		}
		if err := os.Chmod(socketPath, os.FileMode(mode)); err != nil {
			logging.Warn().Err(err).Msg("Failed to set socket file permissions")
		}

		if cfg.UnixSocket.User != "" || cfg.UnixSocket.Group != "" {
			logging.Warn().Msg("Unix socket user/group config requires root privileges, skipped")
		}

		return listener, nil
	}

	return net.Listen(networkTCP, listenAddr)
}

func (s *Server) matchInheritedListener(inherited []net.Listener, listenAddr string) net.Listener {
	if len(inherited) == 0 {
		return nil
	}

	if strings.HasPrefix(listenAddr, "unix:") {
		socketPath := listenAddr[5:]
		for _, ln := range inherited {
			if ln == nil {
				continue
			}
			if ln.Addr().Network() == "unix" && ln.Addr().String() == socketPath {
				return ln
			}
		}
		return nil
	}

	for _, ln := range inherited {
		if ln == nil {
			continue
		}
		if ln.Addr().Network() != networkTCP {
			continue
		}
		if s.tcpAddrMatch(ln.Addr().String(), listenAddr) {
			return ln
		}
	}
	return nil
}

func (s *Server) tcpAddrMatch(inherited, target string) bool {
	if inherited == target {
		return true
	}
	host1, port1, err1 := net.SplitHostPort(inherited)
	host2, port2, err2 := net.SplitHostPort(target)
	if err1 != nil || err2 != nil {
		return false
	}
	if port1 != port2 {
		return false
	}
	return host1 == host2 || isAnyAddr(host1) || isAnyAddr(host2)
}

func isAnyAddr(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// DupListener 复制 listener 的文件描述符，返回独立的 listener。
//
// 用于热重载场景：新旧 server 各自持有独立 FD，互不影响关闭操作。
func DupListener(ln net.Listener) (net.Listener, error) {
	switch l := ln.(type) {
	case *net.TCPListener:
		file, err := l.File()
		if err != nil {
			return nil, fmt.Errorf("dup tcp listener: %w", err)
		}
		defer func() { _ = file.Close() }()
		return net.FileListener(file)
	case *net.UnixListener:
		file, err := l.File()
		if err != nil {
			return nil, fmt.Errorf("dup unix listener: %w", err)
		}
		defer func() { _ = file.Close() }()
		return net.FileListener(file)
	default:
		return nil, fmt.Errorf("unsupported listener type: %T", ln)
	}
}

// startSingleMode 单服务器模式启动。
//
// 在单服务器模式下，创建单一路由器，注册代理路由和静态文件服务，
// 应用中间件链后启动 fasthttp 服务器。
//
// 返回值：
//   - error: 启动过程中遇到的错误
//
// 注意事项：
//   - 静态文件服务作为 fallback 处理非代理路径的请求
//   - 使用零拷贝传输优化大文件传输
func (s *Server) startSingleMode() error {
	if len(s.config.Servers) == 0 {
		return fmt.Errorf("no servers configured")
	}

	// 使用 Servers[0] 配置（迁移后 Server 字段为空）
	serverCfg := &s.config.Servers[0]

	// 应用 MIME 类型配置
	s.applyTypesConfig(serverCfg)

	// 创建 LocationEngine
	s.locationEngine = matcher.NewLocationEngine()

	// 注册状态监控端点（如果配置）
	if s.config.Monitoring.Status.Enabled {
		statusHandler, err := NewStatusHandler(s, &s.config.Monitoring.Status)
		if err != nil {
			logging.Error().Msg("Failed to create status handler: " + err.Error())
		} else {
			if regErr := s.locationEngine.AddExact(statusHandler.Path(), statusHandler.ServeHTTP, false); regErr != nil {
				if err := s.handleRegistrationError("status", statusHandler.Path(), regErr); err != nil {
					return err
				}
			}
		}
	}

	if s.config.Monitoring.Healthz.Enabled {
		hzPath := s.config.Monitoring.Healthz.Path
		if hzPath == "" {
			hzPath = "/healthz"
		}
		if regErr := s.locationEngine.AddExact(hzPath, HealthzHandler, false); regErr != nil {
			if err := s.handleRegistrationError("healthz", hzPath, regErr); err != nil {
				return err
			}
		}
	}

	if s.config.Monitoring.Readyz.Enabled {
		rzPath := s.config.Monitoring.Readyz.Path
		if rzPath == "" {
			rzPath = "/readyz"
		}
		readyzHandler := NewReadyzHandler(DefaultReadyzChecker(s))
		if regErr := s.locationEngine.AddExact(rzPath, readyzHandler, false); regErr != nil {
			if err := s.handleRegistrationError("readyz", rzPath, regErr); err != nil {
				return err
			}
		}
	}

	if s.config.Monitoring.Pprof.Enabled {
		pprofHandler, err := NewPprofHandler(&s.config.Monitoring.Pprof)
		if err != nil {
			logging.Error().Msg("Failed to create pprof handler: " + err.Error())
		} else {
			if regErr := s.locationEngine.AddExact(pprofHandler.Path(), pprofHandler.ServeHTTP, false); regErr != nil {
				if err := s.handleRegistrationError("pprof", pprofHandler.Path(), regErr); err != nil {
					return err
				}
			}
			if regErr := s.locationEngine.AddPrefixPriority(pprofHandler.Path()+"/", pprofHandler.ServeHTTP, false); regErr != nil {
				if err := s.handleRegistrationError("pprof", pprofHandler.Path()+"/", regErr); err != nil {
					return err
				}
			}
		}
	}

	if serverCfg.CacheAPI != nil && serverCfg.CacheAPI.Enabled {
		purgeHandler, err := NewPurgeHandler(s, serverCfg.CacheAPI)
		if err != nil {
			logging.Error().Msg("Failed to create cache purge handler: " + err.Error())
		} else {
			if regErr := s.locationEngine.AddExact(purgeHandler.Path(), purgeHandler.ServeHTTP, false); regErr != nil {
				if err := s.handleRegistrationError("cache-purge", purgeHandler.Path(), regErr); err != nil {
					return err
				}
			}
		}
	}

	// 注册 ACME http-01 挑战路由（tls-alpn-01 无需注册）
	if err := s.registerACMEChallengeLocation(s.locationEngine); err != nil {
		return err
	}

	if err := s.registerProxyRoutesWithLocationEngine(serverCfg); err != nil {
		return err
	}

	if err := s.registerLuaRoutesWithLocationEngine(serverCfg); err != nil {
		return err
	}

	if err := s.registerStaticHandlersWithLocationEngine(serverCfg); err != nil {
		return err
	}

	// 标记 LocationEngine 初始化完成
	s.locationEngine.MarkInitialized()

	// 创建主请求处理器，使用 LocationEngine 匹配路由
	locationEngine := s.locationEngine
	baseHandler := func(ctx *fasthttp.RequestCtx) {
		result := locationEngine.Match(ctx.Path())
		if result != nil && result.Handler != nil {
			result.Handler(ctx)
			matcher.ReleaseMatchResult(result)
			return
		}
		matcher.ReleaseMatchResult(result)
		ctx.SetStatusCode(404)
		ctx.SetBodyString("Not Found")
	}

	handler, err := s.wrapHandler(baseHandler, serverCfg)
	if err != nil {
		return err
	}
	s.handler = handler

	s.fastServer = s.createFastServer(serverCfg, s.handler)

	s.running.Store(true)

	return s.startServer(0, serverCfg, s.fastServer)
}

// startMultiServerMode 按监听地址分组启动多个服务器。
//
// 同一 listen 的配置共享一个监听器和 fasthttp.Server，并在组内按 Host
// 分流；不同 listen 的分组并行提供服务。
//
// 返回值：
//   - error: 任一监听分组的启动或服务错误
func (s *Server) startMultiServerMode() error {
	groups := groupServersByListen(s.config.Servers)
	s.fastServers = make([]*fasthttp.Server, 0, len(groups))
	s.listeners = make([]net.Listener, 0, len(groups))

	for _, group := range groups {
		fastSrv, err := s.buildListenGroupServer(group)
		if err != nil {
			s.closeActiveListeners()
			return err
		}
		ln, err := s.createListener(&s.config.Servers[group.indices[0]])
		if err != nil {
			s.closeActiveListeners()
			return fmt.Errorf("failed to listen on %s: %w", group.listen, err)
		}
		s.fastServers = append(s.fastServers, fastSrv)
		s.listeners = append(s.listeners, ln)
	}

	s.running.Store(true)
	serve := make([]func() error, len(s.fastServers))
	for i := range s.fastServers {
		fastSrv := s.fastServers[i]
		ln := s.listeners[i]
		serve[i] = func() error {
			if fastSrv.TLSConfig != nil {
				return fastSrv.ServeTLS(ln, "", "")
			}
			return fastSrv.Serve(ln)
		}
	}
	return serveListenGroups(serve, func() {
		for i := range s.fastServers {
			_ = s.fastServers[i].Shutdown()
		}
		s.closeActiveListeners()
	})
}

// serveListenGroups 并行运行监听分组并返回首个服务错误。
//
// 参数：
//   - serve: 每个监听分组的阻塞服务函数
//   - stop: 首次出现服务错误时停止其余分组
//
// 返回值：
//   - error: 所有非 nil 服务错误的聚合，全部正常结束时为 nil
func serveListenGroups(serve []func() error, stop func()) error {
	errCh := make(chan error, len(serve))
	var (
		wg       sync.WaitGroup
		stopOnce sync.Once
	)
	for i := range serve {
		wg.Add(1)
		go func(run func() error) {
			defer wg.Done()
			if err := run(); err != nil {
				errCh <- err
				stopOnce.Do(stop)
			}
		}(serve[i])
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// listenGroup 保存共享一个监听器的服务器配置。
type listenGroup struct {
	// listen 是该分组共用的原始监听地址。
	listen string
	// indices 保留服务器在 config.Servers 中的原始索引，供 ACME 映射使用。
	indices []int
}

// groupServersByListen 按首次出现顺序归并完全相同的监听地址。
func groupServersByListen(servers []config.ServerConfig) []listenGroup {
	positions := make(map[string]int)
	groups := make([]listenGroup, 0, len(servers))
	for i := range servers {
		listen := servers[i].Listen
		position, ok := positions[listen]
		if !ok {
			position = len(groups)
			positions[listen] = position
			groups = append(groups, listenGroup{listen: listen})
		}
		groups[position].indices = append(groups[position].indices, i)
	}
	return groups
}

// buildListenGroupServer 构建一个监听分组的 Host 路由和 TLS 配置。
func (s *Server) buildListenGroupServer(group listenGroup) (*fasthttp.Server, error) {
	vhosts := NewVHostManager()
	defaultIndex := group.indices[0]
	for _, idx := range group.indices {
		if s.config.Servers[idx].Default {
			defaultIndex = idx
			break
		}
	}

	for _, idx := range group.indices {
		serverCfg := &s.config.Servers[idx]
		router := handler.NewRouter()
		s.registerMonitoringEndpoints(router, serverCfg, idx == defaultIndex)
		s.registerProxyRoutes(router, serverCfg)
		s.registerLuaRoutes(router, serverCfg)
		s.registerStaticHandlers(router, serverCfg)
		s.registerACMEChallengeRouter(router)
		h, err := s.wrapHandler(router.Handler(), serverCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to build middleware chain (server[%d]): %w", idx, err)
		}
		for _, name := range serverCfg.EffectiveServerNames() {
			if err := vhosts.AddHost(name, h); err != nil {
				return nil, fmt.Errorf("add host %s: %w", name, err)
			}
		}
		if idx == defaultIndex {
			vhosts.SetDefault(h)
		}
	}

	representative := &s.config.Servers[group.indices[0]]
	fastSrv := s.createFastServer(representative, vhosts.Handler())
	if !representative.UsesTLS() {
		return fastSrv, nil
	}

	groupConfigs := make([]config.ServerConfig, len(group.indices))
	acmeManagers := make(map[int]*ssl.ACMEManager)
	defaultLocalIndex := 0
	for localIndex, originalIndex := range group.indices {
		groupConfigs[localIndex] = s.config.Servers[originalIndex]
		if originalIndex == defaultIndex {
			defaultLocalIndex = localIndex
		}
		if manager := s.acmeManagerAt(originalIndex); manager != nil {
			acmeManagers[localIndex] = manager
		}
	}
	sniManager, err := ssl.BuildSNIManager(groupConfigs, defaultLocalIndex, ssl.WithSNIACMEManagers(acmeManagers))
	if err != nil {
		return nil, fmt.Errorf("failed to build SNI manager for %s: %w", group.listen, err)
	}
	if sniManager != nil {
		fastSrv.TLSConfig = sniManager.TLSConfig()
		s.tlsManagersMu.Lock()
		s.sniManagers = append(s.sniManagers, sniManager)
		s.tlsManagersMu.Unlock()
	}
	return fastSrv, nil
}

// closeActiveListeners 关闭启动过程中已经激活的监听器。
func (s *Server) closeActiveListeners() {
	for _, ln := range s.listeners {
		if ln != nil {
			_ = ln.Close()
		}
	}
	s.listeners = nil
}

// registerMonitoringEndpoints 注册状态监控、性能分析和缓存清理端点。
func (s *Server) registerMonitoringEndpoints(router *handler.Router, serverCfg *config.ServerConfig, isDefault bool) {
	if isDefault && s.config.Monitoring.Status.Enabled {
		statusHandler, err := NewStatusHandler(s, &s.config.Monitoring.Status)
		if err != nil {
			logging.Error().Msg("Failed to create status handler: " + err.Error())
		} else {
			router.GET(statusHandler.Path(), statusHandler.ServeHTTP)
		}
	}

	if isDefault && s.config.Monitoring.Pprof.Enabled {
		pprofHandler, err := NewPprofHandler(&s.config.Monitoring.Pprof)
		if err != nil {
			logging.Error().Msg("Failed to create pprof handler: " + err.Error())
		} else {
			router.GET(pprofHandler.Path(), pprofHandler.ServeHTTP)
			router.GET(pprofHandler.Path()+"/{profile:*}", pprofHandler.ServeHTTP)
		}
	}

	if isDefault && serverCfg.CacheAPI != nil && serverCfg.CacheAPI.Enabled {
		purgeHandler, err := NewPurgeHandler(s, serverCfg.CacheAPI)
		if err != nil {
			logging.Error().Msg("Failed to create cache purge handler: " + err.Error())
		} else {
			router.POST(purgeHandler.Path(), purgeHandler.ServeHTTP)
		}
	}

	if isDefault {
		if s.config.Monitoring.Healthz.Enabled {
			hzPath := s.config.Monitoring.Healthz.Path
			if hzPath == "" {
				hzPath = "/healthz"
			}
			router.GET(hzPath, HealthzHandler)
		}

		if s.config.Monitoring.Readyz.Enabled {
			rzPath := s.config.Monitoring.Readyz.Path
			if rzPath == "" {
				rzPath = "/readyz"
			}
			readyzHandler := NewReadyzHandler(DefaultReadyzChecker(s))
			router.GET(rzPath, readyzHandler)
		}
	}
}

// wrapHandler 应用中间件链、连接池包装和统计追踪。
func (s *Server) wrapHandler(base fasthttp.RequestHandler, serverCfg *config.ServerConfig) (fasthttp.RequestHandler, error) {
	chain, err := s.buildMiddlewareChain(serverCfg)
	if err != nil {
		return nil, err
	}

	handler := chain.Apply(base)
	if s.pool != nil {
		handler = s.pool.WrapHandler(handler)
	}
	handler = s.trackStats(handler)
	return handler, nil
}

// startServer 创建监听器并启动 fasthttp.Server，支持可选 TLS。
//
// 如果 fastSrv.TLSConfig 已经设置（例如虚拟主机模式下由 SNIManager
// 预先配置的多证书 TLS 配置），则直接使用现有配置，不再基于
// serverCfg.SSL 创建新的单证书 TLSManager。
//
// 参数：
//   - idx: serverCfg 在 config.Servers 中的索引，用于取用对应的 ACME 管理器
//   - serverCfg: 服务器配置
//   - fastSrv: 待启动的 fasthttp.Server
//
// 返回值：
//   - error: 监听或 TLS 初始化失败时返回错误
func (s *Server) startServer(idx int, serverCfg *config.ServerConfig, fastSrv *fasthttp.Server) error {
	ln, err := s.createListener(serverCfg)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	s.listeners = append(s.listeners, ln)

	if fastSrv.TLSConfig == nil && serverCfg.UsesTLS() {
		tlsManager, err := ssl.NewTLSManager(&serverCfg.SSL, ssl.WithACMEManager(s.acmeManagerAt(idx)))
		if err != nil {
			return fmt.Errorf("failed to create TLS manager: %w", err)
		}
		fastSrv.TLSConfig = tlsManager.GetTLSConfig()

		s.tlsManagersMu.Lock()
		s.tlsManagers = append(s.tlsManagers, tlsManager)
		s.tlsManagersMu.Unlock()
	}

	if fastSrv.TLSConfig != nil {
		return fastSrv.ServeTLS(ln, "", "")
	}

	return fastSrv.Serve(ln)
}

// SetResolver 设置 DNS 解析器。
func (s *Server) SetResolver(r resolver.Resolver) {
	s.resolver = r
}
