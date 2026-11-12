package dpop

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"time"
)

// NewProof signs a DPoP proof for a request, with the app's private key. The
// app does this on the device (Keystore); the server only verifies. It is here
// for tests and tools.
func NewProof(key *ecdsa.PrivateKey, method, htu, jti string, iat time.Time) (string, error) {
	jwk := JWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(padTo32(key.X)),
		Y:   base64.RawURLEncoding.EncodeToString(padTo32(key.Y)),
	}
	header, err := json.Marshal(map[string]any{"typ": "dpop+jwt", "alg": "ES256", "jwk": jwk})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(Claims{Jti: jti, Htm: method, Htu: htu, Iat: iat.Unix()})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := append(padTo32(r), padTo32(s)...)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// padTo32 is the big-endian 32-byte form of n, as P-256 coordinates and
// signature halves must be.
func padTo32(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}
