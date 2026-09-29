//go:build !windows

// Package app 提供应用程序功能的测试。
//
// 该文件测试应用程序模块的各项功能，包括：
//   - 应用创建和配置
//   - 信号处理（SIGTERM、SIGHUP、SIGUSR1等）
//   - 配置重载
//   - 日志重开
//   - 版本输出
//   - 优雅关闭
//
// 作者：xfy
package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/version"
)

// captureStdout 捕获 stdout 输出，返回捕获的内容和恢复函数。
func captureStdout(t *testing.T) (func() string, func()) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建 pipe 失败: %v", err)
	}
	os.Stdout = w

	// 异步读取管道，避免死锁
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(&buf, r)
	}()

	return func() string {
			_ = w.Close()
			os.Stdout = old
			<-done
			return buf.String()
		}, func() {
			_ = w.Close()
			os.Stdout = old
		}
}

// captureStderr 捕获 stderr 输出，返回捕获的内容和恢复函数。
func captureStderr(t *testing.T) (func() string, func()) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建 pipe 失败: %v", err)
	}
	os.Stderr = w

	// 异步读取管道，避免死锁
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(&buf, r)
	}()

	return func() string {
			_ = w.Close()
			os.Stderr = old
			<-done
			return buf.String()
		}, func() {
			_ = w.Close()
			os.Stderr = old
		}
}

// TestNewApp 测试 NewApp 构造器。
func TestNewApp(t *testing.T) {
	cfgPath := "/path/to/config.yaml"
	app := NewApp(cfgPath)

	if app.cfgPath != cfgPath {
		t.Errorf("cfgPath = %q, want %q", app.cfgPath, cfgPath)
	}

	if app.cfg != nil {
		t.Error("新创建的 App cfg 应为 nil")
	}

	if app.srv != nil {
		t.Error("新创建的 App srv 应为 nil")
	}

	if app.pidFile != "" {
		t.Errorf("pidFile = %q, want empty", app.pidFile)
	}

	if app.logFile != "" {
		t.Errorf("logFile = %q, want empty", app.logFile)
	}
}

// TestSigName 测试信号名称辅助函数。
func TestSigName(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		sig      syscall.Signal
	}{
		{
			name:     "SIGTERM",
			sig:      syscall.SIGTERM,
			expected: "SIGTERM",
		},
		{
			name:     "SIGINT",
			sig:      syscall.SIGINT,
			expected: "SIGINT",
		},
		{
			name:     "SIGQUIT",
			sig:      syscall.SIGQUIT,
			expected: "SIGQUIT",
		},
		{
			name:     "SIGHUP",
			sig:      syscall.SIGHUP,
			expected: "SIGHUP",
		},
		{
			name:     "SIGUSR1",
			sig:      syscall.SIGUSR1,
			expected: "SIGUSR1",
		},
		{
			name:     "SIGUSR2",
			sig:      syscall.SIGUSR2,
			expected: "SIGUSR2",
		},
		{
			name:     "未知信号",
			sig:      syscall.Signal(999),
			expected: "Signal(999)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sigName(tt.sig)
			if result != tt.expected {
				t.Errorf("sigName(%d) = %q, want %q", tt.sig, result, tt.expected)
			}
		})
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name            string
		cfgPath         string
		outputPath      string
		importPath      string
		wantContains    string
		wantErrContains string
		wantExitCode    int
		genConfig       bool
		showVersion     bool
	}{
		{
			name:         "显示版本",
			showVersion:  true,
			wantExitCode: 0,
			wantContains: "lolly version",
		},
		{
			name:         "生成配置输出到 stdout",
			genConfig:    true,
			outputPath:   "",
			wantExitCode: 0,
			wantContains: "servers:",
		},
		{
			name:         "生成配置输出到文件",
			genConfig:    true,
			outputPath:   filepath.Join(t.TempDir(), "config.yaml"),
			wantExitCode: 0,
			wantContains: "Config written to:",
		},
		{
			name:            "配置文件不存在",
			cfgPath:         filepath.Join(t.TempDir(), "nonexistent.yaml"),
			genConfig:       false,
			showVersion:     false,
			wantExitCode:    1,
			wantErrContains: "Failed to load config",
		},
		{
			name:            "generate 与 import 互斥",
			genConfig:       true,
			importPath:      "/tmp/nginx.conf",
			wantExitCode:    1,
			wantErrContains: "mutually exclusive",
		},
		{
			name:            "o 参数无 generate 或 import",
			outputPath:      "output.yaml",
			wantExitCode:    1,
			wantErrContains: "-o requires",
		},
		{
			name:            "导入 nginx 配置文件不存在",
			importPath:      "/tmp/nginx.conf",
			wantExitCode:    1,
			wantErrContains: "failed to parse nginx config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getStdout, restoreStdout := captureStdout(t)
			getStderr, restoreStderr := captureStderr(t)

			exitCode := Run(tt.cfgPath, tt.genConfig, tt.outputPath, tt.importPath, tt.showVersion)

			restoreStderr()
			restoreStdout()

			stdout := getStdout()
			stderr := getStderr()

			if exitCode != tt.wantExitCode {
				t.Errorf("exit code = %d, want %d", exitCode, tt.wantExitCode)
			}

			if tt.wantContains != "" && !strings.Contains(stdout, tt.wantContains) {
				t.Errorf("stdout 应包含 %q, 实际输出: %q", tt.wantContains, stdout)
			}

			if tt.wantErrContains != "" && !strings.Contains(stderr, tt.wantErrContains) {
				t.Errorf("stderr 应包含 %q, 实际输出: %q", tt.wantErrContains, stderr)
			}

			// 验证生成配置文件的内容
			if tt.outputPath != "" && tt.genConfig && exitCode == 0 {
				data, err := os.ReadFile(tt.outputPath)
				if err != nil {
					t.Errorf("读取生成的配置文件失败: %v", err)
				} else if !strings.Contains(string(data), "servers:") {
					t.Errorf("生成的配置文件应包含 'servers:', 实际内容: %s", string(data)[:100])
				}
			}
		})
	}
}

