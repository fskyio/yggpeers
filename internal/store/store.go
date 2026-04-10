package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// StatsWindowDays is the rolling window used for uptime calculations.
const StatsWindowDays = 7

type Store struct {
	db *sql.DB
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	s := &Store{db: db}
	return s, s.migrate()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
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
	`)
	return err
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

	for _, r := range results {
		if _, err := insertCheck.Exec(r.PeerID, r.IsUp); err != nil {
			return err
		}
		if _, err := updatePeer.Exec(r.IsUp, r.IsUp, r.PeerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) PruneOldChecks(days int) error {
	_, err := s.db.Exec(
		`DELETE FROM checks WHERE checked_at < datetime('now', '-' || ? || ' days')`, days,
	)
	return err
}

func (s *Store) GetPeersWithStats(windowDays int) ([]PeerWithStats, error) {
	rows, err := s.db.Query(`
		SELECT
			p.id, p.uri, p.country, p.protocol, p.host, p.port,
			p.is_up, p.is_network, p.state_changed_at,
			CASE WHEN COUNT(c.id) = 0 THEN NULL
			     ELSE CAST(SUM(c.is_up) AS REAL) / COUNT(c.id) * 100
			END
		FROM peers p
		LEFT JOIN checks c
			ON c.peer_id = p.id
			AND c.checked_at >= datetime('now', '-' || ? || ' days')
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
	return result, rows.Err()
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
	rows, err := s.db.Query(`
		SELECT
			p.country,
			COUNT(DISTINCT p.id),
			SUM(CASE WHEN p.is_up = 1 THEN 1 ELSE 0 END),
			AVG(u.uptime)
		FROM peers p
		LEFT JOIN (
			SELECT c.peer_id,
				CAST(SUM(c.is_up) AS REAL) / COUNT(c.id) * 100 AS uptime
			FROM checks c
			WHERE c.checked_at >= datetime('now', '-' || ? || ' days')
			GROUP BY c.peer_id
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
	return counts, rows.Err()
}

func (s *Store) GetStats(windowDays int) (*Stats, error) {
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
			SELECT CAST(SUM(c.is_up) AS REAL) / COUNT(c.id) * 100 as uptime
			FROM peers p
			JOIN checks c ON c.peer_id = p.id
				AND c.checked_at >= datetime('now', '-' || ? || ' days')
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
			SELECT c.peer_id,
				CAST(SUM(c.is_up) AS REAL) / COUNT(c.id) * 100 AS uptime
			FROM checks c
			WHERE c.checked_at >= datetime('now', '-' || ? || ' days')
			GROUP BY c.peer_id
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
	timelineRows, err := s.db.Query(`
		SELECT
			DATE(checked_at) AS day,
			COUNT(DISTINCT peer_id) AS total,
			SUM(is_up) * 1.0 / COUNT(*) * COUNT(DISTINCT peer_id) AS online_approx
		FROM checks
		WHERE checked_at >= datetime('now', '-' || ? || ' days')
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

	return st, nil
}
