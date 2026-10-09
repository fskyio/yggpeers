package server

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/html"

	"yggpeers/internal/config"
	"yggpeers/internal/protocol"
	"yggpeers/internal/store"
)

type presentationFixture struct {
	server   *Server
	peers    []store.Peer
	probes   []store.Probe
	received time.Time
}

func distributedPresentationFixture(t *testing.T) presentationFixture {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	inputs := []store.PeerInput{
		{URI: "tcp://first.onion:1234", Protocol: "tcp", Host: "first.onion", Port: "1234", Country: "Tor", IsNetwork: true},
		{URI: "tcp://second.onion:1234", Protocol: "tcp", Host: "second.onion", Port: "1234", Country: "Tor", IsNetwork: true},
		{URI: "tcp://third.onion:1234", Protocol: "tcp", Host: "third.onion", Port: "1234", Country: "Tor", IsNetwork: true},
	}
	if err := s.BulkUpsertPeers(inputs); err != nil {
		t.Fatal(err)
	}
	peers, err := s.GetAllPeers()
	if err != nil {
		t.Fatal(err)
	}
	byURI := make(map[string]store.Peer)
	for _, p := range peers {
		byURI[p.URI] = p
	}
	peers = []store.Peer{byURI[inputs[0].URI], byURI[inputs[1].URI], byURI[inputs[2].URI]}
	started := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	var probes []store.Probe
	for i, name := range []string{"West", "East", "North", "Public only"} {
		p, _, err := s.CreateProbe(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CheckInProbe(p.ID, i != 3, started); err != nil {
			t.Fatal(err)
		}
		probes = append(probes, p)
	}
	if _, err := s.AdvanceProbeRounds(started, 10*time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	work, err := s.CheckInProbe(probes[0].ID, true, started)
	if err != nil {
		t.Fatal(err)
	}
	if work == nil {
		t.Fatal("eligible probe received no assignment")
	}
	received := started.Add(time.Second)
	results := [][]protocol.Result{
		{{PeerID: peers[0].ID, Status: protocol.Up}, {PeerID: peers[1].ID, Status: protocol.Down}, {PeerID: peers[2].ID, Status: protocol.Down}},
		{{PeerID: peers[0].ID, Status: protocol.Down}, {PeerID: peers[1].ID, Status: protocol.Down}},
		{{PeerID: peers[1].ID, Status: protocol.Down}},
	}
	for i, report := range results {
		if _, err := s.SubmitProbeResults(probes[i].ID, protocol.ReportRequest{RoundID: work.RoundID, Results: report}, received); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AdvanceProbeRounds(started.Add(time.Minute), 10*time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	srv, err := New(config.Config{CheckInterval: 10 * time.Minute}, s)
	if err != nil {
		t.Fatal(err)
	}
	return presentationFixture{server: srv, peers: peers, probes: probes, received: received}
}

func TestPublicStatusFiltersAndExports(t *testing.T) {
	f := distributedPresentationFixture(t)
	for _, status := range []protocol.Status{protocol.Up, protocol.Down, protocol.Unknown} {
		t.Run(string(status), func(t *testing.T) {
			r := httptest.NewRecorder()
			f.server.ServeHTTP(r, httptest.NewRequest("GET", "/api/v1/peers?status="+string(status), nil))
			if r.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", r.Code, r.Body.String())
			}
			var response peersResponse
			if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Total != 1 || len(response.Peers) != 1 || response.Peers[0].Status != status {
				t.Fatalf("filtered response: %+v", response)
			}
			if response.Online+response.Offline+response.Unknown != response.Total {
				t.Fatalf("incoherent counters: %+v", response)
			}
			p := response.Peers[0]
			if p.LastCheckedAt == nil || p.StatusValidUntil == nil || p.LastRoundID == nil || p.RegisteredProbes != 4 || p.ExpectedProbes != 3 {
				t.Fatalf("missing distributed metadata: %+v", p)
			}
			if status == protocol.Up && (p.SuccessfulProbes != 1 || p.ReportingProbes != 2) {
				t.Fatalf("regional disagreement lost: %+v", p)
			}
			if status == protocol.Unknown && (p.SuccessfulProbes != 0 || p.ReportingProbes != 1) {
				t.Fatalf("missing reports counted as failures: %+v", p)
			}
		})
	}
	for _, path := range []string{"/api/v1/peers?is_up=true", "/api/v1/peers?is_up=", "/peers.json?is_up=false", "/peers.csv?is_up=false", "/api/v1/peers?status=false", "/api/v1/peers?status="} {
		r := httptest.NewRecorder()
		f.server.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400", path, r.Code)
		}
	}
	r := httptest.NewRecorder()
	f.server.ServeHTTP(r, httptest.NewRequest("GET", "/peers.json", nil))
	var response struct {
		Peers []map[string]json.RawMessage `json:"peers"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, p := range response.Peers {
		if _, exists := p["is_up"]; exists {
			t.Fatal("obsolete boolean remains in JSON contract")
		}
		if _, exists := p["coverage_pct"]; !exists {
			t.Fatal("coverage percentage missing")
		}
	}
	r = httptest.NewRecorder()
	f.server.ServeHTTP(r, httptest.NewRequest("GET", "/peers.csv", nil))
	records, err := csv.NewReader(r.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	columns := make(map[string]int)
	for i, column := range records[0] {
		columns[column] = i
	}
	if _, exists := columns["is_up"]; exists {
		t.Fatal("obsolete boolean remains in CSV contract")
	}
	for _, name := range []string{"status", "last_checked_at", "status_valid_until", "successful_probes", "reporting_probes", "expected_probes", "registered_probes", "coverage_pct"} {
		if _, exists := columns[name]; !exists {
			t.Fatalf("missing CSV column %s", name)
		}
	}
	statuses := make(map[string]bool)
	for _, row := range records[1:] {
		statuses[row[columns["status"]]] = true
		if row[columns["registered_probes"]] != "4" || row[columns["expected_probes"]] != "3" {
			t.Fatalf("CSV probe denominators: %v", row)
		}
	}
	if !statuses["up"] || !statuses["down"] || !statuses["unknown"] {
		t.Fatalf("CSV status loss: %v", statuses)
	}
}

func TestPeerPageDistinguishesProbeDisagreementMissingAndIneligible(t *testing.T) {
	f := distributedPresentationFixture(t)
	r := httptest.NewRecorder()
	f.server.ServeHTTP(r, httptest.NewRequest("GET", "/peer/"+strconv.FormatInt(f.peers[0].ID, 10), nil))
	if r.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", r.Code, r.Body.String())
	}
	doc, err := html.Parse(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	rows := make(map[string]*html.Node)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			for _, a := range n.Attr {
				if a.Key == "data-probe-id" {
					rows[a.Val] = n
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	for i, class := range []string{"up", "down", "missing", "ineligible"} {
		row := rows[f.probes[i].ID]
		if row == nil {
			t.Fatalf("probe %s missing", f.probes[i].ID)
		}
		gotClass := ""
		for _, a := range row.Attr {
			if a.Key == "class" {
				gotClass = a.Val
			}
		}
		if gotClass != class {
			t.Errorf("probe %s class = %q, want %q", f.probes[i].ID, gotClass, class)
		}
	}
}
