// Package proxy 提供 WebSocket 代理功能的测试。
//
// 该文件测试 WebSocket 代理模块的各项功能，包括：
//   - 桥接器创建
//   - 桥接器关闭
//   - 空连接处理
//   - 连接关闭错误检测
//   - 目标地址拨号
//   - URL 解析
//   - 数据复制
//   - 双向数据转发
//   - 超时错误处理
//   - 并发连接测试
//   - 大消息转发测试
//
// goroutine 泄漏检测说明：
// fasthttp 库使用后台 worker goroutine，与 goleak 不兼容。
// 如需检测泄漏，可手动运行：go test -race ./internal/proxy/...
//
// 作者：xfy
package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
)

// TestNewWebSocketBridge 测试桥接器创建
func TestNewWebSocketBridge(t *testing.T) {
	clientConn, _ := net.Pipe()
	targetConn, _ := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = targetConn.Close() }()

	bridge := NewWebSocketBridge(clientConn, targetConn)
	if bridge == nil {
		t.Fatal("Expected non-nil bridge")
	}
	if bridge.clientConn != clientConn {
		t.Error("Expected clientConn to be set")
	}
	if bridge.targetConn != targetConn {
		t.Error("Expected targetConn to be set")
	}
	if bridge.closed {
		t.Error("Expected closed to be false initially")
	}
}

// TestWebSocketBridge_Close 测试关闭桥接器
func TestWebSocketBridge_Close(t *testing.T) {
	clientConn, client2 := net.Pipe()
	targetConn, target2 := net.Pipe()

	bridge := NewWebSocketBridge(clientConn, targetConn)

	// 关闭桥接器
	err := bridge.Close()
	if err != nil {
		t.Errorf("Expected nil error, got: %v", err)
	}

	// 验证连接已关闭 - 写入应该失败
	_, err = client2.Write([]byte("test"))
	if err == nil {
		t.Error("Expected error writing to closed connection")
	}

	_ = target2

	// 重复关闭应该安全
	err = bridge.Close()
	if err != nil {
		t.Errorf("Expected nil error on double close, got: %v", err)
	}
}

// TestWebSocketBridge_Close_NilConnections 测试空连接的关闭
func TestWebSocketBridge_Close_NilConnections(t *testing.T) {
	bridge := &WebSocketBridge{
		clientConn: nil,
		targetConn: nil,
		closed:     false,
	}

	err := bridge.Close()
	if err != nil {
		t.Errorf("Expected nil error for nil connections, got: %v", err)
	}
}

// TestIsConnectionClosedError 测试连接关闭错误检测
func TestIsConnectionClosedError(t *testing.T) {
	tests := []struct {
		err      error
		name     string
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "EOF",
			err:      io.EOF,
			expected: true,
		},
		{
			name:     "other error",
			err:      errors.New("some other error"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isConnectionClosedError(tt.err)
			if result != tt.expected {
				t.Errorf("isConnectionClosedError(%v) = %v, expected %v", tt.err, result, tt.expected)
			}
		})
	}
}

// TestDialTarget_InvalidAddress 测试无效地址的拨号
func TestDialTarget_InvalidAddress(t *testing.T) {
	// 测试连接到无效端口
	_, err := dialTarget("http://127.0.0.1:1", 100*time.Millisecond, nil)
	if err == nil {
		t.Error("Expected error for invalid address")
	}
}

// TestDialTarget_HTTPS 测试 HTTPS 连接（会失败，但验证错误处理）
func TestDialTarget_HTTPS(t *testing.T) {
	// 测试 HTTPS 连接到无效端口
	_, err := dialTarget("https://127.0.0.1:1", 100*time.Millisecond, nil)
	if err == nil {
		t.Error("Expected error for invalid HTTPS address")
	}
}

