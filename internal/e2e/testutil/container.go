//go:build e2e

// Package testutil 提供 E2E 测试的工具函数。
//
// 包含容器管理、测试配置、等待工具等。
//
// 作者：xfy
package testutil

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// defaultLollyConfig 是 lolly 容器的默认配置。
const defaultLollyConfig = `
servers:
  - listen: ":8080"
    static:
      - path: "/"
        root: "/var/www/html"
        index:
          - "index.html"
`

// LollyContainerOption 容器启动选项。
type LollyContainerOption func(*lollyContainerConfig)

// lollyContainerConfig 容器配置。
type lollyContainerConfig struct {
	configPath   string
	configYAML   string
	network      string
	certPath     string
	keyPath      string
	extraMounts  []testcontainers.ContainerMount
	env          map[string]string
	exposedPorts []string
	waitFor      wait.Strategy
}

// WithConfigFile 使用配置文件路径。
func WithConfigFile(path string) LollyContainerOption {
	return func(c *lollyContainerConfig) {
		c.configPath = path
	}
}

// WithConfigYAML 使用 YAML 字符串配置。
func WithConfigYAML(yaml string) LollyContainerOption {
	return func(c *lollyContainerConfig) {
		c.configYAML = yaml
	}
}

// WithNetwork 加入指定网络。
func WithNetwork(name string) LollyContainerOption {
	return func(c *lollyContainerConfig) {
		c.network = name
	}
}

// WithCert 挂载证书文件。
func WithCert(certPath, keyPath string) LollyContainerOption {
	return func(c *lollyContainerConfig) {
		c.certPath = certPath
		c.keyPath = keyPath
	}
}

// WithExtraMount 添加额外挂载。
func WithExtraMount(hostPath, containerPath string) LollyContainerOption {
	return func(c *lollyContainerConfig) {
		c.extraMounts = append(c.extraMounts, testcontainers.ContainerMount{
			Source: testcontainers.GenericBindMountSource{
				HostPath: hostPath,
			},
			Target: testcontainers.ContainerMountTarget(containerPath),
		})
	}
}

// WithEnv 设置环境变量。
func WithEnv(env map[string]string) LollyContainerOption {
	return func(c *lollyContainerConfig) {
		if c.env == nil {
			c.env = make(map[string]string)
		}
		for k, v := range env {
			c.env[k] = v
		}
	}
}

// WithExposedPorts 设置暴露端口。
func WithExposedPorts(ports ...string) LollyContainerOption {
	return func(c *lollyContainerConfig) {
		c.exposedPorts = ports
	}
}

// WithWaitStrategy 设置等待策略。
func WithWaitStrategy(strategy wait.Strategy) LollyContainerOption {
	return func(c *lollyContainerConfig) {
		c.waitFor = strategy
	}
}

// LollyContainer 封装 lolly 服务器容器。
type LollyContainer struct {
	Container testcontainers.Container
	Host      string
	HTTPPort  int
	HTTPSPort int
}

// StartLollyContainer 启动 lolly 服务器容器。
//
// 使用预构建的 lolly 镜像。如果 configPath 为空，使用默认配置。
// 支持通过选项函数自定义配置。
func StartLollyContainer(ctx context.Context, configPath string) (*LollyContainer, error) {
	return StartLolly(ctx, WithConfigFile(configPath))
}

