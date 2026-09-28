// Package ssl 提供 SSL/TLS 支持。
//
// 该文件包含 TLS 配置管理的核心逻辑，包括：
//   - 安全的 TLS 默认配置（仅 TLSv1.2 和 TLSv1.3）
//   - 证书加载和管理
//   - OCSP Stapling 支持
//
// 多域名共享同一监听端口、按 SNI（服务器名称指示）选择不同证书的场景，
// 见 sni.go 中的 SNIManager，它组合多个 TLSManager 并按 server_name
// 规则（与 HTTP 虚拟主机路由一致）动态选择证书。
//
// 主要用途：
//
//	用于管理 HTTPS 服务器的 TLS 配置。
//
// 安全默认值：
//   - TLS 版本：仅启用 TLSv1.2 和 TLSv1.3
//   - TLSv1.0 和 TLSv1.1 被强制禁用（不安全）
//   - 安全加密套件，支持前向保密
//   - 配置 TLS 时自动启用 HTTP/2
//
// 使用示例：
//
//	cfg := &config.SSLConfig{
//	    Cert: "/path/to/cert.pem",
//	    Key:  "/path/to/key.pem",
//	    Protocols: []string{"TLSv1.2", "TLSv1.3"},
//	}
//
//	manager, err := ssl.NewTLSManager(cfg)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// 配合 fasthttp 使用
//	server := &fasthttp.Server{
//	    TLSConfig: manager.GetTLSConfig(),
//	}
//
// 作者：xfy
package ssl

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/sslutil"
)

// TLSManager TLS 配置管理器。
//
// 管理单个证书的 TLS 配置，包括 OCSP Stapling 用于证书状态验证。
// 多域名共享同一监听端口、按 SNI 选择不同证书的场景由
// SNIManager（sni.go）组合多个 TLSManager 实现。
type TLSManager struct {
	// defaultCfg 默认配置，用于 fallback
	defaultCfg *tls.Config

	// ocspManager OCSP Stapling 管理器
	ocspManager *OCSPManager

	// sessionTicketMgr Session Ticket 管理器
	sessionTicketMgr *SessionTicketManager

	// clientVerifier 客户端证书验证器
	clientVerifier *ClientVerifier

	// certificates 解析后的证书映射，用于 OCSP
	certificates map[string]*x509.Certificate

	// defaultCert 默认证书的解析结果，避免每次握手重新解析
	defaultCert *x509.Certificate

	// issuers 颁发者证书映射，用于 OCSP
	issuers map[string]*x509.Certificate

	// mu 保护并发访问的读写锁
	mu sync.RWMutex
}

// NewTLSManager 创建新的 TLS 配置管理器。
//
// 对于单服务器模式，传入单个 SSLConfig。
//
// 参数：
//   - cfg: SSL 配置，包含证书路径和 TLS 设置
//
// 返回值：
//   - *TLSManager: 配置好的 TLS 管理器
//   - error: 证书加载失败或配置无效时返回错误
func NewTLSManager(cfg *config.SSLConfig) (*TLSManager, error) {
	if cfg == nil {
		return nil, errors.New("ssl config is nil")
	}

	if cfg.Cert == "" || cfg.Key == "" {
		return nil, errors.New("certificate and key paths are required")
	}

	// 加载证书
	cert, err := loadCertificate(cfg.Cert, cfg.Key, cfg.CertChain)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	// 创建 TLS 配置，使用安全默认值
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12, // 强制 TLS 1.2 最低版本
		MaxVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2", "http/1.1"}, // 启用 HTTP/2 ALPN 支持
	}

	// 应用 TLS 1.2 的加密套件
	if len(cfg.Ciphers) > 0 {
		ciphers, err := sslutil.ParseCipherSuites(cfg.Ciphers)
		if err != nil {
			return nil, fmt.Errorf("invalid cipher suites: %w", err)
		}
		tlsCfg.CipherSuites = ciphers
	} else {
		// 使用安全的默认加密套件
		tlsCfg.CipherSuites = sslutil.DefaultCipherSuites()
	}

	// 解析 TLS 协议版本
	if len(cfg.Protocols) > 0 {
		minVer, maxVer, err := sslutil.ParseTLSVersions(cfg.Protocols)
		if err != nil {
			return nil, fmt.Errorf("invalid TLS protocols: %w", err)
		}
		tlsCfg.MinVersion = minVer
		tlsCfg.MaxVersion = maxVer
	}

	manager := &TLSManager{
		certificates: make(map[string]*x509.Certificate),
		issuers:      make(map[string]*x509.Certificate),
	}

	// 初始化 Session Tickets（如果启用）
	if cfg.SessionTickets.Enabled {
		sessionTicketMgr, err := NewSessionTicketManager(cfg.SessionTickets)
		if err != nil {
			logging.Warn().Err(err).Msg("Session Ticket 初始化失败，TLS 性能可能降级")
		} else {
			manager.sessionTicketMgr = sessionTicketMgr
			// 应用 Session Tickets 到 TLS 配置
			sessionTicketMgr.ApplyToTLSConfig(tlsCfg)
			sessionTicketMgr.Start()
		}
	}

	// 初始化 OCSP Stapling（如果启用）
	if cfg.OCSPStapling {
		ocspMgr := NewOCSPManager(DefaultOCSPConfig())
		manager.ocspManager = ocspMgr

		// 解析证书用于 OCSP
		if len(cert.Certificate) > 0 {
			parsedCert, err := x509.ParseCertificate(cert.Certificate[0])
			if err == nil {
				manager.defaultCert = parsedCert
				if len(parsedCert.OCSPServer) > 0 {
					// 存储证书用于 OCSP 查询
					serial := parsedCert.SerialNumber.String()
					manager.certificates[serial] = parsedCert

					// 尝试从证书链解析颁发者证书
					if len(cert.Certificate) > 1 {
						issuerCert, err := x509.ParseCertificate(cert.Certificate[1])
						if err == nil {
							manager.issuers[serial] = issuerCert
							if err := ocspMgr.RegisterCertificate(parsedCert, issuerCert); err != nil {
								logging.Warn().Err(err).Msg("OCSP Stapling 注册失败")
							}
						}
					}
				}
			}
		}

		// 设置 GetConfigForClient 回调用于 OCSP Stapling
		tlsCfg.GetConfigForClient = manager.getConfigForClientWithOCSP

		ocspMgr.Start()
	}

	// 初始化客户端证书验证（如果启用）
	if cfg.ClientVerify.Enabled {
		clientVerifier, err := NewClientVerifier(cfg.ClientVerify)
		if err != nil {
			logging.Warn().Err(err).Msg("客户端证书验证配置失败")
		} else {
			manager.clientVerifier = clientVerifier
			clientVerifier.ConfigureTLS(tlsCfg)
		}
	}

	// 设置为默认配置
	manager.defaultCfg = tlsCfg

	return manager, nil
}

