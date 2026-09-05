package panel

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/wyx2685/v2node/conf"
	"github.com/go-resty/resty/v2"
)

// Panel is the interface for different panel's api.

type Client struct {
	client           *resty.Client
	APIHost          string
	AgentID          string
	Token            string
	NodeId           int
	nodeEtag         string
	userEtag         string
	responseBodyHash string
	UserList         *UserListBody
	AliveMap         *AliveMap
	fallbackConfig   *conf.GlobalDeviceLimitConfig
	fallbackMu       sync.Mutex
}

// UpdateFallbackConfig changes only the signed Redis user-snapshot source.
// It does not touch Xray inbounds, users, or the device limiter.
func (c *Client) UpdateFallbackConfig(config *conf.GlobalDeviceLimitConfig) {
	c.fallbackMu.Lock()
	c.fallbackConfig = cloneGlobalDeviceLimitConfig(config)
	c.fallbackMu.Unlock()
}

// Ordinary panel responses are small JSON documents. Keep a hard ceiling so
// a compromised/misconfigured endpoint cannot make the root Agent buffer an
// unbounded body. The user list is streamed separately in user.go.
const maxBufferedPanelResponseBytes = 8 << 20

func New(c *conf.NodeConfig) (*Client, error) {
	if c == nil {
		return nil, fmt.Errorf("node client requires a config")
	}
	if strings.TrimSpace(c.Key) == "" {
		return nil, fmt.Errorf("node client requires an ApiKey/token")
	}
	apiHost, err := conf.NormalizePanelAPIHost(c.APIHost)
	if err != nil {
		return nil, fmt.Errorf("node client: %w", err)
	}
	client := resty.New()
	client.SetResponseBodyLimit(maxBufferedPanelResponseBytes)
	client.SetRedirectPolicy(resty.NoRedirectPolicy())
	client.SetTLSClientConfig(&tls.Config{MinVersion: tls.VersionTLS12})
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
			logrus.Error(v.Err)
		}
	})
	client.SetBaseURL(apiHost)

	query := map[string]string{
		"node_id": strconv.Itoa(c.NodeID),
	}
	if c.AgentID != "" {
		query["node_type"] = "znode"
		query["type"] = conf.RequiredPanelType
		client.SetHeader("X-ZNode-Version", ClientVersion())
		client.SetHeader("X-ZNode-Type", conf.RequiredPanelType)
		client.SetHeader("X-ZNode-Agent-ID", c.AgentID)
		client.SetHeader("X-ZNode-Instance-ID", effectiveInstanceID(c.AgentInstanceID))
		setInstanceSecretHeader(client)
		client.SetHeader("X-ZNode-Agent-Token", c.Key)
	} else {
		query["node_type"] = "v2node"
		query["token"] = c.Key
	}
	client.SetAuthToken(c.Key)
	setAddressHeaders(client)
	client.SetQueryParams(query)
	return &Client{
		client:         client,
		Token:          c.Key,
		APIHost:        apiHost,
		AgentID:        c.AgentID,
		NodeId:         c.NodeID,
		UserList:       &UserListBody{},
		AliveMap:       &AliveMap{},
		fallbackConfig: cloneGlobalDeviceLimitConfig(c.GlobalDeviceLimitConfig),
	}, nil
}
