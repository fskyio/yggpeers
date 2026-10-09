package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"yggpeers/internal/protocol"
	"yggpeers/internal/store"
	"yggpeers/internal/ygg"
)

type fakeResolver struct {
	mu         sync.Mutex
	addresses  map[string][]netip.Addr
	hostErrors map[string]error
	txt        map[string][]string
	txtErrors  map[string]error
	calls      map[string]int
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{addresses: make(map[string][]netip.Addr), hostErrors: make(map[string]error), txt: make(map[string][]string), txtErrors: make(map[string]error), calls: make(map[string]int)}
}

func (r *fakeResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[host]++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing lookup deadline")
	}
	if network != "ip" {
		return nil, errors.New("missing dual-stack lookup")
	}
	return r.addresses[host], r.hostErrors[host]
}

func (r *fakeResolver) LookupTXT(ctx context.Context, query string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[query]++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing lookup deadline")
	}
	return r.txt[query], r.txtErrors[query]
}

func metadataFixture(t *testing.T, peers []store.PeerInput) (*Enricher, *fakeResolver, *time.Time) {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.BulkUpsertPeers(peers); err != nil {
		t.Fatal(err)
	}
	r := newFakeResolver()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	e := New(s, nil)
	e.resolver, e.now = r, func() time.Time { return now }
	return e, r, &now
}

func peer(uri string) store.PeerInput {
	return store.PeerInput{URI: uri, Country: "Unknown", Protocol: "tcp"}
}

func TestRefreshDeduplicatesDestinationsAndExcludesSensitiveTargets(t *testing.T) {
	peers := []store.PeerInput{
		peer("tcp://DNS.UnitChecks.Net.:123"), peer("tls://dns.unitchecks.net:456"),
		peer("socks://127.0.0.1:9050/dns.unitchecks.net:789"), peer("tcp://8.8.8.8:123"),
		peer("tcp://[::ffff:8.8.8.8]:456"), peer("tcp://[2606:4700:4700::1111]:123"),
		peer("socks://127.0.0.1:9050/secret.onion:123"), peer("sockstls://localhost:4447/secret.i2p:123"),
		peer("tcp://secret.loki:123"), peer("tcp://10.0.0.1:123"), peer("tcp://192.0.2.1:123"),
		peer("tcp://[fc00::1]:123"), peer("tcp://localhost:123"), peer("socks://localhost:9050/broken/path"),
		peer("tcp://host.invalid:123"), peer("tcp://host.local:123"), peer("tcp://host.example:123"),
		peer("tcp://host.home.arpa:123"), peer("tcp://router:123"),
	}
	network := peer("tls://flagged.unitchecks.net:123")
	network.IsNetwork = true
	peers = append(peers, network)
	e, r, now := metadataFixture(t, peers)
	r.addresses["dns.unitchecks.net."] = []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("::ffff:8.8.8.8"), netip.MustParseAddr("8.8.4.4"), netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("2606:4700:4700:0:0:0:0:1111")}
	for _, ip := range []string{"8.8.8.8", "8.8.4.4"} {
		r.txt[originQuery(netip.MustParseAddr(ip))] = []string{"15169 64500 | 8.8.0.0/16 | US | arin | 2023-12-28"}
	}
	r.txt[originQuery(netip.MustParseAddr("2606:4700:4700::1111"))] = []string{"13335 | 2606:4700::/32 | US | arin | 2011-11-01"}
	r.txt["AS15169.asn.cymru.com."] = []string{"15169 | US | arin | 2000-03-30 | GOOGLE - Google LLC, US"}
	r.txt["AS64500.asn.cymru.com."] = []string{"64500 | US | arin | 2000-03-30 | OTHER"}
	r.txt["AS13335.asn.cymru.com."] = []string{"13335 | US | arin | 2010-07-14 | CLOUDFLARENET - Cloudflare, Inc., US"}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantCalls := map[string]int{"dns.unitchecks.net.": 1, "AS15169.asn.cymru.com.": 1, "AS64500.asn.cymru.com.": 1, "AS13335.asn.cymru.com.": 1}
	for _, ip := range []string{"8.8.8.8", "8.8.4.4", "2606:4700:4700::1111"} {
		wantCalls[originQuery(netip.MustParseAddr(ip))] = 1
	}
	if !reflect.DeepEqual(r.calls, wantCalls) {
		t.Fatalf("unexpected DNS targets/counts: %v; want %v", r.calls, wantCalls)
	}
	m, err := e.store.GetIPMetadata("8.8.8.8")
	if err != nil || !reflect.DeepEqual(m.ASNs, []uint32{15169, 64500}) || m.Name != "GOOGLE - Google LLC, US; OTHER" || !m.LookedUpAt.Equal(*now) {
		t.Fatalf("incomplete multi-origin snapshot: %+v, %v", m, err)
	}
	host, err := e.store.GetHostMetadata("dns.unitchecks.net")
	if err != nil || !reflect.DeepEqual(host.Addresses, []string{"2606:4700:4700::1111", "8.8.4.4", "8.8.8.8"}) {
		t.Fatalf("host snapshot contains nonpublic/duplicate/noncanonical addresses: %+v, %v", host, err)
	}
	if _, err := e.store.GetIPMetadata("10.0.0.1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("private ASN metadata exists: %v", err)
	}
	// Fresh persisted snapshots suppress all network calls even after replacing
	// the Enricher (the cache is not tied to upstream archive modifications).
	restarted := New(e.store, nil)
	restarted.resolver, restarted.now = r, e.now
	if err := restarted.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, wantCalls) {
		t.Fatalf("fresh cache performed network calls: %v", r.calls)
	}
}

