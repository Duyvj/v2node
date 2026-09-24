package panel

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
	"github.com/wyx2685/v2node/conf"
)

// Panel is the interface for different panel's api.

type Client struct {
	client           *resty.Client
	reportClient     *resty.Client
	transport        *boundedTransport
	APIHost          string
	Token            string
	NodeId           int
	nodeEtag         string
	userEtag         string
	responseBodyHash string
	UserList         *UserListBody
	AliveMap         *AliveMap
}

func New(c *conf.NodeConfig) (*Client, error) {
	client := resty.New()
	transport := &boundedTransport{base: http.DefaultTransport.(*http.Transport).Clone()}
	transport.base.MaxIdleConns = 32
	transport.base.MaxIdleConnsPerHost = 4
	transport.base.MaxConnsPerHost = 8
	client.SetTransport(transport)
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
			logrus.Error(v.Err)
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
		client:       client,
		reportClient: client.Clone().SetRetryCount(0),
		transport:    transport,
		Token:        c.Key,
		APIHost:      c.APIHost,
		NodeId:       c.NodeID,
		UserList:     &UserListBody{},
		AliveMap:     &AliveMap{},
	}, nil
}

func (c *Client) Close() {
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}
