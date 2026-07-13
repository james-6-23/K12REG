package pkce

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// Generate returns (code_verifier, code_challenge) for S256 PKCE.
func Generate() (verifier, challenge string, err error) {
	buf := make([]byte, 64)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}
