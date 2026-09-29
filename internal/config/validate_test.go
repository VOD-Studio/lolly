// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 该文件测试配置验证模块的各项功能，包括：
//   - 服务器配置验证
//   - 代理配置验证
//   - SSL 配置验证
//   - 认证配置验证
//   - 速率限制验证
//   - 压缩配置验证
//   - 访问控制验证
//   - Stream 配置验证
//   - 性能配置验证
//
// 作者：xfy
package config

import (
	"strings"
	"testing"
)

func TestValidateServer(t *testing.T) {
	t.Parallel()
	// TestValidateServer 测试服务器配置验证。
	tests := []struct {
		name      string
		errMsg    string
		config    ServerConfig
		isDefault bool
		wantErr   bool
	}{
		{
			name: "有效配置",
			config: ServerConfig{
				Listen: ":8080",
				Static: []StaticConfig{{Path: "/", Root: "/var/www"}},
				Proxy: []ProxyConfig{
					{Path: "/api", Targets: []ProxyTarget{{URL: "http://backend:8080"}}},
				},
			},
			isDefault: false,
			wantErr:   false,
		},
		{
			name: "默认服务器可省略Listen",
			config: ServerConfig{
				Static: []StaticConfig{{Path: "/", Root: "/var/www"}},
			},
			isDefault: true,
			wantErr:   false,
		},
		{
			name: "非默认服务器Listen缺失",
			config: ServerConfig{
				Static: []StaticConfig{{Path: "/", Root: "/var/www"}},
			},
			isDefault: false,
			wantErr:   true,
			errMsg:    "listen 地址必填",
		},
		{
			name: "无效Listen地址",
			config: ServerConfig{
				Listen: "invalid:address:format",
			},
			isDefault: false,
			wantErr:   true,
			errMsg:    "无效的监听地址",
		},
		{
			name: "静态根目录含..",
			config: ServerConfig{
				Listen: ":8080",
				Static: []StaticConfig{{Path: "/", Root: "/var/../www"}},
			},
			isDefault: false,
			wantErr:   true,
			errMsg:    "根目录路径不能包含 '..'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServer(&tt.config, tt.isDefault)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateServer() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateServer() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateServer() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateProxy(t *testing.T) {
	t.Parallel()
	// TestValidateProxy 测试代理配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  ProxyConfig
		wantErr bool
	}{
		{
			name: "有效代理配置",
			config: ProxyConfig{
				Path:    "/api",
				Targets: []ProxyTarget{{URL: "http://backend:8080"}},
			},
			wantErr: false,
		},
		{
			name: "有效代理带负载均衡",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "round_robin",
			},
			wantErr: false,
		},
		{
			name: "Path缺失",
			config: ProxyConfig{
				Targets: []ProxyTarget{{URL: "http://backend:8080"}},
			},
			wantErr: true,
			errMsg:  "path 必填",
		},
		{
			name: "Targets空",
			config: ProxyConfig{
				Path:    "/api",
				Targets: []ProxyTarget{},
			},
			wantErr: true,
			errMsg:  "targets 至少需要一个目标地址",
		},
		{
			name: "URL格式错误-无协议",
			config: ProxyConfig{
				Path:    "/api",
				Targets: []ProxyTarget{{URL: "backend:8080"}},
			},
			wantErr: true,
			errMsg:  "必须以 http:// 或 https:// 开头",
		},
		{
			name: "URL格式错误-空URL",
			config: ProxyConfig{
				Path:    "/api",
				Targets: []ProxyTarget{{URL: ""}},
			},
			wantErr: true,
			errMsg:  "url 必填",
		},
		{
			name: "无效负载均衡算法",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "invalid_algorithm",
			},
			wantErr: true,
			errMsg:  "无效的负载均衡算法",
		},
		{
			name: "有效 least_time 配置 metric=header",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "least_time",
				LeastTime:   LeastTimeConfig{Metric: "header"},
			},
			wantErr: false,
		},
		{
			name: "有效 least_time 配置 metric=last_byte",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "least_time",
				LeastTime:   LeastTimeConfig{Metric: "last_byte"},
			},
			wantErr: false,
		},
		{
			name: "无效 least_time metric",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "least_time",
				LeastTime:   LeastTimeConfig{Metric: "invalid"},
			},
			wantErr: true,
			errMsg:  "无效的 least_time metric",
		},
		{
			name: "有效 sticky 配置",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "sticky",
				Sticky:      StickyConfig{Enabled: true, FallbackAlgo: "round_robin"},
			},
			wantErr: false,
		},
		{
			name: "无效 sticky enabled=false",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "sticky",
				Sticky:      StickyConfig{Enabled: false},
			},
			wantErr: true,
			errMsg:  "sticky.enabled 必须为 true",
		},
		{
			name: "无效 sticky fallback_balance",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "sticky",
				Sticky:      StickyConfig{Enabled: true, FallbackAlgo: "invalid"},
			},
			wantErr: true,
			errMsg:  "无效的 sticky fallback_balance",
		},
		{
			name: "无效 sticky same_site",
			config: ProxyConfig{
				Path:        "/api",
				Targets:     []ProxyTarget{{URL: "http://backend:8080"}},
				LoadBalance: "sticky",
				Sticky:      StickyConfig{Enabled: true, SameSite: "Invalid"},
			},
			wantErr: true,
			errMsg:  "无效的 sticky same_site",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProxy(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateProxy() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateProxy() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateProxy() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateSSL(t *testing.T) {
	t.Parallel()
	// TestValidateSSL 测试 SSL 配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  SSLConfig
		wantErr bool
	}{
		{
			name:    "未配置SSL",
			config:  SSLConfig{},
			wantErr: false,
		},
		{
			name: "有效SSL配置",
			config: SSLConfig{
				Cert:      "/path/to/cert.pem",
				Key:       "/path/to/key.pem",
				Protocols: []string{"TLSv1.2", "TLSv1.3"},
			},
			wantErr: false,
		},
		{
			name: "仅Cert配置",
			config: SSLConfig{
				Cert: "/path/to/cert.pem",
			},
			wantErr: true,
			errMsg:  "cert 和 key 必须同时配置",
		},
		{
			name: "仅Key配置",
			config: SSLConfig{
				Key: "/path/to/key.pem",
			},
			wantErr: true,
			errMsg:  "cert 和 key 必须同时配置",
		},
		{
			name: "TLSv1.0不安全",
			config: SSLConfig{
				Cert:      "/path/to/cert.pem",
				Key:       "/path/to/key.pem",
				Protocols: []string{"TLSv1.0"},
			},
			wantErr: true,
			errMsg:  "不安全的 TLS 版本: TLSv1.0",
		},
		{
			name: "TLSv1.1不安全",
			config: SSLConfig{
				Cert:      "/path/to/cert.pem",
				Key:       "/path/to/key.pem",
				Protocols: []string{"TLSv1.1"},
			},
			wantErr: true,
			errMsg:  "不安全的 TLS 版本: TLSv1.1",
		},
		{
			name: "不安全加密套件RC4",
			config: SSLConfig{
				Cert:      "/path/to/cert.pem",
				Key:       "/path/to/key.pem",
				Protocols: []string{"TLSv1.2"},
				Ciphers:   []string{"RC4-SHA"},
			},
			wantErr: true,
			errMsg:  "不安全的加密套件",
		},
		{
			name: "不安全加密套件DES",
			config: SSLConfig{
				Cert:      "/path/to/cert.pem",
				Key:       "/path/to/key.pem",
				Protocols: []string{"TLSv1.2"},
				Ciphers:   []string{"DES-CBC3-SHA"},
			},
			wantErr: true,
			errMsg:  "不安全的加密套件",
		},
		{
			name: "未知TLS版本",
			config: SSLConfig{
				Cert:      "/path/to/cert.pem",
				Key:       "/path/to/key.pem",
				Protocols: []string{"TLSv1.4"},
			},
			wantErr: true,
			errMsg:  "未知的 TLS 版本",
		},
		{
			name: "reject_handshake单独配置",
			config: SSLConfig{
				RejectHandshake: true,
			},
			wantErr: false,
		},
		{
			name: "reject_handshake与证书冲突",
			config: SSLConfig{
				RejectHandshake: true,
				Cert:            "/path/to/cert.pem",
				Key:             "/path/to/key.pem",
			},
			wantErr: true,
			errMsg:  "reject_handshake 不能与 cert/key 或 acme 同时配置",
		},
		{
			name: "reject_handshake与ACME冲突",
			config: SSLConfig{
				RejectHandshake: true,
				ACME: ACMEConfig{
					Enabled: true,
					Email:   "admin@example.com",
					Hosts:   []string{"example.com"},
				},
			},
			wantErr: true,
			errMsg:  "reject_handshake 不能与 cert/key 或 acme 同时配置",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSSL(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateSSL() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateSSL() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateSSL() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateAuth(t *testing.T) {
	t.Parallel()
	// TestValidateAuth 测试认证配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  AuthConfig
		wantErr bool
	}{
		{
			name:    "未配置认证",
			config:  AuthConfig{},
			wantErr: false,
		},
		{
			name: "有效Basic认证配置",
			config: AuthConfig{
				Type:      "basic",
				Algorithm: "bcrypt",
				Users:     []User{{Name: "admin", Password: "hashed_password"}},
			},
			wantErr: false,
		},
		{
			name: "有效MinPasswordLength",
			config: AuthConfig{
				Type:              "basic",
				Algorithm:         "bcrypt",
				Users:             []User{{Name: "admin", Password: "hashed_password"}},
				MinPasswordLength: 8,
			},
			wantErr: false,
		},
		{
			name: "MinPasswordLength过小",
			config: AuthConfig{
				Type:              "basic",
				Users:             []User{{Name: "admin", Password: "hashed_password"}},
				MinPasswordLength: 5,
			},
			wantErr: true,
			errMsg:  "min_password_length 建议至少为 6",
		},
		{
			name: "MinPasswordLength过大",
			config: AuthConfig{
				Type:              "basic",
				Users:             []User{{Name: "admin", Password: "hashed_password"}},
				MinPasswordLength: 129,
			},
			wantErr: true,
			errMsg:  "min_password_length 上限为 128",
		},
		{
			name: "MinPasswordLength边界值6",
			config: AuthConfig{
				Type:              "basic",
				Users:             []User{{Name: "admin", Password: "hashed_password"}},
				MinPasswordLength: 6,
			},
			wantErr: false,
		},
		{
			name: "MinPasswordLength边界值128",
			config: AuthConfig{
				Type:              "basic",
				Users:             []User{{Name: "admin", Password: "hashed_password"}},
				MinPasswordLength: 128,
			},
			wantErr: false,
		},
		{
			name: "无效认证类型",
			config: AuthConfig{
				Type:  "oauth",
				Users: []User{{Name: "admin", Password: "hashed_password"}},
			},
			wantErr: true,
			errMsg:  "不支持的认证类型",
		},
		{
			name: "启用认证但无用户",
			config: AuthConfig{
				Type:      "basic",
				Algorithm: "bcrypt",
				Users:     []User{},
			},
			wantErr: true,
			errMsg:  "启用认证时至少需要一个用户",
		},
		{
			name: "用户名缺失",
			config: AuthConfig{
				Type:      "basic",
				Algorithm: "bcrypt",
				Users:     []User{{Name: "", Password: "hashed_password"}},
			},
			wantErr: true,
			errMsg:  "name 必填",
		},
		{
			name: "密码缺失",
			config: AuthConfig{
				Type:      "basic",
				Algorithm: "bcrypt",
				Users:     []User{{Name: "admin", Password: ""}},
			},
			wantErr: true,
			errMsg:  "password 必填",
		},
		{
			name: "无效哈希算法",
			config: AuthConfig{
				Type:      "basic",
				Algorithm: "md5",
				Users:     []User{{Name: "admin", Password: "hashed_password"}},
			},
			wantErr: true,
			errMsg:  "不支持的哈希算法",
		},
		{
			name: "空算法默认有效",
			config: AuthConfig{
				Type:  "basic",
				Users: []User{{Name: "admin", Password: "hashed_password"}},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAuth(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateAuth() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateAuth() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateAuth() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateRateLimit(t *testing.T) {
	t.Parallel()
	// TestValidateRateLimit 测试速率限制配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  RateLimitConfig
		wantErr bool
	}{
		{
			name:    "未配置速率限制",
			config:  RateLimitConfig{},
			wantErr: false,
		},
		{
			name: "有效速率限制配置",
			config: RateLimitConfig{
				RequestRate: 100,
				Burst:       20,
				Key:         "ip",
			},
			wantErr: false,
		},
		{
			name: "负数RequestRate",
			config: RateLimitConfig{
				RequestRate: -1,
			},
			wantErr: true,
			errMsg:  "request_rate 不能为负数",
		},
		{
			name: "负数Burst",
			config: RateLimitConfig{
				RequestRate: 100,
				Burst:       -1,
			},
			wantErr: true,
			errMsg:  "burst 不能为负数",
		},
		{
			name: "负数ConnLimit",
			config: RateLimitConfig{
				ConnLimit: -1,
			},
			wantErr: true,
			errMsg:  "conn_limit 不能为负数",
		},
		{
			name: "无效Key来源",
			config: RateLimitConfig{
				RequestRate: 100,
				Key:         "invalid_key",
			},
			wantErr: true,
			errMsg:  "无效的 key 来源",
		},
		{
			name: "仅ConnLimit配置",
			config: RateLimitConfig{
				ConnLimit: 10,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRateLimit(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateRateLimit() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateRateLimit() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateRateLimit() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateCompression(t *testing.T) {
	t.Parallel()
	// TestValidateCompression 测试压缩配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  CompressionConfig
		wantErr bool
	}{
		{
			name:    "未配置压缩",
			config:  CompressionConfig{},
			wantErr: false,
		},
		{
			name: "有效gzip压缩配置",
			config: CompressionConfig{
				Type:    "gzip",
				Level:   6,
				MinSize: 1024,
			},
			wantErr: false,
		},
		{
			name: "有效brotli压缩配置",
			config: CompressionConfig{
				Type:    "brotli",
				Level:   4,
				MinSize: 512,
			},
			wantErr: false,
		},
		{
			name: "无效压缩类型",
			config: CompressionConfig{
				Type: "lz4",
			},
			wantErr: true,
			errMsg:  "无效的压缩类型",
		},
		{
			name: "级别过低",
			config: CompressionConfig{
				Type:  "gzip",
				Level: -1,
			},
			wantErr: true,
			errMsg:  "无效的压缩级别",
		},
		{
			name: "级别过高",
			config: CompressionConfig{
				Type:  "gzip",
				Level: 10,
			},
			wantErr: true,
			errMsg:  "无效的压缩级别",
		},
		{
			name: "负数MinSize",
			config: CompressionConfig{
				Type:    "gzip",
				MinSize: -100,
			},
			wantErr: true,
			errMsg:  "min_size 不能为负数",
		},
		{
			name: "级别0有效",
			config: CompressionConfig{
				Type:  "gzip",
				Level: 0,
			},
			wantErr: false,
		},
		{
			name: "级别9有效",
			config: CompressionConfig{
				Type:  "gzip",
				Level: 9,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCompression(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateCompression() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateCompression() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateCompression() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateAccess(t *testing.T) {
	t.Parallel()
	// TestValidateAccess 测试访问控制配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  AccessConfig
		wantErr bool
	}{
		{
			name:    "空配置有效",
			config:  AccessConfig{},
			wantErr: false,
		},
		{
			name: "有效CIDR",
			config: AccessConfig{
				Allow: []string{"192.168.1.0/24", "10.0.0.0/8"},
			},
			wantErr: false,
		},
		{
			name: "有效单个IP",
			config: AccessConfig{
				Allow: []string{"192.168.1.100"},
				Deny:  []string{"10.0.0.1"},
			},
			wantErr: false,
		},
		{
			name: "有效IPv6 CIDR",
			config: AccessConfig{
				Allow: []string{"2001:db8::/32"},
			},
			wantErr: false,
		},
		{
			name: "有效IPv6地址",
			config: AccessConfig{
				Allow: []string{"::1", "2001:db8::1"},
			},
			wantErr: false,
		},
		{
			name: "无效CIDR格式",
			config: AccessConfig{
				Allow: []string{"invalid-cidr"},
			},
			wantErr: true,
			errMsg:  "无效的 allow CIDR/IP",
		},
		{
			name: "无效Deny CIDR",
			config: AccessConfig{
				Deny: []string{"not-a-cidr"},
			},
			wantErr: true,
			errMsg:  "无效的 deny CIDR/IP",
		},
		{
			name: "有效默认动作allow",
			config: AccessConfig{
				Default: "allow",
			},
			wantErr: false,
		},
		{
			name: "有效默认动作deny",
			config: AccessConfig{
				Default: "deny",
			},
			wantErr: false,
		},
		{
			name: "无效默认动作",
			config: AccessConfig{
				Default: "reject",
			},
			wantErr: true,
			errMsg:  "无效的 default 动作",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccess(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateAccess() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateAccess() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateAccess() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateSecurity(t *testing.T) {
	t.Parallel()
	// TestValidateSecurity 测试安全配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  SecurityConfig
		wantErr bool
	}{
		{
			name:    "空配置有效",
			config:  SecurityConfig{},
			wantErr: false,
		},
		{
			name: "有效安全配置",
			config: SecurityConfig{
				Access: AccessConfig{
					Allow: []string{"192.168.1.0/24"},
				},
				Auth: AuthConfig{
					Type:  "basic",
					Users: []User{{Name: "admin", Password: "hashed"}},
				},
				RateLimit: RateLimitConfig{
					RequestRate: 100,
				},
			},
			wantErr: false,
		},
		{
			name: "无效Access配置",
			config: SecurityConfig{
				Access: AccessConfig{
					Allow: []string{"invalid-ip"},
				},
			},
			wantErr: true,
			errMsg:  "无效的 allow CIDR/IP",
		},
		{
			name: "无效Auth配置",
			config: SecurityConfig{
				Auth: AuthConfig{
					Type:  "invalid",
					Users: []User{{Name: "admin", Password: "hashed"}},
				},
			},
			wantErr: true,
			errMsg:  "不支持的认证类型",
		},
		{
			name: "无效RateLimit配置",
			config: SecurityConfig{
				RateLimit: RateLimitConfig{
					RequestRate: -1,
				},
			},
			wantErr: true,
			errMsg:  "request_rate 不能为负数",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSecurity(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateSecurity() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateSecurity() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateSecurity() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateStream(t *testing.T) {
	t.Parallel()
	// TestValidateStream 测试 Stream 代理配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  StreamConfig
		wantErr bool
	}{
		{
			name: "有效 TCP Stream",
			config: StreamConfig{
				Listen:   ":3306",
				Protocol: "tcp",
				Upstream: StreamUpstream{
					Targets:     []StreamTarget{{Addr: "db1:3306"}},
					LoadBalance: "round_robin",
				},
			},
			wantErr: false,
		},
		{
			name: "有效 UDP Stream",
			config: StreamConfig{
				Listen:   ":53",
				Protocol: "udp",
				Upstream: StreamUpstream{
					Targets:     []StreamTarget{{Addr: "dns1:53"}},
					LoadBalance: "least_conn",
				},
			},
			wantErr: false,
		},
		{
			name: "监听地址为空",
			config: StreamConfig{
				Listen:   "",
				Protocol: "tcp",
			},
			wantErr: true,
			errMsg:  "listen 地址必填",
		},
		{
			name: "无效协议类型",
			config: StreamConfig{
				Listen:   ":3306",
				Protocol: "http",
			},
			wantErr: true,
			errMsg:  "无效的协议类型",
		},
		{
			name: "无目标地址",
			config: StreamConfig{
				Listen:   ":3306",
				Protocol: "tcp",
				Upstream: StreamUpstream{
					Targets: []StreamTarget{},
				},
			},
			wantErr: true,
			errMsg:  "upstream.targets 至少需要一个目标地址",
		},
		{
			name: "目标地址为空",
			config: StreamConfig{
				Listen:   ":3306",
				Protocol: "tcp",
				Upstream: StreamUpstream{
					Targets: []StreamTarget{{Addr: ""}},
				},
			},
			wantErr: true,
			errMsg:  "addr 必填",
		},
		{
			name: "无效负载均衡算法",
			config: StreamConfig{
				Listen:   ":3306",
				Protocol: "tcp",
				Upstream: StreamUpstream{
					Targets:     []StreamTarget{{Addr: "db1:3306"}},
					LoadBalance: "invalid_algorithm",
				},
			},
			wantErr: true,
			errMsg:  "无效的负载均衡算法",
		},
		{
			name: "有效加权轮询算法",
			config: StreamConfig{
				Listen:   ":3306",
				Protocol: "tcp",
				Upstream: StreamUpstream{
					Targets:     []StreamTarget{{Addr: "db1:3306"}},
					LoadBalance: "weighted_round_robin",
				},
			},
			wantErr: false,
		},
		{
			name: "有效 IP 哈希算法",
			config: StreamConfig{
				Listen:   ":3306",
				Protocol: "tcp",
				Upstream: StreamUpstream{
					Targets:     []StreamTarget{{Addr: "db1:3306"}},
					LoadBalance: "ip_hash",
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStream(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateStream() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateStream() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateStream() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidatePerformance(t *testing.T) {
	t.Parallel()
	// TestValidatePerformance 测试性能配置验证。
	tests := []struct {
		name    string
		errMsg  string
		config  PerformanceConfig
		wantErr bool
	}{
		{
			name: "空配置有效",
			config: PerformanceConfig{
				FileCache: FileCacheConfig{},
				Transport: TransportConfig{},
			},
			wantErr: false,
		},
		{
			name: "有效的 file_cache 配置",
			config: PerformanceConfig{
				FileCache: FileCacheConfig{
					MaxEntries: 1000,
					MaxSize:    1024 * 1024 * 100,
				},
				GoroutinePool: GoroutinePoolConfig{
					Enabled: true,
				},
			},
			wantErr: false,
		},
		{
			name: "有效的 transport 配置（零值）",
			config: PerformanceConfig{
				Transport: TransportConfig{
					MaxConnsPerHost: 0,
				},
			},
			wantErr: false,
		},
		{
			name: "有效的 transport 配置（正值）",
			config: PerformanceConfig{
				Transport: TransportConfig{
					MaxConnsPerHost: 50,
				},
			},
			wantErr: false,
		},
		{
			name: "MaxConnsPerHost 负数",
			config: PerformanceConfig{
				Transport: TransportConfig{
					MaxConnsPerHost: -1,
				},
			},
			wantErr: true,
			errMsg:  "transport.max_conns_per_host 不能为负数",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePerformance(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validatePerformance() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validatePerformance() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validatePerformance() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateVariables(t *testing.T) {
	t.Parallel()
	// TestValidateVariables 测试自定义变量配置验证。
	tests := []struct {
		config  VariablesConfig
		name    string
		errMsg  string
		wantErr bool
	}{
		{
			name:    "空配置有效",
			config:  VariablesConfig{},
			wantErr: false,
		},
		{
			name: "有效变量名",
			config: VariablesConfig{
				Set: map[string]string{
					"app_name": "lolly",
					"version":  "1.0.0",
					"ENV_VAR":  "production",
				},
			},
			wantErr: false,
		},
		{
			name: "空变量名",
			config: VariablesConfig{
				Set: map[string]string{
					"": "value",
				},
			},
			wantErr: true,
			errMsg:  "变量名不能为空",
		},
		{
			name: "变量名含特殊字符",
			config: VariablesConfig{
				Set: map[string]string{
					"app-name": "value",
				},
			},
			wantErr: true,
			errMsg:  "包含非法字符",
		},
		{
			name: "变量名arg_前缀冲突",
			config: VariablesConfig{
				Set: map[string]string{
					"arg_foo": "value",
				},
			},
			wantErr: true,
			errMsg:  "与动态变量前缀冲突",
		},
		{
			name: "变量名http_前缀冲突",
			config: VariablesConfig{
				Set: map[string]string{
					"http_custom": "value",
				},
			},
			wantErr: true,
			errMsg:  "与动态变量前缀冲突",
		},
		{
			name: "变量名cookie_前缀冲突",
			config: VariablesConfig{
				Set: map[string]string{
					"cookie_session": "value",
				},
			},
			wantErr: true,
			errMsg:  "与动态变量前缀冲突",
		},
		{
			name: "变量名与内置变量冲突",
			config: VariablesConfig{
				Set: map[string]string{
					"host": "custom",
				},
			},
			wantErr: true,
			errMsg:  "与内置变量冲突",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVariables(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateVariables() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateVariables() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateVariables() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateTryFilesPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pattern string
		errMsg  string
		wantErr bool
	}{
		// 基本占位符
		{name: "有效 $uri", pattern: "$uri", wantErr: false},
		{name: "有效 $uri/", pattern: "$uri/", wantErr: false},

		// 动态后缀
		{name: "有效 $uri.html", pattern: "$uri.html", wantErr: false},
		{name: "有效 $uri.json", pattern: "$uri.json", wantErr: false},
		{name: "有效 $uri.css", pattern: "$uri.css", wantErr: false},
		{name: "有效 $uri.js", pattern: "$uri.js", wantErr: false},
		{name: "有效 $uri.xml", pattern: "$uri.xml", wantErr: false},
		{name: "有效 $uri.webmanifest", pattern: "$uri.webmanifest", wantErr: false},
		{name: "有效 $uri.txt", pattern: "$uri.txt", wantErr: false},
		{name: "有效 $uri.svg", pattern: "$uri.svg", wantErr: false},
		{name: "有效 $uri.woff2", pattern: "$uri.woff2", wantErr: false},

		// 绝对路径回退
		{name: "有效绝对路径", pattern: "/index.html", wantErr: false},
		{name: "有效嵌套路径", pattern: "/fallback/index.html", wantErr: false},

		// 相对路径回退
		{name: "有效文件名", pattern: "fallback.html", wantErr: false},
		{name: "有效嵌套文件名", pattern: "app-shell.html", wantErr: false},

		// 安全检查 - null byte
		{name: "拒绝 null byte", pattern: "$uri\x00.html", wantErr: true, errMsg: "null byte"},
		{name: "拒绝扩展名 null byte", pattern: "$uri.ht\x00ml", wantErr: true, errMsg: "null byte"},

		// 安全检查 - 路径分隔符
		{name: "拒绝扩展名中斜杠", pattern: "$uri./../etc/passwd", wantErr: true, errMsg: "路径分隔符"},
		{name: "拒绝扩展名中反斜杠", pattern: "$uri.\\..\\passwd", wantErr: true, errMsg: "路径分隔符"},
		{name: "拒绝扩展名中单个斜杠", pattern: "$uri.dir/file", wantErr: true, errMsg: "路径分隔符"},

		// 安全检查 - 危险后缀
		{name: "拒绝 .php 后缀", pattern: "$uri.php", wantErr: true, errMsg: "被禁止"},
		{name: "拒绝 .exe 后缀", pattern: "$uri.exe", wantErr: true, errMsg: "被禁止"},
		{name: "拒绝 .bat 后缀", pattern: "$uri.bat", wantErr: true, errMsg: "被禁止"},
		{name: "拒绝 .sh 后缀", pattern: "$uri.sh", wantErr: true, errMsg: "被禁止"},
		{name: "拒绝 .cgi 后缀", pattern: "$uri.cgi", wantErr: true, errMsg: "被禁止"},
		{name: "拒绝 .phtml 后缀", pattern: "$uri.phtml", wantErr: true, errMsg: "被禁止"},
		{name: "拒绝 .PHP 大写后缀", pattern: "$uri.PHP", wantErr: true, errMsg: "被禁止"},

		// 安全检查 - 非法字符
		{name: "拒绝扩展名中空格", pattern: "$uri. html", wantErr: true, errMsg: "非法字符"},
		{name: "拒绝扩展名中特殊字符", pattern: "$uri.<script>", wantErr: true, errMsg: "非法字符"},
		{name: "拒绝扩展名中百分号", pattern: "$uri.%20", wantErr: true, errMsg: "非法字符"},
		{name: "拒绝扩展名中中文", pattern: "$uri.测试", wantErr: true, errMsg: "非法字符"},

		// 空模式
		{name: "拒绝空模式", pattern: "", wantErr: true, errMsg: "不能为空"},
		{name: "拒绝空扩展名", pattern: "$uri.", wantErr: true, errMsg: "扩展名不能为空"},

		// 路径遍历
		{name: "拒绝绝对路径遍历", pattern: "/../../../etc/passwd", wantErr: true, errMsg: "路径遍历"},
		{name: "拒绝文件名路径遍历", pattern: "../etc/passwd", wantErr: true, errMsg: "路径遍历"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTryFilesPattern(tt.pattern)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateTryFilesPattern(%q) 期望返回错误，但返回 nil", tt.pattern)
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateTryFilesPattern(%q) 期望返回 nil，但返回错误: %v", tt.pattern, err)
				}
			}
		})
	}
}

func TestValidateStaticsWithTryFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		errMsg  string
		statics []StaticConfig
		wantErr bool
	}{
		{
			name: "有效 try_files 配置",
			statics: []StaticConfig{
				{
					Path:     "/",
					Root:     "/var/www",
					TryFiles: []string{"$uri", "$uri.html", "/index.html"},
				},
			},
			wantErr: false,
		},
		{
			name: "多静态目录有效配置",
			statics: []StaticConfig{
				{
					Path:     "/",
					Root:     "/var/www",
					TryFiles: []string{"$uri", "/index.html"},
				},
				{
					Path:     "/api",
					Root:     "/var/api",
					TryFiles: []string{"$uri.json", "/api.json"},
				},
			},
			wantErr: false,
		},
		{
			name: "无效 try_files 模式",
			statics: []StaticConfig{
				{
					Path:     "/",
					Root:     "/var/www",
					TryFiles: []string{"$uri", "$uri.php"},
				},
			},
			wantErr: true,
			errMsg:  "try_files[1]",
		},
		{
			name: "空 try_files 配置",
			statics: []StaticConfig{
				{
					Path:     "/",
					Root:     "/var/www",
					TryFiles: []string{},
				},
			},
			wantErr: false,
		},
		{
			name: "无 try_files 配置",
			statics: []StaticConfig{
				{
					Path: "/",
					Root: "/var/www",
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStatics(tt.statics)
			if tt.wantErr {
				if err == nil {
					t.Errorf("期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateRewrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		errMsg  string
		config  RewriteRule
		wantErr bool
	}{
		{
			name: "有效重写规则",
			config: RewriteRule{
				Pattern:     "^/old/(.*)$",
				Replacement: "/new/$1",
				Flag:        "last",
			},
			wantErr: false,
		},
		{
			name: "有效redirect标志",
			config: RewriteRule{
				Pattern:     "^/old$",
				Replacement: "/new",
				Flag:        "redirect",
			},
			wantErr: false,
		},
		{
			name: "有效permanent标志",
			config: RewriteRule{
				Pattern:     "^/old$",
				Replacement: "/new",
				Flag:        "permanent",
			},
			wantErr: false,
		},
		{
			name: "有效break标志",
			config: RewriteRule{
				Pattern:     "^/api/(.*)$",
				Replacement: "/backend/$1",
				Flag:        "break",
			},
			wantErr: false,
		},
		{
			name: "空标志有效",
			config: RewriteRule{
				Pattern:     "^/old$",
				Replacement: "/new",
			},
			wantErr: false,
		},
		{
			name: "Pattern缺失",
			config: RewriteRule{
				Replacement: "/new",
			},
			wantErr: true,
			errMsg:  "pattern 必填",
		},
		{
			name: "无效flag",
			config: RewriteRule{
				Pattern:     "^/old$",
				Replacement: "/new",
				Flag:        "invalid",
			},
			wantErr: true,
			errMsg:  "无效的 flag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRewrite(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateRewrite() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateRewrite() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateRewrite() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateNextUpstream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		errMsg  string
		config  NextUpstreamConfig
		wantErr bool
	}{
		{
			name:    "空配置有效",
			config:  NextUpstreamConfig{},
			wantErr: false,
		},
		{
			name: "有效重试配置",
			config: NextUpstreamConfig{
				Tries:     3,
				HTTPCodes: []int{500, 502, 503, 504},
			},
			wantErr: false,
		},
		{
			name: "仅Tries配置",
			config: NextUpstreamConfig{
				Tries: 2,
			},
			wantErr: false,
		},
		{
			name: "仅HTTPCodes配置",
			config: NextUpstreamConfig{
				HTTPCodes: []int{500, 502},
			},
			wantErr: false,
		},
		{
			name: "负数Tries",
			config: NextUpstreamConfig{
				Tries: -1,
			},
			wantErr: true,
			errMsg:  "tries 不能为负数",
		},
		{
			name: "无效HTTP状态码-过低",
			config: NextUpstreamConfig{
				HTTPCodes: []int{99},
			},
			wantErr: true,
			errMsg:  "无效的 HTTP 状态码",
		},
		{
			name: "无效HTTP状态码-过高",
			config: NextUpstreamConfig{
				HTTPCodes: []int{600},
			},
			wantErr: true,
			errMsg:  "无效的 HTTP 状态码",
		},
		{
			name: "有效边界状态码100",
			config: NextUpstreamConfig{
				HTTPCodes: []int{100},
			},
			wantErr: false,
		},
		{
			name: "有效边界状态码599",
			config: NextUpstreamConfig{
				HTTPCodes: []int{599},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNextUpstream(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateNextUpstream() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateNextUpstream() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateNextUpstream() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateDefaultServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		errMsg  string
		servers []ServerConfig
		wantErr bool
	}{
		{
			name:    "空服务器列表",
			servers: []ServerConfig{},
			wantErr: false,
		},
		{
			name: "无默认服务器",
			servers: []ServerConfig{
				{Listen: ":8080"},
				{Listen: ":8081"},
			},
			wantErr: false,
		},
		{
			name: "单个默认服务器",
			servers: []ServerConfig{
				{Listen: ":8080", Default: true},
				{Listen: ":8081"},
			},
			wantErr: false,
		},
		{
			name: "同一监听多个默认服务器",
			servers: []ServerConfig{
				{Listen: ":8080", Default: true},
				{Listen: ":8080", Default: true},
			},
			wantErr: true,
			errMsg:  "只能有一个 default: true 服务器",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDefaultServer(tt.servers)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateDefaultServer() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateDefaultServer() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateDefaultServer() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		errMsg  string
		mode    ServerMode
		wantErr bool
	}{
		{name: "空模式有效", mode: "", wantErr: false},
		{name: "auto模式有效", mode: ServerModeAuto, wantErr: false},
		{name: "single模式有效", mode: ServerModeSingle, wantErr: false},
		{name: "vhost模式有效", mode: ServerModeVHost, wantErr: false},
		{name: "multi_server模式有效", mode: ServerModeMultiServer, wantErr: false},
		{name: "无效模式", mode: "invalid", wantErr: true, errMsg: "无效的 mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMode(tt.mode)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateMode() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateMode() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateMode() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateListenConflicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		errMsg  string
		servers []ServerConfig
		mode    ServerMode
		wantErr bool
	}{
		{
			name:    "非multi_server模式跳过验证",
			servers: []ServerConfig{{Listen: ":8080"}},
			mode:    ServerModeSingle,
			wantErr: false,
		},
		{
			name: "multi_server模式有效配置",
			servers: []ServerConfig{
				{Listen: ":8080"},
				{Listen: ":8081"},
			},
			mode:    ServerModeMultiServer,
			wantErr: false,
		},
		{
			name:    "multi_server模式缺少listen",
			servers: []ServerConfig{{Listen: ""}},
			mode:    ServerModeMultiServer,
			wantErr: true,
			errMsg:  "multi_server 模式下每个 server 必须配置 listen 地址",
		},
		{
			name: "multi_server模式允许同一监听的不同主机",
			servers: []ServerConfig{
				{Listen: ":8080", Name: "a.example.com"},
				{Listen: ":8080", Name: "b.example.com"},
			},
			mode:    ServerModeMultiServer,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateListenConflicts(tt.servers, tt.mode)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateListenConflicts() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateListenConflicts() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateListenConflicts() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

// TestValidateListenGroups 测试监听分组相关的跨服务器约束。
func TestValidateListenGroups(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name: "不同监听可分别设置默认服务器",
			cfg: &Config{Servers: []ServerConfig{
				{Listen: ":80", Default: true},
				{Listen: ":443", Default: true},
			}},
		},
		{
			name: "同一监听只能设置一个默认服务器",
			cfg: &Config{Servers: []ServerConfig{
				{Listen: ":80", Default: true},
				{Listen: ":80", Default: true},
			}},
			wantErr: "只能有一个 default: true 服务器",
		},
		{
			name: "同一监听拒绝重复有效主机名",
			cfg: &Config{Servers: []ServerConfig{
				{Listen: ":80", ServerNames: []string{"example.com"}},
				{Listen: ":80", Name: "example.com"},
			}},
			wantErr: "重复的 server_name",
		},
		{
			name: "不同监听允许相同主机名",
			cfg: &Config{Servers: []ServerConfig{
				{Listen: ":80", Name: "example.com"},
				{Listen: ":443", Name: "example.com"},
			}},
		},
		{
			name: "同一监听拒绝混合明文和TLS",
			cfg: &Config{Servers: []ServerConfig{
				{Listen: ":443", Name: "plain"},
				{Listen: ":443", Name: "secure", SSL: SSLConfig{RejectHandshake: true}},
			}},
			wantErr: "不能混合 TLS 和明文",
		},
		{
			name: "single模式必须恰好一个服务器",
			cfg: &Config{Mode: ServerModeSingle, Servers: []ServerConfig{
				{Listen: ":80"}, {Listen: ":81"},
			}},
			wantErr: "single 模式必须恰好配置一个 server",
		},
		{
			name:    "vhost模式至少需要两个服务器",
			cfg:     &Config{Mode: ServerModeVHost, Servers: []ServerConfig{{Listen: ":80"}}},
			wantErr: "vhost 模式至少需要配置两个 server",
		},
		{
			name:    "multi_server模式至少需要两个服务器",
			cfg:     &Config{Mode: ServerModeMultiServer, Servers: []ServerConfig{{Listen: ":80"}}},
			wantErr: "multi_server 模式至少需要配置两个 server",
		},
		{
			name: "vhost模式只能使用一个监听地址",
			cfg: &Config{Mode: ServerModeVHost, Servers: []ServerConfig{
				{Listen: ":80"}, {Listen: ":81"},
			}},
			wantErr: "vhost 模式只能使用一个 listen 地址",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.cfg)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() 返回意外错误: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() 错误 = %v，期望包含 %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateHTTP2(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		errMsg  string
		config  HTTP2Config
		hasSSL  bool
		wantErr bool
	}{
		{
			name:    "未启用HTTP2",
			config:  HTTP2Config{Enabled: false},
			hasSSL:  false,
			wantErr: false,
		},
		{
			name:    "启用HTTP2且有SSL",
			config:  HTTP2Config{Enabled: true},
			hasSSL:  true,
			wantErr: false,
		},
		{
			name:    "启用HTTP2但无SSL",
			config:  HTTP2Config{Enabled: true},
			hasSSL:  false,
			wantErr: true,
			errMsg:  "HTTP/2 需要配置 SSL/TLS 证书",
		},
		{
			name:    "启用H2C但无SSL",
			config:  HTTP2Config{Enabled: true, H2CEnabled: true},
			hasSSL:  false,
			wantErr: false,
		},
		{
			name:    "负数MaxConcurrentStreams",
			config:  HTTP2Config{MaxConcurrentStreams: -1},
			hasSSL:  false,
			wantErr: true,
			errMsg:  "max_concurrent_streams 不能为负数",
		},
		{
			name:    "负数MaxHeaderListSize",
			config:  HTTP2Config{MaxHeaderListSize: -1},
			hasSSL:  false,
			wantErr: true,
			errMsg:  "max_header_list_size 不能为负数",
		},
		{
			name:    "负数IdleTimeout",
			config:  HTTP2Config{IdleTimeout: -1},
			hasSSL:  false,
			wantErr: true,
			errMsg:  "idle_timeout 不能为负数",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHTTP2(&tt.config, tt.hasSSL)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateHTTP2() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateHTTP2() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateHTTP2() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}

func TestValidateRedirectRewrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		errMsg  string
		config  *RedirectRewriteConfig
		wantErr bool
	}{
		{
			name:    "nil配置有效",
			config:  nil,
			wantErr: false,
		},
		{
			name:    "空配置有效",
			config:  &RedirectRewriteConfig{},
			wantErr: false,
		},
		{
			name:    "default模式有效",
			config:  &RedirectRewriteConfig{Mode: "default"},
			wantErr: false,
		},
		{
			name:    "off模式有效",
			config:  &RedirectRewriteConfig{Mode: "off"},
			wantErr: false,
		},
		{
			name: "custom模式有规则",
			config: &RedirectRewriteConfig{
				Mode: "custom",
				Rules: []RedirectRewriteRule{
					{Pattern: "^/old$", Replacement: "/new"},
				},
			},
			wantErr: false,
		},
		{
			name:    "custom模式无规则",
			config:  &RedirectRewriteConfig{Mode: "custom"},
			wantErr: true,
			errMsg:  "rules required when mode is custom",
		},
		{
			name:    "无效模式",
			config:  &RedirectRewriteConfig{Mode: "invalid"},
			wantErr: true,
			errMsg:  "must be one of",
		},
		{
			name: "规则pattern为空",
			config: &RedirectRewriteConfig{
				Mode: "custom",
				Rules: []RedirectRewriteRule{
					{Pattern: "", Replacement: "/new"},
				},
			},
			wantErr: true,
			errMsg:  "pattern cannot be empty",
		},
		{
			name: "正则模式有效",
			config: &RedirectRewriteConfig{
				Mode: "custom",
				Rules: []RedirectRewriteRule{
					{Pattern: "~^/old/(.*)$", Replacement: "/new/$1"},
				},
			},
			wantErr: false,
		},
		{
			name: "正则模式无效",
			config: &RedirectRewriteConfig{
				Mode: "custom",
				Rules: []RedirectRewriteRule{
					{Pattern: "~[invalid(regex", Replacement: "/new"},
				},
			},
			wantErr: true,
			errMsg:  "invalid regex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRedirectRewrite(tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateRedirectRewrite() 期望返回错误，但返回 nil")
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateRedirectRewrite() 错误消息不匹配，期望包含 %q，实际 %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("validateRedirectRewrite() 期望返回 nil，但返回错误: %v", err)
				}
			}
		})
	}
}
