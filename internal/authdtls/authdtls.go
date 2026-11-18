// Package authdtls configures the tunnel's DTLS layer to MUTUALLY AUTHENTICATE
// its two endpoints with a pre-shared key, so only holders of the shared secret
// can establish — or man-in-the-middle — a session.
//
// The cipher suite is ECDHE-PSK with ChaCha20-Poly1305:
//   - ECDHE: ephemeral Diffie-Hellman, so each session key is fresh and a later
//     secret compromise does not decrypt past traffic (forward secrecy);
//   - PSK: the handshake only completes if both sides derive the same key from
//     the shared secret, which authenticates each end to the other (an attacker
//     without the secret cannot complete the handshake, so it cannot pose as the
//     server to the client nor use the server as an open proxy);
//   - ChaCha20-Poly1305: an AEAD, so every record is encrypted and tamper-evident.
//
// This replaces the earlier self-signed + InsecureSkipVerify setup, which
// encrypted but did not authenticate the peer.
package authdtls

import (
	"crypto/sha256"

	"github.com/pion/dtls/v3"
)

// Cipher is the only suite the endpoints offer, forcing PSK mutual auth.
const Cipher = dtls.TLS_ECDHE_PSK_WITH_CHACHA20_POLY1305_SHA256

// identityHint is sent in the handshake; it is not a secret.
const identityHint = "turnvpn"

// deriveKey turns a human secret into a fixed 32-byte PSK. A domain prefix keeps
// the derived key specific to this use.
func deriveKey(secret string) []byte {
	h := sha256.Sum256([]byte("turnvpn-psk-v1:" + secret))
	return h[:]
}

// PSK returns a callback that supplies the pre-shared key derived from secret.
func PSK(secret string) dtls.PSKCallback {
	key := deriveKey(secret)
	return func([]byte) ([]byte, error) { return key, nil }
}

// IdentityHint returns the PSK identity hint bytes.
func IdentityHint() []byte { return []byte(identityHint) }
