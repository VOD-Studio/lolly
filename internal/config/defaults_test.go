// Package config 提供默认配置生成功能的测试。
//
// 该文件测试默认配置模块的各项功能，包括：
//   - DefaultConfig 默认值验证
//   - GenerateConfigYAML YAML 生成测试
//   - 性能配置默认值测试
//
// 作者：xfy
package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	// TestDefaultConfig 测试默认配置生成。
	cfg := DefaultConfig()

	// 验证 Listen 默认值
	if cfg.Servers[0].Listen != ":8080" {
		t.Errorf("Server.Listen 期望 :8080, 实际 %s", cfg.Servers[0].Listen)
	}

	// 验证 SSL 默认版本
	if len(cfg.Servers[0].SSL.Protocols) != 2 {
		t.Errorf("SSL.Protocols 期望 2 个版本，实际 %d", len(cfg.Servers[0].SSL.Protocols))
	}
	expectedProtocols := []string{"TLSv1.2", "TLSv1.3"}
	for i, proto := range cfg.Servers[0].SSL.Protocols {
		if proto != expectedProtocols[i] {
			t.Errorf("SSL.Protocols[%d] 期望 %s, 实际 %s", i, expectedProtocols[i], proto)
		}
	}

	// 验证 HSTS 默认值
	if cfg.Servers[0].SSL.HSTS.MaxAge != 31536000 {
		t.Errorf("HSTS.MaxAge 期望 31536000, 实际 %d", cfg.Servers[0].SSL.HSTS.MaxAge)
	}
	if !cfg.Servers[0].SSL.HSTS.IncludeSubDomains {
		t.Errorf("HSTS.IncludeSubDomains 期望 true, 实际 %v", cfg.Servers[0].SSL.HSTS.IncludeSubDomains)
	}
	if cfg.Servers[0].SSL.HSTS.Preload {
		t.Errorf("HSTS.Preload 期望 false, 实际 %v", cfg.Servers[0].SSL.HSTS.Preload)
	}

	// 验证压缩默认值
	if cfg.Servers[0].Compression.Type != "gzip" {
		t.Errorf("Compression.Type 期望 gzip, 实际 %s", cfg.Servers[0].Compression.Type)
	}
	if cfg.Servers[0].Compression.Level != 6 {
		t.Errorf("Compression.Level 期望 6, 实际 %d", cfg.Servers[0].Compression.Level)
	}
	if cfg.Servers[0].Compression.MinSize != 1024 {
		t.Errorf("Compression.MinSize 期望 1024, 实际 %d", cfg.Servers[0].Compression.MinSize)
	}
	expectedTypes := []string{"text/html", "text/css", "text/javascript", "application/json", "application/javascript"}
	for i, ct := range cfg.Servers[0].Compression.Types {
		if ct != expectedTypes[i] {
			t.Errorf("Compression.Types[%d] 期望 %s, 实际 %s", i, expectedTypes[i], ct)
		}
	}
}

func TestDefaultConfigGeoIPAndAuthRequest(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Servers[0].Security.Access.GeoIP.CacheSize != 10000 {
		t.Errorf("GeoIP.CacheSize = %d, want 10000", cfg.Servers[0].Security.Access.GeoIP.CacheSize)
	}
	if cfg.Servers[0].Security.Access.GeoIP.CacheTTL != 3600*time.Second {
		t.Errorf("GeoIP.CacheTTL = %v, want 3600s", cfg.Servers[0].Security.Access.GeoIP.CacheTTL)
	}
	if cfg.Servers[0].Security.AuthRequest.Timeout != 5*time.Second {
		t.Errorf("AuthRequest.Timeout = %v, want 5s", cfg.Servers[0].Security.AuthRequest.Timeout)
	}
}

