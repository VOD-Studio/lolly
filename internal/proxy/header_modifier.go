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
//  1. 设置 Host header 为目标主机地址
//  2. 提取并设置 X-Forwarded-For、X-Real-IP、X-Forwarded-Host、X-Forwarded-Proto
//  3. 应用自定义请求头配置（支持变量展开）
//  4. 移除配置的请求头
//
// 参数：
//   - ctx: FastHTTP 请求上下文
//   - target: 选中的后端目标
func (p *Proxy) modifyRequestHeaders(ctx *fasthttp.RequestCtx, target *loadbalance.Target) {
	headers := &ctx.Request.Header

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
