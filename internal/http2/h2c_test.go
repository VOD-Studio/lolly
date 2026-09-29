// Package http2 提供明文 HTTP/2（h2c）的测试。
//
// 覆盖四类行为：
//   - 连接前导嗅探与字节回放（h2c 与 HTTP/1.1 共用端口的前提）
//   - Wrap 模式下 h2c 连接的分派与 fasthttp 的 HTTP/1.1 共存
//   - Upgrade: h2c 握手（101 后同一连接转 HTTP/2，被升级请求即流 1）
//   - 独立 Serve 模式下明文连接的 HTTP/1.1 回退
//
// 作者：xfy
package http2

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"rua.plus/lolly/internal/config"
)

// h2cTestConfig 构造一份最小可用的 h2c 配置。
//
// 返回值：
//   - *config.HTTP2Config: 已启用 h2c 的配置
func h2cTestConfig() *config.HTTP2Config {
	return &config.HTTP2Config{
		Enabled:              true,
		H2CEnabled:           true,
		MaxConcurrentStreams: 10,
		IdleTimeout:          5 * time.Second,
	}
}

// echoHandler 返回回显固定字符串的 fasthttp 处理器。
//
// 参数：
//   - body: 响应体内容
//
// 返回值：
//   - fasthttp.RequestHandler: 处理器
func echoHandler(body string) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.WriteString(body)
	}
}

// clientPreface 返回 HTTP/2 连接前导字节，供测试直接写入裸连接。
//
// 返回值：
//   - []byte: 前导字节
func clientPreface() []byte {
	return []byte(h2cPreface)
}

// TestSniffH2C_Preface 验证以 HTTP/2 前导开头的连接被识别，且前导可重新读出。
func TestSniffH2C_Preface(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() {
		_, _ = client.Write(clientPreface())
	}()

	isH2C, sniffed, err := sniffH2C(server, time.Second)
	if err != nil {
		t.Fatalf("sniffH2C() error: %v", err)
	}
	if !isH2C {
		t.Fatal("sniffH2C() 应识别 HTTP/2 前导")
	}

	replayed := make([]byte, len(h2cPreface))
	if _, err := io.ReadAtLeast(sniffed, replayed, len(h2cPreface)); err != nil {
		t.Fatalf("读取回放前导失败: %v", err)
	}
	if string(replayed) != h2cPreface {
		t.Errorf("回放前导不匹配: %q", string(replayed))
	}
	_ = sniffed.Close()
}

// TestSniffH2C_HTTP1Replay 验证普通 HTTP/1.1 请求被判定为非 h2c，
// 且嗅探消耗的字节完整回放（请求行不能被丢弃）。
func TestSniffH2C_HTTP1Replay(t *testing.T) {
	raw := "GET /index.html HTTP/1.1\r\nHost: example\r\n\r\n"
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() {
		_, _ = client.Write([]byte(raw))
	}()

	isH2C, sniffed, err := sniffH2C(server, time.Second)
	if err != nil {
		t.Fatalf("sniffH2C() error: %v", err)
	}
	if isH2C {
		t.Fatal("HTTP/1.1 请求不应被识别为 h2c")
	}

	// 回放后应能按 HTTP/1.1 读出完整请求行。
	line, err := bufio.NewReader(sniffed).ReadString('\n')
	if err != nil {
		t.Fatalf("读取回放请求行失败: %v", err)
	}
	if line != "GET /index.html HTTP/1.1\r\n" {
		t.Errorf("回放请求行不匹配: %q", line)
	}
	_ = sniffed.Close()
}

// TestSniffH2C_SilentConn 验证沉默连接在嗅探超时后按 HTTP/1.1 处理，
// 而不是被当作 h2c 或让 Accept 循环长时间阻塞。
func TestSniffH2C_SilentConn(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	start := time.Now()
	isH2C, sniffed, err := sniffH2C(server, 100*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("sniffH2C() error: %v", err)
	}
	if isH2C {
		t.Error("无字节的连接不应被识别为 h2c")
	}
	if elapsed > 2*time.Second {
		t.Errorf("嗅探超时未生效，耗时 %v", elapsed)
	}
	if sniffed == nil {
		t.Fatal("sniffH2C() 应返回可交回 HTTP/1.1 的连接")
	}
	_ = sniffed.Close()
}

