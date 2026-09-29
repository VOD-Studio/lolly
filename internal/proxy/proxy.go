// Package proxy 反向代理包，为 Lolly HTTP 服务器提供反向代理功能。
//
// 该包使用 fasthttp.HostClient 实现高性能反向代理，支持连接池和自动 keep-alive 管理。
// 支持负载均衡、WebSocket 转发、自定义请求头/响应头、上游 SSL/TLS、DNS 动态解析、
// 代理缓存、重定向改写和全面的超时配置。
//
// 主要功能：
//   - 多后端负载均衡：支持 round_robin、weighted_round_robin、least_conn、ip_hash、consistent_hash
//   - Lua 动态选择：通过 balancer_by_lua 脚本实现自定义负载均衡逻辑
//   - 故障转移：支持 next_upstream 配置，自动重试失败请求到其他健康目标
//   - WebSocket 代理：支持 ws:// 和 wss:// 协议的透明双向转发
//   - 上游 SSL/TLS：支持自定义 CA 证书、客户端证书（mTLS）、SNI 和 TLS 版本控制
//   - DNS 动态解析：支持后端域名自动解析、IP 缓存和定时刷新
//   - 代理缓存：支持响应缓存、缓存锁防击穿、后台刷新过期缓存
//   - 重定向改写：支持 default/custom/off 模式改写 Location 和 Refresh 响应头
//   - 健康检查：支持主动 HTTP 探测和被动失败标记
//   - 临时文件：大响应自动写入临时文件，避免内存溢出
//
// 主要用途：
//
//	用于将客户端 HTTP 请求代理转发到后端服务器集群，实现负载均衡、缓存加速、
//	协议转换等功能，适用于 API 网关、反向代理服务器等场景。
//
// 注意事项：
//   - Proxy 实例的公开方法均为并发安全
//   - 使用前需确保 targets 中至少有一个健康目标
//   - Lua 脚本执行有超时保护，默认 100ms
//
// 作者：xfy
package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	urlpath "path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/sync/singleflight"
	"rua.plus/lolly/internal/cache"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/lua"
	"rua.plus/lolly/internal/netutil"
	"rua.plus/lolly/internal/resolver"
	"rua.plus/lolly/internal/utils"
	"rua.plus/lolly/internal/variable"
)

// proxyDebugLog 在 DEBUG 级别记录代理日志
// 调用者必须先检查 logging.Debug().Enabled() 以避免不必要的内存分配
func proxyDebugLog(msg string, kv ...any) {
	event := logging.Debug()
	for i := 0; i < len(kv)-1; i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		switch v := kv[i+1].(type) {
		case string:
			event = event.Str(key, v)
		case int:
			event = event.Int(key, v)
		case bool:
			event = event.Bool(key, v)
		default:
			event = event.Interface(key, v)
		}
	}
	event.Msg(msg)
}

const (
	// upstreamCache 上游缓存标识。
	// 用于标记请求可直接使用缓存响应，无需转发到上游。
	upstreamCache = "CACHE"

	// 负载均衡算法名称，与配置中的 LoadBalance 字段对应。
	lbRoundRobin         = "round_robin"          // 简单轮询
	lbWeightedRoundRobin = "weighted_round_robin" // 加权轮询
	lbLeastConn          = "least_conn"           // 最少连接
	lbIPHash             = "ip_hash"              // IP 哈希
	lbConsistentHash     = "consistent_hash"      // 一致性哈希
	lbRandom             = "random"               // 随机（Power of Two Choices）
	lbLeastTime          = "least_time"           // 最小响应时间
	lbSticky             = "sticky"               // 会话粘性
)

var upstreamTimingPool = sync.Pool{
	New: func() any {
		return NewUpstreamTiming()
	},
}

// Proxy 表示反向代理实例，负责将 HTTP 请求转发到后端目标。
//
// 它为每个后端目标管理连接池（HostClient），并提供负载均衡、
// 缓存、健康检查、Lua 动态选择等功能。
//
// 注意事项：
//   - 所有公开方法均为并发安全
//   - 使用前需确保 targets 中至少有一个健康目标
type Proxy struct {
	balancer         loadbalance.Balancer            // 主负载均衡器
	fallbackBalancer loadbalance.Balancer            // Lua 失败时的备用均衡器
	resolver         resolver.Resolver               // DNS 解析器
	clients          map[string]*fasthttp.HostClient // 后端连接池，key 为 target URL
	config           *config.ProxyConfig             // 代理配置
	cache            *cache.ProxyCache               // 代理缓存
	healthChecker    *HealthChecker                  // 健康检查器
	luaEngine        *lua.LuaEngine                  // Lua 引擎，用于 balancer_by_lua 功能
	redirectRewriter *RedirectRewriter               // 重定向改写器
	stopCh           chan struct{}                   // 停止信号通道
	targets          []*loadbalance.Target           // 后端目标列表
	mu               sync.RWMutex                    // 保护并发访问的读写锁
	started          atomic.Bool                     // 代理启动标志
	requestStreaming bool                            // 请求体流式转发（buffering.request_mode: off）
	cacheIgnoreSet   map[string]bool                 // 缓存时忽略的响应头集合
	refreshGroup     singleflight.Group              // 合并并发后台缓存刷新
	closeOnce        sync.Once                       // 保证生命周期资源仅关闭一次
	dnsWG            sync.WaitGroup                  // 等待 DNS 刷新协程退出
}

