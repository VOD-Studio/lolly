// Package ssl 提供 SSL/TLS 支持。
//
// 该文件包含基于 ACME 协议的自动证书签发/续期逻辑，底层使用
// golang.org/x/crypto/acme/autocert，包含：
//   - 账户注册与证书申请
//   - 到期前自动续期（无需重启）
//   - tls-alpn-01 / http-01 挑战响应
//
// 主要用途：
//
//	使 lolly 自身即可对接 Let's Encrypt 等 CA 完成证书签发与续期，
//	不再依赖 certbot/acme-companion 等外部工具。
//
// 注意事项：
//   - 同一个 ACMEManager 实例必须同时用于 TLS 握手与 HTTP-01 挑战响应，
//     二者共享内存中的挑战令牌，跨实例使用会导致校验失败
//   - 通配符域名需要 DNS-01 挑战，本实现不支持
//
// 作者：xfy
package ssl

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/logging"
)

// DefaultACMEStatePath ACME 状态目录默认路径。
//
// 存放账户私钥与已签发证书，进程重启后复用，避免重复申请
// 触发 CA 速率限制。
const DefaultACMEStatePath = "/var/lib/lolly/acme"

// ACMEManager 管理 ACME 自动证书的申请与续期。
//
// 封装 autocert.Manager，对 TLS 层暴露 GetCertificate 回调，
// 并对外暴露 http-01 挑战响应处理器。二者必须来自同一个实例，
// 因此 SNI/TLS 管理器与 HTTP 挑战路由共享同一 ACMEManager。
type ACMEManager struct {
	// mgr 底层 autocert 管理器
	mgr *autocert.Manager

	// hosts 允许申请证书的域名白名单，空表示不限制
	hosts map[string]bool

	// http01 是否启用 http-01 挑战
	http01 bool
}

// NewACMEManager 根据配置创建 ACME 管理器。
//
// 参数：
//   - cfg: ACME 配置，Enabled 为 false 或 cfg 为 nil 时返回 (nil, nil)
//   - hosts: 允许申请证书的域名列表；为空时不设白名单限制（不推荐，
//     存在被脚本用任意 SNI 刷证书从而触发 CA 速率限制的风险）
//
// 返回值：
//   - *ACMEManager: 创建的管理器，未启用时为 nil
//   - error: 配置无效（如 EAB 密钥不是合法 base64url）时返回错误
func NewACMEManager(cfg *config.ACMEConfig, hosts []string) (*ACMEManager, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}

	directory := cfg.Directory
	if directory == "" {
		directory = autocert.DefaultACMEDirectory
	}

	statePath := cfg.StatePath
	if statePath == "" {
		statePath = DefaultACMEStatePath
	}

	challenge := cfg.Challenge
	if challenge == "" {
		challenge = config.ACMEChallengeTLSALPN01
	}

	mgr := &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Cache:  autocert.DirCache(statePath),
		Email:  cfg.Email,
		Client: &acme.Client{DirectoryURL: directory},
	}

	// 外部账户绑定（EAB）：部分 CA 要求提供 KID 与 HMAC 密钥
	if cfg.EABKid != "" {
		key, err := base64.RawURLEncoding.DecodeString(cfg.EABHmacKey)
		if err != nil {
			return nil, fmt.Errorf("acme: 无效的 eab_hmac_key（应为 base64url 编码）: %w", err)
		}
		mgr.ExternalAccountBinding = &acme.ExternalAccountBinding{KID: cfg.EABKid, Key: key}
	}

	m := &ACMEManager{
		mgr:    mgr,
		http01: challenge == config.ACMEChallengeHTTP01,
	}

	// 域名白名单：限制只为已配置域名签发证书
	whitelist := normalizeACMEHosts(hosts)
	if len(whitelist) == 0 {
		// 显式配置了域名但全部被剔除（如仅通配符）时，直接报错而非放开限制，
		// 避免退化成"为任意 SNI 签发证书"从而触发 CA 速率限制
		if len(hosts) > 0 {
			return nil, fmt.Errorf("acme: 配置的域名均无法用于 ACME（通配符需 DNS-01 挑战，本实现不支持）: %v", hosts)
		}
		logging.Warn().Msg("ACME 未配置域名白名单，将为任意 SNI 域名尝试签发证书，存在触发 CA 速率限制的风险")
	} else {
		mgr.HostPolicy = autocert.HostWhitelist(whitelist...)
		m.hosts = make(map[string]bool, len(whitelist))
		for _, h := range whitelist {
			m.hosts[h] = true
		}
	}

	logging.Info().
		Str("directory", directory).
		Str("challenge", challenge).
		Str("statePath", statePath).
		Int("hosts", len(whitelist)).
		Msg("ACME 自动证书已启用")

	return m, nil
}

// GetCertificate 是 tls.Config.GetCertificate 回调，按 SNI 返回证书。
//
// 首次访问某域名时触发签发（同步等待，最长 5 分钟），之后命中缓存
// 直接返回；autocert 会在到期前自动续期。
//
// 参数：
//   - hello: 客户端 Hello 信息，使用其中的 ServerName
//
// 返回值：
//   - *tls.Certificate: 域名对应的证书
//   - error: 白名单拒绝或签发失败时返回错误
func (m *ACMEManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return m.mgr.GetCertificate(hello)
}

// HTTPHandler 返回用于响应 http-01 挑战的 net/http 处理器。
//
// 参数：
//   - fallback: 非挑战路径的处理逻辑，可为 nil（默认重定向到 HTTPS）
//
// 返回值：
//   - http.Handler: 挑战响应处理器
//
// 注意事项：
//   - 仅当 Challenge 为 http-01 时应在 80 端口注册该处理器；
//     未启用时调用会令 autocert 额外尝试 http-01 挑战
func (m *ACMEManager) HTTPHandler(fallback http.Handler) http.Handler {
	return m.mgr.HTTPHandler(fallback)
}

// HTTP01 报告是否启用 http-01 挑战。
func (m *ACMEManager) HTTP01() bool {
	return m.http01
}

// HasHost 报告给定域名是否属于本管理器的签发范围。
//
// 白名单为空（未限制）时始终返回 true。
//
// 参数：
//   - host: 域名（不含端口）
//
// 返回值：
//   - bool: 属于签发范围返回 true
func (m *ACMEManager) HasHost(host string) bool {
	if len(m.hosts) == 0 {
		return true
	}
	return m.hosts[normalizeACMEHost(host)]
}

// ACMEALPNProto 返回 tls-alpn-01 挑战所需的 ALPN 协议标识。
//
// TLS 配置需在 NextProtos 中包含该值，autocert 才能响应
// CA 发来的 tls-alpn-01 校验连接。
func ACMEALPNProto() string {
	return acme.ALPNProto
}

// normalizeACMEHost 规范化单个域名：去空白、转小写、去除尾部点。
func normalizeACMEHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// normalizeACMEHosts 规范化域名列表：去重、转小写、剔除通配符。
//
// 通配符域名（*.example.com）需要 DNS-01 挑战，本实现不支持，
// 因此直接排除并给出告警，避免这些域名进入白名单后反而拦截
// 正常子域名的签发。
//
// 参数：
//   - hosts: 原始域名列表
//
// 返回值：
//   - []string: 规范化后的域名列表（已去重），可能为空
func normalizeACMEHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, raw := range hosts {
		h := normalizeACMEHost(raw)
		if h == "" || seen[h] {
			continue
		}
		if strings.Contains(h, "*") {
			logging.Warn().Str("host", h).Msg("ACME 不支持通配符域名（需 DNS-01 挑战），已忽略")
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}
