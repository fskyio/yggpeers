package store

import (
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"yggpeers/internal/protocol"
)

func newHistoryStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addHistoryPeer(t *testing.T, s *Store, uri string) int64 {
	t.Helper()
	result, err := s.db.Exec(`INSERT INTO peers (uri, country, protocol, host, port, state_changed_at)
		VALUES (?, 'Test', 'tcp', 'example.com', '123', '2000-01-01 00:00:00')`, uri)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func historyDetail(t *testing.T, s *Store, id int64, now time.Time) PeerDetail {
	t.Helper()
	detail, err := s.GetPeerDetail(id, now)
	if err != nil {
		t.Fatal(err)
	}
	return detail
}

func TestLastCheckedMigrationBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE peers (
			id INTEGER PRIMARY KEY, uri TEXT UNIQUE NOT NULL, country TEXT NOT NULL,
			protocol TEXT NOT NULL, host TEXT NOT NULL, port TEXT NOT NULL,
			is_up INTEGER NOT NULL DEFAULT 0, is_network INTEGER NOT NULL DEFAULT 0,
			state_changed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
		CREATE TABLE checks (
			id INTEGER PRIMARY KEY, peer_id INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
			checked_at DATETIME NOT NULL, is_up INTEGER NOT NULL);
		INSERT INTO peers (id, uri, country, protocol, host, port) VALUES
			(1, 'tcp://observed:1', 'Test', 'tcp', 'observed', '1'),
			(2, 'tcp://unknown:1', 'Test', 'tcp', 'unknown', '1');
		INSERT INTO checks (peer_id, checked_at, is_up) VALUES
			(1, '2025-01-03 04:05:06', 0), (1, '2025-01-02 04:05:06', 1);
	`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2025, 1, 3, 4, 5, 6, 0, time.UTC)
	// Reopen twice to prove migration is idempotent and missing checks remain unknown.
	for range 2 {
		s, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		observed := historyDetail(t, s, 1, want.Add(time.Hour))
		unknown := historyDetail(t, s, 2, want.Add(time.Hour))
		if observed.LastCheckedAt == nil || !observed.LastCheckedAt.Equal(want) {
			t.Fatalf("backfilled last check = %v, want %v", observed.LastCheckedAt, want)
		}
		if unknown.LastCheckedAt != nil || unknown.UptimePct != nil {
			t.Fatalf("never-observed peer acquired observations: %+v", unknown)
		}
		if observed.UptimePct == nil || *observed.UptimePct != 50 {
			t.Fatalf("legacy rollup uptime = %v, want 50", observed.UptimePct)
		}
		if observed.Status != protocol.Unknown || unknown.Status != protocol.Unknown || observed.StatusValidUntil != nil {
			t.Fatalf("legacy state must have no current evidence: observed=%+v unknown=%+v", observed, unknown)
		}
		if observed.Checks != 2 || observed.UpCount != 1 || observed.UnknownCount != 0 || observed.CoveragePct == nil || *observed.CoveragePct != 100 {
			t.Fatalf("legacy history changed: %+v", observed)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestObservedStateTransitionsAndPruning(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://example.com:123")
	now := time.Now().UTC().Truncate(time.Second)
	unknown := historyDetail(t, s, id, now)
	if unknown.LastCheckedAt != nil || unknown.Status != protocol.Unknown || unknown.UptimePct != nil || unknown.CoveragePct != nil {
		t.Fatalf("new peer must be unobserved: %+v", unknown)
	}
	probe, _, err := s.CreateProbe("history")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckInProbe(probe.ID, false, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	observe := func(at time.Time, status protocol.Status) PeerDetail {
		t.Helper()
		if _, err := s.AdvanceProbeRounds(at, time.Minute, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		work, err := s.CheckInProbe(probe.ID, false, at)
		if err != nil || work == nil {
			t.Fatalf("assignment = %v, error = %v", work, err)
		}
		if _, err := s.SubmitProbeResults(probe.ID, protocol.ReportRequest{
			RoundID: work.RoundID, Results: []protocol.Result{{PeerID: id, Status: status}},
		}, at.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		return historyDetail(t, s, id, at.Add(2*time.Second))
	}
	first := observe(now, protocol.Down)
	if first.Status != protocol.Down || first.LastCheckedAt == nil || !first.StateChangedAt.Equal(*first.LastCheckedAt) || first.StateChangedAt.Equal(unknown.StateChangedAt) {
		t.Fatalf("first-down baseline is not first observation: %+v", first)
	}
	repeated := observe(now.Add(time.Minute), protocol.Down)
	if repeated.LastCheckedAt == nil || !repeated.LastCheckedAt.After(*first.LastCheckedAt) || !repeated.StateChangedAt.Equal(first.StateChangedAt) || repeated.Status != protocol.Down {
		t.Fatalf("repeated down changed baseline or missed freshness: %+v", repeated)
	}
	changed := observe(now.Add(2*time.Minute), protocol.Up)
	if changed.Status != protocol.Up || changed.LastCheckedAt == nil || !changed.StateChangedAt.Equal(*changed.LastCheckedAt) || !changed.StateChangedAt.After(first.StateChangedAt) {
		t.Fatalf("up transition not recorded: %+v", changed)
	}
	if changed.UptimePct == nil || math.Abs(*changed.UptimePct-100.0/3.0) > 1e-10 {
		t.Fatalf("daily uptime = %v, want one of three checks up", changed.UptimePct)
	}
	repeatedUp := observe(now.Add(3*time.Minute), protocol.Up)
	if repeatedUp.LastCheckedAt == nil || !repeatedUp.LastCheckedAt.After(*changed.LastCheckedAt) || !repeatedUp.StateChangedAt.Equal(changed.StateChangedAt) || repeatedUp.Status != protocol.Up {
		t.Fatalf("repeated up changed baseline or missed freshness: %+v", repeatedUp)
	}
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := s.db.Exec(`UPDATE checks SET checked_at = ? WHERE peer_id = ?;
		UPDATE peers SET last_checked_at = ? WHERE id = ?;
		UPDATE daily_peer_stats SET day = DATE(day, '-10000 days') WHERE peer_id = ?`, sqliteTime(old), id, sqliteTime(old), id, id); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneOldChecks(7); err != nil {
		t.Fatal(err)
	}
	pruned := historyDetail(t, s, id, now.Add(4*time.Minute))
	if pruned.LastCheckedAt == nil || !pruned.LastCheckedAt.Equal(old) || pruned.Checks != 0 || pruned.UptimePct != nil || pruned.CoveragePct != nil {
		t.Fatalf("pruning lost freshness or retained old history: %+v", pruned)
	}
}

func TestPeerHistoryExactWindow(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://example.com:123")
	now := time.Date(2026, 10, 8, 12, 34, 56, 123456789, time.FixedZone("offset", 2*60*60))
	start := now.UTC().Add(-168 * time.Hour)
	samples := []struct {
		at     time.Time
		status protocol.Status
	}{
		{start.Add(-time.Nanosecond), protocol.Up}, // retained physical row outside window
		{start, protocol.Up},
		{start.Add(time.Hour - time.Nanosecond), protocol.Down},
		{start.Add(time.Hour), protocol.Up},
		{start.Add(time.Hour + time.Minute), protocol.Up},
		{start.Add(2 * time.Hour), protocol.Down},
		{start.Add(2*time.Hour + time.Minute), protocol.Down},
		{start.Add(3 * time.Hour), protocol.Unknown},
		{now.UTC().Add(-time.Nanosecond), protocol.Up},
		{now.UTC(), protocol.Unknown},           // half-open end
		{now.UTC().Add(time.Hour), protocol.Up}, // future row
	}
	for _, sample := range samples {
		if _, err := s.db.Exec(`INSERT INTO checks (peer_id, checked_at, status) VALUES (?, ?, ?)`, id, sqliteTime(sample.at), sample.status); err != nil {
			t.Fatal(err)
		}
	}
	detail := historyDetail(t, s, id, now)
	if len(detail.Hours) != 168 || !detail.WindowStart.Equal(start) || !detail.WindowEnd.Equal(now) || detail.Checks != 7 || detail.UpCount != 4 || detail.UnknownCount != 1 {
		t.Fatalf("incorrect rolling window: %+v", detail)
	}
	for i, hour := range detail.Hours {
		wantStart := start.Add(time.Duration(i) * time.Hour)
		if hour.Start.Location() != time.UTC || hour.End.Location() != time.UTC || !hour.Start.Equal(wantStart) || !hour.End.Equal(wantStart.Add(time.Hour)) {
			t.Fatalf("hour %d boundaries = %+v", i, hour)
		}
		wantChecks, wantUp, wantUnknown := 0, 0, 0
		switch i {
		case 0:
			wantChecks, wantUp = 2, 1 // mixed
		case 1:
			wantChecks, wantUp = 2, 2 // all pass
		case 2:
			wantChecks, wantUp = 2, 0 // all fail
		case 3:
			wantUnknown = 1
		case 167:
			wantChecks, wantUp = 1, 1
		}
		if hour.Checks != wantChecks || hour.UpCount != wantUp || hour.UnknownCount != wantUnknown {
			t.Fatalf("hour %d = %+v, want checks %d/up %d", i, hour, wantChecks, wantUp)
		}
	}
}

func TestPeerDetailDeletedPeer(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://example.com:123")
	if _, err := s.db.Exec(`INSERT INTO checks(peer_id, checked_at, status) VALUES (?, ?, 'up');
		INSERT INTO daily_peer_stats(peer_id, day, checks, up_count) VALUES (?, DATE('now'), 1, 1)`,
		id, sqliteTime(time.Now()), id); err != nil {
		t.Fatal(err)
	}
	if err := s.BulkUpsertPeers([]PeerInput{{URI: "tcp://replacement:123", Country: "Test", Protocol: "tcp", Host: "replacement", Port: "123"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPeerDetail(id, time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted peer error = %v, want sql.ErrNoRows", err)
	}
	var checks, daily int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM checks WHERE peer_id = ?`, id).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM daily_peer_stats WHERE peer_id = ?`, id).Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if checks != 0 || daily != 0 {
		t.Fatalf("deleted peer retained check history: checks=%d daily=%d", checks, daily)
	}
}