// NewProxy 使用给定的配置和后台目标创建一个新的反向代理实例。
// 它根据配置初始化负载均衡器，并为每个后端目标创建 HostClient。
//
// 参数：
//   - cfg: 代理配置，包括超时时间、请求头和负载均衡策略
//   - targets: 要代理请求的后端目标列表
//   - transportCfg: 可选的 Transport 连接池配置，nil 时使用默认值
//   - luaEngine: 可选的 Lua 引擎，用于 balancer_by_lua 功能
//
// 返回值：
//   - *Proxy: 配置完成并可处理请求的代理实例
//   - error: 初始化失败时非空（无效配置、没有健康目标等）
func NewProxy(cfg *config.ProxyConfig, targets []*loadbalance.Target, transportCfg *config.TransportConfig, luaEngine *lua.LuaEngine) (*Proxy, error) {
	if cfg == nil {
		return nil, errors.New("proxy config is nil")
	}

	if len(targets) == 0 {
		return nil, errors.New("no proxy targets provided")
	}

	// 根据配置创建负载均衡器
	balancer, err := createBalancer(cfg)
	if err != nil {
		return nil, err
	}

	// 创建 fallback 负载均衡器
	fallbackAlgo := cfg.BalancerByLua.Fallback
	if fallbackAlgo == "" {
		fallbackAlgo = lbRoundRobin
	}
	fallbackBalancer, err := createBalancerByName(fallbackAlgo, cfg)
	if err != nil {
		return nil, fmt.Errorf("create fallback balancer: %w", err)
	}

	p := &Proxy{
		targets:          targets,
		clients:          make(map[string]*fasthttp.HostClient),
		balancer:         balancer,
		fallbackBalancer: fallbackBalancer,
		config:           cfg,
		luaEngine:        luaEngine,
		stopCh:           make(chan struct{}),
	}

	// 为每个后端目标初始化 HostClient
	for _, target := range targets {
		if target.URL == "" {
			continue
		}

		client := createHostClient(target.URL, cfg.Timeout, transportCfg, cfg.ProxySSL, cfg.ProxyBind, cfg.Buffering)
		clientKey := target.URL
		if cfg.ProxyBind != "" {
			clientKey = target.URL + "|" + cfg.ProxyBind
		}
		p.clients[clientKey] = client
	}

	// 初始化代理缓存（如果启用）
	if cfg.Cache.Enabled {
		rules := make([]cache.ProxyCacheRule, 0)
		if cfg.Cache.MaxAge > 0 {
			// 使用配置中的方法，若为空则使用默认值 GET, HEAD (nginx 默认行为)
			methods := cfg.Cache.Methods
			if len(methods) == 0 {
				methods = []string{"GET", "HEAD"}
			}
			rules = append(rules, cache.ProxyCacheRule{
				Path:     cfg.Path,
				Methods:  methods,
				Statuses: nil, // nil = 所有可缓存状态码 (由 getCacheDuration 处理)
				MaxAge:   cfg.Cache.MaxAge,
			})
		}
		p.cache = cache.NewProxyCache(rules, cfg.Cache.CacheLock, cfg.Cache.StaleWhileRevalidate, cfg.Cache.StaleIfError, cfg.Cache.StaleIfTimeout)
	}

	// 初始化重定向改写器
	rewriter, err := NewRedirectRewriter(cfg.RedirectRewrite, cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to create redirect rewriter: %w", err)
	}
	p.redirectRewriter = rewriter

	cacheIgnoreSet := make(map[string]bool, len(cfg.Cache.CacheIgnoreHeaders))
	for _, h := range cfg.Cache.CacheIgnoreHeaders {
		cacheIgnoreSet[strings.ToLower(h)] = true
	}
	p.cacheIgnoreSet = cacheIgnoreSet

	// 请求体流式转发：buffering.request_mode == "off"
	p.requestStreaming = cfg.Buffering.RequestStreamingEnabled()

	return p, nil
}

// RequestStreamingEnabled 报告是否启用了请求体流式转发。
//
// 启用时，带 body stream 的请求将直接把流交给上游 HostClient，
// 不在 lolly 侧物化；同时禁用 next_upstream 重试与代理缓存。
//
// 返回值：
//   - bool: true 表示已启用
func (p *Proxy) RequestStreamingEnabled() bool { return p.requestStreaming }

// cacheVaryHeaders 返回用于构建缓存键的 Vary 请求头列表。
//
// 当 varyResponse 非空时，将响应中的 Vary 头与配置的值合并去重。
// 请求头名称统一按大小写不敏感方式比较，避免重复。
func (p *Proxy) cacheVaryHeaders(varyResponse []byte) []string {
	configured := p.config.Cache.Vary
	if len(varyResponse) == 0 {
		return configured
	}

	result := make([]string, 0, len(configured)+bytes.Count(varyResponse, []byte(","))+1)
	result = append(result, configured...)
	seen := make(map[string]struct{}, len(result))
	for _, h := range result {
		seen[strings.ToLower(h)] = struct{}{}
	}

	for _, part := range strings.Split(b2s(varyResponse), ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, name)
	}

	return result
}

// stickyBalancer wraps StickySession to implement loadbalance.Balancer.
// It delegates Select/SelectExcluding to the fallback balancer while
// allowing the proxy to access the StickySession for cookie-based routing.
type stickyBalancer struct {
	sticky   *loadbalance.StickySession
	fallback loadbalance.Balancer
}

