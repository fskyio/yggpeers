package server

import (
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"yggpeers/internal/config"
	"yggpeers/internal/fetcher"
	"yggpeers/internal/store"
)

//go:embed templates
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg   config.Config
	store *store.Store
	mux   *http.ServeMux
	tmpls *template.Template
}

func New(cfg config.Config, s *store.Store) (*Server, error) {
	funcs := template.FuncMap{
		"status": statusString,
		"uptime": uptimeString,
		"pct":    percentString,
		"optpct": optPercentString,
		"json":   jsonString,
	}
	tmpls, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	srv := &Server{cfg: cfg, store: s, mux: http.NewServeMux(), tmpls: tmpls}
	srv.routes()
	return srv, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /static/countries.pmtiles", s.mapTiles)
	s.mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
	s.mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, staticFS, "static/favicon.ico")
	})
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /stats", s.statsPage)
	s.mux.HandleFunc("GET /stats/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/stats", http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /map", s.mapPage)
	s.mux.HandleFunc("GET /map/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/map", http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /api", s.apiPage)
	s.mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api", http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /peers.json", apiHandler(s.peersJSON))
	s.mux.HandleFunc("OPTIONS /peers.json", apiHandler(s.peersJSON))
	s.mux.HandleFunc("GET /peers.csv", apiHandler(s.peersCSV))
	s.mux.HandleFunc("OPTIONS /peers.csv", apiHandler(s.peersCSV))
	s.mux.HandleFunc("GET /api/countries", apiHandler(s.countries))
	s.mux.HandleFunc("OPTIONS /api/countries", apiHandler(s.countries))
	s.mux.HandleFunc("GET /api/v1/peers", apiHandler(s.apiPeers))
	s.mux.HandleFunc("OPTIONS /api/v1/peers", apiHandler(s.apiPeers))
}

type countryGroup struct {
	Name  string
	Peers []store.PeerWithStats
}

type indexData struct {
	Countries    []countryGroup
	Networks     []countryGroup
	CheckDarknet bool
	UpdatedAt    time.Time
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	peers, err := s.store.GetPeersWithStats(store.StatsWindowDays)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("GetPeersWithStats: %v", err)
		return
	}

	var countries, networks []countryGroup
	countryIdx := map[string]int{}
	networkIdx := map[string]int{}

	for _, p := range peers {
		if p.IsNetwork {
			if _, ok := networkIdx[p.Country]; !ok {
				networkIdx[p.Country] = len(networks)
				networks = append(networks, countryGroup{Name: p.Country})
			}
			i := networkIdx[p.Country]
			networks[i].Peers = append(networks[i].Peers, p)
		} else {
			if _, ok := countryIdx[p.Country]; !ok {
				countryIdx[p.Country] = len(countries)
				countries = append(countries, countryGroup{Name: p.Country})
			}
			i := countryIdx[p.Country]
			countries[i].Peers = append(countries[i].Peers, p)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "index.html", indexData{
		Countries:    countries,
		Networks:     networks,
		CheckDarknet: s.cfg.CheckDarknet,
		UpdatedAt:    time.Now().UTC(),
	}); err != nil && !isBrokenPipe(err) {
		log.Printf("template index: %v", err)
	}
}

func (s *Server) statsPage(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.GetStats(store.StatsWindowDays)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("GetStats: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "stats.html", stats); err != nil && !isBrokenPipe(err) {
		log.Printf("template stats: %v", err)
	}
}

func (s *Server) mapPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "map.html", nil); err != nil && !isBrokenPipe(err) {
		log.Printf("template map: %v", err)
	}
}

func (s *Server) apiPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "api.html", nil); err != nil && !isBrokenPipe(err) {
		log.Printf("template api: %v", err)
	}
}

// peerRow is the JSON shape for a single peer. It's also used to derive the
// CSV columns so the two formats stay in sync.
type peerRow struct {
	URI            string     `json:"uri"`
	Country        string     `json:"country"`
	Protocol       string     `json:"protocol"`
	Host           string     `json:"host"`
	Port           int        `json:"port"`
	IsUp           bool       `json:"is_up"`
	IsNetwork      bool       `json:"is_network"`
	StateChangedAt *time.Time `json:"state_changed_at"`
	UptimePct      *float64   `json:"uptime_pct"`
}

type peersResponse struct {
	UpdatedAt  time.Time `json:"updated_at"`
	WindowDays int       `json:"window_days"`
	Total      int       `json:"total"`
	Online     int       `json:"online"`
	Peers      []peerRow `json:"peers"`
}

func toPeerRows(peers []store.PeerWithStats) []peerRow {
	out := make([]peerRow, len(peers))
	for i, p := range peers {
		port, _ := strconv.Atoi(p.Port)
		var stateChanged *time.Time
		if !p.StateChangedAt.IsZero() {
			t := p.StateChangedAt.UTC()
			stateChanged = &t
		}
		country := p.Country
		if code := fetcher.CountryToIso(country); code != "" {
			country = code
		}
		out[i] = peerRow{
			URI:            p.URI,
			Country:        country,
			Protocol:       p.Protocol,
			Host:           p.Host,
			Port:           port,
			IsUp:           p.IsUp,
			IsNetwork:      p.IsNetwork,
			StateChangedAt: stateChanged,
			UptimePct:      p.UptimePct,
		}
	}
	return out
}

