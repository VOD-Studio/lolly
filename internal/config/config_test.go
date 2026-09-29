// Package config 提供配置加载和管理的测试。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoad 测试从文件加载配置。
func TestLoad(t *testing.T) {
	t.Run("有效配置文件", func(t *testing.T) {
		// 创建临时配置文件
		content := `
servers:
  - listen: ":8080"
    static:
      - path: "/"
        root: "/var/www"
        index:
          - "index.html"
logging:
  access:
    path: "/var/log/access.log"
    format: "combined"
  error:
    path: "/var/log/error.log"
    level: "info"
performance:
  goroutine_pool:
    enabled: true
    max_workers: 100
  file_cache:
    max_entries: 1000
monitoring:
  status:
    path: "/status"
`
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "config.yaml")
		if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
			t.Fatalf("创建临时配置文件失败: %v", err)
		}

		cfg, err := Load(tmpFile)
		if err != nil {
			t.Fatalf("Load() 失败: %v", err)
		}

		if cfg.Servers[0].Listen != ":8080" {
			t.Errorf("Servers[0].Listen = %q, want %q", cfg.Servers[0].Listen, ":8080")
		}
		if cfg.Servers[0].Static[0].Root != "/var/www" {
			t.Errorf("Servers[0].Static.Root = %q, want %q", cfg.Servers[0].Static[0].Root, "/var/www")
		}
		if len(cfg.Servers[0].Static[0].Index) != 1 || cfg.Servers[0].Static[0].Index[0] != "index.html" {
			t.Errorf("Servers[0].Static.Index = %v, want [index.html]", cfg.Servers[0].Static[0].Index)
		}
	})

	t.Run("文件不存在", func(t *testing.T) {
		_, err := Load("/nonexistent/path/config.yaml")
		if err == nil {
			t.Error("Load() 期望返回错误，但返回 nil")
		}
	})

	t.Run("无效YAML", func(t *testing.T) {
		content := `
server:
  listen: ":8080"
  static:
    root: [invalid yaml structure
`
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "invalid.yaml")
		if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
			t.Fatalf("创建临时配置文件失败: %v", err)
		}

		_, err := Load(tmpFile)
		if err == nil {
			t.Error("Load() 期望返回错误，但返回 nil")
		}
	})

	t.Run("缺少必填字段（无服务器配置）", func(t *testing.T) {
		content := `
logging:
  access:
    path: "/var/log/access.log"
`
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "no_server.yaml")
		if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
			t.Fatalf("创建临时配置文件失败: %v", err)
		}

		_, err := Load(tmpFile)
		if err == nil {
			t.Error("Load() 期望返回错误，但返回 nil")
		}
	})

	t.Run("多虚拟主机模式", func(t *testing.T) {
		content := `
servers:
  - listen: ":8080"
    name: "server1"
  - listen: ":8081"
    name: "server2"
`
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "multi.yaml")
		if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
			t.Fatalf("创建临时配置文件失败: %v", err)
		}

		cfg, err := Load(tmpFile)
		if err != nil {
			t.Fatalf("Load() 失败: %v", err)
		}

		if len(cfg.Servers) != 2 {
			t.Fatalf("len(Servers) = %d, want 2", len(cfg.Servers))
		}
		if cfg.Servers[0].Name != "server1" {
			t.Errorf("Servers[0].Name = %q, want %q", cfg.Servers[0].Name, "server1")
		}
		if cfg.Servers[1].Name != "server2" {
			t.Errorf("Servers[1].Name = %q, want %q", cfg.Servers[1].Name, "server2")
		}
	})
}

// TestConfigMethods 测试 Config 结构体的方法。

func TestProxyBufferingConfig_ParseBuffers(t *testing.T) {
	tests := []struct {
		name         string
		buffers      string
		bufferSize   int
		wantCount    int
		wantSizeEach int
	}{
		{
			name:         "empty uses buffer_size",
			buffers:      "",
			bufferSize:   4096,
			wantCount:    1,
			wantSizeEach: 4096,
		},
		{
			name:         "8 16k format",
			buffers:      "8 16k",
			wantCount:    8,
			wantSizeEach: 16 * 1024,
		},
		{
			name:         "4 4k format",
			buffers:      "4 4k",
			wantCount:    4,
			wantSizeEach: 4 * 1024,
		},
		{
			name:         "2 1m format",
			buffers:      "2 1m",
			wantCount:    2,
			wantSizeEach: 1024 * 1024,
		},
		{
			name:         "bytes without unit",
			buffers:      "4 8192",
			wantCount:    4,
			wantSizeEach: 8192,
		},
		{
			name:         "uppercase K",
			buffers:      "8 16K",
			wantCount:    8,
			wantSizeEach: 16 * 1024,
		},
		{
			name:         "invalid format",
			buffers:      "invalid",
			wantCount:    0,
			wantSizeEach: 0,
		},
		{
			name:         "missing size",
			buffers:      "8",
			wantCount:    0,
			wantSizeEach: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ProxyBufferingConfig{
				Buffers:    tt.buffers,
				BufferSize: tt.bufferSize,
			}
			cfg.ParseBuffers()

			if cfg.BufferCount != tt.wantCount {
				t.Errorf("BufferCount = %d, want %d", cfg.BufferCount, tt.wantCount)
			}
			if cfg.BufferSizeEach != tt.wantSizeEach {
				t.Errorf("BufferSizeEach = %d, want %d", cfg.BufferSizeEach, tt.wantSizeEach)
			}
		})
	}
}

