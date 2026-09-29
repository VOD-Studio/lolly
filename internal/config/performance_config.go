// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 包含性能配置相关的结构体，用于控制服务器性能优化参数。
//
// 作者：xfy
package config

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// HTTP2Config HTTP/2 配置。
//
// HTTP/2 提供多路复用、头部压缩和服务器推送等功能，
// 默认需要服务器配置 SSL/TLS 证书才能正常工作。
//
// 注意事项：
//   - 必须配置有效的 SSL 证书（TLS 1.2 或更高版本）
//   - 客户端可以通过 ALPN 协商使用 HTTP/2 或 HTTP/1.1
//   - 明文 HTTP/2（h2c）需 enabled 与 h2c_enabled 同时为 true，
//     且仅在不配置证书的监听器上生效，支持 prior knowledge 与 Upgrade: h2c
//
// 使用示例：
//
//	server:
//	  ssl:
//	    cert: "/etc/ssl/server.crt"
//	    key: "/etc/ssl/server.key"
//	    http2:
//	      enabled: true
//	      max_concurrent_streams: 128
//	      max_header_list_size: "16KB"
//
// # 明文监听器上的 h2c（服务间调用、内部负载均衡）
//
//	server:
//	  listen: "127.0.0.1:8080"
//	  ssl:
//	    http2:
//	      enabled: true
//	      h2c_enabled: true
type HTTP2Config struct {
	MaxConcurrentStreams    int           `yaml:"max_concurrent_streams"`
	MaxHeaderListSize       int           `yaml:"max_header_list_size"`
	IdleTimeout             time.Duration `yaml:"idle_timeout"`
	Enabled                 bool          `yaml:"enabled"`
	PushEnabled             bool          `yaml:"push_enabled"`
	H2CEnabled              bool          `yaml:"h2c_enabled"`
	GracefulShutdownTimeout time.Duration `yaml:"graceful_shutdown_timeout"`
	MaxBodySize             int64         `yaml:"max_body_size,omitempty"`
	StreamRequestBody       bool          `yaml:"stream_request_body,omitempty"`
}

