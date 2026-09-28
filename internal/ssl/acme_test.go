// Package ssl 提供 SSL/TLS 功能的测试。
//
// 该文件测试 ACME 自动证书相关功能，包括：
//   - ACME 管理器创建与配置
//   - 域名白名单与通配符处理
//   - 与 TLSManager / SNIManager 的集成
//
// 注意：这些测试不发起真实网络请求（不调用 GetCertificate），
// 仅验证配置装配与 TLS 配置是否符合预期。
//
// 作者：xfy
package ssl

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"

	"rua.plus/lolly/internal/config"
)

// TestNewACMEManager_Disabled 验证未启用时返回 nil。
func TestNewACMEManager_Disabled(t *testing.T) {
	mgr, err := NewACMEManager(nil, nil)
	if err != nil {
		t.Fatalf("NewACMEManager(nil) error = %v", err)
	}
	if mgr != nil {
		t.Fatal("NewACMEManager(nil) should return nil manager")
	}

	mgr, err = NewACMEManager(&config.ACMEConfig{Enabled: false}, nil)
	if err != nil {
		t.Fatalf("NewACMEManager(disabled) error = %v", err)
	}
	if mgr != nil {
		t.Fatal("NewACMEManager(disabled) should return nil manager")
	}
}

// TestNewACMEManager_Defaults 验证默认值：tls-alpn-01 挑战、白名单生效。
func TestNewACMEManager_Defaults(t *testing.T) {
	cfg := &config.ACMEConfig{
		Enabled:   true,
		StatePath: t.TempDir(),
		Email:     "admin@example.com",
		Hosts:     []string{"example.com", "www.example.com"},
	}

	mgr, err := NewACMEManager(cfg, cfg.Hosts)
	if err != nil {
		t.Fatalf("NewACMEManager() error = %v", err)
	}
	if mgr == nil {
		t.Fatal("NewACMEManager() returned nil, want manager")
	}

	if mgr.HTTP01() {
		t.Error("default challenge should be tls-alpn-01, HTTP01() = true")
	}

	// 白名单命中
	if !mgr.HasHost("example.com") {
		t.Error("HasHost(example.com) = false, want true")
	}
	// 大小写与尾点应被规范化
	if !mgr.HasHost("EXAMPLE.com.") {
		t.Error("HasHost(EXAMPLE.com.) = false, want true")
	}
	// 未配置的域名应被拒绝
	if mgr.HasHost("evil.example.net") {
		t.Error("HasHost(evil.example.net) = true, want false")
	}
}

// TestNewACMEManager_HTTP01 验证 http-01 挑战类型。
func TestNewACMEManager_HTTP01(t *testing.T) {
	mgr, err := NewACMEManager(&config.ACMEConfig{
		Enabled:   true,
		StatePath: t.TempDir(),
		Challenge: config.ACMEChallengeHTTP01,
	}, []string{"example.com"})
	if err != nil {
		t.Fatalf("NewACMEManager() error = %v", err)
	}
	if !mgr.HTTP01() {
		t.Error("HTTP01() = false, want true for http-01 challenge")
	}
}

// TestNewACMEManager_WildcardIgnored 验证通配符域名被剔除。
func TestNewACMEManager_WildcardIgnored(t *testing.T) {
	mgr, err := NewACMEManager(&config.ACMEConfig{
		Enabled:   true,
		StatePath: t.TempDir(),
	}, []string{"*.example.com", "example.com"})
	if err != nil {
		t.Fatalf("NewACMEManager() error = %v", err)
	}
	if mgr.HasHost("*.example.com") {
		t.Error("wildcard host should not be in whitelist")
	}
	if !mgr.HasHost("example.com") {
		t.Error("exact host should remain in whitelist")
	}
}

// TestNewACMEManager_WildcardOnly 验证仅通配符域名时直接报错，避免放开限制。
func TestNewACMEManager_WildcardOnly(t *testing.T) {
	_, err := NewACMEManager(&config.ACMEConfig{
		Enabled:   true,
		StatePath: t.TempDir(),
	}, []string{"*.example.com"})
	if err == nil {
		t.Fatal("NewACMEManager() error = nil, want error when only wildcards configured")
	}
}

