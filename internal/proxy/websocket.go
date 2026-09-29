// Package proxy 提供反向代理功能，支持 HTTP、WebSocket 和流式代理。
//
// 该文件实现了 WebSocket 代理桥接器，用于在客户端和后端服务器之间
// 建立 WebSocket 连接并进行双向数据转发。
//
// 主要功能：
//   - WebSocket 连接升级：处理 HTTP 到 WebSocket 的协议升级
//   - 双向数据转发：在客户端和后端之间透明转发数据帧
//   - TLS 支持：支持 ws:// 和 wss:// 协议
//   - 超时控制：可配置的连接和读写超时
//
// 使用示例：
//
//	err := proxy.WebSocket(ctx, target, 30*time.Second)
//	if err != nil {
//	    log.Printf("WebSocket proxy error: %v", err)
//	}
//
// 作者：xfy
package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/netutil"
)

// wsBufPool WebSocket 数据转发 buffer pool。
// 复用 32KB buffer 避免每次 copyData 调用分配。
var wsBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

// WebSocketBridge WebSocket 桥接器。
//
// 在客户端和后端服务器之间建立双向数据通道，透明转发 WebSocket 数据帧。
// 支持并发读写，使用互斥锁保护关闭状态。
//
// 注意事项：
//   - 调用 Bridge() 会阻塞直到连接关闭
//   - 使用完毕后应调用 Close() 释放资源
type WebSocketBridge struct {
	clientConn net.Conn   // 客户端 TCP 连接
	targetConn net.Conn   // 后端目标 TCP 连接
	mu         sync.Mutex // 保护 closed 字段的互斥锁
	closed     bool       // 连接关闭标志
}

// NewWebSocketBridge 创建新的 WebSocket 桥接器。
//
// 参数：
//   - clientConn: 客户端网络连接
//   - targetConn: 后端目标网络连接
//
// 返回值：
//   - *WebSocketBridge: 初始化的桥接器实例
func NewWebSocketBridge(clientConn, targetConn net.Conn) *WebSocketBridge {
	return &WebSocketBridge{
		clientConn: clientConn,
		targetConn: targetConn,
		closed:     false,
	}
}

// Bridge 启动双向数据转发。
//
// 创建两个 goroutine 分别处理客户端到后端和后端到客户端的数据流，
// 阻塞直到两个方向的转发都完成。
//
// 返回值：
//   - error: 转发过程中的错误（连接正常关闭返回 nil）
func (b *WebSocketBridge) Bridge() error {
	var wg sync.WaitGroup
	wg.Add(2)

	var copyErr1, copyErr2 error

	// 客户端 -> 后端方向
	go func() {
		defer wg.Done()
		copyErr1 = b.copyData(b.clientConn, b.targetConn, "client->target")
	}()

	// 后端 -> 客户端方向
	go func() {
		defer wg.Done()
		copyErr2 = b.copyData(b.targetConn, b.clientConn, "target->client")
	}()

	// 等待双向转发完成
	wg.Wait()

	// 返回第一个非 nil 的错误（忽略连接关闭错误）
	if copyErr1 != nil && !isConnectionClosedError(copyErr1) {
		return copyErr1
	}
	if copyErr2 != nil && !isConnectionClosedError(copyErr2) {
		return copyErr2
	}

	return nil
}

// copyData 在两个连接之间复制数据。
//
// 使用 32KB 缓冲区进行数据拷贝，遇到连接关闭错误时返回 nil。
//
// 参数：
//   - dst: 目标连接（写入端）
//   - src: 源连接（读取端）
//   - direction: 方向描述，用于错误信息
//
// 返回值：
//   - error: 读写错误，连接正常关闭返回 nil
func (b *WebSocketBridge) copyData(dst, src net.Conn, direction string) error {
	bufPtr := wsBufPool.Get().(*[]byte) //nolint:errcheck // pool always returns valid *[]byte
	buf := *bufPtr
	defer wsBufPool.Put(bufPtr)

	for {
		n, err := src.Read(buf)
		if err != nil {
			if isConnectionClosedError(err) {
				return nil
			}
			return fmt.Errorf("read error (%s): %w", direction, err)
		}

		if n > 0 {
			_, err = dst.Write(buf[:n])
			if err != nil {
				if isConnectionClosedError(err) {
					return nil
				}
				return fmt.Errorf("write error (%s): %w", direction, err)
			}
		}
	}
}

// Close 关闭桥接器的两个连接。
//
// 关闭客户端和后端连接，使用互斥锁确保只关闭一次。
//
// 返回值：
//   - error: 关闭过程中的错误
func (b *WebSocketBridge) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()

	var err1, err2 error
	if b.clientConn != nil {
		err1 = b.clientConn.Close()
	}
	if b.targetConn != nil {
		err2 = b.targetConn.Close()
	}

	if err1 != nil {
		return err1
	}
	return err2
}

