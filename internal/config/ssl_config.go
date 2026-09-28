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
//   - RejectHandshake 与 Cert/Key/ACME 互斥，仅用于拒绝未知 SNI 握手
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
	ACME           ACMEConfig           `yaml:"acme"`
	HTTP2          HTTP2Config          `yaml:"http2"`
	HSTS           HSTSConfig           `yaml:"hsts"`
	// RejectHandshake 是否拒绝 TLS 握手（对应 nginx ssl_reject_handshake）。
	//
	// 启用后该虚拟主机不再提供证书，而是对匹配到它的 ClientHello 直接
	// 返回握手失败（handshake_failure），常用于 default_server 上拒绝
	// 未知 SNI 的连接。与 Cert/Key、ACME 互斥：拒绝握手时无需证书。
	//
	// 典型用法（拒绝所有未匹配已知 server_name 的 SNI）：
	//
	//	servers:
	//	  - default: true
	//	    ssl:
	//	      reject_handshake: true
	//	  - server_names: ["example.com"]
	//	    ssl:
	//	      cert: "/etc/ssl/example.crt"
	//	      key:  "/etc/ssl/example.key"
	RejectHandshake bool `yaml:"reject_handshake"`
	OCSPStapling    bool `yaml:"ocsp_stapling"`
}

// ACME 挑战类型常量。
const (
	// ACMEChallengeTLSALPN01 通过 TLS ALPN 扩展验证（默认，无需 80 端口）
	ACMEChallengeTLSALPN01 = "tls-alpn-01"
	// ACMEChallengeHTTP01 在 /.well-known/acme-challenge/ 放置文件验证（需 80 端口）
	ACMEChallengeHTTP01 = "http-01"
)

// DefaultACMEDirectory Let's Encrypt 生产环境 ACME 目录 URL。
const DefaultACMEDirectory = "https://acme-v02.api.letsencrypt.org/directory"

// DefaultACMEStagingDirectory Let's Encrypt 测试环境 ACME 目录 URL。
//
// 测试环境不受速率限制影响，但签发的证书不被浏览器信任，仅用于调试。
const DefaultACMEStagingDirectory = "https://acme-staging-v02.api.letsencrypt.org/directory"

// ACMEConfig ACME（Let's Encrypt）自动证书签发/续期配置。
//
// 启用后 lolly 会作为 ACME 客户端，向 CA 自动申请并在证书到期前自动
// 续期，无需 certbot/acme-companion 等外部工具。证书与账户状态持久化在
// StatePath 目录，进程重启后复用，避免重复申请触发 CA 速率限制。
//
// 与 nginx `ngx_http_acme_module` 的对应关系：
//   - acme_issuer 的 uri/contact/challenge/state_path/account_key 对应
//     本结构体的 Directory/Email/Challenge/StatePath 等字段
//   - acme_certificate 的标识符列表对应 Hosts（为空时使用 server_names）
//
// 注意事项：
//   - Cert/Key 已配置时优先使用静态证书，ACME 配置被忽略
//   - tls-alpn-01 挑战不依赖 80 端口，是纯 TLS 服务端最省心的选择
//   - http-01 挑战要求 80 端口可被 CA 访问，并由 lolly 处理挑战请求
//   - 通配符域名（*.example.com）需要 DNS-01 挑战，本实现不支持
//   - 使用本功能即表示同意 CA 的服务条款（TOS）
//
// 使用示例：
//
//	ssl:
//	  acme:
//	    enabled: true
//	    email: "admin@example.com"
//	    state_path: "/var/lib/lolly/acme"
//	    challenge: "tls-alpn-01"
type ACMEConfig struct {
	// Directory ACME 服务器目录 URL
	// 默认使用 Let's Encrypt 生产环境，测试时可改为 DefaultACMEStagingDirectory
	Directory string `yaml:"directory"`

	// Email 联系邮箱
	// 用于账户注册和证书问题通知，建议配置
	Email string `yaml:"email"`

	// StatePath 状态持久化目录
	// 存放账户私钥和已签发证书，默认 /var/lib/lolly/acme
	// 目录权限应为 0700
	StatePath string `yaml:"state_path"`

	// Challenge 挑战类型
	// 可选值：tls-alpn-01（默认，无需 80 端口）、http-01（需 80 端口）
	Challenge string `yaml:"challenge"`

	// Hosts 申请证书的域名列表
	// 为空时使用服务器的 server_names（无 server_names 时使用 name）
	Hosts []string `yaml:"hosts"`

	// EABKid 外部账户绑定 Key ID（可选）
	// 仅在使用支持 EAB 的 CA（如 ZeroSSL、Google CA）时需要
	EABKid string `yaml:"eab_kid"`

	// EABHmacKey 外部账户绑定 HMAC 密钥（可选）
	// base64url 编码（RFC 4648 §5，无 padding），与 EABKid 配对使用
	EABHmacKey string `yaml:"eab_hmac_key"`

	// Enabled 是否启用 ACME 自动证书
	Enabled bool `yaml:"enabled"`
}

// ResolveHosts 推导 ACME 申请证书使用的域名列表。
//
// 优先级：Hosts（显式配置）> serverNames > name。
// 配置校验与运行时（Server 启动建管理器）共用该规则，避免两处逻辑漂移。
//
// 参数：
//   - serverNames: 服务器的 server_names 列表
//   - name: 服务器名称
//
// 返回值：
//   - []string: 域名列表，可能为空（表示无可用域名来源）
func (a *ACMEConfig) ResolveHosts(serverNames []string, name string) []string {
	if len(a.Hosts) > 0 {
		return a.Hosts
	}
	if len(serverNames) > 0 {
		return serverNames
	}
	if name != "" {
		return []string{name}
	}
	return nil
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
