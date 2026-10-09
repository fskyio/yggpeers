package store

import (
	"database/sql"
	"fmt"
	"time"
)

// NodeMetadata is a node's NodeInfo snapshot, with the same snapshot/error
// semantics as HostMetadata. Empty build fields with a nonzero FetchedAt mean
// the node does not publish them.
type NodeMetadata struct {
	Key           string
	BuildName     string
	BuildVersion  string
	BuildPlatform string
	BuildArch     string
	FetchedAt     time.Time
	ExpiresAt     time.Time
	NextAttemptAt time.Time
	Error         string
}

// peerNodeColumns selects Peer's identity fields from peers aliased as p.
const peerNodeColumns = `COALESCE(p.node_key, ''), COALESCE(p.protocol_version, ''), p.node_seen_at`

// migrateNodes adds the handshake identity to probe results and peers. A peer
// keeps the identity from its latest successful handshake.
func (s *Store) migrateNodes() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for table, columns := range map[string][]struct{ name, definition string }{
		"peers":         {{"node_key", "TEXT"}, {"protocol_version", "TEXT"}, {"node_seen_at", "DATETIME"}},
		"probe_results": {{"node_key", "TEXT"}, {"protocol_version", "TEXT"}},
	} {
		existing, err := tableColumns(tx, table)
		if err != nil {
			return err
		}
		for _, column := range columns {
			if existing[column.name] {
				continue
			}
			if _, err := tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return fmt.Errorf("migrate %s %s: %w", table, column.name, err)
			}
		}
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS node_metadata (
		node_key TEXT PRIMARY KEY NOT NULL,
		build_name TEXT NOT NULL,
		build_version TEXT NOT NULL,
		build_platform TEXT NOT NULL,
		build_arch TEXT NOT NULL,
		fetched_at TEXT,
		expires_at TEXT,
		next_attempt_at TEXT,
		error TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveNodeMetadata(m NodeMetadata) error {
	_, err := s.db.Exec(`
		INSERT INTO node_metadata(node_key, build_name, build_version, build_platform, build_arch,
			fetched_at, expires_at, next_attempt_at, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_key) DO UPDATE SET build_name=excluded.build_name,
		build_version=excluded.build_version, build_platform=excluded.build_platform,
		build_arch=excluded.build_arch, fetched_at=excluded.fetched_at, expires_at=excluded.expires_at,
		next_attempt_at=excluded.next_attempt_at, error=excluded.error`,
		m.Key, m.BuildName, m.BuildVersion, m.BuildPlatform, m.BuildArch,
		metadataTime(m.FetchedAt), metadataTime(m.ExpiresAt), metadataTime(m.NextAttemptAt), m.Error)
	return err
}

const nodeMetadataColumns = `node_key, build_name, build_version, build_platform, build_arch, fetched_at, expires_at, next_attempt_at, error`

func (s *Store) GetNodeMetadata(key string) (NodeMetadata, error) {
	return scanNodeMetadata(s.db.QueryRow(`SELECT `+nodeMetadataColumns+` FROM node_metadata WHERE node_key = ?`, key))
}

func (s *Store) GetNodeMetadataAll() (map[string]NodeMetadata, error) {
	rows, err := s.db.Query(`SELECT ` + nodeMetadataColumns + ` FROM node_metadata`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]NodeMetadata)
	for rows.Next() {
		m, err := scanNodeMetadata(rows)
		if err != nil {
			return nil, err
		}
		result[m.Key] = m
	}
	return result, rows.Err()
}

func scanNodeMetadata(row metadataScanner) (NodeMetadata, error) {
	var m NodeMetadata
	var fetched, expires, next sql.NullString
	if err := row.Scan(&m.Key, &m.BuildName, &m.BuildVersion, &m.BuildPlatform, &m.BuildArch,
		&fetched, &expires, &next, &m.Error); err != nil {
		return m, err
	}
	return m, scanMetadataTimes(fetched, expires, next, &m.FetchedAt, &m.ExpiresAt, &m.NextAttemptAt)
}
