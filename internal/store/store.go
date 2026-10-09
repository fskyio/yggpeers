package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"yggpeers/internal/protocol"

	_ "github.com/mattn/go-sqlite3"
)

// StatsWindowDays is the rolling window used for uptime calculations.
const StatsWindowDays = 7

// cacheTTL bounds cached reads; fresh evidence may expire sooner.
const cacheTTL = 30 * time.Second

// Each cache slot has its own refreshMu to dedupe concurrent refreshes: on a
// cold cache, only one goroutine runs the query while the rest block on the
// mutex and pick up the freshly-populated value.
type cachedPeers struct {
	refreshMu  sync.Mutex
	value      []PeerWithStats
	expires    time.Time
	windowDays int
}

type cachedStats struct {
	refreshMu  sync.Mutex
	value      *Stats
	expires    time.Time
	windowDays int
}

type cachedCountries struct {
	refreshMu sync.Mutex
	value     map[string]CountryCount
	expires   time.Time
}

type Store struct {
	db              *sql.DB
	cacheMu         sync.Mutex
	peers           cachedPeers
	stats           cachedStats
	countries       cachedCountries
	cacheGeneration uint64
}

type Peer struct {
	ID               int64
	URI              string
	Country          string
	Protocol         string
	Host             string
	Port             string
	Status           protocol.Status
	IsNetwork        bool
	StateChangedAt   time.Time
	LastCheckedAt    *time.Time
	StatusValidUntil *time.Time
	LastRoundID      *int64
	SuccessfulProbes int
	ReportingProbes  int
	ExpectedProbes   int
	RegisteredProbes int
	// Identity from the latest successful handshake; empty until one arrives.
	NodeKey         string
	ProtocolVersion string
	NodeSeenAt      *time.Time
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
	UptimePct   *float64
	CoveragePct *float64
}

// StateDuration returns elapsed time since the last published status transition.
func (p PeerWithStats) StateDuration() time.Duration {
	return time.Since(p.StateChangedAt)
}

type Stats struct {
	TotalPeers   int
	OnlinePeers  int
	OfflinePeers int
	UnknownPeers int
	OnlinePct    float64
	AvgUptimePct *float64
	TopCountries []CountryStat
	Protocols    []ProtocolStat

	// Chart data
	DailyTimeline    []DailyPoint
	CountryProtocols []CountryProtocolStat
}

type CountryStat struct {
	Name         string
	Count        int
	OnlineCount  int
	OfflineCount int
	UnknownCount int
	OnlinePct    float64
	AvgUptime    *float64
}

type ProtocolStat struct {
	Name         string
	Count        int
	Percent      float64
	OnlineCount  int
	OfflineCount int
	UnknownCount int
	OnlinePct    float64
}

