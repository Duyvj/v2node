package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/vmihailenco/msgpack/v5"
	"github.com/vmihailenco/msgpack/v5/msgpcode"
)

type OnlineUser struct {
	UID int
	IP  string
}

type UserInfo struct {
	Id          int    `json:"id" msgpack:"id"`
	Uuid        string `json:"uuid" msgpack:"uuid"`
	SpeedLimit  int    `json:"speed_limit" msgpack:"speed_limit"`
	DeviceLimit int    `json:"device_limit" msgpack:"device_limit"`
}

type UserListBody struct {
	Users []UserInfo `json:"users" msgpack:"users"`
}

type AliveMap struct {
	Alive map[int]int `json:"alive"`
}

// GetUserList will pull user from v2board
func (c *Client) GetUserList(ctx context.Context) ([]UserInfo, error) {
	const path = "/api/v1/server/UniProxy/user"
	r, err := c.client.R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.userEtag).
		SetHeader("X-Response-Format", "msgpack").
		SetDoNotParseResponse(true).
		Get(path)
	if err != nil {
		return nil, redactError(err, c.Token)
	}
	if r == nil || r.RawResponse == nil {
		return nil, fmt.Errorf("received nil response or raw response")
	}
	defer r.RawResponse.Body.Close()
	status := r.StatusCode()
	if status == 304 {
		return nil, nil
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("user list request failed with HTTP status %d", status)
	}
	if r.RawResponse.ContentLength > int64(c.maxResponseBytes) {
		return nil, fmt.Errorf("user list response is too large: %d bytes", r.RawResponse.ContentLength)
	}
	limitedBody := &io.LimitedReader{R: r.RawResponse.Body, N: int64(c.maxResponseBytes) + 1}

	var users []UserInfo
	if strings.Contains(r.Header().Get("Content-Type"), "application/x-msgpack") {
		decoder := msgpack.NewDecoder(limitedBody)
		users, err = decodeMsgpackUsers(decoder, c.maxUsers)
		if err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
	} else {
		dec := jsontext.NewDecoder(limitedBody)
		users, err = decodeJSONUsers(dec, c.maxUsers)
		if err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
	}
	if limitedBody.N <= 0 {
		return nil, fmt.Errorf("user list response exceeds limit of %d bytes", c.maxResponseBytes)
	}
	c.userEtag = r.Header().Get("ETag")
	return users, nil
}

func decodeJSONUsers(decoder *jsontext.Decoder, maxUsers int) ([]UserInfo, error) {
	tok, err := decoder.ReadToken()
	if err != nil {
		return nil, err
	}
	if tok.Kind() != '{' {
		return nil, errors.New("expected top-level object")
	}

	users := make([]UserInfo, 0)
	foundUsers := false
	for decoder.PeekKind() != '}' {
		key, err := decoder.ReadToken()
		if err != nil {
			return nil, err
		}
		if key.Kind() != '"' {
			return nil, errors.New("expected top-level object field name")
		}
		if key.String() != "users" {
			if err := decoder.SkipValue(); err != nil {
				return nil, err
			}
			continue
		}
		if foundUsers {
			return nil, errors.New(`duplicate top-level "users" field`)
		}
		foundUsers = true

		tok, err := decoder.ReadToken()
		if err != nil {
			return nil, err
		}
		if tok.Kind() != '[' {
			return nil, errors.New(`expected top-level "users" array`)
		}
		for decoder.PeekKind() != ']' {
			if len(users) >= maxUsers {
				return nil, fmt.Errorf("user list exceeds limit of %d users", maxUsers)
			}
			value, err := decoder.ReadValue()
			if err != nil {
				return nil, fmt.Errorf("read user object: %w", err)
			}
			var user UserInfo
			if err := json.Unmarshal(value, &user); err != nil {
				return nil, fmt.Errorf("unmarshal user: %w", err)
			}
			users = append(users, user)
		}
		if _, err := decoder.ReadToken(); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	if !foundUsers {
		return nil, errors.New(`missing top-level "users" field`)
	}
	if _, err := decoder.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("unexpected trailing JSON value")
		}
		return nil, err
	}
	return users, nil
}

func decodeMsgpackUsers(decoder *msgpack.Decoder, maxUsers int) ([]UserInfo, error) {
	code, err := decoder.PeekCode()
	if err != nil {
		return nil, err
	}
	if !msgpcode.IsFixedMap(code) && code != msgpcode.Map16 && code != msgpcode.Map32 {
		return nil, errors.New("expected top-level map")
	}
	fields, err := decoder.DecodeMapLen()
	if err != nil {
		return nil, err
	}
	if fields < 0 {
		return nil, errors.New("expected top-level map")
	}

	users := make([]UserInfo, 0)
	foundUsers := false
	for i := 0; i < fields; i++ {
		key, err := decoder.DecodeString()
		if err != nil {
			return nil, err
		}
		if key != "users" {
			if err := decoder.Skip(); err != nil {
				return nil, err
			}
			continue
		}
		if foundUsers {
			return nil, errors.New(`duplicate top-level "users" field`)
		}
		foundUsers = true

		count, err := decoder.DecodeArrayLen()
		if err != nil {
			return nil, err
		}
		if count < 0 {
			return nil, errors.New(`expected top-level "users" array`)
		}
		if count > maxUsers {
			return nil, fmt.Errorf("user list exceeds limit of %d users", maxUsers)
		}
		users = make([]UserInfo, 0, count)
		for j := 0; j < count; j++ {
			var user UserInfo
			if err := decoder.Decode(&user); err != nil {
				return nil, err
			}
			users = append(users, user)
		}
	}
	if !foundUsers {
		return nil, errors.New(`missing top-level "users" field`)
	}
	if _, err := decoder.PeekCode(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("unexpected trailing MessagePack value")
	}
	return users, nil
}

// GetUserAlive will fetch the alive_ip count for users
func (c *Client) GetUserAlive(ctx context.Context) (map[int]int, error) {
	c.AliveMap = &AliveMap{}
	const path = "/api/v1/server/UniProxy/alivelist"
	r, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, redactError(err, c.Token)
		}
		c.AliveMap.Alive = make(map[int]int)
		return c.AliveMap.Alive, nil
	}
	if r == nil || r.RawResponse == nil || r.StatusCode() >= 399 {
		c.AliveMap.Alive = make(map[int]int)
		return c.AliveMap.Alive, nil
	}
	defer r.RawResponse.Body.Close()
	if err := json.Unmarshal(r.Body(), c.AliveMap); err != nil {
		fmt.Printf("unmarshal user alive list error: %s", err)
		c.AliveMap.Alive = make(map[int]int)
	}

	return c.AliveMap.Alive, nil
}

type UserTraffic struct {
	UID      int
	Upload   int64
	Download int64
}

// ReportUserTraffic reports the user traffic
func (c *Client) ReportUserTraffic(ctx context.Context, userTraffic []UserTraffic) error {
	data := make(map[int][]int64, len(userTraffic))
	for i := range userTraffic {
		data[userTraffic[i].UID] = []int64{userTraffic[i].Upload, userTraffic[i].Download}
	}
	const path = "/api/v1/server/UniProxy/push"
	_, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return redactError(err, c.Token)
	}
	return nil
}

func (c *Client) ReportNodeOnlineUsers(ctx context.Context, data *map[int][]string) error {
	const path = "/api/v1/server/UniProxy/alive"
	_, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)

	if err != nil {
		return redactError(err, c.Token)
	}

	return nil
}
