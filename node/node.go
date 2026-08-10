package node

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

type Node struct {
	controllers []*Controller
	NodeInfos   []*panel.NodeInfo
}

func New(nodes []conf.NodeConfig, runtimeConfigs ...conf.RuntimeConfig) (*Node, error) {
	runtimeConfig := conf.DefaultRuntimeConfig()
	if len(runtimeConfigs) > 0 {
		runtimeConfig = runtimeConfigs[0]
	}
	runtimeConfig.Normalize()
	n := &Node{
		controllers: make([]*Controller, len(nodes)),
		NodeInfos:   make([]*panel.NodeInfo, len(nodes)),
	}
	for i, node := range nodes {
		p, err := panel.New(&node, runtimeConfig)
		if err != nil {
			n.closePanelClients()
			return nil, err
		}
		info, err := p.GetNodeInfo(context.Background())
		if err != nil {
			p.Close()
			n.closePanelClients()
			return nil, err
		}
		n.controllers[i] = NewController(p, &node, info, runtimeConfig)
		n.NodeInfos[i] = info
	}
	return n, nil
}

func (n *Node) Start(nodes []conf.NodeConfig, core *core.V2Core) error {
	for i, node := range nodes {
		err := n.controllers[i].Start(core)
		if err != nil {
			startErr := fmt.Errorf("start node controller [%s-%d] error: %s",
				node.APIHost,
				node.NodeID,
				err)
			for j := i - 1; j >= 0; j-- {
				startErr = errors.Join(startErr, n.controllers[j].Close())
			}
			for j := i; j < len(n.controllers); j++ {
				if n.controllers[j] != nil {
					n.controllers[j].apiClient.Close()
				}
			}
			return startErr
		}
	}
	return nil
}

func (n *Node) Close() error {
	var closeErr error
	for _, c := range n.controllers {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			log.Errorf("close controller failed: %v", err)
			closeErr = errors.Join(closeErr, err)
		}
	}
	if closeErr == nil {
		n.controllers = nil
		n.NodeInfos = nil
	}
	return closeErr
}

func (n *Node) closePanelClients() {
	for _, controller := range n.controllers {
		if controller != nil && controller.apiClient != nil {
			controller.apiClient.Close()
		}
	}
}
