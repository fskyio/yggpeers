package server

import (
	"testing"
	"time"

	"yggpeers/internal/endpoint"
	"yggpeers/internal/protocol"
	"yggpeers/internal/store"
)

func TestMatchingPeersUsesDirectFreshAddresses(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	selected := store.Peer{ID: 1, URI: "tcp://alpha.example.com:1234"}
	destination, err := endpoint.Parse(selected.URI)
	if err != nil {
		t.Fatal(err)
	}
	peers := []store.PeerWithStats{
		{Peer: selected},
		{Peer: store.Peer{ID: 2, URI: "tls://ALPHA.example.com.:4321"}},
		{Peer: store.Peer{ID: 3, URI: "quic://beta.example.com:1234"}},
		{Peer: store.Peer{ID: 4, URI: "tcp://gamma.example.com:1234"}},
		{Peer: store.Peer{ID: 5, URI: "tcp://1.1.1.1:5678"}},
		{Peer: store.Peer{ID: 6, URI: "tcp://expired.example.com:1234"}},
		{Peer: store.Peer{ID: 7, URI: "tcp://failed.example.com:1234"}},
		{Peer: store.Peer{ID: 8, URI: "tcp://[::ffff:1.1.1.1]:9999"}},
		{Peer: store.Peer{ID: 9, URI: "tcp://alpha.example.com:5555", IsNetwork: true}},
	}
	hosts := map[string]store.HostMetadata{
		"alpha.example.com":   {Addresses: []string{"1.1.1.1", "8.8.8.8"}, ResolvedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)},
		"beta.example.com":    {Addresses: []string{"8.8.8.8", "9.9.9.9"}, ResolvedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)},
		"gamma.example.com":   {Addresses: []string{"9.9.9.9"}, ResolvedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)},
		"expired.example.com": {Addresses: []string{"1.1.1.1"}, ResolvedAt: now.Add(-time.Hour), ExpiresAt: now},
		"failed.example.com":  {Addresses: []string{"8.8.8.8"}, ResolvedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute), Error: "DNS unavailable"},
	}
	got := matchingPeers(selected, destination, peers, hosts, now)
	want := map[int64]bool{2: true, 3: true, 5: true, 8: true}
	for _, match := range got {
		if !want[match.Peer.ID] {
			t.Errorf("unexpected relation to %d (%s)", match.Peer.ID, match.Peer.URI)
		}
		delete(want, match.Peer.ID)
	}
	if len(want) != 0 {
		t.Errorf("missing related peers: %v", want)
	}
}

func TestMatchingPeersUsesSOCKSDestinationNotProxy(t *testing.T) {
	selected := store.Peer{ID: 1, URI: "socks://localhost:9050/first.onion:1234", IsNetwork: true}
	destination, err := endpoint.Parse(selected.URI)
	if err != nil {
		t.Fatal(err)
	}
	peers := []store.PeerWithStats{
		{Peer: selected},
		{Peer: store.Peer{ID: 2, URI: "sockstls://localhost:9050/first.onion:4567", Port: "9050", IsNetwork: true}},
		{Peer: store.Peer{ID: 3, URI: "socks://localhost:9050/second.onion:1234", Port: "9050", IsNetwork: true}},
	}
	got := matchingPeers(selected, destination, peers, nil, time.Now())
	if len(got) != 1 || got[0].Peer.ID != 2 {
		t.Fatalf("proxy grouping leaked unrelated peers: %+v", got)
	}
	if got[0].Destination.Port != "4567" {
		t.Errorf("related port = %q, want destination port", got[0].Destination.Port)
	}
}

func TestObservedStatusDoesNotTreatMissingOrStaleChecksAsOffline(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	expired := now.Add(-time.Nanosecond)
	cases := []struct {
		name  string
		peer  store.Peer
		class string
	}{
		{"unobserved", store.Peer{Status: protocol.Unknown}, "unknown"},
		{"observed down", store.Peer{Status: protocol.Down, LastCheckedAt: &now, StatusValidUntil: &future}, "down"},
		{"observed up", store.Peer{Status: protocol.Up, LastCheckedAt: &now, StatusValidUntil: &future}, "up"},
		{"expiry boundary", store.Peer{Status: protocol.Up, LastCheckedAt: &now, StatusValidUntil: &now}, "unknown"},
		{"expired up", store.Peer{Status: protocol.Up, LastCheckedAt: &now, StatusValidUntil: &expired}, "unknown"},
		{"expired down", store.Peer{Status: protocol.Down, LastCheckedAt: &now, StatusValidUntil: &expired}, "unknown"},
		{"no validity", store.Peer{Status: protocol.Down, LastCheckedAt: &now}, "unknown"},
		{"incomplete fresh round", store.Peer{Status: protocol.Unknown, StatusValidUntil: &future}, "unknown"},
		{"overlay eligible", store.Peer{Status: protocol.Up, LastCheckedAt: &now, StatusValidUntil: &future, IsNetwork: true}, "up"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := observedStatus(tc.peer, now); got.Class != tc.class {
				t.Errorf("status class = %q, want %q", got.Class, tc.class)
			}
		})
	}
}

func TestHistoryCellsKeepUnknownSeparateFromFailures(t *testing.T) {
	cases := []struct {
		hour       store.HistoryHour
		class      string
		incomplete bool
	}{
		{store.HistoryHour{}, "unknown", false},
		{store.HistoryHour{UnknownCount: 2}, "unknown", true},
		{store.HistoryHour{Checks: 1, UnknownCount: 2}, "down", true},
		{store.HistoryHour{Checks: 1, UpCount: 1, UnknownCount: 2}, "up", true},
		{store.HistoryHour{Checks: 2, UpCount: 1}, "mixed", false},
	}
	for _, tc := range cases {
		got := historyCells([]store.HistoryHour{tc.hour})[0]
		if got.Class != tc.class || got.Unknown != tc.incomplete {
			t.Errorf("hour %+v: class %q, incomplete %v; want %q, %v", tc.hour, got.Class, got.Unknown, tc.class, tc.incomplete)
		}
	}
}
