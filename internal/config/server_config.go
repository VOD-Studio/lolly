// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 包含服务器配置相关的结构体，用于配置监听地址、静态文件、代理、SSL 等设置。
//
// 作者：xfy
package config

import "time"

// ServerConfig 服务器配置，包含监听地址、静态文件、代理、SSL 等设置。
//
// 用于定义单个服务器的完整行为，包括网络监听、请求处理、
// 安全防护和性能控制等方面。
//
// 注意事项：
//   - Listen 字段为必填项，格式为 "host:port" 或 ":port"
//   - Name 字段用于虚拟主机匹配，多服务器模式下建议配置
//   - SSL 配置为可选，但生产环境强烈建议启用
//   - 超时设置需根据实际业务场景调整
//
// 使用示例：
//
//	server:
//	  listen: ":8080"
//	  name: "api.example.com"
//	  read_timeout: 30s
//	  write_timeout: 30s
type ServerConfig struct {
	// 指针类型字段（按大小排列，减少 padding）
	CacheAPI *CacheAPIConfig      `yaml:"cache_api"`
	Lua      *LuaMiddlewareConfig `yaml:"lua"`
	// 切片字段
	Static  []StaticConfig `yaml:"static"`
	Proxy   []ProxyConfig  `yaml:"proxy"`
	Rewrite []RewriteRule  `yaml:"rewrite"`
	// 字符串字段
	ClientMaxBodySize string `yaml:"client_max_body_size"`
	Name              string `yaml:"name"`
	Listen            string `yaml:"listen"`
	// 结构体字段（嵌入类型）
	Security    SecurityConfig    `yaml:"security"`
	Compression CompressionConfig `yaml:"compression"`
	SSL         SSLConfig         `yaml:"ssl"`
	UnixSocket  UnixSocketConfig  `yaml:"unix_socket"` // Unix socket 配置
	LimitRate   LimitRateConfig   `yaml:"limit_rate"`  // 响应速率限制配置
	Types       TypesConfig       `yaml:"types"`       // MIME 类型配置
	// 切片字段
	ServerNames []string `yaml:"server_names"` // 支持多个 server_name
	// time.Duration 字段（int64）
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	// 基本类型字段（int 按大小排列）
	MaxRequestsPerConn int `yaml:"max_requests_per_conn"`
	MaxConnsPerIP      int `yaml:"max_conns_per_ip"`
	Concurrency        int `yaml:"concurrency"`       // 最大并发连接数（默认 256 * 1024）
	ReadBufferSize     int `yaml:"read_buffer_size"`  // 读缓冲区大小（字节，默认 16KB）
	WriteBufferSize    int `yaml:"write_buffer_size"` // 写缓冲区大小（字节，默认 16KB）
	// 布尔字段（放在一起减少 padding）
	Default           bool `yaml:"default,omitempty"`   // VHost 默认主机标记
	ReduceMemoryUsage bool `yaml:"reduce_memory_usage"` // 是否优先减少内存使用（默认 false，优先性能）
	ServerTokens      bool `yaml:"server_tokens"`       // false 隐藏版本号，默认 true（零值表示显示版本）
}

// UsesTLS 判断服务器是否要求监听器执行 TLS 握手。
//
// 返回值：
//   - bool: 配置了静态证书、ACME 或拒绝握手时返回 true
func (s *ServerConfig) UsesTLS() bool {
	return (s.SSL.Cert != "" && s.SSL.Key != "") || s.SSL.ACME.Enabled || s.SSL.RejectHandshake
}

// EffectiveServerNames 返回参与 Host 和 SNI 匹配的主机名。
//
// 返回值：
//   - []string: server_names，未配置时回退到非空 name
func (s *ServerConfig) EffectiveServerNames() []string {
	if len(s.ServerNames) > 0 {
		return s.ServerNames
	}
	if s.Name != "" {
		return []string{s.Name}
	}
	return nil
}

