//go:build e2e

// proxy_request_streaming_e2e_test.go - 请求体流式反代 E2E 测试（需要 Docker）
//
// 验证 buffering.request_mode: off 在容器化 lolly 中的接线：
//   - 大 POST 请求体经 lolly 流式转发到后端（不崩溃、不 5xx）
//   - client_max_body_size 超限返回 413（流式路径 + bodylimit 联动）
//
// 注意：请求体字节完整性由 internal/proxy 的单元测试覆盖
// （TestServeHTTP_StreamRequestBody 用真实 TCP 后端校验字节一致），
// 此处只验证容器化二进制的配置解析与端到端通路。
//
// 作者：xfy
package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"rua.plus/lolly/internal/e2e/testutil"
)

// TestE2EProxyRequestStreaming_BodyForwarded 验证流式请求体转发通路。
//
// 启用 request_mode: off，向 lolly POST 一个较大的请求体，
// 后端（nginx）应返回非 5xx 响应（默认静态站点 POST 返回 405），
// 证明 lolly 在流式路径下正常完成请求转发，未崩溃或返回 502。
func TestE2EProxyRequestStreaming_BodyForwarded(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if !testutil.LollyImageAvailable(ctx) {
		t.Skip("lolly:latest image not available, run 'make docker-build' first")
	}

	netObj, networkName, pool, err := testutil.SetupProxyTest(ctx, 1, t.Name())
	require.NoError(t, err, "Failed to start backend pool")
	defer testutil.CleanupProxyTest(ctx, netObj, networkName, pool)

	cfg := testutil.NewConfigBuilder().
		WithServer(":8080").
		WithProxy("/", pool.InternalAddresses(),
			testutil.WithProxyRequestBuffering("off"),
		)
	configYAML, err := cfg.Build()
	require.NoError(t, err, "Failed to build config")

	lolly, err := testutil.StartLolly(ctx, testutil.WithConfigYAML(configYAML), testutil.WithNetwork(networkName))
	require.NoError(t, err, "Failed to start lolly")
	defer lolly.Terminate(ctx)

	require.NoError(t, lolly.WaitForHealthy(ctx, testutil.HealthCheckWaitTimeout))

	client := &http.Client{Timeout: 30 * time.Second}

	// ~256KB 请求体，超过适配器 inline 阈值，确保走流式路径
	body := bytes.Repeat([]byte("lolly-e2e-"), 256*102)
	req, err := http.NewRequestWithContext(ctx, "POST", lolly.HTTPBaseURL()+"/", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	require.NoError(t, err, "POST through streaming proxy failed")
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	// nginx 静态站点 POST 返回 405；只要不是 5xx 即说明 lolly 正常完成转发
	assert.Less(t, resp.StatusCode, 500,
		"streaming proxy should not return 5xx, got %d", resp.StatusCode)
}

// TestE2EProxyRequestStreaming_ClientMaxBodySize 验证流式路径下 bodylimit 强制。
//
// 配置 client_max_body_size: 1kb，发送 2KB 请求体，
// 应在 Content-Length 预检阶段返回 413（流式路径与 bodylimit 接线正确）。
func TestE2EProxyRequestStreaming_ClientMaxBodySize(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if !testutil.LollyImageAvailable(ctx) {
		t.Skip("lolly:latest image not available, run 'make docker-build' first")
	}

	netObj, networkName, pool, err := testutil.SetupProxyTest(ctx, 1, t.Name())
	require.NoError(t, err, "Failed to start backend pool")
	defer testutil.CleanupProxyTest(ctx, netObj, networkName, pool)

	// 用自定义 YAML 注入 client_max_body_size（ConfigBuilder 暂无该选项）
	cfg := testutil.NewConfigBuilder().
		WithServer(":8080").
		WithProxy("/", pool.InternalAddresses(),
			testutil.WithProxyRequestBuffering("off"),
		)
	configYAML, err := cfg.Build()
	require.NoError(t, err)
	// 在第一个 proxy 块中追加 client_max_body_size
	configYAML = strings.Replace(configYAML,
		"    load_balance: round_robin\n",
		"    load_balance: round_robin\n    client_max_body_size: \"1kb\"\n", 1)

	lolly, err := testutil.StartLolly(ctx, testutil.WithConfigYAML(configYAML), testutil.WithNetwork(networkName))
	require.NoError(t, err, "Failed to start lolly")
	defer lolly.Terminate(ctx)

	require.NoError(t, lolly.WaitForHealthy(ctx, testutil.HealthCheckWaitTimeout))

	client := &http.Client{Timeout: 30 * time.Second}

	body := bytes.Repeat([]byte("x"), 2048) // 2KB，超过 1KB 限制
	req, err := http.NewRequestWithContext(ctx, "POST", lolly.HTTPBaseURL()+"/", bytes.NewReader(body))
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode,
		"streaming proxy should reject oversized body with 413, got %d", resp.StatusCode)
}
