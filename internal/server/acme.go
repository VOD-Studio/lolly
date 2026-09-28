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

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"rua.plus/lolly/internal/handler"
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
		managers[i] = mgr
	}

	s.acmeManagers = managers
	return nil
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

// acmeManagerMap 返回"服务器索引 → ACME 管理器"的映射。
//
// 仅包含启用 ACME 的服务器，供 SNI 管理器按索引取用。
//
// 返回值：
//   - map[int]*ssl.ACMEManager: 索引到 ACME 管理器的映射
func (s *Server) acmeManagerMap() map[int]*ssl.ACMEManager {
	out := make(map[int]*ssl.ACMEManager, len(s.acmeManagers))
	for i, mgr := range s.acmeManagers {
		if mgr != nil {
			out[i] = mgr
		}
	}
	return out
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