func (b *stickyBalancer) Select(targets []*loadbalance.Target) *loadbalance.Target {
	return b.fallback.Select(targets)
}

func (b *stickyBalancer) SelectExcluding(targets []*loadbalance.Target, excluded []*loadbalance.Target) *loadbalance.Target {
	return b.fallback.SelectExcluding(targets, excluded)
}

// createBalancerByName 根据算法名称创建负载均衡器。
//
// 支持的算法：
//   - round_robin: 简单轮询，按顺序选择目标
//   - weighted_round_robin: 加权轮询，按权重比例分配
//   - least_conn: 最少连接，选择当前连接数最少的目标
//   - ip_hash: IP 哈希，同一客户端 IP 固定选择同一目标
//   - consistent_hash: 一致性哈希，支持虚拟节点和自定义 hash_key
//
// 参数：
//   - name: 算法名称
//   - cfg: 代理配置，用于获取虚拟节点数和 hash_key
//
// 返回值：
//   - loadbalance.Balancer: 创建的负载均衡器实例
//   - error: 不支持的算法时返回错误
func createBalancerByName(name string, cfg *config.ProxyConfig) (loadbalance.Balancer, error) {
	switch name {
	case lbRoundRobin, "":
		return loadbalance.NewRoundRobin(), nil
	case lbWeightedRoundRobin:
		return loadbalance.NewWeightedRoundRobin(), nil
	case lbLeastConn:
		return loadbalance.NewLeastConnections(), nil
	case lbIPHash:
		return loadbalance.NewIPHash(), nil
	case lbConsistentHash:
		virtualNodes := cfg.VirtualNodes
		if virtualNodes <= 0 {
			virtualNodes = 150
		}
		return loadbalance.NewConsistentHash(virtualNodes, cfg.HashKey), nil
	case lbRandom:
		return loadbalance.NewRandom(), nil
	case lbLeastTime:
		metric := cfg.LeastTime.Metric
		if metric == "" {
			metric = "last_byte"
		}
		defaultTime := cfg.LeastTime.DefaultTime
		if defaultTime <= 0 {
			defaultTime = time.Millisecond
		}
		return loadbalance.NewLeastTime(metric, defaultTime), nil
	case lbSticky:
		stickyCfg := loadbalance.StickyConfig{
			Enabled:  cfg.Sticky.Enabled,
			Name:     cfg.Sticky.Name,
			Expires:  cfg.Sticky.Expires,
			Domain:   cfg.Sticky.Domain,
			Path:     cfg.Sticky.Path,
			Secure:   cfg.Sticky.Secure,
			HttpOnly: cfg.Sticky.HttpOnly,
			SameSite: cfg.Sticky.SameSite,
		}
		if stickyCfg.Name == "" {
			stickyCfg.Name = "lolly_route"
		}
		if stickyCfg.Expires <= 0 {
			stickyCfg.Expires = time.Hour
		}
		if stickyCfg.Path == "" {
			stickyCfg.Path = "/"
		}

		fallbackAlgo := cfg.Sticky.FallbackAlgo
		if fallbackAlgo == "" {
			fallbackAlgo = lbRoundRobin
		}
		fallbackBalancer, err := createBalancerByName(fallbackAlgo, cfg)
		if err != nil {
			return nil, fmt.Errorf("sticky fallback balancer: %w", err)
		}

		sticky := loadbalance.NewStickySession(stickyCfg, fallbackBalancer)
		sticky.Start()
		return &stickyBalancer{sticky: sticky, fallback: fallbackBalancer}, nil
	default:
		return nil, errors.New("unsupported load balance algorithm: " + name)
	}
}

// SetHealthChecker 设置健康检查器用于被动健康检查。
//
// 当代理请求失败时，将调用健康检查器的 MarkUnhealthy 方法，
// 将失败的目标标记为不健康，避免后续请求继续路由到该目标。
//
// 参数：
//   - hc: 健康检查器实例，nil 时禁用被动健康检查
func (p *Proxy) SetHealthChecker(hc *HealthChecker) {
	p.healthChecker = hc
}

// createBalancer 根据配置中指定的算法名称创建负载均衡器。
// 是对 createBalancerByName 的便捷封装。
//
// 参数：
//   - cfg: 代理配置，从 cfg.LoadBalance 读取算法名称
//
// 返回值：
//   - loadbalance.Balancer: 创建的负载均衡器实例
//   - error: 不支持的算法时返回错误
func createBalancer(cfg *config.ProxyConfig) (loadbalance.Balancer, error) {
	return createBalancerByName(cfg.LoadBalance, cfg)
}

