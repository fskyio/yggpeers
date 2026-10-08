package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// HostMetadata preserves the last successful DNS snapshot separately from the
// most recent refresh error. ResolvedAt is zero before the first success; an
// empty Addresses with a nonzero ResolvedAt is a successful negative result.
// Addresses are usable as relationship evidence only while Error is empty and
// ExpiresAt is in the future. ExpiresAt is application freshness, not DNS TTL.
type HostMetadata struct {
	Host          string
	Addresses     []string
	ResolvedAt    time.Time
	ExpiresAt     time.Time
	NextAttemptAt time.Time
	Error         string
}

// IPMetadata has the same snapshot/error semantics as HostMetadata. An empty
// ASNs with a nonzero LookedUpAt means the DNS service returned no origin.
type IPMetadata struct {
	IP            string
	ASNs          []uint32
	Prefix        string
	Name          string
	LookedUpAt    time.Time
	ExpiresAt     time.Time
	NextAttemptAt time.Time
	Error         string
}

func (s *Store) migrateMetadata() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS host_metadata (
			host TEXT PRIMARY KEY NOT NULL,
			addresses TEXT NOT NULL,
			resolved_at TEXT,
			expires_at TEXT,
			next_attempt_at TEXT,
			error TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS ip_metadata (
			ip TEXT PRIMARY KEY NOT NULL,
			asns TEXT NOT NULL,
			prefix TEXT NOT NULL,
			name TEXT NOT NULL,
			looked_up_at TEXT,
			expires_at TEXT,
			next_attempt_at TEXT,
			error TEXT NOT NULL DEFAULT ''
		);
	`)
	return err
}

func (s *Store) SaveHostMetadata(m HostMetadata) error {
	addresses, err := json.Marshal(m.Addresses)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO host_metadata(host, addresses, resolved_at, expires_at, next_attempt_at, error)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(host) DO UPDATE SET addresses=excluded.addresses,
		resolved_at=excluded.resolved_at, expires_at=excluded.expires_at,
		next_attempt_at=excluded.next_attempt_at, error=excluded.error`,
		m.Host, string(addresses), metadataTime(m.ResolvedAt), metadataTime(m.ExpiresAt), metadataTime(m.NextAttemptAt), m.Error)
	return err
}

func (s *Store) SaveIPMetadata(m IPMetadata) error {
	asns, err := json.Marshal(m.ASNs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO ip_metadata(ip, asns, prefix, name, looked_up_at, expires_at, next_attempt_at, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET asns=excluded.asns, prefix=excluded.prefix,
		name=excluded.name, looked_up_at=excluded.looked_up_at, expires_at=excluded.expires_at,
		next_attempt_at=excluded.next_attempt_at, error=excluded.error`,
		m.IP, string(asns), m.Prefix, m.Name, metadataTime(m.LookedUpAt), metadataTime(m.ExpiresAt), metadataTime(m.NextAttemptAt), m.Error)
	return err
}

const hostMetadataColumns = `host, addresses, resolved_at, expires_at, next_attempt_at, error`
const ipMetadataColumns = `ip, asns, prefix, name, looked_up_at, expires_at, next_attempt_at, error`

func (s *Store) GetHostMetadata(host string) (HostMetadata, error) {
	return scanHostMetadata(s.db.QueryRow(`SELECT `+hostMetadataColumns+` FROM host_metadata WHERE host = ?`, host))
}

func (s *Store) GetIPMetadata(ip string) (IPMetadata, error) {
	return scanIPMetadata(s.db.QueryRow(`SELECT `+ipMetadataColumns+` FROM ip_metadata WHERE ip = ?`, ip))
}

func (s *Store) GetHostMetadataAll() (map[string]HostMetadata, error) {
	rows, err := s.db.Query(`SELECT ` + hostMetadataColumns + ` FROM host_metadata`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]HostMetadata)
	for rows.Next() {
		m, err := scanHostMetadata(rows)
		if err != nil {
			return nil, err
		}
		result[m.Host] = m
	}
	return result, rows.Err()
}

func (s *Store) GetIPMetadataAll() (map[string]IPMetadata, error) {
	rows, err := s.db.Query(`SELECT ` + ipMetadataColumns + ` FROM ip_metadata`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]IPMetadata)
	for rows.Next() {
		m, err := scanIPMetadata(rows)
		if err != nil {
			return nil, err
		}
		result[m.IP] = m
	}
	return result, rows.Err()
}

type metadataScanner interface {
	Scan(...any) error
}

func scanHostMetadata(row metadataScanner) (HostMetadata, error) {
	var m HostMetadata
	var addresses string
	var resolved, expires, next sql.NullString
	if err := row.Scan(&m.Host, &addresses, &resolved, &expires, &next, &m.Error); err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(addresses), &m.Addresses); err != nil {
		return m, fmt.Errorf("host metadata addresses: %w", err)
	}
	if err := scanMetadataTimes(resolved, expires, next, &m.ResolvedAt, &m.ExpiresAt, &m.NextAttemptAt); err != nil {
		return m, err
	}
	return m, nil
}

func scanIPMetadata(row metadataScanner) (IPMetadata, error) {
	var m IPMetadata
	var asns string
	var lookedUp, expires, next sql.NullString
	if err := row.Scan(&m.IP, &asns, &m.Prefix, &m.Name, &lookedUp, &expires, &next, &m.Error); err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(asns), &m.ASNs); err != nil {
		return m, fmt.Errorf("IP metadata ASNs: %w", err)
	}
	if err := scanMetadataTimes(lookedUp, expires, next, &m.LookedUpAt, &m.ExpiresAt, &m.NextAttemptAt); err != nil {
		return m, err
	}
	return m, nil
}

func metadataTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func scanMetadataTimes(first, expires, next sql.NullString, a, b, c *time.Time) error {
	destinations := [...]*time.Time{a, b, c}
	for i, value := range []sql.NullString{first, expires, next} {
		if !value.Valid {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, value.String)
		if err != nil {
			return fmt.Errorf("metadata timestamp: %w", err)
		}
		*destinations[i] = t
	}
	return nil
}