// DailyPoint holds aggregate check data for one day.
type DailyPoint struct {
	Date         string
	TotalPeers   int
	OnlinePeers  int
	OfflinePeers int
	UnknownPeers int
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
	dsn := path + "?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=1&_busy_timeout=5000&_cache_size=-40000&_txlock=immediate"
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
		db.Close()
		return nil, err
	}
	if err := s.migrateProbes(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateNodes(); err != nil {
		db.Close()
		return nil, err
	}
	// ANALYZE lets the planner pick the best index for the new
	// (peer_id, day) primary key on daily_peer_stats and the date index
	// on checks once the tables have real cardinality.
	if _, err := db.Exec("ANALYZE"); err != nil {
		db.Close()
		return nil, fmt.Errorf("analyze: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RefreshCaches invalidates all cached query results and repopulates them.
func (s *Store) RefreshCaches() {
	s.invalidateCaches()
	_, _ = s.GetPeersWithStats(StatsWindowDays)
	_, _ = s.GetStats(StatsWindowDays)
	_, _ = s.GetCountryCounts()
}

func (s *Store) invalidateCaches() {
	s.cacheMu.Lock()
	s.cacheGeneration++
	s.peers.expires = time.Time{}
	s.stats.expires = time.Time{}
	s.countries.expires = time.Time{}
	s.cacheMu.Unlock()
}

// cacheDeadline prevents cached current status from outliving its evidence.
func (s *Store) cacheDeadline(now time.Time) (time.Time, error) {
	expires := now.Add(cacheTTL)
	var validUntil time.Time
	err := s.db.QueryRow(`SELECT status_valid_until FROM peers
		WHERE status IN ('up', 'down') AND status_valid_until > ?
		ORDER BY status_valid_until LIMIT 1`, sqliteTime(now)).Scan(&validUntil)
	if err != nil && err != sql.ErrNoRows {
		return time.Time{}, err
	}
	if err == nil && validUntil.Before(expires) {
		expires = validUntil
	}
	return expires, nil
}

func sqliteTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.999999999")
}

const effectiveStatusSQL = `CASE WHEN p.status_valid_until > ? THEN p.status ELSE 'unknown' END`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS peers (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			uri              TEXT UNIQUE NOT NULL,
			country          TEXT NOT NULL,
			protocol         TEXT NOT NULL,
			host             TEXT NOT NULL,
			port             TEXT NOT NULL,
			status           TEXT NOT NULL DEFAULT 'unknown' CHECK(status IN ('up', 'down', 'unknown')),
			is_network       INTEGER NOT NULL DEFAULT 0,
			state_changed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_checked_at DATETIME,
			status_valid_until DATETIME,
			last_round_id INTEGER,
			successful_probes INTEGER NOT NULL DEFAULT 0,
			reporting_probes INTEGER NOT NULL DEFAULT 0,
			expected_probes INTEGER NOT NULL DEFAULT 0,
			registered_probes INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS checks (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			peer_id    INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
			checked_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			status     TEXT NOT NULL CHECK(status IN ('up', 'down', 'unknown')),
			round_id   INTEGER
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
			unknown_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (peer_id, day)
		);
		CREATE INDEX IF NOT EXISTS idx_daily_peer_stats_day
			ON daily_peer_stats(day);
	`); err != nil {
		return err
	}
	if err := s.migrateLastCheckedAt(); err != nil {
		return err
	}
	if err := s.migrateStatus(); err != nil {
		return err
	}
	if err := s.backfillDailyStats(); err != nil {
		return err
	}
	return s.migrateMetadata()
}

// migrateStatus preserves legacy samples and peer identity while removing the
// old boolean state. Pre-cutover state deliberately has no validity lease.
func (s *Store) migrateStatus() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	peerColumns, err := tableColumns(tx, "peers")
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"status", "TEXT NOT NULL DEFAULT 'unknown' CHECK(status IN ('up', 'down', 'unknown'))"},
		{"status_valid_until", "DATETIME"},
		{"last_round_id", "INTEGER"},
		{"successful_probes", "INTEGER NOT NULL DEFAULT 0"},
		{"reporting_probes", "INTEGER NOT NULL DEFAULT 0"},
		{"expected_probes", "INTEGER NOT NULL DEFAULT 0"},
		{"registered_probes", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if !peerColumns[column.name] {
			if _, err := tx.Exec("ALTER TABLE peers ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return fmt.Errorf("migrate peer %s: %w", column.name, err)
			}
		}
	}
	if peerColumns["is_up"] {
		if _, err := tx.Exec(`UPDATE peers SET status = CASE
			WHEN last_checked_at IS NULL THEN 'unknown'
			WHEN is_up = 1 THEN 'up' ELSE 'down' END;
			ALTER TABLE peers DROP COLUMN is_up`); err != nil {
			return fmt.Errorf("migrate peer status: %w", err)
		}
	}
	checkColumns, err := tableColumns(tx, "checks")
	if err != nil {
		return err
	}
	if !checkColumns["status"] {
		if _, err := tx.Exec(`ALTER TABLE checks ADD COLUMN status TEXT NOT NULL DEFAULT 'unknown'
			CHECK(status IN ('up', 'down', 'unknown'))`); err != nil {
			return err
		}
	}
	if checkColumns["is_up"] {
		if _, err := tx.Exec(`UPDATE checks SET status = CASE WHEN is_up = 1 THEN 'up' ELSE 'down' END;
			ALTER TABLE checks DROP COLUMN is_up`); err != nil {
			return fmt.Errorf("migrate check status: %w", err)
		}
	}
	if !checkColumns["round_id"] {
		if _, err := tx.Exec(`ALTER TABLE checks ADD COLUMN round_id INTEGER`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_checks_peer_round ON checks(peer_id, round_id)`); err != nil {
		return err
	}
	dailyColumns, err := tableColumns(tx, "daily_peer_stats")
	if err != nil {
		return err
	}
	if !dailyColumns["unknown_count"] {
		if _, err := tx.Exec(`ALTER TABLE daily_peer_stats ADD COLUMN unknown_count INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func tableColumns(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

// backfillDailyStats populates daily_peer_stats from the checks table the
// first time the new schema is seen. Existing rollups are not re-aggregated.
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
		INSERT INTO daily_peer_stats (peer_id, day, checks, up_count, unknown_count)
		SELECT peer_id, DATE(checked_at), SUM(status != 'unknown'),
			SUM(status = 'up'), SUM(status = 'unknown')
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

	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateCaches()
	return nil
}

func (s *Store) GetAllPeers() ([]Peer, error) {
	now := time.Now()
	rows, err := s.db.Query(`
		SELECT p.id, p.uri, p.country, p.protocol, p.host, p.port,
			`+effectiveStatusSQL+`, p.is_network, p.state_changed_at, p.last_checked_at,
			p.status_valid_until, p.last_round_id, p.successful_probes,
			p.reporting_probes, p.expected_probes, p.registered_probes, `+peerNodeColumns+`
		FROM peers p`, sqliteTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []Peer
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.ID, &p.URI, &p.Country, &p.Protocol,
			&p.Host, &p.Port, &p.Status, &p.IsNetwork, &p.StateChangedAt, &p.LastCheckedAt,
			&p.StatusValidUntil, &p.LastRoundID, &p.SuccessfulProbes,
			&p.ReportingProbes, &p.ExpectedProbes, &p.RegisteredProbes,
			&p.NodeKey, &p.ProtocolVersion, &p.NodeSeenAt); err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

func (s *Store) PruneOldChecks(days int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{
		`DELETE FROM checks WHERE checked_at < datetime('now', '-' || ? || ' days')`,
		`DELETE FROM daily_peer_stats WHERE day < DATE('now', '-' || ? || ' days')`,
		`DELETE FROM probe_rounds WHERE finalized = 1 AND started_at < datetime('now', '-' || ? || ' days')`,
	} {
		if _, err := tx.Exec(query, days); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateCaches()
	return nil
}

func (s *Store) GetPeersWithStats(windowDays int) ([]PeerWithStats, error) {
	s.cacheMu.Lock()
	if s.peers.windowDays == windowDays && time.Now().Before(s.peers.expires) {
		cached := s.peers.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	s.cacheMu.Unlock()

	s.peers.refreshMu.Lock()
	defer s.peers.refreshMu.Unlock()
	// Another caller may have refreshed while we waited.
	s.cacheMu.Lock()
	if s.peers.windowDays == windowDays && time.Now().Before(s.peers.expires) {
		cached := s.peers.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	generation := s.cacheGeneration
	s.cacheMu.Unlock()
	now := time.Now()
	expires, err := s.cacheDeadline(now)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT
			p.id, p.uri, p.country, p.protocol, p.host, p.port,
			`+effectiveStatusSQL+` AS effective_status, p.is_network, p.state_changed_at, p.last_checked_at,
			p.status_valid_until, p.last_round_id, p.successful_probes,
			p.reporting_probes, p.expected_probes, p.registered_probes, `+peerNodeColumns+`,
			CAST(SUM(d.up_count) AS REAL) / NULLIF(SUM(d.checks), 0) * 100,
			CAST(SUM(d.checks) AS REAL) / NULLIF(SUM(d.checks + d.unknown_count), 0) * 100
		FROM peers p
		LEFT JOIN daily_peer_stats d
			ON d.peer_id = p.id
			AND d.day >= DATE('now', '-' || ? || ' days')
		GROUP BY p.id
		ORDER BY p.country, CASE effective_status WHEN 'up' THEN 0 WHEN 'down' THEN 1 ELSE 2 END, p.uri
	`, sqliteTime(now), windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PeerWithStats
	for rows.Next() {
		var p PeerWithStats
		if err := rows.Scan(
			&p.ID, &p.URI, &p.Country, &p.Protocol, &p.Host, &p.Port,
			&p.Status, &p.IsNetwork, &p.StateChangedAt, &p.LastCheckedAt,
			&p.StatusValidUntil, &p.LastRoundID, &p.SuccessfulProbes,
			&p.ReportingProbes, &p.ExpectedProbes, &p.RegisteredProbes,
			&p.NodeKey, &p.ProtocolVersion, &p.NodeSeenAt,
			&p.UptimePct, &p.CoveragePct,
		); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	if generation == s.cacheGeneration {
		s.peers.value = result
		s.peers.expires = expires
		s.peers.windowDays = windowDays
	}
	s.cacheMu.Unlock()
	return result, nil
}

// CountryCount holds total, online, and average uptime stats for a country.
type CountryCount struct {
	Total     int      `json:"total"`
	Online    int      `json:"online"`
	Offline   int      `json:"offline"`
	Unknown   int      `json:"unknown"`
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
	generation := s.cacheGeneration
	s.cacheMu.Unlock()
	now := time.Now()
	expires, err := s.cacheDeadline(now)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		WITH current_peers AS (SELECT p.*, `+effectiveStatusSQL+` AS effective_status FROM peers p)
		SELECT
			p.country,
			COUNT(DISTINCT p.id),
			SUM(p.effective_status = 'up'),
			SUM(p.effective_status = 'down'),
			SUM(p.effective_status = 'unknown'),
			AVG(u.uptime)
		FROM current_peers p
		LEFT JOIN (
			SELECT peer_id,
				CAST(SUM(up_count) AS REAL) / NULLIF(SUM(checks), 0) * 100 AS uptime
			FROM daily_peer_stats
			WHERE day >= DATE('now', '-' || ? || ' days')
			GROUP BY peer_id
		) u ON u.peer_id = p.id
		WHERE p.is_network = 0
		GROUP BY p.country
	`, sqliteTime(now), StatsWindowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]CountryCount)
	for rows.Next() {
		var country string
		var total, online, offline, unknown int
		var avgUptime *float64
		if err := rows.Scan(&country, &total, &online, &offline, &unknown, &avgUptime); err != nil {
			return nil, err
		}
		counts[country] = CountryCount{Total: total, Online: online, Offline: offline, Unknown: unknown, AvgUptime: avgUptime}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	if generation == s.cacheGeneration {
		s.countries.value = counts
		s.countries.expires = expires
	}
	s.cacheMu.Unlock()
	return counts, nil
}

func (s *Store) GetStats(windowDays int) (*Stats, error) {
	s.cacheMu.Lock()
	if s.stats.windowDays == windowDays && time.Now().Before(s.stats.expires) {
		cached := s.stats.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	s.cacheMu.Unlock()

	s.stats.refreshMu.Lock()
	defer s.stats.refreshMu.Unlock()
	s.cacheMu.Lock()
	if s.stats.windowDays == windowDays && time.Now().Before(s.stats.expires) {
		cached := s.stats.value
		s.cacheMu.Unlock()
		return cached, nil
	}
	generation := s.cacheGeneration
	s.cacheMu.Unlock()

	st := &Stats{}
	now := time.Now()
	expires, err := s.cacheDeadline(now)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRow(`
		WITH current_peers AS (SELECT p.*, `+effectiveStatusSQL+` AS effective_status FROM peers p)
		SELECT COUNT(*), COALESCE(SUM(effective_status = 'up'), 0),
			COALESCE(SUM(effective_status = 'down'), 0),
			COALESCE(SUM(effective_status = 'unknown'), 0)
		FROM current_peers WHERE is_network = 0
	`, sqliteTime(now)).Scan(&st.TotalPeers, &st.OnlinePeers, &st.OfflinePeers, &st.UnknownPeers)
	if err != nil {
		return nil, err
	}
	if st.TotalPeers > 0 {
		st.OnlinePct = float64(st.OnlinePeers) / float64(st.TotalPeers) * 100
	}

	err = s.db.QueryRow(`
		SELECT AVG(uptime) FROM (
			SELECT CAST(SUM(d.up_count) AS REAL) / NULLIF(SUM(d.checks), 0) * 100 AS uptime
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
		WITH current_peers AS (SELECT p.*, `+effectiveStatusSQL+` AS effective_status FROM peers p)
		SELECT
			p.country,
			COUNT(*) as cnt,
			SUM(p.effective_status = 'up'),
			SUM(p.effective_status = 'down'),
			SUM(p.effective_status = 'unknown'),
			AVG(u.uptime)
		FROM current_peers p
		LEFT JOIN (
			SELECT peer_id,
				CAST(SUM(up_count) AS REAL) / NULLIF(SUM(checks), 0) * 100 AS uptime
			FROM daily_peer_stats
			WHERE day >= DATE('now', '-' || ? || ' days')
			GROUP BY peer_id
		) u ON u.peer_id = p.id
		WHERE p.is_network = 0
		GROUP BY p.country
		ORDER BY cnt DESC
		LIMIT 10
	`, sqliteTime(now), windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cs CountryStat
		if err := rows.Scan(&cs.Name, &cs.Count, &cs.OnlineCount, &cs.OfflineCount, &cs.UnknownCount, &cs.AvgUptime); err != nil {
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
		WITH current_peers AS (SELECT p.*, `+effectiveStatusSQL+` AS effective_status FROM peers p)
		SELECT protocol, COUNT(*), SUM(effective_status = 'up'),
			SUM(effective_status = 'down'), SUM(effective_status = 'unknown')
		FROM current_peers GROUP BY protocol ORDER BY COUNT(*) DESC
	`, sqliteTime(now))
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	var totalProto int
	var protos []ProtocolStat
	for rows2.Next() {
		var ps ProtocolStat
		if err := rows2.Scan(&ps.Name, &ps.Count, &ps.OnlineCount, &ps.OfflineCount, &ps.UnknownCount); err != nil {
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

	// Approximate each day's peer status shares from all aggregate observations,
	// keeping absent evidence separate from observed failures.
	timelineRows, err := s.db.Query(`
		SELECT
			day,
			COUNT(*) AS total,
			COALESCE(SUM(up_count) * 1.0 / NULLIF(SUM(checks + unknown_count), 0), 0) * COUNT(*) AS online_approx,
			COALESCE(SUM(unknown_count) * 1.0 / NULLIF(SUM(checks + unknown_count), 0), 0) * COUNT(*) AS unknown_approx
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
		var onlineF, unknownF float64
		if err := timelineRows.Scan(&dp.Date, &dp.TotalPeers, &onlineF, &unknownF); err != nil {
			return nil, err
		}
		dp.OnlinePeers = int(onlineF + 0.5)
		dp.UnknownPeers = min(int(unknownF+0.5), dp.TotalPeers-dp.OnlinePeers)
		dp.OfflinePeers = dp.TotalPeers - dp.OnlinePeers - dp.UnknownPeers
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
	if generation == s.cacheGeneration {
		s.stats.value = st
		s.stats.expires = expires
		s.stats.windowDays = windowDays
	}
	s.cacheMu.Unlock()
	return st, nil
}
