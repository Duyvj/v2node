package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
	"github.com/wyx2685/v2node/node"
)

var (
	config string
	watch  bool
)

var serverCommand = cobra.Command{
	Use:   "server",
	Short: "Run v2node server",
	RunE:  serverHandle,
	Args:  cobra.NoArgs,
}

func init() {
	serverCommand.PersistentFlags().
		StringVarP(&config, "config", "c",
			"/etc/v2node/config.json", "config file path")
	serverCommand.PersistentFlags().
		BoolVarP(&watch, "watch", "w",
			true, "watch file path change")
	command.AddCommand(&serverCommand)
}

func serverHandle(_ *cobra.Command, _ []string) error {
	showVersion()
	return runServer()
}

func runServer() error {
	c := conf.New()
	if err := c.LoadFromPath(config); err != nil {
		return fmt.Errorf("load config file: %w", err)
	}
	configureLogging(c.LogConfig.Level)
	logFile, err := configureLogOutput(c.LogConfig.Output)
	if err != nil {
		return err
	}
	if logFile != nil {
		defer func() {
			log.SetOutput(os.Stdout)
			_ = logFile.Close()
		}()
	}
	if err := applyRuntimeConfig(c.Runtime); err != nil {
		return fmt.Errorf("apply runtime configuration: %w", err)
	}

	pprofServer := startPprofServer(c.PprofPort)
	limiter.Init()
	reloadCh := make(chan struct{}, 1)
	nodes, err := node.New(c.NodeConfigs, c.Runtime)
	if err != nil {
		shutdownPprof(pprofServer)
		return fmt.Errorf("get node info: %w", err)
	}
	v2core := core.New(c)
	v2core.ReloadCh = reloadCh
	if err := v2core.Start(nodes.NodeInfos); err != nil {
		_ = nodes.Close()
		shutdownPprof(pprofServer)
		return fmt.Errorf("start core: %w", err)
	}
	if err := nodes.Start(c.NodeConfigs, v2core); err != nil {
		_ = closeRuntime(nodes, v2core, 25*time.Second)
		shutdownPprof(pprofServer)
		return fmt.Errorf("run nodes: %w", err)
	}
	log.Info("Nodes started")

	if watch {
		if err := c.Watch(config, func() {
			select {
			case reloadCh <- struct{}{}:
			default:
			}
		}); err != nil {
			_ = closeRuntime(nodes, v2core, 25*time.Second)
			shutdownPprof(pprofServer)
			return fmt.Errorf("start config watcher: %w", err)
		}
	}

	// Return startup scratch pages to the OS once; periodic forced GC would hurt
	// throughput and cannot fix live-object leaks.
	runtime.GC()
	debug.FreeOSMemory()

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	restart := false
	select {
	case sig := <-osSignals:
		log.WithField("signal", sig).Info("Shutting down")
	case <-reloadCh:
		restart = true
		log.Info("Configuration changed; replacing process to release the complete old core")
	}
	signal.Stop(osSignals)

	c.CloseWatch()
	shutdownPprof(pprofServer)
	closeErr := closeRuntime(nodes, v2core, 25*time.Second)
	if closeErr != nil {
		log.WithField("err", closeErr).Warn("Resource cleanup reported an error")
	}
	if restart {
		// The pinned Xray dependency contains protocol cleanup loops that have no
		// stop API. exec replaces the address space, guaranteeing that every old
		// goroutine/cache is reclaimed without running two generations together.
		return errors.Join(closeErr, restartProcess())
	}
	return closeErr
}

func closeRuntime(nodes *node.Node, v2core *core.V2Core, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		closeErr := nodes.Close()
		closeErr = errors.Join(closeErr, v2core.Close())
		done <- closeErr
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("resource cleanup exceeded %s", timeout)
	}
}

func configureLogging(level string) {
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableQuote:     true,
		PadLevelText:     false,
	})
	switch level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}
}

func configureLogOutput(path string) (*os.File, error) {
	log.SetOutput(os.Stdout)
	if path == "" {
		return nil, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// Preserve upstream behavior: a bad optional log path must not prevent
		// the proxy from starting. stdout remains active and owns no file handle.
		log.WithField("err", err).Error("Open log file failed, using stdout instead")
		return nil, nil
	}
	log.SetOutput(file)
	return file, nil
}

func startPprofServer(port int) *http.Server {
	if port == 0 {
		return nil
	}
	server := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", port),
		Handler:           http.DefaultServeMux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		log.Infof("Starting pprof server on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithField("err", err).Error("pprof server failed")
		}
	}()
	return server
}

func shutdownPprof(server *http.Server) {
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}
}

func applyRuntimeConfig(runtimeConfig conf.RuntimeConfig) error {
	if runtimeConfig.MemoryLimit == "" {
		return nil
	}
	limit, err := conf.ParseMemoryLimit(runtimeConfig.MemoryLimit)
	if err != nil {
		return err
	}
	if limit > 0 {
		debug.SetMemoryLimit(limit)
	}
	return nil
}
