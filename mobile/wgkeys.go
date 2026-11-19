package turnvpn

import "github.com/pristupaanastasia/turnvpn/internal/wgkey"

// GenerateWGKeypair returns a fresh WireGuard keypair as "privateHex publicHex".
//
// First brick of the Android WireGuard migration (see the PR description): the app
// generates a keypair on the device, keeps the private half in the Keystore
// (EncryptedSharedPreferences), and shows the public half for the operator to add
// to the server's peers file. The data plane itself moves to WireGuard in later
// commits on this branch.
func GenerateWGKeypair() (string, error) {
	priv, pub, err := wgkey.Generate()
	if err != nil {
		return "", err
	}
	return priv + " " + pub, nil
}
