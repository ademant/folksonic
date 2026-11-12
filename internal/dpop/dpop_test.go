package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	testURL    = "https://music.example.org/api/device/me"
	testMethod = "GET"
)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestValidProofVerifies(t *testing.T) {
	k := newKey(t)
	now := time.Now()
	proof, err := NewProof(k, testMethod, testURL, "jti-1", now)
	if err != nil {
		t.Fatal(err)
	}
	jwk, claims, err := Verify(proof, testMethod, testURL, now, time.Minute)
	if err != nil {
		t.Fatalf("valid proof refused: %v", err)
	}
	if claims.Jti != "jti-1" {
		t.Errorf("jti %q", claims.Jti)
	}
	want, _ := Thumbprint(jwk)
	got, _ := Thumbprint(JWK{Kty: "EC", Crv: "P-256", X: jwk.X, Y: jwk.Y})
	if want != got || want == "" {
		t.Errorf("thumbprint mismatch")
	}
}

func TestRefusals(t *testing.T) {
	k := newKey(t)
	now := time.Now()
	good, _ := NewProof(k, testMethod, testURL, "j", now)
	other, _ := NewProof(newKey(t), testMethod, testURL, "j", now)

	cases := []struct {
		name   string
		proof  string
		method string
		url    string
		at     time.Time
		want   error
	}{
		{"wrong method", good, "POST", testURL, now, ErrMethod},
		{"wrong url", good, testMethod, testURL + "/other", now, ErrURL},
		{"stale", good, testMethod, testURL, now.Add(2 * time.Minute), ErrTimestamp},
		{"future", good, testMethod, testURL, now.Add(-2 * time.Minute), ErrTimestamp},
		{"malformed", "not.a.proof.at.all", testMethod, testURL, now, ErrMalformed},
		{"two parts", "abc.def", testMethod, testURL, now, ErrMalformed},
		{"tampered payload", tamper(t, good), testMethod, testURL, now, ErrSignature},
		{"signed by other key", swapKey(t, good, other), testMethod, testURL, now, ErrSignature},
		{"hs256", withHeader(t, good, `{"typ":"dpop+jwt","alg":"HS256"}`), testMethod, testURL, now, ErrAlgorithm},
	}
	for _, c := range cases {
		_, _, err := Verify(c.proof, c.method, c.url, c.at, time.Minute)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if c.want != nil && err != c.want {
			t.Errorf("%s: err %v, want %v", c.name, err, c.want)
		}
	}
}

func TestMissingJTIRefused(t *testing.T) {
	k := newKey(t)
	now := time.Now()
	proof, _ := NewProof(k, testMethod, testURL, "", now)
	if _, _, err := Verify(proof, testMethod, testURL, now, time.Minute); err != ErrNoJTI {
		t.Fatalf("empty jti: %v", err)
	}
}

func TestPrivateKeyInHeaderRefused(t *testing.T) {
	k := newKey(t)
	now := time.Now()
	proof, _ := NewProof(k, testMethod, testURL, "j", now)
	withD := withHeader(t, proof, `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":"AA","y":"AA","d":"AA"}}`)
	if _, _, err := Verify(withD, testMethod, testURL, now, time.Minute); err == nil {
		t.Fatal("bogus key accepted")
	}
}

func TestThumbprintIsRFC7638Canonical(t *testing.T) {
	k := newKey(t)
	jwk := JWK{
		Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(padTo32(k.X)),
		Y: base64.RawURLEncoding.EncodeToString(padTo32(k.Y)),
	}
	got, err := Thumbprint(jwk)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, "+/=") || len(got) != 43 {
		t.Errorf("thumbprint %q is not unpadded base64url of SHA-256", got)
	}
	other, _ := Thumbprint(JWK{Kty: "EC", Crv: "P-256", X: jwk.Y, Y: jwk.X})
	if other == got {
		t.Error("different keys share a thumbprint")
	}
}

func TestValidateRejectsBadKeys(t *testing.T) {
	bad := []JWK{
		{Kty: "RSA", Crv: "P-256", X: "AA", Y: "AA"},
		{Kty: "EC", Crv: "P-384", X: "AA", Y: "AA"},
		{Kty: "EC", Crv: "P-256", X: "AA", Y: "AA"},
	}
	for _, k := range bad {
		if err := k.Validate(); err == nil {
			t.Errorf("accepted %+v", k)
		}
	}
}

// tamper changes the payload, which the signature no longer covers.
func tamper(t *testing.T, proof string) string {
	t.Helper()
	parts := strings.Split(proof, ".")
	payload := `{"jti":"x","htm":"GET","htu":"` + testURL + `","iat":` + "0" + `}`
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(payload))
	return strings.Join(parts, ".")
}

// swapKey keeps the header of proof but takes the signature from other.
func swapKey(t *testing.T, proof, other string) string {
	t.Helper()
	p := strings.Split(proof, ".")
	o := strings.Split(other, ".")
	return p[0] + "." + p[1] + "." + o[2]
}

// withHeader replaces the header, leaving the payload and signature.
func withHeader(t *testing.T, proof, header string) string {
	t.Helper()
	parts := strings.Split(proof, ".")
	parts[0] = base64.RawURLEncoding.EncodeToString([]byte(header))
	var check map[string]any
	if err := json.Unmarshal([]byte(header), &check); err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, ".")
}
