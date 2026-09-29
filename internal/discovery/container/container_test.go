// Package container 验证容器发现对外行为。
//
// 该文件通过兼容 API 边界验证标签解析、网络选择、路由聚合与快照容错。
//
// 作者：xfy
package container

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWatcherSyncsAndAggregatesRoutes 验证运行中容器会按主机与路径聚合。
func TestWatcherSyncsAndAggregatesRoutes(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"one": inspectWith([]string{"VIRTUAL_HOST=example.com", "VIRTUAL_PORT=8080", "VIRTUAL_PATH=/api", "VIRTUAL_DEST=/", "VIRTUAL_PROTO=https", "EXTERNAL_HTTP_PORT=8081", "EXTERNAL_HTTPS_PORT=8443", "HTTPS_METHOD=redirect", "LETSENCRYPT_HOST=example.com", "LETSENCRYPT_EMAIL=ops@example.com"}, "chosen", "10.0.0.2", "2001:db8::2", "8080/tcp"),
		"two": inspectWith([]string{"VIRTUAL_HOST=example.com", "VIRTUAL_PORT=8080", "VIRTUAL_PATH=/api", "VIRTUAL_DEST=/", "VIRTUAL_PROTO=https", "EXTERNAL_HTTP_PORT=8081", "EXTERNAL_HTTPS_PORT=8443", "HTTPS_METHOD=redirect", "LETSENCRYPT_HOST=example.com", "LETSENCRYPT_EMAIL=ops@example.com"}, "chosen", "10.0.0.3", "", "8080/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "chosen", ResyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 1 {
		t.Fatalf("期望一条聚合路由，实际为 %#v", snapshot.Routes)
	}
	route := snapshot.Routes[0]
	if route.Host != "example.com" || route.Path != "/api" || route.Protocol != "https" || route.Destination != "/" {
		t.Fatalf("路由标签解析错误：%#v", route)
	}
	if len(route.Endpoints) != 2 || route.Endpoints[0].Address != "10.0.0.2" || route.Endpoints[1].Address != "10.0.0.3" {
		t.Fatalf("端点聚合或 IPv4 选择错误：%#v", route.Endpoints)
	}
	if route.ExternalHTTPPort != 8081 || route.ExternalHTTPSPort != 8443 || route.HTTPSMethod != "redirect" || route.ACMEEmail != "ops@example.com" {
		t.Fatalf("扩展标签解析错误：%#v", route)
	}
}

// TestWatcherUsesExposedPortAndReportsUnsupportedProtocol 验证默认端口规则与无效协议隔离。
func TestWatcherUsesExposedPortAndReportsUnsupportedProtocol(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"single": inspectWith([]string{"VIRTUAL_HOST=single.example"}, "bridge", "", "2001:db8::4", "9000/tcp"),
		"many":   inspectWith([]string{"VIRTUAL_HOST=many.example"}, "bridge", "10.0.0.5", "", "8000/tcp", "9000/tcp"),
		"bad":    inspectWith([]string{"VIRTUAL_HOST=bad.example", "VIRTUAL_PROTO=uwsgi"}, "bridge", "10.0.0.6", "", "80/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge", ResyncInterval: 0})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 2 || snapshot.Routes[0].Endpoints[0].Port != 80 || snapshot.Routes[1].Endpoints[0].Port != 9000 {
		t.Fatalf("默认端口规则错误：%#v", snapshot.Routes)
	}
	if snapshot.Routes[1].Endpoints[0].Address != "2001:db8::4" {
		t.Fatalf("IPv6 回退错误：%#v", snapshot.Routes[1].Endpoints)
	}
	if len(snapshot.Issues) != 1 {
		t.Fatalf("应报告并跳过不支持协议：%#v", snapshot)
	}
}

