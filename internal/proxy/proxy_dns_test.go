// Package proxy 提供 DNS 代理功能的测试。
//
// 该文件测试 proxy_dns.go 中的 DNS 相关功能，包括：
//   - DNS 解析器设置和获取
//   - DNS 缓存刷新机制
//   - HostClient 地址更新
//   - 错误处理场景
//
// 作者：xfy
package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/resolver"
)

// mockResolver 实现 resolver.Resolver 接口的模拟解析器。
type mockResolver struct {
	mu                   sync.RWMutex
	lookupResults        map[string][]string
	lookupError          error
	startErr             error
	stopErr              error
	lookupHostCalls      int
	lookupWithCacheCalls int
	startCalls           int
	stopCalls            int
}

func (m *mockResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	m.mu.Lock()
	m.lookupHostCalls++
	m.mu.Unlock()
	return m.resolve(host)
}

func (m *mockResolver) LookupHostWithCache(_ context.Context, host string) ([]string, error) {
	m.mu.Lock()
	m.lookupWithCacheCalls++
	m.mu.Unlock()
	return m.resolve(host)
}

func (m *mockResolver) resolve(host string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.lookupError != nil {
		return nil, m.lookupError
	}
	if ips, ok := m.lookupResults[host]; ok {
		return ips, nil
	}
	return nil, errors.New("host not found in mock resolver")
}

func (m *mockResolver) Refresh(_ string) error {
	return nil
}

func (m *mockResolver) Start() error {
	m.mu.Lock()
	m.startCalls++
	m.mu.Unlock()
	return m.startErr
}

func (m *mockResolver) Stop() error {
	m.mu.Lock()
	m.stopCalls++
	m.mu.Unlock()
	return m.stopErr
}

func (m *mockResolver) Stats() resolver.Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return resolver.Stats{
		CacheHits:   int64(m.lookupWithCacheCalls),
		CacheMisses: int64(m.lookupHostCalls),
	}
}

// TestProxyCloseIsIdempotent 验证关闭代理可重复调用并释放生命周期资源。
func TestProxyCloseIsIdempotent(t *testing.T) {
	p, err := NewProxy(&config.ProxyConfig{}, []*loadbalance.Target{
		loadbalance.NewTargetFromConfig("http://127.0.0.1:8080", 1, 0, 0, 0, false, false, ""),
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	p.Close()
	p.Close()
	select {
	case <-p.stopCh:
	default:
		t.Fatal("关闭代理后停止信号应已关闭")
	}
}

// TestSetResolver 测试设置 DNS 解析器。
func TestSetResolver(t *testing.T) {
	cfg := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
	}
	targets := []*loadbalance.Target{
		{URL: "http://localhost:8080"},
	}

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	// 初始状态 resolver 为 nil
	if p.resolver != nil {
		t.Error("resolver should be nil initially")
	}

	// 设置解析器
	mr := &mockResolver{}
	p.SetResolver(mr)

	if p.resolver != mr {
		t.Error("SetResolver() did not set resolver correctly")
	}
}

// TestGetResolverStats_NoResolver 测试没有解析器时返回空统计。

// TestGetResolverStats_WithResolver 测试有解析器时返回统计。

// TestStartWithResolver 测试启动代理时解析器正确启动。

// TestStartResolverFails 测试解析器启动失败时代理返回错误。

// TestStartIdempotent 测试 Start 是幂等的。

// TestStopIdempotent 测试 Stop 是幂等的。

// TestStopWithoutResolver 测试没有解析器时停止代理。

// TestRefreshDNS_Success 测试 DNS 刷新成功场景。

// TestRefreshDNS_LookupError 测试 DNS 刷新时查找失败场景。

// TestRefreshDNS_NoResolver 测试没有解析器时刷新不执行任何操作。

// TestRefreshDNS_IPAddressTarget 测试 IP 类型的目标不需要解析。

// TestRefreshDNS_RecentlyResolved 测试最近已解析的目标不需要再次解析。

// TestRefreshDNS_ExpiredResolve 测试 TTL 过期后需要重新解析。
// 该测试验证 TTL 过期检查的正确性：
// - 短时间内（< TTL）不需要重新解析
// - 长时间后（> TTL）需要重新解析

// TestUpdateHostClientAddr_HTTP 测试 HTTP 目标地址更新。

// TestUpdateHostClientAddr_HTTPS 测试 HTTPS 目标地址更新。

// TestUpdateHostClientAddr_DefaultPort 测试没有端口时使用默认端口。

// TestUpdateHostClientAddr_NonExistentTarget 测试不存在的目标不更新。

// TestGetResolverTTL 测试 TTL 获取。

// TestDNSRefreshLoop_StartStop 测试 DNS 刷新循环的启动和停止。

// TestMultipleTargets_Refresh 测试多目标刷新。

// TestUpdateHostClientAddr_ReplacesClient 验证 DNS 更新时重建 HostClient 而不是修改 Addr。
func TestUpdateHostClientAddr_ReplacesClient(t *testing.T) {
	cfg := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
	}
	targets := []*loadbalance.Target{
		{URL: "http://backend.example.com:8080"},
	}

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	oldClient := p.clients["http://backend.example.com:8080"]
	if oldClient == nil {
		t.Fatal("old client should not be nil")
	}
	oldAddr := oldClient.Addr

	p.updateHostClientAddr(targets[0], "192.168.1.100")

	newClient := p.clients["http://backend.example.com:8080"]
	if newClient == nil {
		t.Fatal("new client should not be nil")
	}
	if newClient == oldClient {
		t.Error("updateHostClientAddr should replace the HostClient instead of mutating it")
	}
	if newClient.Addr != "192.168.1.100:8080" {
		t.Errorf("new client addr = %q, want %q", newClient.Addr, "192.168.1.100:8080")
	}
	// 旧 client 的 Addr 不应被修改，旧连接继续使用
	if oldClient.Addr != oldAddr {
		t.Errorf("old client addr was mutated: got %q, want %q", oldClient.Addr, oldAddr)
	}
}