// createHostClient 为指定的后端目标 URL 创建 fasthttp.HostClient。
//
// 从目标 URL 解析地址和 TLS 标志，应用 Transport 连接池配置
// （空闲连接超时、最大连接数），以及上游 SSL 配置。
//
// 参数：
//   - targetURL: 后端目标 URL（如 http://backend:8080）
//   - timeout: 代理超时配置（读写超时、连接超时）
//   - transportCfg: 可选的 Transport 连接池配置，nil 时使用默认值
//   - sslCfg: 可选的上游 SSL 配置
//
// 返回值：
//   - *fasthttp.HostClient: 配置完成的 HostClient 实例
func createHostClient(targetURL string, timeout config.ProxyTimeout, transportCfg *config.TransportConfig, sslCfg *config.ProxySSLConfig, proxyBind string, buffering *config.ProxyBufferingConfig) *fasthttp.HostClient {
	// 从目标 URL 解析主机和协议
	// addDefaultPort=true 确保 HostClient.Addr 包含端口（host:port 格式）
	addr, isTLS := netutil.ParseTargetURL(targetURL, true)

	// 默认值
	maxIdleConnDuration := 90 * time.Second
	maxConns := 512 // fasthttp 推荐值 DefaultMaxConnsPerHost

	// 应用 Transport 配置
	if transportCfg != nil {
		if transportCfg.IdleConnTimeout > 0 {
			maxIdleConnDuration = transportCfg.IdleConnTimeout
		}
		if transportCfg.MaxConnsPerHost > 0 {
			maxConns = transportCfg.MaxConnsPerHost
		}
	}

	client := &fasthttp.HostClient{
		Addr:                   addr,
		IsTLS:                  isTLS,
		ReadTimeout:            timeout.Read,
		WriteTimeout:           timeout.Write,
		MaxIdleConnDuration:    maxIdleConnDuration,
		MaxConns:               maxConns,
		MaxConnWaitTimeout:     timeout.Connect,
		RetryIf:                nil, // 禁用自动重试
		DisablePathNormalizing: false,
		SecureErrorLogMessage:  false,
	}

	// Dial timeout：如果配置了 Dial，使用它作为 TCP 连接建立超时
	// 否则使用 Connect 作为向后兼容
	dialTimeout := timeout.Dial
	if dialTimeout <= 0 {
		dialTimeout = timeout.Connect
	}
	if dialTimeout <= 0 {
		dialTimeout = 30 * time.Second // 最终默认值
	}

	// 设置自定义 Dial 函数以使用 Dial timeout
	// 如果有 ProxyBind 或需要自定义 Dial timeout
	if proxyBind != "" || timeout.Dial > 0 {
		client.Dial = func(addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout: dialTimeout,
			}
			if proxyBind != "" {
				dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(proxyBind)}
			}
			return dialer.Dial("tcp", addr)
		}
	}

	// Buffering 控制
	if buffering != nil && buffering.Mode == "off" {
		client.StreamResponseBody = true
	}
	if buffering != nil && buffering.BufferSize > 0 {
		client.ReadBufferSize = buffering.BufferSize
		client.WriteBufferSize = buffering.BufferSize
	}

	// 上游 SSL 配置（使用原生 TLSConfig）
	if sslCfg != nil && sslCfg.Enabled && isTLS {
		host, _ := netutil.ParseTargetURL(targetURL, false)
		tlsCfg, err := CreateTLSConfig(sslCfg, host)
		if err != nil {
			logging.Error().Err(err).Str("target", targetURL).Msg("Failed to create upstream TLS config")
		} else {
			client.TLSConfig = tlsCfg
		}
	}

	return client
}

// UpstreamTiming 记录上游请求的各个时间戳。
//
// 用于捕获连接建立、首字节接收、响应完成等关键时间点，
// 计算连接时间、首字节时间和总响应时间，供日志和监控使用。
type UpstreamTiming struct {
	start          time.Time // 请求开始时间
	connectStart   time.Time // 连接开始时间
	connectEnd     time.Time // 连接完成时间
	headerReceived time.Time // 接收到响应头的时间
	responseEnd    time.Time // 响应完成时间
}

// NewUpstreamTiming 创建并初始化上游计时器。
// 自动记录请求开始时间。
//
// 返回值：
//   - *UpstreamTiming: 初始化的计时器实例
func NewUpstreamTiming() *UpstreamTiming {
	return &UpstreamTiming{
		start: time.Now(),
	}
}

func (t *UpstreamTiming) reset() {
	*t = UpstreamTiming{}
}

// MarkConnectStart 标记连接开始时间点。
func (t *UpstreamTiming) MarkConnectStart() {
	t.connectStart = time.Now()
}

// MarkConnectEnd 标记连接完成时间点。
func (t *UpstreamTiming) MarkConnectEnd() {
	t.connectEnd = time.Now()
}

// MarkHeaderReceived 标记接收到响应头时间点。
func (t *UpstreamTiming) MarkHeaderReceived() {
	t.headerReceived = time.Now()
}

// MarkResponseEnd 标记响应完成时间点。
func (t *UpstreamTiming) MarkResponseEnd() {
	t.responseEnd = time.Now()
}

// GetConnectTime 获取连接建立耗时（秒）。
// 如果连接开始或结束时间未记录，返回 0。
//
// 返回值：
//   - float64: 连接耗时，单位为秒
func (t *UpstreamTiming) GetConnectTime() float64 {
	if t.connectStart.IsZero() || t.connectEnd.IsZero() {
		return 0
	}
	return t.connectEnd.Sub(t.connectStart).Seconds()
}

// GetHeaderTime 获取首字节响应时间（秒）。
// 计算从连接完成到接收到响应头的耗时。
// 如果任一时间点未记录，返回 0。
//
// 返回值：
//   - float64: 首字节耗时，单位为秒
func (t *UpstreamTiming) GetHeaderTime() float64 {
	if t.connectEnd.IsZero() || t.headerReceived.IsZero() {
		return 0
	}
	return t.headerReceived.Sub(t.connectEnd).Seconds()
}

