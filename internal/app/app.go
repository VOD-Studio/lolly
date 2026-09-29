//go:build !windows

// Package app 提供 Lolly 应用程序的生命周期管理和命令行入口。
//
// 包含应用生命周期管理和命令行入口相关的逻辑。
//
// 作者：xfy
package app

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/server"
)

// Run starts the application: loads config, creates servers, and handles signals.
func (a *App) Run() int {
	if err := a.loadAndValidateConfig(); err != nil {
		return 1
	}

	a.initVariables()

	// Inherit parent listeners when running as a graceful upgrade child.
	a.inheritListeners()

	a.logServerAddresses()
	a.initResolver()
	a.initServer()
	a.initStreamServers()
	a.initHTTP3()

	a.upgradeMgr = server.NewUpgradeManager(a.srv)
	a.srv.SetUpgradeManager(a.upgradeMgr)
	if a.pidFile != "" {
		a.upgradeMgr.SetPidFile(a.pidFile)
		_ = a.upgradeMgr.WritePid()
	}

	a.signalReady()

	sigChan := make(chan os.Signal, 1)
	a.setupSignalHandlers(sigChan)

	errChan := make(chan error, 1)
	go func() {
		a.logger.LogStartup("Starting HTTP server", nil)
		if err := a.srv.Start(); err != nil {
			errChan <- err
		}
	}()

	sigintCount := 0

	for {
		select {
		case err := <-errChan:
			a.logger.Error().Err(err).Msg("Server failed to start")
			return 1
		case sig := <-sigChan:
			if sig == syscall.SIGINT {
				sigintCount++
				if sigintCount >= 3 {
					a.logger.LogShutdown("Received 3 SIGINT, forcing exit")
					return 1
				}
			}
			if !a.handleSignal(sig) {
				a.logger.LogShutdown("Server stopped")
				return 0
			}
		}
	}
}

// inheritListeners inherits parent listeners during graceful upgrade.
func (a *App) inheritListeners() {
	if os.Getenv("GRACEFUL_UPGRADE") == "1" {
		a.logger.LogStartup("Graceful upgrade mode detected, inheriting parent listeners", nil)
		a.upgradeMgr = server.NewUpgradeManager(nil)
		listeners, err := a.upgradeMgr.GetInheritedListeners()
		if err == nil && len(listeners) > 0 {
			a.listeners = listeners
		}
	}
}

func (a *App) setupSignalHandlers(sigChan chan<- os.Signal) {
	signal.Notify(sigChan,
		syscall.SIGTERM,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGHUP,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
	)
}

// handleSignal returns false to indicate the app should exit.
func (a *App) handleSignal(sig os.Signal) bool {
	if a.cfg == nil {
		a.logger.Error().Msg("Signal handling failed: config is nil, using default timeout")
		a.cfg = &config.Config{
			Shutdown: config.ShutdownConfig{
				GracefulTimeout: 30 * time.Second,
				FastTimeout:     5 * time.Second,
			},
		}
	}

	switch sig {
	case syscall.SIGQUIT:
		timeout := a.cfg.Shutdown.GracefulTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		a.logger.LogSignal("SIGQUIT", fmt.Sprintf("Graceful stop (waiting %v)", timeout))
		a.shutdownStream()
		a.shutdownHTTP3()
		_ = a.srv.GracefulStop(timeout)
		return false

	case syscall.SIGTERM, syscall.SIGINT:
		timeout := a.cfg.Shutdown.FastTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		sigTyped, ok := sig.(syscall.Signal)
		if !ok {
			a.logger.LogSignal("unknown", "Stopping server")
		} else {
			a.logger.LogSignal(sigName(sigTyped), "Stopping server")
		}
		a.shutdownStream()
		a.shutdownHTTP3()
		_ = a.srv.StopWithTimeout(timeout)
		return false

	case syscall.SIGHUP:
		a.logger.LogSignal("SIGHUP", "Reloading config")
		a.reloadConfig()
		return true

	case syscall.SIGUSR1:
		a.logger.LogSignal("SIGUSR1", "Reopening logs")
		a.reopenLogs()
		return true

	case syscall.SIGUSR2:
		a.logger.LogSignal("SIGUSR2", "Performing graceful upgrade")
		a.gracefulUpgrade()
		return true

	default:
		a.logger.Info().Str("signal", sig.String()).Msg("Received unknown signal")
		return true
	}
}

