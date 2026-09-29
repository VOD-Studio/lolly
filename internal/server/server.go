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
	"context"
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
	containerdiscovery "rua.plus/lolly/internal/discovery/container"
	"rua.plus/lolly/internal/handler"
	"rua.plus/lolly/internal/http2"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/lua"
	"rua.plus/lolly/internal/matcher"
	"rua.plus/lolly/internal/middleware/accesslog"
	"rua.plus/lolly/internal/middleware/bodylimit"
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
	handler                  fasthttp.RequestHandler
	resolver                 resolver.Resolver
	tlsManager               *ssl.TLSManager
	tlsManagers              []*ssl.TLSManager
	sniManagers              []*ssl.SNIManager
	acmeManagers             []*ssl.ACMEManager
	certMonitor              *ssl.CertMonitor
	tlsManagersMu            sync.Mutex
	accessLogMiddleware      *accesslog.AccessLog
	luaEngine                *lua.LuaEngine
	accessControl            *security.AccessControl
	accessControls           []*security.AccessControl
	accessControlsMu         sync.Mutex
	rateLimiters             []*security.RateLimiter
	rateLimitersMu           sync.Mutex
	errorPageManager         *handler.ErrorPageManager
	fileCache                *cache.FileCache
	pool                     *GoroutinePool
	upgradeManager           *UpgradeManager
	config                   *config.Config
	fastServer               *fasthttp.Server
	fastServers              []*fasthttp.Server // 多监听器模式使用
	proxies                  []*proxy.Proxy
	proxiesMu                sync.RWMutex
	providedListeners        []net.Listener
	listeners                []net.Listener
	listenersMu              sync.Mutex
	h2cServers               []*http2.Server
	h2cServersMu             sync.Mutex
	healthCheckers           []*proxy.HealthChecker
	locationEngine           *matcher.LocationEngine
	startTime                time.Time
	connections              atomic.Int64
	requests                 atomic.Int64
	bytesSent                atomic.Int64
	bytesReceived            atomic.Int64
	running                  atomic.Bool
	cleanupOnce              sync.Once
	containerDiscoveryCancel context.CancelFunc
	containerWatcher         *containerdiscovery.Watcher
	containerDiscoveryWG     sync.WaitGroup
	containerRoutersMu       sync.Mutex
	containerRouters         []*containerRouter
	containerTLSBindings     []containerTLSBinding
	containerSnapshot        containerdiscovery.Snapshot
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
// 当 streamRequestBody 为 true 时，开启请求体流式读取，
// 使 ctx.Request.BodyStream() 直接从连接读取，供代理层
// 实现请求体流式转发（buffering.request_mode: off）。
//
// 参数：
//   - serverCfg: 服务器配置对象
//   - handler: 请求处理器
//   - streamRequestBody: 是否开启请求体流式读取
//
// 返回值：
//   - *fasthttp.Server: 配置好的 fasthttp.Server 实例
func (s *Server) createFastServer(serverCfg *config.ServerConfig, handler fasthttp.RequestHandler, streamRequestBody bool) *fasthttp.Server {
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
		StreamRequestBody:  streamRequestBody,
	}
}

// anyProxyRequestStreaming 报告给定服务器配置中是否有任一代理启用了请求体流式。
//
// 用于决定共享监听器的 fasthttp.Server 是否开启 StreamRequestBody。
// 只要组内任一 location 的 proxy 配置了 buffering.request_mode: off，
// 整个监听器就启用流式读取（server 级开关无法按 location 切换）。
//
// 参数：
//   - servers: 同一监听分组的所有服务器配置
//
// 返回值：
//   - bool: true 表示至少有一个代理启用了请求体流式
func anyProxyRequestStreaming(servers ...*config.ServerConfig) bool {
	for _, sc := range servers {
		if sc == nil {
			continue
		}
		for i := range sc.Proxy {
			if sc.Proxy[i].Buffering.RequestStreamingEnabled() {
				return true
			}
		}
	}
	return false
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
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	return s.listeners
}

