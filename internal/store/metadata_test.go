package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMetadataPersistsUnknownAndStaleSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, get := range []func() error{
		func() error { _, err := s.GetHostMetadata("missing.example"); return err },
		func() error { _, err := s.GetIPMetadata("8.8.8.8"); return err },
	} {
		if err := get(); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing metadata error = %v", err)
		}
	}
	at := time.Date(2026, 7, 1, 12, 0, 0, 123456789, time.FixedZone("offset", 3600))
	unknown := HostMetadata{Host: "unknown.example", Error: "timeout", NextAttemptAt: at.Add(time.Minute)}
	if err := s.SaveHostMetadata(unknown); err != nil {
		t.Fatal(err)
	}
	var success sql.NullString
	if err := s.db.QueryRow(`SELECT resolved_at FROM host_metadata WHERE host = ?`, unknown.Host).Scan(&success); err != nil || success.Valid {
		t.Fatalf("never-resolved snapshot became a success: %v, %v", success, err)
	}
	host := HostMetadata{Host: "peer.example", Addresses: []string{"8.8.8.8"}, ResolvedAt: at, ExpiresAt: at.Add(15 * time.Minute), NextAttemptAt: at.Add(15 * time.Minute)}
	ip := IPMetadata{IP: "8.8.8.8", ASNs: []uint32{15169, 64500}, Prefix: "8.8.8.0/24", Name: "GOOGLE; OTHER", LookedUpAt: at, ExpiresAt: at.Add(24 * time.Hour), NextAttemptAt: at.Add(24 * time.Hour)}
	if err := s.SaveHostMetadata(host); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIPMetadata(ip); err != nil {
		t.Fatal(err)
	}
	// A failed refresh retains the successful snapshot but records its own
	// error and retry schedule. Restart must not turn this into fresh evidence.
	host.Error, host.NextAttemptAt = "DNS failure", at.Add(16*time.Minute)
	ip.Error, ip.NextAttemptAt = "Cymru failure", at.Add(25*time.Hour)
	if err := s.SaveHostMetadata(host); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIPMetadata(ip); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hosts, err := s.GetHostMetadataAll()
	if err != nil {
		t.Fatal(err)
	}
	got := hosts[host.Host]
	if got.Error != host.Error || !got.ResolvedAt.Equal(at) || !got.ExpiresAt.Equal(host.ExpiresAt) || !got.NextAttemptAt.Equal(host.NextAttemptAt) || len(got.Addresses) != 1 || got.Addresses[0] != "8.8.8.8" {
		t.Fatalf("stale DNS snapshot lost on restart: %+v", got)
	}
	if u := hosts[unknown.Host]; !u.ResolvedAt.IsZero() || !u.ExpiresAt.IsZero() || u.Error != "timeout" || !u.NextAttemptAt.Equal(unknown.NextAttemptAt) {
		t.Fatalf("unknown DNS state lost on restart: %+v", u)
	}
	ips, err := s.GetIPMetadataAll()
	if err != nil {
		t.Fatal(err)
	}
	gotIP := ips[ip.IP]
	if gotIP.Error != ip.Error || !gotIP.LookedUpAt.Equal(at) || !gotIP.NextAttemptAt.Equal(ip.NextAttemptAt) || len(gotIP.ASNs) != 2 || gotIP.ASNs[0] != 15169 || gotIP.ASNs[1] != 64500 || gotIP.Prefix != ip.Prefix || gotIP.Name != ip.Name {
		t.Fatalf("stale ASN snapshot lost on restart: %+v", gotIP)
	}
	// Successful NXDOMAIN overwrites old addresses rather than leaving an
	// ambiguous empty/unknown state, and clears a prior error.
	host.Addresses, host.Error = []string{}, ""
	host.ResolvedAt = at.Add(time.Hour)
	host.ExpiresAt, host.NextAttemptAt = host.ResolvedAt.Add(time.Minute), host.ResolvedAt.Add(time.Minute)
	if err := s.SaveHostMetadata(host); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetHostMetadata(host.Host)
	if err != nil || len(got.Addresses) != 0 || got.ResolvedAt.IsZero() || got.Error != "" {
		t.Fatalf("negative DNS result did not replace stale addresses: %+v, %v", got, err)
	}
}