// GetTLSConfig 返回默认的 TLS 配置。
//
// 用于单服务器模式。
//
// 返回值：
//   - *tls.Config: TLS 配置对象
func (m *TLSManager) GetTLSConfig() *tls.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultCfg
}

// Close 停止 OCSP 管理器和 Session Ticket 管理器并释放资源。
func (m *TLSManager) Close() {
	if m.ocspManager != nil {
		m.ocspManager.Stop()
	}
	if m.sessionTicketMgr != nil {
		m.sessionTicketMgr.Stop()
	}
}

// getConfigForClientWithOCSP 返回启用 OCSP Stapling 的 TLS 配置。
//
// 该回调在每次 TLS 握手时调用，附加最新的 OCSP 响应。
//
// 参数：
//   - hello: 客户端 Hello 信息
//
// 返回值：
//   - *tls.Config: 带有 OCSP 响应的 TLS 配置
//   - error: 配置错误
func (m *TLSManager) getConfigForClientWithOCSP(_ *tls.ClientHelloInfo) (*tls.Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	baseCfg := m.defaultCfg

	// 无 OCSP 管理器或无证书时，返回基础配置
	if m.ocspManager == nil || len(baseCfg.Certificates) == 0 {
		return baseCfg, nil
	}

	// 创建配置副本并附加 OCSP 响应
	cfgCopy := baseCfg.Clone()

	// 将 OCSP 响应附加到证书
	cert := &cfgCopy.Certificates[0]
	if len(cert.Certificate) > 0 {
		// 使用已缓存的证书解析结果获取序列号
		m.mu.RLock()
		parsedCert := m.defaultCert
		m.mu.RUnlock()
		if parsedCert != nil {
			serial := parsedCert.SerialNumber.String()
			ocspResp := m.ocspManager.GetOCSPResponse(serial)
			if ocspResp != nil {
				// 将 OCSP 响应附加到证书
				cert.OCSPStaple = ocspResp
			}
		}
	}

	return cfgCopy, nil
}

// OCSPStatusInfo OCSP 响应状态信息。
type OCSPStatusInfo struct {
	Serial      string     // 证书序列号
	Subject     string     // 证书主题 CN
	Status      OCSPStatus // OCSP 响应状态
	HasResponse bool       // 是否有可用响应
}

// loadCertificate 从给定路径加载 TLS 证书。
//
// 如果提供了证书链路径，则合并证书链。
//
// 参数：
//   - certPath: 证书文件路径
//   - keyPath: 私钥文件路径
//   - certChainPath: 证书链文件路径（可选）
//
// 返回值：
//   - tls.Certificate: 加载的证书
//   - error: 加载失败时返回错误
func loadCertificate(certPath, keyPath, certChainPath string) (tls.Certificate, error) {
	// 加载主证书
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}

	// 合并证书链（如果提供）
	if certChainPath != "" {
		chainData, err := os.ReadFile(certChainPath)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to read certificate chain: %w", err)
		}

		// 将证书链追加到证书（每个证书作为独立的 [][]byte 条目）
		certs := parsePEMChain(chainData)
		cert.Certificate = append(cert.Certificate, certs...)
	}

	return cert, nil
}

// parsePEMChain 解析 PEM 编码的证书链数据。
//
// 返回 ASN.1 DER 编码的证书切片。
//
// 参数：
//   - data: PEM 编码的数据
//
// 返回值：
//   - [][]byte: DER 编码的证书列表
func parsePEMChain(data []byte) [][]byte {
	var certs [][]byte
	var block []byte
	rest := data

	for {
		block, rest = extractPEMBlock(rest)
		if block == nil {
			break
		}
		if len(block) > 0 {
			certs = append(certs, block)
		}
	}

	return certs
}

// extractPEMBlock 从数据中提取单个 PEM 块。
//
// 返回 DER 编码的块和剩余数据。
//
// 参数：
//   - data: PEM 数据
//
// 返回值：
//   - []byte: DER 编码的块
//   - []byte: 剩余数据
func extractPEMBlock(data []byte) ([]byte, []byte) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil
	}
	return block.Bytes, rest
}
