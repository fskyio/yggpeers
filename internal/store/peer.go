package store

import (
	"fmt"
	"time"

	"yggpeers/internal/protocol"
)

// HistoryHour contains observations in the half-open interval [Start, End).
// No observations is distinct from an observed failure.
type HistoryHour struct {
	Start        time.Time
	End          time.Time
	Checks       int
	UpCount      int
	UnknownCount int
}

// PeerDetail combines current state, calendar-day rollup uptime, and an exact
// rolling seven-day observation history. Checks and UpCount cover Hours only.
type PeerDetail struct {
	PeerWithStats
	Hours        []HistoryHour
	WindowStart  time.Time
	WindowEnd    time.Time
	Checks       int
	UpCount      int
	UnknownCount int
}

func (s *Store) migrateLastCheckedAt() error {
	rows, err := s.db.Query(`PRAGMA table_info(peers)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "last_checked_at" {
			found = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE peers ADD COLUMN last_checked_at DATETIME`); err != nil {
		return fmt.Errorf("add last check timestamp: %w", err)
	}
	if _, err := tx.Exec(`UPDATE peers SET last_checked_at = (
		SELECT MAX(checked_at) FROM checks WHERE peer_id = peers.id
	)`); err != nil {
		return fmt.Errorf("backfill last check timestamp: %w", err)
	}
	return tx.Commit()
}

// GetPeerDetail returns sql.ErrNoRows when the peer no longer exists. It does
// not use list caches: current state and both histories share a read snapshot.
func (s *Store) GetPeerDetail(id int64, now time.Time) (PeerDetail, error) {
	var detail PeerDetail
	now = now.UTC()
	detail.WindowEnd = now
	detail.WindowStart = now.Add(-168 * time.Hour)
	tx, err := s.db.Begin()
	if err != nil {
		return detail, err
	}
	defer tx.Rollback()
	err = tx.QueryRow(`
		SELECT p.id, p.uri, p.country, p.protocol, p.host, p.port,
			p.status, p.is_network, p.state_changed_at, p.last_checked_at,
			p.status_valid_until, p.last_round_id, p.successful_probes,
			p.reporting_probes, p.expected_probes, p.registered_probes, `+peerNodeColumns+`,
			CAST(SUM(d.up_count) AS REAL) / NULLIF(SUM(d.checks), 0) * 100,
			CAST(SUM(d.checks) AS REAL) / NULLIF(SUM(d.checks + d.unknown_count), 0) * 100
		FROM peers p
		LEFT JOIN daily_peer_stats d ON d.peer_id = p.id
			AND d.day >= DATE(?, '-' || ? || ' days')
		WHERE p.id = ?
		GROUP BY p.id
	`, now.Format("2006-01-02"), StatsWindowDays, id).Scan(
		&detail.ID, &detail.URI, &detail.Country, &detail.Protocol,
		&detail.Host, &detail.Port, &detail.Status, &detail.IsNetwork,
		&detail.StateChangedAt, &detail.LastCheckedAt,
		&detail.StatusValidUntil, &detail.LastRoundID, &detail.SuccessfulProbes,
		&detail.ReportingProbes, &detail.ExpectedProbes, &detail.RegisteredProbes,
		&detail.NodeKey, &detail.ProtocolVersion, &detail.NodeSeenAt,
		&detail.UptimePct, &detail.CoveragePct,
	)
	if err != nil {
		return detail, err
	}
	if detail.StatusValidUntil == nil || !now.Before(*detail.StatusValidUntil) {
		detail.Status = protocol.Unknown
	}
	detail.Hours = make([]HistoryHour, 168)
	for i := range detail.Hours {
		start := detail.WindowStart.Add(time.Duration(i) * time.Hour)
		detail.Hours[i] = HistoryHour{Start: start, End: start.Add(time.Hour)}
	}
	// SQLite timestamps may have fractional seconds. Indexed second-rounded
	// bounds fetch a small superset, then time comparisons enforce exact bounds
	// without losing timestamp precision to SQLite date functions.
	const sqliteSecond = "2006-01-02 15:04:05"
	rows, err := tx.Query(`SELECT checked_at, status FROM checks
		WHERE peer_id = ? AND checked_at >= ? AND checked_at < ?`,
		id, detail.WindowStart.Truncate(time.Second).Format(sqliteSecond),
		now.Truncate(time.Second).Add(time.Second).Format(sqliteSecond))
	if err != nil {
		return detail, err
	}
	defer rows.Close()
	for rows.Next() {
		var checkedAt time.Time
		var status protocol.Status
		if err := rows.Scan(&checkedAt, &status); err != nil {
			return detail, err
		}
		if checkedAt.Before(detail.WindowStart) || !checkedAt.Before(now) {
			continue
		}
		i := int(checkedAt.Sub(detail.WindowStart) / time.Hour)
		if status == protocol.Unknown {
			detail.Hours[i].UnknownCount++
			detail.UnknownCount++
			continue
		}
		detail.Hours[i].Checks++
		detail.Checks++
		if status == protocol.Up {
			detail.Hours[i].UpCount++
			detail.UpCount++
		}
	}
	if err := rows.Err(); err != nil {
		return detail, err
	}
	if err := rows.Close(); err != nil {
		return detail, err
	}
	return detail, tx.Commit()
}
