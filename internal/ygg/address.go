package ygg

import (
	"crypto/ed25519"
	"net/netip"
)

// AddressForKey returns the node's address in 200::/7. After the 0x02 prefix
// byte comes the number of leading zero bits in the key, then the inverted key
// bits that follow its first one bit, truncated to fill the address.
func AddressForKey(key ed25519.PublicKey) netip.Addr {
	var addr [16]byte
	addr[0] = 0x02
	bits := len(key) * 8
	bit := 0
	for bit < bits && key[bit/8]&(0x80>>(bit%8)) == 0 {
		bit++
	}
	addr[1] = byte(bit)
	bit++ // the first one bit only terminates the count
	for out := 16; out < 128 && bit < bits; out, bit = out+1, bit+1 {
		if key[bit/8]&(0x80>>(bit%8)) == 0 {
			addr[out/8] |= 0x80 >> (out % 8)
		}
	}
	return netip.AddrFrom16(addr)
}
