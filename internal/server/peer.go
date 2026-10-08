package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"time"

	"yggpeers/internal/endpoint"
	"yggpeers/internal/store"
)

type peerStatus struct {
	Label string
	Class string
}

type historyCell struct {
	store.HistoryHour
	Class string
	Label string
}

type peerAddress struct {
	IP        string
	Metadata  store.IPMetadata
	Available bool
	Fresh     bool
}

type relatedPeer struct {
	Peer        store.PeerWithStats
	Destination endpoint.Destination
	Reason      string
	Status      peerStatus
}

type peerPageData struct {
	Detail              store.PeerDetail
	Destination         endpoint.Destination
	DestinationError    bool
	Status              peerStatus
	Hours               []historyCell
	Resolution          store.HostMetadata
	ResolutionAvailable bool
	ResolutionFresh     bool
	NetworkApplicable   bool
	Addresses           []peerAddress
	Related             []relatedPeer
	UpdatedAt           time.Time
}

func observedStatus(p store.Peer, checkInterval time.Duration, checkDarknet bool, now time.Time) peerStatus {
	if p.IsNetwork && !checkDarknet {
		return peerStatus{"Not monitored", "unknown"}
	}
	if p.LastCheckedAt == nil {
		return peerStatus{"Unobserved", "unknown"}
	}
	state, class := "Offline", "down"
	if p.IsUp {
		state, class = "Online", "up"
	}
	if checkInterval <= 0 {
		checkInterval = 5 * time.Minute
	}
	if now.Sub(*p.LastCheckedAt) > 3*checkInterval {
		return peerStatus{"Stale — last observed " + state, "unknown"}
	}
	return peerStatus{state, class}
}

func historyCells(hours []store.HistoryHour) []historyCell {
	cells := make([]historyCell, len(hours))
	for i, hour := range hours {
		class, description := "unknown", "No observations"
		if hour.Checks > 0 {
			switch {
			case hour.UpCount == hour.Checks:
				class, description = "up", "All recorded checks passed"
			case hour.UpCount == 0:
				class, description = "down", "All recorded checks failed"
			default:
				class, description = "mixed", "Mixed results"
			}
		}
		label := fmt.Sprintf("%s – %s UTC: %s; %d/%d checks passed",
			hour.Start.Format("2006-01-02 15:04:05"), hour.End.Format("2006-01-02 15:04:05"),
			description, hour.UpCount, hour.Checks)
		cells[i] = historyCell{HistoryHour: hour, Class: class, Label: label}
	}
	return cells
}

func freshResolution(m store.HostMetadata, now time.Time) bool {
	return m.Error == "" && !m.ResolvedAt.IsZero() && now.Before(m.ExpiresAt)
}

func destinationAddresses(d endpoint.Destination, hosts map[string]store.HostMetadata, now time.Time) []string {
	if endpoint.IsOverlayHost(d.Host) {
		return nil
	}
	if ip, err := netip.ParseAddr(d.Host); err == nil {
		if endpoint.PublicIP(ip) {
			return []string{ip.Unmap().String()}
		}
		return nil
	}
	m, ok := hosts[d.Host]
	if !ok || !freshResolution(m, now) {
		return nil
	}
	return m.Addresses
}

func matchingPeers(selected store.Peer, destination endpoint.Destination, peers []store.PeerWithStats, hosts map[string]store.HostMetadata, now time.Time) []relatedPeer {
	var addresses []string
	if !selected.IsNetwork {
		addresses = destinationAddresses(destination, hosts, now)
	}
	var related []relatedPeer
	for _, p := range peers {
		if p.ID == selected.ID || p.IsNetwork != selected.IsNetwork {
			continue
		}
		d, err := endpoint.Parse(p.URI)
		if err != nil {
			continue
		}
		reason := ""
		if d.Host == destination.Host {
			reason = "Same destination hostname/IP"
		} else if !p.IsNetwork && len(addresses) > 0 {
			candidate := destinationAddresses(d, hosts, now)
			for _, ip := range addresses {
				for _, other := range candidate {
					if ip == other {
						reason = "Shared IP: " + ip
						break
					}
				}
				if reason != "" {
					break
				}
			}
		}
		if reason != "" {
			related = append(related, relatedPeer{Peer: p, Destination: d, Reason: reason})
		}
	}
	sort.Slice(related, func(i, j int) bool { return related[i].Peer.URI < related[j].Peer.URI })
	return related
}

func (s *Server) peerPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	now := time.Now().UTC()
	detail, err := s.store.GetPeerDetail(id, now)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("GetPeerDetail: %v", err)
		return
	}
	data := peerPageData{
		Detail: detail, Status: observedStatus(detail.Peer, s.cfg.CheckInterval, s.cfg.CheckDarknet, now),
		Hours: historyCells(detail.Hours), UpdatedAt: now,
	}
	destination, err := endpoint.Parse(detail.URI)
	data.Destination, data.DestinationError = destination, err != nil
	if err == nil {
		hosts, err := s.store.GetHostMetadataAll()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			log.Printf("GetHostMetadataAll: %v", err)
			return
		}
		data.NetworkApplicable = !detail.IsNetwork && endpoint.PublicHost(destination.Host)
		if ip, err := netip.ParseAddr(destination.Host); err == nil {
			if data.NetworkApplicable {
				data.Addresses = []peerAddress{{IP: ip.Unmap().String()}}
			}
		} else if data.NetworkApplicable {
			data.Resolution, data.ResolutionAvailable = hosts[destination.Host]
			data.ResolutionFresh = data.ResolutionAvailable && freshResolution(data.Resolution, now)
			for _, ip := range data.Resolution.Addresses {
				data.Addresses = append(data.Addresses, peerAddress{IP: ip})
			}
		}
		for i := range data.Addresses {
			m, err := s.store.GetIPMetadata(data.Addresses[i].IP)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				log.Printf("GetIPMetadata: %v", err)
				return
			}
			data.Addresses[i].Metadata = m
			data.Addresses[i].Available = err == nil
			data.Addresses[i].Fresh = err == nil && m.Error == "" && !m.LookedUpAt.IsZero() && now.Before(m.ExpiresAt)
		}
		peers, err := s.store.GetPeersWithStats(store.StatsWindowDays)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			log.Printf("GetPeersWithStats: %v", err)
			return
		}
		data.Related = matchingPeers(detail.Peer, destination, peers, hosts, now)
		for i := range data.Related {
			data.Related[i].Status = observedStatus(data.Related[i].Peer.Peer, s.cfg.CheckInterval, s.cfg.CheckDarknet, now)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "peer.html", data); err != nil && !isBrokenPipe(err) {
		log.Printf("template peer: %v", err)
	}
}
