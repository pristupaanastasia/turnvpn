// Package wgkey generates and derives WireGuard (Curve25519) keys in the hex form
// wireguard-go's UAPI expects.
package wgkey

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/curve25519"
)

// clamp applies the standard X25519 private-key clamping WireGuard uses.
func clamp(sk []byte) {
	sk[0] &= 248
	sk[31] &= 127
	sk[31] |= 64
}

// Generate returns a new clamped private key and its public key, hex-encoded.
func Generate() (priv, pub string, err error) {
	var sk [32]byte
	if _, err := io.ReadFull(rand.Reader, sk[:]); err != nil {
		return "", "", err
	}
	clamp(sk[:])
	pk, err := curve25519.X25519(sk[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(sk[:]), hex.EncodeToString(pk), nil
}

// Public derives the hex public key from a hex private key.
func Public(privHex string) (string, error) {
	sk, err := hex.DecodeString(privHex)
	if err != nil {
		return "", fmt.Errorf("wgkey: decode private: %w", err)
	}
	if len(sk) != 32 {
		return "", fmt.Errorf("wgkey: private key must be 32 bytes, got %d", len(sk))
	}
	clamp(sk)
	pk, err := curve25519.X25519(sk, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(pk), nil
}
