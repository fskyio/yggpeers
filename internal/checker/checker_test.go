package checker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"yggpeers/internal/config"
	"yggpeers/internal/protocol"
)

func httpTarget(t *testing.T, server *httptest.Server, transport string) protocol.Target {
	t.Helper()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Target{PeerID: 42, Protocol: transport, Host: host, Port: port}
}

func TestReachabilitySemantics(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer secure.Close()
	c := New(config.ProbeConfig{DialTimeout: time.Second})
	for _, transport := range []string{"tcp", "tls", "ws", "wss"} {
		t.Run(transport, func(t *testing.T) {
			server := plain
			if transport == "wss" {
				server = secure
			}
			result, err := c.Check(context.Background(), httpTarget(t, server, transport))
			if err != nil || result.PeerID != 42 || result.Status != protocol.Up {
				t.Fatalf("reachable %s: result=%+v err=%v", transport, result, err)
			}
		})
	}
}

func TestAssignmentCancellationIsNotDown(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := New(config.ProbeConfig{DialTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result protocol.Result
		err    error
	}
	done := make(chan outcome, 1)
	target := httpTarget(t, server, "ws")
	go func() {
		result, err := c.Check(ctx, target)
		done <- outcome{result, err}
	}()
	<-started
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) || got.result.Status != "" {
		t.Fatalf("cancellation produced an observation: %+v, %v", got.result, got.err)
	}
}

func TestTargetTimeoutIsDown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	c := New(config.ProbeConfig{DialTimeout: 50 * time.Millisecond})
	result, err := c.Check(context.Background(), httpTarget(t, server, "ws"))
	if err != nil || result.Status != protocol.Down {
		t.Fatalf("target timeout: result=%+v err=%v", result, err)
	}
}
