// Package http2 提供明文 HTTP/2（h2c）的连接识别与协议分派。
//
// 该文件实现 RFC 7540 的两种明文接入方式：
//   - prior knowledge（3.4）：在明文监听器上嗅探 HTTP/2 连接前导，命中的连接
//     交给 http2.Server.ServeConn，其余连接把嗅探阶段读走的字节原样回放给
//     fasthttp，使 h2c 与 HTTP/1.1 共享同一端口（见 Wrap）；
//   - Upgrade 握手（3.2）：识别 HTTP/1.1 的 Upgrade: h2c 请求，劫持连接写出
//     101 响应，并按规范把被升级的请求当作 HTTP/2 的流 1 继续服务（见 UpgradeHandler）。
//
// 与 attach.go 的 ALPN 分派互补：TLS 监听器靠 ALPN 协商 h2，明文监听器只能靠
// 连接首字节或 HTTP/1.1 升级头区分协议（fasthttp 的 NextProto 仅在 TLS 路径生效）。
//
// 注意事项：
//   - h2c 是明文协议，只应在可信网络（服务间调用、内部负载均衡）启用
//   - 嗅探命中的 h2c 连接不经过 fasthttp 的 Accept 循环，因此不受 fasthttp 的
//     max_conns_per_ip 约束（多路复用下单连接即可承载全部请求，该限制语义并不适用）；
//     连接总数由 WithMaxConcurrentConns 限制
//   - 升级握手走 fasthttp 的 Hijack，fasthttp 对劫持连接不施加 Concurrency 与
//     读写超时，因此同样计入连接额度
//
// 作者：xfy
package http2

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/valyala/fasthttp"

	"rua.plus/lolly/internal/logging"
)

// h2cPreface 是 HTTP/2 连接前导（RFC 7540 3.5）。
//
// 客户端在未使用 TLS 的端口上以 prior knowledge 发起 HTTP/2 时，连接的第一批
// 字节必须是这 24 字节；服务端据此把 h2c 连接与 HTTP/1.1 请求区分开。
const h2cPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// defaultH2CSniffTimeout 是嗅探连接前导的最长等待时间。
//
// 连上就沉默的半开连接、以及首包慢于该时限的客户端都按 HTTP/1.1 处理：
// 嗅探读到的字节留在缓冲区里回放，由 fasthttp 重新解析（正常请求或 400）。
const defaultH2CSniffTimeout = 5 * time.Second

// h2cUpgradeResponse 是接受 h2c 升级的 101 响应（RFC 7540 3.2）。
//
// 101 必须先于服务端 HTTP/2 连接前导（SETTINGS 帧）写出，Upgrade 与 Connection
// 头部必须带上，客户端据此把连接切换到 HTTP/2 帧格式。
const h2cUpgradeResponse = "HTTP/1.1 101 Switching Protocols\r\n" +
	"Connection: Upgrade\r\n" +
	"Upgrade: h2c\r\n\r\n"

// settingPayloadLen 是 SETTINGS 帧负载里单个参数的字节数（ID 2 字节 + 取值 4 字节）。
//
// HTTP2-Settings 头部的内容就是 SETTINGS 帧负载，长度必须是该值的整数倍。
const settingPayloadLen = 6

// h2cSkipHeaders 是从被升级请求转换到 HTTP/2 流 1 时要去掉的头部名（小写）。
//
// RFC 7540 8.1.2.2 禁止连接特定头部出现在 HTTP/2 请求中，升级握手自身的
// Upgrade/HTTP2-Settings 也同理：留着它们会被原样转发给上游，造成协议错误。
// Host 与 Content-Length 已由 http.Request 的对应字段承载，不能再进头部表。
var h2cSkipHeaders = [...]string{"host", "connection", "upgrade", "http2-settings", "proxy-connection", "transfer-encoding", "content-length"}

// keepAliveSetter 是 TCP/Unix 套接字连接支持的 keepalive 设置能力。
//
// fasthttp 在 accept 后通过该接口设置 TCP keepalive（见其 connKeepAliveer）；
// 嗅探包装连接必须转发这两个方法，否则被嗅探过的 HTTP/1.1 连接会静默失去
// keepalive，空闲连接只能等 fasthttp 自身的超时兜底。
type keepAliveSetter interface {
	SetKeepAlive(keepalive bool) error
	SetKeepAlivePeriod(d time.Duration) error
}