// GetResponseTime 获取总响应时间（秒）。
// 计算从连接完成到响应完成的耗时。
// 如果任一时间点未记录，返回 0。
//
// 返回值：
//   - float64: 响应耗时，单位为秒
func (t *UpstreamTiming) GetResponseTime() float64 {
	if t.connectEnd.IsZero() || t.responseEnd.IsZero() {
		return 0
	}
	return t.responseEnd.Sub(t.connectEnd).Seconds()
}

// FinalizeUpstreamVars 在请求处理结束时设置上游变量到变量上下文。
//
// 该函数应在 ServeHTTP 的 defer 中调用，用于计算并设置以下变量：
//   - upstream_addr: 上游服务器地址
//   - upstream_status: 上游响应状态码
//   - upstream_response_time: 响应耗时
//   - upstream_connect_time: 连接耗时
//   - upstream_header_time: 首字节耗时
//
// 参数：
//   - vc: 变量上下文，用于存储上游变量
//   - upstreamAddr: 上游服务器地址
//   - upstreamStatus: 上游响应状态码
//   - timing: 时间记录器
func FinalizeUpstreamVars(vc *variable.Context, upstreamAddr string, upstreamStatus int, timing *UpstreamTiming) {
	if vc == nil {
		return
	}

	connectTime := timing.GetConnectTime()
	headerTime := timing.GetHeaderTime()
	responseTime := timing.GetResponseTime()

	vc.SetUpstreamVars(upstreamAddr, upstreamStatus, responseTime, connectTime, headerTime)
}