func TestGenerateConfigYAMLFieldsCoverage(t *testing.T) {
	cfg := DefaultConfig()
	yamlData, err := GenerateConfigYAML(cfg)
	if err != nil {
		t.Fatalf("GenerateConfigYAML failed: %v", err)
	}
	yamlStr := string(yamlData)

	checks := []struct {
		typ  reflect.Type
		name string
	}{
		{reflect.TypeFor[GeoIPConfig](), "GeoIPConfig"},
		{reflect.TypeFor[AuthRequestConfig](), "AuthRequestConfig"},
		{reflect.TypeFor[LuaGlobalSettings](), "LuaGlobalSettings"},
		{reflect.TypeFor[LimitRateConfig](), "LimitRateConfig"},
		{reflect.TypeFor[TypesConfig](), "TypesConfig"},
		{reflect.TypeFor[ContainerDiscoveryConfig](), "ContainerDiscoveryConfig"},
		{reflect.TypeFor[ContainerDiscoveryACMEConfig](), "ContainerDiscoveryACMEConfig"},
	}

	for _, c := range checks {
		for field := range c.typ.Fields() {
			tag := field.Tag.Get("yaml")
			fieldName := strings.Split(tag, ",")[0]
			if fieldName == "" || fieldName == "-" {
				continue
			}
			if !strings.Contains(yamlStr, fieldName) {
				t.Errorf("%s.%s (yaml:%q) not found in GenerateConfigYAML output", c.name, field.Name, fieldName)
			}
		}
	}
}

func TestGenerateConfigYAML(t *testing.T) {
	// TestGenerateConfigYAML 测试 YAML 配置生成。
	cfg := DefaultConfig()

	yamlData, err := GenerateConfigYAML(cfg)
	if err != nil {
		t.Fatalf("GenerateConfigYAML 返回错误：%v", err)
	}

	// 验证输出非空
	if len(yamlData) == 0 {
		t.Error("GenerateConfigYAML 输出为空")
	}

	yamlStr := string(yamlData)

	// 验证包含注释
	if !strings.Contains(yamlStr, "#") {
		t.Error("YAML 输出未包含注释")
	}
	if !strings.Contains(yamlStr, "# Lolly 配置文件") {
		t.Error("YAML 输出未包含文件头注释")
	}
	if !strings.Contains(yamlStr, "# 服务器配置") {
		t.Error("YAML 输出未包含服务器配置注释")
	}
}

func TestDefaultConfigPerformance(t *testing.T) {
	// TestDefaultConfigPerformance 测试性能配置默认值。
	cfg := DefaultConfig()

	// 验证 GoroutinePool 默认值
	if cfg.Performance.GoroutinePool.Enabled {
		t.Errorf("GoroutinePool.Enabled 期望 false, 实际 %v", cfg.Performance.GoroutinePool.Enabled)
	}
	if cfg.Performance.GoroutinePool.MaxWorkers != 1000 {
		t.Errorf("GoroutinePool.MaxWorkers 期望 1000, 实际 %d", cfg.Performance.GoroutinePool.MaxWorkers)
	}
	if cfg.Performance.GoroutinePool.MinWorkers != 10 {
		t.Errorf("GoroutinePool.MinWorkers 期望 10, 实际 %d", cfg.Performance.GoroutinePool.MinWorkers)
	}
	if cfg.Performance.GoroutinePool.IdleTimeout != 60*time.Second {
		t.Errorf("GoroutinePool.IdleTimeout 期望 60s, 实际 %v", cfg.Performance.GoroutinePool.IdleTimeout)
	}

	// 验证 FileCache 默认值
	if cfg.Performance.FileCache.MaxEntries != 10000 {
		t.Errorf("FileCache.MaxEntries 期望 10000, 实际 %d", cfg.Performance.FileCache.MaxEntries)
	}
	if cfg.Performance.FileCache.MaxSize != 256*1024*1024 {
		t.Errorf("FileCache.MaxSize 期望 256MB, 实际 %d", cfg.Performance.FileCache.MaxSize)
	}
	if cfg.Performance.FileCache.Inactive != 20*time.Second {
		t.Errorf("FileCache.Inactive 期望 20s, 实际 %v", cfg.Performance.FileCache.Inactive)
	}

	// 验证 Transport 默认值
	if cfg.Performance.Transport.IdleConnTimeout != 90*time.Second {
		t.Errorf("Transport.IdleConnTimeout 期望 90s, 实际 %v", cfg.Performance.Transport.IdleConnTimeout)
	}
	if cfg.Performance.Transport.MaxConnsPerHost != 512 {
		t.Errorf("Transport.MaxConnsPerHost 期望 512 (fasthttp 推荐), 实际 %d", cfg.Performance.Transport.MaxConnsPerHost)
	}
}