// TestNewACMEManager_NoWhitelist 验证无白名单时不限制域名。
func TestNewACMEManager_NoWhitelist(t *testing.T) {
	mgr, err := NewACMEManager(&config.ACMEConfig{
		Enabled:   true,
		StatePath: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("NewACMEManager() error = %v", err)
	}
	if !mgr.HasHost("anything.example.com") {
		t.Error("without whitelist HasHost should return true")
	}
}

// TestNewACMEManager_InvalidEAB 验证非法 EAB 密钥返回错误。
func TestNewACMEManager_InvalidEAB(t *testing.T) {
	_, err := NewACMEManager(&config.ACMEConfig{
		Enabled:    true,
		StatePath:  t.TempDir(),
		EABKid:     "kid",
		EABHmacKey: "!!!not-base64url!!!",
	}, []string{"example.com"})
	if err == nil {
		t.Fatal("NewACMEManager() error = nil, want error for invalid EAB key")
	}
	if !strings.Contains(err.Error(), "eab_hmac_key") {
		t.Errorf("error = %v, want it to mention eab_hmac_key", err)
	}
}

// TestTLSManager_ACME 验证无静态证书时 ACME 接管证书来源。
func TestTLSManager_ACME(t *testing.T) {
	cfg := &config.SSLConfig{
		ACME: config.ACMEConfig{
			Enabled:   true,
			StatePath: t.TempDir(),
			Hosts:     []string{"example.com"},
		},
	}

	manager, err := NewTLSManager(cfg)
	if err != nil {
		t.Fatalf("NewTLSManager() error = %v", err)
	}
	defer manager.Close()

	tlsCfg := manager.GetTLSConfig()
	if tlsCfg.GetCertificate == nil {
		t.Error("GetCertificate should be set when ACME is enabled")
	}
	if len(tlsCfg.Certificates) != 0 {
		t.Errorf("Certificates should be empty for ACME-only config, got %d", len(tlsCfg.Certificates))
	}
	if !containsString(strings.Join(tlsCfg.NextProtos, ","), ACMEALPNProto()) {
		t.Errorf("NextProtos = %v, want it to contain %q", tlsCfg.NextProtos, ACMEALPNProto())
	}
	if manager.ACMEManager() == nil {
		t.Error("ACMEManager() should return the created manager")
	}
}

// TestTLSManager_StaticCertOverridesACME 验证静态证书优先于 ACME。
func TestTLSManager_StaticCertOverridesACME(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestCert(t, dir, "static")

	cfg := &config.SSLConfig{
		Cert: certPath,
		Key:  keyPath,
		ACME: config.ACMEConfig{
			Enabled:   true,
			StatePath: t.TempDir(),
			Hosts:     []string{"example.com"},
		},
	}

	manager, err := NewTLSManager(cfg)
	if err != nil {
		t.Fatalf("NewTLSManager() error = %v", err)
	}
	defer manager.Close()

	tlsCfg := manager.GetTLSConfig()
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("Certificates = %d, want 1 (static cert)", len(tlsCfg.Certificates))
	}
	if tlsCfg.GetCertificate != nil {
		t.Error("GetCertificate should be nil when static cert is used")
	}
}

// TestTLSManager_ACMEInjectedManager 验证注入的 ACME 管理器被复用。
func TestTLSManager_ACMEInjectedManager(t *testing.T) {
	acmeMgr, err := NewACMEManager(&config.ACMEConfig{
		Enabled:   true,
		StatePath: t.TempDir(),
	}, []string{"example.com"})
	if err != nil {
		t.Fatalf("NewACMEManager() error = %v", err)
	}

	cfg := &config.SSLConfig{
		ACME: config.ACMEConfig{Enabled: true, Hosts: []string{"example.com"}},
	}

	manager, err := NewTLSManager(cfg, WithACMEManager(acmeMgr))
	if err != nil {
		t.Fatalf("NewTLSManager() error = %v", err)
	}
	defer manager.Close()

	if manager.ACMEManager() != acmeMgr {
		t.Error("injected ACME manager should be reused")
	}
}

// TestTLSManager_ACMECertAndKeyRequired 验证既无静态证书又未启用 ACME 时报错。
func TestTLSManager_ACMECertAndKeyRequired(t *testing.T) {
	_, err := NewTLSManager(&config.SSLConfig{})
	if err == nil {
		t.Fatal("NewTLSManager() error = nil, want error when no cert and no ACME")
	}
	if !strings.Contains(err.Error(), "certificate and key paths are required") {
		t.Errorf("error = %v, want cert/key required message", err)
	}
}

// TestBuildSNIManager_ACMEWithoutCert 验证启用 ACME 的服务器无需静态证书，
// 且注入的管理器在 entry 与 default 之间复用（去重）。
func TestBuildSNIManager_ACMEWithoutCert(t *testing.T) {
	servers := []config.ServerConfig{
		{
			Name:    "a.example.com",
			Default: true,
			SSL: config.SSLConfig{
				ACME: config.ACMEConfig{
					Enabled:   true,
					StatePath: t.TempDir(),
					Hosts:     []string{"a.example.com"},
				},
			},
		},
	}

	injected, err := NewACMEManager(&servers[0].SSL.ACME, servers[0].SSL.ACME.Hosts)
	if err != nil {
		t.Fatalf("NewACMEManager() error = %v", err)
	}

	mgr, err := BuildSNIManager(servers, 0, WithSNIACMEManagers(map[int]*ACMEManager{0: injected}))
	if err != nil {
		t.Fatalf("BuildSNIManager() error = %v", err)
	}
	if mgr == nil {
		t.Fatal("BuildSNIManager() returned nil, want manager for ACME server")
	}
	defer mgr.Close()

	acmes := mgr.ACMEManagers()
	if len(acmes) != 1 {
		t.Fatalf("ACMEManagers() len = %d, want 1 (deduplicated)", len(acmes))
	}
	if acmes[0] != injected {
		t.Error("ACMEManagers() should return the injected manager")
	}

	tlsCfg := mgr.TLSConfig()
	got, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "a.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient() error = %v", err)
	}
	if got.GetCertificate == nil {
		t.Error("selected config should have GetCertificate set for ACME")
	}
}

// TestACMEManager_HTTPHandler 验证 HTTP 处理器可正常构造。
func TestACMEManager_HTTPHandler(t *testing.T) {
	mgr, err := NewACMEManager(&config.ACMEConfig{
		Enabled:   true,
		StatePath: t.TempDir(),
		Challenge: config.ACMEChallengeHTTP01,
	}, []string{"example.com"})
	if err != nil {
		t.Fatalf("NewACMEManager() error = %v", err)
	}

	var h http.Handler = mgr.HTTPHandler(nil)
	if h == nil {
		t.Fatal("HTTPHandler() returned nil")
	}
}