// StartLolly 启动 lolly 容器（增强版）。
//
// 支持多种配置方式和自定义选项。
//
// 使用示例：
//
//	// 使用默认配置
//	lolly, err := StartLolly(ctx)
//
//	// 使用配置文件
//	lolly, err := StartLolly(ctx, WithConfigFile("/path/to/config.yaml"))
//
//	// 使用动态配置
//	cfg := NewConfigBuilder().WithProxy("/api/", targets).Build()
//	lolly, err := StartLolly(ctx, WithConfigYAML(cfg))
//
//	// 使用 SSL
//	lolly, err := StartLolly(ctx, WithConfigBuilder(cfg), WithCert(certPath, keyPath))
func StartLolly(ctx context.Context, opts ...LollyContainerOption) (*LollyContainer, error) {
	cfg := &lollyContainerConfig{
		exposedPorts: []string{"8080/tcp", "8443/tcp"},
		waitFor:      wait.ForLog("Starting HTTP server").WithStartupTimeout(30 * time.Second),
	}

	for _, opt := range opts {
		opt(cfg)
	}

	req := testcontainers.ContainerRequest{
		Image:        "lolly:latest",
		ExposedPorts: cfg.exposedPorts,
		WaitingFor:   cfg.waitFor,
	}

	// 设置环境变量
	if len(cfg.env) > 0 {
		req.Env = cfg.env
	}

	// 配置网络
	if cfg.network != "" {
		req.Networks = []string{cfg.network}
	}

	// 处理配置文件
	if cfg.configPath != "" {
		req.Mounts = append(req.Mounts, testcontainers.ContainerMount{
			Source: testcontainers.GenericBindMountSource{
				HostPath: cfg.configPath,
			},
			Target: "/etc/lolly/lolly.yaml",
		})
	} else if cfg.configYAML != "" {
		req.Files = append(req.Files, testcontainers.ContainerFile{
			Reader:            strings.NewReader(cfg.configYAML),
			ContainerFilePath: "/etc/lolly/lolly.yaml",
			FileMode:          0o644,
		})
	} else {
		// 使用内嵌默认配置
		req.Files = append(req.Files, testcontainers.ContainerFile{
			Reader:            strings.NewReader(defaultLollyConfig),
			ContainerFilePath: "/etc/lolly/lolly.yaml",
			FileMode:          0o644,
		})
	}

	// 挂载证书
	if cfg.certPath != "" && cfg.keyPath != "" {
		req.Mounts = append(req.Mounts,
			testcontainers.ContainerMount{
				Source: testcontainers.GenericBindMountSource{
					HostPath: cfg.certPath,
				},
				Target: "/etc/lolly/ssl/server.crt",
			},
			testcontainers.ContainerMount{
				Source: testcontainers.GenericBindMountSource{
					HostPath: cfg.keyPath,
				},
				Target: "/etc/lolly/ssl/server.key",
			},
		)
	}

	// 添加额外挂载
	req.Mounts = append(req.Mounts, cfg.extraMounts...)

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		return nil, fmt.Errorf("failed to get host: %w", err)
	}

	httpPort, err := container.MappedPort(ctx, "8080/tcp")
	if err != nil {
		container.Terminate(ctx)
		return nil, fmt.Errorf("failed to get HTTP port: %w", err)
	}

	httpsPort, err := container.MappedPort(ctx, "8443/tcp")
	if err != nil {
		container.Terminate(ctx)
		return nil, fmt.Errorf("failed to get HTTPS port: %w", err)
	}

	// 解析端口数字
	httpPortInt := parsePort(httpPort.Port())
	httpsPortInt := parsePort(httpsPort.Port())

	return &LollyContainer{
		Container: container,
		Host:      host,
		HTTPPort:  httpPortInt,
		HTTPSPort: httpsPortInt,
	}, nil
}

// parsePort 解析端口字符串为整数。
func parsePort(portStr string) int {
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return port
}

// HTTPBaseURL 返回 HTTP 基础 URL。
func (c *LollyContainer) HTTPBaseURL() string {
	return fmt.Sprintf("http://%s:%d", c.Host, c.HTTPPort)
}

// HTTPSBaseURL 返回 HTTPS 基础 URL。
func (c *LollyContainer) HTTPSBaseURL() string {
	return fmt.Sprintf("https://%s:%d", c.Host, c.HTTPSPort)
}

// Terminate 终止容器。
func (c *LollyContainer) Terminate(ctx context.Context) error {
	if c.Container != nil {
		return c.Container.Terminate(ctx)
	}
	return nil
}

