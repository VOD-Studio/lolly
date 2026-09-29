// Package proxy 反向代理包，为 Lolly HTTP 服务器提供反向代理功能。
//
// 该文件实现 DNS 动态解析和刷新功能，支持后端目标的域名自动解析、
// IP 缓存、定时刷新和故障恢复。
//
// 主要功能：
//   - DNS 解析器集成：支持自定义 resolver 实现域名解析
//   - 定时刷新循环：根据 TTL 自动刷新已解析目标的 IP 地址
//   - 连接池同步更新：DNS 解析结果自动同步到 HostClient 连接池
//   - 统计信息查询：暴露 DNS 解析器的运行统计数据
//
// 注意事项：
//   - 所有公开方法均为并发安全
//   - DNS 刷新在后台 goroutine 中运行，通过 stopCh 控制生命周期
//
// 作者：xfy
package proxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/resolver"
)

// SetResolver 设置 DNS 解析器。
//
// 该方法为代理实例配置自定义的 DNS 解析器，用于动态解析后端目标的域名。
// 必须在调用 Start() 之前设置，否则 DNS 刷新循环不会启动。
//
// 参数：
//   - r: DNS 解析器实例，需实现 resolver.Resolver 接口
func (p *Proxy) SetResolver(r resolver.Resolver) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resolver = r
}

// Start 启动代理服务，包括 DNS 刷新循环。
//
// 该方法标记代理为已启动状态，如果配置了 resolver，则启动解析器并
// 在后台 goroutine 中启动 DNS 定时刷新循环。该方法是幂等的，
// 重复调用不会重复启动。
//
// 返回值：
//   - error: 启动 resolver 失败时返回非 nil 错误
func (p *Proxy) Start() error {
	if p.started.Load() {
		return nil
	}

	p.started.Store(true)

	// 启动 DNS 刷新循环（如果配置了 resolver）
	if p.resolver != nil {
		if err := p.resolver.Start(); err != nil {
			p.started.Store(false)
			return fmt.Errorf("failed to start resolver: %w", err)
		}
		p.dnsWG.Add(1)
		go func() {
			defer p.dnsWG.Done()
			p.startDNSRefreshLoop()
		}()
	}

	return nil
}

// Close 停止后台刷新并关闭当前连接池中的空闲连接。
//
// 已在途请求仍持有 HostClient，可继续完成；重复调用不会再次关闭通道。
func (p *Proxy) Close() {
	p.closeOnce.Do(func() {
		close(p.stopCh)
		p.dnsWG.Wait()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, client := range p.clients {
			client.CloseIdleConnections()
		}
	})
}

// startDNSRefreshLoop 启动 DNS 刷新后台循环。
//
// 根据 resolver 的 TTL 计算刷新间隔（TTL / 2，最小 1 秒），
// 定时调用 refreshDNS 刷新所有需要解析的目标。
// 该方法阻塞运行，直到收到 stopCh 信号。
func (p *Proxy) startDNSRefreshLoop() {
	if p.resolver == nil {
		return
	}

	ttl := p.getResolverTTL()
	if ttl == 0 {
		ttl = 30 * time.Second
	}

	// 刷新间隔为 TTL / 2
	interval := max(ttl/2, time.Second)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.refreshDNS()
		case <-p.stopCh:
			return
		}
	}
}

// refreshDNS 刷新所有需要解析的目标。
//
// 遍历所有后端目标，对超过 TTL 的域名执行 DNS 查询，
// 更新目标的已解析 IP 列表，并同步更新对应 HostClient 的地址。
func (p *Proxy) refreshDNS() {
	if p.resolver == nil {
		return
	}

	ttl := p.getResolverTTL()

	p.mu.RLock()
	targets := p.targets
	p.mu.RUnlock()

	for _, target := range targets {
		if !target.NeedsResolve(ttl) {
			continue
		}

		hostname := target.Hostname()
		if hostname == "" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ips, err := p.resolver.LookupHostWithCache(ctx, hostname)
		cancel()

		if err != nil {
			logging.Debug().Msgf("DNS refresh failed for %s: %v", hostname, err)
			continue
		}

		if len(ips) > 0 {
			target.SetResolvedIPs(ips)
			p.updateHostClientAddr(target, ips[0])
		}
	}
}

