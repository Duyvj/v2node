package node

import (
	"context"
	"errors"
	"reflect"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/task"
	vCore "github.com/wyx2685/v2node/core"
)

func (c *Controller) startTasks(node *panel.NodeInfo) {
	// fetch node info task
	c.nodeInfoMonitorPeriodic = &task.Task{
		Name:     "nodeInfoMonitor",
		Interval: node.PullInterval,
		Execute:  c.nodeInfoMonitor,
		ReloadCh: c.reloadCh,
	}
	// fetch user list task
	c.userReportPeriodic = &task.Task{
		Name:     "reportUserTrafficTask",
		Interval: node.PushInterval,
		Execute:  c.reportUserTrafficTask,
		ReloadCh: c.reloadCh,
	}
	log.WithField("tag", c.tag).Info("Start monitor node status")
	// delay to start nodeInfoMonitor
	_ = c.nodeInfoMonitorPeriodic.Start(false)
	log.WithField("tag", c.tag).Info("Start report node status")
	_ = c.userReportPeriodic.Start(false)
	if node.Security == panel.Tls {
		switch c.info.Common.CertInfo.CertMode {
		case "none", "", "file", "self", "remote":
		default:
			c.renewCertPeriodic = &task.Task{
				Name:     "renewCertTask",
				Interval: time.Hour * 24,
				Execute:  c.renewCertTask,
				ReloadCh: c.reloadCh,
			}
			log.WithField("tag", c.tag).Info("Start renew cert")
			// delay to start renewCert
			_ = c.renewCertPeriodic.Start(true)
		}
	}
}

func (c *Controller) nodeInfoMonitor(ctx context.Context) (err error) {
	// get node info
	newN, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get node info failed")
		return nil
	}
	if newN != nil {
		if needsReload(c.info, newN) {
			log.WithField("tag", c.tag).Warn("Got new node info, reload")
			c.requestReload()
			return nil
		}
		if err := c.rebuildInbound(newN); err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Rebuild inbound failed, reload")
			c.requestReload()
			return nil
		}
		log.WithField("tag", c.tag).Info("Got new node info, inbound rebuilt")
	} else if digest := certDigest(c.info); digest != "" && digest != c.certDigest {
		if err := c.rebuildInbound(c.info); err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Rebuild inbound for new certificate failed, reload")
			c.requestReload()
			return nil
		}
		log.WithField("tag", c.tag).Info("Certificate changed, inbound rebuilt")
	}
	log.WithField("tag", c.tag).Debug("Node info no change")

	// get user info
	newU, err := c.apiClient.GetUserList(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get user list failed")
		return nil
	}
	// get user alive
	newA, err := c.apiClient.GetUserAlive(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get alive list failed")
		return nil
	}

	// update alive list
	if newA != nil {
		c.limiter.AliveList = newA
	}
	// node no changed, check users
	if len(newU) == 0 {
		log.WithField("tag", c.tag).Debug("User list no change")
		return nil
	}
	deleted, added, modified := compareUserList(c.userList, newU)
	if len(deleted) > 0 {
		// have deleted users
		err = c.server.DelUsers(deleted, c.tag, c.info)
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Delete users failed")
			return nil
		}
	}
	if len(added) > 0 {
		// have added users
		_, err = c.server.AddUsers(&vCore.AddUsersParams{
			Tag:      c.tag,
			NodeInfo: c.info,
			Users:    added,
		})
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Add users failed")
			return nil
		}
	}
	if len(added) > 0 || len(deleted) > 0 || len(modified) > 0 {
		// update Limiter
		c.limiter.UpdateUser(c.tag, added, deleted, modified)
	}
	c.userList = newU
	log.WithField("tag", c.tag).Infof("%d user deleted, %d user added, %d user modified", len(deleted), len(added), len(modified))
	return nil
}

func needsReload(old, new *panel.NodeInfo) bool {
	return old.Tag != new.Tag ||
		old.Security != new.Security ||
		old.PullInterval != new.PullInterval ||
		old.PushInterval != new.PushInterval ||
		old.Common.CertInfo.CertMode != new.Common.CertInfo.CertMode ||
		!reflect.DeepEqual(old.Common.Routes, new.Common.Routes)
}

func (c *Controller) rebuildInbound(info *panel.NodeInfo) error {
	c.setNodeInfo(info)
	if info.Security == panel.Tls {
		if err := c.requestCert(); err != nil {
			return err
		}
	}
	if err := c.server.DelNode(c.tag); err != nil {
		return err
	}
	if err := c.server.AddNode(c.tag, info); err != nil {
		return err
	}
	if _, err := c.server.AddUsers(&vCore.AddUsersParams{
		Tag:      c.tag,
		Users:    c.userList,
		NodeInfo: info,
	}); err != nil {
		return err
	}
	c.certDigest = certDigest(info)
	return nil
}

func (c *Controller) requestReload() {
	if c.reloadCh == nil {
		log.Panic("Reload failed")
	}
	select {
	case c.reloadCh <- struct{}{}:
	default:
	}
}
