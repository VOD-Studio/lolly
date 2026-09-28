// Package config 提供配置解析与校验的测试。
//
// 该文件测试 ACME 自动证书配置的校验逻辑，包括：
//   - 挑战类型、目录 URL、EAB 配对校验
//   - 申请域名来源校验
//   - 与 SSL/HTTP2 校验的联动（ACME 提供 TLS 能力）
//
// 作者：xfy
package config

import (
	"strings"
	"testing"
)

// TestValidateACME 覆盖 ACME 配置的各类校验分支。
func TestValidateACME(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *ACMEConfig
		serverNames []string
		fallback    string
		errMsg      string
	}{
		{
			name: "disabled passes",
			cfg:  &ACMEConfig{Enabled: false},
		},
		{
			name: "valid defaults",
			cfg: &ACMEConfig{
				Enabled:   true,
				Directory: DefaultACMEDirectory,
				Email:     "admin@example.com",
			},
			serverNames: []string{"example.com"},
		},
		{
			name: "invalid challenge",
			cfg: &ACMEConfig{
				Enabled:   true,
				Challenge: "dns-01",
			},
			serverNames: []string{"example.com"},
			errMsg:      "challenge",
		},
		{
			name: "invalid directory url",
			cfg: &ACMEConfig{
				Enabled:   true,
				Directory: "not-a-url",
			},
			serverNames: []string{"example.com"},
			errMsg:      "directory",
		},
		{
			name: "eab kid without key",
			cfg: &ACMEConfig{
				Enabled: true,
				EABKid:  "kid",
			},
			serverNames: []string{"example.com"},
			errMsg:      "eab",
		},
		{
			name: "eab key without kid",
			cfg: &ACMEConfig{
				Enabled:    true,
				EABHmacKey: "aGVsbG8",
			},
			serverNames: []string{"example.com"},
			errMsg:      "eab",
		},
		{
			name: "no host source",
			cfg: &ACMEConfig{
				Enabled: true,
			},
			errMsg: "hosts",
		},
		{
			name: "hosts configured, no server names",
			cfg: &ACMEConfig{
				Enabled: true,
				Hosts:   []string{"example.com"},
			},
		},
		{
			name: "fallback name only",
			cfg: &ACMEConfig{
				Enabled: true,
			},
			fallback: "example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateACME(tt.cfg, tt.serverNames, tt.fallback)
			if tt.errMsg == "" {
				if err != nil {
					t.Fatalf("validateACME() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateACME() error = nil, want error containing %q", tt.errMsg)
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("validateACME() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

// TestACMEConfig_ResolveHosts 验证申请域名推导优先级。
func TestACMEConfig_ResolveHosts(t *testing.T) {
	withHosts := &ACMEConfig{Hosts: []string{"a.com"}}
	if got := withHosts.ResolveHosts([]string{"b.com"}, "c.com"); len(got) != 1 || got[0] != "a.com" {
		t.Errorf("ResolveHosts() = %v, want [a.com]", got)
	}

	empty := &ACMEConfig{}
	if got := empty.ResolveHosts([]string{"b.com"}, "c.com"); len(got) != 1 || got[0] != "b.com" {
		t.Errorf("ResolveHosts() = %v, want [b.com]", got)
	}
	if got := empty.ResolveHosts(nil, "c.com"); len(got) != 1 || got[0] != "c.com" {
		t.Errorf("ResolveHosts() = %v, want [c.com]", got)
	}
	if got := empty.ResolveHosts(nil, ""); len(got) != 0 {
		t.Errorf("ResolveHosts() = %v, want empty", got)
	}
}

// TestValidateSSL_ACMEGrantsTLS 验证 ACME 使 HTTP/2 校验通过（提供 TLS 能力）。
func TestValidateSSL_ACMEGrantsTLS(t *testing.T) {
	ssl := &SSLConfig{
		ACME: ACMEConfig{Enabled: true, Hosts: []string{"example.com"}},
		HTTP2: HTTP2Config{
			Enabled: true,
		},
	}
	if err := validateSSL(ssl); err != nil {
		t.Fatalf("validateSSL() error = %v, want nil for ACME-enabled TLS", err)
	}
}

// TestValidateServer_ACME 验证服务器级 ACME 校验的接线。
func TestValidateServer_ACME(t *testing.T) {
	// 合法：使用 server_names 作为域名来源
	srv := &ServerConfig{
		Listen:      ":443",
		ServerNames: []string{"example.com"},
		SSL: SSLConfig{
			ACME: ACMEConfig{Enabled: true},
		},
	}
	if err := validateServer(srv, false); err != nil {
		t.Fatalf("validateServer() error = %v, want nil", err)
	}

	// 非法：无域名来源
	bad := &ServerConfig{
		Listen: ":443",
		SSL: SSLConfig{
			ACME: ACMEConfig{Enabled: true},
		},
	}
	if err := validateServer(bad, false); err == nil {
		t.Fatal("validateServer() error = nil, want error for missing host source")
	}
}