func (s *Server) peersJSON(w http.ResponseWriter, r *http.Request) {
	peers, err := s.store.GetPeersWithStats(store.StatsWindowDays)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("GetPeersWithStats: %v", err)
		return
	}
	rows := toPeerRows(peers)
	online := 0
	for _, p := range rows {
		if p.IsUp {
			online++
		}
	}
	resp := peersResponse{
		UpdatedAt:  time.Now().UTC(),
		WindowDays: store.StatsWindowDays,
		Total:      len(rows),
		Online:     online,
		Peers:      rows,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil && !isBrokenPipe(err) {
		log.Printf("json peers: %v", err)
	}
}

func (s *Server) peersCSV(w http.ResponseWriter, r *http.Request) {
	peers, err := s.store.GetPeersWithStats(store.StatsWindowDays)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("GetPeersWithStats: %v", err)
		return
	}
	rows := toPeerRows(peers)

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="peers.csv"`)
	// Surface the metadata that the JSON response carries inline as headers
	// so CSV consumers don't have to scrape it from elsewhere.
	w.Header().Set("X-Updated-At", time.Now().UTC().Format(time.RFC3339))
	w.Header().Set("X-Window-Days", strconv.Itoa(store.StatsWindowDays))

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"uri", "country", "protocol", "host", "port",
		"is_up", "is_network", "state_changed_at", "uptime_pct",
	})
	for _, p := range rows {
		stateChanged := ""
		if p.StateChangedAt != nil {
			stateChanged = p.StateChangedAt.Format(time.RFC3339)
		}
		uptime := ""
		if p.UptimePct != nil {
			uptime = strconv.FormatFloat(*p.UptimePct, 'f', 2, 64)
		}
		_ = cw.Write([]string{
			p.URI, p.Country, p.Protocol, p.Host, strconv.Itoa(p.Port),
			strconv.FormatBool(p.IsUp),
			strconv.FormatBool(p.IsNetwork),
			stateChanged,
			uptime,
		})
	}
	cw.Flush()
}

func (s *Server) countries(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.GetCountryCounts()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("GetCountryCounts: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(counts); err != nil && !isBrokenPipe(err) {
		log.Printf("json countries: %v", err)
	}
}

// apiPeers handles GET /api/v1/peers.
//
// Supported query parameters:
//
//	country    – case-insensitive exact match on country name or ISO 3166-1 alpha-2 code (e.g., "us" or "US")
//	protocol   – exact match on protocol (tcp, tls, quic, ws, wss, socks, sockstls)
//	is_up      – "true" or "false"
//	is_network – "true" or "false"
func (s *Server) apiPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.store.GetPeersWithStats(store.StatsWindowDays)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("GetPeersWithStats: %v", err)
		return
	}

	q := r.URL.Query()
	countryParam := q.Get("country")
	var country string
	if countryParam != "" {
		country = strings.ToLower(countryParam)
		if converted := fetcher.IsoToCountryName(countryParam); converted != "" {
			country = strings.ToLower(converted)
		}
	}
	var protocol string
	if v := q.Get("protocol"); v != "" {
		protocol = strings.ToLower(v)
		validProtocols := map[string]bool{
			"tcp": true, "tls": true, "quic": true,
			"ws": true, "wss": true, "socks": true, "sockstls": true,
		}
		if !validProtocols[protocol] {
			jsonError(w, `invalid protocol: must be one of tcp, tls, quic, ws, wss, socks, sockstls`, http.StatusBadRequest)
			return
		}
	}

	var isUp, isNetwork *bool
	if v := q.Get("is_up"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			jsonError(w, `invalid is_up: use "true" or "false"`, http.StatusBadRequest)
			return
		}
		isUp = &b
	}
	if v := q.Get("is_network"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			jsonError(w, `invalid is_network: use "true" or "false"`, http.StatusBadRequest)
			return
		}
		isNetwork = &b
	}

	rows := toPeerRows(peers)
	filtered := rows[:0]
	for _, p := range rows {
		if country != "" && strings.ToLower(p.Country) != country {
			continue
		}
		if protocol != "" && p.Protocol != protocol {
			continue
		}
		if isUp != nil && p.IsUp != *isUp {
			continue
		}
		if isNetwork != nil && p.IsNetwork != *isNetwork {
			continue
		}
		filtered = append(filtered, p)
	}

	online := 0
	for _, p := range filtered {
		if p.IsUp {
			online++
		}
	}

	resp := peersResponse{
		UpdatedAt:  time.Now().UTC(),
		WindowDays: store.StatsWindowDays,
		Total:      len(filtered),
		Online:     online,
		Peers:      filtered,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil && !isBrokenPipe(err) {
		log.Printf("json api peers: %v", err)
	}
}

// apiHandler wraps a data endpoint with CORS headers and a short Cache-Control
// so browsers and CDNs can cache responses without hammering the database.
func apiHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Cache-Control", "public, max-age=30")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}

func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