func TestRefreshFailureNegativeAndAddressReplacement(t *testing.T) {
	e, r, now := metadataFixture(t, []store.PeerInput{peer("tcp://peer.unitchecks.net:123")})
	r.addresses["peer.unitchecks.net."] = []netip.Addr{netip.MustParseAddr("8.8.8.8")}
	r.txt[originQuery(netip.MustParseAddr("8.8.8.8"))] = []string{"15169 | 8.8.8.0/24 | US | arin | 2023-12-28"}
	r.txt["AS15169.asn.cymru.com."] = []string{"15169 | US | arin | 2000-03-30 | GOOGLE"}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := *now
	oldIP, err := e.store.GetIPMetadata("8.8.8.8")
	if err != nil {
		t.Fatal(err)
	}
	oldIP.ExpiresAt, oldIP.NextAttemptAt = now.Add(hostFreshness), now.Add(hostFreshness)
	if err := e.store.SaveIPMetadata(oldIP); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(hostFreshness)
	r.hostErrors["peer.unitchecks.net."] = &net.DNSError{Err: "timeout", IsTimeout: true, IsTemporary: true}
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("transient DNS failure disappeared")
	}
	failed, err := e.store.GetHostMetadata("peer.unitchecks.net")
	if err != nil || failed.Error == "" || !failed.ResolvedAt.Equal(first) || !reflect.DeepEqual(failed.Addresses, []string{"8.8.8.8"}) || now.Before(failed.ExpiresAt) || !failed.NextAttemptAt.Equal(now.Add(retryDelay)) {
		t.Fatalf("failed refresh corrupted/still trusted snapshot: %+v, %v", failed, err)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.calls["peer.unitchecks.net."] != 2 {
		t.Fatal("failure retried before backoff")
	}
	if r.calls[originQuery(netip.MustParseAddr("8.8.8.8"))] != 1 {
		t.Fatal("failed/expired DNS addresses were used to refresh ASN evidence")
	}
	// The archive/peer list is unchanged. Due refresh still discovers a new
	// address and replaces the old relationship evidence.
	*now = now.Add(retryDelay)
	delete(r.hostErrors, "peer.unitchecks.net.")
	r.addresses["peer.unitchecks.net."] = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	replaced, _ := e.store.GetHostMetadata("peer.unitchecks.net")
	if !reflect.DeepEqual(replaced.Addresses, []string{"1.1.1.1"}) || replaced.Error != "" || !replaced.ResolvedAt.Equal(*now) {
		t.Fatalf("address replacement failed: %+v", replaced)
	}
	negativeIP, _ := e.store.GetIPMetadata("1.1.1.1")
	if negativeIP.LookedUpAt.IsZero() || len(negativeIP.ASNs) != 0 || !negativeIP.ExpiresAt.Equal(now.Add(negativeFreshness)) {
		t.Fatalf("empty TXT is not a bounded successful negative: %+v", negativeIP)
	}
	*now = now.Add(hostFreshness)
	r.hostErrors["peer.unitchecks.net."] = &net.DNSError{Err: "no such host", IsNotFound: true}
	delete(r.addresses, "peer.unitchecks.net.")
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	negative, _ := e.store.GetHostMetadata("peer.unitchecks.net")
	if negative.ResolvedAt.IsZero() || negative.Error != "" || len(negative.Addresses) != 0 || !negative.ExpiresAt.Equal(now.Add(negativeFreshness)) {
		t.Fatalf("NXDOMAIN retained stale addresses: %+v", negative)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.calls["peer.unitchecks.net."] != 4 {
		t.Fatal("negative DNS retried before expiry")
	}
	*now = now.Add(negativeFreshness)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.calls["peer.unitchecks.net."] != 5 {
		t.Fatal("negative DNS not refreshed at boundary")
	}
}

func TestASNFailuresKeepSuccessStaleAndNegativesRetry(t *testing.T) {
	e, r, now := metadataFixture(t, []store.PeerInput{peer("tcp://8.8.8.8:123")})
	query := originQuery(netip.MustParseAddr("8.8.8.8"))
	r.txt[query] = []string{"15169 | 8.8.8.0/24 | US | arin | 2023-12-28"}
	r.txt["AS15169.asn.cymru.com."] = []string{"15169 | US | arin | 2000-03-30 | GOOGLE"}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := *now
	*now = now.Add(asnFreshness)
	r.txtErrors[query] = &net.DNSError{Err: "SERVFAIL", IsTemporary: true}
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("transient ASN error disappeared")
	}
	stale, _ := e.store.GetIPMetadata("8.8.8.8")
	if stale.Error == "" || !stale.LookedUpAt.Equal(first) || stale.Name != "GOOGLE" || !reflect.DeepEqual(stale.ASNs, []uint32{15169}) || now.Before(stale.ExpiresAt) {
		t.Fatalf("ASN failure lost/still trusted success: %+v", stale)
	}
	if err := e.Refresh(context.Background()); err != nil || r.calls[query] != 2 {
		t.Fatalf("ASN failure ignored backoff: calls=%v, err=%v", r.calls, err)
	}
	*now = now.Add(retryDelay)
	delete(r.txtErrors, query)
	r.txtErrors["AS15169.asn.cymru.com."] = &net.DNSError{Err: "description timeout", IsTimeout: true}
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("description failure was treated as a complete success")
	}
	stale, _ = e.store.GetIPMetadata("8.8.8.8")
	if stale.Error == "" || !stale.LookedUpAt.Equal(first) || stale.Name != "GOOGLE" {
		t.Fatalf("description failure erased old success: %+v", stale)
	}
	*now = now.Add(retryDelay)
	r.txtErrors[query] = &net.DNSError{Err: "no such host", IsNotFound: true}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	negative, _ := e.store.GetIPMetadata("8.8.8.8")
	if negative.Error != "" || !negative.LookedUpAt.Equal(*now) || len(negative.ASNs) != 0 || negative.Name != "" || negative.Prefix != "" || !negative.ExpiresAt.Equal(now.Add(negativeFreshness)) {
		t.Fatalf("ASN NXDOMAIN retained stale result: %+v", negative)
	}
	*now = now.Add(negativeFreshness)
	if err := e.Refresh(context.Background()); err != nil || r.calls[query] != 5 {
		t.Fatalf("negative ASN did not refresh at expiry: calls=%v, err=%v", r.calls, err)
	}
}

