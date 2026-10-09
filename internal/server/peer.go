package server

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"yggpeers/internal/endpoint"
	"yggpeers/internal/protocol"
	"yggpeers/internal/store"
	"yggpeers/internal/ygg"
)

type peerStatus struct {
	Label string
	Class string
}

type historyCell struct {
	Unknown bool
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

// peerNode presents the identity from the endpoint's latest handshake.
type peerNode struct {
	Address         string
	NodeInfoEnabled bool
	Software        string
	SoftwareNote    string
}

func nodeSoftware(m store.NodeMetadata, available bool) (software, note string) {
	switch {
	case !available:
		return "Awaiting NodeInfo lookup", ""
	case m.FetchedAt.IsZero():
		software = "Unavailable"
	case m.BuildName == "" && m.BuildVersion == "":
		software = "Not published (NodeInfo privacy)"
	default:
		software = strings.TrimSpace(m.BuildName + " " + m.BuildVersion)
		if m.BuildPlatform != "" || m.BuildArch != "" {
			software += " (" + m.BuildPlatform + "/" + m.BuildArch + ")"
		}
	}
	if m.Error != "" {
		return software, "Last NodeInfo lookup failed: " + m.Error
	}
	return software, "NodeInfo fetched " + m.FetchedAt.Format("2006-01-02 15:04 UTC")
}

type peerPageData struct {
	Detail              store.PeerDetail
	Destination         endpoint.Destination
	DestinationError    bool
	Status              peerStatus
	Coverage            store.ProbeCoverage
	Hours               []historyCell
	Resolution          store.HostMetadata
	ResolutionAvailable bool
	ResolutionFresh     bool
	NetworkApplicable   bool
	Addresses           []peerAddress
	Related             []relatedPeer
	Node                peerNode
	UpdatedAt           time.Time
}

func observedStatus(p store.Peer, now time.Time) peerStatus {
	if p.StatusValidUntil == nil || !now.Before(*p.StatusValidUntil) {
		if p.LastCheckedAt != nil {
			return peerStatus{"Unknown — measurement expired", "unknown"}
		}
		return peerStatus{"Unknown — no fresh measurement", "unknown"}
	}
	switch p.Status {
	case protocol.Up:
		return peerStatus{"Online", "up"}
	case protocol.Down:
		return peerStatus{"Offline", "down"}
	default:
		return peerStatus{"Unknown — insufficient probe reports", "unknown"}
	}
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
		if hour.UnknownCount > 0 {
			if hour.Checks == 0 {
				description = "Unknown measurements"
			} else {
				description += "; incomplete coverage"
			}
		}
		label := fmt.Sprintf("%s – %s UTC: %s; %d/%d known rounds succeeded; %d unknown rounds",
			hour.Start.Format("2006-01-02 15:04:05"), hour.End.Format("2006-01-02 15:04:05"),
			description, hour.UpCount, hour.Checks, hour.UnknownCount)
		cells[i] = historyCell{HistoryHour: hour, Class: class, Label: label, Unknown: hour.UnknownCount > 0}
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

// matchingPeers relates endpoints that verifiably reach the same node, or share
// a destination. A shared key spans overlay and public endpoints.
func matchingPeers(selected store.Peer, destination endpoint.Destination, peers []store.PeerWithStats, hosts map[string]store.HostMetadata, now time.Time) []relatedPeer {
	var addresses []string
	if !selected.IsNetwork {
		addresses = destinationAddresses(destination, hosts, now)
	}
	var related []relatedPeer
	for _, p := range peers {
		if p.ID == selected.ID {
			continue
		}
		d, err := endpoint.Parse(p.URI)
		if err != nil {
			continue
		}
		reason := ""
		switch {
		case selected.NodeKey != "" && p.NodeKey == selected.NodeKey:
			reason = "Same Yggdrasil node (public key)"
		case p.IsNetwork != selected.IsNetwork:
		case d.Host == destination.Host:
			reason = "Same destination hostname/IP"
		case !p.IsNetwork && len(addresses) > 0:
			if ip := sharedAddress(addresses, destinationAddresses(d, hosts, now)); ip != "" {
				reason = "Shared IP: " + ip
			}
		}
		if reason != "" {
			related = append(related, relatedPeer{Peer: p, Destination: d, Reason: reason})
		}
	}
	sort.Slice(related, func(i, j int) bool { return related[i].Peer.URI < related[j].Peer.URI })
	return related
}

func sharedAddress(addresses, candidates []string) string {
	for _, ip := range addresses {
		if slices.Contains(candidates, ip) {
			return ip
		}
	}
	return ""
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
	coverage, err := s.store.GetPeerProbeCoverage(id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("GetPeerProbeCoverage: %v", err)
		return
	}
	data := peerPageData{
		Detail: detail, Status: observedStatus(detail.Peer, now), Coverage: coverage,
		Hours: historyCells(detail.Hours), UpdatedAt: now,
	}
	if key, err := hex.DecodeString(detail.NodeKey); err == nil && len(key) > 0 {
		data.Node.Address = ygg.AddressForKey(key).String()
		if data.Node.NodeInfoEnabled = s.cfg.YggdrasilAdmin != ""; data.Node.NodeInfoEnabled {
			m, err := s.store.GetNodeMetadata(detail.NodeKey)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				log.Printf("GetNodeMetadata: %v", err)
				return
			}
			data.Node.Software, data.Node.SoftwareNote = nodeSoftware(m, err == nil)
		}
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
			data.Related[i].Status = observedStatus(data.Related[i].Peer.Peer, now)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "peer.html", data); err != nil && !isBrokenPipe(err) {
		log.Printf("template peer: %v", err)
	}
}
