// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 包含代理配置相关的结构体，用于配置反向代理行为。
//
// 作者：xfy
package config

import (
	"strconv"
	"strings"
	"time"
)

// ProxyConfig 反向代理配置，支持负载均衡和健康检查。
//
// 用于将请求转发到后端服务器，支持多种负载均衡算法
// 和健康检查机制。
//
// 注意事项：
//   - Path 使用前缀匹配，较长路径优先匹配
//   - 至少配置一个 Target 才能正常工作
//   - 负载均衡算法支持：round_robin、weighted_round_robin、least_conn、ip_hash、consistent_hash、random、least_time、sticky
//   - 一致性哈希需要配置 HashKey
//
// 使用示例：
//
//	proxy:
//	  - path: "/api/"
//	    targets:
//	      - url: "http://backend1:8080"
//	        weight: 3
//	      - url: "http://backend2:8080"
//	        weight: 1
//	    load_balance: "weighted_round_robin"
//	    health_check:
//	      interval: 10s
//	      path: "/health"
type ProxyConfig struct {
	// 指针类型字段（按大小排列）
	RedirectRewrite *RedirectRewriteConfig `yaml:"redirect_rewrite"`
	ProxySSL        *ProxySSLConfig        `yaml:"proxy_ssl"`
	CacheValid      *ProxyCacheValidConfig `yaml:"cache_valid"`
	Buffering       *ProxyBufferingConfig  `yaml:"buffering"`
	// 切片字段
	Targets []ProxyTarget `yaml:"targets"`
	// 字符串字段
	Path              string `yaml:"path"`
	LoadBalance       string `yaml:"load_balance"`
	HashKey           string `yaml:"hash_key"`
	ClientMaxBodySize string `yaml:"client_max_body_size"`
	ProxyBind         string `yaml:"proxy_bind"`
	// 结构体字段
	Headers       ProxyHeaders        `yaml:"headers"`
	BalancerByLua BalancerByLuaConfig `yaml:"balancer_by_lua"`
	HealthCheck   HealthCheckConfig   `yaml:"health_check"`
	NextUpstream  NextUpstreamConfig  `yaml:"next_upstream"`
	Cache         ProxyCacheConfig    `yaml:"cache"`
	Timeout       ProxyTimeout        `yaml:"timeout"`
	LeastTime     LeastTimeConfig     `yaml:"least_time"`
	Sticky        StickyConfig        `yaml:"sticky"`
	// 基本类型字段
	VirtualNodes int `yaml:"virtual_nodes"`

	// LocationType 位置匹配类型
	// 可选值：exact、prefix_priority、regex、regex_caseless、prefix、named
	LocationType string `yaml:"location_type"`

	// LocationName 位置名称
	// 仅当 LocationType 为 named 时使用，用于命名位置块
	LocationName string `yaml:"location_name"`

	// Internal 仅允许内部访问
	// 设置为 true 时，该位置仅允许内部重定向访问
	Internal bool `yaml:"internal"`
}

// ProxyBufferingConfig 代理缓冲配置。
//
// 控制代理的缓冲行为，分别独立控制请求体和响应体：
//   - 响应（Mode）："default"/"on" 缓冲到内存/临时文件，"off" 流式转发响应
//   - 请求（RequestMode）："default"/"on" 完整缓冲请求体后再转发，
//     "off" 流式转发请求体（等价 nginx proxy_request_buffering off）
//
// RequestMode=off 时：
//   - 请求体在 客户端→lolly→上游 全程不落全量内存，支持大上传低内存
//   - 带请求体的请求禁用 next_upstream 重试（流不可回放）
//   - $request_body 类变量为空（与 nginx 行为一致）
//
// 使用示例：
//
//	buffering:
//	  mode: "off"             # 响应流式
//	  request_mode: "off"     # 请求体流式
type ProxyBufferingConfig struct {
	// Mode 响应缓冲模式
	// 可选值："default"（默认缓冲）, "on"（强制缓冲）, "off"（关闭缓冲）
	Mode string `yaml:"mode"`

	// RequestMode 请求体缓冲模式
	// 可选值："default"（默认，完整缓冲）, "on"（强制完整缓冲）, "off"（流式转发）
	// 为空时按 "default" 处理，向后兼容
	RequestMode string `yaml:"request_mode"`

	// BufferSize 响应缓冲区大小（字节）
	// 0 表示使用默认值
	BufferSize int `yaml:"buffer_size"`

	// Buffers 多缓冲区配置字符串
	// 格式："数量 大小" 或 "数量1 大小1 数量2 大小2 ..."
	// 例如："8 16k" 表示 8 个 16KB 缓冲区
	// 例如："4 4k 8 16k" 表示 4 个 4KB + 8 个 16KB 缓冲区
	Buffers string `yaml:"buffers"`

	// BufferCount 缓冲区数量（解析后）
	BufferCount int `yaml:"-"`

	// BufferSizeEach 每个缓冲区大小（字节，解析后）
	BufferSizeEach int `yaml:"-"`
}

