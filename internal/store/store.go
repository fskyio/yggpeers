package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// StatsWindowDays is the rolling window used for uptime calculations.
const StatsWindowDays = 7

// cacheTTL is how long query results are cached. Data only changes when the
// checker runs (default every 5 minutes), so 30 seconds is a safe window.
const cacheTTL = 30 * time.Second

// Each cache slot has its own refreshMu to dedupe concurrent refreshes: on a
// cold cache, only one goroutine runs the query while the rest block on the
// mutex and pick up the freshly-populated value.
type cachedPeers struct {
	refreshMu sync.Mutex
	value     []PeerWithStats
	expires   time.Time
}

type cachedStats struct {
	refreshMu sync.Mutex
	value     *Stats
	expires   time.Time
}

type cachedCountries struct {
	refreshMu sync.Mutex
	value     map[string]CountryCount
	expires   time.Time
}

type Store struct {
	db        *sql.DB
	cacheMu   sync.Mutex
	peers     cachedPeers
	stats     cachedStats
	countries cachedCountries
}

type Peer struct {
	ID             int64
	URI            string
	Country        string
	Protocol       string
	Host           string
	Port           string
	IsUp           bool
	IsNetwork      bool
	StateChangedAt time.Time
}

// PeerInput is the data fetcher passes to BulkUpsertPeers.
type PeerInput struct {
	URI       string
	Country   string
	Protocol  string
	Host      string
	Port      string
	IsNetwork bool
}

type PeerWithStats struct {
	Peer
	UptimePct *float64
}

// StateDuration returns how long the peer has been in its current up/down state.
func (p PeerWithStats) StateDuration() time.Duration {
	return time.Since(p.StateChangedAt)
}

type Stats struct {
	TotalPeers   int
	OnlinePeers  int
	OfflinePeers int
	OnlinePct    float64
	AvgUptimePct *float64
	TopCountries []CountryStat
	Protocols    []ProtocolStat

	// Chart data
	DailyTimeline    []DailyPoint
	CountryProtocols []CountryProtocolStat
}

type CountryStat struct {
	Name        string
	Count       int
	OnlineCount int
	OnlinePct   float64
	AvgUptime   *float64
}

type ProtocolStat struct {
	Name        string
	Count       int
	Percent     float64
	OnlineCount int
	OnlinePct   float64
}

// DailyPoint holds aggregate check data for one day.
type DailyPoint struct {
	Date        string
	TotalPeers  int
	OnlinePeers int
}

// CountryProtocolStat holds protocol breakdown for a country.
type CountryProtocolStat struct {
	Country   string
	Protocols map[string]int
}