// StaticConfig 静态文件服务配置。
//
// 用于配置静态文件服务器的行为，包括路径匹配、根目录和索引文件。
//
// 注意事项：
//   - Path 为路径前缀，匹配的请求将被该静态处理器处理
//   - Root 路径可以是相对路径或绝对路径
//   - 索引文件按顺序查找，第一个存在的文件将被使用
//   - 目录路径需要确保有读取权限
//
// 使用示例：
//
//	static:
//	  - path: "/"
//	    root: "/var/www/html"
//	    index: ["index.html", "index.htm"]
//	  - path: "/assets/"
//	    root: "/var/www/assets"
type StaticConfig struct {
	// Path 匹配路径前缀
	// 以此前缀开头的请求将被该静态处理器处理
	// 默认为 "/"，匹配所有路径
	Path string `yaml:"path"`

	// Root 静态文件根目录
	// 所有静态文件请求都将以此目录为基础解析
	// 请求路径追加到 root 后面
	// 示例: root=/var/www, path=/static/ → /static/img.png → /var/www/static/img.png
	Root string `yaml:"root"`

	// Alias 替换路径（与 root 互斥）
	// 将 location 路径替换为 alias 路径（nginx alias 语义）
	// 示例: alias=/var/www/files/, path=/images/ → /images/logo.png → /var/www/files/logo.png
	Alias string `yaml:"alias"`

	// Index 索引文件列表
	// 访问目录时依次查找这些文件作为默认页面
	// 默认为 ["index.html", "index.htm"]
	Index []string `yaml:"index"`

	// TryFiles 按顺序尝试查找的文件列表
	// 支持以下模式：
	//   - $uri: 请求路径
	//   - $uri/: 请求路径加斜杠（目录）
	//   - $uri.<ext>: 请求路径加扩展名（如 $uri.html, $uri.json）
	//   - /path: 绝对路径回退（如 /index.html）
	//   - filename: 相对路径回退（如 fallback.html）
	//
	// nginx 兼容性：
	//   - $uri 变量语义与 nginx try_files 指令一致
	//   - 配置语法可从 nginx 直接迁移
	//
	// 安全限制（附加于 nginx 基础）：
	//   - 扩展名仅允许字母、数字、点、下划线、连字符
	//   - 禁止危险后缀（.php, .exe, .bat 等）
	//   - 禁止 null byte 和路径分隔符
	//
	// 根路径边界情况：
	//   - 当 relPath="/" 且模式为 "$uri.<ext>" 时，返回空字符串
	//   - 此设计避免生成 "/.html" 这样的隐藏文件名
	//   - 建议使用绝对路径回退（如 /index.html）处理根路径
	//
	// 示例:
	//   try_files: ["$uri", "$uri.html", "/index.html"]
	//   try_files: ["$uri", "$uri/", "/app.html"]
	TryFiles []string `yaml:"try_files"`

	// TryFilesPass 内部重定向是否触发中间件
	// 默认为 false，内部重定向不触发中间件
	// 设置为 true 时，try_files 回退会重新进入中间件链
	TryFilesPass bool `yaml:"try_files_pass"`

	// SymlinkCheck 是否启用符号链接安全检查
	// 默认为 false，启用后会验证符号链接指向的文件是否在允许的路径范围内
	// 防止通过符号链接访问敏感文件（如 /etc/passwd）
	SymlinkCheck bool `yaml:"symlink_check"`

	// LocationType 位置匹配类型
	// 可选值：exact、prefix、regex、regex_caseless、prefix_priority、named
	LocationType string `yaml:"location_type"`

	// Internal 仅允许内部访问
	// 设置为 true 时，该位置仅允许内部重定向访问
	Internal bool `yaml:"internal"`

	// Expires 缓存过期时间
	// 支持 nginx 兼容格式：30d, 1h, 1m, max, epoch, off
	// 设置 Cache-Control: max-age 和 Expires 响应头
	// 示例：expires: 30d → Cache-Control: max-age=2592000
	Expires string `yaml:"expires"`

	// AutoIndex 是否启用目录列表
	// 当请求目录且没有索引文件时，生成目录列表页面
	// 默认为 false，返回 403 Forbidden
	AutoIndex bool `yaml:"auto_index"`

	// AutoIndexFormat 目录列表输出格式
	// 可选值：html（默认）、json、xml
	AutoIndexFormat string `yaml:"auto_index_format"`

	// AutoIndexLocaltime 是否使用本地时间
	// 默认为 false，使用 GMT 时间
	AutoIndexLocaltime bool `yaml:"auto_index_localtime"`

	// AutoIndexExactSize 是否显示精确文件大小
	// 默认为 false，显示人类可读格式（K/M/G）
	AutoIndexExactSize bool `yaml:"auto_index_exact_size"`
}

// TypesConfig MIME 类型配置
//
// 用于配置静态文件的 MIME 类型映射。
//
// 注意事项：
//   - DefaultType 为默认 MIME 类型
//   - Map 为扩展名到 MIME 类型的映射
//
// 使用示例：
//
//	types:
//	  default_type: "application/octet-stream"
//	  map:
//	    ".html": "text/html"
//	    ".css": "text/css"
//	    ".js": "application/javascript"
type TypesConfig struct {
	// DefaultType 默认 MIME 类型
	// 当无法识别文件扩展名时使用
	DefaultType string `yaml:"default_type"`

	// Map 扩展名到 MIME 类型的映射
	// 键为文件扩展名（如 ".html"），值为 MIME 类型
	Map map[string]string `yaml:"map"`
}

