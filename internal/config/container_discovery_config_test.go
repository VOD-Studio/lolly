// Package config 提供容器发现配置的测试。
//
// 该文件验证容器发现配置的默认值、YAML 解析和跨服务器约束。
//
// 作者：xfy
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestContainerDiscoveryDefaults 验证容器发现默认关闭且使用本机 Docker 套接字。
func TestContainerDiscoveryDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ContainerDiscovery.Enabled {
		t.Fatal("ContainerDiscovery.Enabled 默认应为 false")
	}
	if cfg.ContainerDiscovery.Endpoint != "unix:///var/run/docker.sock" {
		t.Errorf("ContainerDiscovery.Endpoint = %q", cfg.ContainerDiscovery.Endpoint)
	}
	if cfg.ContainerDiscovery.ResyncInterval != 30*time.Second {
		t.Errorf("ContainerDiscovery.ResyncInterval = %v", cfg.ContainerDiscovery.ResyncInterval)
	}
	if cfg.ContainerDiscovery.Debounce != 200*time.Millisecond {
		t.Errorf("ContainerDiscovery.Debounce = %v", cfg.ContainerDiscovery.Debounce)
	}
	if cfg.ContainerDiscovery.RequestTimeout != 5*time.Second {
		t.Errorf("ContainerDiscovery.RequestTimeout = %v", cfg.ContainerDiscovery.RequestTimeout)
	}
}

// TestValidateContainerDiscovery 验证启用容器发现后的端点、网络和模板约束。
func TestValidateContainerDiscovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "有效HTTP和动态ACME模板",
			mutate: func(cfg *Config) {
				cfg.Servers = []ServerConfig{
					{Listen: ":80", Name: "http-template"},
					{Listen: ":443", Name: "https-template", SSL: SSLConfig{ACME: ACMEConfig{Enabled: true, AllowDynamicHosts: true}}},
				}
				cfg.ContainerDiscovery = ContainerDiscoveryConfig{
					Enabled:     true,
					Endpoint:    "unix:///var/run/docker.sock",
					Network:     "frontend",
					HTTPServer:  "http-template",
					HTTPSServer: "https-template",
				}
			},
		},
		{
			name: "拒绝TCP端点",
			mutate: func(cfg *Config) {
				cfg.ContainerDiscovery.Enabled = true
				cfg.ContainerDiscovery.Endpoint = "tcp://127.0.0.1:2375"
				cfg.ContainerDiscovery.Network = "frontend"
			},
			wantErr: "endpoint 必须使用 unix://",
		},
		{
			name: "启用时网络必填",
			mutate: func(cfg *Config) {
				cfg.ContainerDiscovery.Enabled = true
			},
			wantErr: "network 必填",
		},
		{
			name: "至少配置一个模板",
			mutate: func(cfg *Config) {
				cfg.ContainerDiscovery.Enabled = true
				cfg.ContainerDiscovery.Network = "frontend"
			},
			wantErr: "至少配置一个",
		},
		{
			name: "模板名称必须唯一",
			mutate: func(cfg *Config) {
				cfg.Servers = []ServerConfig{{Listen: ":80", Name: "template"}, {Listen: ":81", Name: "template"}}
				cfg.ContainerDiscovery = ContainerDiscoveryConfig{Enabled: true, Endpoint: "unix:///docker.sock", Network: "frontend", HTTPServer: "template"}
			},
			wantErr: "必须唯一",
		},
		{
			name: "HTTP模板必须明文",
			mutate: func(cfg *Config) {
				cfg.Servers = []ServerConfig{{Listen: ":443", Name: "template", SSL: SSLConfig{RejectHandshake: true}}}
				cfg.ContainerDiscovery = ContainerDiscoveryConfig{Enabled: true, Endpoint: "unix:///docker.sock", Network: "frontend", HTTPServer: "template"}
			},
			wantErr: "HTTP 模板必须是明文",
		},
		{
			name: "HTTPS模板必须启用TLS",
			mutate: func(cfg *Config) {
				cfg.ContainerDiscovery = ContainerDiscoveryConfig{Enabled: true, Endpoint: "unix:///docker.sock", Network: "frontend", HTTPSServer: "localhost"}
			},
			wantErr: "HTTPS 模板必须启用 TLS",
		},
		{
			name: "引用的模板必须存在",
			mutate: func(cfg *Config) {
				cfg.ContainerDiscovery = ContainerDiscoveryConfig{Enabled: true, Endpoint: "unix:///docker.sock", Network: "frontend", HTTPServer: "missing"}
			},
			wantErr: "未找到",
		},
		{
			name: "时长不能为负数",
			mutate: func(cfg *Config) {
				cfg.ContainerDiscovery = ContainerDiscoveryConfig{ResyncInterval: -time.Second}
			},
			wantErr: "resync_interval 不能为负数",
		},
		{
			name: "请求超时不能为负数",
			mutate: func(cfg *Config) {
				cfg.ContainerDiscovery.RequestTimeout = -time.Second
			},
			wantErr: "request_timeout 不能为负数",
		},
		{
			name: "ACME挑战类型必须有效",
			mutate: func(cfg *Config) {
				cfg.ContainerDiscovery.ACME.Challenge = "dns-01"
			},
			wantErr: "无效的 challenge",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Servers[0].SSL.HTTP2.Enabled = false
			tt.mutate(cfg)
			err := Validate(cfg)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() 返回意外错误: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() 错误 = %v，期望包含 %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoadContainerDiscovery 验证 YAML 字段及默认值合并。
func TestLoadContainerDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `
servers:
  - listen: ":80"
    name: http-template
container_discovery:
  enabled: true
  network: frontend
  http_server: http-template
  required: true
  acme:
    email: admin@example.com
    challenge: http-01
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() 返回错误: %v", err)
	}
	if cfg.ContainerDiscovery.Endpoint != "unix:///var/run/docker.sock" || cfg.ContainerDiscovery.Debounce != 200*time.Millisecond {
		t.Fatalf("默认值未保留: %+v", cfg.ContainerDiscovery)
	}
	if !cfg.ContainerDiscovery.Required || cfg.ContainerDiscovery.ACME.Email != "admin@example.com" {
		t.Fatalf("YAML 字段未解析: %+v", cfg.ContainerDiscovery)
	}
}

// TestGenerateConfigYAMLContainsContainerDiscovery 验证默认 YAML 展示完整容器发现配置。
func TestGenerateConfigYAMLContainsContainerDiscovery(t *testing.T) {
	data, err := GenerateConfigYAML(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"container_discovery:", "endpoint:", "network:", "http_server:", "https_server:", "resync_interval:", "debounce:", "request_timeout:", "required:", "acme:"} {
		if !strings.Contains(string(data), field) {
			t.Errorf("默认 YAML 缺少 %s", field)
		}
	}
}

// TestValidateACMEAllowsDynamicHosts 验证显式动态白名单允许启动时尚无域名的模板。
func TestValidateACMEAllowsDynamicHosts(t *testing.T) {
	acme := ACMEConfig{Enabled: true, AllowDynamicHosts: true}
	if err := validateACME(&acme, nil, ""); err != nil {
		t.Fatalf("validateACME() 返回意外错误: %v", err)
	}
}