// TestProxyBufferingConfig_RequestStreamingEnabled 测试请求体流式判定。
func TestProxyBufferingConfig_RequestStreamingEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  *ProxyBufferingConfig
		want bool
	}{
		{name: "nil", cfg: nil, want: false},
		{name: "empty request_mode", cfg: &ProxyBufferingConfig{}, want: false},
		{name: "default", cfg: &ProxyBufferingConfig{RequestMode: "default"}, want: false},
		{name: "on", cfg: &ProxyBufferingConfig{RequestMode: "on"}, want: false},
		{name: "off", cfg: &ProxyBufferingConfig{RequestMode: "off"}, want: true},
		{name: "response off only", cfg: &ProxyBufferingConfig{Mode: "off"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.RequestStreamingEnabled(); got != tt.want {
				t.Errorf("RequestStreamingEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestValidateProxyBuffering 测试代理缓冲配置校验。
func TestValidateProxyBuffering(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *ProxyBufferingConfig
		wantErr bool
	}{
		{name: "nil", cfg: nil, wantErr: false},
		{name: "empty", cfg: &ProxyBufferingConfig{}, wantErr: false},
		{name: "valid off", cfg: &ProxyBufferingConfig{Mode: "off", RequestMode: "off"}, wantErr: false},
		{name: "invalid mode", cfg: &ProxyBufferingConfig{Mode: "yes"}, wantErr: true},
		{name: "invalid request_mode", cfg: &ProxyBufferingConfig{RequestMode: "yes"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProxyBuffering(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateProxyBuffering() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestAnyProxyRequestStreaming 测试请求体流式检测的自由函数。
func TestAnyProxyRequestStreaming(t *testing.T) {
	servers := []ServerConfig{
		{}, // 无代理
		{
			Proxy: []ProxyConfig{{
				Path:      "/",
				Targets:   []ProxyTarget{{URL: "http://127.0.0.1:1"}},
				Buffering: &ProxyBufferingConfig{Mode: "off"},
			}},
		},
	}
	if AnyProxyRequestStreaming(servers) {
		t.Error("无 request_mode: off 时应返回 false")
	}

	servers = append(servers, ServerConfig{
		Proxy: []ProxyConfig{{
			Path:      "/upload",
			Targets:   []ProxyTarget{{URL: "http://127.0.0.1:1"}},
			Buffering: &ProxyBufferingConfig{RequestMode: "off"},
		}},
	})
	if !AnyProxyRequestStreaming(servers) {
		t.Error("存在 request_mode: off 时应返回 true")
	}
}

func TestLoad_Include(t *testing.T) {
	t.Run("append servers from include", func(t *testing.T) {
		tmpDir := t.TempDir()

		mainCfg := `
servers:
  - listen: ":8080"
    name: main
include:
  - path: "conf.d/*.yaml"
`
		incCfg := `
servers:
  - listen: ":9090"
    name: included
`
		confDir := filepath.Join(tmpDir, "conf.d")
		if err := os.MkdirAll(confDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(mainCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(confDir, "extra.yaml"), []byte(incCfg), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := Load(filepath.Join(tmpDir, "config.yaml"))
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if len(cfg.Servers) != 2 {
			t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
		}
		if cfg.Servers[0].Name != "main" {
			t.Errorf("Servers[0].Name = %q, want %q", cfg.Servers[0].Name, "main")
		}
		if cfg.Servers[1].Name != "included" {
			t.Errorf("Servers[1].Name = %q, want %q", cfg.Servers[1].Name, "included")
		}
	})

	t.Run("merge variables with main priority", func(t *testing.T) {
		tmpDir := t.TempDir()

		mainCfg := `
servers:
  - listen: ":8080"
variables:
  set:
    app: lolly
    env: production
include:
  - path: "extra.yaml"
`
		incCfg := `
servers:
  - listen: ":9090"
variables:
  set:
    app: other
    debug: "true"
`
		if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(mainCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "extra.yaml"), []byte(incCfg), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := Load(filepath.Join(tmpDir, "config.yaml"))
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if cfg.Variables.Set["app"] != "lolly" {
			t.Errorf("app = %q, want %q (main should win)", cfg.Variables.Set["app"], "lolly")
		}
		if cfg.Variables.Set["debug"] != "true" {
			t.Errorf("debug = %q, want %q (included should fill missing)", cfg.Variables.Set["debug"], "true")
		}
		if cfg.Variables.Set["env"] != "production" {
			t.Errorf("env = %q, want %q", cfg.Variables.Set["env"], "production")
		}
	})

	t.Run("no matches returns error", func(t *testing.T) {
		tmpDir := t.TempDir()

		mainCfg := `
servers:
  - listen: ":8080"
include:
  - path: "nonexistent/*.yaml"
`
		if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(mainCfg), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := Load(filepath.Join(tmpDir, "config.yaml"))
		if err == nil {
			t.Error("expected error for non-matching glob pattern")
		}
	})

	t.Run("circular include detected immediately", func(t *testing.T) {
		tmpDir := t.TempDir()

		cfg1 := `
servers:
  - listen: ":8080"
include:
  - path: "b.yaml"
`
		cfg2 := `
servers:
  - listen: ":9090"
include:
  - path: "a.yaml"
`
		if err := os.WriteFile(filepath.Join(tmpDir, "a.yaml"), []byte(cfg1), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "b.yaml"), []byte(cfg2), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := Load(filepath.Join(tmpDir, "a.yaml"))
		if err == nil {
			t.Error("expected error for circular include")
		}
		if !strings.Contains(err.Error(), "循环引入") {
			t.Errorf("error should mention circular include, got: %v", err)
		}
	})

	t.Run("self include detected", func(t *testing.T) {
		tmpDir := t.TempDir()

		cfg := `
servers:
  - listen: ":8080"
include:
  - path: "a.yaml"
`
		if err := os.WriteFile(filepath.Join(tmpDir, "a.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := Load(filepath.Join(tmpDir, "a.yaml"))
		if err == nil {
			t.Error("expected error for self include")
		}
	})

	t.Run("diamond include works", func(t *testing.T) {
		tmpDir := t.TempDir()

		mainCfg := `
servers:
  - listen: ":8080"
include:
  - path: "b.yaml"
  - path: "c.yaml"
`
		bCfg := `
servers:
  - listen: ":9090"
include:
  - path: "shared.yaml"
`
		cCfg := `
servers:
  - listen: ":9091"
include:
  - path: "shared.yaml"
`
		sharedCfg := `
variables:
  set:
    shared: value
`
		if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(mainCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "b.yaml"), []byte(bCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "c.yaml"), []byte(cCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "shared.yaml"), []byte(sharedCfg), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := Load(filepath.Join(tmpDir, "config.yaml"))
		if err != nil {
			t.Fatalf("diamond include should work: %v", err)
		}

		if len(cfg.Servers) != 3 {
			t.Errorf("expected 3 servers, got %d", len(cfg.Servers))
		}
		if cfg.Variables.Set["shared"] != "value" {
			t.Error("shared variable should be merged")
		}
	})

	t.Run("empty include list is no-op", func(t *testing.T) {
		tmpDir := t.TempDir()

		mainCfg := `
servers:
  - listen: ":8080"
`
		if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(mainCfg), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := Load(filepath.Join(tmpDir, "config.yaml"))
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if len(cfg.Servers) != 1 {
			t.Errorf("expected 1 server, got %d", len(cfg.Servers))
		}
	})

	t.Run("append stream from include", func(t *testing.T) {
		tmpDir := t.TempDir()

		mainCfg := `
servers:
  - listen: ":8080"
stream:
  - listen: ":5432"
    protocol: tcp
    upstream:
      targets:
        - addr: "127.0.0.1:9000"
include:
  - path: "stream.yaml"
`
		incCfg := `
stream:
  - listen: ":5433"
    protocol: udp
    upstream:
      targets:
        - addr: "127.0.0.1:9001"
`
		if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(mainCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "stream.yaml"), []byte(incCfg), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := Load(filepath.Join(tmpDir, "config.yaml"))
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		if len(cfg.Stream) != 2 {
			t.Fatalf("expected 2 streams, got %d", len(cfg.Stream))
		}
	})
}
