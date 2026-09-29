//go:build e2e

// Package testutil 提供 E2E 测试的工具函数。
//
// 包含动态配置生成器，支持编程方式生成 YAML 配置文件。
//
// 作者：xfy
package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"rua.plus/lolly/internal/config"
)

// ConfigBuilder 动态配置构建器。
//
// 支持编程方式生成 YAML 配置，用于 E2E 测试场景。
// 提供链式调用方式，方便组合不同配置。
//
// 使用示例：
//
//	cfg := testutil.NewConfigBuilder().
//	    WithServer(":8080").
//	    WithProxy("/api/", targets).
//	    WithSSL(certPath, keyPath).
//	    Build()
type ConfigBuilder struct {
	cfg *config.Config
}

// NewConfigBuilder 创建配置构建器。
func NewConfigBuilder() *ConfigBuilder {
	return &ConfigBuilder{
		cfg: &config.Config{
			Servers: []config.ServerConfig{},
		},
	}
}

// WithServer 添加服务器配置。
//
// 参数：
//   - listen: 监听地址，如 ":8080"
//
// 返回构建器以支持链式调用。
func (b *ConfigBuilder) WithServer(listen string) *ConfigBuilder {
	b.cfg.Servers = append(b.cfg.Servers, config.ServerConfig{
		Listen: listen,
	})
	return b
}

// WithServerConfig 添加完整服务器配置。
func (b *ConfigBuilder) WithServerConfig(server config.ServerConfig) *ConfigBuilder {
	b.cfg.Servers = append(b.cfg.Servers, server)
	return b
}

// ProxyTargetOption 代理目标选项。
type ProxyTargetOption func(*config.ProxyTarget)

// WithWeight 设置权重。
func WithWeight(weight int) ProxyTargetOption {
	return func(t *config.ProxyTarget) {
		t.Weight = weight
	}
}

// WithMaxConns 设置最大连接数。
func WithMaxConns(maxConns int) ProxyTargetOption {
	return func(t *config.ProxyTarget) {
		t.MaxConns = maxConns
	}
}

// WithMaxFails 设置最大失败次数。
func WithMaxFails(maxFails int, failTimeout time.Duration) ProxyTargetOption {
	return func(t *config.ProxyTarget) {
		t.MaxFails = maxFails
		t.FailTimeout = failTimeout
	}
}

// WithBackup 设置为备份服务器。
func WithBackup() ProxyTargetOption {
	return func(t *config.ProxyTarget) {
		t.Backup = true
	}
}

// ProxyOption 代理配置选项。
type ProxyOption func(*config.ProxyConfig)

// ProxyConfig 代理配置类型别名。
type ProxyConfig = config.ProxyConfig

// WithLoadBalance 设置负载均衡算法。
func WithLoadBalance(algorithm string) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.LoadBalance = algorithm
	}
}

// WithHealthCheck 设置健康检查。
func WithHealthCheck(path string, interval, timeout time.Duration) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.HealthCheck = config.HealthCheckConfig{
			Path:     path,
			Interval: interval,
			Timeout:  timeout,
		}
	}
}

// WithProxyTimeout 设置代理超时。
func WithProxyTimeout(connect, read, write time.Duration) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.Timeout = config.ProxyTimeout{
			Connect: connect,
			Read:    read,
			Write:   write,
		}
	}
}

// WithProxyHeaders 设置代理头部。
func WithProxyHeaders(setRequest, setResponse map[string]string) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.Headers = config.ProxyHeaders{
			SetRequest:  setRequest,
			SetResponse: setResponse,
		}
	}
}

// WithProxyCache 设置代理缓存。
func WithProxyCache(maxAge time.Duration, cacheLock bool) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.Cache = config.ProxyCacheConfig{
			Enabled:   true,
			MaxAge:    maxAge,
			CacheLock: cacheLock,
		}
	}
}

// WithProxySSL 设置上游 SSL。
func WithProxySSL(serverName string, insecureSkipVerify bool) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.ProxySSL = &config.ProxySSLConfig{
			Enabled:            true,
			ServerName:         serverName,
			InsecureSkipVerify: insecureSkipVerify,
		}
	}
}

// WithProxyBuffering 设置代理缓冲。
func WithProxyBuffering(mode string, bufferSize int) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.Buffering = &config.ProxyBufferingConfig{
			Mode:       mode,
			BufferSize: bufferSize,
		}
	}
}