// TestDialTarget_TLSClearsHandshakeDeadline 验证 TLS 握手超时不会限制后续 WebSocket 长连接。
func TestDialTarget_TLSClearsHandshakeDeadline(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	conn, err := dialTarget(server.URL, 50*time.Millisecond, &config.ProxySSLConfig{
		Enabled:            true,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("dialTarget() error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	time.Sleep(100 * time.Millisecond)
	if _, err = conn.Write([]byte("x")); err != nil {
		t.Fatalf("TLS connection retained handshake deadline: %v", err)
	}
}

// mockNetError 模拟网络错误
type mockNetError struct {
	msg string
}

func (e *mockNetError) Error() string   { return e.msg }
func (e *mockNetError) Timeout() bool   { return true }
func (e *mockNetError) Temporary() bool { return false }

// TestIsConnectionClosedError_Timeout 测试超时错误
func TestIsConnectionClosedError_Timeout(t *testing.T) {
	timeoutErr := &mockNetError{msg: "timeout"}
	result := isConnectionClosedError(timeoutErr)
	if !result {
		t.Error("Expected timeout error to be treated as closed connection error")
	}
}

// TestWebSocketBridge_Bridge 测试双向数据转发
func TestWebSocketBridge_Bridge(t *testing.T) {
	// 创建管道连接
	client1, client2 := net.Pipe()
	target1, target2 := net.Pipe()
	defer func() { _ = client2.Close() }()
	defer func() { _ = target2.Close() }()

	bridge := NewWebSocketBridge(client1, target1)

	// 启动桥接（在 goroutine 中）
	errCh := make(chan error, 1)
	go func() {
		errCh <- bridge.Bridge()
	}()

	// 发送数据从客户端到后端
	testData := []byte("hello from client")
	go func() {
		_, _ = client2.Write(testData)
	}()

	// 在后端读取数据
	buf := make([]byte, 1024)
	n, err := target2.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read from target: %v", err)
	}
	if string(buf[:n]) != string(testData) {
		t.Errorf("Expected %q, got %q", string(testData), string(buf[:n]))
	}

	// 发送数据从后端到客户端
	testData2 := []byte("hello from target")
	go func() {
		_, _ = target2.Write(testData2)
	}()

	// 在客户端读取数据
	buf2 := make([]byte, 1024)
	n, err = client2.Read(buf2)
	if err != nil {
		t.Fatalf("Failed to read from client: %v", err)
	}
	if string(buf2[:n]) != string(testData2) {
		t.Errorf("Expected %q, got %q", string(testData2), string(buf2[:n]))
	}

	// 关闭连接以结束桥接
	_ = client2.Close()
	_ = target2.Close()

	// 等待桥接完成
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Bridge returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Bridge did not complete in time")
	}
}

// TestDialTarget_URLParsing 测试 URL 解析
func TestDialTarget_URLParsing(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		expectError bool
	}{
		{
			name:        "http URL with invalid port",
			url:         "http://127.0.0.1:1",
			expectError: true, // 连接会失败
		},
		{
			name:        "https URL with invalid port",
			url:         "https://127.0.0.1:1",
			expectError: true, // 连接会失败
		},
		{
			name:        "URL with path",
			url:         "http://127.0.0.1:1/ws",
			expectError: true, // 连接会失败
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := dialTarget(tt.url, 10*time.Millisecond, nil)
			if tt.expectError {
				if err == nil {
					t.Error("Expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

// TestCopyData 测试数据复制
func TestCopyData(t *testing.T) {
	// 创建管道连接
	src1, src2 := net.Pipe()
	dst1, dst2 := net.Pipe()
	defer func() { _ = src2.Close() }()
	defer func() { _ = dst2.Close() }()

	bridge := &WebSocketBridge{}

	// 启动数据复制
	errCh := make(chan error, 1)
	go func() {
		errCh <- bridge.copyData(dst1, src1, "test")
	}()

	// 发送数据
	testData := []byte("test data")
	_, _ = src2.Write(testData)

	// 接收数据
	buf := make([]byte, 1024)
	n, err := dst2.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read: %v", err)
	}
	if string(buf[:n]) != string(testData) {
		t.Errorf("Expected %q, got %q", string(testData), string(buf[:n]))
	}

	// 关闭连接
	_ = src2.Close()
	_ = dst2.Close()

	// 等待复制完成
	select {
	case err := <-errCh:
		// 连接关闭错误应返回 nil
		if err != nil && !strings.Contains(err.Error(), "closed") {
			t.Errorf("copyData returned unexpected error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("copyData did not complete in time")
	}
}

// TestBuildWebSocketUpgradeRequest 测试构建 WebSocket 升级请求
func TestBuildWebSocketUpgradeRequest(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		query        string
		host         string
		targetHost   string
		wantContains []string
	}{
		{
			name:       "basic request",
			path:       "/ws",
			query:      "",
			host:       "client.example.com",
			targetHost: "backend.example.com:8080",
			wantContains: []string{
				"GET /ws HTTP/1.1",
				"Host: backend.example.com:8080",
				"X-Forwarded-For:",
				"X-Real-IP:",
				"X-Forwarded-Host: client.example.com",
			},
		},
		{
			name:       "request with query",
			path:       "/ws",
			query:      "token=abc123",
			host:       "client.example.com",
			targetHost: "backend.example.com",
			wantContains: []string{
				"GET /ws?token=abc123 HTTP/1.1",
				"Host: backend.example.com",
			},
		},
		{
			name:       "empty path defaults to slash",
			path:       "",
			query:      "",
			host:       "client.example.com",
			targetHost: "backend.example.com",
			wantContains: []string{
				"GET / HTTP/1.1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetRequestURI(tt.path)
			if tt.query != "" {
				ctx.QueryArgs().Parse(tt.query)
			}
			ctx.Request.Header.SetHost(tt.host)

			result := buildWebSocketUpgradeRequest(ctx, tt.targetHost, nil)

			for _, want := range tt.wantContains {
				if !strings.Contains(result, want) {
					t.Errorf("buildWebSocketUpgradeRequest() missing %q in output:\n%s", want, result)
				}
			}
		})
	}
}

// TestBuildWebSocketUpgradeRequest_WithHeaders 测试复制 WebSocket 头
func TestBuildWebSocketUpgradeRequest_WithHeaders(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/ws")
	ctx.Request.Header.Set("Upgrade", "websocket")
	ctx.Request.Header.Set("Connection", "Upgrade")
	ctx.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	ctx.Request.Header.Set("Sec-WebSocket-Version", "13")
	ctx.Request.Header.Set("Sec-WebSocket-Protocol", "chat")

	result := buildWebSocketUpgradeRequest(ctx, "backend.example.com", nil)

	// 验证关键头被复制
	expectedHeaders := []string{
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==",
		"Sec-WebSocket-Version: 13",
		"Sec-WebSocket-Protocol: chat",
	}

	for _, expected := range expectedHeaders {
		if !strings.Contains(result, expected) {
			t.Errorf("Missing expected header %q in:\n%s", expected, result)
		}
	}
}

// TestBuildWebSocketUpgradeRequest_TLSProto 测试 TLS 协议标记
func TestBuildWebSocketUpgradeRequest_TLSProto(t *testing.T) {
	tests := []struct {
		name      string
		wantProto string
		isTLS     bool
	}{
		{
			name:      "non-TLS connection",
			isTLS:     false,
			wantProto: "X-Forwarded-Proto: http",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetRequestURI("/ws")

			// 注意：fasthttp.RequestCtx 默认 IsTLS() 返回 false
			// 无法在单元测试中直接模拟 TLS 连接

			result := buildWebSocketUpgradeRequest(ctx, "backend.example.com", nil)

			if !strings.Contains(result, tt.wantProto) {
				t.Errorf("Missing %q in:\n%s", tt.wantProto, result)
			}
		})
	}
}

// TestExtractHost 测试从 URL 提取主机
func TestExtractHost(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected string
	}{
		{
			name:     "http with port",
			url:      "http://example.com:8080",
			expected: "example.com:8080",
		},
		{
			name:     "https with port",
			url:      "https://example.com:8443",
			expected: "example.com:8443",
		},
		{
			name:     "http without port",
			url:      "http://example.com",
			expected: "example.com:80",
		},
		{
			name:     "https without port",
			url:      "https://example.com",
			expected: "example.com:443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractHost(tt.url)
			if result != tt.expected {
				t.Errorf("extractHost(%q) = %q, want %q", tt.url, result, tt.expected)
			}
		})
	}
}

// TestWriteUpgradeResponse 测试写入升级响应
func TestWriteUpgradeResponse(t *testing.T) {
	// 创建管道连接
	conn1, conn2 := net.Pipe()
	defer func() { _ = conn2.Close() }()

	// 创建模拟 HTTP 响应
	resp := &http.Response{
		ProtoMajor: 1,
		ProtoMinor: 1,
		Status:     "101 Switching Protocols",
		StatusCode: 101,
		Header: http.Header{
			"Upgrade":    []string{"websocket"},
			"Connection": []string{"Upgrade"},
		},
	}

	// 启动写入
	errCh := make(chan error, 1)
	go func() {
		errCh <- writeUpgradeResponse(conn1, resp)
		_ = conn1.Close()
	}()

	// 读取响应
	buf := make([]byte, 1024)
	n, err := conn2.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	response := string(buf[:n])

	// 验证响应格式
	expectedParts := []string{
		"HTTP/1.1 101 Switching Protocols",
		"Upgrade: websocket",
		"Connection: Upgrade",
	}

	for _, expected := range expectedParts {
		if !strings.Contains(response, expected) {
			t.Errorf("Missing %q in response:\n%s", expected, response)
		}
	}

	// 等待写入完成
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("writeUpgradeResponse returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("writeUpgradeResponse did not complete in time")
	}
}

// TestReadWebSocketUpgradeResponse 测试读取升级响应
func TestReadWebSocketUpgradeResponse(t *testing.T) {
	// 创建管道连接
	conn1, conn2 := net.Pipe()
	defer func() { _ = conn1.Close() }()
	defer func() { _ = conn2.Close() }()

	// 在另一个 goroutine 中写入响应
	go func() {
		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"\r\n"
		_, _ = conn2.Write([]byte(response))
	}()

	// 读取响应
	resp, _, err := readWebSocketUpgradeResponse(conn1, 1*time.Second)
	if err != nil {
		t.Fatalf("readWebSocketUpgradeResponse failed: %v", err)
	}

	if resp.StatusCode != 101 {
		t.Errorf("Expected status 101, got %d", resp.StatusCode)
	}

	if resp.Header.Get("Upgrade") != "websocket" {
		t.Errorf("Expected Upgrade: websocket, got %q", resp.Header.Get("Upgrade"))
	}
}

// TestReadWebSocketUpgradeResponse_Timeout 测试读取超时
func TestReadWebSocketUpgradeResponse_Timeout(t *testing.T) {
	// 创建管道连接但不写入数据
	conn1, conn2 := net.Pipe()
	defer func() { _ = conn1.Close() }()
	defer func() { _ = conn2.Close() }()

	// 使用很短的超时
	_, _, err := readWebSocketUpgradeResponse(conn1, 10*time.Millisecond)
	if err == nil {
		t.Error("Expected timeout error, got nil")
	}
}

// TestReadWebSocketUpgradeResponse_ClearsDeadline 验证握手读取超时不会限制后续帧读取。
func TestReadWebSocketUpgradeResponse_ClearsDeadline(t *testing.T) {
	conn1, conn2 := net.Pipe()
	defer func() { _ = conn1.Close() }()
	defer func() { _ = conn2.Close() }()

	go func() {
		_, _ = conn2.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		time.Sleep(100 * time.Millisecond)
		_, _ = conn2.Write([]byte("x"))
	}()

	_, reader, err := readWebSocketUpgradeResponse(conn1, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("readWebSocketUpgradeResponse() error: %v", err)
	}
	if _, err = reader.ReadByte(); err != nil {
		t.Fatalf("connection retained upgrade deadline: %v", err)
	}
}

// TestDialTarget_TLS 测试 TLS 连接（连接无效端口应失败）
func TestDialTarget_TLS(t *testing.T) {
	// 测试 HTTPS 连接到无效端口
	_, err := dialTarget("https://127.0.0.1:1", 100*time.Millisecond, nil)
	if err == nil {
		t.Error("Expected error for invalid HTTPS address")
	}
}

// TestIsConnectionClosedError_ClosedConn 测试已关闭连接错误
func TestIsConnectionClosedError_ClosedConn(t *testing.T) {
	// 创建并立即关闭连接
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	conn, _ := net.Dial("tcp", ln.Addr().String())
	_ = conn.Close()
	_ = ln.Close()

	// 尝试读取应返回错误
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Error("Expected error reading from closed connection")
	}

	// 验证错误被识别为连接关闭错误
	if !isConnectionClosedError(err) {
		t.Errorf("Expected closed connection error, got: %v", err)
	}
}

// TestWebSocketBridge_LargeMessage 测试大消息转发
func TestWebSocketBridge_LargeMessage(t *testing.T) {
	// 创建管道连接
	client1, client2 := net.Pipe()
	target1, target2 := net.Pipe()
	defer func() { _ = client2.Close() }()
	defer func() { _ = target2.Close() }()

	bridge := NewWebSocketBridge(client1, target1)

	// 启动桥接
	errCh := make(chan error, 1)
	go func() {
		errCh <- bridge.Bridge()
	}()

	// 发送超过 64KB 的数据
	largeData := make([]byte, 100*1024) // 100KB
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	// 客户端发送大消息
	go func() {
		_, _ = client2.Write(largeData)
	}()

	// 后端接收数据
	buf := make([]byte, 150*1024)
	total := 0
	for total < len(largeData) {
		n, err := target2.Read(buf[total:])
		if err != nil {
			t.Fatalf("Failed to read large message: %v", err)
		}
		total += n
	}

	// 验证数据完整性
	for i := range largeData {
		if buf[i] != largeData[i] {
			t.Errorf("Data mismatch at byte %d: got %d, want %d", i, buf[i], largeData[i])
			break
		}
	}

	// 关闭连接
	_ = client2.Close()
	_ = target2.Close()

	// 等待桥接完成
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Bridge returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Bridge did not complete in time")
	}
}

// TestWebSocketBridge_Concurrent 测试并发桥接
func TestWebSocketBridge_Concurrent(t *testing.T) {
	const numBridges = 10

	var wg sync.WaitGroup
	errCh := make(chan error, numBridges)

	for i := range numBridges {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// 创建管道连接
			client1, client2 := net.Pipe()
			target1, target2 := net.Pipe()
			defer func() { _ = client2.Close() }()
			defer func() { _ = target2.Close() }()

			bridge := NewWebSocketBridge(client1, target1)

			// 启动桥接
			done := make(chan error, 1)
			go func() {
				done <- bridge.Bridge()
			}()

			// 发送测试数据
			testData := []byte("concurrent test data")
			go func() {
				_, _ = client2.Write(testData)
			}()

			// 接收数据
			buf := make([]byte, 1024)
			n, err := target2.Read(buf)
			if err != nil {
				errCh <- fmt.Errorf("bridge %d: read error: %v", id, err)
				return
			}

			if string(buf[:n]) != string(testData) {
				errCh <- fmt.Errorf("bridge %d: data mismatch", id)
				return
			}

			// 关闭连接
			_ = client2.Close()
			_ = target2.Close()

			// 等待桥接完成
			select {
			case err := <-done:
				if err != nil {
					errCh <- fmt.Errorf("bridge %d: %v", id, err)
				}
			case <-time.After(1 * time.Second):
				errCh <- fmt.Errorf("bridge %d: timeout", id)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	// 检查错误
	for err := range errCh {
		if err != nil {
			t.Errorf("Concurrent bridge error: %v", err)
		}
	}
}

// TestCopyData_WriteError 测试写入错误处理
func TestCopyData_WriteError(t *testing.T) {
	// 创建管道连接
	src1, src2 := net.Pipe()
	dst1, dst2 := net.Pipe()

	bridge := &WebSocketBridge{}

	// 先关闭目标连接
	_ = dst1.Close()
	_ = dst2.Close()

	// 启动数据复制
	errCh := make(chan error, 1)
	go func() {
		errCh <- bridge.copyData(dst1, src1, "test")
	}()

	// 发送数据（应该触发写入错误）
	_, _ = src2.Write([]byte("test data"))
	_ = src2.Close()

	// 等待完成
	select {
	case err := <-errCh:
		// 写入到已关闭连接应该返回 nil（视为连接关闭错误）
		if err != nil && !strings.Contains(err.Error(), "closed") {
			t.Errorf("copyData returned unexpected error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("copyData did not complete in time")
	}

	_ = src1.Close()
}

// TestReadWebSocketUpgradeResponse_BuffersTrailingData 验证升级响应后紧跟的帧数据不会被丢弃。
func TestReadWebSocketUpgradeResponse_BuffersTrailingData(t *testing.T) {
	conn1, conn2 := net.Pipe()
	defer func() { _ = conn1.Close() }()
	defer func() { _ = conn2.Close() }()

	trailing := []byte("first websocket frame data")
	go func() {
		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"\r\n"
		// 一次性写入响应头和后续帧数据，模拟真实后端在同一 TCP 段发送的场景
		_, _ = conn2.Write(append([]byte(response), trailing...))
	}()

	resp, reader, err := readWebSocketUpgradeResponse(conn1, 1*time.Second)
	if err != nil {
		t.Fatalf("readWebSocketUpgradeResponse failed: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("expected 101, got %d", resp.StatusCode)
	}
	if reader == nil {
		t.Fatal("expected buffered reader")
	}
	if reader.Buffered() != len(trailing) {
		t.Errorf("expected %d buffered trailing bytes, got %d", len(trailing), reader.Buffered())
	}

	// 使用 bufferedConn 消费缓冲区数据
	bc := &bufferedConn{Conn: conn1, reader: reader}
	buf := make([]byte, len(trailing))
	n, err := io.ReadFull(bc, buf)
	if err != nil {
		t.Fatalf("failed to read buffered trailing data: %v", err)
	}
	if !bytes.Equal(buf[:n], trailing) {
		t.Errorf("trailing data mismatch: expected %q, got %q", trailing, buf[:n])
	}
}
