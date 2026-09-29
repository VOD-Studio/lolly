// Package server 提供 HTTP 服务器的核心实现。
//
// 该文件包含 ACME 自动证书在服务端的集成逻辑，包括：
//   - 启动阶段为各服务器创建 ACME 管理器
//   - 将 ACME 管理器注入 TLS 管理器（按 SNI 动态签发）
//   - 注册 http-01 挑战响应路由
//
// 主要用途：
//
//	让 lolly 在不依赖 certbot/acme-companion 的情况下，自行完成
//	Let's Encrypt 等 CA 的证书签发与续期。
//
// 注意事项：
//   - TLS 握手与 http-01 挑战必须复用同一个 ACMEManager 实例，
//     否则挑战令牌无法匹配，因此这里统一预创建并按服务器索引保管
//   - tls-alpn-01 挑战无需注册任何 HTTP 路由
//
// 作者：xfy
package server

import (
	"fmt"
	"net"
	"strings"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"rua.plus/lolly/internal/handler"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/matcher"
	"rua.plus/lolly/internal/netutil"
	"rua.plus/lolly/internal/ssl"
)

// initACMEManagers 为所有启用 ACME 的服务器预创建 ACME 管理器。
//
// 预创建是必要的：TLS 管理器与 http-01 挑战路由必须共享同一实例。
// 已配置静态证书的服务器跳过 ACME（静态证书优先）。
//
// 返回值：
//   - error: 任一服务器 ACME 配置无效时返回错误
func (s *Server) initACMEManagers() error {
	if len(s.config.Servers) == 0 {
		return nil
	}

	managers := make([]*ssl.ACMEManager, len(s.config.Servers))
	for i := range s.config.Servers {
		srv := &s.config.Servers[i]
		if !srv.SSL.ACME.Enabled {
			continue
		}
		// 静态证书优先，ACME 被忽略
		if srv.SSL.Cert != "" && srv.SSL.Key != "" {
			continue
		}

		hosts := srv.SSL.ACME.ResolveHosts(srv.ServerNames, srv.Name)
		mgr, err := ssl.NewACMEManager(&srv.SSL.ACME, hosts)
		if err != nil {
			return fmt.Errorf("servers[%d]: 初始化 ACME 失败: %w", i, err)
		}
		if srv.SSL.ACME.AllowDynamicHosts {
			hosts, _ := containerTLSHosts(s.containerSnapshot)
			mgr.SetDynamicHosts(hosts)
		}
		managers[i] = mgr
	}

	s.acmeManagers = managers
	s.warnIfHTTP01Unreachable()
	return nil
}