// WithProxyRequestBuffering 设置请求体缓冲模式。
//
// mode 为 "off" 时启用请求体流式转发（等价 nginx proxy_request_buffering off）。
func WithProxyRequestBuffering(mode string) ProxyOption {
	return func(p *config.ProxyConfig) {
		if p.Buffering == nil {
			p.Buffering = &config.ProxyBufferingConfig{}
		}
		p.Buffering.RequestMode = mode
	}
}

// WithProxyClientMaxBodySize 设置代理路径的请求体大小限制。
//
// 与 nginx client_max_body_size 语义一致：超限返回 413，"0" 表示不限制。
func WithProxyClientMaxBodySize(size string) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.ClientMaxBodySize = size
	}
}

// WithProxyNextUpstream 设置故障转移。
func WithProxyNextUpstream(tries int, httpCodes []int) ProxyOption {
	return func(p *config.ProxyConfig) {
		p.NextUpstream = config.NextUpstreamConfig{
			Tries:     tries,
			HTTPCodes: httpCodes,
		}
	}
}

// WithProxy 添加代理配置。
//
// 参数：
//   - path: 代理路径前缀
//   - urls: 后端 URL 列表
//   - opts: 可选配置选项
//
// 返回构建器以支持链式调用。
func (b *ConfigBuilder) WithProxy(path string, urls []string, opts ...ProxyOption) *ConfigBuilder {
	if len(b.cfg.Servers) == 0 {
		b.WithServer(":8080")
	}

	targets := make([]config.ProxyTarget, len(urls))
	for i, url := range urls {
		targets[i] = config.ProxyTarget{
			URL: url,
		}
	}

	proxy := config.ProxyConfig{
		Path:    path,
		Targets: targets,
	}

	for _, opt := range opts {
		opt(&proxy)
	}

	// 添加到第一个服务器
	b.cfg.Servers[0].Proxy = append(b.cfg.Servers[0].Proxy, proxy)
	return b
}

// WithProxyTargets 添加带选项的代理配置。
//
// targetOptsPerTarget 是每个目标的选项列表（索引对应 urls）。
// opts 是代理级别的选项。
func (b *ConfigBuilder) WithProxyTargets(path string, urls []string, targetOptsPerTarget [][]ProxyTargetOption, opts ...ProxyOption) *ConfigBuilder {
	if len(b.cfg.Servers) == 0 {
		b.WithServer(":8080")
	}

	targets := make([]config.ProxyTarget, len(urls))
	for i, url := range urls {
		targets[i] = config.ProxyTarget{
			URL: url,
		}
		// 应用该目标的选项
		if i < len(targetOptsPerTarget) {
			for _, opt := range targetOptsPerTarget[i] {
				opt(&targets[i])
			}
		}
	}

	proxy := config.ProxyConfig{
		Path:    path,
		Targets: targets,
	}

	for _, opt := range opts {
		opt(&proxy)
	}

	b.cfg.Servers[0].Proxy = append(b.cfg.Servers[0].Proxy, proxy)
	return b
}

// SSLOption SSL 配置选项。
type SSLOption func(*config.SSLConfig)

// WithHTTP2 启用 HTTP/2。
func WithHTTP2(enabled bool, maxConcurrentStreams int) SSLOption {
	return func(s *config.SSLConfig) {
		s.HTTP2 = config.HTTP2Config{
			Enabled:              enabled,
			MaxConcurrentStreams: maxConcurrentStreams,
		}
	}
}

// WithH2C 在明文监听器上启用 HTTP/2（h2c，无证书）。
//
// enabled 与 h2c_enabled 需同时为 true 才会挂载嗅探分派；该选项刻意不设置
// 证书，用于验证"无 SSL 时 h2c 放行"这条配置校验路径与容器内真实行为。
//
// 参数：
//   - maxConcurrentStreams: 最大并发流数，0 使用服务器默认值
//
// 返回构建器以支持链式调用。
func (b *ConfigBuilder) WithH2C(maxConcurrentStreams int) *ConfigBuilder {
	if len(b.cfg.Servers) == 0 {
		b.WithServer(":8080")
	}
	b.cfg.Servers[0].SSL.HTTP2 = config.HTTP2Config{
		Enabled:              true,
		H2CEnabled:           true,
		MaxConcurrentStreams: maxConcurrentStreams,
	}
	return b
}

