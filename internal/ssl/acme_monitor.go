// Package ssl 提供 SSL/TLS 支持。
//
// 该文件包含 ACME 证书到期监控逻辑，包括：
//   - 扫描 ACME 状态目录中已签发的证书
//   - 周期性检查并在临近到期时输出告警
//
// 主要用途：
//
//	autocert 会在到期前自动续期，但续期持续失败（DNS、网络、CA 速率限制等）
//	时证书会静默临近过期。本监控提供"到期前 X 天"的可见性，便于及时介入。
//
// 注意事项：
//   - 仅读取状态目录，不修改任何文件
//   - 目录中证书文件由 autocert 以域名命名，非域名文件会被忽略
//
// 作者：xfy
package ssl

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rua.plus/lolly/internal/logging"
)

// ACME 证书到期监控默认参数。
const (
	// defaultCertMonitorInterval 默认检查间隔
	defaultCertMonitorInterval = 12 * time.Hour
	// acmeCertWarnDays 剩余天数低于该值时输出 warn
	acmeCertWarnDays = 30
	// acmeCertCriticalDays 剩余天数低于该值时输出 error
	acmeCertCriticalDays = 7
)

// CertExpiry 描述一张已签发证书的到期信息。
type CertExpiry struct {
	// Domain 证书对应的域名
	Domain string
	// NotAfter 证书失效时间
	NotAfter time.Time
	// DaysLeft 距失效的剩余天数（可能为负，表示已过期）
	DaysLeft int
}

// ScanACMEDir 扫描单个 ACME 状态目录，返回其中已签发证书的到期信息。
//
// 目录不存在时返回空结果且不报错（首次启动尚未签发任何证书）。
//
// 参数：
//   - statePath: ACME 状态目录
//   - now: 用于计算剩余天数的基准时间
//
// 返回值：
//   - []CertExpiry: 证书到期信息列表
//   - error: 读取目录失败时返回错误
func ScanACMEDir(statePath string, now time.Time) ([]CertExpiry, error) {
	if statePath == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []CertExpiry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		// autocert 的文件名约定：证书为 <域名>，另有 <域名>+rsa、<域名>+token
		// 与 acme_account+key 等辅助文件，只保留纯域名的证书文件
		name := e.Name()
		if strings.Contains(name, "+") || strings.HasPrefix(name, "acme_account") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(statePath, name))
		if err != nil {
			continue
		}

		notAfter, ok := firstCertNotAfter(data)
		if !ok {
			continue
		}

		out = append(out, CertExpiry{
			Domain:   name,
			NotAfter: notAfter,
			DaysLeft: int(notAfter.Sub(now).Hours() / 24),
		})
	}

	return out, nil
}

// firstCertNotAfter 从 PEM 数据中提取第一张证书的失效时间。
//
// 状态文件同时包含证书链与私钥，这里只关心首个 CERTIFICATE 块。
//
// 参数：
//   - data: PEM 编码的数据
//
// 返回值：
//   - time.Time: 证书失效时间
//   - bool: 未找到可解析证书时返回 false
func firstCertNotAfter(data []byte) (time.Time, bool) {
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return time.Time{}, false
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, false
		}
		return cert.NotAfter, true
	}
}

// CertMonitor 周期性检查 ACME 证书到期情况并输出告警日志。
//
// 所有方法均为并发安全；Stop 可重复调用。
type CertMonitor struct {
	statePaths []string
	interval   time.Duration
	stopCh     chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

// NewCertMonitor 创建证书到期监控。
//
// 参数：
//   - statePaths: 需要监控的 ACME 状态目录列表，重复项会被去重
//
// 返回值：
//   - *CertMonitor: 监控实例
func NewCertMonitor(statePaths []string) *CertMonitor {
	return &CertMonitor{
		statePaths: uniqueACMEDirs(statePaths),
		interval:   defaultCertMonitorInterval,
		stopCh:     make(chan struct{}),
	}
}

// SetInterval 设置检查间隔，主要用于测试。
//
// 参数：
//   - d: 检查间隔，非正值时忽略
func (m *CertMonitor) SetInterval(d time.Duration) {
	if d > 0 {
		m.interval = d
	}
}

// Start 启动后台检查协程。
//
// 无状态目录时不做任何事。启动后立即检查一次，之后按间隔周期检查。
func (m *CertMonitor) Start() {
	if m == nil || len(m.statePaths) == 0 {
		return
	}
	m.wg.Add(1)
	go m.loop()
}

// Stop 停止后台检查协程，可重复调用。
func (m *CertMonitor) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.wg.Wait()
}

// loop 后台检查循环。
func (m *CertMonitor) loop() {
	defer m.wg.Done()

	m.check()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.check()
		}
	}
}

// Scan 聚合所有状态目录的证书到期信息。
//
// 返回值：
//   - []CertExpiry: 所有目录中证书的到期信息
func (m *CertMonitor) Scan() []CertExpiry {
	if m == nil {
		return nil
	}
	now := time.Now()
	var out []CertExpiry
	for _, path := range m.statePaths {
		certs, err := ScanACMEDir(path, now)
		if err != nil {
			logging.Warn().Err(err).Str("statePath", path).Msg("ACME 证书到期检查失败")
			continue
		}
		out = append(out, certs...)
	}
	return out
}

// check 执行一次检查并根据剩余天数输出分级告警。
func (m *CertMonitor) check() {
	for _, c := range m.Scan() {
		switch {
		case c.DaysLeft <= 0:
			logging.Error().
				Str("domain", c.Domain).
				Int("daysLeft", c.DaysLeft).
				Time("notAfter", c.NotAfter).
				Msg("ACME 证书已过期，请检查续期是否失败")
		case c.DaysLeft < acmeCertCriticalDays:
			logging.Error().
				Str("domain", c.Domain).
				Int("daysLeft", c.DaysLeft).
				Time("notAfter", c.NotAfter).
				Msg("ACME 证书临近过期，请检查续期是否失败")
		case c.DaysLeft < acmeCertWarnDays:
			logging.Warn().
				Str("domain", c.Domain).
				Int("daysLeft", c.DaysLeft).
				Time("notAfter", c.NotAfter).
				Msg("ACME 证书即将到期")
		}
	}
}

// uniqueACMEDirs 去重并剔除空路径。
func uniqueACMEDirs(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	var out []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
