// Package app 提供 Lolly 应用程序的生命周期管理和命令行入口。
//
// 包含应用通用逻辑相关的工具函数。
//
// 作者：xfy
package app

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/http2"
	"rua.plus/lolly/internal/http3"
	"rua.plus/lolly/internal/logging"
	"rua.plus/lolly/internal/middleware/bodylimit"
	"rua.plus/lolly/internal/resolver"
	"rua.plus/lolly/internal/server"
	"rua.plus/lolly/internal/stream"
	"rua.plus/lolly/internal/variable"
)

// App manages the server lifecycle, including HTTP, HTTP/3, Stream servers and graceful upgrades.
type App struct {
	resv       resolver.Resolver
	cfg        *config.Config
	srv        *server.Server
	http3Srv   *http3.Server
	http2Srv   *http2.Server
	streamSrv  *stream.Server
	upgradeMgr *server.UpgradeManager
	logger     *logging.AppLogger
	cfgPath    string
	pidFile    string
	logFile    string
	listeners  []net.Listener
	ready      chan struct{}
	readyOnce  sync.Once
}

// NewApp creates a new App instance with the given config path.
func NewApp(cfgPath string) *App {
	return &App{
		cfgPath: cfgPath,
		ready:   make(chan struct{}),
	}
}

// WaitReady blocks until the app has completed initialization.
func (a *App) WaitReady() {
	<-a.ready
}

// signalReady closes the ready channel to unblock callers of WaitReady.
func (a *App) signalReady() {
	a.readyOnce.Do(func() { close(a.ready) })
}

// SetPidFile sets the path to the PID file for the app.
func (a *App) SetPidFile(path string) {
	a.pidFile = path
}

// SetLogFile sets the path to the log file for the app.
func (a *App) SetLogFile(path string) {
	a.logFile = path
}

// loadAndValidateConfig loads configuration and initializes the logger.
func (a *App) loadAndValidateConfig() error {
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return err
	}
	a.cfg = cfg
	a.logger = logging.NewAppLogger(&cfg.Logging)
	return nil
}

// initVariables loads global variables from configuration.
func (a *App) initVariables() {
	variable.SetGlobalVariables(a.cfg.Variables.Set)
	if len(a.cfg.Variables.Set) > 0 {
		a.logger.LogStartup("Global variables loaded", map[string]string{
			"count": fmt.Sprintf("%d", len(a.cfg.Variables.Set)),
		})
	}
}

// logServerAddresses logs the listening addresses based on server mode.
func (a *App) logServerAddresses() {
	a.logger.LogStartup("Config loaded successfully", map[string]string{"config_path": a.cfgPath})

	if len(a.cfg.Servers) == 0 {
		return
	}
	const listenField = "listen"
	for listen := range uniqueListens(a.cfg.Servers) {
		a.logger.LogStartup("Listening address", map[string]string{listenField: listen})
	}
}

// initResolver initializes the DNS resolver if enabled.
func (a *App) initResolver() {
	if a.cfg.Resolver.Enabled {
		a.resv = resolver.New(&a.cfg.Resolver)
		a.logger.LogStartup("DNS resolver enabled", map[string]string{
			"addresses": fmt.Sprintf("%v", a.cfg.Resolver.Addresses),
			"ttl":       a.cfg.Resolver.TTL().String(),
		})
	}
}

// initServer creates the main server and sets the resolver.
func (a *App) initServer() {
	a.srv = server.New(a.cfg)

	if a.resv != nil {
		a.srv.SetResolver(a.resv)
	}

	if len(a.listeners) > 0 {
		a.srv.SetListeners(a.listeners)
	}
}

// initStreamServers configures and starts stream servers.
func (a *App) initStreamServers() {
	if len(a.cfg.Stream) == 0 {
		return
	}

	a.streamSrv = stream.NewServer()
	for _, sc := range a.cfg.Stream {
		targets := make([]stream.TargetSpec, len(sc.Upstream.Targets))
		for i, t := range sc.Upstream.Targets {
			targets[i] = stream.TargetSpec{
				Addr:   t.Addr,
				Weight: t.Weight,
			}
		}

		if err := a.streamSrv.AddUpstream(sc.Listen, targets, sc.Upstream.LoadBalance, stream.HealthCheckSpec{}); err != nil {
			a.logger.Error().Err(err).Msg("Failed to add Stream upstream")
		}

		if sc.Protocol == "udp" {
			if err := a.streamSrv.ListenUDP(sc.Listen, sc.Listen, 60*time.Second); err != nil {
				a.logger.Error().Err(err).Str("listen", sc.Listen).Msg("Failed to listen on UDP")
			}
		} else {
			if err := a.streamSrv.ListenTCP(sc.Listen, sc.Listen); err != nil {
				a.logger.Error().Err(err).Str("listen", sc.Listen).Msg("Failed to listen on TCP")
			}
		}
	}

	go func() {
		a.logger.LogStartup("Starting Stream server", nil)
		if err := a.streamSrv.Start(); err != nil {
			a.logger.Error().Err(err).Msg("Stream server failed to start")
		}
	}()
}

