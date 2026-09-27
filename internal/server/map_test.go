package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yggpeers/internal/config"
)

func TestMapTileRanges(t *testing.T) {
	srv, err := New(config.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(countryTiles[:8], []byte{'P', 'M', 'T', 'i', 'l', 'e', 's', 3}) {
		t.Fatal("expected a PMTiles v3 archive")
	}
	for _, start := range []int{0, 16384, len(countryTiles) - 16384} {
		t.Run(fmt.Sprint(start), func(t *testing.T) {
			req := httptest.NewRequest("GET", "/static/countries.pmtiles", nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+16383))
			req.Header.Set("Accept-Encoding", "gzip")
			res := httptest.NewRecorder()
			srv.ServeHTTP(res, req)
			if res.Code != http.StatusPartialContent {
				t.Fatalf("status = %d; expected a partial response", res.Code)
			}
			if !bytes.Equal(res.Body.Bytes(), countryTiles[start:start+16384]) {
				t.Fatal("range did not return the requested archive bytes")
			}
			want := fmt.Sprintf("bytes %d-%d/%d", start, start+16383, len(countryTiles))
			if res.Header().Get("Content-Range") != want || res.Header().Get("Content-Length") != "16384" {
				t.Fatalf("incorrect range headers: %v", res.Header())
			}
			if res.Header().Get("ETag") != countryTilesETag || res.Header().Get("Accept-Ranges") != "bytes" {
				t.Fatalf("missing archive identity/range support: %v", res.Header())
			}
			if res.Header().Get("Content-Encoding") != "" {
				t.Fatal("whole-response compression would invalidate PMTiles byte offsets")
			}
		})
	}

	req := httptest.NewRequest("GET", "/static/countries.pmtiles", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(countryTiles)))
	res := httptest.NewRecorder()
	srv.ServeHTTP(res, req)
	if res.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-bounds range: status = %d", res.Code)
	}

	req = httptest.NewRequest("GET", "/static/countries.pmtiles", nil)
	req.Header.Set("If-None-Match", countryTilesETag)
	res = httptest.NewRecorder()
	srv.ServeHTTP(res, req)
	if res.Code != http.StatusNotModified || res.Body.Len() != 0 {
		t.Fatal("unchanged archive should be revalidated without downloading it")
	}
}

func TestMapCountryCatalog(t *testing.T) {
	data, err := staticFS.ReadFile("static/countries.json")
	if err != nil {
		t.Fatal(err)
	}
	var countries map[string]struct {
		Name       string    `json:"name"`
		Aliases    []string  `json:"aliases"`
		Marker     []float64 `json:"marker"`
		MarkerZoom int       `json:"markerZoom"`
	}
	if err := json.Unmarshal(data, &countries); err != nil {
		t.Fatal(err)
	}
	aliases := make(map[string]string)
	for code, country := range countries {
		for _, alias := range country.Aliases {
			key := strings.ToLower(strings.TrimSpace(alias))
			if previous, ok := aliases[key]; ok && previous != code {
				t.Errorf("ambiguous country alias %q: %s and %s", alias, previous, code)
			}
			aliases[key] = code
		}
	}
	for name, code := range map[string]string{
		"Hong Kong": "HK", "Singapore": "SG", "United States": "US",
		"Czech Republic": "CZ", "Russia": "RU", "South Korea": "KR",
	} {
		if aliases[strings.ToLower(name)] != code {
			t.Errorf("%s does not resolve to %s", name, code)
		}
	}
	for _, code := range []string{"HK", "SG"} {
		country := countries[code]
		if len(country.Marker) != 2 || country.MarkerZoom <= 2 || country.MarkerZoom > 11 {
			t.Errorf("%s needs a world-view marker that gives way to its polygon: %+v", code, country)
		}
	}
}
