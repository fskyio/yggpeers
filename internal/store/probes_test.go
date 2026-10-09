package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yggpeers/internal/protocol"
)

var roundTestTime = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func roundFixture(t *testing.T, count int, network bool) (*Store, []Probe, int64) {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "rounds.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.BulkUpsertPeers([]PeerInput{{URI: "tcp://example.com:123", Country: "Test", Protocol: "tcp", Host: "example.com", Port: "123", IsNetwork: network}}); err != nil {
		t.Fatal(err)
	}
	peers, err := st.GetAllPeers()
	if err != nil {
		t.Fatal(err)
	}
	probes := make([]Probe, count)
	for i := range probes {
		probes[i], _, err = st.CreateProbe(string(rune('A' + i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CheckInProbe(probes[i].ID, false, roundTestTime); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AdvanceProbeRounds(roundTestTime, 10*time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	return st, probes, peers[0].ID
}

func assignmentFor(t *testing.T, st *Store, p Probe, now time.Time) *protocol.Assignment {
	t.Helper()
	work, err := st.CheckInProbe(p.ID, p.CheckDarknet, now)
	if err != nil || work == nil {
		t.Fatalf("assignment = %v, error = %v", work, err)
	}
	return work
}

func sendObservation(t *testing.T, st *Store, p Probe, roundID, peerID int64, status protocol.Status, now time.Time) {
	t.Helper()
	if n, err := st.SubmitProbeResults(p.ID, protocol.ReportRequest{RoundID: roundID, Results: []protocol.Result{{PeerID: peerID, Status: status}}}, now); err != nil || n != 1 {
		t.Fatalf("accepted %d observations, error %v", n, err)
	}
}

func TestDistributedReachabilityAndOneSamplePerRound(t *testing.T) {
	cases := []struct {
		name    string
		results []protocol.Status
		want    protocol.Status
		up      int
		known   int
		unknown int
	}{
		{"regional disagreement", []protocol.Status{protocol.Up, protocol.Down}, protocol.Up, 1, 1, 0},
		{"failure arrives first", []protocol.Status{protocol.Down, protocol.Up}, protocol.Up, 1, 1, 0},
		{"all failed", []protocol.Status{protocol.Down, protocol.Down}, protocol.Down, 0, 1, 0},
		{"success with missing probe", []protocol.Status{protocol.Up}, protocol.Up, 1, 1, 0},
		{"failure with missing probe", []protocol.Status{protocol.Down}, protocol.Unknown, 0, 0, 1},
		{"no reports", nil, protocol.Unknown, 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, probes, id := roundFixture(t, 2, false)
			work := assignmentFor(t, st, probes[0], roundTestTime)
			for i, status := range tc.results {
				sendObservation(t, st, probes[i], work.RoundID, id, status, roundTestTime.Add(time.Second))
			}
			// A failed connection by just one probe must not prove downtime.
			if len(tc.results) == 1 && tc.results[0] == protocol.Down {
				before, err := st.GetPeerDetail(id, roundTestTime.Add(2*time.Second))
				if err != nil || before.Status != protocol.Unknown || before.Checks != 0 {
					t.Fatalf("partial failure became downtime: %+v, %v", before, err)
				}
			}
			if _, err := st.AdvanceProbeRounds(roundTestTime.Add(time.Minute), 10*time.Minute, time.Minute); err != nil {
				t.Fatal(err)
			}
			detail, err := st.GetPeerDetail(id, roundTestTime.Add(2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if detail.Status != tc.want || detail.Checks != tc.known || detail.UpCount != tc.up || detail.UnknownCount != tc.unknown {
				t.Fatalf("aggregate = %+v, want status %s, known/up/unknown %d/%d/%d", detail, tc.want, tc.known, tc.up, tc.unknown)
			}
			coverage, err := st.GetPeerProbeCoverage(id)
			if err != nil || coverage.Registered != 2 || coverage.Eligible != 2 || coverage.Reported != len(tc.results) || coverage.Successful != tc.up {
				t.Fatalf("coverage = %+v, %v", coverage, err)
			}
			if len(tc.results) > 0 {
				repeat := protocol.ReportRequest{RoundID: work.RoundID, Results: []protocol.Result{{PeerID: id, Status: tc.results[0]}}}
				if n, err := st.SubmitProbeResults(probes[0].ID, repeat, roundTestTime.Add(2*time.Minute)); err != nil || n != 0 {
					t.Fatalf("acknowledged duplicate after closure = %d, %v", n, err)
				}
			}
			var samples int
			if err := st.db.QueryRow(`SELECT COUNT(*) FROM checks WHERE peer_id = ? AND round_id = ?`, id, work.RoundID).Scan(&samples); err != nil || samples != 1 {
				t.Fatalf("round wrote %d samples, %v", samples, err)
			}
		})
	}
}

func TestRoundDeadlinesAndImmutableObservations(t *testing.T) {
	st, probes, id := roundFixture(t, 2, false)
	work := assignmentFor(t, st, probes[0], roundTestTime)
	sendObservation(t, st, probes[0], work.RoundID, id, protocol.Down, roundTestTime.Add(time.Second))
	conflict := protocol.ReportRequest{RoundID: work.RoundID, Results: []protocol.Result{{PeerID: id, Status: protocol.Up}}}
	if _, err := st.SubmitProbeResults(probes[0].ID, conflict, roundTestTime.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting duplicate error = %v", err)
	}
	if _, err := st.SubmitProbeResults(probes[1].ID, conflict, work.Deadline); !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("result at exact deadline error = %v", err)
	}
	if _, err := st.AdvanceProbeRounds(work.Deadline, 10*time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	detail, err := st.GetPeerDetail(id, work.Deadline.Add(time.Second))
	if err != nil || detail.Status != protocol.Unknown || detail.UnknownCount != 1 || detail.UpCount != 0 {
		t.Fatalf("late success changed incomplete round: %+v, %v", detail, err)
	}
}

func TestHandshakeIdentityFromLatestSuccess(t *testing.T) {
	st, probes, id := roundFixture(t, 2, false)
	work := assignmentFor(t, st, probes[0], roundTestTime)
	key := strings.Repeat("ab", 32)
	report := func(r protocol.Result) protocol.ReportRequest {
		return protocol.ReportRequest{RoundID: work.RoundID, Results: []protocol.Result{r}}
	}
	for _, invalid := range []protocol.Result{
		{PeerID: id, Status: protocol.Down, Key: key, ProtocolVersion: "0.5"},
		{PeerID: id, Status: protocol.Up, Key: strings.ToUpper(key), ProtocolVersion: "0.5"},
		{PeerID: id, Status: protocol.Up, Key: key},
	} {
		if _, err := st.SubmitProbeResults(probes[0].ID, report(invalid), roundTestTime); !errors.Is(err, ErrInvalidReport) {
			t.Fatalf("%+v accepted: %v", invalid, err)
		}
	}
	seen := roundTestTime.Add(time.Second)
	up := protocol.Result{PeerID: id, Status: protocol.Up, Key: key, ProtocolVersion: "0.5"}
	if n, err := st.SubmitProbeResults(probes[0].ID, report(up), seen); err != nil || n != 1 {
		t.Fatalf("accepted %d, error %v", n, err)
	}
	if n, err := st.SubmitProbeResults(probes[0].ID, report(up), seen.Add(time.Second)); err != nil || n != 0 {
		t.Fatalf("identical retry accepted %d, error %v", n, err)
	}
	other := up
	other.Key = strings.Repeat("cd", 32)
	if _, err := st.SubmitProbeResults(probes[0].ID, report(other), seen.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry with another key error = %v", err)
	}
	// A probe that predates handshake reporting must not erase the identity.
	sendObservation(t, st, probes[1], assignmentFor(t, st, probes[1], seen).RoundID, id, protocol.Up, seen.Add(2*time.Second))
	detail, err := st.GetPeerDetail(id, seen.Add(3*time.Second))
	if err != nil || detail.NodeKey != key || detail.ProtocolVersion != "0.5" || detail.NodeSeenAt == nil || !detail.NodeSeenAt.Equal(seen) {
		t.Fatalf("identity = %q %q %v, error %v", detail.NodeKey, detail.ProtocolVersion, detail.NodeSeenAt, err)
	}
}

func TestRoundRosterAndOverlayEligibility(t *testing.T) {
	st, probes, id := roundFixture(t, 2, true)
	detail, err := st.GetPeerDetail(id, roundTestTime.Add(time.Second))
	if err != nil || detail.Status != protocol.Unknown || detail.ExpectedProbes != 0 || detail.RegisteredProbes != 2 || detail.UnknownCount != 1 {
		t.Fatalf("no capable probes = %+v, %v", detail, err)
	}
	probes[0].CheckDarknet = true
	if _, err := st.CheckInProbe(probes[0].ID, true, roundTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	late, _, err := st.CreateProbe("late registration")
	if err != nil {
		t.Fatal(err)
	}
	if work, err := st.CheckInProbe(late.ID, false, roundTestTime.Add(time.Second)); err != nil || work != nil {
		t.Fatalf("new registration joined existing round: %v, %v", work, err)
	}
	next := roundTestTime.Add(10 * time.Minute)
	if _, err := st.AdvanceProbeRounds(next, 10*time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	work := assignmentFor(t, st, probes[0], next)
	if len(work.Targets) != 1 || !work.Targets[0].IsNetwork {
		t.Fatalf("overlay assignment = %+v", work)
	}
	// Capability changes apply to later rounds, not an outstanding assignment.
	if work, err := st.CheckInProbe(probes[0].ID, false, next); err != nil || work == nil {
		t.Fatalf("capability update mutated round membership: %+v, %v", work, err)
	}
	sendObservation(t, st, probes[0], work.RoundID, id, protocol.Up, next.Add(time.Second))
	coverage, err := st.GetPeerProbeCoverage(id)
	if err != nil || coverage.Registered != 3 || coverage.Eligible != 1 || coverage.Reported != 1 || coverage.Successful != 1 {
		t.Fatalf("overlay coverage = %+v, %v", coverage, err)
	}
}

func TestReportBatchRollbackAndCredentialRevocation(t *testing.T) {
	st, probes, id := roundFixture(t, 2, false)
	work := assignmentFor(t, st, probes[0], roundTestTime)
	batch := protocol.ReportRequest{RoundID: work.RoundID, Results: []protocol.Result{
		{PeerID: id, Status: protocol.Up}, {PeerID: id, Status: protocol.Down},
	}}
	if _, err := st.SubmitProbeResults(probes[0].ID, batch, roundTestTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting batch error = %v", err)
	}
	var rows int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM probe_results`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed batch left %d observations, %v", rows, err)
	}
	p, token, err := st.CreateProbe("credential test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticateProbe(token); err != nil {
		t.Fatal(err)
	}
	newToken, err := st.RotateProbeToken(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticateProbe(token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old credential remains accepted: %v", err)
	}
	if _, err := st.AuthenticateProbe(newToken); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProbeEnabled(p.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticateProbe(newToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled probe remains authenticated: %v", err)
	}
	if err := st.SetProbeEnabled(probes[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SubmitProbeResults(probes[0].ID, protocol.ReportRequest{RoundID: work.RoundID, Results: []protocol.Result{{PeerID: id, Status: protocol.Up}}}, roundTestTime); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled assignment accepted: %v", err)
	}
}

func TestConcurrentReportsAndPersistedRoundRecovery(t *testing.T) {
	st, probes, id := roundFixture(t, 2, false)
	work := assignmentFor(t, st, probes[0], roundTestTime)
	results := make(chan error, 2)
	for i, p := range probes {
		status := protocol.Down
		if i == 0 {
			status = protocol.Up
		}
		go func(p Probe, status protocol.Status) {
			_, err := st.SubmitProbeResults(p.ID, protocol.ReportRequest{RoundID: work.RoundID, Results: []protocol.Result{{PeerID: id, Status: status}}}, roundTestTime.Add(time.Second))
			results <- err
		}(p, status)
	}
	for range probes {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	detail, err := st.GetPeerDetail(id, roundTestTime.Add(2*time.Second))
	if err != nil || detail.Status != protocol.Up || detail.SuccessfulProbes != 1 || detail.ReportingProbes != 2 || detail.Checks != 1 {
		t.Fatalf("concurrent aggregation = %+v, %v", detail, err)
	}
	// Reopen the actual database with an incomplete second round.
	next := roundTestTime.Add(10 * time.Minute)
	if _, err := st.AdvanceProbeRounds(next, 10*time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	work = assignmentFor(t, st, probes[0], next)
	sendObservation(t, st, probes[0], work.RoundID, id, protocol.Down, next.Add(time.Second))
	var path string
	var seq int
	var dbName string
	if err := st.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &path); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.AdvanceProbeRounds(work.Deadline, 10*time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	detail, err = reopened.GetPeerDetail(id, work.Deadline.Add(time.Second))
	if err != nil || detail.Status != protocol.Unknown || detail.Checks != 1 || detail.UpCount != 1 || detail.UnknownCount != 1 {
		t.Fatalf("restart lost or duplicated round: %+v, %v", detail, err)
	}
}

func TestCatalogRemovalCannotReviveAnOldAssignment(t *testing.T) {
	st, probes, id := roundFixture(t, 1, false)
	work := assignmentFor(t, st, probes[0], roundTestTime)
	if err := st.BulkUpsertPeers([]PeerInput{{URI: "tcp://other:123", Country: "Test", Protocol: "tcp", Host: "other", Port: "123"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.BulkUpsertPeers([]PeerInput{{URI: "tcp://example.com:123", Country: "Test", Protocol: "tcp", Host: "example.com", Port: "123"}}); err != nil {
		t.Fatal(err)
	}
	peers, err := st.GetAllPeers()
	if err != nil || len(peers) != 1 || peers[0].ID == id {
		t.Fatalf("replacement identity = %+v, %v", peers, err)
	}
	if n, err := st.SubmitProbeResults(probes[0].ID, protocol.ReportRequest{RoundID: work.RoundID, Results: []protocol.Result{{PeerID: id, Status: protocol.Up}}}, roundTestTime.Add(time.Second)); err != nil || n != 0 {
		t.Fatalf("removed target acknowledgement = %d, %v", n, err)
	}
	detail, err := st.GetPeerDetail(peers[0].ID, roundTestTime.Add(2*time.Second))
	if err != nil || detail.Status != protocol.Unknown || detail.Checks != 0 || detail.LastCheckedAt != nil {
		t.Fatalf("old result affected returning URI: %+v, %v", detail, err)
	}
}
