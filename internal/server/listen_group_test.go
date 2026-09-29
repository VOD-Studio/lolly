// Package server 提供按监听地址分组的服务器测试。
//
// 该文件验证同一监听器上的 Host 分流和监听器生命周期。
//
// 作者：xfy
package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rua.plus/lolly/internal/config"
)

// TestMultiServerModeRoutesHostsOnOneListener 验证同一 listen 只创建一个监听器并按 Host 分流。
func TestMultiServerModeRoutesHostsOnOneListener(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	for i := range roots {
		if err := os.WriteFile(filepath.Join(roots[i], "index.html"), []byte(fmt.Sprintf("host-%d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Mode: config.ServerModeMultiServer, Servers: []config.ServerConfig{
		{Name: "a.example", ServerNames: []string{"a.example"}, Listen: "127.0.0.1:0", Static: []config.StaticConfig{{Path: "/", Root: roots[0], Index: []string{"index.html"}}}},
		{Name: "b.example", ServerNames: []string{"b.example"}, Listen: "127.0.0.1:0", Static: []config.StaticConfig{{Path: "/", Root: roots[1], Index: []string{"index.html"}}}},
	}}
	srv := New(cfg)
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	waitForServerRunning(srv, 2*time.Second)
	t.Cleanup(func() { _ = srv.GracefulStop(time.Second) })

	if got := len(srv.GetListeners()); got != 1 {
		t.Fatalf("监听器数量 = %d，期望 1", got)
	}
	for host, want := range map[string]string{"a.example": "host-0", "b.example": "host-1", "unknown.example": "host-0"} {
		req, err := http.NewRequest(http.MethodGet, "http://"+srv.GetListeners()[0].Addr().String()+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", host, err)
		}
		body := make([]byte, len(want))
		_, _ = resp.Body.Read(body)
		_ = resp.Body.Close()
		if string(body) != want {
			t.Errorf("Host %s 响应 = %q，期望 %q", host, body, want)
		}
	}
}

// TestStartAutoModeRoutesHostsOnOneListener 验证 auto 推断为 vhost 时仍使用统一分组启动路径。
func TestStartAutoModeRoutesHostsOnOneListener(t *testing.T) {
	cfg := &config.Config{Servers: []config.ServerConfig{
		{Name: "first.example", Listen: "127.0.0.1:0"},
		{Name: "second.example", Listen: "127.0.0.1:0"},
	}}
	srv := New(cfg)
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	waitForServerRunning(srv, 2*time.Second)
	t.Cleanup(func() { _ = srv.GracefulStop(time.Second) })

	if len(srv.fastServers) != 1 || srv.fastServer != nil {
		t.Fatalf("auto 多服务器应使用分组启动，fastServers=%d fastServer=%v", len(srv.fastServers), srv.fastServer)
	}
}

// TestServeListenGroupsReturnsError 验证任一监听分组服务失败时向调用方返回错误。
func TestServeListenGroupsReturnsError(t *testing.T) {
	wantFirst := errors.New("first serve failed")
	wantSecond := errors.New("second serve failed")
	err := serveListenGroups([]func() error{
		func() error { return nil },
		func() error { return wantFirst },
		func() error { return wantSecond },
	}, func() {})
	if !errors.Is(err, wantFirst) || !errors.Is(err, wantSecond) {
		t.Fatalf("serveListenGroups() = %v，期望包含两个服务错误", err)
	}
}

// TestServeListenGroupsStopsSibling 验证一个分组失败时会停止仍在阻塞的兄弟分组。
func TestServeListenGroupsStopsSibling(t *testing.T) {
	want := errors.New("serve failed")
	blocking := make(chan struct{})
	err := serveListenGroups([]func() error{
		func() error { <-blocking; return nil },
		func() error { return want },
	}, func() { close(blocking) })
	if !errors.Is(err, want) {
		t.Fatalf("serveListenGroups() = %v，期望 %v", err, want)
	}
}

// TestMultiListenGroupsUseIndependentDefaults 验证多个监听分组分别使用各自的首个服务器作为默认主机。
func TestMultiListenGroupsUseIndependentDefaults(t *testing.T) {
	roots := make([]string, 4)
	servers := make([]config.ServerConfig, 4)
	for i := range roots {
		roots[i] = t.TempDir()
		if err := os.WriteFile(filepath.Join(roots[i], "index.html"), []byte(fmt.Sprintf("group-host-%d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		listen := "127.0.0.1:0"
		if i >= 2 {
			listen = "localhost:0"
		}
		servers[i] = config.ServerConfig{Name: fmt.Sprintf("host-%d.example", i), Listen: listen, Static: []config.StaticConfig{{Path: "/", Root: roots[i], Index: []string{"index.html"}}}}
	}
	srv := New(&config.Config{Mode: config.ServerModeMultiServer, Servers: servers})
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	waitForServerRunning(srv, 2*time.Second)
	t.Cleanup(func() { _ = srv.GracefulStop(time.Second) })

	if len(srv.GetListeners()) != 2 {
		t.Fatalf("监听器数量 = %d，期望 2", len(srv.GetListeners()))
	}
	for i, want := range []string{"group-host-0", "group-host-2"} {
		resp, err := http.Get("http://" + srv.GetListeners()[i].Addr().String() + "/")
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, len(want))
		_, _ = resp.Body.Read(body)
		_ = resp.Body.Close()
		if string(body) != want {
			t.Errorf("分组 %d 默认响应 = %q，期望 %q", i, body, want)
		}
	}
}

// TestListenGroupSelectsCertificateBySNI 验证监听分组按 SNI 选择各虚拟主机证书。
func TestListenGroupSelectsCertificateBySNI(t *testing.T) {
	dir := t.TempDir()
	certA, keyA := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	certB, keyB := filepath.Join(dir, "b.crt"), filepath.Join(dir, "b.key")
	if err := generateSelfSignedCert(certA, keyA); err != nil {
		t.Fatal(err)
	}
	if err := generateSelfSignedCert(certB, keyB); err != nil {
		t.Fatal(err)
	}
	srv := New(&config.Config{Servers: []config.ServerConfig{
		{Name: "a.example", Listen: ":443", SSL: config.SSLConfig{Cert: certA, Key: keyA}},
		{Name: "b.example", Listen: ":443", SSL: config.SSLConfig{Cert: certB, Key: keyB}},
	}})
	fastSrv, err := srv.buildListenGroupServer(listenGroup{listen: ":443", indices: []int{0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.cleanupResources)

	cfgA, err := fastSrv.TLSConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "a.example"})
	if err != nil {
		t.Fatal(err)
	}
	cfgB, err := fastSrv.TLSConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "b.example"})
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgA.Certificates[0].Certificate[0]) == string(cfgB.Certificates[0].Certificate[0]) {
		t.Fatal("不同 SNI 应选择不同证书")
	}
}

// TestGroupedStartupFailureCleansResources 验证后续分组绑定失败会释放已创建的监听器和后台资源。
func TestGroupedStartupFailureCleansResources(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()

	srv := New(&config.Config{
		Mode: config.ServerModeMultiServer,
		Servers: []config.ServerConfig{
			{Name: "first", Listen: "127.0.0.1:0"},
			{Name: "occupied", Listen: probe.Addr().String()},
		},
		Performance: config.PerformanceConfig{GoroutinePool: config.GoroutinePoolConfig{Enabled: true, MaxWorkers: 1}},
	})
	if err := srv.Start(); err == nil {
		t.Fatal("第二个分组绑定冲突时 Start() 应返回错误")
	}
	if len(srv.GetListeners()) != 0 {
		t.Fatalf("启动失败后仍有 %d 个活动监听器", len(srv.GetListeners()))
	}
	if err := srv.StopWithTimeout(time.Second); err != nil {
		t.Fatalf("启动失败后 StopWithTimeout() = %v", err)
	}
}

// TestSetListenersKeepsProvidedListenersSeparate 验证外部监听器仅作为启动输入保存。
func TestSetListenersKeepsProvidedListenersSeparate(t *testing.T) {
	srv := New(&config.Config{})
	srv.SetListeners([]net.Listener{})
	if srv.GetListeners() != nil {
		t.Fatal("启动前不应暴露尚未激活的监听器")
	}
}