// startCertMonitor 启动 ACME 证书到期监控。
//
// 把所有启用 ACME（且未使用静态证书）的服务器状态目录交给一个监控实例，
// 由它周期性扫描并输出到期告警。无 ACME 服务器时不启动。
func (s *Server) startCertMonitor() {
	var paths []string
	for i := range s.config.Servers {
		srv := &s.config.Servers[i]
		if !srv.SSL.ACME.Enabled {
			continue
		}
		if srv.SSL.Cert != "" && srv.SSL.Key != "" {
			continue
		}
		path := srv.SSL.ACME.StatePath
		if path == "" {
			path = ssl.DefaultACMEStatePath
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return
	}

	s.certMonitor = ssl.NewCertMonitor(paths)
	s.certMonitor.Start()
}

// warnIfHTTP01Unreachable 在启用 http-01 挑战但没有任何 server 监听 80 端口时告警。
//
// http-01 校验要求 CA 能通过 80 端口访问到挑战文件。常见配置错误是把
// ACME 放在只有 443 的 server 上，导致签发一直失败且难以定位，因此在
// 启动阶段主动提示。
func (s *Server) warnIfHTTP01Unreachable() {
	needsHTTP01 := false
	for _, mgr := range s.acmeManagers {
		if mgr != nil && mgr.HTTP01() {
			needsHTTP01 = true
			break
		}
	}
	if !needsHTTP01 || s.listensOnPort("80") {
		return
	}
	logging.Warn().Msg("ACME 使用 http-01 挑战，但未发现监听 80 端口的 server，CA 校验可能失败（可改用 tls-alpn-01）")
}

// listensOnPort 报告是否有 server 监听指定 TCP 端口。
//
// 参数：
//   - port: 端口号字符串，如 "80"
//
// 返回值：
//   - bool: 存在监听该端口的 server 时返回 true
func (s *Server) listensOnPort(port string) bool {
	for i := range s.config.Servers {
		if listenUsesPort(s.config.Servers[i].Listen, port) {
			return true
		}
	}
	return false
}

// listenUsesPort 判断监听地址是否使用指定端口。
//
// 非 TCP 监听（Unix socket、空地址、格式非法）一律返回 false。
//
// 参数：
//   - listen: 监听地址，如 ":80"、"0.0.0.0:8080"、"unix:/tmp/x.sock"
//   - port: 端口号字符串
//
// 返回值：
//   - bool: 命中时返回 true
func listenUsesPort(listen, port string) bool {
	if listen == "" || strings.HasPrefix(listen, "unix:") {
		return false
	}
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	return p == port
}

// acmeManagerAt 返回指定服务器索引对应的 ACME 管理器。
//
// 参数：
//   - idx: 服务器在 config.Servers 中的索引
//
// 返回值：
//   - *ssl.ACMEManager: ACME 管理器，索引越界或未启用时为 nil
func (s *Server) acmeManagerAt(idx int) *ssl.ACMEManager {
	if idx < 0 || idx >= len(s.acmeManagers) {
		return nil
	}
	return s.acmeManagers[idx]
}

// acmeChallengeHandler 返回处理 ACME http-01 挑战的请求处理器。
//
// 收集所有启用 http-01 的 ACME 管理器，按请求 Host 分派到对应管理器：
// 挑战令牌保存在管理器内存中，必须与签发时使用的是同一实例。
//
// 返回值：
//   - fasthttp.RequestHandler: 挑战处理器；无可用管理器时返回 nil
func (s *Server) acmeChallengeHandler() fasthttp.RequestHandler {
	type entry struct {
		mgr *ssl.ACMEManager
		h   fasthttp.RequestHandler
	}

	var entries []entry
	for _, mgr := range s.acmeManagers {
		if mgr == nil || !mgr.HTTP01() {
			continue
		}
		// 适配器只构造一次，避免每个请求都重新分配
		entries = append(entries, entry{mgr: mgr, h: fasthttpadaptor.NewFastHTTPHandlerFunc(mgr.HTTPHandler(nil).ServeHTTP)})
	}
	if len(entries) == 0 {
		return nil
	}

	return func(ctx *fasthttp.RequestCtx) {
		host := netutil.StripPort(string(ctx.Host()))
		for _, e := range entries {
			if e.mgr.HasHost(host) {
				e.h(ctx)
				return
			}
		}
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		ctx.SetBodyString("Not Found")
	}
}

// registerACMEChallengeLocation 在 LocationEngine 上注册 http-01 挑战路由。
//
// 供单服务器模式使用。无可用 ACME 处理器时不做任何注册。
//
// 参数：
//   - engine: 目标 LocationEngine
//
// 返回值：
//   - error: 注册失败（非路径冲突）时返回错误
func (s *Server) registerACMEChallengeLocation(engine *matcher.LocationEngine) error {
	h := s.acmeChallengeHandler()
	if h == nil {
		return nil
	}
	if err := engine.AddPrefixPriority(acmeChallengePath, h, false); err != nil {
		// 与其它路由冲突时仅告警，不阻断启动
		return s.handleRegistrationError("acme-challenge", acmeChallengePath, err)
	}
	return nil
}

// registerACMEChallengeRouter 在 Router 上注册 http-01 挑战路由。
//
// 供虚拟主机/多服务器模式使用。无可用 ACME 处理器时不做任何注册。
//
// 参数：
//   - router: 目标路由器
func (s *Server) registerACMEChallengeRouter(router *handler.Router) {
	h := s.acmeChallengeHandler()
	if h == nil {
		return
	}
	router.GET(acmeChallengePath+"{token}", h)
}
