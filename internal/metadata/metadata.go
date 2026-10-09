// Package metadata refreshes persisted destination and node snapshots
// independently of archive updates. HTTP handlers only read those snapshots.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"yggpeers/internal/endpoint"
	"yggpeers/internal/store"
	"yggpeers/internal/ygg"
)

const (
	hostFreshness     = 15 * time.Minute
	asnFreshness      = 24 * time.Hour
	negativeFreshness = time.Minute
	retryDelay        = time.Minute
	lookupTimeout     = 5 * time.Second
	maxLookups        = 8
	nodeFreshness     = 24 * time.Hour
	// Unanswered NodeInfo requests each hold an admin socket call for seconds.
	nodeRetryDelay    = 15 * time.Minute
	nodeLookupTimeout = 10 * time.Second
)

// net.Resolver does not expose DNS TTLs. These are application freshness
// windows, not claims about authoritative DNS cache lifetimes.
type dnsResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
	LookupTXT(context.Context, string) ([]string, error)
}

// nodeInfoSource fetches NodeInfo through a local Yggdrasil node.
type nodeInfoSource interface {
	NodeInfo(ctx context.Context, key string) (ygg.NodeInfo, error)
}

type Enricher struct {
	store     *store.Store
	resolver  dnsResolver
	nodes     nodeInfoSource // nil disables NodeInfo lookups
	now       func() time.Time
	refreshMu sync.Mutex
}

// New refreshes DNS and ASN snapshots, plus NodeInfo when admin is non-nil.
func New(s *store.Store, admin *ygg.Admin) *Enricher {
	e := &Enricher{store: s, resolver: &net.Resolver{PreferGo: true}, now: time.Now}
	if admin != nil {
		e.nodes = admin
	}
	return e
}

func (e *Enricher) Refresh(ctx context.Context) error {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	peers, err := e.store.GetAllPeers()
	if err != nil {
		return err
	}
	hosts, err := e.store.GetHostMetadataAll()
	if err != nil {
		return err
	}
	ips, err := e.store.GetIPMetadataAll()
	if err != nil {
		return err
	}
	hostSet := make(map[string]struct{})
	ipSet := make(map[string]struct{})
	for _, p := range peers {
		if p.IsNetwork {
			continue
		}
		d, err := endpoint.Parse(p.URI)
		if err != nil || !endpoint.PublicHost(d.Host) {
			continue
		}
		if ip, err := netip.ParseAddr(d.Host); err == nil {
			if endpoint.PublicIP(ip) {
				ipSet[ip.Unmap().String()] = struct{}{}
			}
		} else {
			hostSet[d.Host] = struct{}{}
		}
	}
	now := e.now().UTC()
	var errorsMu sync.Mutex
	var refreshErrors []error
	recordError := func(err error) {
		if err != nil {
			errorsMu.Lock()
			refreshErrors = append(refreshErrors, err)
			errorsMu.Unlock()
		}
	}
	var hostMu sync.Mutex
	e.parallel(ctx, sortedKeys(hostSet), func(host string) {
		hostMu.Lock()
		m := hosts[host]
		hostMu.Unlock()
		if !refreshDue(now, m.ResolvedAt, m.ExpiresAt, m.NextAttemptAt, m.Error) {
			return
		}
		m.Host = host
		addresses, err := e.resolveHost(ctx, host)
		if ctx.Err() != nil {
			return
		}
		at := e.now().UTC()
		if err != nil {
			m.Error = err.Error()
			m.NextAttemptAt = at.Add(retryDelay)
			if m.ExpiresAt.After(at) {
				m.ExpiresAt = at
			}
			recordError(fmt.Errorf("resolve %s: %w", host, err))
		} else {
			m.Addresses = addresses
			m.ResolvedAt = at
			m.ExpiresAt = at.Add(hostFreshness)
			if len(addresses) == 0 {
				m.ExpiresAt = at.Add(negativeFreshness)
			}
			m.NextAttemptAt = m.ExpiresAt
			m.Error = ""
		}
		if err := e.store.SaveHostMetadata(m); err != nil {
			recordError(fmt.Errorf("save host %s: %w", host, err))
			return
		}
		hostMu.Lock()
		hosts[host] = m
		hostMu.Unlock()
	})
	if err := ctx.Err(); err != nil {
		return errors.Join(append(refreshErrors, err)...)
	}
	// Never use old addresses after expiry or an unsuccessful refresh. A
	// successful empty result replaces, rather than merges, the old snapshot.
	now = e.now().UTC()
	for host := range hostSet {
		m := hosts[host]
		if m.ResolvedAt.IsZero() || m.Error != "" || !now.Before(m.ExpiresAt) {
			continue
		}
		for _, address := range m.Addresses {
			if ip, err := netip.ParseAddr(address); err == nil && endpoint.PublicIP(ip) {
				ipSet[ip.Unmap().String()] = struct{}{}
			}
		}
	}
	names := &asnNames{values: make(map[uint32]*asnName)}
	e.parallel(ctx, sortedKeys(ipSet), func(address string) {
		m := ips[address]
		if !refreshDue(now, m.LookedUpAt, m.ExpiresAt, m.NextAttemptAt, m.Error) {
			return
		}
		m.IP = address
		result, err := e.lookupASN(ctx, netip.MustParseAddr(address), names)
		if ctx.Err() != nil {
			return
		}
		at := e.now().UTC()
		if err != nil {
			m.Error = err.Error()
			m.NextAttemptAt = at.Add(retryDelay)
			if m.ExpiresAt.After(at) {
				m.ExpiresAt = at
			}
			recordError(fmt.Errorf("ASN %s: %w", address, err))
		} else {
			m.ASNs, m.Prefix, m.Name = result.asns, result.prefix, result.name
			m.LookedUpAt = at
			m.ExpiresAt = at.Add(asnFreshness)
			if len(m.ASNs) == 0 {
				m.ExpiresAt = at.Add(negativeFreshness)
			}
			m.NextAttemptAt = m.ExpiresAt
			m.Error = ""
		}
		recordError(e.store.SaveIPMetadata(m))
	})
	if e.nodes != nil {
		recordError(e.refreshNodes(ctx, peers))
	}
	if ctx.Err() != nil {
		refreshErrors = append(refreshErrors, ctx.Err())
	}
	return errors.Join(refreshErrors...)
}

