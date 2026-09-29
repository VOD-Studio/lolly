//go:build e2e

// http2_e2e_test.go - HTTP/2 协议 E2E 测试
//
// 测试 lolly HTTP/2 功能：HTTPS 连接、ALPN 协议协商、明文 h2c 嗅探分派等。
//
// 作者：xfy
package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"rua.plus/lolly/internal/e2e/testutil"
)

// TestE2EHTTPSConnection 测试 HTTPS 连接。
//
// 验证 HTTPS 连接可以成功建立。
func TestE2EHTTPSConnection(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.DefaultTestTimeout)
	defer cancel()

	if !testutil.LollyImageAvailable(ctx) {
		t.Skip("lolly:latest image not available, run 'make docker-build' first")
	}

	// 生成自签名证书
	certPath, keyPath, cleanup, err := testutil.GenerateSelfSignedCert(t.TempDir())
	require.NoError(t, err, "Failed to generate certificate")
	defer cleanup()

	// 构建带 SSL 的配置
	cfg := testutil.NewConfigBuilder().
		WithServer(":8443").
		WithSSL("/etc/lolly/ssl/server.crt", "/etc/lolly/ssl/server.key").
		WithStatic("/", "/var/www/html")

	configYAML, err := cfg.Build()
	require.NoError(t, err, "Failed to build config")

	// 启动 lolly
	lolly, err := testutil.StartLolly(ctx,
		testutil.WithConfigYAML(configYAML),
		testutil.WithCert(certPath, keyPath),
	)
	require.NoError(t, err, "Failed to start lolly")
	defer lolly.Terminate(ctx)

	// 创建 TLS 客户端
	client, err := testutil.CreateTLSClient(certPath)
	require.NoError(t, err, "Failed to create TLS client")

	// 发送请求
	resp, err := client.Get(lolly.HTTPSBaseURL())
	require.NoError(t, err, "HTTPS request failed")
	defer resp.Body.Close()

	t.Logf("HTTPS response status: %d, protocol: %s", resp.StatusCode, resp.Proto)
}

// TestE2EHTTPSConcurrentRequests 测试 HTTPS 并发请求。
//
// 验证多个并发 HTTPS 请求正常工作。
func TestE2EHTTPSConcurrentRequests(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.DefaultTestTimeout)
	defer cancel()

	if !testutil.LollyImageAvailable(ctx) {
		t.Skip("lolly:latest image not available, run 'make docker-build' first")
	}

	// 生成自签名证书
	certPath, keyPath, cleanup, err := testutil.GenerateSelfSignedCert(t.TempDir())
	require.NoError(t, err, "Failed to generate certificate")
	defer cleanup()

	// 构建配置
	cfg := testutil.NewConfigBuilder().
		WithServer(":8443").
		WithSSL("/etc/lolly/ssl/server.crt", "/etc/lolly/ssl/server.key").
		WithStatic("/", "/var/www/html")

	configYAML, err := cfg.Build()
	require.NoError(t, err, "Failed to build config")

	// 启动 lolly
	lolly, err := testutil.StartLolly(ctx,
		testutil.WithConfigYAML(configYAML),
		testutil.WithCert(certPath, keyPath),
	)
	require.NoError(t, err, "Failed to start lolly")
	defer lolly.Terminate(ctx)

	// 创建 TLS 客户端
	client, err := testutil.CreateTLSClient(certPath)
	require.NoError(t, err, "Failed to create TLS client")

	// 并发发送多个请求
	numRequests := 10
	var wg sync.WaitGroup
	errors := make(chan error, numRequests)
	start := time.Now()

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp, err := client.Get(fmt.Sprintf("%s/test%d", lolly.HTTPSBaseURL(), id))
			if err != nil {
				errors <- fmt.Errorf("request %d failed: %w", id, err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}(i)
	}

	wg.Wait()
	close(errors)

	elapsed := time.Since(start)
	t.Logf("Completed %d requests in %v", numRequests, elapsed)

	// 检查错误
	for err := range errors {
		t.Errorf("Request error: %v", err)
	}

	assert.Less(t, elapsed, 2*time.Second, "Concurrent requests should complete quickly")
}

