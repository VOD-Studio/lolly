// Package server 提供 HTTP 服务器的核心实现，支持单服务器、虚拟主机和多服务器三种运行模式。
//
// 包含服务器生命周期管理相关的逻辑，用于处理启动、停止和信号处理。
//
// 作者：xfy
package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/logging"
)

// cleanupResources 清理服务器资源。
//
// 停止 Goroutine 池、健康检查器，关闭访问日志、TLS 管理器、
// AccessControl 和 Lua 引擎。由 StopWithTimeout 和 GracefulStop 共用。
func (s *Server) cleanupResources() {
	s.cleanupOnce.Do(s.cleanupResourcesOnce)
}

// cleanupResourcesOnce 执行一次服务器资源释放，保证启动失败后仍可安全调用 Stop。
func (s *Server) cleanupResourcesOnce() {
	// 停止 Goroutine 池
	if s.pool != nil {
		s.pool.Stop()
	}

	// 停止健康检查器
	s.proxiesMu.Lock()
	for _, hc := range s.healthCheckers {
		hc.Stop()
	}
	s.proxiesMu.Unlock()

	// 关闭访问日志
	if s.accessLogMiddleware != nil {
		_ = s.accessLogMiddleware.Close()
	}

	// 关闭 TLS 管理器
	if s.tlsManager != nil {
		s.tlsManager.Close()
	}

	// 关闭多服务器模式下创建的子 TLS 管理器
	s.tlsManagersMu.Lock()
	for _, m := range s.tlsManagers {
		m.Close()
	}
	s.tlsManagers = nil
	for _, m := range s.sniManagers {
		m.Close()
	}
	s.sniManagers = nil
	s.tlsManagersMu.Unlock()

	// 关闭 AccessControl (释放 GeoIP 资源)
	s.accessControlsMu.Lock()
	for _, ac := range s.accessControls {
		if err := ac.Close(); err != nil {
			logging.Warn().Err(err).Msg("Failed to close AccessControl")
		}
	}
	s.accessControlsMu.Unlock()

	// 停止 RateLimiter 的后台清理 goroutine
	s.stopRateLimiters()

	// 停止 ACME 证书到期监控
	if s.certMonitor != nil {
		s.certMonitor.Stop()
	}

	// 关闭 Lua 引擎
	if s.luaEngine != nil {
		s.luaEngine.Close()
		logging.Info().Msg("Lua engine closed")
	}
}

// shutdownServers 并行关闭多个 fasthttp.Server 实例。
//
// 使用 goroutine 并行关闭所有服务器，收集所有错误并返回聚合错误。
// 部分服务器关闭失败不会影响其他服务器的关闭。
//
// 参数：
//   - ctx: 关闭上下文，用于控制超时和取消
//   - servers: 要关闭的 fasthttp.Server 实例列表
//
// 返回值：
//   - error: 聚合错误，无错误或全部成功时返回 nil
func shutdownServers(ctx context.Context, servers []*fasthttp.Server) error {
	// 防御性检查：nil ctx 使用默认背景
	if ctx == nil {
		ctx = context.Background()
	}
	if len(servers) == 0 {
		return nil
	}

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)

	for _, srv := range servers {
		if srv == nil {
			continue
		}
		wg.Add(1)
		go func(s *fasthttp.Server) {
			defer wg.Done()
			if err := s.Shutdown(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(srv)
	}

	// 等待所有关闭完成或上下文取消
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		if len(errs) == 0 {
			return nil
		}
		if len(errs) == 1 {
			return errs[0]
		}
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return fmt.Errorf("failed to close servers: %d errors: %s", len(errs), strings.Join(msgs, "; "))
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StopWithTimeout 快速停止服务器（支持自定义超时）。
//
// 立即停止服务器，不等待正在处理的请求完成。
// 停止所有健康检查器和访问日志中间件。
//
// 参数：
//   - timeout: 快速关闭的最大等待时间
//
// 返回值：
//   - error: 停止过程中遇到的错误
//
// 注意事项：
//   - 对于生产环境，建议使用 GracefulStop 实现优雅关闭
//   - timeout <= 0 时会使用默认 5s 超时
func (s *Server) StopWithTimeout(timeout time.Duration) error {
	// 防御性检查：如果 timeout <= 0，使用默认值
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	s.running.Store(false)
	s.cleanupResources()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 多服务器模式：并行关闭所有 fasthttp.Server
	if len(s.fastServers) > 0 {
		return shutdownServers(ctx, s.fastServers)
	}

	// 单服务器模式：关闭单个 fasthttp.Server
	if s.fastServer != nil {
		done := make(chan struct{})
		go func() {
			_ = s.fastServer.Shutdown()
			close(done)
		}()

		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// GracefulStop 优雅停止服务器。
//
// 等待正在处理的请求完成后再停止服务器，确保连接正常关闭。
// 如果超时时间到达仍有请求未完成，将返回超时错误。
//
// 参数：
//   - timeout: 优雅关闭的最大等待时间
//
// 返回值：
//   - error: 停止过程中遇到的错误，超时返回 context.DeadlineExceeded
//
// 注意事项：
//   - 推荐在生产环境使用此方法关闭服务器
//   - 超时后会强制关闭，可能导致部分请求中断
func (s *Server) GracefulStop(timeout time.Duration) error {
	s.running.Store(false)
	s.cleanupResources()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 多服务器模式：并行关闭所有 fasthttp.Server
	if len(s.fastServers) > 0 {
		return shutdownServers(ctx, s.fastServers)
	}

	// 单服务器模式：关闭单个 fasthttp.Server
	if s.fastServer != nil {
		done := make(chan struct{})
		go func() {
			_ = s.fastServer.Shutdown()
			close(done)
		}()

		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// getProxyCacheStats 收集所有代理缓存的统计信息。
func (s *Server) getProxyCacheStats() ProxyCacheStats {
	var total ProxyCacheStats
	s.proxiesMu.RLock()
	for _, p := range s.proxies {
		if stats := p.GetCacheStats(); stats != nil {
			total.Entries += stats.Entries
			total.Pending += stats.Pending
		}
	}
	s.proxiesMu.RUnlock()
	return total
}
