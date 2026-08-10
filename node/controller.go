package node

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/task"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
)

type Controller struct {
	server                  *core.V2Core
	apiClient               *panel.Client
	tag                     string
	limiter                 *limiter.Limiter
	userList                []panel.UserInfo
	conf                    *conf.NodeConfig
	runtime                 conf.RuntimeConfig
	info                    *panel.NodeInfo
	nodeInfoMonitorPeriodic *task.Task
	userReportPeriodic      *task.Task
	renewCertPeriodic       *task.Task
	started                 bool
}

// NewController return a Node controller with default parameters.
func NewController(api *panel.Client, nodeConf *conf.NodeConfig, info *panel.NodeInfo, runtimeConfigs ...conf.RuntimeConfig) *Controller {
	runtimeConfig := conf.DefaultRuntimeConfig()
	if len(runtimeConfigs) > 0 {
		runtimeConfig = runtimeConfigs[0]
	}
	runtimeConfig.Normalize()
	controller := &Controller{
		apiClient: api,
		info:      info,
		conf:      nodeConf,
		runtime:   runtimeConfig,
	}
	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) (err error) {
	// Init Core
	c.server = x
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
	aliveMap, err := c.apiClient.GetUserAlive(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get user alive list: %s", err)
	}
	c.tag = node.Tag

	// add limiter
	l := limiter.AddLimiter(
		c.info.Type,
		c.tag,
		c.userList,
		aliveMap,
		c.runtime.MaxTrackedIPsPerUser,
		c.runtime.MaxTrackedIPsPerNode,
	)
	c.limiter = l
	nodeAdded := false
	usersAdded := false
	defer func() {
		if err == nil {
			return
		}
		if usersAdded {
			_ = c.server.DelUsers(c.userList, c.tag, c.info)
		}
		if nodeAdded {
			_ = c.server.DelNode(c.tag)
		}
		limiter.DeleteLimiter(c.tag)
	}()
	if node.Security == panel.Tls {
		err = c.requestCert()
		if err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}
	// Add new tag
	err = c.server.AddNode(c.tag, node)
	if err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	nodeAdded = true
	added, err := c.server.AddUsers(&core.AddUsersParams{
		Tag:      c.tag,
		Users:    c.userList,
		NodeInfo: node,
	})
	if err != nil {
		return fmt.Errorf("add users error: %s", err)
	}
	usersAdded = true
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	c.info = node
	if err = c.startTasks(node); err != nil {
		return fmt.Errorf("start tasks error: %s", err)
	}
	c.started = true
	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	var taskErr error
	if c.nodeInfoMonitorPeriodic != nil {
		taskErr = errors.Join(taskErr, c.nodeInfoMonitorPeriodic.Close())
	}
	if c.userReportPeriodic != nil {
		taskErr = errors.Join(taskErr, c.userReportPeriodic.Close())
	}
	if c.renewCertPeriodic != nil {
		taskErr = errors.Join(taskErr, c.renewCertPeriodic.Close())
	}
	if taskErr != nil {
		return taskErr
	}
	limiter.DeleteLimiter(c.tag)
	if !c.started {
		c.apiClient.Close()
		return nil
	}
	err := c.server.DelNode(c.tag)
	c.apiClient.Close()
	c.started = false
	c.limiter = nil
	c.userList = nil
	if err != nil {
		return fmt.Errorf("del node error: %s", err)
	}
	return nil
}
