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

func New(nodes []conf.NodeConfig) (*Node, error) {
	n := &Node{
		controllers: make([]*Controller, len(nodes)),
		NodeInfos:   make([]*panel.NodeInfo, len(nodes)),
	}
	for i, node := range nodes {
		p, err := panel.New(&node)
		if err != nil {
			return nil, err
		}
		info, err := p.GetNodeInfo(context.Background())
		if err != nil {
			p.Close()
			_ = n.Close()
			return nil, err
		}
		n.controllers[i] = NewController(p, &node, info)
		if err := n.controllers[i].Prepare(context.Background()); err != nil {
			_ = n.Close()
			return nil, err
		}
		n.NodeInfos[i] = info
	}
	return n, nil
}

func (n *Node) Start(nodes []conf.NodeConfig, core *core.V2Core) error {
	for i, node := range nodes {
		err := n.controllers[i].Start(core)
		if err != nil {
			_ = n.Close()
			return fmt.Errorf("start node controller [%s-%d] error: %s",
				node.APIHost,
				node.NodeID,
				err)
		}
	}
	return nil
}

func (n *Node) Close() error {
	var result error
	for _, c := range n.controllers {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			log.Errorf("close controller failed: %v", err)
			result = errors.Join(result, err)
		}
	}
	return result
}
