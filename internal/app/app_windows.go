//go:build windows

// Windows lacks POSIX signals (SIGUSR1, SIGUSR2, SIGHUP, SIGQUIT);
// this file provides stub implementations for those Unix-specific signals.
// Package app 提供 Lolly 应用程序的生命周期管理和命令行入口。
//
// 包含 Windows 平台特定的应用逻辑。
//
// 作者：xfy
package app

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rua.plus/lolly/internal/server"
)

// Run starts the application: loads config, creates servers, and handles signals (Windows version).
func (a *App) Run() int {
	if err := a.loadAndValidateConfig(); err != nil {
		return 1
	}

	a.initVariables()
	a.logServerAddresses()
	a.initResolver()
	a.initServer()
	a.initStreamServers()
	a.initHTTP3()

	a.upgradeMgr = server.NewUpgradeManager(a.srv)
	if a.pidFile != "" {
		a.upgradeMgr.SetPidFile(a.pidFile)
		_ = a.upgradeMgr.WritePid()
	}

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

func (a *App) setupSignalHandlers(sigChan chan<- os.Signal) {
	signal.Notify(sigChan,
		syscall.SIGTERM,
		syscall.SIGINT,
	)
}

// handleSignal returns false to indicate the app should exit (Windows version).
func (a *App) handleSignal(sig os.Signal) bool {
	switch sig {
	case syscall.SIGTERM, syscall.SIGINT:
		timeout := a.cfg.Shutdown.FastTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		a.logger.LogSignal(sigName(sig.(syscall.Signal)), "Stopping server")
		a.shutdownStream()
		a.shutdownHTTP3()
		_ = a.srv.StopWithTimeout(timeout)
		return false
	default:
		a.logger.Info().Str("signal", sig.String()).Msg("Received signal (ignored on Windows)")
		return true
	}
}

func (a *App) reloadConfig() {
	// Windows stub - functionality limited
}

func (a *App) gracefulUpgrade() {
	a.logger.Info().Msg("Graceful upgrade is not supported on Windows")
}

func sigName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	default:
		return fmt.Sprintf("Signal(%d)", sig)
	}
}
