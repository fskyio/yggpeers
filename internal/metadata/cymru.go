package metadata

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"

	"yggpeers/internal/endpoint"
)

type asnResult struct {
	asns   []uint32
	prefix string
	name   string
}

func originQuery(ip netip.Addr) string {
	ip = ip.Unmap()
	if ip.Is4() {
		b := ip.As4()
		return fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com.", b[3], b[2], b[1], b[0])
	}
	b := ip.As16()
	const hex = "0123456789abcdef"
	var query strings.Builder
	query.Grow(64 + len("origin6.asn.cymru.com."))
	for i := len(b) - 1; i >= 0; i-- {
		query.WriteByte(hex[b[i]&15])
		query.WriteByte('.')
		query.WriteByte(hex[b[i]>>4])
		query.WriteByte('.')
	}
	query.WriteString("origin6.asn.cymru.com.")
	return query.String()
}

func (e *Enricher) lookupASN(ctx context.Context, ip netip.Addr, names *asnNames) (asnResult, error) {
	if !endpoint.PublicIP(ip) {
		return asnResult{}, fmt.Errorf("nonpublic ASN destination")
	}
	records, err := e.lookupTXT(ctx, originQuery(ip))
	if err != nil {
		return asnResult{}, err
	}
	result, err := parseOrigins(records, ip.Unmap())
	if err != nil {
		return asnResult{}, err
	}
	descriptions := make(map[string]struct{})
	for _, asn := range result.asns {
		name, err := names.get(ctx, e, asn)
		if err != nil {
			return asnResult{}, err
		}
		if name != "" {
			descriptions[name] = struct{}{}
		}
	}
	result.name = strings.Join(sortedKeys(descriptions), "; ")
	return result, nil
}

// Cymru origin TXT: ASN(s) | BGP prefix | CC | registry | allocated.
// Do not interpret registry country as the peer's physical location.
func parseOrigins(records []string, ip netip.Addr) (asnResult, error) {
	asns := make(map[uint32]struct{})
	prefixes := make(map[string]struct{})
	for _, record := range records {
		fields := strings.Split(record, "|")
		if len(fields) != 5 {
			return asnResult{}, fmt.Errorf("invalid Cymru origin TXT")
		}
		origins := strings.Fields(fields[0])
		if len(origins) == 1 && strings.EqualFold(origins[0], "NA") {
			continue
		}
		if len(origins) == 0 {
			return asnResult{}, fmt.Errorf("empty Cymru origin ASN")
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(fields[1]))
		if err != nil || !prefix.Contains(ip) {
			return asnResult{}, fmt.Errorf("invalid Cymru origin prefix")
		}
		for _, value := range origins {
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil || n == 0 {
				return asnResult{}, fmt.Errorf("invalid Cymru origin ASN %q", value)
			}
			asns[uint32(n)] = struct{}{}
		}
		prefixes[prefix.Masked().String()] = struct{}{}
	}
	result := asnResult{asns: make([]uint32, 0, len(asns)), prefix: strings.Join(sortedKeys(prefixes), "; ")}
	for asn := range asns {
		result.asns = append(result.asns, asn)
	}
	sort.Slice(result.asns, func(i, j int) bool { return result.asns[i] < result.asns[j] })
	return result, nil
}

func parseASNName(records []string, asn uint32) (string, error) {
	set := make(map[string]struct{})
	for _, record := range records {
		fields := strings.SplitN(record, "|", 5)
		if len(fields) != 5 {
			return "", fmt.Errorf("invalid Cymru ASN description TXT")
		}
		n, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 32)
		if err != nil || n != uint64(asn) {
			return "", fmt.Errorf("unexpected Cymru ASN description")
		}
		if name := strings.TrimSpace(fields[4]); name != "" {
			set[name] = struct{}{}
		}
	}
	return strings.Join(sortedKeys(set), "; "), nil
}

type asnName struct {
	ready chan struct{}
	value string
	err   error
}

// Names are deduplicated within a refresh, including in-flight requests from
// different addresses sharing an origin. Persisted IP snapshots cache them
// across refreshes; no separate long-lived in-memory name cache is necessary.
type asnNames struct {
	mu     sync.Mutex
	values map[uint32]*asnName
}

func (n *asnNames) get(ctx context.Context, e *Enricher, asn uint32) (string, error) {
	n.mu.Lock()
	if entry, ok := n.values[asn]; ok {
		n.mu.Unlock()
		select {
		case <-entry.ready:
			return entry.value, entry.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	entry := &asnName{ready: make(chan struct{})}
	n.values[asn] = entry
	n.mu.Unlock()
	records, err := e.lookupTXT(ctx, fmt.Sprintf("AS%d.asn.cymru.com.", asn))
	if err == nil {
		entry.value, err = parseASNName(records, asn)
	}
	entry.err = err
	close(entry.ready)
	return entry.value, entry.err
}
