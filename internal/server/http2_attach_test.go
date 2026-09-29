// Package server 提供 HTTP/2 ALPN 集成的端到端测试。
//
// 验证启用 ssl.http2.enabled 后，HTTPS 请求经 ALPN 协商为 HTTP/2；
// 未启用时仍以 HTTP/1.1 服务，且不因 ALPN 通告 h2 而中断连接。
//
// 作者：xfy
package server

import (
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"rua.plus/lolly/internal/config"
)

// startTLSHTTP2Server 启动一个带 TLS 的单服务器，返回其监听地址和停止函数。
//
// 参数：
//   - http2Enabled: 是否启用 ssl.http2.enabled
//
// 返回值：
//   - addr: 服务器监听地址（host:port）
//   - stop: 关闭服务器并回收资源的函数
func startTLSHTTP2Server(t *testing.T, http2Enabled bool) (addr string, stop func()) {
	t.Helper()

	tempDir := t.TempDir()
	certFile := tempDir + "/cert.pem"
	keyFile := tempDir + "/key.pem"
	if err := generateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("生成自签名证书失败: %v", err)
	}

	if err := os.WriteFile(tempDir+"/index.html", []byte("hello-h2"), 0o644); err != nil {
		t.Fatalf("写入 index.html 失败: %v", err)
	}

	cfg := &config.Config{
		Servers: []config.ServerConfig{{
			Listen: "127.0.0.1:0",
			SSL: config.SSLConfig{
				Cert: certFile,
				Key:  keyFile,
				HTTP2: config.HTTP2Config{
					Enabled: http2Enabled,
				},
			},
			Static: []config.StaticConfig{{
				Path:  "/",
				Root:  tempDir,
				Index: []string{"index.html"},
			}},
		}},
	}

	s := New(cfg)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	if !waitForServerRunning(s, 2*time.Second) {
		_ = s.GracefulStop(2 * time.Second)
		t.Fatal("服务器未在超时内启动")
	}

	listeners := s.GetListeners()
	if len(listeners) == 0 {
		_ = s.GracefulStop(2 * time.Second)
		t.Fatal("服务器未暴露监听器")
	}
	addr = listeners[0].Addr().String()

	return addr, func() {
		_ = s.GracefulStop(2 * time.Second)
		select {
		case err := <-errCh:
			if err != nil && !isExpectedServerErrorForIntegration(err) {
				t.Logf("服务器关闭返回错误: %v", err)
			}
		default:
		}
	}
}

// newHTTP2Client 创建一个信任自签名证书、支持 HTTP/2 的客户端。
func newHTTP2Client(t *testing.T) *http.Client {
	t.Helper()
	tlsCfg := &tls.Config{InsecureSkipVerify: true}
	transport := &http.Transport{TLSClientConfig: tlsCfg}
	if err := http2.ConfigureTransport(transport); err != nil {
		t.Fatalf("ConfigureTransport 失败: %v", err)
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

// TestHTTP2Attach_Enabled 验证启用 HTTP/2 后请求经 ALPN 协商为 HTTP/2。
func TestHTTP2Attach_Enabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	addr, stop := startTLSHTTP2Server(t, true)
	defer stop()

	client := newHTTP2Client(t)
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTP/2 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.ProtoMajor != 2 {
		t.Errorf("期望 HTTP/2，实际 Proto=%s", resp.Proto)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if string(body) != "hello-h2" {
		t.Errorf("响应体不匹配: 期望 hello-h2，实际 %q", string(body))
	}
}

// TestHTTP2Attach_Disabled 验证未启用 HTTP/2 时请求以 HTTP/1.1 服务，
// 且 ALPN 不会因通告 h2 而无处理器可分派导致连接中断。
func TestHTTP2Attach_Disabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	addr, stop := startTLSHTTP2Server(t, false)
	defer stop()

	client := newHTTP2Client(t)
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTPS 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.ProtoMajor != 1 {
		t.Errorf("期望 HTTP/1.1，实际 Proto=%s", resp.Proto)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if string(body) != "hello-h2" {
		t.Errorf("响应体不匹配: 期望 hello-h2，实际 %q", string(body))
	}
}