// sniffedConn 包装 net.Conn，使嗅探阶段读走的字节可以被重新读出。
//
// fasthttp 与 http2.Server 都从连接首字节开始解析协议，嗅探消耗的前导必须
// 回放，否则 HTTP/1.1 的请求行和 HTTP/2 的前导都会丢失。
type sniffedConn struct {
	net.Conn

	// br 持有嗅探时已读取但尚未被协议处理器消费的字节。
	br *bufio.Reader
}

// newSniffedConn 创建带前导回放能力的连接包装。
//
// 参数：
//   - conn: 底层连接
//   - br: 嗅探时使用、仍保留已读字节的缓冲区
//
// 返回值：
//   - *sniffedConn: 连接包装
func newSniffedConn(conn net.Conn, br *bufio.Reader) *sniffedConn {
	return &sniffedConn{Conn: conn, br: br}
}

// Read 优先返回嗅探阶段缓冲的字节，读完继续读底层连接。
func (c *sniffedConn) Read(p []byte) (int, error) {
	return c.br.Read(p)
}

// UnderlyingConn 返回被包装的原始连接。
//
// 与 tls.Conn.NetConn 不同，该包装只出现在不使用 TLS 的监听器上，因此解包
// 不会绕过加密；它让 sendfile 这类需要按 *net.TCPConn/*net.UnixConn 取 socket
// 文件描述符的路径在嗅探之后仍然可用。
//
// 返回值：
//   - net.Conn: 原始连接
func (c *sniffedConn) UnderlyingConn() net.Conn {
	return c.Conn
}

// SetKeepAlive 把 keepalive 开关转发给底层套接字。
//
// 底层连接不支持该能力时返回 nil：fasthttp 在报错时会直接关闭连接，而“不支持”
// 并不是错误，交给连接自身的默认行为即可。
//
// 参数：
//   - keepalive: 是否开启 TCP keepalive
//
// 返回值：
//   - error: 转发失败时返回错误
func (c *sniffedConn) SetKeepAlive(keepalive bool) error {
	if ka, ok := c.Conn.(keepAliveSetter); ok {
		return ka.SetKeepAlive(keepalive)
	}
	return nil
}

// SetKeepAlivePeriod 把 keepalive 间隔转发给底层套接字，语义同 SetKeepAlive。
//
// 参数：
//   - d: keepalive 探测间隔
//
// 返回值：
//   - error: 转发失败时返回错误
func (c *sniffedConn) SetKeepAlivePeriod(d time.Duration) error {
	if ka, ok := c.Conn.(keepAliveSetter); ok {
		return ka.SetKeepAlivePeriod(d)
	}
	return nil
}

// ReadFrom 转发 io.ReaderFrom，保留 fasthttp 在明文连接上的零拷贝响应发送。
//
// fasthttp 的 copyZeroAlloc 只在 writer 是 *net.TCPConn 或 io.ReaderFrom 时用
// sendfile；包装连接若不实现该接口，静态文件响应会退化为逐块拷贝。
// bufio.Writer 在调用底层 ReadFrom 前已自行 Flush，因此不会打乱输出顺序。
//
// 参数：
//   - r: 数据来源（通常是 *os.File）
//
// 返回值：
//   - int64: 写入的字节数
//   - error: 拷贝失败时返回错误
func (c *sniffedConn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(c.Conn, r)
}

// sniffH2C 嗅探连接是否以 HTTP/2 前导开头。
//
// 无论命中与否，返回值中的连接都能重新读出被嗅探消耗的字节；嗅探期间设置的
// 读超时在返回前恢复为零值，由后续协议处理器自行管理。
//
// 参数：
//   - conn: 待嗅探的连接
//   - timeout: 嗅探最长等待时间，<=0 时使用 defaultH2CSniffTimeout
//
// 返回值：
//   - isH2C: true 表示连接以 HTTP/2 前导开头
//   - *sniffedConn: 可回放已读字节的连接包装
//   - error: 仅设置读写超时失败等不可恢复错误；短读、嗅探超时都按 HTTP/1.1 处理
func sniffH2C(conn net.Conn, timeout time.Duration) (bool, *sniffedConn, error) {
	if timeout <= 0 {
		timeout = defaultH2CSniffTimeout
	}

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return false, nil, err
	}

	// Peek 只填充缓冲区、不消费，读不满前导长度时返回已缓冲字节与错误。
	br := bufio.NewReaderSize(conn, len(h2cPreface))
	peeked, err := br.Peek(len(h2cPreface))
	if derr := conn.SetReadDeadline(time.Time{}); derr != nil {
		return false, nil, derr
	}
	if err != nil {
		return false, newSniffedConn(conn, br), nil
	}

	return string(peeked) == h2cPreface, newSniffedConn(conn, br), nil
}