// RequestStreamingEnabled 报告是否启用了请求体流式转发。
//
// 仅当 RequestMode == "off" 时返回 true。
// Mode/on/空 都按完整缓冲处理。
//
// 返回值：
//   - bool: true 表示请求体应流式转发到上游
func (c *ProxyBufferingConfig) RequestStreamingEnabled() bool {
	return c != nil && c.RequestMode == "off"
}

// ParseBuffers 解析 Buffers 配置字符串。
//
// 支持格式：
//   - "8 16k" → 8 个 16KB 缓冲区
//   - "4 4k" → 4 个 4KB 缓冲区
//
// 大小单位：
//   - k 或 K: KB (1024 字节)
//   - m 或 M: MB (1024 * 1024 字节)
//   - 无单位: 字节
func (c *ProxyBufferingConfig) ParseBuffers() {
	if c.Buffers == "" {
		// 向后兼容：使用 BufferSize
		if c.BufferSize > 0 {
			c.BufferCount = 1
			c.BufferSizeEach = c.BufferSize
		}
		return
	}

	parts := strings.Fields(c.Buffers)
	if len(parts) < 2 {
		return // 无效格式
	}

	count, err := strconv.Atoi(parts[0])
	if err != nil || count <= 0 {
		return // 无效数量
	}

	sizeEach, err := parseSize(parts[1])
	if err != nil || sizeEach <= 0 {
		return // 无效大小
	}

	c.BufferCount = count
	c.BufferSizeEach = sizeEach
}

// BalancerByLuaConfig Lua 负载均衡配置
//
// 使用 Lua 脚本动态选择后端目标，支持自定义负载均衡逻辑。
//
// 注意事项：
//   - Script 为 Lua 脚本文件路径
//   - Timeout 控制脚本执行超时
//   - Fallback 指定 Lua 失败时的备用算法
//
// 使用示例：
//
//	balancer_by_lua:
//	  enabled: true
//	  script: "/etc/lolly/scripts/balancer.lua"
//	  timeout: 100ms
//	  fallback: "round_robin"
type BalancerByLuaConfig struct {
	// Script Lua 脚本路径
	Script string `yaml:"script"`

	// Fallback 失败时使用的默认负载均衡算法
	// 默认值: "round_robin"
	Fallback string `yaml:"fallback"`

	// Timeout 执行超时
	// 默认值: 100ms
	Timeout time.Duration `yaml:"timeout"`

	// Enabled 是否启用
	Enabled bool `yaml:"enabled"`
}

