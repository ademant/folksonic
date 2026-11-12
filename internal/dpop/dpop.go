// Package dpop verifies DPoP proofs (RFC 9449) for proof-only device auth: a
// paired app has no bearer secret, only a key in its Keystore, and signs each
// request with it. The package uses only the standard library.
package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"
)

// Errors returned by Verify. A caller refuses the request; the reason is for
// the log, not for the client.
var (
	ErrMalformed = errors.New("dpop: malformed proof")
	ErrAlgorithm = errors.New("dpop: proof must be ES256 with typ dpop+jwt")
	ErrKey       = errors.New("dpop: proof key must be a public P-256 key")
	ErrSignature = errors.New("dpop: signature does not verify")
	ErrMethod    = errors.New("dpop: htm does not match the request method")
	ErrURL       = errors.New("dpop: htu does not match the request URL")
	ErrTimestamp = errors.New("dpop: iat is outside the allowed window")
	ErrNoJTI     = errors.New("dpop: proof has no jti")
)

// JWK is the public part of a P-256 key, as sent in a proof header and at
// pairing.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// Claims are the proof's payload fields this server checks.
type Claims struct {
	Jti string `json:"jti"`
	Htm string `json:"htm"`
	Htu string `json:"htu"`
	Iat int64  `json:"iat"`
}

// Validate reports whether jwk is a usable public P-256 key.
func (k JWK) Validate() error {
	_, err := k.PublicKey()
	return err
}

// PublicKey converts the JWK to an ECDSA public key on P-256.
func (k JWK) PublicKey() (*ecdsa.PublicKey, error) {
	if k.Kty != "EC" || k.Crv != "P-256" {
		return nil, ErrKey
	}
	x, err := decodeCoord(k.X)
	if err != nil {
		return nil, ErrKey
	}
	y, err := decodeCoord(k.Y)
	if err != nil {
		return nil, ErrKey
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	if !pub.Curve.IsOnCurve(x, y) {
		return nil, ErrKey
	}
	return pub, nil
}

// decodeCoord reads a 32-byte base64url coordinate, the only length P-256 allows.
func decodeCoord(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, ErrKey
	}
	return new(big.Int).SetBytes(b), nil
}

// Thumbprint is the RFC 7638 thumbprint of the key: the base64url SHA-256 of its
// required members in lexicographic order. It names the device.
func Thumbprint(k JWK) (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	canonical := `{"crv":"P-256","kty":"EC","x":"` + k.X + `","y":"` + k.Y + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// Verify checks a DPoP proof for a request with the given method and URL
// (the URL without its query string), at time now. The proof's iat may differ
// from now by at most skew. It returns the key that signed the proof and the
// claims. The caller checks jti against the proofs already seen.
func Verify(proof, method, htu string, now time.Time, skew time.Duration) (JWK, Claims, error) {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return JWK{}, Claims{}, ErrMalformed
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return JWK{}, Claims{}, ErrMalformed
	}
	var header struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		JWK *JWK   `json:"jwk"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return JWK{}, Claims{}, ErrMalformed
	}
	if header.Alg != "ES256" || header.Typ != "dpop+jwt" {
		return JWK{}, Claims{}, ErrAlgorithm
	}
	if header.JWK == nil {
		return JWK{}, Claims{}, ErrMalformed
	}
	pub, err := header.JWK.PublicKey()
	if err != nil {
		return JWK{}, Claims{}, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return JWK{}, Claims{}, ErrSignature
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return JWK{}, Claims{}, ErrSignature
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return JWK{}, Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(payloadJSON, &c); err != nil {
		return JWK{}, Claims{}, ErrMalformed
	}
	if c.Jti == "" {
		return JWK{}, Claims{}, ErrNoJTI
	}
	if c.Htm != method {
		return JWK{}, Claims{}, ErrMethod
	}
	if c.Htu != htu {
		return JWK{}, Claims{}, ErrURL
	}
	iat := time.Unix(c.Iat, 0)
	if iat.Before(now.Add(-skew)) || iat.After(now.Add(skew)) {
		return JWK{}, Claims{}, ErrTimestamp
	}
	return *header.JWK, c, nil
}
