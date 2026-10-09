package ygg

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// realHandshake was captured from tcp://ygg1.mk16.de:1337, a public node
// running Yggdrasil 0.5. Ed25519 signatures are deterministic, so it is stable.
const realHandshake = "6d6574610075000000020000000100020005000200200000000087ee9949eeab56bd430ee8f324cad55abf3993ed9b9be63ce693e18a0003000100419b14e7298c293a91f2940c354b17c75eaea9106d78decf8accdce4613ba216918788fcf4c3d641c10a5efcb002ce5b1c0e911980e7a0cc5a84d5aa5478aa08"

const realKey = "0000000087ee9949eeab56bd430ee8f324cad55abf3993ed9b9be63ce693e18a"

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(realHandshake)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadMetadataFromYggdrasilNode(t *testing.T) {
	m, err := ReadMetadata(bytes.NewReader(fixture(t)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.MajorVersion != 0 || m.MinorVersion != 5 || m.Priority != 0 || hex.EncodeToString(m.PublicKey) != realKey {
		t.Fatalf("metadata = %+v", m)
	}
	if _, err := ReadMetadata(bytes.NewReader(fixture(t)), []byte("secret")); !errors.Is(err, ErrSignature) {
		t.Fatalf("unexpected password accepted: %v", err)
	}
}

func TestReadMetadataSkipsUnknownFields(t *testing.T) {
	// The signature covers only the key, so a future field keeps it valid.
	b := fixture(t)
	body := b[6 : len(b)-ed25519.SignatureSize]
	extended := append(bytes.Clone(b[:6]), body...)
	extended = append(extended, 0x01, 0x00, 0x00, 0x03, 'n', 'e', 'w')
	extended = append(extended, b[len(b)-ed25519.SignatureSize:]...)
	binary.BigEndian.PutUint16(extended[4:6], uint16(len(extended)-6))
	m, err := ReadMetadata(bytes.NewReader(extended), nil)
	if err != nil || hex.EncodeToString(m.PublicKey) != realKey {
		t.Fatalf("metadata = %+v, error = %v", m, err)
	}
}

func TestReadMetadataRejectsInvalidHandshakes(t *testing.T) {
	b := fixture(t)
	wrongKeySize := bytes.Clone(b)
	wrongKeySize[21] = 31 // public key field length
	for _, tc := range []struct {
		name  string
		input []byte
		want  error
	}{
		{"other protocol", []byte("HTTP/1.1 400 Bad Request\r\n\r\n"), ErrNotYggdrasil},
		{"truncated", b[:len(b)-1], io.ErrUnexpectedEOF},
		{"known field with wrong size", wrongKeySize, ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadMetadata(bytes.NewReader(tc.input), nil); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEncodeRoundTripsWithPassword(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := Metadata{MajorVersion: 0, MinorVersion: 5, PublicKey: public, Priority: 3}
	b, err := sent.Encode(private, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadMetadata(bytes.NewReader(b), []byte("secret"))
	if err != nil || got.MinorVersion != 5 || got.Priority != 3 || !got.PublicKey.Equal(public) {
		t.Fatalf("metadata = %+v, error = %v", got, err)
	}
	if _, err := ReadMetadata(bytes.NewReader(b), nil); !errors.Is(err, ErrSignature) {
		t.Fatalf("missing password accepted: %v", err)
	}
}