// refreshNodes keeps NodeInfo for every key a probe has seen on a listed peer.
func (e *Enricher) refreshNodes(ctx context.Context, peers []store.Peer) error {
	nodes, err := e.store.GetNodeMetadataAll()
	if err != nil {
		return err
	}
	keys := make(map[string]struct{})
	for _, p := range peers {
		if p.NodeKey != "" {
			keys[p.NodeKey] = struct{}{}
		}
	}
	now := e.now().UTC()
	var errorsMu sync.Mutex
	var refreshErrors []error
	recordError := func(err error) {
		if err != nil {
			errorsMu.Lock()
			refreshErrors = append(refreshErrors, err)
			errorsMu.Unlock()
		}
	}
	e.parallel(ctx, sortedKeys(keys), func(key string) {
		m := nodes[key]
		if !refreshDue(now, m.FetchedAt, m.ExpiresAt, m.NextAttemptAt, m.Error) {
			return
		}
		m.Key = key
		lookupCtx, cancel := context.WithTimeout(ctx, nodeLookupTimeout)
		info, err := e.nodes.NodeInfo(lookupCtx, key)
		cancel()
		if ctx.Err() != nil {
			return
		}
		at := e.now().UTC()
		if err != nil {
			m.Error = err.Error()
			m.NextAttemptAt = at.Add(nodeRetryDelay)
			if m.ExpiresAt.After(at) {
				m.ExpiresAt = at
			}
			recordError(fmt.Errorf("NodeInfo %s: %w", key, err))
		} else {
			m.BuildName, m.BuildVersion, m.BuildPlatform, m.BuildArch = info.BuildName, info.BuildVersion, info.BuildPlatform, info.BuildArch
			m.FetchedAt = at
			m.ExpiresAt = at.Add(nodeFreshness)
			m.NextAttemptAt = m.ExpiresAt
			m.Error = ""
		}
		recordError(e.store.SaveNodeMetadata(m))
	})
	return errors.Join(refreshErrors...)
}

func refreshDue(now, success, expires, next time.Time, lastError string) bool {
	if now.Before(next) {
		return false
	}
	return success.IsZero() || lastError != "" || !now.Before(expires)
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Each worker performs at most one DNS operation at a time, including ASN
// descriptions. The two phases share the same concurrency ceiling.
func (e *Enricher) parallel(ctx context.Context, keys []string, fn func(string)) {
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range min(maxLookups, len(keys)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				if ctx.Err() == nil {
					fn(key)
				}
			}
		}()
	}
	for _, key := range keys {
		select {
		case jobs <- key:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func (e *Enricher) resolveHost(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addresses, err := e.resolver.LookupNetIP(ctx, "ip", host+".")
	if notFound(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(addresses))
	for _, ip := range addresses {
		if endpoint.PublicIP(ip) {
			set[ip.Unmap().String()] = struct{}{}
		}
	}
	return sortedKeys(set), nil
}

func (e *Enricher) lookupTXT(ctx context.Context, query string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	records, err := e.resolver.LookupTXT(ctx, query)
	if notFound(err) {
		return nil, nil
	}
	return records, err
}

func notFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound && !dnsErr.IsTimeout && !dnsErr.IsTemporary
}