// isConnectionClosedError 检查错误是否表示连接已关闭。
//
// 判断 EOF、网络超时和使用已关闭连接等正常关闭情况。
//
// 参数：
//   - err: 待检查的错误
//
// 返回值：
//   - bool: true 表示是连接关闭错误
func isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF {
		return true
	}
	if netErr, ok := err.(net.Error); ok {
		// 检查是否为 "use of closed network connection" 错误
		if strings.Contains(err.Error(), "use of closed network connection") {
			return true
		}
		return netErr.Timeout()
	}
	return false
}

// dialTarget 建立到后端目标的 TCP 连接。
//
// 解析目标 URL，支持 HTTP 和 HTTPS 协议，自动添加默认端口。
//
// 参数：
//   - targetURL: 目标 URL（如 http://example.com 或 https://example.com:8443）
//   - timeout: 连接超时时间
//
// 返回值：
//   - net.Conn: 建立的连接（TLS 连接或普通 TCP 连接）
//   - error: 连接失败时返回错误
func dialTarget(targetURL string, timeout time.Duration, proxySSL *config.ProxySSLConfig) (net.Conn, error) {
	// 解析目标 URL
	addr, isTLS := netutil.ParseTargetURL(targetURL, true)

	// 建立 TCP 连接
	dialer := &net.Dialer{
		Timeout: timeout,
	}

	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to target: %w", err)
	}

	if !isTLS {
		return conn, nil
	}

	tlsCfg, err := CreateTLSConfig(proxySSL, addr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if tlsCfg.ServerName == "" {
		tlsCfg.ServerName = host
	}

	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := tlsConn.Handshake(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("TLS handshake failed: %w", err)
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// buildWebSocketUpgradeRequest 构建 WebSocket 升级 HTTP 请求。
//
// 根据客户端请求构建发往后端的 WebSocket 升级请求，
// 复制必要的请求头并添加 X-Forwarded 系列代理头。
//
// 参数：
//   - ctx: FastHTTP 请求上下文
//   - targetHost: 目标主机地址
//   - headersConfig: 代理头配置，控制 X-Forwarded-Host/Proto 的设置
//
// 返回值：
//   - string: 完整的 HTTP 请求字符串
func buildWebSocketUpgradeRequest(ctx *fasthttp.RequestCtx, targetHost string, headersConfig *config.ProxyHeaders) string {
	// 构建请求行
	path := string(ctx.Path())
	if path == "" {
		path = "/"
	}

	// 添加查询参数
	query := string(ctx.QueryArgs().QueryString())
	if query != "" {
		path = path + "?" + query
	}

	// 构建请求头
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", targetHost)

	// 复制原始请求的关键头
	copyHeaders := []string{
		"Upgrade",
		"Connection",
		"Sec-WebSocket-Key",
		"Sec-WebSocket-Version",
		"Sec-WebSocket-Protocol",
		"Sec-WebSocket-Extensions",
		"Origin",
	}

	for _, header := range copyHeaders {
		if value := ctx.Request.Header.Peek(header); len(value) > 0 {
			fmt.Fprintf(&req, "%s: %s\r\n", header, string(value))
		}
	}

	// 添加 X-Forwarded 头
	fh := ExtractForwardedHeaders(ctx)

	// 根据配置决定是否设置 X-Forwarded-Host 和 X-Forwarded-Proto
	setHost := true // 默认值（向后兼容）
	if headersConfig != nil && headersConfig.SetForwardedHost != nil {
		setHost = *headersConfig.SetForwardedHost
	}
	setProto := true // 默认值（向后兼容）
	if headersConfig != nil && headersConfig.SetForwardedProto != nil {
		setProto = *headersConfig.SetForwardedProto
	}

	WriteForwardedHeaders(&req, fh, setHost, setProto)

	// 结束请求头
	req.WriteString("\r\n")

	return req.String()
}

// readWebSocketUpgradeResponse 读取 WebSocket 升级响应。
//
// 从后端连接读取 HTTP 响应，解析响应头和状态码。
//
// 参数：
//   - conn: 后端网络连接
//   - timeout: 读取超时时间
//
// 返回值：
//   - *http.Response: HTTP 响应对象
//   - error: 读取失败时返回错误
func readWebSocketUpgradeResponse(conn net.Conn, timeout time.Duration) (*http.Response, *bufio.Reader, error) {
	// 设置读取超时
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, nil, err
	}

	// 使用 bufio.Reader 读取 HTTP 响应
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read upgrade response: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = resp.Body.Close()
		return nil, nil, err
	}

	return resp, reader, nil
}