// HTTP3Config HTTP/3 (QUIC) 配置。
//
// HTTP/3 基于 QUIC 协议，提供更快的连接建立和更低的延迟。
// 需要服务器配置 SSL/TLS 证书才能正常工作。
//
// 注意事项：
//   - 必须配置有效的 SSL 证书
//   - UDP 监听地址不能与 HTTP/1.1 或 HTTP/2 冲突
//   - 0-RTT 特性可能带来重放攻击风险，需评估安全性
//   - 部分网络环境可能限制 UDP 流量
//
// 使用示例：
//
//	http3:
//	  enabled: true
//	  listen: ":443"
//	  max_streams: 1000
//	  idle_timeout: 30s
//	  enable_0rtt: true
type HTTP3Config struct {
	Listen            string        `yaml:"listen"`
	MaxStreams        int           `yaml:"max_streams"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	Enabled           bool          `yaml:"enabled"`
	Enable0RTT        bool          `yaml:"enable_0rtt"`
	MaxBodySize       int64         `yaml:"max_body_size,omitempty"`
	StreamRequestBody bool          `yaml:"stream_request_body,omitempty"`
}

// PerformanceConfig 性能配置。
//
// 配置服务器性能优化相关参数。
//
// 注意事项：
//   - GoroutinePool 复用 goroutine 减少创建开销
//   - FileCache 缓存静态文件内容提升响应速度
//   - Transport 配置代理连接的连接池参数
//
// 使用示例：
//
//	performance:
//	  goroutine_pool:
//	    enabled: true
//	    max_workers: 1000
//	  file_cache:
//	    max_entries: 10000
//	    max_size: 1073741824
//	  transport:
//	    max_idle_conns: 100
type PerformanceConfig struct {
	// GoroutinePool Goroutine 池配置
	// 控制 worker goroutine 的复用行为
	GoroutinePool GoroutinePoolConfig `yaml:"goroutine_pool"`

	// FileCache 文件缓存配置
	// 缓存静态文件内容避免重复磁盘 IO
	FileCache FileCacheConfig `yaml:"file_cache"`

	// Transport HTTP Transport 配置
	// 代理连接池的参数设置
	Transport TransportConfig `yaml:"transport"`
}

// GoroutinePoolConfig Goroutine 池配置。
//
// 复用 goroutine 减少创建和销毁开销。
//
// 注意事项：
//   - Enabled 为 true 时启用 goroutine 池
//   - MaxWorkers 限制最大并发 worker 数
//   - MinWorkers 预热 worker 数量
//   - IdleTimeout 空闲 worker 回收时间
//
// 使用示例：
//
//	goroutine_pool:
//	  enabled: true
//	  max_workers: 1000
//	  min_workers: 100
//	  idle_timeout: 60s
type GoroutinePoolConfig struct {
	// Enabled 是否启用
	Enabled bool `yaml:"enabled"`

	// MaxWorkers 最大 worker 数
	// 限制同时运行的最大 goroutine 数量
	MaxWorkers int `yaml:"max_workers"`

	// MinWorkers 最小 worker 数（预热）
	// 启动时预创建的 goroutine 数量
	MinWorkers int `yaml:"min_workers"`

	// IdleTimeout 空闲超时
	// 空闲 worker 超过此时间将被回收
	IdleTimeout time.Duration `yaml:"idle_timeout"`
}

// TransportConfig HTTP Transport 配置。
//
// 配置代理后端连接的连接池参数。
//
// 注意事项：
//   - IdleConnTimeout 控制空闲连接的保持时间
//   - MaxConnsPerHost 限制每个后端主机的总连接数（含活跃和空闲）
//
// 使用示例：
//
//	transport:
//	  idle_conn_timeout: 90s
//	  max_conns_per_host: 100
type TransportConfig struct {
	// IdleConnTimeout 空闲连接超时
	// 空闲连接的最大存活时间
	IdleConnTimeout time.Duration `yaml:"idle_conn_timeout"`

	// MaxConnsPerHost 每主机最大连接数
	// 单个后端主机的总连接数上限（包括活跃连接和空闲连接）
	MaxConnsPerHost int `yaml:"max_conns_per_host"`
}

// ShutdownConfig 服务器关闭配置。
//
// 用于配置服务器在接收到不同信号时的关闭超时行为。
// 优雅停止会等待正在处理的请求完成，快速停止会立即中断连接。
//
// 注意事项：
//   - graceful_timeout = 0 表示使用默认值（30s）
//   - fast_timeout = 0 表示使用默认值（5s）
//   - graceful_timeout 应显著大于 fast_timeout
//   - 两个值都必须 >= 0，负数在验证时会报错
//
// 使用示例：
//
//	shutdown:
//	  graceful_timeout: 30s   # SIGQUIT 优雅停止超时
//	  fast_timeout: 5s        # SIGINT/SIGTERM 快速停止超时
type ShutdownConfig struct {
	// GracefulTimeout 优雅停止超时（SIGQUIT）
	// 接收到 SIGQUIT 信号后，等待活跃请求完成的最大时间
	// 默认: 30s（当值为 0 时使用默认值）
	GracefulTimeout time.Duration `yaml:"graceful_timeout"`

	// FastTimeout 快速停止超时（SIGINT/SIGTERM）
	// 接收到 SIGINT 或 SIGTERM 信号后，等待服务器关闭的最大时间
	// 默认: 5s（当值为 0 时使用默认值）
	FastTimeout time.Duration `yaml:"fast_timeout"`

	// ReloadTimeout 热重载启动等待超时（SIGHUP）
	// 等待新服务器启动完成的最大时间，超时后视为启动成功
	// 默认: 5s（当值为 0 时使用默认值）
	ReloadTimeout time.Duration `yaml:"reload_timeout"`
}

// ResolverConfig DNS 解析器配置。
//
// 配置 DNS 解析器的行为，包括服务器地址、缓存 TTL、超时等。
// 启用后可实现动态 DNS 解析和缓存，支持后端域名的动态解析。
//
// 注意事项：
//   - Enabled 为 true 时启用 DNS 解析器
//   - Addresses 配置 DNS 服务器地址，如 "8.8.8.8:53"
//   - Valid 为缓存有效期（TTL），建议 30s-300s
//   - Timeout 为单次查询超时时间
//
// 使用示例：
//
//	resolver:
//	  enabled: true
//	  addresses:
//	    - "8.8.8.8:53"
//	    - "8.8.4.4:53"
//	  valid: 30s
//	  timeout: 5s
//	  ipv4: true
//	  ipv6: false
//	  cache_size: 1024
type ResolverConfig struct {
	Addresses []string      `yaml:"addresses"`
	Valid     time.Duration `yaml:"valid"`
	Timeout   time.Duration `yaml:"timeout"`
	CacheSize int           `yaml:"cache_size"`
	Enabled   bool          `yaml:"enabled"`
	IPv4      bool          `yaml:"ipv4"`
	IPv6      bool          `yaml:"ipv6"`
}

// TTL 返回缓存有效期（Valid 的别名，便于代码理解）。
func (c *ResolverConfig) TTL() time.Duration {
	return c.Valid
}

// Validate 验证 Resolver 配置。
//
// 检查 DNS 服务器地址格式、TTL 和超时设置的有效性。
//
// 返回值：
//   - error: 验证失败时的错误信息
func (c *ResolverConfig) Validate() error {
	if !c.Enabled {
		return nil
	}

	if len(c.Addresses) == 0 {
		return errors.New("resolver.addresses is required when enabled")
	}

	for _, addr := range c.Addresses {
		if _, err := net.ResolveUDPAddr("udp", addr); err != nil {
			return fmt.Errorf("invalid DNS address %s: %w", addr, err)
		}
	}

	if c.Valid > 0 && c.Valid < time.Second {
		return errors.New("resolver.valid must be at least 1s")
	}

	if c.Timeout > 0 && c.Timeout < time.Second {
		return errors.New("resolver.timeout must be at least 1s")
	}

	if !c.IPv4 && !c.IPv6 {
		return errors.New("at least one of ipv4 or ipv6 must be enabled")
	}

	return nil
}
