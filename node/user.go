package node

import (
	"context"
	"errors"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

func (c *Controller) reportUserTrafficTask(ctx context.Context) (err error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	var reportmin = 0
	var devicemin = 0
	if c.info.Common.BaseConfig != nil {
		reportmin = c.info.Common.BaseConfig.NodeReportMinTraffic
		devicemin = c.info.Common.BaseConfig.DeviceOnlineMinTraffic
	}
	userTraffic, commitTraffic := c.server.SnapshotUserTraffic(c.tag, reportmin)
	if len(userTraffic) > 0 {
		err = c.apiClient.ReportUserTraffic(ctx, userTraffic)
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Info("Report user traffic failed")
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		} else {
			commitTraffic()
			log.WithField("tag", c.tag).Infof("Report %d users traffic", len(userTraffic))
			//log.WithField("tag", c.tag).Debugf("User traffic: %+v", userTraffic)
		}
	}

	if onlineDevice, err := c.limiter.GetOnlineDevice(); err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Info("Get online device failed")
	} else if len(*onlineDevice) > 0 {
		data, reported := onlineReport(*onlineDevice, userTraffic, devicemin)
		if len(data) != 0 {
			err := c.apiClient.ReportNodeOnlineUsers(ctx, &data)
			if err != nil {
				log.WithFields(log.Fields{
					"tag": c.tag,
					"err": err,
				}).Info("Report online users failed")
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
			}
		}
		log.WithField("tag", c.tag).Infof("Total %d online users, %d Reported", len(*onlineDevice), reported)
	}

	return nil
}

func onlineReport(online []panel.OnlineUser, traffic []panel.UserTraffic, minimum int) (map[int][]string, int) {
	var excluded map[int]struct{}
	if minimum > 0 {
		excluded = make(map[int]struct{})
		for _, sample := range traffic {
			if sample.Upload+sample.Download < int64(minimum)*1000 {
				excluded[sample.UID] = struct{}{}
			}
		}
	}
	data := make(map[int][]string)
	reported := 0
	for _, user := range online {
		if _, skip := excluded[user.UID]; skip {
			continue
		}
		data[user.UID] = append(data[user.UID], user.IP)
		reported++
	}
	return data, reported
}

func compareUserList(old, new []panel.UserInfo) (deleted, added, modified []panel.UserInfo) {
	oldMap := make(map[string]panel.UserInfo, len(old))
	for _, u := range old {
		oldMap[u.Uuid] = u
	}

	for _, u := range new {
		if o, ok := oldMap[u.Uuid]; !ok {
			added = append(added, u)
		} else {
			if o.Id != u.Id {
				deleted = append(deleted, o)
				added = append(added, u)
			} else if o.SpeedLimit != u.SpeedLimit || o.DeviceLimit != u.DeviceLimit {
				modified = append(modified, u)
			}
			delete(oldMap, u.Uuid)
		}
	}

	for _, o := range oldMap {
		deleted = append(deleted, o)
	}

	return deleted, added, modified
}
