package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"yggpeers/internal/protocol"
)

func TestLegacyStatusMigrationPreservesExistingRollups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE peers (
			id INTEGER PRIMARY KEY AUTOINCREMENT, uri TEXT UNIQUE NOT NULL, country TEXT NOT NULL,
			protocol TEXT NOT NULL, host TEXT NOT NULL, port TEXT NOT NULL,
			is_up INTEGER NOT NULL DEFAULT 0, is_network INTEGER NOT NULL DEFAULT 0,
			state_changed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, last_checked_at DATETIME);
		CREATE TABLE checks (
			id INTEGER PRIMARY KEY AUTOINCREMENT, peer_id INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
			checked_at DATETIME NOT NULL, is_up INTEGER NOT NULL);
		CREATE TABLE daily_peer_stats (
			peer_id INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE, day TEXT NOT NULL,
			checks INTEGER NOT NULL DEFAULT 0, up_count INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(peer_id,day));
		INSERT INTO peers(id,uri,country,protocol,host,port,is_up,last_checked_at)
			VALUES (42,'tcp://legacy:1','Test','tcp','legacy','1',1,'2026-01-03 12:00:00');
		INSERT INTO checks(id,peer_id,checked_at,is_up) VALUES
			(71,42,'2026-01-03 11:00:00',1),(72,42,'2026-01-03 12:00:00',0);
		INSERT INTO daily_peer_stats(peer_id,day,checks,up_count) VALUES (42,'2026-01-03',4,3);
	`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		detail := historyDetail(t, s, 42, time.Date(2026, 1, 3, 13, 0, 0, 0, time.UTC))
		if detail.ID != 42 || detail.Status != protocol.Unknown || detail.StatusValidUntil != nil || detail.LastCheckedAt == nil || detail.LastCheckedAt.Hour() != 12 {
			t.Fatalf("legacy current state changed: %+v", detail)
		}
		if detail.Checks != 2 || detail.UpCount != 1 || detail.UnknownCount != 0 || detail.UptimePct == nil || *detail.UptimePct != 75 || detail.CoveragePct == nil || *detail.CoveragePct != 100 {
			t.Fatalf("legacy history or rollup changed: %+v", detail)
		}
		var firstID, lastID int64
		if err := s.db.QueryRow(`SELECT MIN(id), MAX(id) FROM checks`).Scan(&firstID, &lastID); err != nil {
			t.Fatal(err)
		}
		if firstID != 71 || lastID != 72 {
			t.Fatalf("legacy check identities changed: %d, %d", firstID, lastID)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEffectiveStatusValidityBoundary(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://boundary:123")
	until := time.Date(2026, 10, 8, 12, 0, 0, 123456789, time.UTC)
	if _, err := s.db.Exec(`UPDATE peers SET status = 'up', status_valid_until = ? WHERE id = ?`, sqliteTime(until), id); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at   time.Time
		want protocol.Status
	}{
		{until.Add(-time.Nanosecond), protocol.Up},
		{until, protocol.Unknown},
		{until.Add(time.Nanosecond), protocol.Unknown},
	} {
		if got := historyDetail(t, s, id, tc.at).Status; got != tc.want {
			t.Fatalf("status at %s = %s, want %s", tc.at, got, tc.want)
		}
	}
	if _, err := s.db.Exec(`UPDATE peers SET status_valid_until = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if got := historyDetail(t, s, id, until.Add(-time.Hour)).Status; got != protocol.Unknown {
		t.Fatalf("unleased state = %s, want unknown", got)
	}
}