// WithTLSProtocols 设置 TLS 协议版本。
func WithTLSProtocols(protocols []string) SSLOption {
	return func(s *config.SSLConfig) {
		s.Protocols = protocols
	}
}

// WithSessionTickets 启用 Session Tickets。
func WithSessionTickets(enabled bool) SSLOption {
	return func(s *config.SSLConfig) {
		s.SessionTickets = config.SessionTicketsConfig{
			Enabled: enabled,
		}
	}
}

// WithHSTS 配置 HSTS。
func WithHSTS(maxAge int, includeSubDomains bool) SSLOption {
	return func(s *config.SSLConfig) {
		s.HSTS = config.HSTSConfig{
			MaxAge:            maxAge,
			IncludeSubDomains: includeSubDomains,
		}
	}
}

// WithSSL 配置 SSL/TLS。
//
// 参数：
//   - cert: 证书文件路径
//   - key: 私钥文件路径
//   - opts: 可选 SSL 配置
//
// 返回构建器以支持链式调用。
func (b *ConfigBuilder) WithSSL(cert, key string, opts ...SSLOption) *ConfigBuilder {
	if len(b.cfg.Servers) == 0 {
		b.WithServer(":8443")
	}

	ssl := config.SSLConfig{
		Cert: cert,
		Key:  key,
	}

	for _, opt := range opts {
		opt(&ssl)
	}

	b.cfg.Servers[0].SSL = ssl
	return b
}

// StaticOption 静态文件配置选项。
type StaticOption func(*config.StaticConfig)

// WithIndex 设置索引文件。
func WithIndex(index []string) StaticOption {
	return func(s *config.StaticConfig) {
		s.Index = index
	}
}

// WithTryFiles 设置 try_files。
func WithTryFiles(tryFiles []string) StaticOption {
	return func(s *config.StaticConfig) {
		s.TryFiles = tryFiles
	}
}

// WithStatic 添加静态文件配置。
func (b *ConfigBuilder) WithStatic(path, root string, opts ...StaticOption) *ConfigBuilder {
	if len(b.cfg.Servers) == 0 {
		b.WithServer(":8080")
	}

	static := config.StaticConfig{
		Path: path,
		Root: root,
	}

	for _, opt := range opts {
		opt(&static)
	}

	b.cfg.Servers[0].Static = append(b.cfg.Servers[0].Static, static)
	return b
}

// SecurityOption 安全配置选项。
type SecurityOption func(*config.SecurityConfig)

// WithRateLimit 设置速率限制。
func WithRateLimit(requestRate, burst int) SecurityOption {
	return func(s *config.SecurityConfig) {
		s.RateLimit = config.RateLimitConfig{
			RequestRate: requestRate,
			Burst:       burst,
		}
	}
}

// WithAccessControl 设置访问控制。
func WithAccessControl(allow, deny []string, defaultAction string) SecurityOption {
	return func(s *config.SecurityConfig) {
		s.Access = config.AccessConfig{
			Allow:   allow,
			Deny:    deny,
			Default: defaultAction,
		}
	}
}

// WithBasicAuth 设置 Basic 认证。
func WithBasicAuth(users []config.User) SecurityOption {
	return func(s *config.SecurityConfig) {
		s.Auth = config.AuthConfig{
			Type:  "basic",
			Users: users,
		}
	}
}

// WithSecurity 配置安全选项。
func (b *ConfigBuilder) WithSecurity(opts ...SecurityOption) *ConfigBuilder {
	if len(b.cfg.Servers) == 0 {
		b.WithServer(":8080")
	}

	for _, opt := range opts {
		opt(&b.cfg.Servers[0].Security)
	}
	return b
}

// CompressionOption 压缩配置选项。
type CompressionOption func(*config.CompressionConfig)

// WithCompressionType 设置压缩类型。
func WithCompressionType(typ string) CompressionOption {
	return func(c *config.CompressionConfig) {
		c.Type = typ
	}
}

// WithCompressionLevel 设置压缩级别。
func WithCompressionLevel(level int) CompressionOption {
	return func(c *config.CompressionConfig) {
		c.Level = level
	}
}

// WithCompressionMinSize 设置最小压缩大小。
func WithCompressionMinSize(minSize int) CompressionOption {
	return func(c *config.CompressionConfig) {
		c.MinSize = minSize
	}
}

