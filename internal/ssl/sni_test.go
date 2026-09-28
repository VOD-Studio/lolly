package ssl

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"rua.plus/lolly/internal/config"
)

// writeTestCert 生成自签名测试证书并写入临时目录，返回证书和私钥路径。
func writeTestCert(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()

	certPEM, keyPEM := generateTestCert(t)

	certPath = filepath.Join(dir, name+"-cert.pem")
	keyPath = filepath.Join(dir, name+"-key.pem")

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}
	return certPath, keyPath
}

func TestNewSNIManager_NoEntriesNoDefault(t *testing.T) {
	_, err := NewSNIManager(nil, nil)
	if err == nil {
		t.Fatal("NewSNIManager() error = nil, want error")
	}
}

func TestNewSNIManager_SelectsCorrectCertificate(t *testing.T) {
	dir := t.TempDir()
	aCert, aKey := writeTestCert(t, dir, "a")
	bCert, bKey := writeTestCert(t, dir, "b")

	mgr, err := NewSNIManager([]SNIEntry{
		{Name: "a.example.com", SSL: &config.SSLConfig{Cert: aCert, Key: aKey}},
		{Name: "b.example.com", SSL: &config.SSLConfig{Cert: bCert, Key: bKey}},
	}, nil)
	if err != nil {
		t.Fatalf("NewSNIManager() failed: %v", err)
	}
	defer mgr.Close()

	tlsCfg := mgr.TLSConfig()
	if tlsCfg.GetConfigForClient == nil {
		t.Fatal("TLSConfig() should set GetConfigForClient")
	}

	cfgA, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "a.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient(a) failed: %v", err)
	}
	cfgB, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "b.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient(b) failed: %v", err)
	}

	if len(cfgA.Certificates) == 0 || len(cfgB.Certificates) == 0 {
		t.Fatal("expected certificates to be set for both configs")
	}
	if string(cfgA.Certificates[0].Certificate[0]) == string(cfgB.Certificates[0].Certificate[0]) {
		t.Fatal("expected different certificates for different SNI hosts")
	}
}

func TestNewSNIManager_WildcardMatch(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeTestCert(t, dir, "wild")

	mgr, err := NewSNIManager([]SNIEntry{
		{Name: "*.example.com", SSL: &config.SSLConfig{Cert: cert, Key: key}},
	}, nil)
	if err != nil {
		t.Fatalf("NewSNIManager() failed: %v", err)
	}
	defer mgr.Close()

	tlsCfg := mgr.TLSConfig()
	cfg, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "www.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient() failed: %v", err)
	}
	if len(cfg.Certificates) == 0 {
		t.Fatal("expected certificate for wildcard match")
	}
}

func TestNewSNIManager_FallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	aCert, aKey := writeTestCert(t, dir, "a")
	defCert, defKey := writeTestCert(t, dir, "default")

	mgr, err := NewSNIManager(
		[]SNIEntry{{Name: "a.example.com", SSL: &config.SSLConfig{Cert: aCert, Key: aKey}}},
		&config.SSLConfig{Cert: defCert, Key: defKey},
	)
	if err != nil {
		t.Fatalf("NewSNIManager() failed: %v", err)
	}
	defer mgr.Close()

	tlsCfg := mgr.TLSConfig()

	// 未知域名应回退到默认证书
	cfg, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "unknown.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient(unknown) failed: %v", err)
	}
	if len(cfg.Certificates) == 0 {
		t.Fatal("expected fallback certificate for unmatched SNI")
	}

	// 未携带 SNI（空 ServerName）也应回退到默认证书
	cfg2, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: ""})
	if err != nil {
		t.Fatalf("GetConfigForClient(empty) failed: %v", err)
	}
	if len(cfg2.Certificates) == 0 {
		t.Fatal("expected fallback certificate for missing SNI")
	}
}

func TestNewSNIManager_NoDefaultNoMatch_FallsBackToFirst(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeTestCert(t, dir, "only")

	mgr, err := NewSNIManager([]SNIEntry{
		{Name: "a.example.com", SSL: &config.SSLConfig{Cert: cert, Key: key}},
	}, nil)
	if err != nil {
		t.Fatalf("NewSNIManager() failed: %v", err)
	}
	defer mgr.Close()

	tlsCfg := mgr.TLSConfig()
	cfg, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "unknown.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient() failed: %v", err)
	}
	if len(cfg.Certificates) == 0 {
		t.Fatal("expected fallback to first manager's certificate")
	}
}

