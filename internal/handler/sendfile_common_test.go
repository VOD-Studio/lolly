// Package handler 提供 sendfile 跨平台公共逻辑的测试。
//
// 该文件验证嗅探包装连接的解包逻辑（sendfile 需按 *net.TCPConn 等
// 具体类型取 socket 描述符），与平台无关。
//
// 作者：xfy
package handler

import (
	"net"
	"testing"
)

// wrappedConn 模拟 h2c 嗅探包装：实现 UnderlyingConn 解包接口。
type wrappedConn struct {
	net.Conn

	// inner 是被包装的原始连接。
	inner net.Conn
}

// UnderlyingConn 返回被包装的原始连接。
func (c *wrappedConn) UnderlyingConn() net.Conn {
	return c.inner
}

// TestUnwrapSniffedConn 验证嗅探包装被解包、普通连接与 nil 原样返回。
func TestUnwrapSniffedConn(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	wrapped := &wrappedConn{Conn: server, inner: client}
	if got := unwrapSniffedConn(wrapped); got != client {
		t.Error("unwrapSniffedConn() 应解包嗅探包装为底层连接")
	}

	if got := unwrapSniffedConn(server); got != server {
		t.Error("unwrapSniffedConn() 对普通连接应原样返回")
	}

	if got := unwrapSniffedConn(nil); got != nil {
		t.Errorf("unwrapSniffedConn() 对 nil 应返回 nil，实际 %v", got)
	}
}