// WithCompression 配置压缩。
func (b *ConfigBuilder) WithCompression(opts ...CompressionOption) *ConfigBuilder {
	if len(b.cfg.Servers) == 0 {
		b.WithServer(":8080")
	}

	for _, opt := range opts {
		opt(&b.cfg.Servers[0].Compression)
	}
	return b
}

// RewriteOption 重写规则选项。
type RewriteOption func(*config.RewriteRule)

// WithRewriteFlag 设置重写标志。
func WithRewriteFlag(flag string) RewriteOption {
	return func(r *config.RewriteRule) {
		r.Flag = flag
	}
}

// WithRewrite 添加 URL 重写规则。
func (b *ConfigBuilder) WithRewrite(pattern, replacement string, opts ...RewriteOption) *ConfigBuilder {
	if len(b.cfg.Servers) == 0 {
		b.WithServer(":8080")
	}

	rule := config.RewriteRule{
		Pattern:     pattern,
		Replacement: replacement,
	}

	for _, opt := range opts {
		opt(&rule)
	}

	b.cfg.Servers[0].Rewrite = append(b.cfg.Servers[0].Rewrite, rule)
	return b
}

// WithResolver 配置 DNS 解析器。
func (b *ConfigBuilder) WithResolver(addresses []string, valid, timeout time.Duration) *ConfigBuilder {
	b.cfg.Resolver = config.ResolverConfig{
		Enabled:   true,
		Addresses: addresses,
		Valid:     valid,
		Timeout:   timeout,
	}
	return b
}

// WithLogging 配置日志。
func (b *ConfigBuilder) WithLogging(format string) *ConfigBuilder {
	b.cfg.Logging = config.LoggingConfig{
		Format: format,
	}
	return b
}

// WithShutdown 配置关闭超时。
func (b *ConfigBuilder) WithShutdown(graceful, fast time.Duration) *ConfigBuilder {
	b.cfg.Shutdown = config.ShutdownConfig{
		GracefulTimeout: graceful,
		FastTimeout:     fast,
	}
	return b
}

// Build 生成 YAML 配置字符串。
//
// 返回 YAML 格式的配置字符串。
func (b *ConfigBuilder) Build() (string, error) {
	data, err := yaml.Marshal(b.cfg)
	if err != nil {
		return "", fmt.Errorf("序列化配置失败: %w", err)
	}
	return string(data), nil
}

// WriteTemp 写入临时文件。
//
// 创建临时目录并写入配置文件，返回文件路径。
// 调用者负责清理临时目录。
func (b *ConfigBuilder) WriteTemp() (string, error) {
	yamlStr, err := b.Build()
	if err != nil {
		return "", err
	}

	// 创建临时目录
	tmpDir, err := os.MkdirTemp("", "lolly-e2e-*")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}

	// 写入配置文件
	configPath := filepath.Join(tmpDir, "lolly.yaml")
	if err := os.WriteFile(configPath, []byte(yamlStr), 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("写入配置文件失败: %w", err)
	}

	return configPath, nil
}

// WriteTo 写入指定目录。
func (b *ConfigBuilder) WriteTo(dir string) (string, error) {
	yamlStr, err := b.Build()
	if err != nil {
		return "", err
	}

	// 确保目录存在
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}

	configPath := filepath.Join(dir, "lolly.yaml")
	if err := os.WriteFile(configPath, []byte(yamlStr), 0o644); err != nil {
		return "", fmt.Errorf("写入配置文件失败: %w", err)
	}

	return configPath, nil
}

// GetConfig 返回配置对象。
func (b *ConfigBuilder) GetConfig() *config.Config {
	return b.cfg
}

// Reset 重置构建器。
func (b *ConfigBuilder) Reset() *ConfigBuilder {
	b.cfg = &config.Config{
		Servers: []config.ServerConfig{},
	}
	return b
}

// Clone 克隆构建器。
func (b *ConfigBuilder) Clone() *ConfigBuilder {
	data, err := yaml.Marshal(b.cfg)
	if err != nil {
		return NewConfigBuilder()
	}

	var newCfg config.Config
	if err := yaml.Unmarshal(data, &newCfg); err != nil {
		return NewConfigBuilder()
	}

	return &ConfigBuilder{cfg: &newCfg}
}