// WebSocket 处理 WebSocket 代理请求。
//
// 完整流程：
//  1. 劫持客户端连接
//  2. 建立到后端的 TCP/TLS 连接
//  3. 发送 WebSocket 升级请求
//  4. 验证后端升级响应
//  5. 启动双向数据转发
//
// 参数：
//   - ctx: FastHTTP 请求上下文
//   - target: 负载均衡目标，包含后端 URL
//   - timeout: 连接和 I/O 超时时间
//   - headersConfig: 代理头配置，控制 X-Forwarded-Host/Proto 的设置
//
// 返回值：
//   - error: 代理过程中的错误
func WebSocket(ctx *fasthttp.RequestCtx, target *loadbalance.Target, timeout time.Duration, headersConfig *config.ProxyHeaders, proxySSL *config.ProxySSLConfig) error {
	// 使用 Hijack 获取客户端 TCP 连接
	var clientConn net.Conn

	ctx.Hijack(func(c net.Conn) {
		clientConn = c
	})

	if clientConn == nil {
		return errors.New("failed to hijack connection")
	}

	// 步骤1: 建立到后端目标的连接
	targetConn, err := dialTarget(target.URL, timeout, proxySSL)
	if err != nil {
		_ = clientConn.Close()
		return fmt.Errorf("failed to connect to backend: %w", err)
	}

	// 创建桥接器管理两个连接
	bridge := NewWebSocketBridge(clientConn, targetConn)
	defer func() {
		_ = bridge.Close()
	}()

	// 步骤2: 从目标 URL 提取主机地址
	targetHost := extractHost(target.URL)

	// 步骤3: 构建并发送 WebSocket 升级请求
	upgradeReq := buildWebSocketUpgradeRequest(ctx, targetHost, headersConfig)
	if _, writeErr := targetConn.Write([]byte(upgradeReq)); writeErr != nil {
		return fmt.Errorf("failed to send upgrade request: %w", writeErr)
	}

	// 步骤4: 读取升级响应
	resp, bufferedReader, err := readWebSocketUpgradeResponse(targetConn, timeout)
	if err != nil {
		return fmt.Errorf("failed to read upgrade response: %w", err)
	}

	// 步骤5: 检查响应状态码（期望 101 Switching Protocols）
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// 关闭响应 body（升级失败时）
		_ = resp.Body.Close()
		return fmt.Errorf("backend rejected WebSocket upgrade: %s", resp.Status)
	}

	// 步骤6: 将升级响应发送回客户端
	if err := writeUpgradeResponse(clientConn, resp); err != nil {
		// 关闭响应 body（写入失败时）
		_ = resp.Body.Close()
		return fmt.Errorf("failed to send upgrade response to client: %w", err)
	}

	// 注意: WebSocket 升级成功后，resp.Body 不需要显式关闭
	// 因为底层连接已被 bridge 用于双向数据传输

	// 如果 bufferedReader 已经缓冲了 WebSocket frame 数据，
	// 需要包装连接使后续读取先消耗缓冲区
	if bufferedReader != nil && bufferedReader.Buffered() > 0 {
		targetConn = &bufferedConn{
			Conn:   targetConn,
			reader: bufferedReader,
		}
		bridge.targetConn = targetConn
	}

	// 步骤7: 启动桥接（阻塞直到连接关闭）
	return bridge.Bridge()
}

// extractHost 从 URL 中提取主机地址（带端口）。
//
// 处理 http:// 和 https:// 前缀，自动添加默认端口。
//
// 参数：
//   - url: 完整的 URL 字符串
//
// 返回值：
//   - string: 主机地址（格式 host:port）
func extractHost(url string) string {
	return netutil.ExtractHost(url)
}

// writeUpgradeResponse 将 HTTP 升级响应写回客户端。
//
// 将后端返回的 101 Switching Protocols 响应转发给客户端。
//
// 参数：
//   - conn: 客户端网络连接
//   - resp: HTTP 响应对象
//
// 返回值：
//   - error: 写入失败时返回错误
func writeUpgradeResponse(conn net.Conn, resp *http.Response) error {
	// 构建响应行
	var respStr strings.Builder
	fmt.Fprintf(&respStr, "HTTP/%d.%d %s\r\n", resp.ProtoMajor, resp.ProtoMinor, resp.Status)

	// 写入响应头
	for key, values := range resp.Header {
		for _, value := range values {
			fmt.Fprintf(&respStr, "%s: %s\r\n", key, value)
		}
	}

	respStr.WriteString("\r\n")

	if _, err := conn.Write([]byte(respStr.String())); err != nil {
		return err
	}

	return nil
}

// bufferedConn 包装 net.Conn，优先从 bufio.Reader 的缓冲区读取数据。
//
// 用于 WebSocket 升级响应后，消耗 bufio.Reader 可能已缓冲的 frame 数据。
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

// Read 优先从内部 bufio.Reader 读取，若缓冲区为空则回退到原始连接。
func (bc *bufferedConn) Read(p []byte) (int, error) {
	if bc.reader.Buffered() > 0 {
		return bc.reader.Read(p)
	}
	return bc.Conn.Read(p)
}
