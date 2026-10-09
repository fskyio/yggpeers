// Package ygg implements the part of the Yggdrasil peering protocol a probe
// needs: the signed metadata both sides send as soon as a link opens.
package ygg

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"io"

	"golang.org/x/crypto/blake2b"
)

// Field types are fixed by the wire format; later versions may only add types.
const (
	fieldMajorVersion uint16 = iota
	fieldMinorVersion
	fieldPublicKey
	fieldPriority
)

var preamble = []byte("meta")

var (
	ErrNotYggdrasil = errors.New("remote did not send a Yggdrasil handshake")
	ErrMalformed    = errors.New("malformed Yggdrasil handshake")
	// ErrSignature usually means the link password differs from the peer's.
	ErrSignature = errors.New("handshake signature does not verify (wrong or missing password?)")
)

// Metadata is the identity a node announces when a link opens.
type Metadata struct {
	MajorVersion uint16
	MinorVersion uint16
	PublicKey    ed25519.PublicKey
	Priority     uint8
}

// ReadMetadata reads one handshake and verifies its signature with the link
// password, which is empty unless the peer URI sets one. Unknown fields are
// skipped like Yggdrasil does, and the version is reported rather than judged.
func ReadMetadata(r io.Reader, password []byte) (Metadata, error) {
	var header [6]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Metadata{}, err
	}
	if !bytes.Equal(header[:4], preamble) {
		return Metadata{}, ErrNotYggdrasil
	}
	body := make([]byte, binary.BigEndian.Uint16(header[4:]))
	if len(body) < ed25519.SignatureSize {
		return Metadata{}, ErrMalformed
	}
	if _, err := io.ReadFull(r, body); err != nil {
		return Metadata{}, err
	}
	fields, signature := body[:len(body)-ed25519.SignatureSize], body[len(body)-ed25519.SignatureSize:]
	var m Metadata
	for len(fields) > 0 {
		if len(fields) < 4 {
			return Metadata{}, ErrMalformed
		}
		kind, size := binary.BigEndian.Uint16(fields), int(binary.BigEndian.Uint16(fields[2:]))
		fields = fields[4:]
		if len(fields) < size {
			return Metadata{}, ErrMalformed
		}
		value := fields[:size]
		fields = fields[size:]
		switch {
		case kind == fieldMajorVersion && size == 2:
			m.MajorVersion = binary.BigEndian.Uint16(value)
		case kind == fieldMinorVersion && size == 2:
			m.MinorVersion = binary.BigEndian.Uint16(value)
		case kind == fieldPublicKey && size == ed25519.PublicKeySize:
			m.PublicKey = ed25519.PublicKey(value)
		case kind == fieldPriority && size == 1:
			m.Priority = value[0]
		case kind <= fieldPriority:
			return Metadata{}, ErrMalformed
		}
	}
	if m.PublicKey == nil {
		return Metadata{}, ErrMalformed
	}
	digest, err := keyDigest(m.PublicKey, password)
	if err != nil {
		return Metadata{}, err
	}
	if !ed25519.Verify(m.PublicKey, digest, signature) {
		return Metadata{}, ErrSignature
	}
	return m, nil
}

// Encode returns m in wire format, signed by key; it is the inverse of
// ReadMetadata. The signature covers only the public key and password.
func (m Metadata) Encode(key ed25519.PrivateKey, password []byte) ([]byte, error) {
	digest, err := keyDigest(m.PublicKey, password)
	if err != nil {
		return nil, err
	}
	b := append(make([]byte, 0, 128), preamble...)
	b = append(b, 0, 0) // body length, set below
	b = appendField(b, fieldMajorVersion, binary.BigEndian.AppendUint16(nil, m.MajorVersion))
	b = appendField(b, fieldMinorVersion, binary.BigEndian.AppendUint16(nil, m.MinorVersion))
	b = appendField(b, fieldPublicKey, m.PublicKey)
	b = appendField(b, fieldPriority, []byte{m.Priority})
	b = append(b, ed25519.Sign(key, digest)...)
	binary.BigEndian.PutUint16(b[4:6], uint16(len(b)-6))
	return b, nil
}

func appendField(b []byte, kind uint16, value []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, kind)
	b = binary.BigEndian.AppendUint16(b, uint16(len(value)))
	return append(b, value...)
}

// keyDigest fails only for passwords longer than Yggdrasil's 64-byte limit.
func keyDigest(key ed25519.PublicKey, password []byte) ([]byte, error) {
	h, err := blake2b.New512(password)
	if err != nil {
		return nil, err
	}
	h.Write(key)
	return h.Sum(nil), nil
}