// ServeHTTP 通过将传入的 HTTP 请求转发到选定的后端目标来处理请求。
// 实现了 fasthttp 请求处理器接口。
//
// 处理流程：
// 1. 使用负载均衡选择目标
// 2. 准备请求（修改请求头）
// 3. 将请求转发到后端
// 4. 将响应复制回客户端
//
// 如果没有可用的健康目标，返回 502 Bad Gateway。
// 如果后端请求失败，根据 next_upstream 配置尝试下一个目标。
func (p *Proxy) ServeHTTP(ctx *fasthttp.RequestCtx) {
	if logging.Debug().Enabled() {
		proxyDebugLog("[PROXY] 收到请求",
			"path", b2s(ctx.Path()),
			"host", b2s(ctx.Host()),
			"method", b2s(ctx.Method()),
		)
	}

	// 上游变量捕获
	var upstreamAddr string
	var upstreamStatus int
	timing, ok := upstreamTimingPool.Get().(*UpstreamTiming)
	if !ok {
		timing = NewUpstreamTiming()
	}
	timing.reset()

	// 在请求头被修改前捕获原始 Host，用于构建缓存键。
	// 防止 modifyRequestHeaders 将 Host 改为上游目标后导致缓存键丢失客户端 Host 区分。
	cacheHost := ctx.Request.Header.Host()
	computeCacheKey := func(varyHeaders []string) (uint64, string) {
		return p.buildCacheKeyHashWithHost(ctx, cacheHost, varyHeaders)
	}

	// 创建变量上下文用于设置上游变量
	vc := variable.NewContext(ctx)
	defer func() {
		if timing.responseEnd.IsZero() {
			timing.MarkResponseEnd()
		}
		FinalizeUpstreamVars(vc, upstreamAddr, upstreamStatus, timing)
		variable.ReleaseContext(vc)
		upstreamTimingPool.Put(timing)
	}()

	// 请求体流式：启用且存在 body stream 时，禁用重试与缓存（流不可回放）。
	// 仅对真正带 body 的请求才进入流式路径；GET/HEAD 无 body stream 仍走原逻辑。
	streamRequestBody := p.requestStreaming && ctx.Request.BodyStream() != nil

	// 故障转移配置
	maxTries := p.config.NextUpstream.Tries
	if maxTries <= 0 {
		maxTries = 1 // 默认不重试
	}
	// 流式请求体不可重试：一旦向上游写入部分 body，流无法回放给另一个目标。
	if streamRequestBody && maxTries > 1 {
		if logging.Debug().Enabled() {
			proxyDebugLog("[PROXY] 流式请求体禁用 next_upstream 重试",
				"method", b2s(ctx.Method()),
				"originalTries", maxTries,
			)
		}
		maxTries = 1
	}
	httpCodes := p.config.NextUpstream.HTTPCodes
	if len(httpCodes) == 0 {
		// 默认重试的状态码
		httpCodes = []int{502, 503, 504}
	}

	// 已尝试的目标列表（用于故障转移时排除）
	attemptedTargets := make([]*loadbalance.Target, 0, maxTries)

	var lastErr error

	for attempt := 0; attempt < maxTries; attempt++ {
		// 选择目标（第一次使用普通选择，后续排除已失败目标）
		var target *loadbalance.Target
		if attempt == 0 {
			target = p.selectTarget(ctx)
		} else {
			target = p.selectTargetExcluding(ctx, attemptedTargets)
		}

		if target == nil {
			if attempt == 0 {
				// 没有可用后端
				upstreamAddr = "FAILED"
				upstreamStatus = 502
				utils.SendErrorWithDetail(ctx, utils.ErrBadGateway, "no healthy upstream")
				return
			}
			// 没有更多可用目标，返回最后一次错误
			break
		}

		attemptedTargets = append(attemptedTargets, target)

		if logging.Debug().Enabled() {
			proxyDebugLog("[PROXY] 选中目标",
				"url", target.URL,
				"healthy", target.Healthy.Load(),
			)
		}

		// 获取所选目标的客户端
		client := p.getClient(target.URL)
		if client == nil {
			logging.Warn().Msgf("[PROXY] client 为 nil, url=%s", target.URL)
			// 标记为不健康并继续尝试下一个
			if p.healthChecker != nil {
				p.healthChecker.MarkUnhealthy(target)
			}
			continue
		}

		if logging.Debug().Enabled() {
			proxyDebugLog("[PROXY] client 信息",
				"addr", client.Addr,
				"isTLS", client.IsTLS,
			)
		}

		// 增加连接计数（用于最少连接数负载均衡）
		loadbalance.IncrementConnections(target)

		// 保存客户端原始 host（在 modifyRequestHeaders 改写前）
		// 用于 redirect_rewrite 获取客户端实际访问地址
		originalClientHost := string(ctx.Host())

		// 设置上游地址
		upstreamAddr = target.URL

		// 检查是否为 WebSocket 升级请求
		if isWebSocketRequest(ctx) {
			defer func() {
				loadbalance.DecrementConnections(target)
			}()
			defer timing.MarkConnectEnd()
			timing.MarkConnectStart()
			err := WebSocket(ctx, target, p.config.Timeout.Connect, &p.config.Headers, p.config.ProxySSL)
			if err != nil {
				upstreamStatus = fasthttp.StatusBadGateway
				logging.Error().Err(err).Msg("WebSocket proxy error")
				return
			}
			upstreamStatus = fasthttp.StatusSwitchingProtocols
			return
		}

		// 准备请求
		req := &ctx.Request

		// 修改请求头
		p.modifyRequestHeaders(ctx, target)

		// 关键：修改请求 URI 为完整的目标 URL
		// HostClient 要求 URI 格式必须与 Addr/IsTLS 一致
		// 例如：IsTLS=true 时，URI 应为 https://host/path
		// SAFETY: lifetime=ephemeral - consumed immediately by SetRequestURIBytes
		path := ctx.URI().Path()
		query := ctx.URI().QueryString()

		// ProxyURI 语义：当 target.ProxyURI 设置时，替换请求路径
		// 这实现了 nginx proxy_pass URI 传递语义：
		//   proxy_pass http://backend/v2/ → 请求路径替换为 /v2/
		if target.ProxyURI != "" {
			path = []byte(target.ProxyURI)
		}

		// 检查路径中的危险字符（防止 Proxy URI 注入）
		if bytes.ContainsAny(path, "@\r\n") {
			logging.Warn().Msgf("rejected suspicious proxy path containing dangerous chars: %s", path)
			upstreamStatus = 502
			loadbalance.DecrementConnections(target)
			utils.SendErrorWithDetail(ctx, utils.ErrBadGateway, "invalid proxy path")
			return
		}

		targetURI := make([]byte, 0, len(target.URL)+len(path)+len(query)+1)
		targetURI = append(targetURI, target.URL...)
		targetURI = append(targetURI, path...)
		if len(query) > 0 {
			targetURI = append(targetURI, '?')
			targetURI = append(targetURI, query...)
		}
		req.SetRequestURIBytes(targetURI)

		if logging.Debug().Enabled() {
			proxyDebugLog("[PROXY] 请求准备完成",
				"host", b2s(req.Header.Host()),
				"uri", b2s(req.RequestURI()),
				"targetURI", b2s(targetURI),
			)
		}

		// 尝试从缓存获取（如果启用且非流式请求体）
		if p.cache != nil && attempt == 0 && !streamRequestBody {
			// 检查请求方法是否允许缓存
			method := string(ctx.Request.Header.Method())
			path := string(ctx.Request.URI().Path())
			rule := p.cache.MatchRule(path, method, 0)
			if rule != nil {
				hashKey, origKey := computeCacheKey(p.cacheVaryHeaders(nil))
				if entry, ok, stale := p.cache.Get(hashKey, origKey); ok {
					// 缓存命中
					loadbalance.DecrementConnections(target)
					if !stale {
						// 新鲜缓存，直接返回
						upstreamAddr = upstreamCache
						upstreamStatus = entry.Status
						p.writeCachedResponse(ctx, entry)
						if p.redirectRewriter != nil {
							p.redirectRewriter.RewriteRefreshOnly(&ctx.Response, ctx, upstreamCache, originalClientHost)
						}
						return
					}
					// 过期缓存，尝试后台刷新，同时返回旧数据
					if !p.config.Cache.BackgroundUpdateDisable {
						entry.Updating.Store(true)
						reqCopy := fasthttp.AcquireRequest()
						ctx.Request.CopyTo(reqCopy)
						key := origKey
						go func() {
							defer entry.Updating.Store(false)
							defer fasthttp.ReleaseRequest(reqCopy)
							_, err, _ := p.refreshGroup.Do(key, func() (interface{}, error) {
								p.backgroundRefresh(reqCopy, target, hashKey, origKey)
								return nil, nil
							})
							if err != nil {
								logging.Error().Err(err).Msg("background cache refresh failed")
							}
						}()
					}
					upstreamAddr = upstreamCache
					upstreamStatus = entry.Status

					p.writeCachedResponse(ctx, entry)
					if p.redirectRewriter != nil {
						p.redirectRewriter.RewriteRefreshOnly(&ctx.Response, ctx, upstreamCache, originalClientHost)
					}
					return
				}

				// 检查是否需要缓存锁（防止缓存击穿）
				timeout := p.config.Cache.CacheLockTimeout
				if timeout == 0 && p.config.Cache.CacheLock {
					timeout = 5 * time.Second // nginx 默认 5s
				}
				waitCh, timedOut := p.cache.AcquireLockWithTimeout(hashKey, timeout)
				if !timedOut && waitCh != nil {
					// 有其他请求正在生成缓存，等待
					loadbalance.DecrementConnections(target)
					<-waitCh
					// 重新尝试获取缓存

					if entry, ok, _ := p.cache.Get(hashKey, origKey); ok {
						upstreamAddr = upstreamCache
						upstreamStatus = entry.Status

						p.writeCachedResponse(ctx, entry)
						if p.redirectRewriter != nil {
							p.redirectRewriter.RewriteRefreshOnly(&ctx.Response, ctx, upstreamCache, originalClientHost)
						}
						return
					}
					// 缓存未命中，需要重新选择目标
					loadbalance.IncrementConnections(target)
				}
				// timedOut 或获得锁：继续执行代理请求
			}
		}

		// 执行代理请求
		timing.MarkConnectStart()
		err := client.Do(req, &ctx.Response)
		timing.MarkConnectEnd()

		// DEBUG: 打印执行结果
		if err != nil {
			logging.Error().Msgf("[PROXY] 请求失败: url=%s, err=%v, errType=%T", target.URL, err, err)
		} else {
			if logging.Debug().Enabled() {
				proxyDebugLog("[PROXY] 请求成功",
					"url", target.URL,
					"status", ctx.Response.StatusCode(),
				)
			}
		}

		if err != nil {
			loadbalance.DecrementConnections(target)

			// 被动健康检查：标记目标为不健康
			if p.healthChecker != nil {
				p.healthChecker.MarkUnhealthy(target)
			}

			// 尝试使用 stale 缓存
			if p.cache != nil && !streamRequestBody {
				hashKey, origKey := computeCacheKey(p.cacheVaryHeaders(nil))
				isTimeout := errors.Is(err, fasthttp.ErrTimeout)
				if staleEntry, ok := p.cache.GetStale(hashKey, origKey, isTimeout); ok {
					logging.Info().Msgf("[PROXY] 使用 stale 缓存: key=%s, isTimeout=%v", origKey, isTimeout)
					p.writeCachedResponse(ctx, staleEntry)
					upstreamStatus = staleEntry.Status
					upstreamAddr = upstreamCache
					return
				}
			}

			// 释放缓存锁
			if p.cache != nil && attempt == 0 && !streamRequestBody {
				hashKey, _ := computeCacheKey(p.cacheVaryHeaders(nil))
				p.cache.ReleaseLock(hashKey, err)
			}

			// 设置失败状态
			if errors.Is(err, fasthttp.ErrTimeout) {
				upstreamStatus = 504
			} else {
				upstreamStatus = 502
			}

			lastErr = err
			// 继续尝试下一个目标
			continue
		}

		// 记录首字节时间
		timing.MarkHeaderReceived()

		// 记录首字节响应时间（用于 least_time 负载均衡）
		if recorder, ok := p.balancer.(loadbalance.ResponseTimeRecorder); ok {
			headerTime := timing.headerReceived.Sub(timing.connectEnd)
			recorder.RecordResponseTime(target, headerTime, 0)
		}

		// 请求成功，减少连接计数
		loadbalance.DecrementConnections(target)

		// 记录成功，重置软失败状态
		target.RecordSuccess()

		// 检测 X-Accel-Redirect 头，支持内部重定向
		if redirectPath := ctx.Response.Header.Peek("X-Accel-Redirect"); len(redirectPath) > 0 {
			pathStr := urlpath.Clean(string(redirectPath))
			if !strings.HasPrefix(pathStr, "/internal/") && !strings.HasPrefix(pathStr, "/admin/") {
				utils.SetInternalRedirect(ctx, pathStr)
				ctx.Request.SetRequestURI(pathStr)
			}
			return
		}

		// 检查响应状态码是否需要重试
		statusCode := ctx.Response.StatusCode()
		upstreamStatus = statusCode

		shouldRetry := slices.Contains(httpCodes, statusCode)

		if shouldRetry {
			// 释放缓存锁
			if p.cache != nil && attempt == 0 && !streamRequestBody {
				hashKey, _ := computeCacheKey(p.cacheVaryHeaders(nil))
				p.cache.ReleaseLock(hashKey, fmt.Errorf("HTTP %d", statusCode))
			}

			// 如果不是最后一次尝试，继续下一个目标
			if attempt < maxTries-1 {
				// 标记目标为不健康
				if p.healthChecker != nil {
					p.healthChecker.MarkUnhealthy(target)
				}
				continue
			}
		}

		// 重试成功时恢复健康状态
		if attempt > 0 && p.healthChecker != nil {
			p.healthChecker.MarkHealthy(target)
		}

		// 存入缓存（如果启用且响应可缓存且非流式请求体）
		if p.cache != nil && !streamRequestBody {
			// 再次检查方法是否允许缓存
			method := string(ctx.Request.Header.Method())
			path := string(ctx.Request.URI().Path())
			if rule := p.cache.MatchRule(path, method, statusCode); rule == nil {
				// 方法或状态码不在允许列表中，不缓存
				return
			}

			hashKey, origKey := computeCacheKey(p.cacheVaryHeaders(ctx.Response.Header.Peek("Vary")))
			if statusCode >= 200 && statusCode < 300 {
				// 检查 MinUses 阈值
				if entry, ok, _ := p.cache.Get(hashKey, origKey); ok {
					minUses := p.config.Cache.MinUses
					if minUses > 0 && entry.Uses.Load() < int32(minUses) {
						p.cache.ReleaseLock(hashKey, nil)
						return
					}
				}

				// 提取响应头
				headers := make(map[string]string, 20)

				var lastModified, etag string
				for key, value := range ctx.Response.Header.All() {
					if p.cacheIgnoreSet[b2s(bytes.ToLower(key))] {
						continue
					}
					headers[b2s(key)] = b2s(value)
					if bytes.EqualFold(key, []byte("last-modified")) {
						lastModified = b2s(value)
					} else if bytes.EqualFold(key, []byte("etag")) {
						etag = b2s(value)
					}
				}
				p.cache.Set(hashKey, origKey, ctx.Response.Body(), headers, statusCode, p.getCacheDuration(statusCode))
				if lastModified != "" || etag != "" {
					p.cache.SetValidationHeaders(hashKey, origKey, lastModified, etag)
				}
				// 注意：不能 Put 回 pool，因为 cache.Set 存储了 map 引用
				// 后续 writeCachedResponse 会读取该 map
			}
			p.cache.ReleaseLock(hashKey, nil)
		}

		// 改写重定向响应头（Location/Refresh）
		if p.redirectRewriter != nil && p.redirectRewriter.Mode() != "off" {
			p.redirectRewriter.RewriteResponse(&ctx.Response, ctx, upstreamAddr, originalClientHost)
		}

		// 修改响应头
		p.modifyResponseHeaders(ctx)

		// 记录完整响应时间（用于 least_time 负载均衡）
		timing.MarkResponseEnd()
		if recorder, ok := p.balancer.(loadbalance.ResponseTimeRecorder); ok {
			headerTime := timing.headerReceived.Sub(timing.connectEnd)
			lastByteTime := timing.responseEnd.Sub(timing.connectEnd)
			recorder.RecordResponseTime(target, headerTime, lastByteTime)
		}

		return
	}

	// 所有尝试都失败
	if lastErr != nil {
		// 处理不同类型的错误
		if errors.Is(lastErr, fasthttp.ErrTimeout) {
			upstreamStatus = 504
			utils.SendError(ctx, utils.ErrGatewayTimeout)
		} else if errors.Is(lastErr, fasthttp.ErrConnectionClosed) {
			upstreamStatus = 502
			utils.SendErrorWithDetail(ctx, utils.ErrBadGateway, "upstream connection closed")
		} else {
			upstreamStatus = 502
			utils.SendError(ctx, utils.ErrBadGateway)
		}
	} else {
		upstreamAddr = "FAILED"
		upstreamStatus = 502
		utils.SendErrorWithDetail(ctx, utils.ErrBadGateway, "all upstreams failed")
	}
}