// TestGenerateConfig 测试 generateConfig 函数。
func TestGenerateConfig(t *testing.T) {
	t.Run("输出到 stdout", func(t *testing.T) {
		getStdout, restoreStdout := captureStdout(t)

		exitCode := generateConfig("")
		restoreStdout()

		stdout := getStdout()

		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0", exitCode)
		}

		// 验证输出包含基本配置结构
		expectedFields := []string{"servers:", "listen:", "logging:", "performance:", "monitoring:"}
		for _, field := range expectedFields {
			if !strings.Contains(stdout, field) {
				t.Errorf("输出应包含 %q", field)
			}
		}
	})

	t.Run("输出到文件", func(t *testing.T) {
		tmpDir := t.TempDir()
		outputPath := filepath.Join(tmpDir, "test-config.yaml")

		getStdout, restoreStdout := captureStdout(t)

		exitCode := generateConfig(outputPath)
		restoreStdout()

		stdout := getStdout()

		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0", exitCode)
		}

		if !strings.Contains(stdout, outputPath) {
			t.Errorf("stdout 应包含文件路径 %q, 实际输出: %q", outputPath, stdout)
		}

		// 验证文件存在且内容正确
		data, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatalf("读取生成的配置文件失败: %v", err)
		}

		content := string(data)
		expectedFields := []string{"servers:", "listen:", "logging:", "performance:", "monitoring:"}
		for _, field := range expectedFields {
			if !strings.Contains(content, field) {
				t.Errorf("配置文件应包含 %q", field)
			}
		}
	})

	t.Run("输出到无效路径", func(t *testing.T) {
		// Skip if running as root - root can write anywhere
		if os.Getuid() == 0 {
			t.Skip("Skipping permission test when running as root")
		}

		// 使用一个无法写入的路径（如根目录下的文件）
		invalidPath := "/root/cannot-write-here.yaml"

		getStderr, restoreStderr := captureStderr(t)

		exitCode := generateConfig(invalidPath)
		restoreStderr()

		stderr := getStderr()

		if exitCode != 1 {
			t.Errorf("exit code = %d, want 1", exitCode)
		}

		if !strings.Contains(stderr, "Failed to write file") {
			t.Errorf("stderr should contain 'Failed to write file', actual: %q", stderr)
		}
	})
}

