package endpoint

import (
	"net/netip"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"EXAMPLE.Org.", "example.org"},
		{"BÜCHER.Example", "xn--bcher-kva.example"},
		{"2001:0DB8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"::ffff:8.8.8.8", "8.8.8.8"},
		{"8.8.8.8.", "8.8.8.8"},
		{"fe80::1%eth0", "fe80::1%eth0"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := NormalizeHost(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeHost(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
			}
		})
	}
	for _, input := range []string{"", ".", "example..org", "example.org..", "bad host", "bad/host", "bad_host", "-bad.example", "example:12", "999.1.2.3", "[broken]", "8.8.8.8%eth0"} {
		if got, err := NormalizeHost(input); err == nil {
			t.Errorf("NormalizeHost(%q) accepted %q", input, got)
		}
	}
}

func TestParseDestinations(t *testing.T) {
	for _, tc := range []struct {
		uri  string
		want Destination
	}{
		{"tls://EXAMPLE.org.:00080?key=abc", Destination{Host: "example.org", Port: "80"}},
		{"tcp://[2001:0db8:0:0::1]:123", Destination{Host: "2001:db8::1", Port: "123"}},
		{"quic://[::ffff:8.8.8.8]:321", Destination{Host: "8.8.8.8", Port: "321"}},
		{"socks://user:secret@localhost:9050/REMOTE.Onion:1234?key=abc", Destination{Host: "remote.onion", Port: "1234", ProxyHost: "localhost", ProxyPort: "9050"}},
		{"sockstls://[::1]:9050/[2606:4700:4700:0:0:0:0:1111]:443", Destination{Host: "2606:4700:4700::1111", Port: "443", ProxyHost: "::1", ProxyPort: "9050"}},
		{"socks://localhost:9050/%5B2606:4700:4700::1111%5D:443", Destination{Host: "2606:4700:4700::1111", Port: "443", ProxyHost: "localhost", ProxyPort: "9050"}},
		{"ws://Example.org/chat", Destination{Host: "example.org", Port: "80"}},
		{"wss://example.org", Destination{Host: "example.org", Port: "443"}},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			got, err := Parse(tc.uri)
			if err != nil || got != tc.want {
				t.Fatalf("Parse = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestParseRejectsMalformedDestinations(t *testing.T) {
	for _, uri := range []string{
		"socks://localhost:9050", "socks://localhost:9050/", "socks://localhost:9050/example.org",
		"socks://localhost:9050/example.org:123/extra", "socks://localhost:9050//example.org:123",
		"socks://localhost:9050/example.org%2f:123", "socks://localhost:9050/example.org:0",
		"sockstls://localhost:9050/2001:db8::1:123", "socks://localhost:9050/:123",
		"tls://example.org:65536", "tls://example.org:abc", "tcp://example.org", "tls://example.org:123/extra",
		"tcp://:123", "tcp://example.org:123#fragment", "unix:///tmp/peer", "example.org:123",
		"ws://example.org:", "ws://2606:4700:4700::1111/chat",
	} {
		if got, err := Parse(uri); err == nil {
			t.Errorf("Parse(%q) accepted %+v", uri, got)
		}
	}
}

func TestPublicIPExcludesSpecialAddresses(t *testing.T) {
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "::ffff:8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if !PublicIP(netip.MustParseAddr(ip)) {
			t.Errorf("rejected public address %s", ip)
		}
	}
	for _, ip := range []string{
		"0.0.0.0", "0.1.2.3", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.1.1", "172.16.0.1",
		"192.0.0.8", "192.0.2.1", "192.88.99.1", "192.168.1.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "255.255.255.255", "::ffff:192.168.1.1", "::", "::1", "fe80::1", "fe80::1%eth0",
		"fc00::1", "ff02::1", "64:ff9b::808:808", "100::1", "2001::1", "2001:2::1", "2001:20::1", "2001:db8::1", "2002:0808:0808::1", "3fff::1",
		"192.31.196.1", "192.52.193.1", "192.175.48.1", "2620:4f:8000::1",
	} {
		if PublicIP(netip.MustParseAddr(ip)) {
			t.Errorf("accepted special address %s", ip)
		}
	}
	if PublicIP(netip.Addr{}) {
		t.Fatal("accepted invalid address")
	}
}

func TestOverlaySuffixes(t *testing.T) {
	for _, host := range []string{"hidden.Onion.", "peer.b32.i2p", "peer.loki", "onion"} {
		if !IsOverlayHost(host) {
			t.Errorf("missed overlay %q", host)
		}
	}
	for _, host := range []string{"notonion", "onion.example", "i2p.example"} {
		if IsOverlayHost(host) {
			t.Errorf("misidentified %q", host)
		}
	}
}

func TestPublicHostEligibility(t *testing.T) {
	for _, host := range []string{"DNS.UnitChecks.Net.", "bücher.unitchecks.net", "8.8.8.8", "::ffff:8.8.8.8", "[2606:4700:4700::1111]"} {
		if !PublicHost(host) {
			t.Errorf("rejected public host %q", host)
		}
	}
	for _, host := range []string{
		"", "bad host", "router", "localhost", "host.localhost", "host.local", "host.invalid", "host.test",
		"host.example", "example.net", "host.example.org", "host.internal", "host.alt", "router.home.arpa",
		"hidden.onion", "hidden.i2p", "hidden.loki", "10.0.0.1", "::ffff:10.0.0.1", "2001:db8::1",
	} {
		if PublicHost(host) {
			t.Errorf("accepted special/nonpublic host %q", host)
		}
	}
}
