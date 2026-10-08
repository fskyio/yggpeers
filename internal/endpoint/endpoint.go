// Package endpoint extracts the destination identity without changing a peer's URI.
package endpoint

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

type Destination struct {
	Host      string
	Port      string
	ProxyHost string
	ProxyPort string
}

func Parse(uri string) (Destination, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return Destination{}, err
	}
	if u.Opaque != "" || u.Host == "" || u.Fragment != "" {
		return Destination{}, fmt.Errorf("invalid peer URI")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "tcp", "tls", "quic", "ws", "wss", "socks", "sockstls":
	default:
		return Destination{}, fmt.Errorf("unsupported peer protocol %q", u.Scheme)
	}
	authority := u.Host
	if (scheme == "ws" || scheme == "wss") && u.Port() == "" {
		if u.Host != u.Hostname() && u.Host != "["+u.Hostname()+"]" {
			return Destination{}, fmt.Errorf("invalid WebSocket authority")
		}
		port := "80"
		if scheme == "wss" {
			port = "443"
		}
		authority = net.JoinHostPort(u.Hostname(), port)
	}
	host, port, err := splitAddress(authority)
	if err != nil {
		return Destination{}, err
	}
	d := Destination{Host: host, Port: port}
	if scheme == "socks" || scheme == "sockstls" {
		// The authority names the local proxy, not the remote peer. Require
		// exactly one destination segment; never silently use a malformed path.
		if !strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path[1:], "/") {
			return Destination{}, fmt.Errorf("invalid SOCKS destination path")
		}
		d.Host, d.Port, err = splitAddress(strings.TrimPrefix(u.Path, "/"))
		if err != nil {
			return Destination{}, fmt.Errorf("SOCKS destination: %w", err)
		}
		d.ProxyHost, d.ProxyPort = host, port
	} else if scheme != "ws" && scheme != "wss" && u.Path != "" && u.Path != "/" {
		return Destination{}, fmt.Errorf("unexpected peer URI path")
	}
	return d, nil
}

func splitAddress(address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", err
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", "", fmt.Errorf("invalid peer port %q", port)
	}
	host, err = NormalizeHost(host)
	if err != nil {
		return "", "", err
	}
	return host, strconv.FormatUint(n, 10), nil
}

func NormalizeHost(host string) (string, error) {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
		if _, err := netip.ParseAddr(host); err != nil {
			return "", fmt.Errorf("invalid bracketed destination IP")
		}
	}
	host = strings.TrimSuffix(host, ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String(), nil
	}
	if strings.ContainsAny(host, ":/%[]\\ \t\r\n") {
		return "", fmt.Errorf("invalid destination host %q", host)
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("invalid destination host: %w", err)
	}
	ascii = strings.ToLower(ascii)
	if len(ascii) == 0 || len(ascii) > 253 {
		return "", fmt.Errorf("invalid destination host length")
	}
	allNumeric := true
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid destination DNS label")
		}
		for _, c := range label {
			if c < '0' || c > '9' {
				allNumeric = false
			}
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("invalid destination DNS label")
			}
		}
	}
	if allNumeric {
		return "", fmt.Errorf("invalid numeric destination host")
	}
	return ascii, nil
}

func IsOverlayHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, suffix := range []string{"onion", "i2p", "loki"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// PublicHost reports whether a semantic destination is eligible for public
// DNS/ASN enrichment. It does not establish that the host exists or is reachable.
func PublicHost(host string) bool {
	host, err := NormalizeHost(host)
	if err != nil || IsOverlayHost(host) {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return PublicIP(ip)
	}
	if !strings.Contains(host, ".") {
		return false
	}
	for _, suffix := range []string{
		"localhost", "local", "invalid", "test", "example", "internal", "alt",
		"home.arpa", "example.com", "example.net", "example.org",
	} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	return true
}

// PublicIP is deliberately conservative: global unicast includes private,
// documentation, transition and other special-purpose ranges that must not be
// disclosed to the public ASN service. Mapped IPv4 uses the IPv4 policy.
func PublicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() {
		return false
	}
	if ip.Is6() && !publicV6.Contains(ip) {
		return false
	}
	for _, prefix := range specialPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

var publicV6 = netip.MustParsePrefix("2000::/3")

var specialPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
}