// ProxyTarget 后端目标配置。
//
// 定义单个后端服务器的地址和权重。
//
// 注意事项：
//   - URL 必须包含协议（http:// 或 https://）
//   - Weight 仅在 weighted_round_robin 算法下生效
//
// 使用示例：
//
//	targets:
//	  - url: "http://backend1:8080"
//	    weight: 3
//	  - url: "http://backend2:8080"
//	    weight: 1
type ProxyTarget struct {
	// URL 后端地址
	// 格式："http://host:port" 或 "https://host:port"
	URL string `yaml:"url"`

	// Weight 权重
	// 用于加权轮询算法，值越大分配的请求越多
	Weight int `yaml:"weight"`

	// MaxConns 最大并发连接数
	// 0 表示不限制
	MaxConns int `yaml:"max_conns"`

	// MaxFails 最大失败次数
	// 在 FailTimeout 期间失败次数达到此值后标记为不可用
	// 0 表示不进行被动失败检测
	MaxFails int `yaml:"max_fails"`

	// FailTimeout 失败超时时间
	// 达到 MaxFails 后，目标在此时间内被视为不可用
	FailTimeout time.Duration `yaml:"fail_timeout"`

	// Backup 备份服务器
	// 仅当所有非备份服务器不可用时才使用
	Backup bool `yaml:"backup"`

	// Down 标记服务器为永久不可用
	Down bool `yaml:"down"`

	// ProxyURI 代理传递的 URI 路径
	// 设置后替换请求路径，支持 nginx proxy_pass URI 语义
	ProxyURI string `yaml:"proxy_uri"`
}

// HealthCheckConfig 健康检查配置。
//
// 定期检查后端服务器的健康状态，自动剔除不健康的节点。
//
// 注意事项：
//   - Interval 不宜设置过小，避免增加后端负担
//   - Path 应该是轻量级的健康检查端点
//   - 超时时间应小于检查间隔
//
// 使用示例：
//
//	health_check:
//	  interval: 10s
//	  path: "/health"
//	  timeout: 5s
type HealthCheckConfig struct {
	Path      string             `yaml:"path"`
	Interval  time.Duration      `yaml:"interval"`
	Timeout   time.Duration      `yaml:"timeout"`
	Match     *HealthMatchConfig `yaml:"match"`      // 健康检查匹配配置
	SlowStart time.Duration      `yaml:"slow_start"` // 慢启动时间
}

// HealthMatchConfig 健康检查匹配配置。
type HealthMatchConfig struct {
	Status  []string          `yaml:"status"`  // 状态码范围列表
	Body    string            `yaml:"body"`    // 响应体正则表达式
	Headers map[string]string `yaml:"headers"` // 响应头匹配
}

// ProxyTimeout 代理超时配置。
//
// 控制代理请求的各个阶段超时。
//
// 注意事项：
//   - Connect 超时包括 DNS 解析和 TCP 连接建立
//   - Read 和 Write 超时分别控制响应读取和请求发送
//   - 超时时间需要根据后端服务响应时间调整
//
// 使用示例：
//
//	timeout:
//	  dial: 5s     # TCP 连接建立超时
//	  connect: 30s # 总连接超时（含 DNS/TLS）
//	  read: 30s
//	  write: 30s
type ProxyTimeout struct {
	// Dial TCP 连接建立超时
	// 建立 TCP 连接的超时时间（不含 DNS 和 TLS）
	Dial time.Duration `yaml:"dial"`

	// Connect 总连接超时
	// 从开始连接到连接可用的总超时时间（含 DNS 和 TLS）
	Connect time.Duration `yaml:"connect"`

	// Read 读取超时
	// 从后端读取响应的超时
	Read time.Duration `yaml:"read"`

	// Write 写入超时
	// 向后端发送请求的超时
	Write time.Duration `yaml:"write"`
}

