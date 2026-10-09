package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"yggpeers/internal/protocol"
)

var (
	ErrUnauthorized  = errors.New("invalid or disabled probe credential")
	ErrInvalidReport = errors.New("invalid probe report or unassigned target")
	ErrConflict      = errors.New("result conflicts with an accepted observation")
	ErrRoundClosed   = errors.New("check round has closed")
)

const probeTimeFormat = "2006-01-02 15:04:05.999999999"

type Probe struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Enabled      bool       `json:"enabled"`
	CheckDarknet bool       `json:"check_darknet"`
	RegisteredAt *time.Time `json:"registered_at"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
}

type ProbeObservation struct {
	ID         string
	Name       string
	Eligible   bool
	Status     protocol.Status
	ReportedAt *time.Time
	LastSeenAt *time.Time
}

type ProbeCoverage struct {
	RoundID    int64
	StartedAt  time.Time
	Deadline   time.Time
	Registered int
	Eligible   int
	Reported   int
	Successful int
	Probes     []ProbeObservation
}

type probeRound struct {
	ID         int64
	StartedAt  time.Time
	Deadline   time.Time
	ValidUntil time.Time
}

func (s *Store) migrateProbes() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS probes (
			id TEXT PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			token_hash BLOB UNIQUE NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			check_darknet INTEGER NOT NULL DEFAULT 0,
			registered_at DATETIME,
			last_seen_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS probe_rounds (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			started_at DATETIME NOT NULL,
			deadline_at DATETIME NOT NULL,
			valid_until DATETIME NOT NULL,
			finalized INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS round_probes (
			round_id INTEGER NOT NULL REFERENCES probe_rounds(id) ON DELETE CASCADE,
			probe_id TEXT NOT NULL REFERENCES probes(id),
			name TEXT NOT NULL,
			check_darknet INTEGER NOT NULL,
			PRIMARY KEY (round_id, probe_id)
		);
		CREATE TABLE IF NOT EXISTS round_targets (
			round_id INTEGER NOT NULL REFERENCES probe_rounds(id) ON DELETE CASCADE,
			peer_id INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
			uri TEXT NOT NULL,
			protocol TEXT NOT NULL,
			host TEXT NOT NULL,
			port TEXT NOT NULL,
			is_network INTEGER NOT NULL,
			expected_probes INTEGER NOT NULL,
			registered_probes INTEGER NOT NULL,
			last_report_at DATETIME,
			finalized INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (round_id, peer_id)
		);
		CREATE TABLE IF NOT EXISTS probe_results (
			round_id INTEGER NOT NULL,
			probe_id TEXT NOT NULL,
			peer_id INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('up', 'down')),
			received_at DATETIME NOT NULL,
			PRIMARY KEY (round_id, probe_id, peer_id),
			FOREIGN KEY (round_id, probe_id) REFERENCES round_probes(round_id, probe_id) ON DELETE CASCADE,
			FOREIGN KEY (round_id, peer_id) REFERENCES round_targets(round_id, peer_id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS idx_probe_results_peer_round ON probe_results(peer_id, round_id);
	`)
	return err
}

func newProbeSecret() (string, []byte, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(token))
	return token, hash[:], nil
}

// CreateProbe provisions an identity. It joins rounds only after authenticated
// check-in, and its credential is never persisted in plaintext.
func (s *Store) CreateProbe(name string) (Probe, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 || strings.ContainsAny(name, "\r\n\t") {
		return Probe{}, "", errors.New("probe name must be 1–120 bytes without control separators")
	}
	token, hash, err := newProbeSecret()
	if err != nil {
		return Probe{}, "", err
	}
	p := Probe{ID: rand.Text(), Name: name, Enabled: true}
	if _, err := s.db.Exec(`INSERT INTO probes (id, name, token_hash) VALUES (?, ?, ?)`, p.ID, p.Name, hash); err != nil {
		return Probe{}, "", err
	}
	return p, token, nil
}