// appendListener 记录一个原始监听器。
//
// 只记录未包装的监听器：热升级与重载要用它继承 FD、DupListener 要按
// *net.TCPListener/*net.UnixListener 取描述符，而 h2c 嗅探包装仅作用于
// Serve 调用点。启动协程写入与 app 层读取并发，需加锁。
//
// 参数：
//   - ln: 要记录的原始监听器
func (s *Server) appendListener(ln net.Listener) {
	s.listenersMu.Lock()
	s.listeners = append(s.listeners, ln)
	s.listenersMu.Unlock()
}

// resetListeners 重置监听器列表并按分组数预置容量。
//
// 参数：
//   - n: 预计的监听器数量
func (s *Server) resetListeners(n int) {
	s.listenersMu.Lock()
	s.listeners = make([]net.Listener, 0, n)
	s.listenersMu.Unlock()
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

	// 首次发现必须先于 ACME 与 SNI 管理器构建，以提供初始动态白名单和账户邮箱。
	if err := s.initContainerDiscovery(); err != nil {
		return err
	}

	// 容器发现账户参数必须在 ACME 管理器创建前固定，运行期不切换账户邮箱。
	s.applyContainerDiscoveryACME()

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

	// 提示 h2c 相关的"配了但不生效"组合（校验层看不到监听器与虚拟主机的搭配）。
	s.warnHTTP2H2CConfig()

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
	dynamic, err := s.containerRouterForGroup(listenGroup{listen: serverCfg.Listen, indices: []int{0}})
	if err != nil {
		return err
	}
	if dynamic != nil {
		handler = dynamic.wrap(handler)
	}
	s.handler = handler

	s.fastServer = s.createFastServer(serverCfg, s.handler, anyProxyRequestStreaming(serverCfg))

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
	s.resetListeners(len(groups))
	serveListeners := make([]net.Listener, 0, len(groups))

	for _, group := range groups {
		fastSrv, groupHandler, err := s.buildListenGroupServer(group)
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
		// 记录原始监听器：热升级与重载按它继承 FD，h2c 嗅探包装只在 Serve 前生效。
		s.appendListener(ln)
		serveListeners = append(serveListeners, s.wrapGroupH2C(ln, group, fastSrv, groupHandler))
	}

	s.startContainerDiscovery()
	s.running.Store(true)
	serve := make([]func() error, len(s.fastServers))
	for i := range s.fastServers {
		fastSrv := s.fastServers[i]
		ln := serveListeners[i]
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
//
// 返回值中的 handler 是分组内按 Host 分流的组合处理器，供明文 h2c 嗅探
// 分派复用（h2c 连接上的请求同样要按 Host 落到对应虚拟主机）。
//
// 参数：
//   - group: 共享同一监听地址的虚拟主机分组
//
// 返回值：
//   - *fasthttp.Server: 该分组的 fasthttp.Server
//   - fasthttp.RequestHandler: 该分组的组合请求处理器
//   - error: 中间件链或 SNI 管理器构建失败时返回错误
func (s *Server) buildListenGroupServer(group listenGroup) (*fasthttp.Server, fasthttp.RequestHandler, error) {
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
			return nil, nil, fmt.Errorf("failed to build middleware chain (server[%d]): %w", idx, err)
		}
		for _, name := range serverCfg.EffectiveServerNames() {
			if err := vhosts.AddHost(name, h); err != nil {
				return nil, nil, fmt.Errorf("add host %s: %w", name, err)
			}
		}
		if idx == defaultIndex {
			vhosts.SetDefault(h)
		}
	}

	representative := &s.config.Servers[group.indices[0]]
	streamReqBody := false
	for _, idx := range group.indices {
		if anyProxyRequestStreaming(&s.config.Servers[idx]) {
			streamReqBody = true
			break
		}
	}
	groupHandler := vhosts.Handler()
	dynamic, err := s.containerRouterForGroup(group)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build container middleware chain: %w", err)
	}
	if dynamic != nil {
		groupHandler = dynamic.wrap(groupHandler)
	}
	fastSrv := s.createFastServer(representative, groupHandler, streamReqBody)
	if !representative.UsesTLS() {
		return fastSrv, groupHandler, nil
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
	sniOptions := []ssl.SNIOption{ssl.WithSNIACMEManagers(acmeManagers)}
	dynamicLocalIndex := -1
	if template := s.config.ContainerDiscovery.HTTPSServer; s.config.ContainerDiscovery.Enabled && template != "" {
		for localIndex, originalIndex := range group.indices {
			if s.config.Servers[originalIndex].Name == template {
				dynamicLocalIndex = localIndex
				sniOptions = append(sniOptions, ssl.WithDynamicSNIACME(&groupConfigs[localIndex].SSL, acmeManagers[localIndex]))
				break
			}
		}
	}
	sniManager, err := ssl.BuildSNIManager(groupConfigs, defaultLocalIndex, sniOptions...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build SNI manager for %s: %w", group.listen, err)
	}
	if sniManager != nil {
		fastSrv.TLSConfig = sniManager.TLSConfig()
		s.tlsManagersMu.Lock()
		s.sniManagers = append(s.sniManagers, sniManager)
		s.tlsManagersMu.Unlock()
		if dynamicLocalIndex >= 0 {
			s.containerRoutersMu.Lock()
			binding := containerTLSBinding{acme: acmeManagers[dynamicLocalIndex], sni: sniManager}
			_, hosts := containerTLSHosts(s.containerSnapshot)
			binding.sni.SetDynamicHosts(hosts)
			s.containerTLSBindings = append(s.containerTLSBindings, binding)
			s.containerRoutersMu.Unlock()
		}
	}

	// HTTP/2 是连接级协议：同一监听分组内任一虚拟主机启用 h2 即在
	// 该分组的 fasthttp.Server 上挂载 ALPN "h2" 分派。连接协商出 h2
	// 后，请求按 Host 头由 vhosts 分发，与 HTTP/1.1 路由一致。
	// ponytail: HTTP/2 服务器参数取分组内首个启用 h2 的虚拟主机配置；
	// 不同虚拟主机的 h2 参数差异在连接级不感知，需要时按 vhost 细分。
	for _, idx := range group.indices {
		if s.config.Servers[idx].SSL.HTTP2.Enabled {
			s.attachHTTP2(fastSrv, &s.config.Servers[idx], groupHandler)
			break
		}
	}

	return fastSrv, groupHandler, nil
}