func TestNewSNIManager_InvalidEntry(t *testing.T) {
	_, err := NewSNIManager([]SNIEntry{
		{Name: "a.example.com", SSL: &config.SSLConfig{Cert: "missing.pem", Key: "missing-key.pem"}},
	}, nil)
	if err == nil {
		t.Fatal("NewSNIManager() error = nil, want error for invalid certificate path")
	}
}

func TestNewSNIManager_InvalidRegexEntry(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeTestCert(t, dir, "a")

	_, err := NewSNIManager([]SNIEntry{
		{Name: "~(unclosed", SSL: &config.SSLConfig{Cert: cert, Key: key}},
	}, nil)
	if err == nil {
		t.Fatal("NewSNIManager() error = nil, want error for invalid regex server_name")
	}
}

func TestBuildSNIManager_NoTLSConfigured(t *testing.T) {
	mgr, err := BuildSNIManager([]config.ServerConfig{
		{Name: "a.example.com"},
		{Name: "b.example.com"},
	}, -1)
	if err != nil {
		t.Fatalf("BuildSNIManager() failed: %v", err)
	}
	if mgr != nil {
		t.Fatal("BuildSNIManager() should return nil manager when no server has TLS configured")
	}
}

func TestBuildSNIManager_MultipleServers(t *testing.T) {
	dir := t.TempDir()
	aCert, aKey := writeTestCert(t, dir, "a")
	bCert, bKey := writeTestCert(t, dir, "b")

	servers := []config.ServerConfig{
		{
			Name:    "a.example.com",
			Default: true,
			SSL:     config.SSLConfig{Cert: aCert, Key: aKey},
		},
		{
			ServerNames: []string{"b.example.com", "*.b.example.com"},
			SSL:         config.SSLConfig{Cert: bCert, Key: bKey},
		},
	}

	mgr, err := BuildSNIManager(servers, 0)
	if err != nil {
		t.Fatalf("BuildSNIManager() failed: %v", err)
	}
	if mgr == nil {
		t.Fatal("BuildSNIManager() returned nil, want a manager")
	}
	defer mgr.Close()

	tlsCfg := mgr.TLSConfig()

	cfgB, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "sub.b.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient(sub.b) failed: %v", err)
	}
	if len(cfgB.Certificates) == 0 {
		t.Fatal("expected wildcard match certificate for b's subdomain")
	}

	cfgDefault, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "unmatched.example.org"})
	if err != nil {
		t.Fatalf("GetConfigForClient(unmatched) failed: %v", err)
	}
	if len(cfgDefault.Certificates) == 0 {
		t.Fatal("expected default certificate for unmatched host")
	}
}

// TestSNIManager_RejectHandshake_DefaultRejectsUnknownSNI 验证默认虚拟主机
// 配置了 ssl_reject_handshake 时，未知 SNI 的握手被直接拒绝（返回错误），
// 而已知 SNI 仍正常返回对应证书。
func TestSNIManager_RejectHandshake_DefaultRejectsUnknownSNI(t *testing.T) {
	dir := t.TempDir()
	aCert, aKey := writeTestCert(t, dir, "a")

	// 默认主机仅拒绝握手，不提供证书；a.example.com 提供真实证书
	mgr, err := NewSNIManager(
		[]SNIEntry{{Name: "a.example.com", SSL: &config.SSLConfig{Cert: aCert, Key: aKey}}},
		&config.SSLConfig{RejectHandshake: true},
	)
	if err != nil {
		t.Fatalf("NewSNIManager() failed: %v", err)
	}
	defer mgr.Close()

	tlsCfg := mgr.TLSConfig()

	// 已知 SNI 正常返回证书
	cfg, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "a.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient(known) failed: %v", err)
	}
	if len(cfg.Certificates) == 0 {
		t.Fatal("expected certificate for known SNI")
	}

	// 未知 SNI 应被拒绝
	if _, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "unknown.example.com"}); err == nil {
		t.Fatal("expected handshake rejection for unknown SNI, got nil error")
	}

	// 未携带 SNI 同样命中默认主机，应被拒绝
	if _, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: ""}); err == nil {
		t.Fatal("expected handshake rejection for missing SNI, got nil error")
	}
}

