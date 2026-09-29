// Package server 提供按监听地址分组的服务器测试。
//
// 该文件验证同一监听器上的 Host 分流和监听器生命周期。
//
// 作者：xfy
package server

import (
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
		{Name: "a.example", ServerNames: []string{"a.example"}, Listen: "127.0.0.1:0", Default: true, Static: []config.StaticConfig{{Path: "/", Root: roots[0], Index: []string{"index.html"}}}},
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

// TestSetListenersKeepsProvidedListenersSeparate 验证外部监听器仅作为启动输入保存。
func TestSetListenersKeepsProvidedListenersSeparate(t *testing.T) {
	srv := New(&config.Config{})
	srv.SetListeners([]net.Listener{})
	if srv.GetListeners() != nil {
		t.Fatal("启动前不应暴露尚未激活的监听器")
	}
}