// closeActiveListeners 关闭启动过程中已经激活的监听器。
func (s *Server) closeActiveListeners() {
	s.listenersMu.Lock()
	listeners := s.listeners
	s.listeners = nil
	s.listenersMu.Unlock()

	for _, ln := range listeners {
		if ln != nil {
			_ = ln.Close()
		}
	}
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
	s.appendListener(ln)

	if fastSrv.TLSConfig == nil && serverCfg.UsesTLS() {
		tlsManager, err := ssl.NewTLSManager(&serverCfg.SSL, ssl.WithACMEManager(s.acmeManagerAt(idx)))
		if err != nil {
			return fmt.Errorf("failed to create TLS manager: %w", err)
		}
		fastSrv.TLSConfig = tlsManager.GetTLSConfig()

		s.tlsManagersMu.Lock()
		s.tlsManagers = append(s.tlsManagers, tlsManager)
		s.tlsManagersMu.Unlock()
		if serverCfg.Name == s.config.ContainerDiscovery.HTTPSServer && serverCfg.SSL.ACME.AllowDynamicHosts {
			s.containerRoutersMu.Lock()
			s.containerTLSBindings = append(s.containerTLSBindings, containerTLSBinding{acme: s.acmeManagerAt(idx)})
			s.containerRoutersMu.Unlock()
		}
	}

	// HTTP/2 经 ALPN 分派挂载到 fasthttp.Server，复用其监听器与连接
	// 生命周期，避免与 fasthttp 抢占同一监听器的 Accept 循环。
	s.attachHTTP2(fastSrv, serverCfg, s.handler)

	// 单服务器的 TLS 管理器到此才完成构建，随后才能启动后台发现。
	s.startContainerDiscovery()

	if fastSrv.TLSConfig != nil {
		return fastSrv.ServeTLS(ln, "", "")
	}

	// 明文监听器：h2c 嗅探只包在 Serve 前，s.listeners 仍保留原始监听器，
	// 热升级与重载的 FD 继承按原始监听器类型取描述符。
	return fastSrv.Serve(s.wrapH2C(fastSrv, ln, serverCfg, s.handler))
}

// SetResolver 设置 DNS 解析器。
func (s *Server) SetResolver(r resolver.Resolver) {
	s.resolver = r
}