// WaitForHealthy 等待服务健康。
func (c *LollyContainer) WaitForHealthy(ctx context.Context, timeout time.Duration) error {
	client := &http.Client{Timeout: 5 * time.Second}
	url := c.HTTPBaseURL()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				// 任何非 5xx 响应都表示服务器正在运行
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("service not healthy after %v", timeout)
}

// Logs 获取容器日志。
//
// 用于诊断测试失败原因。
func (c *LollyContainer) Logs(ctx context.Context) (string, error) {
	if c.Container == nil {
		return "", fmt.Errorf("container is nil")
	}

	reader, err := c.Container.Logs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get container logs: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("failed to read container logs: %w", err)
	}

	return string(data), nil
}

// MockBackendContainer 启动一个模拟后端服务器容器。
func MockBackendContainer(ctx context.Context, port int) (testcontainers.Container, string, error) {
	req := testcontainers.ContainerRequest{
		Image:        "nginx:alpine",
		ExposedPorts: []string{fmt.Sprintf("%d/tcp", port)},
		WaitingFor:   wait.ForHTTP("/").WithPort(fmt.Sprintf("%d/tcp", port)).WithStartupTimeout(30 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to start mock backend: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		return nil, "", fmt.Errorf("failed to get host: %w", err)
	}

	mappedPort, err := container.MappedPort(ctx, fmt.Sprintf("%d/tcp", port))
	if err != nil {
		container.Terminate(ctx)
		return nil, "", fmt.Errorf("failed to get port: %w", err)
	}

	addr := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())
	return container, addr, nil
}

// DockerAvailable 检查 Docker 是否可用。
func DockerAvailable(ctx context.Context) bool {
	req := testcontainers.ContainerRequest{
		Image:      "alpine:latest",
		Cmd:        []string{"/bin/true"},
		AutoRemove: true,
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return false
	}
	container.Terminate(ctx)
	return true
}

// LollyImageAvailable 检查 lolly:latest 镜像是否可用。
// 结果被 sync.Once 缓存，避免重复检查。
func LollyImageAvailable(ctx context.Context) bool {
	lollyImageCheckOnce.Do(func() {
		lollyImageAvailable = checkLollyImage(ctx)
	})
	return lollyImageAvailable
}

var (
	lollyImageCheckOnce sync.Once
	lollyImageAvailable bool
)

// checkLollyImage 实际执行镜像检查。
func checkLollyImage(ctx context.Context) bool {
	req := testcontainers.ContainerRequest{
		Image:      "lolly:latest",
		Cmd:        []string{"/lolly", "-v"},
		AutoRemove: true,
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return false
	}
	container.Terminate(ctx)
	return true
}