func TestUnknownIsSeparateFromFailedChecks(t *testing.T) {
	s := newHistoryStore(t)
	now := time.Now().UTC()
	ids := make([]int64, 5)
	for i, uri := range []string{"tcp://up:1", "tcp://down:1", "tcp://expired:1", "tcp://unknown:1", "tcp://never:1"} {
		ids[i] = addHistoryPeer(t, s, uri)
	}
	for _, tc := range []struct {
		id     int64
		status protocol.Status
		until  time.Time
	}{
		{ids[0], protocol.Up, now.Add(time.Hour)},
		{ids[1], protocol.Down, now.Add(time.Hour)},
		{ids[2], protocol.Up, now.Add(-time.Hour)},
	} {
		if _, err := s.db.Exec(`UPDATE peers SET status = ?, status_valid_until = ? WHERE id = ?`, tc.status, sqliteTime(tc.until), tc.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO daily_peer_stats(peer_id,day,checks,up_count,unknown_count)
		VALUES (?,DATE('now'),2,1,2),(?,DATE('now'),0,0,3)`, ids[0], ids[3]); err != nil {
		t.Fatal(err)
	}
	for i, status := range []protocol.Status{protocol.Up, protocol.Down, protocol.Unknown, protocol.Unknown} {
		if _, err := s.db.Exec(`INSERT INTO checks(peer_id,checked_at,status) VALUES (?,?,?)`, ids[0], sqliteTime(now.Add(time.Duration(i-5)*time.Minute)), status); err != nil {
			t.Fatal(err)
		}
	}
	detail := historyDetail(t, s, ids[0], now)
	if detail.Checks != 2 || detail.UpCount != 1 || detail.UnknownCount != 2 || detail.UptimePct == nil || *detail.UptimePct != 50 || detail.CoveragePct == nil || *detail.CoveragePct != 50 {
		t.Fatalf("unknown diluted known observations: %+v", detail)
	}
	unknown := historyDetail(t, s, ids[3], now)
	if unknown.UptimePct != nil || unknown.CoveragePct == nil || *unknown.CoveragePct != 0 {
		t.Fatalf("unknown-only history = %+v", unknown)
	}
	peers, err := s.GetPeersWithStats(StatsWindowDays)
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range peers {
		if peer.ID == ids[0] && (peer.UptimePct == nil || *peer.UptimePct != 50 || peer.CoveragePct == nil || *peer.CoveragePct != 50) {
			t.Fatalf("list uptime or coverage = %+v", peer)
		}
		if peer.ID >= ids[2] && peer.Status != protocol.Unknown {
			t.Fatalf("list reported absent evidence as known: %+v", peer)
		}
	}
	all, err := s.GetAllPeers()
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range all {
		if peer.ID >= ids[2] && peer.Status != protocol.Unknown {
			t.Fatalf("catalog reported absent evidence as known: %+v", peer)
		}
	}
	stats, err := s.GetStats(StatsWindowDays)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalPeers != 5 || stats.OnlinePeers != 1 || stats.OfflinePeers != 1 || stats.UnknownPeers != 3 || stats.AvgUptimePct == nil || *stats.AvgUptimePct != 50 {
		t.Fatalf("summary counters or known uptime = %+v", stats)
	}
	if len(stats.TopCountries) != 1 || stats.TopCountries[0].OnlineCount != 1 || stats.TopCountries[0].OfflineCount != 1 || stats.TopCountries[0].UnknownCount != 3 {
		t.Fatalf("country summary = %+v", stats.TopCountries)
	}
	if len(stats.Protocols) != 1 || stats.Protocols[0].OnlineCount != 1 || stats.Protocols[0].OfflineCount != 1 || stats.Protocols[0].UnknownCount != 3 {
		t.Fatalf("protocol summary = %+v", stats.Protocols)
	}
	countries, err := s.GetCountryCounts()
	if err != nil {
		t.Fatal(err)
	}
	country := countries["Test"]
	if country.Total != 5 || country.Online != 1 || country.Offline != 1 || country.Unknown != 3 || country.AvgUptime == nil || *country.AvgUptime != 50 {
		t.Fatalf("map counters or known uptime = %+v", country)
	}
}

func TestUnknownOnlyTimelineAndCatalogCacheInvalidation(t *testing.T) {
	s := newHistoryStore(t)
	id := addHistoryPeer(t, s, "tcp://unknown:123")
	if _, err := s.db.Exec(`INSERT INTO daily_peer_stats(peer_id,day,checks,up_count,unknown_count)
		VALUES (?,DATE('now'),0,0,2)`, id); err != nil {
		t.Fatal(err)
	}
	s.RefreshCaches()
	stats, err := s.GetStats(StatsWindowDays)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.DailyTimeline) != 1 || stats.DailyTimeline[0].OnlinePeers != 0 || stats.DailyTimeline[0].OfflinePeers != 0 || stats.DailyTimeline[0].UnknownPeers != 1 {
		t.Fatalf("unknown timeline treated missing observations as failure: %+v", stats.DailyTimeline)
	}
	if err := s.BulkUpsertPeers([]PeerInput{{URI: "tcp://replacement:123", Country: "Other", Protocol: "tls", Host: "replacement", Port: "123"}}); err != nil {
		t.Fatal(err)
	}
	peers, err := s.GetPeersWithStats(StatsWindowDays)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].URI != "tcp://replacement:123" || peers[0].CoveragePct != nil {
		t.Fatalf("catalog mutation retained cached history: %+v", peers)
	}
	countries, err := s.GetCountryCounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := countries["Test"]; ok || countries["Other"].Unknown != 1 {
		t.Fatalf("catalog mutation retained cached country: %+v", countries)
	}
	stats, err = s.GetStats(StatsWindowDays)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnknownPeers != 1 || len(stats.DailyTimeline) != 0 || stats.AvgUptimePct != nil || len(stats.Protocols) != 1 || stats.Protocols[0].Name != "tls" {
		t.Fatalf("catalog mutation retained cached stats: %+v", stats)
	}
}

// state_changed_at must follow the effective status: an expired lease is
// already unknown, so neither fresh evidence nor a later unknown round may
// extend the previous known state across the measurement gap.
func TestStateBaselineFollowsEffectiveStatus(t *testing.T) {
	const interval, timeout = 10 * time.Minute, time.Minute
	t.Run("same status after expired evidence", func(t *testing.T) {
		st, probes, id := roundFixture(t, 1, false)
		work := assignmentFor(t, st, probes[0], roundTestTime)
		sendObservation(t, st, probes[0], work.RoundID, id, protocol.Up, roundTestTime.Add(time.Second))
		// No rounds run while the main instance is down.
		resumed := roundTestTime.Add(time.Hour)
		if got := historyDetail(t, st, id, resumed).Status; got != protocol.Unknown {
			t.Fatalf("status during gap = %s, want unknown", got)
		}
		if _, err := st.AdvanceProbeRounds(resumed, interval, timeout); err != nil {
			t.Fatal(err)
		}
		work = assignmentFor(t, st, probes[0], resumed)
		observed := resumed.Add(time.Second)
		sendObservation(t, st, probes[0], work.RoundID, id, protocol.Up, observed)
		detail := historyDetail(t, st, id, observed.Add(time.Second))
		if detail.Status != protocol.Up || !detail.StateChangedAt.Equal(observed) {
			t.Fatalf("%s since %s across an unknown gap, want since %s", detail.Status, detail.StateChangedAt, observed)
		}
	})
	t.Run("unknown round after expired evidence", func(t *testing.T) {
		st, probes, id := roundFixture(t, 1, false)
		work := assignmentFor(t, st, probes[0], roundTestTime)
		sendObservation(t, st, probes[0], work.RoundID, id, protocol.Up, roundTestTime.Add(time.Second))
		resumed := roundTestTime.Add(time.Hour)
		if _, err := st.AdvanceProbeRounds(resumed, interval, timeout); err != nil {
			t.Fatal(err)
		}
		// The only probe misses the resumed round.
		if _, err := st.AdvanceProbeRounds(resumed.Add(timeout), interval, timeout); err != nil {
			t.Fatal(err)
		}
		expiry := roundTestTime.Add(interval + timeout)
		detail := historyDetail(t, st, id, resumed.Add(2*timeout))
		if detail.Status != protocol.Unknown || !detail.StateChangedAt.Equal(expiry) {
			t.Fatalf("%s since %s, want unknown since lease expiry %s", detail.Status, detail.StateChangedAt, expiry)
		}
	})
	t.Run("first report after legacy status without lease", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy.db")
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`
			CREATE TABLE peers (
				id INTEGER PRIMARY KEY AUTOINCREMENT, uri TEXT UNIQUE NOT NULL, country TEXT NOT NULL,
				protocol TEXT NOT NULL, host TEXT NOT NULL, port TEXT NOT NULL,
				is_up INTEGER NOT NULL DEFAULT 0, is_network INTEGER NOT NULL DEFAULT 0,
				state_changed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, last_checked_at DATETIME);
			INSERT INTO peers(id,uri,country,protocol,host,port,is_up,state_changed_at,last_checked_at)
				VALUES (42,'tcp://legacy:1','Test','tcp','legacy','1',1,'2026-01-01 00:00:00','2026-01-03 12:00:00');
		`)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		st, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		p, _, err := st.CreateProbe("upgrade")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CheckInProbe(p.ID, false, roundTestTime); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AdvanceProbeRounds(roundTestTime, interval, timeout); err != nil {
			t.Fatal(err)
		}
		work := assignmentFor(t, st, p, roundTestTime)
		observed := roundTestTime.Add(time.Second)
		sendObservation(t, st, p, work.RoundID, 42, protocol.Up, observed)
		detail := historyDetail(t, st, 42, observed.Add(time.Second))
		if detail.Status != protocol.Up || !detail.StateChangedAt.Equal(observed) {
			t.Fatalf("%s since legacy %s, want since first distributed report %s", detail.Status, detail.StateChangedAt, observed)
		}
	})
}