// newTCPConnPair 创建一对回环 TCP 连接，返回客户端与服务器端。
//
// 用真实 TCP 而非 net.Pipe：net.Pipe 上一端 Close 会让另一端的
// SetReadDeadline 直接失败，掩盖被测试路径的真实行为。
//
// 参数：
//   - t: 测试上下文
//
// 返回值：
//   - *net.TCPConn: 客户端连接（可 CloseWrite 模拟半关闭）
//   - net.Conn: 服务端接受到的连接
func newTCPConnPair(t *testing.T) (*net.TCPConn, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	server := <-accepted
	if server == nil {
		t.Fatal("Accept() 未返回连接")
	}
	t.Cleanup(func() { _ = server.Close() })

	return client.(*net.TCPConn), server
}

// TestSniffH2C_ShortPreface 验证前导被截断（客户端只送了一半就半关闭）时
// 按 HTTP/1.1 处理，且已读到的字节仍可回放。
func TestSniffH2C_ShortPreface(t *testing.T) {
	client, server := newTCPConnPair(t)

	if _, err := client.Write([]byte(h2cPreface[:10])); err != nil {
		t.Fatalf("写入截断前导失败: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error: %v", err)
	}

	isH2C, sniffed, err := sniffH2C(server, time.Second)
	if err != nil {
		t.Fatalf("sniffH2C() error: %v", err)
	}
	if isH2C {
		t.Error("截断的前导不应被识别为 h2c")
	}

	body, err := io.ReadAll(sniffed)
	if err != nil {
		t.Fatalf("读取回放字节失败: %v", err)
	}
	if string(body) != h2cPreface[:10] {
		t.Errorf("回放字节不匹配: %q", string(body))
	}
}

// TestSniffedConnPreservesSocketFeatures 验证包装连接不丢失裸 TCP 能力：
// UnderlyingConn 可解包（sendfile 需要按具体类型取 socket）、keepalive 与
// io.ReaderFrom 均转发到底层连接。
func TestSniffedConnPreservesSocketFeatures(t *testing.T) {
	_, server := newTCPConnPair(t)

	wrapped := newSniffedConn(server, bufio.NewReaderSize(server, len(h2cPreface)))

	if wrapped.UnderlyingConn() != server {
		t.Error("UnderlyingConn() 应返回被包装的原始连接")
	}
	if _, ok := wrapped.UnderlyingConn().(*net.TCPConn); !ok {
		t.Errorf("解包后应拿到 *net.TCPConn，实际 %T", wrapped.UnderlyingConn())
	}
	if err := wrapped.SetKeepAlive(true); err != nil {
		t.Errorf("SetKeepAlive() error: %v", err)
	}
	if err := wrapped.SetKeepAlivePeriod(time.Second); err != nil {
		t.Errorf("SetKeepAlivePeriod() error: %v", err)
	}
	if _, ok := interface{}(wrapped).(io.ReaderFrom); !ok {
		t.Error("包装连接应实现 io.ReaderFrom 以保留零拷贝发送")
	}
}

// TestSniffedConnIgnoresUnsupportedKeepAlive 验证底层连接不支持 keepalive 时
// 不返回错误：fasthttp 在报错时会直接关闭连接。
func TestSniffedConnIgnoresUnsupportedKeepAlive(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	wrapped := newSniffedConn(server, bufio.NewReader(server))
	if err := wrapped.SetKeepAlive(true); err != nil {
		t.Errorf("SetKeepAlive() 对不支持的连接应返回 nil，实际 %v", err)
	}
	if err := wrapped.SetKeepAlivePeriod(time.Second); err != nil {
		t.Errorf("SetKeepAlivePeriod() 对不支持的连接应返回 nil，实际 %v", err)
	}
}

