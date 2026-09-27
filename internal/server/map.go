package server

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"time"
)

var countryTiles = func() []byte {
	data, err := staticFS.ReadFile("static/countries.pmtiles")
	if err != nil {
		panic(err) // The archive is a required embedded asset.
	}
	return data
}()

var countryTilesETag = fmt.Sprintf(`"%x"`, sha256.Sum256(countryTiles))

func (s *Server) mapTiles(w http.ResponseWriter, r *http.Request) {
	// PMTiles reads byte ranges. Do not apply whole-response compression here.
	// The ETag lets the reader detect an archive replacement between requests.
	w.Header().Set("Content-Type", "application/vnd.pmtiles")
	w.Header().Set("ETag", countryTilesETag)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, "countries.pmtiles", time.Time{}, bytes.NewReader(countryTiles))
}