// attachHTTP2 在 fasthttp.Server 上挂载 HTTP/2 ALPN 分派。
//
// 仅在 serverCfg.SSL.HTTP2.Enabled 且 fastSrv 已配置 TLS 时生效：
//   - 从 serverCfg.ClientMaxBodySize 解析请求体上限（未配置时使用默认值）；
//   - 任一代理启用请求体流式时，h2 适配器同步开启流式读取；
//   - 调用 http2.Attach 注册 "h2" NextProto 处理器。
//
// 参数：
//   - fastSrv: 目标 fasthttp.Server（需已设置 TLSConfig）
//   - serverCfg: 该分组代表虚拟主机的配置
//   - handler: 该分组的 fasthttp 请求处理器
func (s *Server) attachHTTP2(fastSrv *fasthttp.Server, serverCfg *config.ServerConfig, handler fasthttp.RequestHandler) {
	if fastSrv == nil || fastSrv.TLSConfig == nil || serverCfg == nil || !serverCfg.SSL.HTTP2.Enabled || handler == nil {
		return
	}

	http2.Attach(fastSrv, s.prepareHTTP2Config(serverCfg), handler)
}

// prepareHTTP2Config 补全 HTTP/2 连接级的请求体参数。
//
// ALPN 挂载与明文 h2c 嗅探共用同一份推导逻辑：请求体上限取自虚拟主机的
// client_max_body_size（解析失败用默认值），任一代理开启请求体流式时
// h2 适配器同步流式读取。返回配置副本而非就地改写，避免多个监听分组
// 先后推导时相互影响。
//
// 参数：
//   - serverCfg: 虚拟主机配置
//
// 返回值：
//   - *config.HTTP2Config: 补全请求体参数后的配置副本
func (s *Server) prepareHTTP2Config(serverCfg *config.ServerConfig) *config.HTTP2Config {
	h2cfg := serverCfg.SSL.HTTP2
	if h2cfg.MaxBodySize <= 0 {
		if size, err := bodylimit.ParseSize(serverCfg.ClientMaxBodySize); err == nil {
			h2cfg.MaxBodySize = size
		} else {
			h2cfg.MaxBodySize = bodylimit.DefaultMaxBodySize
		}
	}
	h2cfg.StreamRequestBody = config.AnyProxyRequestStreaming(s.config.Servers)

	return &h2cfg
}

// wrapH2C 在不使用 TLS 的监听器上启用明文 HTTP/2（h2c）。
//
// 仅当 ssl.http2.enabled 与 ssl.http2.h2c_enabled 同时为 true 时生效，并同时
// 挂上两种接入方式：
//   - 监听器嗅探 HTTP/2 连接前导（prior knowledge），命中的请求由 http2.Server
//     直接服务，其余连接回放原始字节交回 fasthttp 按 HTTP/1.1 处理；
//   - fasthttp 处理器最外层拦截 Upgrade: h2c 握手，劫持连接写 101 后转 HTTP/2。
//
// TLS 监听器的协议分派由 ALPN 负责（见 attachHTTP2），此处不介入。
//
// h2c 启用后由包装监听器在协议嗅探前统一执行 max_conns_per_ip，确保
// HTTP/1.1 与 prior-knowledge h2c 共用同一 IP 额度；fasthttp 自身的同名限制
// 随后关闭以避免 HTTP/1.1 重复计数。连接总数仍用 serverCfg.Concurrency 约束。
//
// 必须在 fastSrv 启动前调用：升级握手要包住整个处理器链，因此会就地替换
// fastSrv.Handler，让握手不经过中间件、不计入访问日志与限流。
//
// 参数：
//   - fastSrv: 待启用 h2c 的 fasthttp.Server（明文监听器）
//   - ln: 原始明文监听器
//   - serverCfg: 该监听分组代表虚拟主机的配置
//   - handler: 该分组的 fasthttp 请求处理器
//
// 返回值：
//   - net.Listener: 需要嗅探时返回包装监听器，否则原样返回 ln
func (s *Server) wrapH2C(fastSrv *fasthttp.Server, ln net.Listener, serverCfg *config.ServerConfig, handler fasthttp.RequestHandler) net.Listener {
	if fastSrv == nil || ln == nil || serverCfg == nil || handler == nil {
		return ln
	}
	h2cfg := s.prepareHTTP2Config(serverCfg)
	if !h2cfg.Enabled || !h2cfg.H2CEnabled {
		return ln
	}

	h2s, err := http2.NewServer(
		h2cfg,
		handler,
		nil,
		http2.WithMaxConcurrentConns(serverCfg.Concurrency),
		http2.WithMaxConnsPerIP(serverCfg.MaxConnsPerIP),
	)
	if err != nil {
		logging.Error().Err(err).Msg("Failed to create h2c server")
		return ln
	}

	// 包装监听器已在协议嗅探前统一计数，fasthttp 不再重复登记 HTTP/1.1。
	fastSrv.MaxConnsPerIP = 0
	// h2c Upgrade 的流 1 先由 fasthttp 读取，需与 HTTP/2 适配器使用同一上限。
	if h2cfg.MaxBodySize > 0 && h2cfg.MaxBodySize <= int64(^uint(0)>>1) {
		fastSrv.MaxRequestBodySize = int(h2cfg.MaxBodySize)
	}
	// 升级握手放在处理器链最外层：被升级的请求不该走一遍业务中间件。
	fastSrv.Handler = h2s.UpgradeHandler(handler)

	s.h2cServersMu.Lock()
	s.h2cServers = append(s.h2cServers, h2s)
	s.h2cServersMu.Unlock()

	logging.Info().
		Str("listen", serverCfg.Listen).
		Str("protocol", "h2c").
		Int("max_concurrent_streams", h2cfg.MaxConcurrentStreams).
		Msg("HTTP/2 prior-knowledge and upgrade (h2c) attached to plaintext listener")

	return h2s.Wrap(ln)
}