// ProxyHeaders 代理请求/响应头配置。
//
// 在代理转发过程中修改 HTTP 头部。
//
// 注意事项：
//   - SetRequest 添加/修改发送到后端的请求头
//   - SetResponse 添加/修改返回给客户端的响应头
//   - Remove 会删除指定的请求头（在发送到后端之前）
//
// 使用示例：
//
//	headers:
//	  set_request:
//	    X-Forwarded-For: "$remote_addr"
//	    X-Real-IP: "$remote_addr"
//	  set_response:
//	    X-Proxy-By: "lolly"
//	  remove:
//	    - "X-Internal-Header"
type ProxyHeaders struct {
	// SetRequest 设置请求头
	// 发送到后端的请求中添加或覆盖的头部
	SetRequest map[string]string `yaml:"set_request"`

	// SetResponse 设置响应头
	// 返回给客户端的响应中添加或覆盖的头部
	SetResponse map[string]string `yaml:"set_response"`

	// Remove 移除的头部
	// 从发送到后端的请求中移除的头部列表
	Remove []string `yaml:"remove"`

	// HideResponse 隐藏的响应头
	// 从返回给客户端的响应中移除的头部列表
	HideResponse []string `yaml:"hide_response"`

	// PassResponse 允许传递的响应头
	// 仅传递列出的头部，其他全部隐藏（白名单模式）
	PassResponse []string `yaml:"pass_response"`

	// IgnoreHeaders 忽略的头部
	// 代理时完全忽略这些头部，不转发到后端也不返回给客户端
	IgnoreHeaders []string `yaml:"ignore_headers"`

	// CookieDomain Cookie 域重写
	// 将响应中 Set-Cookie 的 domain 替换为此值
	CookieDomain string `yaml:"cookie_domain"`

	// CookiePath Cookie 路径重写
	// 将响应中 Set-Cookie 的 path 替换为此值
	CookiePath string `yaml:"cookie_path"`

	// SetForwardedHost 控制 X-Forwarded-Host 头的设置
	// nil (默认): 设置 X-Forwarded-Host（向后兼容）
	// true: 显式设置 X-Forwarded-Host
	// false: 不设置 X-Forwarded-Host
	SetForwardedHost *bool `yaml:"set_forwarded_host"`

	// SetForwardedProto 控制 X-Forwarded-Proto 头的设置
	// nil (默认): 设置 X-Forwarded-Proto（向后兼容）
	// true: 显式设置 X-Forwarded-Proto
	// false: 不设置 X-Forwarded-Proto
	SetForwardedProto *bool `yaml:"set_forwarded_proto"`
}