// TestWatcherParsesMultiports 验证 YAML 与 JSON 多端口标签会展开成路由。
func TestWatcherParsesMultiports(t *testing.T) {
	t.Parallel()

	yamlValue := "one.example:\n  /api:\n    port: 8080\n    proto: https\n    dest: /v1\n"
	jsonValue := `{"two.example":{"/":{"port":9090,"proto":"http"}}}`
	api := newTestAPI(t, map[string]inspectResponse{
		"yaml": inspectWith([]string{"VIRTUAL_HOST_MULTIPORTS=" + yamlValue}, "bridge", "10.0.0.7", "", "8080/tcp"),
		"json": inspectWith([]string{"VIRTUAL_HOST_MULTIPORTS=" + jsonValue}, "bridge", "10.0.0.8", "", "9090/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge", ResyncInterval: 0})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 2 || snapshot.Routes[0].Host != "one.example" || snapshot.Routes[0].Destination != "/v1" || snapshot.Routes[1].Host != "two.example" {
		t.Fatalf("多端口标签解析错误：%#v", snapshot.Routes)
	}
}

// TestWatcherHonorsNginxProxyDefaults 验证传统与多端口声明共享官方默认推导规则。
func TestWatcherHonorsNginxProxyDefaults(t *testing.T) {
	t.Parallel()

	multiports := "Empty.Example.: {}\npaths.example:\n  /api: {}\n  external_http_port: 8081\n  external_https_port: 8443\n"
	api := newTestAPI(t, map[string]inspectResponse{
		"multi": inspectWith([]string{"VIRTUAL_HOST_MULTIPORTS=" + multiports, "VIRTUAL_PORT=9000"}, "bridge", "10.0.0.2", "", "8000/tcp"),
		"plain": inspectWith([]string{"VIRTUAL_HOST=PLAIN.Example.", "VIRTUAL_PATH=/app"}, "bridge", "10.0.0.3", "", "7000/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge"})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 3 {
		t.Fatalf("默认声明应生成三条路由：%#v", snapshot)
	}
	for _, route := range snapshot.Routes {
		if route.Protocol != "http" || route.Destination != "" || route.Endpoints[0].Port == 0 {
			t.Fatalf("默认协议、目标或端口错误：%#v", route)
		}
	}
	if snapshot.Routes[0].Host != "empty.example" || snapshot.Routes[0].Endpoints[0].Port != 9000 {
		t.Fatalf("空 host 配置未使用传统端口推导：%#v", snapshot.Routes[0])
	}
	var pathsRoute Route
	for _, route := range snapshot.Routes {
		if route.Host == "paths.example" {
			pathsRoute = route
		}
	}
	if pathsRoute.ExternalHTTPPort != 8081 || pathsRoute.ExternalHTTPSPort != 8443 {
		t.Fatalf("host 外部端口未应用到路径：%#v", pathsRoute)
	}
}

// TestWatcherReportsInvalidDeclarationsAndConflicts 验证不安全声明与冲突不会被静默接受。
func TestWatcherReportsInvalidDeclarationsAndConflicts(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"a":              inspectWith([]string{"VIRTUAL_HOST=Example.com", "VIRTUAL_PORT=8000", "HTTPS_METHOD=noredirect", "LETSENCRYPT_HOST=one.example", "ACME_HOST=two.example"}, "bridge", "10.0.0.2", "", "8000/tcp"),
		"b":              inspectWith([]string{"VIRTUAL_HOST=example.com", "VIRTUAL_PORT=9000", "VIRTUAL_PROTO=https"}, "bridge", "10.0.0.3", "", "9000/tcp"),
		"bad-host":       inspectWith([]string{"VIRTUAL_HOST=https://bad.example/path"}, "bridge", "10.0.0.4", "", "80/tcp"),
		"bad-path":       inspectWith([]string{"VIRTUAL_HOST=path.example", "VIRTUAL_PATH=relative"}, "bridge", "10.0.0.5", "", "80/tcp"),
		"bad-port":       inspectWith([]string{"VIRTUAL_HOST=port.example", "EXTERNAL_HTTP_PORT=nope"}, "bridge", "10.0.0.6", "", "80/tcp"),
		"bad-method":     inspectWith([]string{"VIRTUAL_HOST=method.example", "HTTPS_METHOD=sometimes"}, "bridge", "10.0.0.7", "", "80/tcp"),
		"bad-multi-port": inspectWith([]string{"VIRTUAL_HOST_MULTIPORTS=multi-port.example:\n  external_http_port: 0\n"}, "bridge", "10.0.0.8", "", "80/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge"})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 1 || len(snapshot.Routes[0].Endpoints) != 1 || snapshot.Routes[0].Endpoints[0].ContainerID != "a" {
		t.Fatalf("冲突应确定地保留首个容器声明：%#v", snapshot.Routes)
	}
	if len(snapshot.Routes[0].ACMEHosts) != 2 {
		t.Fatalf("两个 ACME 主机变量应合并：%#v", snapshot.Routes[0].ACMEHosts)
	}
	if len(snapshot.Issues) != 6 {
		t.Fatalf("应分别报告主机、路径、HTTPS 方法、外部端口和冲突：%#v", snapshot.Issues)
	}
}

// TestWatcherDebouncesEventBurst 验证连续事件只在安静窗口后触发一次同步。
func TestWatcherDebouncesEventBurst(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"one": inspectWith([]string{"VIRTUAL_HOST=example.com"}, "bridge", "10.0.0.2", "", "80/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge", ResyncInterval: time.Hour, Debounce: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published := make(chan Snapshot, 4)
	go func() { _ = watcher.Start(ctx, func(snapshot Snapshot) { published <- snapshot }) }()
	<-published
	api.emitEvent()
	time.Sleep(20 * time.Millisecond)
	api.emitEvent()
	select {
	case <-published:
		t.Fatal("防抖窗口结束前不应发布")
	case <-time.After(70 * time.Millisecond):
	}
	select {
	case <-published:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("防抖窗口结束后未发布")
	}
}

// TestWatcherPublishesAfterEvent 验证事件会触发全量同步并发布新快照。
func TestWatcherPublishesAfterEvent(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"one": inspectWith([]string{"VIRTUAL_HOST=example.com"}, "bridge", "10.0.0.2", "", "80/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge", ResyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	published := make(chan Snapshot, 2)
	stopped := make(chan error, 1)
	go func() { stopped <- watcher.Start(ctx, func(snapshot Snapshot) { published <- snapshot }) }()

	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("未发布初始快照")
	}
	api.emitEvent()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("事件未触发全量同步")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("取消后观察器未停止")
	}
}

// TestWatcherContinuesAfterInspectFailure 验证单个详情请求失败只产生问题报告。
func TestWatcherContinuesAfterInspectFailure(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"good": inspectWith([]string{"VIRTUAL_HOST=good.example"}, "bridge", "10.0.0.2", "", "80/tcp"),
		"bad":  inspectWith([]string{"VIRTUAL_HOST=bad.example"}, "bridge", "10.0.0.3", "", "80/tcp"),
	})
	api.failInspect = "bad"
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge"})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 1 || snapshot.Routes[0].Host != "good.example" || len(snapshot.Issues) != 1 || snapshot.Issues[0].ContainerID != "bad" {
		t.Fatalf("详情失败未被隔离：%#v", snapshot)
	}
}

