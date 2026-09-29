// Package server 提供明文 HTTP/2（h2c）的集成测试。
//
// 验证 ssl.http2.h2c_enabled 在明文监听器上的生效范围：
//   - prior-knowledge 连接以 HTTP/2 服务，同端口的普通请求仍按 HTTP/1.1 服务
//   - enabled 与 h2c_enabled 必须同时为 true 才分派
//   - 按 listen 分组的虚拟主机在 h2c 连接上仍按 Host 分流
//   - 对外暴露的监听器保持原始类型，热升级与重载的 FD 继承不受影响
//
// 作者：xfy
package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"rua.plus/lolly/internal/config"
)

// readHTTP1Head 读取 HTTP/1.1 响应的起始行与头部。
//
// 必须复用调用方持有的 bufio.Reader：101 之后同一连接上紧跟着的就是
// HTTP/2 帧，另起读取器会丢字节。
//
// 参数：
//   - t: 测试上下文
//   - br: 连接的缓冲读取器
//
// 返回值：
//   - statusLine: 响应起始行（不含行尾换行）
//   - headers: 小写化的响应头部
func readHTTP1Head(t *testing.T, br *bufio.Reader) (statusLine string, headers map[string]string) {
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

// writeH2CIndex 写入内容为 body 的 index.html，返回目录路径。
//
// 参数：
//   - t: 测试上下文
//   - body: 静态文件内容
//
// 返回值：
//   - string: 临时目录路径
func writeH2CIndex(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644); err != nil {
		t.Fatalf("写入 index.html 失败: %v", err)
	}
	return dir
}

// h2cServerConfig 构造一份明文静态站点配置。
//
// 参数：
//   - name: server_name（用于分组模式的 Host 分流）
//   - listen: 监听地址
//   - dir: 静态根目录
//   - enabled: ssl.http2.enabled
//   - h2cEnabled: ssl.http2.h2c_enabled
//
// 返回值：
//   - config.ServerConfig: 服务器配置
func h2cServerConfig(name, listen, dir string, enabled, h2cEnabled bool) config.ServerConfig {
	return config.ServerConfig{
		Name:   name,
		Listen: listen,
		SSL: config.SSLConfig{
			HTTP2: config.HTTP2Config{
				Enabled:              enabled,
				H2CEnabled:           h2cEnabled,
				MaxConcurrentStreams: 10,
				IdleTimeout:          5 * time.Second,
				// 嗅探与收尾都用短超时，避免测试等待默认 30s 优雅期。
				GracefulShutdownTimeout: 2 * time.Second,
			},
		},
		Static: []config.StaticConfig{{
			Path:  "/",
			Root:  dir,
			Index: []string{"index.html"},
		}},
	}
}

