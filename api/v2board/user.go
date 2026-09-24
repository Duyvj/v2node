package panel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"encoding/json/v2"
	"github.com/vmihailenco/msgpack/v5"
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
type UserTraffic struct {
	UID              int
	Upload, Download int64
}

func (c *Client) GetUserList(ctx context.Context) ([]UserInfo, error) {
	r, err := c.client.R().SetContext(ctx).SetHeader("If-None-Match", c.userEtag).
		SetHeader("X-Response-Format", "msgpack").SetDoNotParseResponse(true).
		Get("/api/v1/server/UniProxy/user")
	if err != nil {
		return nil, err
	}
	if r == nil || r.RawResponse == nil {
		return nil, fmt.Errorf("empty user response")
	}
	defer r.RawResponse.Body.Close()
	if r.StatusCode() == 304 {
		if c.UserList == nil || c.UserList.Users == nil {
			return nil, fmt.Errorf("304 without initial users")
		}
		// Retry applying the last desired state if a later stage failed.
		return c.UserList.Users, nil
	}
	if r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return nil, fmt.Errorf("user response HTTP %d", r.StatusCode())
	}
	data, err := io.ReadAll(io.LimitReader(r.RawResponse.Body, maxPanelBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPanelBody {
		return nil, errPanelBodyTooLarge
	}
	var users []UserInfo
	if strings.Contains(r.Header().Get("Content-Type"), "application/x-msgpack") {
		users, err = decodeUsersMsgpack(data)
	} else {
		var body UserListBody
		err = json.Unmarshal(data, &body)
		users = body.Users
	}
	if err != nil {
		return nil, fmt.Errorf("invalid user response: %w", err)
	}
	if users == nil {
		return nil, fmt.Errorf("missing users array")
	}
	if len(users) > 100000 {
		return nil, fmt.Errorf("too many users")
	}
	seen := make(map[string]struct{}, len(users))
	for _, u := range users {
		if u.Id <= 0 || u.Uuid == "" || len(u.Uuid) > 512 || u.SpeedLimit < 0 || u.SpeedLimit > 1000000000 || u.DeviceLimit < 0 || u.DeviceLimit > 1000000 {
			return nil, fmt.Errorf("invalid user fields")
		}
		if _, ok := seen[u.Uuid]; ok {
			return nil, fmt.Errorf("duplicate user credential")
		}
		seen[u.Uuid] = struct{}{}
	}
	c.UserList = &UserListBody{Users: users}
	c.userEtag = r.Header().Get("ETag")
	return users, nil
}
func decodeUsersMsgpack(data []byte) ([]UserInfo, error) {
	d := msgpack.NewDecoder(bytes.NewReader(data))
	n, err := d.DecodeMapLen()
	if err != nil {
		return nil, err
	}
	if n < 0 || n > 64 {
		return nil, fmt.Errorf("invalid user response map")
	}
	var users []UserInfo
	for i := 0; i < n; i++ {
		key, err := d.DecodeString()
		if err != nil {
			return nil, err
		}
		if key != "users" {
			if err := d.Skip(); err != nil {
				return nil, err
			}
			continue
		}
		if users != nil {
			return nil, fmt.Errorf("duplicate users field")
		}
		count, err := d.DecodeArrayLen()
		if err != nil {
			return nil, err
		}
		if count < 0 || count > 100000 {
			return nil, fmt.Errorf("invalid user count")
		}
		users = make([]UserInfo, 0, count)
		for j := 0; j < count; j++ {
			var u UserInfo
			if err := d.Decode(&u); err != nil {
				return nil, err
			}
			users = append(users, u)
		}
	}
	if _, err := d.PeekCode(); err != io.EOF {
		return nil, fmt.Errorf("trailing msgpack data")
	}
	return users, nil
}
func (c *Client) GetUserAlive(ctx context.Context) (map[int]int, error) {
	r, err := c.client.R().SetContext(ctx).Get("/api/v1/server/UniProxy/alivelist")
	if err != nil {
		return nil, err
	}
	if r == nil || r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return nil, fmt.Errorf("alive response failed")
	}
	var body AliveMap
	if err := json.Unmarshal(r.Body(), &body); err != nil {
		return nil, fmt.Errorf("invalid alive response")
	}
	if body.Alive == nil {
		body.Alive = make(map[int]int)
	}
	for uid, n := range body.Alive {
		if uid <= 0 || n < 0 {
			return nil, fmt.Errorf("invalid alive count")
		}
	}
	c.AliveMap = &body
	return body.Alive, nil
}
func (c *Client) ReportUserTraffic(ctx context.Context, traffic []UserTraffic) error {
	data := make(map[int][]int64, len(traffic))
	for _, v := range traffic {
		n := data[v.UID]
		if n == nil {
			n = make([]int64, 2)
		}
		n[0] += v.Upload
		n[1] += v.Download
		data[v.UID] = n
	}
	r, err := c.reportClient.R().SetContext(ctx).SetBody(data).Post("/api/v1/server/UniProxy/push")
	if err != nil {
		return err
	}
	if r == nil || r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return fmt.Errorf("traffic report rejected")
	}
	return nil
}
func (c *Client) ReportNodeOnlineUsers(ctx context.Context, data *map[int][]string) error {
	r, err := c.reportClient.R().SetContext(ctx).SetBody(data).Post("/api/v1/server/UniProxy/alive")
	if err != nil {
		return err
	}
	if r == nil || r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return fmt.Errorf("online report rejected")
	}
	return nil
}