// clientMaxBodySize parses the first server's client_max_body_size into bytes.
func (a *App) clientMaxBodySize() int64 {
	size, err := bodylimit.ParseSize(a.cfg.Servers[0].ClientMaxBodySize)
	if err != nil {
		return bodylimit.DefaultMaxBodySize
	}
	return size
}

// initHTTP3 starts the HTTP/3 server if enabled.
func (a *App) initHTTP3() {
	if len(a.cfg.Servers) == 0 || !a.cfg.HTTP3.Enabled || a.cfg.Servers[0].SSL.Cert == "" {
		return
	}

	tlsConfig, err := a.srv.GetTLSConfig()
	if err != nil {
		a.logger.Error().Err(err).Msg("Failed to get TLS config, skipping HTTP/3")
		return
	}

	a.cfg.HTTP3.MaxBodySize = a.clientMaxBodySize()

	a.http3Srv, err = http3.NewServer(&a.cfg.HTTP3, a.srv.GetHandler(), tlsConfig)
	if err != nil {
		a.logger.Error().Err(err).Msg("Failed to create HTTP/3 server")
		return
	}

	go func() {
		a.logger.LogStartup("Starting HTTP/3 server", map[string]string{"listen": a.cfg.HTTP3.Listen})
		if err := a.http3Srv.Start(); err != nil {
			a.logger.Error().Err(err).Msg("HTTP/3 server failed to start")
		}
	}()
}

// initHTTP2 starts the HTTP/2 server if enabled.
func (a *App) initHTTP2() {
	if len(a.cfg.Servers) == 0 || !a.cfg.Servers[0].SSL.HTTP2.Enabled || a.cfg.Servers[0].SSL.Cert == "" {
		return
	}

	tlsConfig, err := a.srv.GetTLSConfig()
	if err != nil {
		a.logger.Error().Err(err).Msg("Failed to get TLS config, skipping HTTP/2")
		return
	}

	a.cfg.Servers[0].SSL.HTTP2.MaxBodySize = a.clientMaxBodySize()

	a.http2Srv, err = http2.NewServer(&a.cfg.Servers[0].SSL.HTTP2, a.srv.GetHandler(), tlsConfig)
	if err != nil {
		a.logger.Error().Err(err).Msg("Failed to create HTTP/2 server")
		return
	}

	go func() {
		a.logger.LogStartup("Starting HTTP/2 server", map[string]string{
			"listen":                 a.cfg.Servers[0].Listen,
			"max_concurrent_streams": fmt.Sprintf("%d", a.cfg.Servers[0].SSL.HTTP2.MaxConcurrentStreams),
			"push_enabled":           fmt.Sprintf("%t", a.cfg.Servers[0].SSL.HTTP2.PushEnabled),
		})
		// HTTP/2 shares the main server's listener; ALPN negotiates protocol selection.
		listeners := a.srv.GetListeners()
		if len(listeners) > 0 {
			if err := a.http2Srv.Serve(listeners[0]); err != nil {
				a.logger.Error().Err(err).Msg("HTTP/2 server failed to start")
			}
		} else {
			a.logger.Error().Msg("HTTP/2 server failed to start: no available listeners")
		}
	}()
}

// shutdownHTTP3 gracefully stops the HTTP/3 server.
func (a *App) shutdownHTTP3() {
	if a.http3Srv != nil {
		if err := a.http3Srv.Stop(); err != nil {
			a.logger.Error().Err(err).Msg("Failed to shutdown HTTP/3 server")
		}
	}
}

// shutdownHTTP2 gracefully stops the HTTP/2 server.
func (a *App) shutdownHTTP2() {
	if a.http2Srv != nil {
		if err := a.http2Srv.Stop(); err != nil {
			a.logger.Error().Err(err).Msg("Failed to shutdown HTTP/2 server")
		}
	}
}

// reopenLogs reinitializes the logger from current config.
func (a *App) shutdownStream() {
	if a.streamSrv != nil {
		a.streamSrv.Stop()
	}
}

func (a *App) reopenLogs() {
	if a.cfg != nil {
		logging.Init(a.cfg.Logging.Error.Level, a.cfg.Logging.Format)
		a.logger = logging.NewAppLogger(&a.cfg.Logging)
	}
	a.logger.LogStartup("Logs reopened", nil)
}
