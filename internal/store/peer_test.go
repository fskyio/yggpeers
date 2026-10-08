package store

import (
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"
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
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestObservedStateTransitionsAndPruning(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://example.com:123")
	unknown := historyDetail(t, s, id, time.Now())
	if unknown.LastCheckedAt != nil || unknown.IsUp || unknown.UptimePct != nil {
		t.Fatalf("new peer must be unobserved: %+v", unknown)
	}
	if err := s.RecordChecks([]CheckResult{{PeerID: id, IsUp: false}}); err != nil {
		t.Fatal(err)
	}
	first := historyDetail(t, s, id, time.Now())
	if first.LastCheckedAt == nil || !first.StateChangedAt.Equal(*first.LastCheckedAt) || first.StateChangedAt.Equal(unknown.StateChangedAt) {
		t.Fatalf("first-down baseline is not first observation: %+v", first)
	}
	// Put the baseline and freshness in the past so repeated calls need no sleep.
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := s.db.Exec(`UPDATE peers SET state_changed_at = ?, last_checked_at = ? WHERE id = ?`, old, old, id); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChecks([]CheckResult{{PeerID: id, IsUp: false}}); err != nil {
		t.Fatal(err)
	}
	repeated := historyDetail(t, s, id, time.Now())
	if repeated.LastCheckedAt == nil || !repeated.LastCheckedAt.After(old) || !repeated.StateChangedAt.Equal(old) || repeated.IsUp {
		t.Fatalf("repeated down changed baseline or missed freshness: %+v", repeated)
	}
	if err := s.RecordChecks([]CheckResult{{PeerID: id, IsUp: true}}); err != nil {
		t.Fatal(err)
	}
	changed := historyDetail(t, s, id, time.Now())
	if !changed.IsUp || changed.LastCheckedAt == nil || !changed.StateChangedAt.Equal(*changed.LastCheckedAt) || !changed.StateChangedAt.After(old) {
		t.Fatalf("up transition not recorded: %+v", changed)
	}
	if changed.UptimePct == nil || math.Abs(*changed.UptimePct-100.0/3.0) > 1e-10 {
		t.Fatalf("daily uptime = %v, want one of three checks up", changed.UptimePct)
	}
	if _, err := s.db.Exec(`UPDATE peers SET state_changed_at = ?, last_checked_at = ? WHERE id = ?`, old, old, id); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChecks([]CheckResult{{PeerID: id, IsUp: true}}); err != nil {
		t.Fatal(err)
	}
	repeatedUp := historyDetail(t, s, id, time.Now())
	if repeatedUp.LastCheckedAt == nil || !repeatedUp.LastCheckedAt.After(old) || !repeatedUp.StateChangedAt.Equal(old) || !repeatedUp.IsUp {
		t.Fatalf("repeated up changed baseline or missed freshness: %+v", repeatedUp)
	}
	if _, err := s.db.Exec(`UPDATE checks SET checked_at = ? WHERE peer_id = ?;
		UPDATE peers SET last_checked_at = ? WHERE id = ?`, old, id, old, id); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneOldChecks(7); err != nil {
		t.Fatal(err)
	}
	pruned := historyDetail(t, s, id, time.Now())
	if pruned.LastCheckedAt == nil || !pruned.LastCheckedAt.Equal(old) || pruned.Checks != 0 {
		t.Fatalf("pruning lost freshness or retained old history: %+v", pruned)
	}
}

func TestRecordChecksRollback(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://example.com:123")
	if err := s.RecordChecks([]CheckResult{{PeerID: id, IsUp: true}, {PeerID: id + 100, IsUp: true}}); err == nil {
		t.Fatal("expected missing peer to reject batch")
	}
	detail := historyDetail(t, s, id, time.Now())
	if detail.IsUp || detail.LastCheckedAt != nil || detail.Checks != 0 || detail.UptimePct != nil {
		t.Fatalf("failed batch persisted partial observations: %+v", detail)
	}
}

func TestPeerHistoryExactWindow(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://example.com:123")
	now := time.Date(2026, 10, 8, 12, 34, 56, 123456789, time.FixedZone("offset", 2*60*60))
	start := now.UTC().Add(-168 * time.Hour)
	samples := []struct {
		at time.Time
		up bool
	}{
		{start.Add(-time.Nanosecond), true}, // retained physical row outside window
		{start, true},
		{start.Add(time.Hour - time.Nanosecond), false},
		{start.Add(time.Hour), true},
		{start.Add(time.Hour + time.Minute), true},
		{start.Add(2 * time.Hour), false},
		{start.Add(2*time.Hour + time.Minute), false},
		{now.UTC().Add(-time.Nanosecond), true},
		{now.UTC(), false},               // half-open end
		{now.UTC().Add(time.Hour), true}, // future row
	}
	for _, sample := range samples {
		if _, err := s.db.Exec(`INSERT INTO checks (peer_id, checked_at, is_up) VALUES (?, ?, ?)`, id, sample.at.UTC(), sample.up); err != nil {
			t.Fatal(err)
		}
	}
	detail := historyDetail(t, s, id, now)
	if len(detail.Hours) != 168 || !detail.WindowStart.Equal(start) || !detail.WindowEnd.Equal(now) || detail.Checks != 7 || detail.UpCount != 4 {
		t.Fatalf("incorrect rolling window: %+v", detail)
	}
	for i, hour := range detail.Hours {
		wantStart := start.Add(time.Duration(i) * time.Hour)
		if hour.Start.Location() != time.UTC || hour.End.Location() != time.UTC || !hour.Start.Equal(wantStart) || !hour.End.Equal(wantStart.Add(time.Hour)) {
			t.Fatalf("hour %d boundaries = %+v", i, hour)
		}
		wantChecks, wantUp := 0, 0
		switch i {
		case 0:
			wantChecks, wantUp = 2, 1 // mixed
		case 1:
			wantChecks, wantUp = 2, 2 // all pass
		case 2:
			wantChecks, wantUp = 2, 0 // all fail
		case 167:
			wantChecks, wantUp = 1, 1
		}
		if hour.Checks != wantChecks || hour.UpCount != wantUp {
			t.Fatalf("hour %d = %+v, want checks %d/up %d", i, hour, wantChecks, wantUp)
		}
	}
}

func TestPeerDetailDeletedPeer(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://example.com:123")
	if err := s.RecordChecks([]CheckResult{{PeerID: id, IsUp: true}}); err != nil {
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
