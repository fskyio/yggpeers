package probe

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"yggpeers/internal/config"
	"yggpeers/internal/protocol"
)

func workerConfig(serverURL string) config.ProbeConfig {
	return config.ProbeConfig{ServerURL: serverURL, Token: "test-credential", DialTimeout: time.Second, MaxConcurrentChecks: 1}
}

func workerTarget(t *testing.T, peer *httptest.Server, id int64) protocol.Target {
	t.Helper()
	host, port, err := net.SplitHostPort(peer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Target{PeerID: id, Protocol: "ws", Host: host, Port: port}
}

func TestWorkerRetriesSameObservationAfterAmbiguousUpload(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer peer.Close()
	target := workerTarget(t, peer, 17)
	uploads := make(chan []byte, 4)
	var attempts atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Error("probe request missing credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case protocol.CheckInPath:
			json.NewEncoder(w).Encode(protocol.WorkResponse{PollAfter: 1000, Work: &protocol.Assignment{
				RoundID: 9, Deadline: time.Now().Add(10 * time.Second), Targets: []protocol.Target{target},
			}})
		case protocol.ResultsPath:
			body, _ := io.ReadAll(r.Body)
			uploads <- body
			if attempts.Add(1) == 1 {
				// Simulate accepting a result but losing its acknowledgement.
				// The peer becomes unavailable before the next upload attempt.
				peer.Close()
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(protocol.ReportResponse{Accepted: 1})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	worker, err := New(workerConfig(api.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	getUpload := func() []byte {
		select {
		case body := <-uploads:
			return body
		case <-ctx.Done():
			t.Fatal("worker did not upload before timeout")
			return nil
		}
	}
	first, second := getUpload(), getUpload()
	if string(first) != string(second) {
		t.Fatalf("retried observation changed after peer went down: %s -> %s", first, second)
	}
	var report protocol.ReportRequest
	if err := json.Unmarshal(second, &report); err != nil {
		t.Fatal(err)
	}
	if report.RoundID != 9 || len(report.Results) != 1 || report.Results[0].PeerID != 17 || report.Results[0].Status != protocol.Up {
		t.Fatalf("incorrect observation: %+v", report)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestExpiredAssignmentLeavesUnfinishedTargetsMissing(t *testing.T) {
	var checked atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		checked.Add(1)
		<-r.Context().Done()
	}))
	defer peer.Close()
	var uploaded atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		uploaded.Add(1)
		json.NewEncoder(w).Encode(protocol.ReportResponse{Accepted: 1})
	}))
	defer api.Close()
	cfg := workerConfig(api.URL)
	cfg.DialTimeout = time.Minute
	worker, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assignment := protocol.Assignment{RoundID: 1, Deadline: time.Now().Add(500 * time.Millisecond), Targets: []protocol.Target{
		workerTarget(t, peer, 1), workerTarget(t, peer, 2),
	}}
	if err := worker.execute(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	if checked.Load() != 1 || uploaded.Load() != 0 {
		t.Fatalf("unfinished targets must stay missing: checked=%d uploads=%d", checked.Load(), uploaded.Load())
	}
}

func TestAPICertificatesAreVerified(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(protocol.WorkResponse{})
	}))
	defer api.Close()
	worker, err := New(workerConfig(api.URL))
	if err != nil {
		t.Fatal(err)
	}
	var response protocol.WorkResponse
	err = worker.post(context.Background(), protocol.CheckInPath, protocol.CheckInRequest{}, &response)
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthority) {
		t.Fatalf("untrusted API certificate was not rejected: %v", err)
	}
}

func TestAuthenticationRejectionStopsWorker(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer api.Close()
	worker, err := New(workerConfig(api.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = worker.Run(ctx)
	var rejection *apiError
	if !errors.As(err, &rejection) || rejection.status != http.StatusUnauthorized {
		t.Fatalf("invalid credential did not stop worker: %v", err)
	}
}

func TestCompletedObservationUploadsWhileSiblingCheckIsPending(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	deadline := time.Now().Add(time.Second)
	accepted := make(chan protocol.Result, 2)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror the server contract: reports at or after the deadline are rejected.
		if !time.Now().Before(deadline) {
			w.WriteHeader(http.StatusGone)
			return
		}
		var report protocol.ReportRequest
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, result := range report.Results {
			accepted <- result
		}
		json.NewEncoder(w).Encode(protocol.ReportResponse{Accepted: len(report.Results)})
	}))
	defer api.Close()
	cfg := workerConfig(api.URL)
	cfg.DialTimeout = time.Minute
	cfg.MaxConcurrentChecks = 2
	worker, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assignment := protocol.Assignment{RoundID: 1, Deadline: deadline, Targets: []protocol.Target{
		workerTarget(t, fast, 1), workerTarget(t, slow, 2),
	}}
	if err := worker.execute(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	close(accepted)
	var got []protocol.Result
	for result := range accepted {
		got = append(got, result)
	}
	if len(got) != 1 || got[0] != (protocol.Result{PeerID: 1, Status: protocol.Up}) {
		t.Fatalf("accepted before deadline = %+v, want only the completed success", got)
	}
}