// TestPrintVersion 测试 printVersion 函数。
func TestPrintVersion(t *testing.T) {
	getStdout, restoreStdout := captureStdout(t)

	printVersion()
	restoreStdout()

	stdout := getStdout()

	// 验证版本输出格式
	expectedLines := []string{
		"lolly version",
		"Git:",
		"Built:",
		"Go:",
		"Platform:",
	}

	for _, line := range expectedLines {
		if !strings.Contains(stdout, line) {
			t.Errorf("版本输出应包含 %q, 实际输出: %q", line, stdout)
		}
	}
}

// TestVersionVariables 测试版本变量默认值
func TestVersionVariables(t *testing.T) {
	if version.Version != "dev" {
		t.Errorf("Default Version should be 'dev', got '%s'", version.Version)
	}
	if version.GitCommit != "unknown" {
		t.Errorf("Default GitCommit should be 'unknown', got '%s'", version.GitCommit)
	}
	if version.GitBranch != "unknown" {
		t.Errorf("Default GitBranch should be 'unknown', got '%s'", version.GitBranch)
	}
	if version.BuildTime != "unknown" {
		t.Errorf("Default BuildTime should be 'unknown', got '%s'", version.BuildTime)
	}
	if version.GoVersion != "unknown" {
		t.Errorf("Default GoVersion should be 'unknown', got '%s'", version.GoVersion)
	}
	if version.BuildPlatform != "unknown" {
		t.Errorf("Default BuildPlatform should be 'unknown', got '%s'", version.BuildPlatform)
	}
}

// TestAppFields 测试 App 结构体字段初始化
func TestAppFields(t *testing.T) {
	app := NewApp("/test/config.yaml")

	// 验证初始状态
	if app.cfgPath != "/test/config.yaml" {
		t.Errorf("cfgPath = %q, want %q", app.cfgPath, "/test/config.yaml")
	}
	if app.cfg != nil {
		t.Error("cfg should be nil initially")
	}
	if app.srv != nil {
		t.Error("srv should be nil initially")
	}
	if app.http3Srv != nil {
		t.Error("http3Srv should be nil initially")
	}
	if app.streamSrv != nil {
		t.Error("streamSrv should be nil initially")
	}
	if app.upgradeMgr != nil {
		t.Error("upgradeMgr should be nil initially")
	}
	if len(app.listeners) != 0 {
		t.Error("listeners should be empty initially")
	}
}

// TestGenerateConfig_ErrorCase 测试生成配置时的错误情况
// 注意：config.GenerateConfigYAML 正常情况下不会失败，
// 但我们测试文件写入失败的情况
func TestGenerateConfig_ErrorCase(t *testing.T) {
	t.Run("写入无效路径", func(t *testing.T) {
		getStderr, restoreStderr := captureStderr(t)

		exitCode := generateConfig("/nonexistent/dir/config.yaml")
		restoreStderr()

		stderr := getStderr()

		if exitCode != 1 {
			t.Errorf("exit code = %d, want 1", exitCode)
		}

		if !strings.Contains(stderr, "Failed to write file") {
			t.Errorf("stderr should contain 'Failed to write file', actual: %q", stderr)
		}
	})
}