// wrapGroupH2C 为明文监听分组启用 h2c 嗅探与升级握手。
//
// 与 attachHTTP2 一致取分组内首个满足条件的虚拟主机：h2c 是连接级协议，
// 连接上后续请求按 Host 头由分组处理器分流，无需每虚拟主机一个嗅探器。
// 已配置 TLS 的分组直接返回原监听器（明文嗅探不适用于 TLS 端口）。
//
// 参数：
//   - ln: 分组创建好的原始监听器
//   - group: 共享该监听器的虚拟主机分组
//   - fastSrv: 该分组的 fasthttp.Server（用于判断是否已配置 TLS）
//   - handler: 该分组的组合请求处理器
//
// 返回值：
//   - net.Listener: 供 fasthttp.Server.Serve 使用的监听器
func (s *Server) wrapGroupH2C(ln net.Listener, group listenGroup, fastSrv *fasthttp.Server, handler fasthttp.RequestHandler) net.Listener {
	if fastSrv != nil && fastSrv.TLSConfig != nil {
		return ln
	}
	for _, idx := range group.indices {
		serverCfg := &s.config.Servers[idx]
		if serverCfg.SSL.HTTP2.Enabled && serverCfg.SSL.HTTP2.H2CEnabled {
			return s.wrapH2C(fastSrv, ln, serverCfg, handler)
		}
	}
	return ln
}

// warnHTTP2H2CConfig 提示 h2c 相关的无效配置组合。
//
// h2c_enabled 只在明文监听器上、且 ssl.http2.enabled 为 true 时才生效；
// 校验层只能判断"无 SSL 时是否放行 enabled"，无法感知监听器与虚拟主机的
// 组合，因此这类"配了但不生效"的情形在启动阶段告警。
func (s *Server) warnHTTP2H2CConfig() {
	for i := range s.config.Servers {
		serverCfg := &s.config.Servers[i]
		h2cfg := &serverCfg.SSL.HTTP2
		if !h2cfg.H2CEnabled {
			continue
		}
		if !h2cfg.Enabled {
			logging.Warn().
				Str("server", serverCfg.Name).
				Msg("ssl.http2.h2c_enabled requires ssl.http2.enabled: ignored")
			continue
		}
		if serverCfg.UsesTLS() {
			logging.Warn().
				Str("server", serverCfg.Name).
				Msg("ssl.http2.h2c_enabled only applies to plaintext listeners: use ALPN on this TLS listener")
		}
	}
}
