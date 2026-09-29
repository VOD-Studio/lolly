// Package ssl 提供 SSL/TLS 支持。
//
// 该文件包含基于 SNI（服务器名称指示）的多证书选择逻辑，
// 用于虚拟主机模式：多个域名共享同一个监听端口，
// 每个域名可以配置独立的证书、协议、加密套件、OCSP、mTLS 等参数。
//
// 匹配规则与 HTTP 层的 server_name 虚拟主机路由（internal/server.VHostManager）
// 完全一致（由 internal/hostmatch 提供），保证 TLS 握手阶段选中的证书
// 与请求实际路由到的虚拟主机一致。
//
// 作者：xfy
package ssl

import (
	"crypto/tls"
	"errors"
	"fmt"
	"sync/atomic"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/hostmatch"
)

// errRejectHandshake 表示因 ssl_reject_handshake 配置而拒绝握手。
//
// 作为 tls.Config.GetConfigForClient 的返回错误，会使 crypto/tls
// 向客户端发送 handshake_failure 警告并中断握手。定义在 sni.go 而非
// ssl.go 之外，是因为 SNIManager 与 TLSManager 共用同一个错误语义。
var errRejectHandshake = errors.New("ssl: handshake rejected by ssl_reject_handshake")

// SNIEntry 表示一个虚拟主机的名称及其 SSL 配置。
//
// Name 支持与 server_name 一致的匹配语法：精确匹配、前缀通配
// （*.example.com）、后缀通配（example.*）、正则匹配（~regex）。
type SNIEntry struct {
	// Name 虚拟主机名称（server_name）
	Name string
	// SSL 该虚拟主机的 SSL 配置
	SSL *config.SSLConfig
	// ACME 该虚拟主机使用的 ACME 管理器（启用自动证书时非 nil）
	ACME *ACMEManager
}

// sniOptions 保存构建 SNI 管理器时注入的可选依赖。
type sniOptions struct {
	// acmes 服务器索引到 ACME 管理器的映射
	acmes map[int]*ACMEManager
	// defaultACME 默认虚拟主机的 ACME 管理器
	defaultACME *ACMEManager
	// dynamicCfg 容器发现 HTTPS 模板配置
	dynamicCfg *config.SSLConfig
	// dynamicACME 容器发现 HTTPS 模板 ACME 管理器
	dynamicACME *ACMEManager
}

// SNIOption 配置 SNI 管理器构建过程的可选参数。
type SNIOption func(*sniOptions)

// WithSNIACMEManagers 注入与 servers 索引对应的 ACME 管理器。
//
// 参数：
//   - acmes: 索引到 ACME 管理器的映射，缺失索引视为未启用 ACME
//
// 返回值：
//   - SNIOption: 应用于构建过程的选项函数
func WithSNIACMEManagers(acmes map[int]*ACMEManager) SNIOption {
	return func(o *sniOptions) {
		o.acmes = acmes
	}
}

// WithDefaultACMEManager 设置默认虚拟主机使用的 ACME 管理器。
//
// 参数：
//   - mgr: 默认虚拟主机的 ACME 管理器，可为 nil
//
// 返回值：
//   - SNIOption: 应用于构建过程的选项函数
func WithDefaultACMEManager(mgr *ACMEManager) SNIOption {
	return func(o *sniOptions) {
		o.defaultACME = mgr
	}
}

// WithDynamicSNIACME 设置动态域名使用的 HTTPS 模板。
func WithDynamicSNIACME(cfg *config.SSLConfig, mgr *ACMEManager) SNIOption {
	return func(o *sniOptions) {
		o.dynamicCfg = cfg
		o.dynamicACME = mgr
	}
}

// SNIManager 基于 SNI 在同一监听端口上按域名选择证书。
//
// 每个虚拟主机拥有独立的 *TLSManager（独立证书、协议、加密套件、
// Session Ticket、OCSP Stapling、mTLS 配置），SNIManager 仅负责在
// TLS 握手时根据 ClientHelloInfo.ServerName 选出对应的 TLSManager。
type SNIManager struct {
	matcher      *hostmatch.Matcher[*TLSManager]
	managers     []*TLSManager
	defaultMgr   *TLSManager
	dynamicMgr   *TLSManager
	dynamicHosts atomic.Pointer[map[string]bool]
}