// h2c 升级握手检查用到的 token，预先生成以避免每请求分配。
var (
	h2cUpgradeToken   = []byte("h2c")
	h2cSettingsOption = []byte("HTTP2-Settings")
)

// hasHeaderToken 判断逗号分隔的头部值中是否含指定 token（大小写不敏感）。
//
// 参数：
//   - value: 头部原始值
//   - token: 目标 token
//
// 返回值：
//   - bool: 列表中包含该 token 时返回 true
func hasHeaderToken(value, token []byte) bool {
	for {
		comma := bytes.IndexByte(value, ',')
		part := value
		if comma >= 0 {
			part = value[:comma]
		}
		if bytes.EqualFold(bytes.TrimSpace(part), token) {
			return true
		}
		if comma < 0 {
			return false
		}
		value = value[comma+1:]
	}
}

// h2cUpgradeSettings 校验 h2c 升级请求并解析 HTTP2-Settings 头部。
//
// 按 RFC 7540 3.2 与 3.2.1：Upgrade 必须含 h2c token，Connection 必须把
// HTTP2-Settings 声明为连接选项（否则该头部会被中间设备转发掉），
// HTTP2-Settings 必须恰好出现一次且内容为 base64url 编码的 SETTINGS 帧负载。
//
// 参数：
//   - req: 待校验的 HTTP/1.1 请求
//
// 返回值：
//   - settings: SETTINGS 帧负载，未携带参数时为 nil
//   - ok: false 表示这不是（或不是合法的）h2c 升级请求
func h2cUpgradeSettings(req *fasthttp.Request) (settings []byte, ok bool) {
	h := &req.Header
	// 先用两个廉价查找挡掉绝大多数普通请求，再做 token 与 base64 校验。
	upgrade := h.Peek("Upgrade")
	if len(upgrade) == 0 || !h.ConnectionUpgrade() || !hasHeaderToken(upgrade, h2cUpgradeToken) {
		return nil, false
	}
	if !hasHeaderToken(h.Peek("Connection"), h2cSettingsOption) {
		return nil, false
	}

	var raw []byte
	count := 0
	for key, value := range h.All() {
		if !strings.EqualFold(string(key), "http2-settings") {
			continue
		}
		count++
		if count == 1 {
			raw = append([]byte(nil), value...)
		}
	}
	// "恰好一个"是 MUST：多份 SETTINGS 无法确定用哪份，退回普通请求。
	if count != 1 {
		return nil, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(string(raw))
	if err != nil || len(payload)%settingPayloadLen != 0 {
		return nil, false
	}
	if len(payload) == 0 {
		return nil, true
	}
	return payload, true
}

// h2cUpgradeRequest 把被升级的 HTTP/1.1 请求转换成 ServeConn 需要的 http.Request。
//
// 字段必须逐项复制：ctx 及其请求缓冲区在处理器返回后即被 fasthttp 回收，
// 而升级出的 HTTP/2 连接会在之后很长时间里继续使用这份请求。
//
// 参数：
//   - ctx: 升级请求上下文
//
// 返回值：
//   - *http.Request: 等价的 HTTP/2 请求（将作为流 1 处理）
//   - error: 请求 URI 无法解析时返回错误
func h2cUpgradeRequest(ctx *fasthttp.RequestCtx) (*http.Request, error) {
	target := string(ctx.URI().RequestURI())
	u, err := url.ParseRequestURI(target)
	if err != nil {
		return nil, err
	}

	host := string(ctx.Request.Header.Host())
	if host == "" {
		host = string(ctx.URI().Host())
	}

	header := make(http.Header)
	for key, value := range ctx.Request.Header.All() {
		name := string(key)
		skip := false
		for _, drop := range h2cSkipHeaders {
			if strings.EqualFold(name, drop) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		header.Add(name, string(value))
	}

	body := ctx.Request.Body()
	req := &http.Request{
		Method:        string(ctx.Request.Header.Method()),
		URL:           u,
		RequestURI:    target,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		Header:        header,
		Host:          host,
		ContentLength: int64(len(body)),
		RemoteAddr:    ctx.RemoteAddr().String(),
		Body:          http.NoBody,
	}
	if len(body) > 0 {
		// 请求体已由 fasthttp 完整读出：HTTP/2 帧只能在升级之后开始，
		// 留在连接里的字节会被当成非法前导。
		req.Body = io.NopCloser(bytes.NewReader(append([]byte(nil), body...)))
	}

	return req, nil
}

// parseH2CUpgrade 判定并解析当前请求是否为可服务的 h2c 升级请求。
//
// 带流式请求体的请求不参与升级：请求体尚未读完时连接里剩下的仍是 HTTP/1.1
// 字节，无法直接交给 HTTP/2 帧解析器。
//
// 参数：
//   - ctx: 请求上下文
//
// 返回值：
//   - settings: HTTP2-Settings 携带的 SETTINGS 负载（可为 nil）
//   - req: 作为流 1 处理的等价请求
//   - ok: false 表示应按普通 HTTP/1.1 请求处理
func parseH2CUpgrade(ctx *fasthttp.RequestCtx) (settings []byte, req *http.Request, ok bool) {
	settings, valid := h2cUpgradeSettings(&ctx.Request)
	if !valid {
		return nil, nil, false
	}
	if ctx.Request.IsBodyStream() {
		return nil, nil, false
	}

	req, err := h2cUpgradeRequest(ctx)
	if err != nil {
		logging.Warn().Err(err).Msg("HTTP/2 (h2c) upgrade request has an unparsable URI: served as HTTP/1.1")
		return nil, nil, false
	}

	return settings, req, true
}

// UpgradeHandler 返回拦截 h2c Upgrade 握手的处理器包装。
//
// 非升级请求原样交给 handler；合法的升级请求按 RFC 7540 3.2 把被升级的请求
// 视为 HTTP/2 的流 1（客户端不会重发它），劫持连接写出 101 响应后在同一连接
// 上继续按 HTTP/2 服务。校验不通过的升级请求退回普通 HTTP/1.1 处理，与不
// 支持 h2c 的实现行为一致。
//
// 该包装应位于处理器链最外层（在中间件之前）：升级握手不是业务请求，
// 不该计入访问日志、限流或压缩。必须在 fasthttp.Server.Serve 之前替换 Handler，
// 且只对明文监听器有意义。
//
// 参数：
//   - handler: 原始请求处理器
//
// 返回值：
//   - fasthttp.RequestHandler: 带 h2c 升级能力的处理器
func (s *Server) UpgradeHandler(handler fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		settings, req, ok := parseH2CUpgrade(ctx)
		if !ok {
			handler(ctx)
			return
		}

		// 101 与后续 HTTP/2 帧都要直接写到 socket 上，不能让 fasthttp 再发
		// 一份响应；劫持后的连接由本处理器独占，直到 ServeConn 返回。
		ctx.HijackSetNoResponse(true)
		ctx.Hijack(func(c net.Conn) {
			s.serveUpgradedConn(c, req, settings)
		})
	}
}

// serveUpgradedConn 在劫持得到的连接上写出 101 响应并服务 HTTP/2。
//
// 参数：
//   - c: 劫持后的连接（读会先排空 fasthttp 已缓冲的字节）
//   - req: 被升级的 HTTP/1.1 请求，作为流 1 处理
//   - settings: HTTP2-Settings 携带的 SETTINGS 负载
func (s *Server) serveUpgradedConn(c net.Conn, req *http.Request, settings []byte) {
	if !s.acquireConnSlot() {
		// 额度不足时不写 101：连接按 HTTP/1.1 结束，客户端可继续用 HTTP/1.1。
		logging.Warn().
			Str("remote_addr", c.RemoteAddr().String()).
			Msg("HTTP/2 (h2c) upgrade rejected: max connections reached")
		return
	}
	defer s.releaseConnSlot()

	if _, err := c.Write([]byte(h2cUpgradeResponse)); err != nil {
		logging.Error().Err(err).Msg("HTTP/2 (h2c) failed to write 101 switching protocols response")
		return
	}

	// ServeConn 会读走客户端在 101 之后发出的连接前导，无需嗅探或回放。
	s.serveHTTP2WithUpgrade(c, req, settings)
}

// h2cListener 嗅探 HTTP/2 前导的监听器包装。
//
// Accept 只把 HTTP/1.1 连接交给调用方（通常是 fasthttp.Server.Serve），
// 命中前导的 h2c 连接由内部 Server 起 goroutine 服务，两者共用同一个
// Accept 循环，避免两个循环争抢同一监听器。
type h2cListener struct {
	net.Listener

	// server 处理命中前导的 HTTP/2 连接。
	server *Server
}

// Accept 返回下一个 HTTP/1.1 连接，期间分派所有命中前导的 h2c 连接。
//
// 返回值：
//   - net.Conn: 可回放嗅探字节的 HTTP/1.1 连接
//   - error: 底层监听器错误或服务器已停止时返回错误
func (l *h2cListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		select {
		case <-l.server.stopChan:
			_ = conn.Close()
			return nil, net.ErrClosed
		default:
		}

		isH2C, sniffed, err := sniffH2C(conn, l.server.sniffTimeout)
		if err != nil {
			logging.Error().Err(err).Msg("HTTP/2 (h2c) preface sniff error")
			_ = conn.Close()
			continue
		}
		if !isH2C {
			return sniffed, nil
		}

		l.server.serveH2CConn(sniffed)
	}
}

