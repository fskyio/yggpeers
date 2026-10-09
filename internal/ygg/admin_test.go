package ygg

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

const nodeKey = "0000000087ee9949eeab56bd430ee8f324cad55abf3993ed9b9be63ce693e18a"

// serveAdmin answers one request per connection like Yggdrasil's admin socket.
func serveAdmin(t *testing.T, respond func(name string, args map[string]string) any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var request struct {
				Request   string            `json:"request"`
				Arguments map[string]string `json:"arguments"`
			}
			if json.NewDecoder(conn).Decode(&request) == nil {
				json.NewEncoder(conn).Encode(respond(request.Request, request.Arguments))
			}
			conn.Close()
		}
	}()
	return "unix://" + path
}

func TestNodeInfo(t *testing.T) {
	endpoint := serveAdmin(t, func(name string, args map[string]string) any {
		if !strings.EqualFold(name, "getnodeinfo") || args["key"] != nodeKey {
			return map[string]any{"status": "error", "error": "unknown action '" + name + "'"}
		}
		return map[string]any{"status": "success", "response": map[string]any{nodeKey: map[string]any{
			"buildname": "yggdrasil", "buildversion": "0.5.14", "buildplatform": "linux", "buildarch": "amd64", "name": "operator",
		}}}
	})
	admin, err := NewAdmin(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	info, err := admin.NodeInfo(context.Background(), nodeKey)
	want := NodeInfo{BuildName: "yggdrasil", BuildVersion: "0.5.14", BuildPlatform: "linux", BuildArch: "amd64"}
	if err != nil || info != want {
		t.Fatalf("NodeInfo = %+v, %v; want %+v", info, err, want)
	}
}

func TestNodeInfoReportsAdminErrors(t *testing.T) {
	endpoint := serveAdmin(t, func(string, map[string]string) any {
		return map[string]any{"status": "error", "error": "timed out waiting for response"}
	})
	admin, err := NewAdmin(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.NodeInfo(context.Background(), nodeKey); err == nil || !strings.Contains(err.Error(), "timed out waiting for response") {
		t.Fatalf("error = %v", err)
	}
}