// NewSNIManager 根据虚拟主机列表创建 SNI 管理器。
//
// 参数：
//   - entries: 虚拟主机名称到 SSL 配置的映射列表
//   - defaultCfg: 未匹配任何虚拟主机时使用的默认 SSL 配置，可为 nil
//   - opts: 可选注入项，如默认虚拟主机的 ACME 管理器
//
// 返回值：
//   - *SNIManager: 配置好的 SNI 管理器
//   - error: 参数为空、证书加载失败或 server_name 模式无效时返回错误
func NewSNIManager(entries []SNIEntry, defaultCfg *config.SSLConfig, opts ...SNIOption) (*SNIManager, error) {
	if len(entries) == 0 && defaultCfg == nil {
		return nil, errors.New("ssl: no SNI entries or default SSL config provided")
	}

	o := &sniOptions{}
	for _, opt := range opts {
		opt(o)
	}

	m := &SNIManager{matcher: hostmatch.New[*TLSManager]()}

	for _, e := range entries {
		mgr, err := NewTLSManager(e.SSL, WithACMEManager(e.ACME))
		if err != nil {
			return nil, fmt.Errorf("ssl: failed to create TLS manager for %q: %w", e.Name, err)
		}
		if err := m.matcher.Add(e.Name, mgr); err != nil {
			m.Close()
			return nil, fmt.Errorf("ssl: invalid server_name pattern %q: %w", e.Name, err)
		}
		m.managers = append(m.managers, mgr)
	}

	if o.dynamicCfg != nil {
		mgr, err := NewTLSManager(o.dynamicCfg, WithACMEManager(o.dynamicACME))
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("ssl: failed to create dynamic TLS manager: %w", err)
		}
		m.dynamicMgr = mgr
		m.managers = append(m.managers, mgr)
	}

	if defaultCfg != nil {
		mgr, err := NewTLSManager(defaultCfg, WithACMEManager(o.defaultACME))
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("ssl: failed to create default TLS manager: %w", err)
		}
		m.defaultMgr = mgr
		m.managers = append(m.managers, mgr)
	}

	return m, nil
}

// TLSConfig 返回带有 GetConfigForClient 回调的 tls.Config。
//
// 握手时根据 ClientHelloInfo.ServerName 动态选择对应虚拟主机的
// TLS 配置；未携带 SNI 或未匹配任何虚拟主机时，回退到默认配置
// （或第一个虚拟主机的配置）。若回退目标配置了 ssl_reject_handshake，
// 则直接拒绝握手。
func (m *SNIManager) TLSConfig() *tls.Config {
	base := m.fallbackConfig()

	cfg := &tls.Config{
		GetConfigForClient: m.getConfigForClient,
	}
	if base != nil {
		cfg.MinVersion = base.MinVersion
		cfg.MaxVersion = base.MaxVersion
		cfg.NextProtos = base.NextProtos
		cfg.Certificates = base.Certificates
		cfg.CipherSuites = base.CipherSuites
	}
	return cfg
}

// fallbackManager 返回未匹配任何虚拟主机时使用的兜底 TLSManager。
//
// 优先使用显式配置的默认虚拟主机，其次回退到首个虚拟主机，
// 都没有时返回 nil。
func (m *SNIManager) fallbackManager() *TLSManager {
	if m.defaultMgr != nil {
		return m.defaultMgr
	}
	if len(m.managers) > 0 {
		return m.managers[0]
	}
	return nil
}

// fallbackConfig 返回兜底 TLSManager 的 TLS 配置，用于初始化外层
// tls.Config 的协议版本、ALPN 等基础参数。
func (m *SNIManager) fallbackConfig() *tls.Config {
	if mgr := m.fallbackManager(); mgr != nil {
		return mgr.GetTLSConfig()
	}
	return nil
}

// getConfigForClient 是 tls.Config.GetConfigForClient 回调，
// 按 ClientHelloInfo.ServerName 选择对应虚拟主机的 TLS 配置。
//
// 选择规则：
//   - ServerName 匹配某个虚拟主机时使用其配置；若该虚拟主机配置了
//     ssl_reject_handshake，则返回握手失败错误，拒绝连接
//   - 未携带 SNI 或未匹配任何虚拟主机时回退到默认（或首个）虚拟主机；
//     若回退目标也配置了 ssl_reject_handshake，同样拒绝握手
func (m *SNIManager) getConfigForClient(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	if hello.ServerName != "" {
		if mgr, ok := m.matcher.Find(hello.ServerName); ok && mgr != nil {
			if mgr.RejectHandshake() {
				return nil, errRejectHandshake
			}
			return mgr.GetTLSConfig(), nil
		}
		if hosts := m.dynamicHosts.Load(); m.dynamicMgr != nil && hosts != nil && (*hosts)[normalizeACMEHost(hello.ServerName)] {
			return m.dynamicMgr.GetTLSConfig(), nil
		}
	}

	if mgr := m.fallbackManager(); mgr != nil {
		if mgr.RejectHandshake() {
			return nil, errRejectHandshake
		}
		return mgr.GetTLSConfig(), nil
	}

	return nil, errors.New("ssl: no TLS configuration available for SNI")
}