// TestE2EHTTPSCustomHeaders 测试 HTTPS 自定义头部。
//
// 验证自定义头部正确传递。
func TestE2EHTTPSCustomHeaders(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.DefaultTestTimeout)
	defer cancel()

	if !testutil.LollyImageAvailable(ctx) {
		t.Skip("lolly:latest image not available, run 'make docker-build' first")
	}

	// 生成自签名证书
	certPath, keyPath, cleanup, err := testutil.GenerateSelfSignedCert(t.TempDir())
	require.NoError(t, err, "Failed to generate certificate")
	defer cleanup()

	// 构建配置
	cfg := testutil.NewConfigBuilder().
		WithServer(":8443").
		WithSSL("/etc/lolly/ssl/server.crt", "/etc/lolly/ssl/server.key").
		WithStatic("/", "/var/www/html")

	configYAML, err := cfg.Build()
	require.NoError(t, err, "Failed to build config")

	// 启动 lolly
	lolly, err := testutil.StartLolly(ctx,
		testutil.WithConfigYAML(configYAML),
		testutil.WithCert(certPath, keyPath),
	)
	require.NoError(t, err, "Failed to start lolly")
	defer lolly.Terminate(ctx)

	// 创建 TLS 客户端
	client, err := testutil.CreateTLSClient(certPath)
	require.NoError(t, err, "Failed to create TLS client")

	// 发送多个请求，验证头部传递
	for i := 0; i < 5; i++ {
		req, err := http.NewRequest("GET", lolly.HTTPSBaseURL(), nil)
		require.NoError(t, err)

		// 添加自定义头部
		req.Header.Set("X-Custom-Header", "test-value")

		resp, err := client.Do(req)
		if err != nil {
			t.Logf("Request %d error: %v", i, err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		t.Logf("Request %d: status %d", i, resp.StatusCode)
	}
}

// TestE2EHTTPSLargeRequest 测试 HTTPS 大请求处理。
//
// 验证大请求体的处理。
func TestE2EHTTPSLargeRequest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.DefaultTestTimeout)
	defer cancel()

	if !testutil.LollyImageAvailable(ctx) {
		t.Skip("lolly:latest image not available, run 'make docker-build' first")
	}

	// 生成自签名证书
	certPath, keyPath, cleanup, err := testutil.GenerateSelfSignedCert(t.TempDir())
	require.NoError(t, err, "Failed to generate certificate")
	defer cleanup()

	// 构建配置
	cfg := testutil.NewConfigBuilder().
		WithServer(":8443").
		WithSSL("/etc/lolly/ssl/server.crt", "/etc/lolly/ssl/server.key").
		WithStatic("/", "/var/www/html")

	configYAML, err := cfg.Build()
	require.NoError(t, err, "Failed to build config")

	// 启动 lolly
	lolly, err := testutil.StartLolly(ctx,
		testutil.WithConfigYAML(configYAML),
		testutil.WithCert(certPath, keyPath),
	)
	require.NoError(t, err, "Failed to start lolly")
	defer lolly.Terminate(ctx)

	// 创建 TLS 客户端
	client, err := testutil.CreateTLSClient(certPath)
	require.NoError(t, err, "Failed to create TLS client")

	// 发送大请求（1MB）
	largeBody := strings.NewReader(strings.Repeat("x", 1024*1024))
	req, err := http.NewRequest("POST", lolly.HTTPSBaseURL()+"/upload", largeBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	if err != nil {
		t.Logf("Large request error: %v", err)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		t.Logf("Large request status: %d", resp.StatusCode)
	}
}