// waitH2CListener 等待服务器登记首个监听器并返回它。
//
// 单服务器模式下 running 先于监听器登记置位，必须轮询；同时监听启动错误，
// 端口绑定失败时立即失败而不是空等超时。
//
// 参数：
//   - t: 测试上下文
//   - s: 已异步启动的服务器
//   - errCh: Start 的错误通道
//
// 返回值：
//   - net.Listener: 服务器登记的第一个监听器
func waitH2CListener(t *testing.T, s *Server, errCh chan error) net.Listener {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if listeners := s.GetListeners(); len(listeners) > 0 {
			return listeners[0]
		}
		select {
		case err := <-errCh:
			_ = s.GracefulStop(2 * time.Second)
			t.Fatalf("服务器启动失败: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			_ = s.GracefulStop(2 * time.Second)
			t.Fatal("服务器未在超时内暴露监听器")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startH2CServer 启动明文服务器，返回监听地址与停止函数。
//
// 参数：
//   - t: 测试上下文
//   - servers: 一个配置走单服务器模式，多个配置走按 listen 分组模式
//
// 返回值：
//   - addr: 监听地址（host:port）
//   - stop: 关闭服务器并回收资源的函数
func startH2CServer(t *testing.T, servers ...config.ServerConfig) (addr string, stop func()) {
	t.Helper()

	s := New(&config.Config{Servers: servers})
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	if !waitForServerRunning(s, 2*time.Second) {
		_ = s.GracefulStop(2 * time.Second)
		t.Fatal("服务器未在超时内启动")
	}

	addr = waitH2CListener(t, s, errCh).Addr().String()

	return addr, func() {
		_ = s.GracefulStop(2 * time.Second)
		select {
		case err := <-errCh:
			if err != nil && !isExpectedServerErrorForIntegration(err) {
				t.Logf("服务器关闭返回错误: %v", err)
			}
		default:
		}
	}
}

// newH2CClient 构造以 prior-knowledge 方式访问明文端口的 HTTP/2 客户端。
//
// 标准库的 net/http 不提供明文 HTTP/2 入口，需用 x/net 的 http2.Transport
// 配合 AllowHTTP，行为等价于 curl --http2-prior-knowledge。
//
// 返回值：
//   - *http.Client: HTTP/2 客户端
func newH2CClient() *http.Client {
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

// getH2CBody 通过 h2c 客户端请求路径并返回协议主版本与响应体。
//
// URL 的 host 使用真实监听地址（保证可拨号），Host 字段单独指定虚拟主机名。
//
// 参数：
//   - t: 测试上下文
//   - client: h2c 客户端
//   - addr: 监听地址
//   - host: 请求的 Host（空则用监听地址）
//   - path: 请求路径
//
// 返回值：
//   - protoMajor: 响应协议主版本（2 表示 HTTP/2）
//   - body: 响应体
//   - err: 请求失败时返回错误
func getH2CBody(t *testing.T, client *http.Client, addr, host, path string) (protoMajor int, body string, err error) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if host != "" {
		req.Host = host
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return resp.ProtoMajor, string(data), nil
}

// TestH2C_PlaintextServesBothProtocols 验证 h2c 与 HTTP/1.1 共用明文监听器。
func TestH2C_PlaintextServesBothProtocols(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := writeH2CIndex(t, "hello-h2c")
	addr, stop := startH2CServer(t, h2cServerConfig("", "127.0.0.1:0", dir, true, true))
	defer stop()

	t.Run("prior-knowledge 走 HTTP/2", func(t *testing.T) {
		proto, body, err := getH2CBody(t, newH2CClient(), addr, "", "/")
		if err != nil {
			t.Fatalf("h2c 请求失败: %v", err)
		}
		if proto != 2 {
			t.Errorf("期望 HTTP/2，实际协议主版本=%d", proto)
		}
		if body != "hello-h2c" {
			t.Errorf("响应体不匹配: %q", body)
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
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取响应体失败: %v", err)
		}
		if string(data) != "hello-h2c" {
			t.Errorf("响应体不匹配: %q", string(data))
		}
	})
}

// TestH2C_RequiresHTTPTwoEnabled 验证只开 h2c_enabled 而未开 ssl.http2.enabled
// 时不分派 HTTP/2（该组合由启动告警提示）。
func TestH2C_RequiresHTTPTwoEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := writeH2CIndex(t, "plain-only")
	addr, stop := startH2CServer(t, h2cServerConfig("", "127.0.0.1:0", dir, false, true))
	defer stop()

	proto, _, err := getH2CBody(t, newH2CClient(), addr, "", "/")
	if err == nil && proto == 2 {
		t.Error("enabled=false 时不应分派 h2c 连接")
	}

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTP/1.1 请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 1 {
		t.Errorf("期望 HTTP/1.1，实际 Proto=%s", resp.Proto)
	}
}

// TestH2C_DisabledLeavesHTTP1Only 验证两者都未启用时监听器不被包装。
func TestH2C_DisabledLeavesHTTP1Only(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := writeH2CIndex(t, "no-h2")
	addr, stop := startH2CServer(t, h2cServerConfig("", "127.0.0.1:0", dir, false, false))
	defer stop()

	if _, _, err := getH2CBody(t, newH2CClient(), addr, "", "/"); err == nil {
		t.Error("未启用 h2c 时 prior-knowledge 连接不应成功")
	}

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTP/1.1 请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 1 {
		t.Errorf("期望 HTTP/1.1，实际 Proto=%s", resp.Proto)
	}
}

// TestH2C_GroupVHostRouting 验证按 listen 分组的明文监听器上，
// h2c 连接的请求仍按 Host 头分流到各自虚拟主机。
func TestH2C_GroupVHostRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dirA := writeH2CIndex(t, "vhost-a")
	dirB := writeH2CIndex(t, "vhost-b")
	listen := "127.0.0.1:0"

	addr, stop := startH2CServer(t,
		h2cServerConfig("a.example", listen, dirA, true, true),
		h2cServerConfig("b.example", listen, dirB, false, false),
	)
	defer stop()

	client := newH2CClient()
	for host, want := range map[string]string{"a.example": "vhost-a", "b.example": "vhost-b"} {
		t.Run(host, func(t *testing.T) {
			proto, body, err := getH2CBody(t, client, addr, host, "/")
			if err != nil {
				t.Fatalf("h2c 请求失败: %v", err)
			}
			if proto != 2 {
				t.Errorf("期望 HTTP/2，实际协议主版本=%d", proto)
			}
			if body != want {
				t.Errorf("响应体不匹配: 期望 %q，实际 %q", want, body)
			}
		})
	}
}

// TestH2C_UpgradeHandshake 验证生产装配下 Upgrade: h2c 握手可用：
// 升级后的请求要经过完整处理器链（此处为静态文件服务），
// 并按 RFC 7540 3.2 作为流 1 响应。
func TestH2C_UpgradeHandshake(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := writeH2CIndex(t, "upgraded")
	addr, stop := startH2CServer(t, h2cServerConfig("", "127.0.0.1:0", dir, true, true))
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error: %v", err)
	}

	// SETTINGS_MAX_CONCURRENT_STREAMS(0x3) = 100 的 base64url 编码。
	settings := base64.RawURLEncoding.EncodeToString([]byte{0x00, 0x03, 0x00, 0x00, 0x00, 0x64})
	request := "GET / HTTP/1.1\r\n" +
		"Host: example\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\n" +
		"Upgrade: h2c\r\n" +
		"HTTP2-Settings: " + settings + "\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("写入升级请求失败: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, _ := readHTTP1Head(t, br)
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("期望 101 响应，实际 %q", statusLine)
	}

	// 101 之后按 RFC 7540 3.5 发连接前导与 SETTINGS。
	if _, err := conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")); err != nil {
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
			fields, err := hpack.NewDecoder(4096, nil).DecodeFull(f.HeaderBlockFragment())
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
	if body != "upgraded" {
		t.Errorf("响应体不匹配: %q", body)
	}
}

