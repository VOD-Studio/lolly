//go:build integration

// container_discovery_integration_test.go - 容器发现进程内集成测试
//
// 使用最小 Docker-compatible Unix socket API、真实 Lolly 服务与真实 HTTP 后端，
// 验证动态容器路由的公开网络行为，不依赖 Docker 守护进程。
//
// 作者：xfy
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/server"
)

// TestContainerDiscoveryDynamicRoutes 验证容器事件会原地更新路由，且同步失败保留旧路由。
func TestContainerDiscoveryDynamicRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("backend:" + request.URL.Path))
	}))
	defer backend.Close()
	backendHost, backendPortText, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	backendPort, err := strconv.Atoi(backendPortText)
	if err != nil {
		t.Fatal(err)
	}

	api := newContainerIntegrationAPI(t)
	defer api.close()
	api.put("initial", containerIntegrationInspect("initial.example", backendHost, backendPort))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Servers = []config.ServerConfig{{Name: "containers", Listen: listener.Addr().String()}}
	cfg.Monitoring.Healthz.Enabled = false
	cfg.Monitoring.Readyz.Enabled = false
	cfg.ContainerDiscovery = config.ContainerDiscoveryConfig{
		Enabled:        true,
		Required:       true,
		Endpoint:       "unix://" + api.socket,
		Network:        "frontend",
		HTTPServer:     "containers",
		ResyncInterval: time.Hour,
		Debounce:       10 * time.Millisecond,
		RequestTimeout: time.Second,
	}

	lolly := server.New(cfg)
	lolly.SetListeners([]net.Listener{listener})
	startResult := make(chan error, 1)
	go func() { startResult <- lolly.Start() }()
	defer func() {
		if err := lolly.StopWithTimeout(2 * time.Second); err != nil {
			t.Errorf("停止 Lolly 失败: %v", err)
		}
		select {
		case err := <-startResult:
			if err != nil && !strings.Contains(err.Error(), "closed") {
				t.Errorf("Lolly 服务异常退出: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Lolly 服务未退出")
		}
	}()

	var dialCount atomic.Int32
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCount.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	defer transport.CloseIdleConnections()
	baseURL := "http://" + listener.Addr().String()

	awaitContainerResponse(t, client, baseURL, "initial.example", http.StatusOK, "backend:/")
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("初始请求建立了 %d 条 Lolly 连接，期望 1 条", got)
	}

	api.put("dynamic", containerIntegrationInspect("dynamic.example", backendHost, backendPort))
	api.emit()
	awaitContainerResponse(t, client, baseURL, "dynamic.example", http.StatusOK, "backend:/")
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("动态新增路由后 Lolly listener 连接已更换，连接数为 %d", got)
	}

	api.remove("dynamic")
	api.emit()
	awaitContainerResponse(t, client, baseURL, "dynamic.example", http.StatusNotFound, "Not Found")
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("动态删除路由后 Lolly listener 连接已更换，连接数为 %d", got)
	}

	beforeFailure := api.listFailureCount()
	api.failLists(true)
	api.emit()
	awaitCondition(t, func() bool { return api.listFailureCount() > beforeFailure }, "容器列表失败请求未完成")
	awaitContainerResponse(t, client, baseURL, "initial.example", http.StatusOK, "backend:/")
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("容器 API 暂时失败后 Lolly listener 连接已更换，连接数为 %d", got)
	}
}

type containerIntegrationAPI struct {
	server       *http.Server
	listener     net.Listener
	socket       string
	events       chan struct{}
	mu           sync.RWMutex
	items        map[string]containerIntegrationInspectResponse
	failList     bool
	listFailures atomic.Int64
}

