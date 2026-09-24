package cmd

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"syscall"

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
	Run:   serverHandle,
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

func serverHandle(_ *cobra.Command, _ []string) {
	showVersion()
	c := conf.New()
	err := c.LoadFromPath(config)
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableQuote:     true,
		PadLevelText:     false,
	})
	if err != nil {
		log.WithField("err", err).Error("Load config file failed")
		return
	}
	switch c.LogConfig.Level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	}
	if c.LogConfig.Output != "" {
		f, err := os.OpenFile(c.LogConfig.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithField("err", err).Error("Open log file failed, using stdout instead")
		}
		if err == nil {
			log.SetOutput(f)
		}
	}
	// Enable pprof if configured
	if c.PprofPort != 0 {
		go func() {
			log.Infof("Starting pprof server on :%d", c.PprofPort)
			if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", c.PprofPort), nil); err != nil {
				log.WithField("err", err).Error("pprof server failed")
			}
		}()
	}
	//init limiter
	limiter.Init()
	//get node info
	nodes, err := node.New(c.NodeConfigs)
	if err != nil {
		log.WithField("err", err).Error("Get node info failed")
		return
	}
	log.Info("Got nodes info from server")
	//core
	var reloadCh = make(chan struct{}, 1)
	v2core := core.New(c)
	v2core.ReloadCh = reloadCh
	defer func() { _ = nodes.Close(); _ = v2core.Close() }()
	err = v2core.Start(nodes.NodeInfos)
	if err != nil {
		log.WithField("err", err).Error("Start core failed")
		return
	}
	//node
	err = nodes.Start(c.NodeConfigs, v2core)
	if err != nil {
		log.WithField("err", err).Error("Run nodes failed")
		return
	}
	applyResources(c.ResourceConfig)
	log.Info("Nodes started")
	if watch {
		// On file change, just signal reload; do not run reload concurrently here
		err = c.Watch(config, func() {
			select {
			case reloadCh <- struct{}{}:
			default: // drop if a reload is already queued
			}
		})
		if err != nil {
			log.WithField("err", err).Error("start watch failed")
			return
		}
	}
	defer c.CloseWatch()
	// clear memory
	runtime.GC()

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(osSignals)

	for {
		select {
		case <-osSignals:
			log.Info("收到退出信号，正在关闭程序...")
			return
		case <-reloadCh:
			log.Info("收到重启信号，正在重新加载配置...")
			if err := reload(config, &nodes, &v2core); err != nil {
				log.WithError(err).Error("Reload rejected or rolled back")
				continue
			}
			log.Info("重启成功")
		}
	}
}

// Prepare first, then activate. A failed replacement restores the already
// prepared previous snapshot without another panel request.
func reload(path string, nodes **node.Node, v2core **core.V2Core) error {
	next := conf.New()
	if err := next.LoadFromPath(path); err != nil {
		return err
	}
	nextNodes, err := node.New(next.NodeConfigs)
	if err != nil {
		return err
	}
	previousNodes, previousCore := *nodes, *v2core
	if err := previousNodes.Close(); err != nil {
		_ = nextNodes.Close()
		return err
	}
	if err := previousCore.Close(); err != nil {
		_ = nextNodes.Close()
		return err
	}
	candidate := core.New(next)
	candidate.ReloadCh = previousCore.ReloadCh
	startErr := candidate.Start(nextNodes.NodeInfos)
	if startErr == nil {
		startErr = nextNodes.Start(next.NodeConfigs, candidate)
	}
	if startErr != nil {
		_ = nextNodes.Close()
		_ = candidate.Close()
		restored := core.New(previousCore.Config)
		restored.ReloadCh = previousCore.ReloadCh
		restoreErr := restored.Start(previousNodes.NodeInfos)
		if restoreErr == nil {
			restoreErr = previousNodes.Start(previousCore.Config.NodeConfigs, restored)
		}
		if restoreErr != nil {
			_ = previousNodes.Close()
			_ = restored.Close()
			return fmt.Errorf("reload failed: %v; rollback failed: %w", startErr, restoreErr)
		}
		*nodes = previousNodes
		*v2core = restored
		return fmt.Errorf("previous runtime restored: %w", startErr)
	}
	*nodes = nextNodes
	*v2core = candidate
	applyResources(next.ResourceConfig)
	if level, err := log.ParseLevel(next.LogConfig.Level); err == nil {
		log.SetLevel(level)
	}
	runtime.GC()
	return nil
}

var baselineMemoryLimit = debug.SetMemoryLimit(-1)
var baselineGC = func() int {
	s := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(s)
	return int(s[0].Value.Uint64())
}()

func applyResources(r conf.ResourceConfig) {
	gc := baselineGC
	if r.GOGC > 0 {
		gc = r.GOGC
	}
	debug.SetGCPercent(gc)
	memory := baselineMemoryLimit
	if r.MemoryLimitMB > 0 {
		memory = r.MemoryLimitMB * 1024 * 1024
	}
	debug.SetMemoryLimit(memory)
}