// newH2CTestClient 构造以 prior-knowledge 方式访问明文端口的 HTTP/2 客户端。
//
// 返回值：
//   - *http.Client: 直接使用 HTTP/2、不经 TLS 的客户端
func newH2CTestClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(_ context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// TestWrapServesH2CAndHTTP1 验证 fasthttp 与 h2c 嗅探共用一个监听器：
// 前导连接以 HTTP/2 服务，其余连接仍由 fasthttp 按 HTTP/1.1 服务。
func TestWrapServesH2CAndHTTP1(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h2s, err := NewServer(h2cTestConfig(), echoHandler("hello-h2c"), nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	wrapped := h2s.Wrap(ln)

	fastSrv := &fasthttp.Server{Handler: echoHandler("hello-h2c")}
	serveErr := make(chan error, 1)
	go func() { serveErr <- fastSrv.Serve(wrapped) }()

	defer func() {
		_ = fastSrv.Shutdown()
		if err := h2s.Stop(); err != nil {
			t.Logf("Stop() error: %v", err)
		}
		_ = ln.Close()
		select {
		case err := <-serveErr:
			if err != nil && !strings.Contains(err.Error(), "closed") {
				t.Logf("Serve() error: %v", err)
			}
		case <-time.After(time.Second):
		}
	}()

	addr := ln.Addr().String()

	t.Run("prior-knowledge 走 HTTP/2", func(t *testing.T) {
		resp, err := newH2CTestClient().Get("http://" + addr + "/")
		if err != nil {
			t.Fatalf("h2c 请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.ProtoMajor != 2 {
			t.Errorf("期望 HTTP/2，实际 Proto=%s", resp.Proto)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取响应体失败: %v", err)
		}
		if string(body) != "hello-h2c" {
			t.Errorf("响应体不匹配: %q", string(body))
		}
	})

	t.Run("普通请求仍走 HTTP/1.1", func(t *testing.T) {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			t.Fatalf("HTTP/1.1 请求失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.ProtoMajor != 1 {
			t.Errorf("期望 HTTP/1.1，实际 Proto=%s", resp.Proto)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取响应体失败: %v", err)
		}
		if string(body) != "hello-h2c" {
			t.Errorf("响应体不匹配: %q", string(body))
		}
	})
}

// TestWrapRespectsMaxConcurrentConns 验证 h2c 连接数上限：超出额度的连接
// 被直接关闭，而不是无限制地派生 goroutine。
func TestWrapRespectsMaxConcurrentConns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h2s, err := NewServer(h2cTestConfig(), echoHandler("limited"), nil, WithMaxConcurrentConns(1))
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	wrapped := h2s.Wrap(ln)

	fastSrv := &fasthttp.Server{Handler: echoHandler("limited")}
	go func() { _ = fastSrv.Serve(wrapped) }()
	defer func() {
		_ = h2s.Stop()
		_ = fastSrv.Shutdown()
		_ = ln.Close()
	}()

	addr := ln.Addr().String()

	// 第一个连接占满额度：发出前导后不写完 SETTINGS，保持连接活跃。
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer func() { _ = first.Close() }()
	if _, err := first.Write(clientPreface()); err != nil {
		t.Fatalf("写入前导失败: %v", err)
	}

	// 等待第一个连接被分派（额度被占用）。
	deadline := time.Now().Add(2 * time.Second)
	for h2s.activeConns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h2s.activeConns.Load() == 0 {
		t.Fatal("第一个 h2c 连接未被分派")
	}

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer func() { _ = second.Close() }()
	if _, err := second.Write(clientPreface()); err != nil {
		t.Fatalf("写入前导失败: %v", err)
	}

	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Error("超出连接数上限的 h2c 连接应被关闭")
	}
}

// startWrappedH2CServer 启动"嗅探 + 升级握手"双路 h2c 服务器。
//
// 与生产装配一致：fasthttp 拥有 Accept 循环与 HTTP/1.1 处理器，
// h2c 服务器包装监听器并在处理器最外层拦截 Upgrade 握手。
//
// 参数：
//   - t: 测试上下文
//   - body: 响应体内容
//   - opts: 传给 http2.Server 的选项
//
// 返回值：
//   - addr: 监听地址
//   - stop: 关闭服务器与监听器的函数
func startWrappedH2CServer(t *testing.T, body string, opts ...Option) (addr string, stop func()) {
	t.Helper()

	h2s, err := NewServer(h2cTestConfig(), echoHandler(body), nil, opts...)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	wrapped := h2s.Wrap(ln)

	fastSrv := &fasthttp.Server{Handler: h2s.UpgradeHandler(echoHandler(body))}
	go func() { _ = fastSrv.Serve(wrapped) }()

	return ln.Addr().String(), func() {
		_ = h2s.Stop()
		_ = fastSrv.Shutdown()
		_ = ln.Close()
	}
}

// readHTTP1ResponseHead 读取一段 HTTP/1.1 响应起始行与头部。
//
// 用同一个 bufio.Reader 读，后续 HTTP/2 帧才不会丢失已缓冲字节。
//
// 参数：
//   - t: 测试上下文
//   - br: 连接的缓冲读取器
//
// 返回值：
//   - statusLine: 响应起始行（如 "HTTP/1.1 101 Switching Protocols"）
//   - headers: 小写化的响应头部
func readHTTP1ResponseHead(t *testing.T, br *bufio.Reader) (statusLine string, headers map[string]string) {
	t.Helper()

	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读取响应起始行失败: %v", err)
	}
	statusLine = strings.TrimRight(line, "\r\n")

	headers = make(map[string]string)
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			t.Fatalf("读取响应头部失败: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			return statusLine, headers
		}
		name, value, found := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
		if !found {
			t.Fatalf("响应头部格式异常: %q", line)
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
}

// TestUpgradeHandshakeServesUpgradedRequest 验证 RFC 7540 3.2 的 h2c 升级：
// 101 之后同一连接转入 HTTP/2，且被升级的 HTTP/1.1 请求即流 1（客户端不重发）。
func TestUpgradeHandshakeServesUpgradedRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	addr, stop := startWrappedH2CServer(t, "hello-upgrade")
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error: %v", err)
	}

	// HTTP2-Settings: SETTINGS_MAX_CONCURRENT_STREAMS(0x3) = 100
	settings := base64.RawURLEncoding.EncodeToString([]byte{0x00, 0x03, 0x00, 0x00, 0x00, 0x64})
	request := "GET /index.html HTTP/1.1\r\n" +
		"Host: example\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\n" +
		"Upgrade: h2c\r\n" +
		"HTTP2-Settings: " + settings + "\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("写入升级请求失败: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, headers := readHTTP1ResponseHead(t, br)
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("期望 101 响应，实际 %q", statusLine)
	}
	if headers["upgrade"] != "h2c" {
		t.Errorf("Upgrade 头部不匹配: %q", headers["upgrade"])
	}
	if !strings.Contains(headers["connection"], "Upgrade") {
		t.Errorf("Connection 头部不匹配: %q", headers["connection"])
	}

	// 101 之后客户端必须发连接前导 + SETTINGS（RFC 7540 3.5）。
	if _, err := conn.Write([]byte(h2cPreface)); err != nil {
		t.Fatalf("写入连接前导失败: %v", err)
	}
	fr := http2.NewFramer(conn, br)
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}

	var status, body string
	for {
		frame, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("读取 HTTP/2 帧失败: %v", err)
		}
		switch f := frame.(type) {
		case *http2.HeadersFrame:
			if f.StreamID != 1 {
				continue
			}
			decoder := hpack.NewDecoder(4096, nil)
			fields, err := decoder.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				t.Fatalf("解码响应头失败: %v", err)
			}
			for _, hf := range fields {
				if hf.Name == ":status" {
					status = hf.Value
				}
			}
		case *http2.DataFrame:
			if f.StreamID == 1 {
				body += string(f.Data())
			}
		}
		if status != "" && body != "" {
			break
		}
	}

	if status != "200" {
		t.Errorf("期望流 1 响应 200，实际 :status=%q", status)
	}
	if body != "hello-upgrade" {
		t.Errorf("响应体不匹配: %q", body)
	}
}