// ProxySSLConfig 上游 SSL/TLS 配置。
//
// 配置代理连接上游服务器时的 TLS 行为，支持自定义 CA、客户端证书（mTLS）、
// SNI 和 TLS 版本控制。
//
// 注意事项：
//   - Enabled 为 true 时启用自定义 TLS 配置
//   - TrustedCA 用于验证上游服务器证书
//   - ClientCert + ClientKey 用于 mTLS 客户端认证
//   - InsecureSkipVerify 仅用于测试，生产环境禁用
//
// 使用示例：
//
//	proxy_ssl:
//	  enabled: true
//	  server_name: "api.internal"
//	  trusted_ca: "/etc/ssl/ca/upstream-ca.crt"
//	  client_cert: "/etc/ssl/client.crt"
//	  client_key: "/etc/ssl/client.key"
//	  min_version: "TLSv1.2"
type ProxySSLConfig struct {
	// 字符串字段
	ServerName string `yaml:"server_name"`
	TrustedCA  string `yaml:"trusted_ca"`
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
	MinVersion string `yaml:"min_version"`
	MaxVersion string `yaml:"max_version"`
	// 布尔字段
	Enabled            bool `yaml:"enabled"`
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// RedirectRewriteConfig Location/Refresh 头改写配置
//
// 用于配置代理响应中 Location 和 Refresh 头的改写行为。
//
// 注意事项：
//   - Mode 支持 "default"、"off"、"custom" 三种模式
//   - 未配置或空字符串时默认为 "default" 模式
//   - "custom" 模式必须配置至少一条规则
//
// 使用示例：
//
//	redirect_rewrite:
//	  mode: "default"  # 或 "off" 或 "custom"
//	  rules:
//	    - pattern: "http://backend:8000/"
//	      replacement: "$scheme://$host:$server_port/"
type RedirectRewriteConfig struct {
	// Mode 运行模式: "default" | "off" | "custom"
	// default: 自动从选中的 target URL 生成规则（运行时）
	// off: 禁用改写
	// custom: 使用 Rules 列表（预编译）
	// 未配置或空字符串时默认为 "default"
	Mode string `yaml:"mode"`

	// Rules 改写规则列表，仅在 Mode="custom" 时使用
	Rules []RedirectRewriteRule `yaml:"rules"`
}

// RedirectRewriteRule 单条改写规则
//
// 定义 Location/Refresh 头改写的匹配模式和替换目标。
//
// 注意事项：
//   - Pattern 以 ~ 开头表示正则，~* 表示大小写不敏感
//   - 无 ~ 前缀时使用前缀匹配语义
//   - Replacement 支持变量展开（$host, $scheme, $server_port 等）
//
// 使用示例：
//
//	rules:
//	  - pattern: "http://backend:8000/"
//	    replacement: "$scheme://$host:$server_port/"
//	  - pattern: "~^http://[^/]+:8000/(.*)$"
//	    replacement: "$scheme://$host/$1"
type RedirectRewriteRule struct {
	// Pattern 匹配模式，支持正则（以 ~ 开头）或精确匹配
	// 示例: "http://localhost:8000/" 或 "~^http://[^/]+:8000/"
	Pattern string `yaml:"pattern"`

	// Replacement 替换目标，支持变量展开
	// 示例: "$scheme://$host:$server_port/" 或 "/"
	Replacement string `yaml:"replacement"`
}

// LeastTimeConfig 最小时间负载均衡配置。
//
// 基于后端响应时间选择最优后端。
//
// 注意事项：
//   - Metric 决定时间统计方式
//   - DefaultTime 用于首次请求无样本时
//
// 使用示例：
//
//	least_time:
//	  metric: last_byte      # 指标类型（header | last_byte）
//	  default_time: 1ms      # 无统计样本时的默认响应时间
type LeastTimeConfig struct {
	// Metric 时间指标类型
	// 可选值：header（首字节时间）、last_byte（末字节时间）
	Metric string `yaml:"metric"`

	// DefaultTime 无统计样本时的默认响应时间
	// 用于首次请求或新后端加入时
	DefaultTime time.Duration `yaml:"default_time"`
}

// StickyConfig Session Sticky 配置。
//
// 通过 Cookie 将同一客户端固定到同一后端。
//
// 注意事项：
//   - Enabled 为 true 时启用
//   - FallbackAlgo 指定当无法找到 sticky 后端时使用的算法
//
// 使用示例：
//
//	sticky:
//	  enabled: true
//	  name: lolly_route     # cookie 名称
//	  expires: 1h          # session 有效期
//	  path: /             # cookie 路径
//	  http_only: true     # HttpOnly flag
//	  same_site: Lax      # SameSite 属性
//	  fallback_balance: round_robin  # fallback 算法
type StickyConfig struct {
	// Enabled 是否启用 Session Sticky
	Enabled bool `yaml:"enabled"`

	// Name Cookie 名称
	Name string `yaml:"name"`

	// Expires session 有效期
	Expires time.Duration `yaml:"expires"`

	// Domain Cookie 域
	Domain string `yaml:"domain"`

	// Path Cookie 路径
	Path string `yaml:"path"`

	// Secure Secure flag
	Secure bool `yaml:"secure"`

	// HttpOnly HttpOnly flag
	HttpOnly bool `yaml:"http_only"`

	// SameSite SameSite 属性
	// 可选值：Lax, Strict, None
	SameSite string `yaml:"same_site"`

	// FallbackAlgo 无法找到 sticky 后端时的 fallback 算法
	FallbackAlgo string `yaml:"fallback_balance"`
}

// NextUpstreamConfig 故障转移配置，定义后端失败时的自动重试行为。
//
// 当后端返回特定错误状态码或连接失败时，自动尝试下一个可用后端。
//
// 注意事项：
//   - Tries 为 1 时禁用故障转移
//   - 空 NextUpstream 使用默认值（Tries=1，禁用故障转移）
//   - 建议根据后端数量合理设置 Tries 值
//
// 使用示例：
//
//	next_upstream:
//	  tries: 3
//	  http_codes: [502, 503, 504]
type NextUpstreamConfig struct {
	HTTPCodes []int `yaml:"http_codes"`
	Tries     int   `yaml:"tries"`
}