func (a *App) reloadConfig() {
	newCfg, err := config.Load(a.cfgPath)
	if err != nil {
		a.logger.Error().Err(err).Msg("Failed to reload config")
		return
	}

	if a.srv == nil {
		a.cfg = newCfg
		a.logger = logging.NewAppLogger(&newCfg.Logging)
		a.logger.LogStartup("Config reloaded (no running server)", nil)
		return
	}

	if a.requiresFullRestart(newCfg) {
		logging.Warn().Msg("Config requires full restart (listen address or mode changed). Use SIGUSR2 for graceful upgrade.")
		return
	}

	listeners := a.srv.GetListeners()
	if len(listeners) == 0 {
		a.logger.Error().Msg("Cannot reload: server has no saved listeners")
		return
	}

	duped := make([]net.Listener, len(listeners))
	for i, ln := range listeners {
		duped[i], err = server.DupListener(ln)
		if err != nil {
			for j := range i {
				_ = duped[j].Close()
			}
			a.logger.Error().Err(err).Msg("Failed to dup listener for reload")
			return
		}
	}

	newSrv := server.New(newCfg)
	if a.resv != nil {
		newSrv.SetResolver(a.resv)
	}
	newSrv.SetListeners(duped)

	startErr := make(chan error, 1)
	go func() {
		if err := newSrv.Start(); err != nil {
			startErr <- err
		}
	}()

	reloadTimeout := a.cfg.Shutdown.ReloadTimeout
	if reloadTimeout <= 0 {
		reloadTimeout = 5 * time.Second
	}

	select {
	case err := <-startErr:
		a.logger.Error().Err(err).Msg("Failed to start new server with reloaded config")
		for _, ln := range duped {
			_ = ln.Close()
		}
		return
	case <-time.After(reloadTimeout):
	}

	oldSrv := a.srv
	oldHTTP3 := a.http3Srv

	a.srv = newSrv
	a.cfg = newCfg
	a.logger = logging.NewAppLogger(&newCfg.Logging)
	a.http3Srv = nil

	a.initVariables()
	a.initHTTP3()

	if a.upgradeMgr != nil {
		a.upgradeMgr.SetListeners(newSrv.GetListeners())
	}

	// HTTP/2 由新 server.Start 内部经 fasthttp.NextProto("h2", ...) 挂载，
	// 旧 server 的 HTTP/2 连接随 fasthttp.GracefulStop 一并关闭，
	// 无需在 App 层维护独立的 HTTP/2 关闭路径。
	go func() {
		if oldHTTP3 != nil {
			_ = oldHTTP3.Stop()
		}
		_ = oldSrv.GracefulStop(30 * time.Second)
	}()

	a.logger.LogStartup("Config reloaded successfully", nil)
}

// requiresFullRestart 判断新配置是否改变运行模式或实际监听地址集合。
//
// 参数：
//   - newCfg: 待重载的新配置
//
// 返回值：
//   - bool: 需要重新绑定监听器时返回 true
func (a *App) requiresFullRestart(newCfg *config.Config) bool {
	if a.cfg.GetMode() != newCfg.GetMode() {
		return true
	}
	oldListens := uniqueListens(a.cfg.Servers)
	newListens := uniqueListens(newCfg.Servers)
	if len(oldListens) != len(newListens) {
		return true
	}
	for listen := range oldListens {
		if !newListens[listen] {
			return true
		}
	}
	return false
}

// uniqueListens 返回配置实际需要创建的监听地址集合。
//
// 参数：
//   - servers: 服务器配置列表
//
// 返回值：
//   - map[string]bool: 去重后的监听地址集合
func uniqueListens(servers []config.ServerConfig) map[string]bool {
	listens := make(map[string]bool, len(servers))
	for i := range servers {
		listens[servers[i].Listen] = true
	}
	return listens
}

// uniqueListenOrder 返回按首次出现顺序排列的监听地址。
//
// 参数：
//   - servers: 服务器配置列表
//
// 返回值：
//   - []string: 去重且顺序稳定的监听地址
func uniqueListenOrder(servers []config.ServerConfig) []string {
	seen := make(map[string]bool, len(servers))
	listens := make([]string, 0, len(servers))
	for i := range servers {
		listen := servers[i].Listen
		if seen[listen] {
			continue
		}
		seen[listen] = true
		listens = append(listens, listen)
	}
	return listens
}

func (a *App) gracefulUpgrade() {
	execPath, err := os.Executable()
	if err != nil {
		a.logger.Error().Err(err).Msg("Failed to get executable path")
		return
	}

	if a.srv == nil {
		a.logger.Error().Msg("Graceful upgrade failed: server instance is nil")
		return
	}

	listeners := a.srv.GetListeners()
	if len(listeners) == 0 {
		a.logger.Error().Msg("Graceful upgrade failed: server has no saved listeners (graceful upgrade not fully implemented)")
		a.logger.Info().Msg("Hint: graceful upgrade requires the server to use manual listener management mode")
		return
	}

	a.upgradeMgr.SetListeners(listeners)

	if err := a.upgradeMgr.GracefulUpgrade(execPath); err != nil {
		a.logger.Error().Err(err).Msg("Graceful upgrade failed")
		return
	}

	a.logger.LogStartup("Graceful upgrade started, new process is taking over", nil)

	timeout := a.cfg.Shutdown.GracefulTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	a.shutdownStream()
	a.shutdownHTTP3()
	_ = a.srv.GracefulStop(timeout)
}

func sigName(sig syscall.Signal) string {
	//nolint:exhaustive // Only handling app-relevant signals.
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGUSR1:
		return "SIGUSR1"
	case syscall.SIGUSR2:
		return "SIGUSR2"
	default:
		return fmt.Sprintf("Signal(%d)", sig)
	}
}
