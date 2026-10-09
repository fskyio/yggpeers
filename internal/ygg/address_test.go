package ygg

import (
	"encoding/hex"
	"testing"
)

// Expected addresses are what Yggdrasil 0.5 reports for these public nodes.
func TestAddressForKey(t *testing.T) {
	for key, want := range map[string]string{
		"0000000087ee9949eeab56bd430ee8f324cad55abf3993ed9b9be63ce693e18a": "220:f022:cd6c:22a9:5285:79e2:2e19:b66a",
		"ecbbcb3298e7d3b4196103333c3e839cfe47a6ca47602b94a6d596683f6bb358": "200:2688:699a:ce30:5897:cd3d:f999:8782",
	} {
		b, err := hex.DecodeString(key)
		if err != nil {
			t.Fatal(err)
		}
		if got := AddressForKey(b).String(); got != want {
			t.Errorf("AddressForKey(%s) = %s, want %s", key, got, want)
		}
	}
}
