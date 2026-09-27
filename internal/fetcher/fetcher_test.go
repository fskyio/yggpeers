package fetcher

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"yggpeers/internal/store"
)

type archiveEntry struct {
	name string
	body string
}

type recordingStore struct {
	calls    int
	failures int
	peers    []store.PeerInput
}

func (s *recordingStore) BulkUpsertPeers(peers []store.PeerInput) error {
	s.calls++
	if s.failures > 0 {
		s.failures--
		return errors.New("store failed")
	}
	s.peers = append([]store.PeerInput(nil), peers...)
	return nil
}

func TestFetchAndParseUsesETag(t *testing.T) {
	body := makeArchive(t,
		archiveEntry{"public-peers-abc/europe/sweden.md", "`tls://se.example:1234`\n"},
		archiveEntry{"public-peers-abc/other/community.md", "`socks://other.example:5678`\n"},
		archiveEntry{"public-peers-abc/README.md", "`tcp://ignored.example:9999`\n"},
	)
	requests := 0
	archiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		switch requests {
		case 1:
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("first If-None-Match = %q, want empty", got)
			}
			w.Header().Set("ETag", `"version-1"`)
			_, _ = w.Write(body)
		case 2:
			if got := r.Header.Get("If-None-Match"); got != `"version-1"` {
				t.Errorf("second If-None-Match = %q, want version ETag", got)
			}
			w.WriteHeader(http.StatusNotModified)
		default:
			t.Errorf("unexpected request %d", requests)
		}
	}))
	defer archiveServer.Close()
	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, archiveServer.URL, http.StatusFound)
	}))
	defer redirectServer.Close()

	destination := &recordingStore{}
	f := &Fetcher{store: destination, client: redirectServer.Client(), archiveURL: redirectServer.URL}
	if err := f.FetchAndParse(context.Background()); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if err := f.FetchAndParse(context.Background()); err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	if destination.calls != 1 {
		t.Fatalf("store calls = %d, want 1", destination.calls)
	}
	if len(destination.peers) != 2 {
		t.Fatalf("stored peers = %d, want 2", len(destination.peers))
	}
	if got := destination.peers[0]; got.Country != "Sweden" || got.IsNetwork {
		t.Errorf("Sweden peer = %+v", got)
	}
	if got := destination.peers[1]; got.Country != "Community" || !got.IsNetwork {
		t.Errorf("network peer = %+v", got)
	}
}

func TestFetchAndParseRemembersETagOnlyAfterStoreSucceeds(t *testing.T) {
	body := makeArchive(t,
		archiveEntry{"public-peers-abc/europe/sweden.md", "`tls://se.example:1234`\n"},
	)
	var conditionalHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conditionalHeaders = append(conditionalHeaders, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"version-1"`)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	destination := &recordingStore{failures: 1}
	f := &Fetcher{store: destination, client: server.Client(), archiveURL: server.URL}
	if err := f.FetchAndParse(context.Background()); err == nil {
		t.Fatal("first fetch succeeded, want store error")
	}
	if err := f.FetchAndParse(context.Background()); err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	if len(conditionalHeaders) != 2 || conditionalHeaders[0] != "" || conditionalHeaders[1] != "" {
		t.Fatalf("conditional headers = %q, want two unconditional requests", conditionalHeaders)
	}
	if f.etag != `"version-1"` {
		t.Fatalf("saved ETag = %q, want version-1", f.etag)
	}
}

func TestFetchAndParseRejectsTruncatedArchive(t *testing.T) {
	body := makeArchive(t,
		archiveEntry{"public-peers-abc/europe/sweden.md", "`tls://se.example:1234`\n"},
	)
	body = body[:len(body)-4]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"broken"`)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	destination := &recordingStore{}
	f := &Fetcher{store: destination, client: server.Client(), archiveURL: server.URL}
	if err := f.FetchAndParse(context.Background()); err == nil {
		t.Fatal("fetch succeeded, want truncated archive error")
	}
	if destination.calls != 0 {
		t.Fatalf("store calls = %d, want 0", destination.calls)
	}
	if f.etag != "" {
		t.Fatalf("saved ETag = %q, want empty", f.etag)
	}
}

func TestParsePeerArchiveRejectsInvalidPath(t *testing.T) {
	body := makeArchive(t,
		archiveEntry{"public-peers-abc/../other/community.md", "`tls://example.com:1234`\n"},
	)
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	if _, err := parsePeerArchive(gz); err == nil {
		t.Fatal("parse succeeded, want invalid path error")
	}
}

func makeArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name,
			Mode: 0o644,
			Size: int64(len(entry.body)),
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