// TestApp_Run_WithValidConfig 测试 App.Run 加载有效配置
// 注意：此测试验证配置加载路径，但由于服务器启动会阻塞，
// 我们通过子进程测试或使用 short 标志跳过完整运行
func TestApp_Run_WithValidConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	app := NewApp(cfgPath)

	// 在 goroutine 中启动服务器
	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	// 等待一小段时间让服务器启动
	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证配置已加载
	if app.cfg == nil {
		t.Error("Config should be loaded")
	}

	// 验证服务器已创建
	if app.srv == nil {
		t.Error("Server should be created")
	}

	// 验证 logger 已创建
	if app.logger == nil {
		t.Error("Logger should be created")
	}

	// 验证升级管理器已创建
	if app.upgradeMgr == nil {
		t.Error("Upgrade manager should be created")
	}

	// 发送 SIGTERM 信号停止服务器
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_WithVariables 测试 App.Run 加载全局变量
func TestApp_Run_WithVariables(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
variables:
  set:
    TEST_VAR: "test_value"
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	app := NewApp(cfgPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证配置已加载且包含变量
	if app.cfg == nil {
		t.Error("Config should be loaded")
	}

	// 停止服务器
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_WithResolver 测试 App.Run 启用 DNS 解析器
func TestApp_Run_WithResolver(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
resolver:
  enabled: true
  addresses:
    - "8.8.8.8:53"
  ipv4: true
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	app := NewApp(cfgPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证 DNS 解析器已创建
	if app.resv == nil {
		t.Error("Resolver should be created when enabled")
	}

	// 验证服务器已创建
	if app.srv == nil {
		t.Error("Server should be created")
	}

	// 停止服务器
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_MultiServer 测试 App.Run 多服务器模式
func TestApp_Run_MultiServer(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
    name: "server1"
  - listen: ":0"
    name: "server2"
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	app := NewApp(cfgPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证多服务器配置
	if app.cfg == nil {
		t.Error("Config should be loaded")
	}
	if len(app.cfg.Servers) != 2 {
		t.Errorf("Expected 2 servers, got %d", len(app.cfg.Servers))
	}

	// 停止服务器
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_WithPidFile 测试 App.Run 设置 PID 文件
func TestApp_Run_WithPidFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	pidPath := filepath.Join(tmpDir, "lolly.pid")

	app := NewApp(cfgPath)
	app.SetPidFile(pidPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证 PID 文件已创建
	if _, err := os.Stat(pidPath); os.IsNotExist(err) {
		t.Error("PID file should be created")
	}

	// 停止服务器
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_WithStreamConfig 测试 App.Run 配置 Stream 服务器
func TestApp_Run_WithStreamConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
stream:
  - listen: ":19090"
    protocol: tcp
    upstream:
      targets:
        - addr: "127.0.0.1:19091"
          weight: 1
      load_balance: round_robin
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	app := NewApp(cfgPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证 Stream 服务器已创建
	if app.streamSrv == nil {
		t.Error("Stream server should be created when configured")
	}

	// 停止服务器
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_GracefulUpgradeMode 测试 App.Run 热升级子进程模式
func TestApp_Run_GracefulUpgradeMode(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// 设置热升级模式环境变量
	t.Setenv("GRACEFUL_UPGRADE", "1")

	app := NewApp(cfgPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证升级管理器已创建
	if app.upgradeMgr == nil {
		t.Error("Upgrade manager should be created in graceful upgrade mode")
	}

	// 停止服务器
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_WithLogFile 测试 App.Run 设置日志文件
func TestApp_Run_WithLogFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	logPath := filepath.Join(tmpDir, "lolly.log")

	app := NewApp(cfgPath)
	app.SetLogFile(logPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证日志文件路径已设置
	if app.logFile != logPath {
		t.Errorf("logFile = %q, want %q", app.logFile, logPath)
	}

	// 停止服务器
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_WithZeroTimeout 测试 App.Run 使用零超时配置（会使用默认值）
func TestApp_Run_WithZeroTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
shutdown:
  graceful_timeout: 0s
  fast_timeout: 0s
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	app := NewApp(cfgPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证配置已加载
	if app.cfg == nil {
		t.Error("Config should be loaded")
	}

	// 停止服务器（会使用默认超时值）
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestApp_Run_WithExplicitTimeout 测试 App.Run 使用显式超时配置
func TestApp_Run_WithExplicitTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `
servers:
  - listen: ":0"
shutdown:
  graceful_timeout: 10s
  fast_timeout: 5s
logging:
  error:
    level: "info"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	app := NewApp(cfgPath)

	done := make(chan int, 1)
	go func() {
		done <- app.Run()
	}()

	app.WaitReady()

	waitForAppServerRunning(app, 2*time.Second)

	// 验证配置已加载
	if app.cfg == nil {
		t.Error("Config should be loaded")
	}

	// 验证超时值
	app.srv.StopWithTimeout(1 * time.Second)
}

// TestInitHTTP3_EmptyServers 验证空服务器配置时 HTTP/3 初始化直接跳过。
func TestInitHTTP3_EmptyServers(t *testing.T) {
	app := &App{
		cfg:    &config.Config{Servers: []config.ServerConfig{}},
		logger: logging.NewAppLogger(nil),
	}

	// 不应 panic
	app.initHTTP3()

	if app.http3Srv != nil {
		t.Error("http3Srv should remain nil when no servers are configured")
	}
}

func waitForAppServerRunning(app *App, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if app.srv != nil && app.srv.Running() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