func TestDefaultConfigResolver(t *testing.T) {
	// TestDefaultConfigResolver 测试 Resolver 默认值。
	cfg := DefaultConfig()

	// 验证 Resolver 默认值
	if cfg.Resolver.Enabled {
		t.Error("Resolver.Enabled 期望 false")
	}
	if len(cfg.Resolver.Addresses) != 2 {
		t.Errorf("Resolver.Addresses 期望 2 个 DNS 服务器，实际 %d", len(cfg.Resolver.Addresses))
	}
	if cfg.Resolver.Valid != 30*time.Second {
		t.Errorf("Resolver.Valid 期望 30s，实际 %v", cfg.Resolver.Valid)
	}
	if cfg.Resolver.Timeout != 5*time.Second {
		t.Errorf("Resolver.Timeout 期望 5s，实际 %v", cfg.Resolver.Timeout)
	}
	if !cfg.Resolver.IPv4 {
		t.Error("Resolver.IPv4 期望 true")
	}
	if cfg.Resolver.IPv6 {
		t.Error("Resolver.IPv6 期望 false")
	}
	if cfg.Resolver.CacheSize != 1024 {
		t.Errorf("Resolver.CacheSize 期望 1024，实际 %d", cfg.Resolver.CacheSize)
	}
}

func TestDefaultConfigSSLDefaults(t *testing.T) {
	// TestDefaultConfigSSLDefaults 测试 SSL 相关默认值。
	cfg := DefaultConfig()

	// 验证 SessionTickets 默认值
	if cfg.Servers[0].SSL.SessionTickets.Enabled {
		t.Error("SessionTickets.Enabled 期望 false")
	}
	if cfg.Servers[0].SSL.SessionTickets.RetainKeys != 3 {
		t.Errorf("SessionTickets.RetainKeys 期望 3，实际 %d", cfg.Servers[0].SSL.SessionTickets.RetainKeys)
	}

	// 验证 ClientVerify 默认值
	if cfg.Servers[0].SSL.ClientVerify.Enabled {
		t.Error("ClientVerify.Enabled 期望 false")
	}
	if cfg.Servers[0].SSL.ClientVerify.Mode != "none" {
		t.Errorf("ClientVerify.Mode 期望 none，实际 %s", cfg.Servers[0].SSL.ClientVerify.Mode)
	}
	if cfg.Servers[0].SSL.ClientVerify.VerifyDepth != 1 {
		t.Errorf("ClientVerify.VerifyDepth 期望 1，实际 %d", cfg.Servers[0].SSL.ClientVerify.VerifyDepth)
	}
}

func TestGenerateConfigYAMLContainsAllSections(t *testing.T) {
	// TestGenerateConfigYAMLContainsAllSections 测试 YAML 包含所有配置块。
	cfg := DefaultConfig()
	yamlData, err := GenerateConfigYAML(cfg)
	if err != nil {
		t.Fatalf("GenerateConfigYAML 失败: %v", err)
	}
	yamlStr := string(yamlData)

	// 验证包含非注释的配置块
	requiredSections := []string{
		"resolver:",
		"variables:",
		"content_security_policy:",
		"permissions_policy:",
		"auth_request:",
	}

	for _, section := range requiredSections {
		// 检查配置块存在
		if !strings.Contains(yamlStr, section) {
			t.Errorf("配置块 %s 缺失", section)
		}
	}
}