// UnixSocketConfig Unix socket 特定配置。
//
// 用于配置服务器监听 Unix domain socket 时的文件权限和所有权。
//
// 注意事项：
//   - Mode 为 socket 文件权限，默认 0666
//   - User 为 socket 文件所有者用户名
//   - Group 为 socket 文件所属用户组
//
// 使用示例：
//
//	unix_socket:
//	  mode: 0660
//	  user: "www-data"
//	  group: "www-data"
type UnixSocketConfig struct {
	// Mode 文件权限
	// Unix socket 文件的访问权限，默认 0666
	Mode int `yaml:"mode"`

	// User 文件所有者
	// Unix socket 文件的所有者用户名
	User string `yaml:"user"`

	// Group 文件组
	// Unix socket 文件的所属用户组
	Group string `yaml:"group"`
}

// StreamConfig TCP/UDP Stream 代理配置。
//
// 用于四层网络代理，如数据库、Redis 等 TCP/UDP 服务。
//
// 注意事项：
//   - Listen 配置监听地址
//   - Protocol 支持 tcp 或 udp
//   - Upstream 配置后端目标列表
//   - Stream 代理工作在传输层，不解析应用层协议
//
// 使用示例：
//
//	stream:
//	  - listen: ":3306"
//	    protocol: "tcp"
//	    upstream:
//	      targets:
//	        - addr: "mysql1:3306"
//	          weight: 3
//	        - addr: "mysql2:3306"
//	          weight: 1
//	      load_balance: "round_robin"
type StreamConfig struct {
	Listen   string               `yaml:"listen"`
	Protocol string               `yaml:"protocol"`
	Upstream StreamUpstream       `yaml:"upstream"`
	ProxySSL StreamProxySSLConfig `yaml:"proxy_ssl"`
	SSL      StreamSSLConfig      `yaml:"ssl"`
}

// StreamUpstream Stream 上游配置。
//
// 配置 Stream 代理的后端服务器列表。
//
// 注意事项：
//   - Targets 配置后端服务器地址
//   - LoadBalance 配置负载均衡算法
//
// 使用示例：
//
//	upstream:
//	  targets:
//	    - addr: "backend1:3306"
//	      weight: 3
//	  load_balance: "round_robin"
type StreamUpstream struct {
	LoadBalance string         `yaml:"load_balance"`
	Targets     []StreamTarget `yaml:"targets"`
}

// StreamTarget Stream 目标配置。
//
// 定义单个 Stream 后端服务器。
//
// 注意事项：
//   - Addr 为后端服务器地址
//   - Weight 在加权轮询算法下生效
//
// 使用示例：
//
//	targets:
//	  - addr: "mysql1:3306"
//	    weight: 3
//	  - addr: "mysql2:3306"
//	    weight: 1
type StreamTarget struct {
	// Addr 目标地址
	// 后端服务器地址，如 "host:port"
	Addr string `yaml:"addr"`

	// Weight 权重
	// 用于加权轮询负载均衡
	Weight int `yaml:"weight"`
}

// StreamSSLConfig Stream SSL 服务端配置。
//
// 配置 Stream 模块的 TLS 终端功能，用于加密 TCP 流量。
//
// 注意事项：
//   - 仅对 TCP 协议有效，UDP 不支持 TLS
//   - 证书文件需要 PEM 格式
//   - 支持配置客户端证书验证（mTLS）
//
// 使用示例：
//
//	stream:
//	  - listen: ":3306"
//	    protocol: "tcp"
//	    ssl:
//	      enabled: true
//	      cert: "/etc/ssl/server.crt"
//	      key: "/etc/ssl/server.key"
//	    upstream:
//	      targets:
//	        - addr: "mysql:3306"
type StreamSSLConfig struct {
	Cert        string   `yaml:"cert"`
	Key         string   `yaml:"key"`
	ClientCA    string   `yaml:"client_ca"`
	Protocols   []string `yaml:"protocols"`
	Ciphers     []string `yaml:"ciphers"`
	VerifyDepth int      `yaml:"verify_depth"`
	Enabled     bool     `yaml:"enabled"`
}

// StreamProxySSLConfig Stream 上游 SSL 配置。
//
// 配置到上游服务器的 TLS 连接，用于加密代理到后端的流量。
//
// 注意事项：
//   - 启用后，代理将使用 TLS 连接到上游
//   - 支持客户端证书（mTLS）和服务器证书验证
//   - ServerName 用于 SNI 和证书验证
//
// 使用示例：
//
//	stream:
//	  - listen: ":3306"
//	    protocol: "tcp"
//	    proxy_ssl:
//	      enabled: true
//	      verify: true
//	      trusted_ca: "/etc/ssl/ca.crt"
//	      server_name: "mysql.internal"
//	    upstream:
//	      targets:
//	        - addr: "mysql:3306"
type StreamProxySSLConfig struct {
	TrustedCA    string   `yaml:"trusted_ca"`
	ServerName   string   `yaml:"server_name"`
	Cert         string   `yaml:"cert"`
	Key          string   `yaml:"key"`
	Protocols    []string `yaml:"protocols"`
	Enabled      bool     `yaml:"enabled"`
	Verify       bool     `yaml:"verify"`
	SessionReuse bool     `yaml:"session_reuse"`
}
