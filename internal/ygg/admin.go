package ygg

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
)

// Admin queries a local Yggdrasil node through its admin socket.
type Admin struct {
	network, address string
}

// NewAdmin accepts the endpoint forms yggdrasilctl uses: unix:///path or
// tcp://host:port.
func NewAdmin(endpoint string) (*Admin, error) {
	u, err := url.Parse(endpoint)
	if err == nil {
		switch {
		case u.Scheme == "unix" && u.Path != "":
			return &Admin{network: "unix", address: u.Path}, nil
		case u.Scheme == "tcp" && u.Host != "":
			return &Admin{network: "tcp", address: u.Host}, nil
		}
	}
	return nil, fmt.Errorf("admin endpoint %q: want unix:///path or tcp://host:port", endpoint)
}

// NodeInfo holds the build details a node publishes unless it enables
// NodeInfoPrivacy, in which case every field is empty.
type NodeInfo struct {
	BuildName     string
	BuildVersion  string
	BuildPlatform string
	BuildArch     string
}

// NodeInfo asks the local node to fetch the NodeInfo of key, a lowercase hex
// public key, over the network. The node gives up after six seconds.
func (a *Admin) NodeInfo(ctx context.Context, key string) (NodeInfo, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, a.network, a.address)
	if err != nil {
		return NodeInfo{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	request := struct {
		Request   string            `json:"request"`
		Arguments map[string]string `json:"arguments"`
	}{"getNodeInfo", map[string]string{"key": key}}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return NodeInfo{}, err
	}
	var response struct {
		Status   string                     `json:"status"`
		Error    string                     `json:"error"`
		Response map[string]json.RawMessage `json:"response"`
	}
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return NodeInfo{}, cmp.Or(ctx.Err(), err)
	}
	if response.Status != "success" {
		return NodeInfo{}, fmt.Errorf("getNodeInfo: %s", cmp.Or(response.Error, response.Status))
	}
	raw, ok := response.Response[key]
	if !ok {
		return NodeInfo{}, errors.New("getNodeInfo: response lacks the requested key")
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return NodeInfo{}, fmt.Errorf("getNodeInfo: %w", err)
	}
	text := func(name string) string {
		s, _ := fields[name].(string)
		return s
	}
	return NodeInfo{
		BuildName:     text("buildname"),
		BuildVersion:  text("buildversion"),
		BuildPlatform: text("buildplatform"),
		BuildArch:     text("buildarch"),
	}, nil
}