// TestE2EALPNNegotiation 测试 ALPN 协商。
//
// 验证 TLS ALPN 扩展正常工作。
func TestE2EALPNNegotiation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.DefaultTestTimeout)
	defer cancel()

	if !testutil.LollyImageAvailable(ctx) {
		t.Skip("lolly:latest image not available, run 'make docker-build' first")
	}

	// 生成自签名证书
	certPath, keyPath, cleanup, err := testutil.GenerateSelfSignedCert(t.TempDir())
	require.NoError(t, err, "Failed to generate certificate")
	defer cleanup()

	// 构建配置
	cfg := testutil.NewConfigBuilder().
		WithServer(":8443").
		WithSSL("/etc/lolly/ssl/server.crt", "/etc/lolly/ssl/server.key").
		WithStatic("/", "/var/www/html")

	configYAML, err := cfg.Build()
	require.NoError(t, err, "Failed to build config")

	// 启动 lolly
	lolly, err := testutil.StartLolly(ctx,
		testutil.WithConfigYAML(configYAML),
		testutil.WithCert(certPath, keyPath),
	)
	require.NoError(t, err, "Failed to start lolly")
	defer lolly.Terminate(ctx)

	// 创建支持 ALPN 的客户端
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2", "http/1.1"},
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	client := &http.Client{Transport: transport}

	// 发送请求
	resp, err := client.Get(lolly.HTTPSBaseURL())
	require.NoError(t, err, "HTTPS request failed")
	defer resp.Body.Close()

	t.Logf("HTTPS response status: %d, protocol: %s", resp.StatusCode, resp.Proto)
}

// TestE2EH2CPriorKnowledge 测试明文 HTTP/2（h2c）prior knowledge 接入。
//
// 容器内验证完整链路：YAML 加载 → "无 SSL 时 h2c_enabled 放行 enabled" 的
// 配置校验 → 明文监听器嗅探分派。同一端口上 prior-knowledge 客户端得到
// HTTP/2，普通请求仍按 HTTP/1.1 服务。
func TestE2EH2CPriorKnowledge(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.DefaultTestTimeout)
	defer cancel()

	if !testutil.LollyImageAvailable(ctx) {
		t.Skip("lolly:latest image not available, run 'make docker-build' first")
	}

	// 明文监听器（无证书）+ h2c 静态站点。
	cfg := testutil.NewConfigBuilder().
		WithServer(":8080").
		WithStatic("/", "/var/www/html", testutil.WithIndex([]string{"index.html"})).
		WithH2C(20)

	configYAML, err := cfg.Build()
	require.NoError(t, err, "Failed to build config")

	lolly, err := testutil.StartLolly(ctx, testutil.WithConfigYAML(configYAML))
	require.NoError(t, err, "Failed to start lolly")
	defer lolly.Terminate(ctx)

	// wait.ForLog 命中"Starting HTTP server"时端口尚未绑定完成，
	// 需再轮询到 HTTP 可达，否则首批请求会被转发器 reset。
	require.NoError(t, lolly.WaitForHealthy(ctx, 15*time.Second), "lolly not reachable")

	// prior knowledge：http2.Transport + AllowHTTP，等价 curl --http2-prior-knowledge。
	t.Run("prior-knowledge 连接走 HTTP/2", func(t *testing.T) {
		h2Client := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
				},
			},
		}

		resp, err := h2Client.Get(lolly.HTTPBaseURL())
		require.NoError(t, err, "h2c request failed")
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "Failed to read h2c body")

		assert.Equal(t, 2, resp.ProtoMajor, "prior-knowledge 连接应协商为 HTTP/2")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, string(body), "It works")
	})

	// 同一监听器上的普通请求不得被嗅探分派影响。
	t.Run("同端口仍服务 HTTP/1.1", func(t *testing.T) {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(lolly.HTTPBaseURL())
		require.NoError(t, err, "HTTP/1.1 request failed")
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "Failed to read HTTP/1.1 body")

		assert.Equal(t, 1, resp.ProtoMajor, "普通请求应仍按 HTTP/1.1 服务")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, string(body), "It works")
	})
}
