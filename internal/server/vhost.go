// Package server 提供 HTTP 服务器的核心实现，支持单服务器和虚拟主机两种运行模式。
//
// 该文件包含虚拟主机管理相关的核心逻辑，包括：
//   - 虚拟主机管理器的创建和配置
//   - 基于 Host 头的请求分发
//   - 默认主机 fallback 机制
//
// 主机名匹配规则由 internal/hostmatch 提供，TLS SNI 证书选择
// （internal/ssl.SNIManager）使用相同的匹配规则，确保握手阶段选中的
// 证书与请求实际路由到的虚拟主机一致。
//
// 主要用途：
//
//	用于支持多域名虚拟主机场景，根据请求的 Host 头分发到不同的处理器。
//
// 注意事项：
//   - 所有方法均为并发安全
//   - 未匹配的 Host 头请求由默认主机处理
//
// 作者：xfy
package server

import (
	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/hostmatch"
	"rua.plus/lolly/internal/netutil"
	"rua.plus/lolly/internal/utils"
)

// VHostManager 虚拟主机管理器。
//
// 管理多个虚拟主机，根据请求的 Host 头分发到对应的处理器。
// 支持默认主机作为未匹配请求的 fallback。
// 支持精确匹配、前缀通配（*.example.com）、后缀通配（example.*）和正则匹配。
type VHostManager struct {
	matcher *hostmatch.Matcher[*VirtualHost]
}

// VirtualHost 虚拟主机。
//
// 代表一个虚拟主机配置，包含名称和对应的请求处理器。
type VirtualHost struct {
	// handler 请求处理器
	handler fasthttp.RequestHandler

	// name 虚拟主机名称（域名）
	name string
}

// NewVHostManager 创建虚拟主机管理器。
//
// 返回值：
//   - *VHostManager: 新创建的管理器实例
func NewVHostManager() *VHostManager {
	return &VHostManager{matcher: hostmatch.New[*VirtualHost]()}
}

// AddHost 添加虚拟主机。
//
// 支持以下 server_name 格式：
//   - 精确匹配: "example.com"
//   - 前缀通配: "*.example.com"（匹配任意子域名）
//   - 后缀通配: "example.*"（匹配任意 TLD）
//   - 正则匹配: "~regex"（以 ~ 开头，后面是正则表达式）
//
// 参数：
//   - name: 虚拟主机名称（域名）
//   - handler: 请求处理器
//
// 返回值：
//   - error: 正则表达式无效时返回错误
func (v *VHostManager) AddHost(name string, handler fasthttp.RequestHandler) error {
	return v.matcher.Add(name, &VirtualHost{name: name, handler: handler})
}

// SetDefault 设置默认主机。
//
// 参数：
//   - handler: 默认主机的请求处理器
func (v *VHostManager) SetDefault(handler fasthttp.RequestHandler) {
	v.matcher.SetDefault(&VirtualHost{name: "default", handler: handler})
}

// FindHost 根据主机名查找虚拟主机。
//
// 匹配优先级（nginx server_name 规则）：
//  1. 精确匹配
//  2. 最长前缀通配（*.example.com）
//  3. 后缀通配（example.*）
//  4. 正则匹配（按配置顺序）
//  5. 默认主机
//
// 参数：
//   - host: 主机名
//
// 返回值：
//   - *VirtualHost: 匹配的虚拟主机，未匹配返回 nil
func (v *VHostManager) FindHost(host string) *VirtualHost {
	vhost, ok := v.matcher.Find(host)
	if !ok {
		return nil
	}
	return vhost
}

// Handler 返回虚拟主机选择器。
//
// 返回值：
//   - fasthttp.RequestHandler: 根据 Host 头分发请求的处理器
func (v *VHostManager) Handler() fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		host := netutil.StripPort(string(ctx.Host()))

		if vhost := v.FindHost(host); vhost != nil {
			vhost.handler(ctx)
		} else {
			utils.SendError(ctx, utils.ErrNotFound)
		}
	}
}
