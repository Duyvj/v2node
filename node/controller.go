package node

import (
	"context"
	"errors"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/task"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
)

type nodeServer interface {
	AddNode(tag string, info *panel.NodeInfo) error
	DelNode(tag string) error
	AddUsers(p *core.AddUsersParams) (int, error)
	DelUsers(users []panel.UserInfo, tag string, info *panel.NodeInfo) error
	GetUserTrafficSlice(tag string, mintraffic int) ([]panel.UserTraffic, error)
}

type Controller struct {
	server                  nodeServer
	reloadCh                chan struct{}
	apiClient               *panel.Client
	tag                     string
	limiter                 *limiter.Limiter
	userList                []panel.UserInfo
	aliveMap                map[int]int
	conf                    *conf.NodeConfig
	info                    *panel.NodeInfo
	infoMu                  sync.RWMutex
	certDigest              string
	nodeInfoMonitorPeriodic *task.Task
	userReportPeriodic      *task.Task
	renewCertPeriodic       *task.Task
}

// NewController return a Node controller with default parameters.
func NewController(api *panel.Client, conf *conf.NodeConfig, info *panel.NodeInfo) *Controller {
	controller := &Controller{
		apiClient: api,
		info:      info,
		conf:      conf,
	}
	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) error {
	// Init Core
	c.server = x
	c.reloadCh = x.ReloadCh
	var err error
	// First fetch Node Info
	node := c.info
	if node == nil {
		c.info, err = c.apiClient.GetNodeInfo(context.Background())
		if err != nil {
			return fmt.Errorf("get node info error: %s", err)
		}
		node = c.info
	}
	// Update user
	c.userList, err = c.apiClient.GetUserList(context.Background())
	if err != nil {
		return fmt.Errorf("get user list error: %s", err)
	}
	if len(c.userList) == 0 {
		return errors.New("add users error: not have any user")
	}
	c.aliveMap, err = c.apiClient.GetUserAlive(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get user alive list: %s", err)
	}
	c.tag = node.Tag

	// add limiter
	l := limiter.AddLimiter(c.info.Type, c.tag, c.userList, c.aliveMap)
	c.limiter = l
	if node.Security == panel.Tls {
		err = c.requestCert()
		if err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
		c.certDigest = certDigest(node)
	}
	// Add new tag
	err = c.server.AddNode(c.tag, node)
	if err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	added, err := c.server.AddUsers(&core.AddUsersParams{
		Tag:      c.tag,
		Users:    c.userList,
		NodeInfo: node,
	})
	if err != nil {
		return fmt.Errorf("add users error: %s", err)
	}
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	c.info = node
	c.startTasks(node)
	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	limiter.DeleteLimiter(c.tag)
	if c.nodeInfoMonitorPeriodic != nil {
		c.nodeInfoMonitorPeriodic.Close()
	}
	if c.userReportPeriodic != nil {
		c.userReportPeriodic.Close()
	}
	if c.renewCertPeriodic != nil {
		c.renewCertPeriodic.Close()
	}
	err := c.server.DelNode(c.tag)
	if err != nil {
		return fmt.Errorf("del node error: %s", err)
	}
	return nil
}

func (c *Controller) nodeInfo() *panel.NodeInfo {
	c.infoMu.RLock()
	defer c.infoMu.RUnlock()
	return c.info
}

func (c *Controller) setNodeInfo(info *panel.NodeInfo) {
	c.infoMu.Lock()
	c.info = info
	c.infoMu.Unlock()
}