// TestUpgradeHandshakeRejectsInvalidSettings 验证 HTTP2-Settings 非法时
// 不升级：请求按普通 HTTP/1.1 正常处理，与不支持 h2c 的实现行为一致。
func TestUpgradeHandshakeRejectsInvalidSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	addr, stop := startWrappedH2CServer(t, "not-upgraded")
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error: %v", err)
	}

	request := "GET /index.html HTTP/1.1\r\n" +
		"Host: example\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\n" +
		"Upgrade: h2c\r\n" +
		"HTTP2-Settings: @@not-base64url@@\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("写入升级请求失败: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("非法 HTTP2-Settings 不应接受升级")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if string(body) != "not-upgraded" {
		t.Errorf("应按 HTTP/1.1 正常处理，实际响应体 %q", string(body))
	}
}

// TestUpgradeHandlerPassesThroughPlainRequests 验证升级包装不影响普通请求。
func TestUpgradeHandlerPassesThroughPlainRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	addr, stop := startWrappedH2CServer(t, "passthrough")
	defer stop()

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTP/1.1 请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.ProtoMajor != 1 {
		t.Errorf("期望 HTTP/1.1，实际 Proto=%s", resp.Proto)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if string(body) != "passthrough" {
		t.Errorf("响应体不匹配: %q", string(body))
	}
}