func (s *Store) ListProbes() ([]Probe, error) {
	rows, err := s.db.Query(`SELECT id, name, enabled, check_darknet, registered_at, last_seen_at FROM probes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	probes := make([]Probe, 0)
	for rows.Next() {
		var p Probe
		if err := rows.Scan(&p.ID, &p.Name, &p.Enabled, &p.CheckDarknet, &p.RegisteredAt, &p.LastSeenAt); err != nil {
			return nil, err
		}
		probes = append(probes, p)
	}
	return probes, rows.Err()
}

func (s *Store) SetProbeEnabled(id string, enabled bool) error {
	result, err := s.db.Exec(`UPDATE probes SET enabled = ? WHERE id = ?`, enabled, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Store) RotateProbeToken(id string) (string, error) {
	token, hash, err := newProbeSecret()
	if err != nil {
		return "", err
	}
	result, err := s.db.Exec(`UPDATE probes SET token_hash = ? WHERE id = ?`, hash, id)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", sql.ErrNoRows
	}
	return token, nil
}

func (s *Store) AuthenticateProbe(token string) (Probe, error) {
	if token == "" || len(token) > 128 {
		return Probe{}, ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(token))
	var p Probe
	err := s.db.QueryRow(`SELECT id, name, enabled, check_darknet, registered_at, last_seen_at
		FROM probes WHERE token_hash = ? AND enabled = 1`, hash[:]).Scan(
		&p.ID, &p.Name, &p.Enabled, &p.CheckDarknet, &p.RegisteredAt, &p.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Probe{}, ErrUnauthorized
	}
	return p, err
}

func (s *Store) CheckInProbe(id string, checkDarknet bool, now time.Time) (*protocol.Assignment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	at := now.UTC().Format(probeTimeFormat)
	result, err := tx.Exec(`UPDATE probes SET registered_at = COALESCE(registered_at, ?),
		last_seen_at = ?, check_darknet = ? WHERE id = ? AND enabled = 1`, at, at, checkDarknet, id)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return nil, err
		}
		return nil, ErrUnauthorized
	}
	var work protocol.Assignment
	var darknet bool
	err = tx.QueryRow(`SELECT r.id, r.started_at, r.deadline_at, p.check_darknet
		FROM probe_rounds r JOIN round_probes p ON p.round_id = r.id
		WHERE p.probe_id = ? AND r.finalized = 0 AND r.deadline_at > ?
		ORDER BY r.id DESC LIMIT 1`, id, at).Scan(&work.RoundID, &work.StartedAt, &work.Deadline, &darknet)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT t.peer_id, t.uri, t.protocol, t.host, t.port, t.is_network
		FROM round_targets t WHERE t.round_id = ? AND t.finalized = 0
		AND (t.is_network = 0 OR ?)
		AND NOT EXISTS (SELECT 1 FROM probe_results o WHERE o.round_id = t.round_id
			AND o.peer_id = t.peer_id AND o.probe_id = ?)
		ORDER BY t.peer_id`, work.RoundID, darknet, id)
	if err != nil {
		return nil, err
	}
	work.Targets = make([]protocol.Target, 0)
	for rows.Next() {
		var target protocol.Target
		if err := rows.Scan(&target.PeerID, &target.URI, &target.Protocol, &target.Host, &target.Port, &target.IsNetwork); err != nil {
			rows.Close()
			return nil, err
		}
		work.Targets = append(work.Targets, target)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(work.Targets) == 0 {
		return nil, nil
	}
	return &work, nil
}

// AdvanceProbeRounds recovers persisted rounds and starts at most one new round.
// An outage creates a measurement gap, never manufactured up/down samples.
func (s *Store) AdvanceProbeRounds(now time.Time, interval, timeout time.Duration) (bool, error) {
	if interval <= 0 || timeout <= 0 || timeout >= interval {
		return false, errors.New("round timeout must be positive and less than check interval")
	}
	now = now.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, started_at, deadline_at, valid_until FROM probe_rounds
		WHERE finalized = 0 AND deadline_at <= ? ORDER BY id`, now.Format(probeTimeFormat))
	if err != nil {
		return false, err
	}
	var expired []probeRound
	for rows.Next() {
		var r probeRound
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.Deadline, &r.ValidUntil); err != nil {
			rows.Close()
			return false, err
		}
		expired = append(expired, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	changed := false
	for _, r := range expired {
		ids, err := roundTargetIDs(tx, r.ID, false)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			if _, err := publishRoundTarget(tx, r, id, now, true); err != nil {
				return false, err
			}
		}
		if _, err := tx.Exec(`UPDATE probe_rounds SET finalized = 1 WHERE id = ?`, r.ID); err != nil {
			return false, err
		}
		changed = true
	}
	var last time.Time
	err = tx.QueryRow(`SELECT started_at FROM probe_rounds ORDER BY id DESC LIMIT 1`).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if errors.Is(err, sql.ErrNoRows) || !now.Before(last.Add(interval)) {
		r := probeRound{StartedAt: now, Deadline: now.Add(timeout), ValidUntil: now.Add(interval + timeout)}
		result, err := tx.Exec(`INSERT INTO probe_rounds (started_at, deadline_at, valid_until) VALUES (?, ?, ?)`,
			r.StartedAt.Format(probeTimeFormat), r.Deadline.Format(probeTimeFormat), r.ValidUntil.Format(probeTimeFormat))
		if err != nil {
			return false, err
		}
		r.ID, err = result.LastInsertId()
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(`INSERT INTO round_probes (round_id, probe_id, name, check_darknet)
			SELECT ?, id, name, check_darknet FROM probes WHERE enabled = 1 AND registered_at IS NOT NULL`, r.ID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`INSERT INTO round_targets
			(round_id, peer_id, uri, protocol, host, port, is_network, expected_probes, registered_probes)
			SELECT ?, p.id, p.uri, p.protocol, p.host, p.port, p.is_network,
				(SELECT COUNT(*) FROM round_probes rp WHERE rp.round_id = ? AND (p.is_network = 0 OR rp.check_darknet = 1)),
				(SELECT COUNT(*) FROM round_probes rp WHERE rp.round_id = ?)
			FROM peers p`, r.ID, r.ID, r.ID); err != nil {
			return false, err
		}
		// Targets without coverage are immediately unknown, not failed dials.
		ids, err := roundTargetIDs(tx, r.ID, true)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			if _, err := publishRoundTarget(tx, r, id, now, true); err != nil {
				return false, err
			}
		}
		if err := finalizeCompletedRound(tx, r.ID); err != nil {
			return false, err
		}
		changed = true
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if changed {
		s.invalidateCaches()
	}
	return changed, nil
}

func roundTargetIDs(tx *sql.Tx, roundID int64, noCoverageOnly bool) ([]int64, error) {
	query := `SELECT peer_id FROM round_targets WHERE round_id = ? AND finalized = 0`
	if noCoverageOnly {
		query += ` AND expected_probes = 0`
	}
	rows, err := tx.Query(query, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func finalizeCompletedRound(tx *sql.Tx, roundID int64) error {
	_, err := tx.Exec(`UPDATE probe_rounds SET finalized = 1 WHERE id = ?
		AND NOT EXISTS (SELECT 1 FROM round_targets WHERE round_id = ? AND finalized = 0)`, roundID, roundID)
	return err
}

func (s *Store) SubmitProbeResults(id string, report protocol.ReportRequest, now time.Time) (int, error) {
	if report.RoundID <= 0 || len(report.Results) == 0 {
		return 0, ErrInvalidReport
	}
	now = now.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var enabled bool
	if err := tx.QueryRow(`SELECT enabled FROM probes WHERE id = ?`, id).Scan(&enabled); err != nil || !enabled {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		return 0, ErrUnauthorized
	}
	var r probeRound
	var finalized bool
	var darknet bool
	err = tx.QueryRow(`SELECT r.id, r.started_at, r.deadline_at, r.valid_until, r.finalized, p.check_darknet
		FROM probe_rounds r JOIN round_probes p ON p.round_id = r.id
		WHERE r.id = ? AND p.probe_id = ?`, report.RoundID, id).Scan(
		&r.ID, &r.StartedAt, &r.Deadline, &r.ValidUntil, &finalized, &darknet)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrInvalidReport
	}
	if err != nil {
		return 0, err
	}
	open := !finalized && now.Before(r.Deadline)
	accepted := 0
	changedTargets := make([]int64, 0, len(report.Results))
	for _, observation := range report.Results {
		if !observation.Valid() {
			return 0, ErrInvalidReport
		}
		var network bool
		err := tx.QueryRow(`SELECT is_network FROM round_targets WHERE round_id = ? AND peer_id = ?`,
			r.ID, observation.PeerID).Scan(&network)
		if errors.Is(err, sql.ErrNoRows) {
			// Catalog removals cascade their assignments. Never recreate an
			// endpoint from an observation, or apply it to a re-added URI.
			var exists bool
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM peers WHERE id = ?)`, observation.PeerID).Scan(&exists); err != nil {
				return 0, err
			}
			if !exists {
				continue
			}
			return 0, ErrInvalidReport
		}
		if err != nil {
			return 0, err
		}
		if network && !darknet {
			return 0, ErrInvalidReport
		}
		var previous protocol.Result
		err = tx.QueryRow(`SELECT status, COALESCE(node_key, ''), COALESCE(protocol_version, '')
			FROM probe_results WHERE round_id = ? AND probe_id = ? AND peer_id = ?`,
			r.ID, id, observation.PeerID).Scan(&previous.Status, &previous.Key, &previous.ProtocolVersion)
		if err == nil {
			previous.PeerID = observation.PeerID
			if previous != observation {
				return 0, ErrConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if !open {
			return 0, ErrRoundClosed
		}
		at := now.Format(probeTimeFormat)
		if _, err := tx.Exec(`INSERT INTO probe_results (round_id, probe_id, peer_id, status, node_key, protocol_version, received_at)
			VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`, r.ID, id, observation.PeerID, observation.Status,
			observation.Key, observation.ProtocolVersion, at); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE round_targets SET last_report_at = ? WHERE round_id = ? AND peer_id = ?`,
			at, r.ID, observation.PeerID); err != nil {
			return 0, err
		}
		if observation.Key != "" {
			if _, err := tx.Exec(`UPDATE peers SET node_key = ?, protocol_version = ?, node_seen_at = ? WHERE id = ?`,
				observation.Key, observation.ProtocolVersion, at, observation.PeerID); err != nil {
				return 0, err
			}
		}
		accepted++
		changedTargets = append(changedTargets, observation.PeerID)
	}
	published := false
	for _, peerID := range changedTargets {
		changed, err := publishRoundTarget(tx, r, peerID, now, false)
		if err != nil {
			return 0, err
		}
		published = published || changed
	}
	if err := finalizeCompletedRound(tx, r.ID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE probes SET last_seen_at = ? WHERE id = ?`, now.Format(probeTimeFormat), id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if published {
		s.invalidateCaches()
	}
	return accepted, nil
}

// publishRoundTarget changes current state only when there is sufficient
// evidence, or the round closes. Only finalization writes an uptime sample.
func publishRoundTarget(tx *sql.Tx, r probeRound, peerID int64, now time.Time, force bool) (bool, error) {
	var expected, registered, reported, successful int
	var finalized bool
	var lastReport *time.Time
	err := tx.QueryRow(`SELECT t.expected_probes, t.registered_probes, t.finalized, t.last_report_at,
		(SELECT COUNT(*) FROM probe_results o WHERE o.round_id = t.round_id AND o.peer_id = t.peer_id),
		(SELECT COUNT(*) FROM probe_results o WHERE o.round_id = t.round_id AND o.peer_id = t.peer_id AND o.status = 'up')
		FROM round_targets t WHERE t.round_id = ? AND t.peer_id = ?`, r.ID, peerID).Scan(
		&expected, &registered, &finalized, &lastReport, &reported, &successful)
	if err != nil {
		return false, err
	}
	if finalized {
		return false, nil
	}
	complete := reported == expected
	if !force && !complete && successful == 0 {
		return false, nil
	}
	status := protocol.Unknown
	if successful > 0 {
		status = protocol.Up
	} else if expected > 0 && complete {
		status = protocol.Down
	}
	var observedAt any
	if lastReport != nil {
		observedAt = lastReport.UTC().Format(probeTimeFormat)
	}
	at := now.UTC().Format(probeTimeFormat)
	// Transitions compare against the effective previous status. An expired or
	// absent lease is already unknown, so fresh evidence starts a new baseline;
	// unknown following an expired lease changed at that expiry.
	if _, err := tx.Exec(`UPDATE peers SET status = ?,
		state_changed_at = CASE
			WHEN (CASE WHEN status_valid_until > ? THEN status ELSE 'unknown' END) != ? THEN ?
			WHEN status != ? THEN COALESCE(status_valid_until, ?)
			ELSE state_changed_at END,
		last_checked_at = COALESCE(?, last_checked_at), status_valid_until = ?, last_round_id = ?,
		successful_probes = ?, reporting_probes = ?, expected_probes = ?, registered_probes = ?
		WHERE id = ? AND (last_round_id IS NULL OR last_round_id <= ?)`, status, at, status, at, status, at,
		observedAt, r.ValidUntil.Format(probeTimeFormat), r.ID, successful, reported, expected, registered, peerID, r.ID); err != nil {
		return false, err
	}
	if !complete && !force {
		return true, nil
	}
	if _, err := tx.Exec(`INSERT INTO checks (peer_id, checked_at, status, round_id) VALUES (?, ?, ?, ?)`,
		peerID, r.StartedAt.Format(probeTimeFormat), status, r.ID); err != nil {
		return false, err
	}
	known, up, unknown := 1, 0, 0
	if status == protocol.Up {
		up = 1
	} else if status == protocol.Unknown {
		known, unknown = 0, 1
	}
	if _, err := tx.Exec(`INSERT INTO daily_peer_stats (peer_id, day, checks, up_count, unknown_count)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(peer_id, day) DO UPDATE SET
		checks = checks + excluded.checks, up_count = up_count + excluded.up_count,
		unknown_count = unknown_count + excluded.unknown_count`, peerID, r.StartedAt.Format("2006-01-02"), known, up, unknown); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE round_targets SET finalized = 1 WHERE round_id = ? AND peer_id = ?`, r.ID, peerID); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) GetPeerProbeCoverage(peerID int64) (ProbeCoverage, error) {
	var coverage ProbeCoverage
	tx, err := s.db.Begin()
	if err != nil {
		return coverage, err
	}
	defer tx.Rollback()
	var roundID *int64
	var network bool
	if err := tx.QueryRow(`SELECT last_round_id, is_network FROM peers WHERE id = ?`, peerID).Scan(&roundID, &network); err != nil {
		return coverage, err
	}
	if roundID == nil {
		coverage, err = unassignedProbeCoverage(tx, network)
		if err != nil {
			return coverage, err
		}
		return coverage, tx.Commit()
	}
	coverage.RoundID = *roundID
	err = tx.QueryRow(`SELECT r.started_at, r.deadline_at, t.registered_probes, t.expected_probes, t.is_network
		FROM probe_rounds r JOIN round_targets t ON t.round_id = r.id
		WHERE r.id = ? AND t.peer_id = ?`, *roundID, peerID).Scan(
		&coverage.StartedAt, &coverage.Deadline, &coverage.Registered, &coverage.Eligible, &network)
	if errors.Is(err, sql.ErrNoRows) {
		// The last published round may have aged out while the peer remains.
		coverage, err = unassignedProbeCoverage(tx, network)
		if err != nil {
			return coverage, err
		}
		return coverage, tx.Commit()
	}
	if err != nil {
		return coverage, err
	}
	rows, err := tx.Query(`SELECT rp.probe_id, rp.name, rp.check_darknet, p.last_seen_at, o.status, o.received_at
		FROM round_probes rp JOIN probes p ON p.id = rp.probe_id
		LEFT JOIN probe_results o ON o.round_id = rp.round_id AND o.probe_id = rp.probe_id AND o.peer_id = ?
		WHERE rp.round_id = ? ORDER BY rp.name`, peerID, *roundID)
	if err != nil {
		return coverage, err
	}
	for rows.Next() {
		var p ProbeObservation
		var darknet bool
		var status sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &darknet, &p.LastSeenAt, &status, &p.ReportedAt); err != nil {
			rows.Close()
			return coverage, err
		}
		p.Eligible = !network || darknet
		p.Status = protocol.Unknown
		if status.Valid {
			p.Status = protocol.Status(status.String)
			coverage.Reported++
			if p.Status == protocol.Up {
				coverage.Successful++
			}
		}
		coverage.Probes = append(coverage.Probes, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return coverage, err
	}
	return coverage, tx.Commit()
}

func unassignedProbeCoverage(tx *sql.Tx, network bool) (ProbeCoverage, error) {
	var coverage ProbeCoverage
	rows, err := tx.Query(`SELECT id, name, check_darknet, last_seen_at FROM probes
		WHERE enabled = 1 AND registered_at IS NOT NULL ORDER BY name`)
	if err != nil {
		return coverage, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ProbeObservation
		var darknet bool
		if err := rows.Scan(&p.ID, &p.Name, &darknet, &p.LastSeenAt); err != nil {
			return coverage, err
		}
		p.Eligible = !network || darknet
		p.Status = protocol.Unknown
		coverage.Probes = append(coverage.Probes, p)
		coverage.Registered++
		if p.Eligible {
			coverage.Eligible++
		}
	}
	return coverage, rows.Err()
}
