package metadata

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestOriginQueries(t *testing.T) {
	for _, tc := range []struct{ ip, query string }{
		{"8.8.4.4", "4.4.8.8.origin.asn.cymru.com."},
		{"::ffff:8.8.4.4", "4.4.8.8.origin.asn.cymru.com."},
		{"2001:db8::1", "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.origin6.asn.cymru.com."},
	} {
		if got := originQuery(netip.MustParseAddr(tc.ip)); got != tc.query {
			t.Errorf("originQuery(%s) = %s; want %s", tc.ip, got, tc.query)
		}
	}
}

func TestParseCymruMultiOrigin(t *testing.T) {
	got, err := parseOrigins([]string{
		"15169 64500 | 8.8.8.0/24 | US | arin | 2023-12-28",
		"64500 15169 | 8.8.8.0/24 | US | arin | 2023-12-28",
	}, netip.MustParseAddr("8.8.8.8"))
	if err != nil || !reflect.DeepEqual(got.asns, []uint32{15169, 64500}) || got.prefix != "8.8.8.0/24" {
		t.Fatalf("parseOrigins = %+v, %v", got, err)
	}
	v6, err := parseOrigins([]string{"13335 | 2606:4700::/32 | US | arin | 2011-11-01"}, netip.MustParseAddr("2606:4700:4700::1111"))
	if err != nil || !reflect.DeepEqual(v6.asns, []uint32{13335}) || v6.prefix != "2606:4700::/32" {
		t.Fatalf("IPv6 origin = %+v, %v", v6, err)
	}
	name, err := parseASNName([]string{"15169 | US | arin | 2000-03-30 | GOOGLE - Google LLC, US"}, 15169)
	if err != nil || name != "GOOGLE - Google LLC, US" {
		t.Fatalf("parseASNName = %q, %v", name, err)
	}
	for _, records := range [][]string{nil, {"NA | NA | NA | NA | NA"}} {
		negative, err := parseOrigins(records, netip.MustParseAddr("8.8.8.8"))
		if err != nil || len(negative.asns) != 0 || negative.prefix != "" {
			t.Fatalf("negative origin = %+v, %v", negative, err)
		}
	}
}

func TestMalformedCymruIsNotNegativeSuccess(t *testing.T) {
	for _, txt := range []string{
		"garbage", " | 8.8.8.0/24 | US | arin | 2023-12-28", "not-an-ASN | 8.8.8.0/24 | US | arin | 2023-12-28",
		"0 | 8.8.8.0/24 | US | arin | 2023-12-28", "15169 | invalid | US | arin | 2023-12-28",
		"15169 | 1.1.1.0/24 | AU | apnic | 2023-12-28", "4294967296 | 8.8.8.0/24 | US | arin | 2023-12-28",
	} {
		if got, err := parseOrigins([]string{txt}, netip.MustParseAddr("8.8.8.8")); err == nil {
			t.Errorf("malformed record %q became %+v", txt, got)
		}
	}
	if _, err := parseASNName([]string{"64500 | US | arin | 2000-03-30 | WRONG"}, 15169); err == nil {
		t.Error("accepted description for wrong ASN")
	}
}
