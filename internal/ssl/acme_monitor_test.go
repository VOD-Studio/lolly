// Package ssl 提供 SSL/TLS 功能的测试。
//
// 该文件测试 ACME 证书到期监控，包括：
//   - 状态目录扫描与证书到期解析
//   - 监控器的启动/停止与扫描聚合
//
// 作者：xfy
package ssl

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeACMEFakeCert 在 statePath 下写入一张证书文件，NotAfter 由 notAfter 指定。
func writeACMEFakeCert(t *testing.T, statePath, domain string, notAfter time.Time) {
	t.Helper()
	certPEM := generateTestCertWithNotAfter(t, notAfter)
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", statePath, err)
	}
	if err := os.WriteFile(filepath.Join(statePath, domain), certPEM, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", domain, err)
	}
}

// generateTestCertWithNotAfter 生成一张指定失效时间的自签证书（仅 PEM 证书块）。
func generateTestCertWithNotAfter(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Test"}},
		NotBefore:    time.Now(),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("Failed to create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

// TestScanACMEDir_Empty 验证空路径返回 nil。
func TestScanACMEDir_Empty(t *testing.T) {
	out, err := ScanACMEDir("", time.Now())
	if err != nil {
		t.Fatalf("ScanACMEDir() error = %v", err)
	}
	if out != nil {
		t.Errorf("ScanACMEDir(\"\") = %v, want nil", out)
	}
}

// TestScanACMEDir_NotExist 验证目录不存在时返回空且不报错。
func TestScanACMEDir_NotExist(t *testing.T) {
	out, err := ScanACMEDir("/nonexistent/acme/state", time.Now())
	if err != nil {
		t.Fatalf("ScanACMEDir() error = %v", err)
	}
	if len(out) != 0 {
		t.Errorf("ScanACMEDir(notexist) len = %d, want 0", len(out))
	}
}

// TestScanACMEDir_ParsesAndFilters 验证解析证书到期、过滤辅助文件。
func TestScanACMEDir_ParsesAndFilters(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// 正常证书：剩余约 10 天
	writeACMEFakeCert(t, dir, "example.com", now.Add(10*24*time.Hour))
	// 临近过期：剩余 3 天
	writeACMEFakeCert(t, dir, "www.example.com", now.Add(3*24*time.Hour))
	// 已过期
	writeACMEFakeCert(t, dir, "expired.example.com", now.Add(-2*24*time.Hour))

	// 辅助文件应被忽略
	if err := os.WriteFile(filepath.Join(dir, "example.com+rsa"), []byte("noise"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "example.com+token"), []byte("noise"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acme_account+key"), []byte("noise"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 不可解析的文件应被跳过
	if err := os.WriteFile(filepath.Join(dir, "garbage.example.com"), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := ScanACMEDir(dir, now)
	if err != nil {
		t.Fatalf("ScanACMEDir() error = %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("ScanACMEDir() len = %d, want 3 (filtered out aux files)", len(out))
	}

	byDomain := make(map[string]CertExpiry, len(out))
	for _, c := range out {
		byDomain[c.Domain] = c
	}
	if c, ok := byDomain["example.com"]; !ok {
		t.Error("missing example.com")
	} else if c.DaysLeft < 9 || c.DaysLeft > 10 {
		t.Errorf("example.com DaysLeft = %d, want 9-10", c.DaysLeft)
	}
	if c, ok := byDomain["www.example.com"]; !ok {
		t.Error("missing www.example.com")
	} else if c.DaysLeft < 2 || c.DaysLeft > 3 {
		t.Errorf("www.example.com DaysLeft = %d, want 2-3", c.DaysLeft)
	}
	if c, ok := byDomain["expired.example.com"]; !ok {
		t.Error("missing expired.example.com")
	} else if c.DaysLeft >= 0 {
		t.Errorf("expired.example.com DaysLeft = %d, want negative", c.DaysLeft)
	}
}

// TestCertMonitor_ScanAggregates 验证监控器聚合多目录。
func TestCertMonitor_ScanAggregates(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	now := time.Now()

	writeACMEFakeCert(t, dir1, "a.example.com", now.Add(20*24*time.Hour))
	writeACMEFakeCert(t, dir2, "b.example.com", now.Add(5*24*time.Hour))

	m := NewCertMonitor([]string{dir1, dir2, dir2}) // dir2 重复应去重
	out := m.Scan()
	if len(out) != 2 {
		t.Fatalf("Scan() len = %d, want 2 (deduped)", len(out))
	}
}

// TestCertMonitor_StartStop 验证启动/停止不阻塞、可重复 Stop。
func TestCertMonitor_StartStop(t *testing.T) {
	m := NewCertMonitor([]string{t.TempDir()})
	m.SetInterval(10 * time.Millisecond)
	m.Start()
	// 立即停止不应阻塞或 panic
	m.Stop()
	m.Stop() // 重复调用安全
}

// TestCertMonitor_NoPathsNoOp 验证无状态目录时不启动协程。
func TestCertMonitor_NoPathsNoOp(t *testing.T) {
	m := NewCertMonitor(nil)
	m.Start() // 不应 panic
	m.Stop()
	if m.Scan() != nil {
		t.Error("Scan() = non-nil, want nil with no paths")
	}
}