// TestServePlaintextFallsBackToHTTP1 验证独立 Serve 模式下明文连接不再被
// 一律当作 HTTP/2：普通 HTTP/1.1 请求应得到正常响应。
func TestServePlaintextFallsBackToHTTP1(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h2s, err := NewServer(h2cTestConfig(), echoHandler("standalone"), nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	go func() {
		if err := h2s.Serve(ln); err != nil {
			t.Logf("Serve() error: %v", err)
		}
	}()
	defer func() {
		_ = h2s.Stop()
		_ = ln.Close()
	}()

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("HTTP/1.1 请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "standalone" {
		t.Errorf("HTTP/1.1 回退异常: status=%d body=%q", resp.StatusCode, string(body))
	}
}

// TestServePlaintextAcceptsH2C 验证独立 Serve 模式同样支持 prior-knowledge。
func TestServePlaintextAcceptsH2C(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h2s, err := NewServer(h2cTestConfig(), echoHandler("standalone-h2c"), nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	go func() {
		if err := h2s.Serve(ln); err != nil {
			t.Logf("Serve() error: %v", err)
		}
	}()
	defer func() {
		_ = h2s.Stop()
		_ = ln.Close()
	}()

	resp, err := newH2CTestClient().Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("h2c 请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.ProtoMajor != 2 {
		t.Errorf("期望 HTTP/2，实际 Proto=%s", resp.Proto)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if string(body) != "standalone-h2c" {
		t.Errorf("响应体不匹配: %q", string(body))
	}
}

// TestStopInWrapModeKeepsListener 验证 Wrap 模式下 Stop 不关闭外层监听器，
// 监听器生命周期仍由 fasthttp 持有（热升级按原始监听器继承 FD）。
func TestStopInWrapModeKeepsListener(t *testing.T) {
	h2s, err := NewServer(h2cTestConfig(), echoHandler("keep"), nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer func() { _ = ln.Close() }()

	wrapped := h2s.Wrap(ln)
	if wrapped.Addr().String() != ln.Addr().String() {
		t.Errorf("包装监听器地址应保持一致: %s vs %s", wrapped.Addr(), ln.Addr())
	}

	if err := h2s.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	// Stop 之后原始监听器必须仍可接受连接。
	accepted := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- err
			return
		}
		_ = c.Close()
		accepted <- nil
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := <-accepted; err != nil {
		t.Errorf("Stop() 不应关闭外层监听器: %v", err)
	}
}