// updateHostClientAddr 更新 HostClient 的连接地址。
//
// 从目标 URL 中解析出端口，与新的 IP 地址组合后创建新的 HostClient
// 并替换连接池中的旧实例。旧连接不受影响，新连接将使用新地址。
// 通过重建 client 而不是直接修改 Addr 字段，避免与正在使用旧 client
// 的 goroutine 产生数据竞争。
//
// 参数：
//   - target: 负载均衡目标，包含原始 URL
//   - ip: 新解析出的 IP 地址
func (p *Proxy) updateHostClientAddr(target *loadbalance.Target, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 从 URL 解析出端口
	u, err := url.Parse(target.URL)
	if err != nil {
		return
	}

	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		// 没有端口，使用默认端口
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	newAddr := net.JoinHostPort(ip, port)

	// 与 getClient 保持一致的 key 计算逻辑
	key := target.URL
	if p.config.ProxyBind != "" {
		key = target.URL + "|" + p.config.ProxyBind
	}

	oldClient, ok := p.clients[key]
	if !ok {
		return
	}

	// 创建新的 HostClient，复制旧 client 的配置，仅修改 Addr
	// 新连接将使用新 IP，旧连接继续使用旧 IP 直到超时
	newClient := &fasthttp.HostClient{
		Addr:                   newAddr,
		IsTLS:                  oldClient.IsTLS,
		ReadTimeout:            oldClient.ReadTimeout,
		WriteTimeout:           oldClient.WriteTimeout,
		MaxIdleConnDuration:    oldClient.MaxIdleConnDuration,
		MaxConns:               oldClient.MaxConns,
		MaxConnWaitTimeout:     oldClient.MaxConnWaitTimeout,
		DisablePathNormalizing: oldClient.DisablePathNormalizing,
		SecureErrorLogMessage:  oldClient.SecureErrorLogMessage,
		Dial:                   oldClient.Dial,
		StreamResponseBody:     oldClient.StreamResponseBody,
		ReadBufferSize:         oldClient.ReadBufferSize,
		WriteBufferSize:        oldClient.WriteBufferSize,
		TLSConfig:              oldClient.TLSConfig,
	}

	// 兼容旧版 RetryIf 配置，转换为 RetryIfErr。
	// RetryIf 已被 fasthttp 标记为 deprecated，新连接使用 RetryIfErr 替代。
	//nolint:staticcheck // 需要读取旧 client 的 deprecated RetryIf 以兼容已有配置。
	if oldClient.RetryIf != nil {
		//nolint:staticcheck // 同上，读取旧 client 的 deprecated RetryIf。
		oldRetryIf := oldClient.RetryIf
		newClient.RetryIfErr = func(req *fasthttp.Request, _ int, _ error) (bool, bool) {
			return false, oldRetryIf(req)
		}
	}

	p.clients[key] = newClient
	oldClient.CloseIdleConnections()
	logging.Debug().Msgf("Updated HostClient addr for %s to %s", target.URL, newAddr)
}

// getResolverTTL 获取 DNS 解析记录的过期时间。
//
// 返回 resolver 的 TTL 配置，默认值为 30 秒。
// 该方法当前返回固定值，未来可从 resolver stats 中动态推断。
//
// 返回值：
//   - time.Duration: DNS 记录的有效期
func (p *Proxy) getResolverTTL() time.Duration {
	if p.resolver == nil {
		return 0
	}

	// 从 stats 中推断 TTL（如果实现了相应接口）
	// 这里返回默认值
	return 30 * time.Second
}