func New(path string) (*Store, error) {
	// Per-connection pragmas go through the DSN so every pooled connection
	// gets them — Exec-ing a PRAGMA only affects the connection that ran it.
	// cache_size=-40000 asks SQLite for a ~40 MB page cache so the hot parts
	// of checks/daily_peer_stats stay resident across cold-cache refreshes.
	dsn := path + "?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=1&_busy_timeout=5000&_cache_size=-40000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// WAL mode supports concurrent readers. Allow up to 10 open connections
	// so HTTP handlers don't queue behind each other or behind writes.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	// ANALYZE lets the planner pick the best index for the new
	// (peer_id, day) primary key on daily_peer_stats and the date index
	// on checks once the tables have real cardinality.
	if _, err := db.Exec("ANALYZE"); err != nil {
		return nil, fmt.Errorf("analyze: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RefreshCaches invalidates all cached query results and repopulates them.
// The checker calls this after writing new data so the next user-facing
// request never has to wait on a cold-cache query.
func (s *Store) RefreshCaches() {
	s.cacheMu.Lock()
	s.peers.expires = time.Time{}
	s.stats.expires = time.Time{}
	s.countries.expires = time.Time{}
	s.cacheMu.Unlock()
	_, _ = s.GetPeersWithStats(StatsWindowDays)
	_, _ = s.GetStats(StatsWindowDays)
	_, _ = s.GetCountryCounts()
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS peers (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			uri              TEXT UNIQUE NOT NULL,
			country          TEXT NOT NULL,
			protocol         TEXT NOT NULL,
			host             TEXT NOT NULL,
			port             TEXT NOT NULL,
			is_up            INTEGER NOT NULL DEFAULT 0,
			is_network       INTEGER NOT NULL DEFAULT 0,
			state_changed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS checks (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			peer_id    INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
			checked_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			is_up      INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_checks_peer_checked
			ON checks(peer_id, checked_at);
		CREATE INDEX IF NOT EXISTS idx_checks_checked_at
			ON checks(checked_at);
		CREATE INDEX IF NOT EXISTS idx_peers_is_network_country
			ON peers(is_network, country);
		CREATE TABLE IF NOT EXISTS daily_peer_stats (
			peer_id  INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
			day      TEXT    NOT NULL,
			checks   INTEGER NOT NULL DEFAULT 0,
			up_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (peer_id, day)
		);
		CREATE INDEX IF NOT EXISTS idx_daily_peer_stats_day
			ON daily_peer_stats(day);
	`); err != nil {
		return err
	}
	return s.backfillDailyStats()
}

// backfillDailyStats populates daily_peer_stats from the checks table the
// first time the new schema is seen. It's a no-op on subsequent boots once
// the rollup table has any rows (the checker keeps it up-to-date from then
// on, so re-aggregating checks would double-count).
func (s *Store) backfillDailyStats() error {
	var rollupCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM daily_peer_stats`).Scan(&rollupCount); err != nil {
		return fmt.Errorf("backfill check: %w", err)
	}
	if rollupCount > 0 {
		return nil
	}
	var checkCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&checkCount); err != nil {
		return fmt.Errorf("backfill check: %w", err)
	}
	if checkCount == 0 {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO daily_peer_stats (peer_id, day, checks, up_count)
		SELECT peer_id, DATE(checked_at), COUNT(*), COALESCE(SUM(is_up), 0)
		FROM checks
		GROUP BY peer_id, DATE(checked_at)
	`)
	if err != nil {
		return fmt.Errorf("backfill: %w", err)
	}
	return nil
}

// BulkUpsertPeers inserts or updates the given peers and removes any peers
// whose URI is not in the list, all within a single transaction.
func (s *Store) BulkUpsertPeers(peers []PeerInput) error {
	if len(peers) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO peers (uri, country, protocol, host, port, is_network)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(uri) DO UPDATE SET
			country    = excluded.country,
			protocol   = excluded.protocol,
			host       = excluded.host,
			port       = excluded.port,
			is_network = excluded.is_network
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range peers {
		if _, err := stmt.Exec(p.URI, p.Country, p.Protocol, p.Host, p.Port, p.IsNetwork); err != nil {
			return err
		}
	}

	// Delete any peer not in the current list.
	uris := make([]any, len(peers))
	for i, p := range peers {
		uris[i] = p.URI
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(uris)), ",")
	if _, err := tx.Exec(
		fmt.Sprintf("DELETE FROM peers WHERE uri NOT IN (%s)", placeholders),
		uris...,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) GetAllPeers() ([]Peer, error) {
	rows, err := s.db.Query(
		`SELECT id, uri, country, protocol, host, port, is_up, is_network, state_changed_at FROM peers`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []Peer
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.ID, &p.URI, &p.Country, &p.Protocol,
			&p.Host, &p.Port, &p.IsUp, &p.IsNetwork, &p.StateChangedAt); err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

type CheckResult struct {
	PeerID int64
	IsUp   bool
}

// RecordChecks inserts a row in `checks` for each result and updates the
// peer's current state, bumping `state_changed_at` only when the up/down
// state actually flips.
func (s *Store) RecordChecks(results []CheckResult) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insertCheck, err := tx.Prepare(`INSERT INTO checks (peer_id, is_up) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer insertCheck.Close()

	updatePeer, err := tx.Prepare(`
		UPDATE peers
		SET is_up = ?,
		    state_changed_at = CASE WHEN is_up != ? THEN datetime('now') ELSE state_changed_at END
		WHERE id = ?
	`)
	if err != nil {
		return err
	}
	defer updatePeer.Close()

	// Roll up into daily_peer_stats as we go so /stats and / don't have to
	// aggregate the raw checks table at request time.
	upsertDaily, err := tx.Prepare(`
		INSERT INTO daily_peer_stats (peer_id, day, checks, up_count)
		VALUES (?, DATE('now'), 1, ?)
		ON CONFLICT(peer_id, day) DO UPDATE SET
			checks   = checks   + 1,
			up_count = up_count + excluded.up_count
	`)
	if err != nil {
		return err
	}
	defer upsertDaily.Close()

	for _, r := range results {
		if _, err := insertCheck.Exec(r.PeerID, r.IsUp); err != nil {
			return err
		}
		if _, err := updatePeer.Exec(r.IsUp, r.IsUp, r.PeerID); err != nil {
			return err
		}
		up := 0
		if r.IsUp {
			up = 1
		}
		if _, err := upsertDaily.Exec(r.PeerID, up); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) PruneOldChecks(days int) error {
	if _, err := s.db.Exec(
		`DELETE FROM checks WHERE checked_at < datetime('now', '-' || ? || ' days')`, days,
	); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`DELETE FROM daily_peer_stats WHERE day < DATE('now', '-' || ? || ' days')`, days,
	)
	return err
}

func (s *Store) GetPeersWithStats(windowDays int) ([]PeerWithStats, error) {
	s.cacheMu.Lock()
	if time.Now().Before(s.peers.expires) {
		cached := s.peers.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	s.cacheMu.Unlock()

	s.peers.refreshMu.Lock()
	defer s.peers.refreshMu.Unlock()
	// Another caller may have refreshed while we waited.
	s.cacheMu.Lock()
	if time.Now().Before(s.peers.expires) {
		cached := s.peers.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	s.cacheMu.Unlock()

	rows, err := s.db.Query(`
		SELECT
			p.id, p.uri, p.country, p.protocol, p.host, p.port,
			p.is_up, p.is_network, p.state_changed_at,
			CASE WHEN COALESCE(SUM(d.checks), 0) = 0 THEN NULL
			     ELSE CAST(SUM(d.up_count) AS REAL) / SUM(d.checks) * 100
			END
		FROM peers p
		LEFT JOIN daily_peer_stats d
			ON d.peer_id = p.id
			AND d.day >= DATE('now', '-' || ? || ' days')
		GROUP BY p.id
		ORDER BY p.country, p.is_up DESC, p.uri
	`, windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PeerWithStats
	for rows.Next() {
		var p PeerWithStats
		if err := rows.Scan(
			&p.ID, &p.URI, &p.Country, &p.Protocol, &p.Host, &p.Port,
			&p.IsUp, &p.IsNetwork, &p.StateChangedAt, &p.UptimePct,
		); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	s.peers.value = result
	s.peers.expires = time.Now().Add(cacheTTL)
	s.cacheMu.Unlock()
	return result, nil
}

// CountryCount holds total, online, and average uptime stats for a country.
type CountryCount struct {
	Total     int      `json:"total"`
	Online    int      `json:"online"`
	AvgUptime *float64 `json:"avgUptime"`
}

// GetCountryCounts returns peer counts grouped by country, excluding overlay
// network peers (which don't have a real geographic location).
func (s *Store) GetCountryCounts() (map[string]CountryCount, error) {
	s.cacheMu.Lock()
	if time.Now().Before(s.countries.expires) {
		cached := s.countries.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	s.cacheMu.Unlock()

	s.countries.refreshMu.Lock()
	defer s.countries.refreshMu.Unlock()
	s.cacheMu.Lock()
	if time.Now().Before(s.countries.expires) {
		cached := s.countries.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	s.cacheMu.Unlock()

	rows, err := s.db.Query(`
		SELECT
			p.country,
			COUNT(DISTINCT p.id),
			SUM(CASE WHEN p.is_up = 1 THEN 1 ELSE 0 END),
			AVG(u.uptime)
		FROM peers p
		LEFT JOIN (
			SELECT peer_id,
				CAST(SUM(up_count) AS REAL) / SUM(checks) * 100 AS uptime
			FROM daily_peer_stats
			WHERE day >= DATE('now', '-' || ? || ' days')
			GROUP BY peer_id
		) u ON u.peer_id = p.id
		WHERE p.is_network = 0
		GROUP BY p.country
	`, StatsWindowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]CountryCount)
	for rows.Next() {
		var country string
		var total, online int
		var avgUptime *float64
		if err := rows.Scan(&country, &total, &online, &avgUptime); err != nil {
			return nil, err
		}
		counts[country] = CountryCount{Total: total, Online: online, AvgUptime: avgUptime}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	s.countries.value = counts
	s.countries.expires = time.Now().Add(cacheTTL)
	s.cacheMu.Unlock()
	return counts, nil
}

func (s *Store) GetStats(windowDays int) (*Stats, error) {
	s.cacheMu.Lock()
	if time.Now().Before(s.stats.expires) {
		cached := s.stats.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	s.cacheMu.Unlock()

	s.stats.refreshMu.Lock()
	defer s.stats.refreshMu.Unlock()
	s.cacheMu.Lock()
	if time.Now().Before(s.stats.expires) {
		cached := s.stats.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	s.cacheMu.Unlock()

	st := &Stats{}

	err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(is_up), 0) FROM peers WHERE is_network = 0
	`).Scan(&st.TotalPeers, &st.OnlinePeers)
	if err != nil {
		return nil, err
	}
	st.OfflinePeers = st.TotalPeers - st.OnlinePeers
	if st.TotalPeers > 0 {
		st.OnlinePct = float64(st.OnlinePeers) / float64(st.TotalPeers) * 100
	}

	err = s.db.QueryRow(`
		SELECT AVG(uptime) FROM (
			SELECT CAST(SUM(d.up_count) AS REAL) / SUM(d.checks) * 100 AS uptime
			FROM peers p
			JOIN daily_peer_stats d ON d.peer_id = p.id
				AND d.day >= DATE('now', '-' || ? || ' days')
			WHERE p.is_network = 0
			GROUP BY p.id
		)
	`, windowDays).Scan(&st.AvgUptimePct)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT
			p.country,
			COUNT(*) as cnt,
			COALESCE(SUM(p.is_up), 0) as online,
			AVG(u.uptime)
		FROM peers p
		LEFT JOIN (
			SELECT peer_id,
				CAST(SUM(up_count) AS REAL) / SUM(checks) * 100 AS uptime
			FROM daily_peer_stats
			WHERE day >= DATE('now', '-' || ? || ' days')
			GROUP BY peer_id
		) u ON u.peer_id = p.id
		WHERE p.is_network = 0
		GROUP BY p.country
		ORDER BY cnt DESC
		LIMIT 10
	`, windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cs CountryStat
		if err := rows.Scan(&cs.Name, &cs.Count, &cs.OnlineCount, &cs.AvgUptime); err != nil {
			return nil, err
		}
		if cs.Count > 0 {
			cs.OnlinePct = float64(cs.OnlineCount) / float64(cs.Count) * 100
		}
		st.TopCountries = append(st.TopCountries, cs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows2, err := s.db.Query(`
		SELECT protocol, COUNT(*), COALESCE(SUM(is_up), 0)
		FROM peers GROUP BY protocol ORDER BY COUNT(*) DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	var totalProto int
	var protos []ProtocolStat
	for rows2.Next() {
		var ps ProtocolStat
		if err := rows2.Scan(&ps.Name, &ps.Count, &ps.OnlineCount); err != nil {
			return nil, err
		}
		totalProto += ps.Count
		protos = append(protos, ps)
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}
	for i := range protos {
		if totalProto > 0 {
			protos[i].Percent = float64(protos[i].Count) / float64(totalProto) * 100
		}
		if protos[i].Count > 0 {
			protos[i].OnlinePct = float64(protos[i].OnlineCount) / float64(protos[i].Count) * 100
		}
	}
	st.Protocols = protos

	// Daily timeline (peers checked per day and how many were online).
	// daily_peer_stats has one row per (peer, day), so COUNT(*) gives the
	// distinct peer count and SUM(up_count)/SUM(checks)*COUNT(*) reproduces
	// the old "fraction up × peers seen" estimate over far fewer rows.
	timelineRows, err := s.db.Query(`
		SELECT
			day,
			COUNT(*) AS total,
			SUM(up_count) * 1.0 / SUM(checks) * COUNT(*) AS online_approx
		FROM daily_peer_stats
		WHERE day >= DATE('now', '-' || ? || ' days')
		GROUP BY day
		ORDER BY day
	`, windowDays)
	if err != nil {
		return nil, err
	}
	defer timelineRows.Close()
	for timelineRows.Next() {
		var dp DailyPoint
		var onlineF float64
		if err := timelineRows.Scan(&dp.Date, &dp.TotalPeers, &onlineF); err != nil {
			return nil, err
		}
		dp.OnlinePeers = int(onlineF + 0.5)
		st.DailyTimeline = append(st.DailyTimeline, dp)
	}
	if err := timelineRows.Err(); err != nil {
		return nil, err
	}

	// Protocol breakdown per country (top 10 countries).
	cpRows, err := s.db.Query(`
		SELECT p.country, p.protocol, COUNT(*) as cnt
		FROM peers p
		JOIN (
			SELECT country, COUNT(*) as total
			FROM peers WHERE is_network = 0
			GROUP BY country ORDER BY total DESC LIMIT 10
		) top ON top.country = p.country
		WHERE p.is_network = 0
		GROUP BY p.country, p.protocol
		ORDER BY top.total DESC, p.country, cnt DESC
	`)
	if err != nil {
		return nil, err
	}
	defer cpRows.Close()
	cpMap := make(map[string]map[string]int)
	var cpOrder []string
	for cpRows.Next() {
		var country, protocol string
		var count int
		if err := cpRows.Scan(&country, &protocol, &count); err != nil {
			return nil, err
		}
		if cpMap[country] == nil {
			cpMap[country] = make(map[string]int)
			cpOrder = append(cpOrder, country)
		}
		cpMap[country][protocol] = count
	}
	if err := cpRows.Err(); err != nil {
		return nil, err
	}
	for _, c := range cpOrder {
		st.CountryProtocols = append(st.CountryProtocols, CountryProtocolStat{
			Country:   c,
			Protocols: cpMap[c],
		})
	}

	s.cacheMu.Lock()
	s.stats.value = st
	s.stats.expires = time.Now().Add(cacheTTL)
	s.cacheMu.Unlock()
	return st, nil
}