type containerIntegrationInspectResponse struct {
	Config struct {
		Env          []string            `json:"Env"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// newContainerIntegrationAPI 在短 Unix socket 路径上启动最小兼容 API。
func newContainerIntegrationAPI(t *testing.T) *containerIntegrationAPI {
	t.Helper()
	file, err := os.CreateTemp("", "ly-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socket := file.Name()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	api := &containerIntegrationAPI{
		listener: listener,
		socket:   socket,
		events:   make(chan struct{}, 1),
		items:    make(map[string]containerIntegrationInspectResponse),
	}
	api.server = &http.Server{Handler: http.HandlerFunc(api.serveHTTP)}
	go func() { _ = api.server.Serve(listener) }()
	return api
}

// close 关闭兼容 API 并删除 Unix socket。
func (a *containerIntegrationAPI) close() {
	_ = a.server.Close()
	_ = a.listener.Close()
	_ = os.Remove(a.socket)
}

// put 新增或替换一个运行中容器。
func (a *containerIntegrationAPI) put(id string, inspect containerIntegrationInspectResponse) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items[id] = inspect
}

// remove 删除一个运行中容器。
func (a *containerIntegrationAPI) remove(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.items, id)
}

// failLists 控制容器列表端点是否返回临时错误。
func (a *containerIntegrationAPI) failLists(fail bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failList = fail
}

// listFailureCount 返回容器列表端点已完成的失败次数。
func (a *containerIntegrationAPI) listFailureCount() int64 {
	return a.listFailures.Load()
}

// emit 向事件长连接发送一次容器变更通知。
func (a *containerIntegrationAPI) emit() {
	a.events <- struct{}{}
}

// serveHTTP 提供列表、详情和事件三个兼容端点。
func (a *containerIntegrationAPI) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/events" {
		for {
			select {
			case <-request.Context().Done():
				return
			case <-a.events:
				_, _ = writer.Write([]byte("{}\n"))
				if flusher, ok := writer.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}

	a.mu.RLock()
	failList := a.failList
	items := make(map[string]containerIntegrationInspectResponse, len(a.items))
	for id, inspect := range a.items {
		items[id] = inspect
	}
	a.mu.RUnlock()
	if request.URL.Path == "/containers/json" {
		if failList {
			http.Error(writer, "temporary failure", http.StatusInternalServerError)
			a.listFailures.Add(1)
			return
		}
		result := make([]map[string]string, 0, len(items))
		for id := range items {
			result = append(result, map[string]string{"Id": id})
		}
		_ = json.NewEncoder(writer).Encode(result)
		return
	}
	for id, inspect := range items {
		if request.URL.Path == "/containers/"+id+"/json" {
			_ = json.NewEncoder(writer).Encode(inspect)
			return
		}
	}
	http.NotFound(writer, request)
}

// containerIntegrationInspect 构造指向真实测试后端的容器详情。
func containerIntegrationInspect(host, address string, port int) containerIntegrationInspectResponse {
	var inspect containerIntegrationInspectResponse
	inspect.Config.Env = []string{"VIRTUAL_HOST=" + host, "VIRTUAL_PORT=" + strconv.Itoa(port)}
	inspect.Config.ExposedPorts = map[string]struct{}{fmt.Sprintf("%d/tcp", port): {}}
	inspect.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{"frontend": {IPAddress: address}}
	return inspect
}

// awaitContainerResponse 等待指定 Host 获得期望的公开 HTTP 响应。
func awaitContainerResponse(t *testing.T, client *http.Client, url, host string, status int, body string) {
	t.Helper()
	var last string
	awaitCondition(t, func() bool {
		request, err := http.NewRequest(http.MethodGet, url+"/", nil)
		if err != nil {
			last = err.Error()
			return false
		}
		request.Host = host
		response, err := client.Do(request)
		if err != nil {
			last = err.Error()
			return false
		}
		responseBody, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			last = readErr.Error()
			return false
		}
		last = fmt.Sprintf("status=%d body=%q", response.StatusCode, string(responseBody))
		return response.StatusCode == status && string(responseBody) == body
	}, fmt.Sprintf("Host %s 未得到期望响应，最后结果：%s", host, last))
}

// awaitCondition 在短期限内轮询异步公开行为。
func awaitCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(message)
}