func TestInitialFailuresRemainUnknown(t *testing.T) {
	e, r, now := metadataFixture(t, []store.PeerInput{peer("tcp://unknown.unitchecks.net:123"), peer("tcp://8.8.8.8:123")})
	r.hostErrors["unknown.unitchecks.net."] = &net.DNSError{Err: "temporary", IsTemporary: true}
	r.txtErrors[originQuery(netip.MustParseAddr("8.8.8.8"))] = errors.New("resolver failure")
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("initial lookup failures disappeared")
	}
	host, _ := e.store.GetHostMetadata("unknown.unitchecks.net")
	ip, _ := e.store.GetIPMetadata("8.8.8.8")
	if !host.ResolvedAt.IsZero() || !ip.LookedUpAt.IsZero() || host.Error == "" || ip.Error == "" || !host.NextAttemptAt.Equal(now.Add(retryDelay)) {
		t.Fatalf("failure was presented as empty success: host=%+v ip=%+v", host, ip)
	}
}

func TestPrivateOnlyDNSAnswersAreBoundedNegativeSnapshots(t *testing.T) {
	e, r, now := metadataFixture(t, []store.PeerInput{peer("tcp://private.unitchecks.net:123")})
	r.addresses["private.unitchecks.net."] = []netip.Addr{
		netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("::ffff:192.168.1.1"),
		netip.MustParseAddr("fc00::1"), netip.MustParseAddr("2001:db8::1"),
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	host, err := e.store.GetHostMetadata("private.unitchecks.net")
	if err != nil || len(host.Addresses) != 0 || !host.ResolvedAt.Equal(*now) || !host.ExpiresAt.Equal(now.Add(negativeFreshness)) || host.Error != "" {
		t.Fatalf("nonpublic addresses became relationship evidence: %+v, %v", host, err)
	}
	if !reflect.DeepEqual(r.calls, map[string]int{"private.unitchecks.net.": 1}) {
		t.Fatalf("nonpublic addresses leaked to Cymru: %v", r.calls)
	}
}

type blockingResolver struct {
	started      chan struct{}
	mu           sync.Mutex
	active, peak int
}

func (r *blockingResolver) LookupNetIP(ctx context.Context, _, _ string) ([]netip.Addr, error) {
	r.mu.Lock()
	r.active++
	if r.active > r.peak {
		r.peak = r.active
	}
	r.mu.Unlock()
	r.started <- struct{}{}
	<-ctx.Done()
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return nil, ctx.Err()
}

func (r *blockingResolver) LookupTXT(context.Context, string) ([]string, error) {
	return nil, errors.New("unexpected TXT request")
}

func TestRefreshBoundsConcurrencyAndCancelsWithoutPoisoningSnapshots(t *testing.T) {
	peers := make([]store.PeerInput, 20)
	for i := range peers {
		peers[i] = peer(fmt.Sprintf("tcp://peer%d.unitchecks.net:123", i))
	}
	e, _, _ := metadataFixture(t, peers)
	r := &blockingResolver{started: make(chan struct{}, 20)}
	e.resolver = r
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Refresh(ctx) }()
	for range maxLookups {
		select {
		case <-r.started:
		case <-time.After(3 * time.Second):
			t.Fatal("bounded worker did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not cancel")
	}
	if r.peak != maxLookups || r.active != 0 {
		t.Fatalf("unbounded/leaked DNS operations: peak=%d active=%d", r.peak, r.active)
	}
	hosts, err := e.store.GetHostMetadataAll()
	if err != nil || len(hosts) != 0 {
		t.Fatalf("shutdown poisoned persisted snapshots: %v, %v", hosts, err)
	}
}

type fakeNodes struct {
	mu    sync.Mutex
	info  map[string]ygg.NodeInfo
	errs  map[string]error
	calls map[string]int
}

func (f *fakeNodes) NodeInfo(ctx context.Context, key string) (ygg.NodeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[key]++
	if _, ok := ctx.Deadline(); !ok {
		return ygg.NodeInfo{}, errors.New("missing lookup deadline")
	}
	return f.info[key], f.errs[key]
}

// observeKeys reports a successful handshake with the given key for each URI.
func observeKeys(t *testing.T, s *store.Store, now time.Time, keys map[string]string) {
	t.Helper()
	p, _, err := s.CreateProbe("probe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckInProbe(p.ID, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceProbeRounds(now, 10*time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	work, err := s.CheckInProbe(p.ID, false, now)
	if err != nil || work == nil {
		t.Fatalf("assignment = %v, %v", work, err)
	}
	report := protocol.ReportRequest{RoundID: work.RoundID}
	for _, target := range work.Targets {
		report.Results = append(report.Results, protocol.Result{PeerID: target.PeerID, Status: protocol.Up, Key: keys[target.URI], ProtocolVersion: "0.5"})
	}
	if _, err := s.SubmitProbeResults(p.ID, report, now); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshFetchesNodeInfoPerObservedKey(t *testing.T) {
	tcp, tls, other := "tcp://192.0.2.1:1", "tls://192.0.2.1:2", "tcp://192.0.2.2:1"
	e, _, now := metadataFixture(t, []store.PeerInput{peer(tcp), peer(tls), peer(other)})
	shared, unreachable := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	observeKeys(t, e.store, *now, map[string]string{tcp: shared, tls: shared, other: unreachable})
	nodes := &fakeNodes{
		info:  map[string]ygg.NodeInfo{shared: {BuildName: "yggdrasil", BuildVersion: "0.5.14", BuildPlatform: "linux", BuildArch: "amd64"}},
		errs:  map[string]error{unreachable: errors.New("timed out waiting for response")},
		calls: make(map[string]int),
	}
	e.nodes = nodes
	if err := e.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("refresh error = %v", err)
	}
	if nodes.calls[shared] != 1 || nodes.calls[unreachable] != 1 {
		t.Fatalf("lookups = %v, want one per key", nodes.calls)
	}
	got, err := e.store.GetNodeMetadata(shared)
	if err != nil || got.BuildVersion != "0.5.14" || got.BuildArch != "amd64" || !got.FetchedAt.Equal(*now) || !got.ExpiresAt.Equal(now.Add(nodeFreshness)) {
		t.Fatalf("snapshot = %+v, %v", got, err)
	}
	failed, err := e.store.GetNodeMetadata(unreachable)
	if err != nil || failed.Error == "" || !failed.FetchedAt.IsZero() || !failed.NextAttemptAt.Equal(now.Add(nodeRetryDelay)) {
		t.Fatalf("failure = %+v, %v", failed, err)
	}
	*now = now.Add(nodeRetryDelay)
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("retry of the unreachable node did not fail")
	}
	if nodes.calls[shared] != 1 || nodes.calls[unreachable] != 2 {
		t.Fatalf("lookups after retry delay = %v; fresh snapshots must not be refetched", nodes.calls)
	}
}
