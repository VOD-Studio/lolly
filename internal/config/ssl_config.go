// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 包含 SSL/TLS 配置相关的结构体，用于配置 HTTPS 加密参数。
//
// 作者：xfy
package config

import "time"

// SSLConfig SSL/TLS 配置。
//
// 用于配置 HTTPS 服务所需的证书和加密参数。
// 支持 TLS 1.2 和 TLS 1.3 协议，可自定义加密套件。
//
// 注意事项：
//   - Cert 和 Key 为必需字段，分别指向证书和私钥文件
//   - CertChain 可选，用于配置完整的证书链
//   - Protocols 建议使用默认值，避免使用不安全的 TLS 1.0/1.1
//   - Ciphers 仅对 TLS 1.2 有效，TLS 1.3 有固定加密套件
//   - 启用 OCSPStapling 可提升握手性能
//
// 使用示例：
//
//	ssl:
//	  cert: "/etc/ssl/certs/server.crt"
//	  key: "/etc/ssl/private/server.key"
//	  cert_chain: "/etc/ssl/certs/chain.crt"
//	  protocols: ["TLSv1.2", "TLSv1.3"]
//	  ocsp_stapling: true
//	  hsts:
//	    max_age: 31536000
//	    include_sub_domains: true
type SSLConfig struct {
	ClientVerify   ClientVerifyConfig   `yaml:"client_verify"`
	Cert           string               `yaml:"cert"`
	Key            string               `yaml:"key"`
	CertChain      string               `yaml:"cert_chain"`
	Protocols      []string             `yaml:"protocols"`
	Ciphers        []string             `yaml:"ciphers"`
	SessionTickets SessionTicketsConfig `yaml:"session_tickets"`
	HTTP2          HTTP2Config          `yaml:"http2"`
	HSTS           HSTSConfig           `yaml:"hsts"`
	OCSPStapling   bool                 `yaml:"ocsp_stapling"`
}

// HSTSConfig HTTP Strict Transport Security 配置。
//
// 强制浏览器使用 HTTPS 访问，防止中间人攻击和协议降级攻击。
//
// 注意事项：
//   - MaxAge 单位为秒，建议至少设置为 1 年（31536000）
//   - IncludeSubDomains 为 true 时策略应用于所有子域名
//   - Preload 为 true 表示申请加入浏览器预加载列表
//   - 启用前确保所有站点资源都支持 HTTPS
//
// 使用示例：
//
//	hsts:
//	  max_age: 31536000
//	  include_sub_domains: true
//	  preload: false
type HSTSConfig struct {
	// MaxAge 过期时间（秒）
	// 默认 31536000（1年），建议至少 6 个月
	MaxAge int `yaml:"max_age"`

	// IncludeSubDomains 包含子域名
	// 为 true 时策略应用于当前域名及其所有子域名
	IncludeSubDomains bool `yaml:"include_sub_domains"`

	// Preload 加入 HSTS 预加载列表
	// 申请加入浏览器内置的 HSTS 列表
	Preload bool `yaml:"preload"`
}

// SessionTicketsConfig TLS Session Ticket 配置。
//
// Session Tickets 允许 TLS 1.3 会话恢复，避免完整握手，显著提升性能。
// 密钥定期轮换增强安全性，同时保留旧密钥确保已发放的票据仍可解密。
//
// 与 nginx ssl_session_cache 的关系：
// nginx 的 ssl_session_cache 是服务端维护的会话 ID 缓存（TLS 1.2 及以下
// 的经典会话恢复机制）。Go 的 crypto/tls 服务端没有暴露等价的会话 ID
// 缓存配置，会话恢复统一通过本配置项对应的 Session Ticket（无状态、
// 加密票据）实现——这也是 TLS 1.3 唯一的恢复机制，且被现代 TLS 部署
// （包括 nginx 自身）广泛采用。因此 session_tickets.enabled=true 即
// 为 nginx ssl_session_cache 的功能等价物；设为 false 等同于同时禁用
// session cache 和 session tickets。
//
// 注意事项：
//   - KeyFile 为密钥存储文件路径，用于持久化密钥
//   - RotateInterval 为密钥轮换间隔，建议 1-24 小时
//   - RetainKeys 为保留的历史密钥数量，至少保留 2 个
//   - 密钥文件权限应为 0600（仅所有者可读写）
//
// 使用示例：
//
//	ssl:
//	  session_tickets:
//	    enabled: true
//	    key_file: "/var/lib/lolly/session_tickets.key"
//	    rotate_interval: 1h
//	    retain_keys: 3
type SessionTicketsConfig struct {
	KeyFile        string        `yaml:"key_file"`
	RotateInterval time.Duration `yaml:"rotate_interval"`
	RetainKeys     int           `yaml:"retain_keys"`
	Enabled        bool          `yaml:"enabled"`
}

// ClientVerifyConfig mTLS 客户端证书验证配置。
//
// 配置双向 TLS 认证，要求客户端提供有效证书才能建立连接。
// 适用于需要强身份验证的场景，如 API 服务、内部系统通信。
//
// 注意事项：
//   - Mode 可选值：none、request、require、optional_no_ca
//   - ClientCA 为客户端 CA 证书文件路径（必需）
//   - VerifyDepth 为证书链验证深度，默认 1
//   - CRL 为证书撤销列表文件路径（可选）
//
// 使用示例：
//
//	ssl:
//	  client_verify:
//	    enabled: true
//	    mode: "require"
//	    client_ca: "/etc/ssl/ca/client-ca.crt"
//	    verify_depth: 2
//	    crl: "/etc/ssl/ca/client-ca.crl"
type ClientVerifyConfig struct {
	Mode        string `yaml:"mode"`
	ClientCA    string `yaml:"client_ca"`
	CRL         string `yaml:"crl"`
	VerifyDepth int    `yaml:"verify_depth"`
	Enabled     bool   `yaml:"enabled"`
}