// selectTarget 使用配置的负载均衡器选择后端目标。
//
// 选择优先级：
//  1. 如果启用了 Lua balancer，先尝试 Lua 脚本选择
//  2. Lua 选择失败时，使用 fallback 算法
//  3. 否则使用传统负载均衡算法
//
// 参数：
//   - ctx: FastHTTP 请求上下文，用于提取客户端 IP 等信息
//
// 返回值：
//   - *loadbalance.Target: 选中的后端目标，无可用目标时返回 nil
func (p *Proxy) selectTarget(ctx *fasthttp.RequestCtx) *loadbalance.Target {
	p.mu.RLock()
	targets := p.targets
	p.mu.RUnlock()

	if len(targets) == 0 {
		return nil
	}

	// 检查是否启用 Lua balancer
	if p.config.BalancerByLua.Enabled && p.config.BalancerByLua.Script != "" && p.luaEngine != nil {
		target, err := p.selectByLua(ctx, targets)
		if err != nil {
			logging.Warn().Err(err).Msg("lua balancer failed, using fallback")
			// Lua 失败，使用 fallback 算法
			return p.selectByFallback(ctx, targets)
		}
		if target != nil {
			return target
		}
		// Lua 未调用 set_current_peer，使用 fallback
		logging.Debug().Msg("lua balancer did not select target, using fallback")
		return p.selectByFallback(ctx, targets)
	}

	// 使用传统负载均衡算法
	return p.selectByBalancer(ctx, targets)
}

// isWebSocketRequest 检查请求是否为 WebSocket 升级请求。
//
// 通过检查 Connection 和 Upgrade 请求头判断：
//   - Connection 头需包含 "upgrade"（不区分大小写）
//   - Upgrade 头需等于 "websocket"（不区分大小写）
//
// 参数：
//   - ctx: FastHTTP 请求上下文
//
// 返回值：
//   - bool: true 表示是 WebSocket 升级请求
func isWebSocketRequest(ctx *fasthttp.RequestCtx) bool {
	connection := ctx.Request.Header.Peek("Connection")
	if len(connection) == 0 {
		return false
	}
	upgradeBytes := []byte("upgrade")
	if !bytes.EqualFold(connection, upgradeBytes) &&
		!utils.BytesContainsFold(connection, upgradeBytes) {
		return false
	}

	upgrade := ctx.Request.Header.Peek("Upgrade")
	return bytes.EqualFold(upgrade, []byte("websocket"))
}