// TestUpdateHostClientAddr_WithProxyBind 验证配置了 proxy_bind 时使用正确的 client key。
func TestUpdateHostClientAddr_WithProxyBind(t *testing.T) {
	cfg := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
		ProxyBind:   "127.0.0.1",
	}
	targets := []*loadbalance.Target{
		{URL: "http://backend.example.com:8080"},
	}

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	key := "http://backend.example.com:8080|127.0.0.1"
	if p.clients[key] == nil {
		t.Fatalf("client with proxy_bind key should exist")
	}

	p.updateHostClientAddr(targets[0], "192.168.1.200")

	newClient := p.clients[key]
	if newClient == nil {
		t.Fatal("new client should not be nil")
	}
	if newClient.Addr != "192.168.1.200:8080" {
		t.Errorf("new client addr = %q, want %q", newClient.Addr, "192.168.1.200:8080")
	}
}

// TestUpdateHostClientAddr_ConcurrentRead 验证更新 Addr 时不会与 getClient 产生数据竞争。
func TestUpdateHostClientAddr_ConcurrentRead(t *testing.T) {
	cfg := &config.ProxyConfig{
		Path:        "/api",
		LoadBalance: "round_robin",
		Timeout:     config.ProxyTimeout{Connect: 5 * time.Second},
	}
	targets := []*loadbalance.Target{
		{URL: "http://backend.example.com:8080"},
	}

	p, err := NewProxy(cfg, targets, nil, nil)
	if err != nil {
		t.Fatalf("NewProxy() error: %v", err)
	}

	var wg sync.WaitGroup
	// 并发读取 client
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := p.getClient(targets[0].URL)
			_ = client.Addr
		}()
	}

	// 并发更新 client
	for i := range 10 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p.updateHostClientAddr(targets[0], "192.168.1."+string(rune('0'+id)))
		}(i)
	}

	wg.Wait()
}

// TestStopResolverFails 测试停止解析器失败时返回错误。