// Wrap 返回嗅探 h2c 前导的监听器包装，供 fasthttp.Server.Serve 使用。
//
// 命中前导的连接由本 Server 直接交给 http2.Server.ServeConn，其余连接原样
// 回放给 fasthttp，因此监听器与连接生命周期仍由 fasthttp 持有：热升级的
// FD 继承、优雅关闭、超时与 keepalive 处理都不需要额外适配。
//
// Wrap 与 Serve 互斥（同一 Server 只用其中一种驱动方式）；包装后的监听器
// 必须交给 fasthttp.Server.Serve 才开始接受连接。Stop 在 Wrap 模式下只取消
// 在役 h2c 连接，不关闭监听器（监听器由 fasthttp 关闭）。
//
// 参数：
//   - ln: 原始明文监听器
//
// 返回值：
//   - net.Listener: 带 h2c 嗅探分派的监听器包装
func (s *Server) Wrap(ln net.Listener) net.Listener {
	s.mu.Lock()
	if !s.running {
		s.running = true
	}
	s.mu.Unlock()

	return &h2cListener{Listener: ln, server: s}
}

// serveH2CConn 在后台服务一个 h2c 连接，并纳入关闭流程。
//
// 该路径不经过 handleConnection，需要自行登记连接池与 WaitGroup，
// Stop 才能取消上下文并等待收尾。
//
// 参数：
//   - conn: 已确认以 HTTP/2 前导开头的连接
func (s *Server) serveH2CConn(conn net.Conn) {
	if !s.acquireConnSlot() {
		logging.Warn().
			Str("remote_addr", conn.RemoteAddr().String()).
			Msg("HTTP/2 (h2c) connection rejected: max connections reached")
		_ = conn.Close()
		return
	}

	key := conn.RemoteAddr().String()
	s.pool.add(key, conn)
	s.connWg.Add(1)

	go func() {
		defer func() {
			s.pool.remove(key, conn)
			s.connWg.Done()
			s.releaseConnSlot()
			// Stop 已经通过连接池关闭过连接时，这里会得到 ErrClosed：
			// 属正常收尾，不应作为错误上报。
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				logging.Error().Err(err).Msg("HTTP/2 connection close error")
			}
		}()

		s.serveHTTP2(conn)
	}()
}

// acquireConnSlot 占用一个 h2c 连接额度。
//
// maxConns <= 0 表示不限制，与 fasthttp 的 Concurrency 零值语义一致。
//
// 返回值：
//   - bool: true 表示额度可用
func (s *Server) acquireConnSlot() bool {
	if s.maxConns <= 0 {
		return true
	}
	if s.activeConns.Add(1) > int64(s.maxConns) {
		s.activeConns.Add(-1)
		return false
	}
	return true
}

// releaseConnSlot 释放一个 h2c 连接额度。
func (s *Server) releaseConnSlot() {
	if s.maxConns > 0 {
		s.activeConns.Add(-1)
	}
}
