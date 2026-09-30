// Package proxy 提供反向代理功能的测试。
//
// 该文件测试入站为 HTTP/2、HTTP/3 时，代理发往 HTTP/1.x 上游的请求规范化：
//   - 请求行协议必须为 HTTP/1.1，而不是透传 HTTP/2.0
//   - hop-by-hop 头部不应转发给上游
//
// 作者：xfy
package proxy

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/testutil"
)

// rawBackend 是只接受 HTTP/1.x 请求行的简易上游，模拟 Python http.server 的行为：
// 请求行协议不是 HTTP/1.0 或 HTTP/1.1 时返回 505。
type rawBackend struct {
	mu      sync.Mutex
	line    string
	headers http.Header
}

func (b *rawBackend) serve(t *testing.T, ln net.Listener) {
	t.Helper()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			r := bufio.NewReader(c)
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			hdr := http.Header{}
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				l = strings.TrimRight(l, "\r\n")
				if l == "" {
					break
				}
				if k, v, ok := strings.Cut(l, ":"); ok {
					hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
				}
			}
			b.mu.Lock()
			b.line, b.headers = line, hdr
			b.mu.Unlock()

			if !strings.HasSuffix(line, " HTTP/1.1") && !strings.HasSuffix(line, " HTTP/1.0") {
				_, _ = c.Write([]byte("HTTP/1.0 505 HTTP Version Not Supported\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
				return
			}
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
		}(conn)
	}
}

// TestServeHTTP_H2InboundUsesHTTP11Upstream 回归测试：入站协议为 HTTP/2.0 / HTTP/3 时，
// 上游请求行必须是 HTTP/1.1，且 hop-by-hop 头部不被转发。
func TestServeHTTP_H2InboundUsesHTTP11Upstream(t *testing.T) {
	for _, proto := range []string{"HTTP/2.0", "HTTP/3", "HTTP/1.1"} {
		t.Run(proto, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer func() { _ = ln.Close() }()
			b := &rawBackend{}
			go b.serve(t, ln)

			url := "http://" + ln.Addr().String()
			targets := testutil.NewTestTargets(url)
			targets[0].Healthy.Store(true)
			p, err := NewProxy(testutil.NewTestProxyConfig("/", url), targets, nil, nil)
			if err != nil {
				t.Fatalf("NewProxy() error: %v", err)
			}

			ctx := testutil.NewRequestCtx("GET", "/who.txt")
			ctx.Request.Header.SetProtocol(proto)
			ctx.Request.Header.Set("Keep-Alive", "timeout=5")
			ctx.Request.Header.Set("TE", "trailers")
			ctx.Request.Header.Set("Connection", "X-Hop, keep-alive")
			ctx.Request.Header.Set("X-Hop", "1")
			ctx.Request.Header.Set("X-Keep", "1")

			done := make(chan struct{})
			go func() { p.ServeHTTP(ctx); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("ServeHTTP timeout")
			}

			if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%q)", got, ctx.Response.Body())
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if want := "GET /who.txt HTTP/1.1"; b.line != want {
				t.Errorf("upstream request line = %q, want %q", b.line, want)
			}
			for _, h := range []string{"Keep-Alive", "Te", "X-Hop"} {
				if v := b.headers.Get(h); v != "" {
					t.Errorf("hop-by-hop header %s forwarded to upstream: %q", h, v)
				}
			}
			if b.headers.Get("X-Keep") != "1" {
				t.Errorf("end-to-end header X-Keep was dropped")
			}
		})
	}
}

// TestNormalizeUpstreamRequest 直接测试请求头规范化函数。
func TestNormalizeUpstreamRequest(t *testing.T) {
	var h fasthttp.RequestHeader
	h.SetProtocol("HTTP/2.0")
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("Upgrade", "h2c")
	h.Set("Connection", "Upgrade, HTTP2-Settings")
	h.Set("HTTP2-Settings", "AAMAAABk")

	normalizeUpstreamRequest(&h)

	if got := string(h.Protocol()); got != "HTTP/1.1" {
		t.Errorf("Protocol = %q, want HTTP/1.1", got)
	}
	for _, k := range []string{"Proxy-Connection", "Upgrade", "Connection", "HTTP2-Settings"} {
		if v := h.Peek(k); len(v) != 0 {
			t.Errorf("header %s should be removed, got %q", k, v)
		}
	}
	if !h.IsHTTP11() {
		t.Error("IsHTTP11() = false, want true")
	}
}