// SetDynamicHosts 原子替换选择 HTTPS 模板的动态 SNI 域名集合。
func (m *SNIManager) SetDynamicHosts(hosts []string) {
	set := make(map[string]bool)
	for _, host := range normalizeACMEHosts(hosts) {
		set[host] = true
	}
	m.dynamicHosts.Store(&set)
}

// Close 关闭所有底层 TLSManager，释放 OCSP / Session Ticket 相关资源。
func (m *SNIManager) Close() {
	for _, mgr := range m.managers {
		mgr.Close()
	}
}

// ACMEManagers 返回所有底层 TLSManager 使用的 ACME 管理器。
//
// 用于让 HTTP-01 挑战路由复用与 TLS 握手相同的实例。返回结果已去重。
//
// 返回值：
//   - []*ACMEManager: ACME 管理器列表，未启用 ACME 时为空切片
func (m *SNIManager) ACMEManagers() []*ACMEManager {
	seen := make(map[*ACMEManager]bool, len(m.managers))
	var out []*ACMEManager
	for _, mgr := range m.managers {
		if am := mgr.ACMEManager(); am != nil && !seen[am] {
			seen[am] = true
			out = append(out, am)
		}
	}
	return out
}

// BuildSNIManager 根据一组共享同一监听端口的服务器配置创建 SNI 管理器。
//
// 配置了 cert/key 或启用 ACME 的服务器都会纳入 SNI 匹配；每个服务器可以
// 配置 server_names（多个）或使用 name 字段作为唯一 server_name。
//
// 参数：
//   - servers: 共享同一监听地址的虚拟主机配置列表
//   - defaultIdx: servers 中作为默认虚拟主机的索引，-1 表示无默认
//   - opts: 可选注入项，如各虚拟主机的 ACME 管理器
//
// 返回值：
//   - *SNIManager: 配置好的 SNI 管理器；servers 中没有任何 TLS 配置时返回 (nil, nil)
//   - error: 构建过程中出现的错误
func BuildSNIManager(servers []config.ServerConfig, defaultIdx int, opts ...SNIOption) (*SNIManager, error) {
	o := &sniOptions{}
	for _, opt := range opts {
		opt(o)
	}

	var entries []SNIEntry
	var defaultCfg *config.SSLConfig
	var defaultACME *ACMEManager

	for i := range servers {
		srv := &servers[i]
		// 具备 TLS 能力：静态证书、ACME 自动证书，或仅拒绝握手
		// （拒绝握手的服务器没有证书，但仍需纳入 SNI 匹配以便
		// 对未知 SNI 返回握手失败）
		hasTLS := (srv.SSL.Cert != "" && srv.SSL.Key != "") || srv.SSL.ACME.Enabled || srv.SSL.RejectHandshake
		if !hasTLS {
			continue
		}

		acme := o.acmes[i]

		names := srv.ServerNames
		if len(names) == 0 {
			names = []string{srv.Name}
		}
		// 拒绝握手且未配置 server_name 的服务器仅作为默认兜底，
		// 不注册具名 SNI 条目（避免空名污染匹配器）。它通过下方
		// defaultIdx 分支设置为 defaultCfg，由 SNIManager 的兜底
		// 路径返回握手失败。
		if srv.SSL.RejectHandshake && len(names) == 1 && names[0] == "" {
			if i == defaultIdx {
				defaultCfg = &srv.SSL
				defaultACME = acme
			}
			continue
		}
		for _, name := range names {
			entries = append(entries, SNIEntry{Name: name, SSL: &srv.SSL, ACME: acme})
		}

		if i == defaultIdx {
			defaultCfg = &srv.SSL
			defaultACME = acme
		}
	}

	if len(entries) == 0 && defaultCfg == nil {
		return nil, nil
	}

	options := []SNIOption{WithDefaultACMEManager(defaultACME)}
	if o.dynamicCfg != nil {
		options = append(options, WithDynamicSNIACME(o.dynamicCfg, o.dynamicACME))
	}
	return NewSNIManager(entries, defaultCfg, options...)
}
