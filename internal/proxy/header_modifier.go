// Package proxy 提供反向代理的核心功能，支持请求转发、负载均衡、健康检查等特性。
//
// 包含请求头修改器相关的结构体，用于配置请求和响应头修改规则。
//
// 作者：xfy
package proxy

import (
	"strings"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/netutil"
	"rua.plus/lolly/internal/variable"
)

// modifyRequestHeaders 在转发请求到后端之前修改请求头。
//
// 执行以下操作：
//  1. 将上游请求协议固定为 HTTP/1.1，并清理 hop-by-hop 头部
//     （HTTP/2、HTTP/3 入站请求的协议标识不能透传给 HTTP/1.x 上游）
//  2. 设置 Host header 为目标主机地址
//  3. 提取并设置 X-Forwarded-For、X-Real-IP、X-Forwarded-Host、X-Forwarded-Proto
//  4. 应用自定义请求头配置（支持变量展开）
//  5. 移除配置的请求头
//
// 参数：
//   - ctx: FastHTTP 请求上下文
//   - target: 选中的后端目标
func (p *Proxy) modifyRequestHeaders(ctx *fasthttp.RequestCtx, target *loadbalance.Target) {
	headers := &ctx.Request.Header

	normalizeUpstreamRequest(headers)

	// 覆盖 Host 前提取原始请求信息，避免 X-Forwarded-Host 误用上游地址。
	fh := ExtractForwardedHeaders(ctx)

	// 设置 Host header 为目标主机
	// 从 target.URL 提取 host:port（HostClient 连接需要此格式）
	targetHost, _ := netutil.ParseTargetURL(target.URL, false)
	if targetHost != "" {
		headers.Set("Host", targetHost)
	}

	// 根据配置决定是否设置 X-Forwarded-Host 和 X-Forwarded-Proto
	setHost := true // 默认值（向后兼容）
	if p.config.Headers.SetForwardedHost != nil {
		setHost = *p.config.Headers.SetForwardedHost
	}
	setProto := true // 默认值（向后兼容）
	if p.config.Headers.SetForwardedProto != nil {
		setProto = *p.config.Headers.SetForwardedProto
	}

	SetForwardedHeaders(headers, fh, true, setHost, setProto)
	SetRequestIDHeader(headers, ctx)

	// 从配置设置自定义请求头（支持变量展开）
	if p.config.Headers.SetRequest != nil {
		vc := variable.NewContext(ctx)
		defer variable.ReleaseContext(vc)
		for key, value := range p.config.Headers.SetRequest {
			expanded := vc.Expand(value)
			if containsCRLF(expanded) {
				logging.Warn().Msgf("rejected CRLF in header value: %s", key)
				continue
			}
			headers.Set(key, expanded)
		}
	}

	// 移除配置的请求头
	if len(p.config.Headers.Remove) > 0 {
		for _, key := range p.config.Headers.Remove {
			headers.Del(key)
		}
	}
}

// hopByHopRequestHeaders 是不应转发给上游的 hop-by-hop 请求头（RFC 9110 7.6.1）。
//
// Connection 与 Transfer-Encoding 由 fasthttp 根据请求自行维护，不在此列。
var hopByHopRequestHeaders = []string{
	"Keep-Alive",
	"Proxy-Connection",
	"TE",
	"Upgrade",
}

// normalizeUpstreamRequest 把入站请求规范化为可发往 HTTP/1.x 上游的形式。
//
// 入站请求经 HTTP/2、HTTP/3 适配器转换后，Request.Header 的协议为 "HTTP/2.0" 或
// "HTTP/3"，fasthttp 客户端会把该值原样写进请求行（如 "GET /x HTTP/2.0"），
// 导致 Python http.server 返回 505，fasthttp 上游解析失败，最终表现为 502。
// 同时 fasthttp 会因非 HTTP/1.1 协议而追加 "Connection: close"，丢失上游连接复用。
//
// 处理：
//   - 协议固定为 HTTP/1.1；
//   - 删除 hop-by-hop 头部，以及 Connection 头部中列出的其他头部名。
//
// WebSocket 升级请求在 modifyRequestHeaders 之前已由独立路径处理，不受影响。
func normalizeUpstreamRequest(headers *fasthttp.RequestHeader) {
	headers.SetProtocol("HTTP/1.1")

	if conn := headers.Peek("Connection"); len(conn) > 0 && !headers.ConnectionClose() {
		// Connection 中列出的头部名均为 hop-by-hop，随 Connection 一并清除。
		// 保留 Connection: close 以维持既有的连接关闭语义。
		for _, token := range strings.Split(string(conn), ",") {
			if token = strings.TrimSpace(token); token != "" && !strings.EqualFold(token, "keep-alive") {
				headers.Del(token)
			}
		}
		headers.Del("Connection")
	}

	for _, name := range hopByHopRequestHeaders {
		headers.Del(name)
	}
}