// TestSNIManager_RejectHandshake_NamedEntryRejectsMatchedSNI 验证具名虚拟主机
// 配置了 ssl_reject_handshake 时，匹配到它的 SNI 也被拒绝。
func TestSNIManager_RejectHandshake_NamedEntryRejectsMatchedSNI(t *testing.T) {
	dir := t.TempDir()
	aCert, aKey := writeTestCert(t, dir, "a")
	defCert, defKey := writeTestCert(t, dir, "default")

	mgr, err := NewSNIManager(
		[]SNIEntry{
			{Name: "a.example.com", SSL: &config.SSLConfig{Cert: aCert, Key: aKey}},
			{Name: "blocked.example.com", SSL: &config.SSLConfig{RejectHandshake: true}},
		},
		&config.SSLConfig{Cert: defCert, Key: defKey},
	)
	if err != nil {
		t.Fatalf("NewSNIManager() failed: %v", err)
	}
	defer mgr.Close()

	tlsCfg := mgr.TLSConfig()

	// 正常主机返回证书
	cfg, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "a.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient(a) failed: %v", err)
	}
	if len(cfg.Certificates) == 0 {
		t.Fatal("expected certificate for a.example.com")
	}

	// 被拒绝的主机匹配后返回握手失败
	if _, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "blocked.example.com"}); err == nil {
		t.Fatal("expected handshake rejection for blocked.example.com, got nil error")
	}

	// 未知主机回退到默认证书，不被拒绝
	cfg2, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "unknown.example.org"})
	if err != nil {
		t.Fatalf("GetConfigForClient(unknown) failed: %v", err)
	}
	if len(cfg2.Certificates) == 0 {
		t.Fatal("expected default certificate for unmatched host")
	}
}

// TestBuildSNIManager_RejectHandshake_CatchAllDefault 验证通过 BuildSNIManager
// 构建的仅拒绝握手的默认服务器（无 server_name、无证书）能正确拒绝未知 SNI。
func TestBuildSNIManager_RejectHandshake_CatchAllDefault(t *testing.T) {
	dir := t.TempDir()
	aCert, aKey := writeTestCert(t, dir, "a")

	servers := []config.ServerConfig{
		// 仅拒绝握手的 catch-all 默认服务器，无 server_name、无证书
		{
			Default: true,
			SSL:     config.SSLConfig{RejectHandshake: true},
		},
		// 正常虚拟主机
		{
			Name: "a.example.com",
			SSL:  config.SSLConfig{Cert: aCert, Key: aKey},
		},
	}

	mgr, err := BuildSNIManager(servers, 0)
	if err != nil {
		t.Fatalf("BuildSNIManager() failed: %v", err)
	}
	if mgr == nil {
		t.Fatal("BuildSNIManager() returned nil, want manager")
	}
	defer mgr.Close()

	tlsCfg := mgr.TLSConfig()

	// 已知 SNI 返回证书
	cfg, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "a.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient(known) failed: %v", err)
	}
	if len(cfg.Certificates) == 0 {
		t.Fatal("expected certificate for known SNI")
	}

	// 未知 SNI 命中拒绝默认主机
	if _, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "evil.example.org"}); err == nil {
		t.Fatal("expected handshake rejection for unknown SNI, got nil error")
	}

	// 缺失 SNI 同样被拒绝
	if _, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: ""}); err == nil {
		t.Fatal("expected handshake rejection for missing SNI, got nil error")
	}
}

// TestTLSManager_RejectHandshake_SingleServer 验证单服务器模式下
// 拒绝握手的管理器对任意 ClientHello 都返回握手失败。
func TestTLSManager_RejectHandshake_SingleServer(t *testing.T) {
	mgr, err := NewTLSManager(&config.SSLConfig{RejectHandshake: true})
	if err != nil {
		t.Fatalf("NewTLSManager() failed: %v", err)
	}
	defer mgr.Close()

	if !mgr.RejectHandshake() {
		t.Fatal("expected RejectHandshake() to return true")
	}

	cfg := mgr.GetTLSConfig()
	if cfg.GetConfigForClient == nil {
		t.Fatal("expected GetConfigForClient to be set for reject-handshake manager")
	}
	if _, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "anything.example.com"}); err == nil {
		t.Fatal("expected handshake rejection, got nil error")
	}
}
