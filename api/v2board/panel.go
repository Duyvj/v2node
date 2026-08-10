package panel

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
	"github.com/wyx2685/v2node/conf"
)

// Panel is the interface for different panel's api.

type Client struct {
	client           *resty.Client
	APIHost          string
	Token            string
	NodeId           int
	nodeEtag         string
	userEtag         string
	responseBodyHash string
	UserList         *UserListBody
	AliveMap         *AliveMap
	minPollInterval  time.Duration
	maxPollInterval  time.Duration
	maxResponseBytes int
	maxUsers         int
}

func New(c *conf.NodeConfig, runtimeConfigs ...conf.RuntimeConfig) (*Client, error) {
	runtimeConfig := conf.DefaultRuntimeConfig()
	if len(runtimeConfigs) > 0 {
		runtimeConfig = runtimeConfigs[0]
	}
	runtimeConfig.Normalize()
	client := resty.New()
	client.SetResponseBodyLimit(runtimeConfig.MaxPanelResponseBytes)
	retryCount := conf.DefaultNodeRetryCount
	if c.RetryCount != nil {
		retryCount = *c.RetryCount
	}
	client.SetRetryCount(retryCount)
	client.SetHeader("User-Agent", fmt.Sprintf("v2node go-resty/%s (https://github.com/go-resty/resty)", resty.Version))
	if c.Timeout > 0 {
		client.SetTimeout(time.Duration(c.Timeout) * time.Second)
	} else {
		client.SetTimeout(time.Duration(conf.DefaultNodeTimeout) * time.Second)
	}
	client.OnError(func(req *resty.Request, err error) {
		var v *resty.ResponseError
		if errors.As(err, &v) {
			// v.Response contains the last response from the server
			// v.Err contains the original error
			logrus.Error(redactError(v.Err, c.Key))
		}
	})
	client.SetBaseURL(c.APIHost)
	// set params
	client.SetQueryParams(map[string]string{
		"node_type": "v2node",
		"node_id":   strconv.Itoa(c.NodeID),
		"token":     c.Key,
	})
	return &Client{
		client:           client,
		Token:            c.Key,
		APIHost:          c.APIHost,
		NodeId:           c.NodeID,
		UserList:         &UserListBody{},
		AliveMap:         &AliveMap{},
		minPollInterval:  time.Duration(runtimeConfig.MinPollIntervalSeconds) * time.Second,
		maxPollInterval:  time.Duration(runtimeConfig.MaxPollIntervalSeconds) * time.Second,
		maxResponseBytes: runtimeConfig.MaxPanelResponseBytes,
		maxUsers:         runtimeConfig.MaxUsers,
	}, nil
}

// Close releases keep-alive connections owned by this panel generation.
func (c *Client) Close() {
	if c != nil && c.client != nil && c.client.GetClient() != nil {
		c.client.GetClient().CloseIdleConnections()
	}
}