// TestH2C_ExposesRawListener 验证 h2c 包装不外泄到监听器列表：
// 热升级与重载按监听器具体类型继承 FD，包装后会导致 DupListener 失败。
func TestH2C_ExposesRawListener(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := writeH2CIndex(t, "raw-listener")
	s := New(&config.Config{Servers: []config.ServerConfig{
		h2cServerConfig("", "127.0.0.1:0", dir, true, true),
	}})

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	if !waitForServerRunning(s, 2*time.Second) {
		_ = s.GracefulStop(2 * time.Second)
		t.Fatal("服务器未在超时内启动")
	}
	defer func() {
		_ = s.GracefulStop(2 * time.Second)
		select {
		case err := <-errCh:
			if err != nil && !isExpectedServerErrorForIntegration(err) {
				t.Logf("服务器关闭返回错误: %v", err)
			}
		default:
		}
	}()

	ln := waitH2CListener(t, s, errCh)

	duped, err := DupListener(ln)
	if err != nil {
		t.Fatalf("DupListener() 应能复制对外暴露的监听器: %v", err)
	}
	_ = duped.Close()

	// 包装仅作用于 Serve：嗅探分派已生效，说明监听器确被包装。
	proto, _, err := getH2CBody(t, newH2CClient(), ln.Addr().String(), "", "/")
	if err != nil {
		t.Fatalf("h2c 请求失败: %v", err)
	}
	if proto != 2 {
		t.Errorf("期望 HTTP/2，实际协议主版本=%d", proto)
	}
}