// TestWatcherRetainsFailedInspectContainer 验证详情暂时失败时保留旧端点，恢复或消失后再更新。
func TestWatcherRetainsFailedInspectContainer(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"old":   inspectWith([]string{"VIRTUAL_HOST=old.example"}, "bridge", "10.0.0.2", "", "80/tcp"),
		"other": inspectWith([]string{"VIRTUAL_HOST=other.example"}, "bridge", "10.0.0.3", "", "80/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = watcher.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	api.setInspect("other", inspectWith([]string{"VIRTUAL_HOST=other.example"}, "bridge", "10.0.0.30", "", "80/tcp"))
	api.setFailInspect("old")
	snapshot, err := watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if endpointAddress(snapshot, "old") != "10.0.0.2" || endpointAddress(snapshot, "other") != "10.0.0.30" {
		t.Fatalf("失败容器应保留而其他容器应更新：%#v", snapshot)
	}
	if len(snapshot.Issues) != 1 || snapshot.Issues[0].ContainerID != "old" || !strings.Contains(snapshot.Issues[0].Message, "查询容器详情失败") {
		t.Fatalf("详情失败应产生问题报告：%#v", snapshot.Issues)
	}

	api.setInspect("old", inspectWith([]string{"VIRTUAL_HOST=old.example"}, "bridge", "10.0.0.20", "", "80/tcp"))
	api.setFailInspect("")
	snapshot, err = watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if endpointAddress(snapshot, "old") != "10.0.0.20" {
		t.Fatalf("详情恢复后应采用新端点：%#v", snapshot)
	}

	api.remove("old")
	snapshot, err = watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if endpointAddress(snapshot, "old") != "" {
		t.Fatalf("列表不再包含容器后应移除旧端点：%#v", snapshot)
	}
}

// TestWatcherTimesOutListAndInspect 验证短请求不会被无响应的兼容 API 永久阻塞。
func TestWatcherTimesOutListAndInspect(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"one": inspectWith([]string{"VIRTUAL_HOST=example.com"}, "bridge", "10.0.0.2", "", "80/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge", RequestTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	api.setHangList(true)
	started := time.Now()
	if _, err = watcher.Sync(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("列表请求应超时，实际错误：%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("列表请求超时返回过慢")
	}

	api.setHangList(false)
	api.setHangInspect("one")
	snapshot, err := watcher.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Issues) != 1 || !strings.Contains(snapshot.Issues[0].Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("详情请求超时应作为容器问题报告：%#v", snapshot)
	}
}

// TestWatcherRequestTimeoutDefaults 验证零请求超时使用五秒默认值，零同步周期使用三十秒默认值。
func TestWatcherRequestTimeoutDefaults(t *testing.T) {
	t.Parallel()

	watcher, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if watcher.client.requestTimeout != 5*time.Second || watcher.resyncInterval != 30*time.Second {
		t.Fatalf("默认周期错误：请求 %s，同步 %s", watcher.client.requestTimeout, watcher.resyncInterval)
	}
}

// TestWatcherWaitsForEventWorker 验证 Start 返回前会等待事件工作协程完成清理。
func TestWatcherWaitsForEventWorker(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	watcher, err := New(Config{ResyncInterval: time.Hour, RequestTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	watcher.client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/containers/json":
			return jsonResponse("[]"), nil
		case "/events":
			close(entered)
			<-request.Context().Done()
			<-release
			return nil, request.Context().Err()
		default:
			return nil, errors.New("意外请求")
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- watcher.Start(ctx, func(Snapshot) {}) }()
	<-entered
	time.Sleep(30 * time.Millisecond)
	select {
	case <-stopped:
		t.Fatal("事件长连接不应应用短请求超时")
	default:
	}
	cancel()
	select {
	case <-stopped:
		t.Fatal("事件工作协程退出前 Start 不应返回")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("事件工作协程退出后 Start 未返回")
	}
}

// TestWatcherRetainsSnapshotAfterSyncFailure 验证同步错误不会覆盖最后成功快照。
func TestWatcherRetainsSnapshotAfterSyncFailure(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t, map[string]inspectResponse{
		"one": inspectWith([]string{"VIRTUAL_HOST=example.com"}, "bridge", "10.0.0.2", "", "80/tcp"),
	})
	watcher, err := New(Config{SocketPath: api.socket, Network: "bridge", ResyncInterval: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = watcher.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.failLists()
	if _, err = watcher.Sync(context.Background()); err == nil {
		t.Fatal("期望同步失败")
	}
	if got := watcher.Snapshot(); len(got.Routes) != 1 {
		t.Fatalf("失败后快照被覆盖：%#v", got)
	}
}

type inspectResponse struct {
	Config struct {
		Env          []string            `json:"Env"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress         string `json:"IPAddress"`
			GlobalIPv6Address string `json:"GlobalIPv6Address"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// inspectWith 构造贴近兼容 API 的容器详情。
func inspectWith(env []string, network, ipv4, ipv6 string, ports ...string) inspectResponse {
	var result inspectResponse
	result.Config.Env = env
	result.Config.ExposedPorts = make(map[string]struct{}, len(ports))
	for _, port := range ports {
		result.Config.ExposedPorts[port] = struct{}{}
	}
	result.NetworkSettings.Networks = map[string]struct {
		IPAddress         string `json:"IPAddress"`
		GlobalIPv6Address string `json:"GlobalIPv6Address"`
	}{network: {IPAddress: ipv4, GlobalIPv6Address: ipv6}}
	return result
}

type testAPI struct {
	t           *testing.T
	socket      string
	server      *http.Server
	mu          sync.Mutex
	fail        bool
	failInspect string
	hangList    bool
	hangInspect string
	events      chan struct{}
	items       map[string]inspectResponse
}

// newTestAPI 在临时 Unix 套接字上启动最小兼容 API。
func newTestAPI(t *testing.T, items map[string]inspectResponse) *testAPI {
	t.Helper()
	socket, err := os.CreateTemp("", "lolly-container-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	api := new(testAPI)
	api.t = t
	api.socket = socket.Name()
	api.events = make(chan struct{}, 1)
	api.items = items
	if err = socket.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(api.socket); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", api.socket)
	if err != nil {
		t.Fatal(err)
	}
	api.server = new(http.Server)
	api.server.Handler = http.HandlerFunc(api.serveHTTP)
	go func() { _ = api.server.Serve(listener) }()
	t.Cleanup(func() {
		_ = api.server.Close()
		_ = os.Remove(api.socket)
	})
	return api
}

// failLists 让后续列表请求失败。
func (a *testAPI) failLists() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fail = true
}

// setFailInspect 设置详情请求失败的容器。
func (a *testAPI) setFailInspect(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failInspect = id
}

// setHangList 设置列表请求是否等待取消。
func (a *testAPI) setHangList(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hangList = enabled
}

// setHangInspect 设置详情请求等待取消的容器。
func (a *testAPI) setHangInspect(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hangInspect = id
}

// setInspect 替换容器详情。
func (a *testAPI) setInspect(id string, response inspectResponse) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items[id] = response
}

// remove 从运行中容器列表移除容器。
func (a *testAPI) remove(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.items, id)
}

// emitEvent 向已连接的事件流写入一个容器变更。
func (a *testAPI) emitEvent() {
	a.events <- struct{}{}
}

// serveHTTP 提供列表、详情与事件三个兼容端点。
func (a *testAPI) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/events" {
		if request.URL.Query().Get("filters") == "" {
			http.Error(writer, "缺少过滤器", http.StatusBadRequest)
			return
		}
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
	a.mu.Lock()
	fail := a.fail
	failInspect := a.failInspect
	hangList := a.hangList
	hangInspect := a.hangInspect
	items := make(map[string]inspectResponse, len(a.items))
	for id, item := range a.items {
		items[id] = item
	}
	a.mu.Unlock()
	if request.URL.Path == "/containers/json" {
		if hangList {
			<-request.Context().Done()
			return
		}
		if fail {
			http.Error(writer, "失败", http.StatusInternalServerError)
			return
		}
		result := make([]map[string]string, 0, len(items))
		for id := range items {
			result = append(result, map[string]string{"Id": id})
		}
		_ = json.NewEncoder(writer).Encode(result)
		return
	}
	for id, item := range items {
		if request.URL.Path == "/containers/"+id+"/json" {
			if id == hangInspect {
				<-request.Context().Done()
				return
			}
			if id == failInspect {
				http.Error(writer, "失败", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(writer).Encode(item)
			return
		}
	}
	http.NotFound(writer, request)
}

// endpointAddress 查询指定容器在快照中的地址。
func endpointAddress(snapshot Snapshot, id string) string {
	for _, route := range snapshot.Routes {
		for _, endpoint := range route.Endpoints {
			if endpoint.ContainerID == id {
				return endpoint.Address
			}
		}
	}
	return ""
}

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip 执行测试定义的 HTTP 往返。
func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

// jsonResponse 构造无需网络连接的 JSON 响应。
func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     http.StatusText(http.StatusOK),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
