// Package handler 提供 HTTP 请求处理器，包括路由、静态文件服务和零拷贝传输。
//
// 该文件包含 sendfile 的公共代码，供所有平台使用。
//
// 作者：xfy
package handler

import (
	"io"
	"net"
	"os"

	"github.com/valyala/fasthttp"
)

const (
	// MinSendfileSize 使用 sendfile 的最小文件大小（8KB）。
	// 小于该值的文件使用普通 io.Copy，避免系统调用开销。
	MinSendfileSize = 8 * 1024
)

// getNetConn 从 fasthttp.RequestCtx 获取底层 net.Conn。
//
// 明文监听器启用 h2c 嗅探后，连接会被包一层"回放已嗅探字节"的包装；
// sendfile 需要按 *net.TCPConn/*net.UnixConn 取 socket 描述符，因此先解包
// 一层。解包接口只由该嗅探包装实现（TLS 连接走 NetConn 语义不同，不能在此
// 解包，否则会绕过加密）。
//
// 参数：
//   - ctx: fasthttp 请求上下文
//
// 返回值：
//   - net.Conn: 底层网络连接，如果无法获取则返回 nil
func getNetConn(ctx *fasthttp.RequestCtx) net.Conn {
	return unwrapSniffedConn(ctx.Conn())
}

// unwrapSniffedConn 解包 h2c 嗅探包装，返回真正的 socket 连接。
//
// 参数：
//   - conn: 请求上下文里的连接，可能是嗅探包装或 nil
//
// 返回值：
//   - net.Conn: 底层连接；非包装连接与 nil 原样返回
func unwrapSniffedConn(conn net.Conn) net.Conn {
	if u, ok := conn.(underlyingConn); ok {
		return u.UnderlyingConn()
	}
	return conn
}

// underlyingConn 是嗅探包装连接的解包接口。
type underlyingConn interface {
	net.Conn

	// UnderlyingConn 返回被包装的原始连接。
	UnderlyingConn() net.Conn
}

// copyFile 普通文件拷贝（fallback）。
//
// 使用 io.Copy 进行文件传输，适用于不支持 sendfile 的平台或小文件。
//
// 参数：
//   - ctx: fasthttp 请求上下文，作为写入目标
//   - file: 源文件对象
//   - offset: 文件起始偏移量
//   - length: 传输长度，0 表示拷贝到文件末尾
//
// 返回值：
//   - error: 拷贝过程中的错误
func copyFile(ctx *fasthttp.RequestCtx, file *os.File, offset, length int64) error {
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return err
		}
	}

	if length > 0 {
		_, err := io.CopyN(ctx, file, length)
		return err
	}

	_, err := io.Copy(ctx, file)
	return err
}