// modifyResponseHeaders 在发送给客户端之前修改响应头。
//
// 应用自定义响应头配置，支持变量展开（如 $upstream_addr、$status 等）。
//
// 参数：
//   - ctx: FastHTTP 请求上下文
func (p *Proxy) modifyResponseHeaders(ctx *fasthttp.RequestCtx) {
	respHeaders := &ctx.Response.Header

	// 构建 PassResponse 集合（多处使用）
	passSet := make(map[string]bool, len(p.config.Headers.PassResponse))
	for _, h := range p.config.Headers.PassResponse {
		passSet[h] = true
	}

	// PassResponse 白名单模式：仅传递列出的头部
	if len(passSet) > 0 {
		var toDelete []string
		for key := range respHeaders.All() {
			// 不在白名单中的应该删除
			if !isInWhitelist(key, passSet) {
				toDelete = append(toDelete, b2s(key))
			}
		}
		for _, k := range toDelete {
			respHeaders.Del(k)
		}
	}

	// HideResponse：移除指定的响应头（PassResponse 优先，跳过已传递的头部）
	for _, key := range p.config.Headers.HideResponse {
		if !passSet[key] {
			respHeaders.Del(key)
		}
	}

	// IgnoreHeaders：从请求和响应中移除（PassResponse 优先）
	for _, key := range p.config.Headers.IgnoreHeaders {
		ctx.Request.Header.Del(key)
		if !passSet[key] {
			respHeaders.Del(key)
		}
	}

	// Cookie 域/路径重写
	if p.config.Headers.CookieDomain != "" || p.config.Headers.CookiePath != "" {
		p.rewriteCookies(respHeaders)
	}

	// 从配置设置自定义响应头（支持变量展开）
	if p.config.Headers.SetResponse != nil {
		vc := variable.NewContext(ctx)
		defer variable.ReleaseContext(vc)
		for key, value := range p.config.Headers.SetResponse {
			expanded := vc.Expand(value)
			if containsCRLF(expanded) {
				logging.Warn().Msgf("rejected CRLF in header value: %s", key)
				continue
			}
			respHeaders.Set(key, expanded)
		}
	}
}

// rewriteCookies 重写响应中 Set-Cookie 头的 domain 和 path。
func (p *Proxy) rewriteCookies(respHeaders *fasthttp.ResponseHeader) {
	cookieDomain := p.config.Headers.CookieDomain
	cookiePath := p.config.Headers.CookiePath
	if cookieDomain == "" && cookiePath == "" {
		return
	}

	cookies := make([]string, 0, respHeaders.Len())
	for _, value := range respHeaders.Cookies() {
		cookie := string(value)
		if cookieDomain != "" {
			cookie = rewriteCookieAttr(cookie, "Domain", cookieDomain)
		}
		if cookiePath != "" {
			cookie = rewriteCookieAttr(cookie, "Path", cookiePath)
		}
		cookies = append(cookies, cookie)
	}

	if len(cookies) > 0 {
		respHeaders.Del("Set-Cookie")
		for _, c := range cookies {
			respHeaders.Add("Set-Cookie", c)
		}
	}
}

// rewriteCookieAttr 替换 Cookie 字符串中指定属性的值（大小写不敏感）。
func rewriteCookieAttr(cookie, attr, newValue string) string {
	prefix := attr + "="
	lower := strings.ToLower(cookie)
	idx := strings.Index(lower, strings.ToLower(prefix))
	if idx == -1 {
		return cookie
	}

	start := idx + len(prefix)
	end := start
	for end < len(cookie) && cookie[end] != ';' && cookie[end] != ' ' {
		end++
	}

	return cookie[:start] + newValue + cookie[end:]
}