// StartMockBackend 启动模拟后端容器（用于代理测试）。
//
// 使用 nginx 作为模拟后端，返回容器和访问地址。
// 注意：此函数仅用于代理测试的后端模拟，不应作为被测系统。
//
// 返回值：
//   - container: 容器实例
//   - hostPort: 宿主机访问地址（用于测试代码访问）
//   - internalAddr: 容器内部访问地址（用于 lolly 配置）
func StartMockBackend(ctx context.Context) (testcontainers.Container, string, error) {
	req := testcontainers.ContainerRequest{
		Image:        "nginx:alpine",
		ExposedPorts: []string{"80/tcp"},
		WaitingFor:   wait.ForHTTP("/").WithStartupTimeout(30 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to start mock backend: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		return nil, "", fmt.Errorf("failed to get host: %w", err)
	}

	port, err := container.MappedPort(ctx, "80/tcp")
	if err != nil {
		container.Terminate(ctx)
		return nil, "", fmt.Errorf("failed to get port: %w", err)
	}

	// 返回宿主机地址
	addr := fmt.Sprintf("http://%s:%s", host, port.Port())
	return container, addr, nil
}

// BackendPool 后端池管理。
//
// 管理多个后端容器，用于负载均衡测试。
// 支持网络模式：当 network 不为空时，容器加入指定网络，
// 并提供内部地址供 lolly 容器访问。
type BackendPool struct {
	containers []testcontainers.Container
	addresses  []string // 宿主机访问地址
	internal   []string // 容器网络内部地址
	network    string   // Docker 网络名称
}

// StartBackendPool 启动多个后端容器。
//
// 参数：
//   - count: 后端数量
//
// 返回后端池和地址列表（宿主机访问地址）。
func StartBackendPool(ctx context.Context, count int) (*BackendPool, error) {
	return StartBackendPoolWithNetwork(ctx, count, "")
}

// StartBackendPoolWithNetwork 启动多个后端容器并加入网络。
//
// 参数：
//   - count: 后端数量
//   - network: Docker 网络名称（可选，为空则不加入网络）
//
// 当 network 不为空时，容器会加入该网络，并提供内部地址。
func StartBackendPoolWithNetwork(ctx context.Context, count int, network string) (*BackendPool, error) {
	pool := &BackendPool{
		containers: make([]testcontainers.Container, count),
		addresses:  make([]string, count),
		internal:   make([]string, count),
		network:    network,
	}

	for i := 0; i < count; i++ {
		container, addr, internalAddr, err := startMockBackendWithNetwork(ctx, network, i)
		if err != nil {
			// 清理已启动的容器
			pool.Terminate(ctx)
			return nil, fmt.Errorf("failed to start backend %d: %w", i, err)
		}
		pool.containers[i] = container
		pool.addresses[i] = addr
		pool.internal[i] = internalAddr
	}

	return pool, nil
}

// startMockBackendWithNetwork 启动单个后端容器。
func startMockBackendWithNetwork(ctx context.Context, network string, index int) (testcontainers.Container, string, string, error) {
	// 生成容器名称（用于网络通信），使用原子计数器避免并行竞态
	id := atomic.AddInt64(&backendCounter, 1)
	containerName := fmt.Sprintf("backend-%d-%d", id, index)

	req := testcontainers.ContainerRequest{
		Image:        "nginx:alpine",
		ExposedPorts: []string{"80/tcp"},
		WaitingFor:   wait.ForHTTP("/").WithStartupTimeout(30 * time.Second),
		Name:         containerName,
	}

	if network != "" {
		req.Networks = []string{network}
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to start mock backend: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		return nil, "", "", fmt.Errorf("failed to get host: %w", err)
	}

	port, err := container.MappedPort(ctx, "80/tcp")
	if err != nil {
		container.Terminate(ctx)
		return nil, "", "", fmt.Errorf("failed to get port: %w", err)
	}

	// 宿主机访问地址
	hostAddr := fmt.Sprintf("http://%s:%s", host, port.Port())

	// 容器网络内部地址（使用容器名称）
	internalAddr := fmt.Sprintf("http://%s:80", containerName)

	return container, hostAddr, internalAddr, nil
}

// Addresses 返回后端地址列表（宿主机访问地址）。
func (p *BackendPool) Addresses() []string {
	return p.addresses
}

// InternalAddresses 返回容器网络内部地址列表。
//
// 当 lolly 和后端在同一 Docker 网络时，应使用此地址。
func (p *BackendPool) InternalAddresses() []string {
	return p.internal
}

// Containers 返回容器列表。
func (p *BackendPool) Containers() []testcontainers.Container {
	return p.containers
}

// Count 返回后端数量。
func (p *BackendPool) Count() int {
	return len(p.containers)
}

// Terminate 终止所有容器。
func (p *BackendPool) Terminate(ctx context.Context) {
	for _, container := range p.containers {
		if container != nil {
			container.Terminate(ctx)
		}
	}
}

// TerminateOne 终止指定索引的容器。
func (p *BackendPool) TerminateOne(ctx context.Context, index int) error {
	if index < 0 || index >= len(p.containers) {
		return fmt.Errorf("invalid index %d", index)
	}
	if p.containers[index] != nil {
		err := p.containers[index].Terminate(ctx)
		p.containers[index] = nil
		p.addresses[index] = ""
		p.internal[index] = ""
		return err
	}
	return nil
}

// RestartOne 重启指定索引的容器。
func (p *BackendPool) RestartOne(ctx context.Context, index int) error {
	if index < 0 || index >= len(p.containers) {
		return fmt.Errorf("invalid index %d", index)
	}

	// 先终止旧容器
	if p.containers[index] != nil {
		p.containers[index].Terminate(ctx)
	}

	// 启动新容器
	container, addr, internalAddr, err := startMockBackendWithNetwork(ctx, p.network, index)
	if err != nil {
		return err
	}

	p.containers[index] = container
	p.addresses[index] = addr
	p.internal[index] = internalAddr
	return nil
}

// CreateNetwork 创建 Docker 网络。
//
// 用于容器间通信。
func CreateNetwork(ctx context.Context, name string) (testcontainers.Network, error) {
	network, err := testcontainers.GenericNetwork(ctx, testcontainers.GenericNetworkRequest{
		NetworkRequest: testcontainers.NetworkRequest{
			Name: name,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create network: %w", err)
	}
	return network, nil
}

// 原子计数器，用于生成唯一的容器名和网络名。
var (
	backendCounter int64
	networkCounter int64
)

// SetupProxyTest 设置代理测试环境。
//
// 创建独立网络、启动后端池，返回网络对象、网络名称和后端池。
// suffix 用于生成唯一网络名（通常传 t.Name()），配合原子计数器确保 -count N 安全。
// lolly 容器应使用 InternalAddresses() 作为代理目标。
//
// 使用示例：
//
//	netObj, networkName, pool, err := testutil.SetupProxyTest(ctx, 2, t.Name())
//	if err != nil {
//	    t.Fatal(err)
//	}
//	defer testutil.CleanupProxyTest(ctx, netObj, networkName, pool)
//
//	lolly, err := testutil.StartLolly(ctx,
//	    testutil.WithConfigYAML(configYAML),
//	    testutil.WithNetwork(networkName),
//	)
func SetupProxyTest(ctx context.Context, backendCount int, suffix string) (testcontainers.Network, string, *BackendPool, error) {
	id := atomic.AddInt64(&networkCounter, 1)
	// 防御性处理：子测试的 t.Name() 含 '/'，Docker 网络名不支持
	suffix = strings.ReplaceAll(suffix, "/", "-")
	networkName := fmt.Sprintf("lolly-e2e-%s-%d", suffix, id)

	network, err := CreateNetwork(ctx, networkName)
	if err != nil && !isNetworkExistsError(err) {
		return nil, "", nil, fmt.Errorf("failed to create network: %w", err)
	}

	// 启动后端池并加入网络
	pool, err := StartBackendPoolWithNetwork(ctx, backendCount, networkName)
	if err != nil {
		if network != nil {
			network.Remove(ctx)
		}
		return nil, "", nil, fmt.Errorf("failed to start backend pool: %w", err)
	}

	return network, networkName, pool, nil
}

// isNetworkExistsError 检查是否是网络已存在错误。
func isNetworkExistsError(err error) bool {
	return err != nil && (containsString(err.Error(), "already exists") ||
		containsString(err.Error(), "network with name") ||
		containsString(err.Error(), "failed to create network"))
}

// containsString 检查字符串是否包含子串。
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// CleanupProxyTest 清理代理测试环境。
func CleanupProxyTest(ctx context.Context, network testcontainers.Network, networkName string, pool *BackendPool) {
	if pool != nil {
		pool.Terminate(ctx)
	}
	if network != nil {
		network.Remove(ctx)
	}
}
