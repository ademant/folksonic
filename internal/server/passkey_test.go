package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// softAuthenticator is a minimal WebAuthn authenticator for tests: one ES256
// key, "none" attestation, and assertions signed over authenticatorData and
// clientDataHash as the spec requires.
type softAuthenticator struct {
	key    *ecdsa.PrivateKey
	credID []byte
	origin string
	rpID   string
}

func newSoftAuthenticator(t *testing.T, origin, rpID string) *softAuthenticator {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &softAuthenticator{key: k, credID: []byte("soft-authenticator-cred-1"), origin: origin, rpID: rpID}
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64u(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (a *softAuthenticator) clientData(t *testing.T, typ, challenge string) ([]byte, [32]byte) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type":        typ,
		"challenge":   challenge,
		"origin":      a.origin,
		"crossOrigin": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, sha256.Sum256(raw)
}

// registration builds the navigator.credentials.create() response.
func (a *softAuthenticator) registration(t *testing.T, challenge string) []byte {
	t.Helper()
	clientJSON, _ := a.clientData(t, "webauthn.create", challenge)

	rpHash := sha256.Sum256([]byte(a.rpID))
	x, y := a.key.PublicKey.X.Bytes(), a.key.PublicKey.Y.Bytes()
	pad := func(b []byte) []byte {
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return out
	}
	coseKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: pad(x), -3: pad(y)})
	if err != nil {
		t.Fatal(err)
	}
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x41)                // UP | AT
	authData = append(authData, 0, 0, 0, 0)          // signCount
	authData = append(authData, make([]byte, 16)...) // AAGUID
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(a.credID)))
	authData = append(authData, l[:]...)
	authData = append(authData, a.credID...)
	authData = append(authData, coseKey...)

	attObj, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := json.Marshal(map[string]any{
		"id":    b64u(a.credID),
		"rawId": b64u(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64u(clientJSON),
			"attestationObject": b64u(attObj),
		},
		"clientExtensionResults": map[string]any{},
	})
	return resp
}

// assertion builds the navigator.credentials.get() response.
func (a *softAuthenticator) assertion(t *testing.T, challenge string) []byte {
	t.Helper()
	clientJSON, clientHash := a.clientData(t, "webauthn.get", challenge)
	rpHash := sha256.Sum256([]byte(a.rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x05)       // UP | UV
	authData = append(authData, 0, 0, 0, 1) // signCount
	signed := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := json.Marshal(map[string]any{
		"id":    b64u(a.credID),
		"rawId": b64u(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64u(clientJSON),
			"authenticatorData": b64u(authData),
			"signature":         b64u(sig),
		},
		"clientExtensionResults": map[string]any{},
	})
	return resp
}

func passkeyState(t *testing.T) *State {
	t.Helper()
	s := newTestState(t)
	wa, err := newWebAuthn("http://localhost:4598")
	if err != nil {
		t.Fatal(err)
	}
	s.webAuthn = wa
	s.ceremonies = &ceremonyStore{m: map[string]ceremony{}}
	return s
}

// The full passkey path: enrol with a key, then sign in with it and receive a
// session token that authenticates as a Bearer on the API.
func TestPasskeyRegisterAndLogin(t *testing.T) {
	s := passkeyState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	key, err := users.CreateAPIKey(ctx, s.Pool, admin.ID, "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	auth := newSoftAuthenticator(t, "http://localhost:4598", "localhost")

	// Enrol.
	req := httptest.NewRequest(http.MethodPost, "/api/passkey/register/begin?apiKey="+key, nil)
	rec := httptest.NewRecorder()
	s.APIPasskeyRegisterBegin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register begin %d: %s", rec.Code, rec.Body.String())
	}
	var begin struct {
		Ceremony string `json:"ceremony"`
		Options  struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &begin); err != nil {
		t.Fatal(err)
	}
	body := auth.registration(t, begin.Options.PublicKey.Challenge)
	req = httptest.NewRequest(http.MethodPost, "/api/passkey/register/finish?apiKey="+key+"&ceremony="+begin.Ceremony, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.APIPasskeyRegisterFinish(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register finish %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := users.ListPasskeys(ctx, s.Pool, admin.ID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("passkey not stored: %v %d", err, len(stored))
	}

	// Sign in.
	req = httptest.NewRequest(http.MethodPost, "/api/passkey/login/begin", strings.NewReader(`{"username":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.APIPasskeyLoginBegin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login begin %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &begin); err != nil {
		t.Fatal(err)
	}
	body = auth.assertion(t, begin.Options.PublicKey.Challenge)
	req = httptest.NewRequest(http.MethodPost, "/api/passkey/login/finish?ceremony="+begin.Ceremony, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.APIPasskeyLoginFinish(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login finish %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("no session token: %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	rec = httptest.NewRecorder()
	s.APIListKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session token rejected: %d", rec.Code)
	}
}

// A signature from a different key must not sign in.
func TestPasskeyRejectsForeignKey(t *testing.T) {
	s := passkeyState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	key, _ := users.CreateAPIKey(ctx, s.Pool, admin.ID, "test", 0)
	enrolled := newSoftAuthenticator(t, "http://localhost:4598", "localhost")

	req := httptest.NewRequest(http.MethodPost, "/api/passkey/register/begin?apiKey="+key, nil)
	rec := httptest.NewRecorder()
	s.APIPasskeyRegisterBegin(rec, req)
	var begin struct {
		Ceremony string `json:"ceremony"`
		Options  struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &begin)
	req = httptest.NewRequest(http.MethodPost, "/api/passkey/register/finish?apiKey="+key+"&ceremony="+begin.Ceremony,
		strings.NewReader(string(enrolled.registration(t, begin.Options.PublicKey.Challenge))))
	rec = httptest.NewRecorder()
	s.APIPasskeyRegisterFinish(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enrol failed: %s", rec.Body.String())
	}

	// A different key signs the login challenge with the enrolled credential id.
	impostor := newSoftAuthenticator(t, "http://localhost:4598", "localhost")
	req = httptest.NewRequest(http.MethodPost, "/api/passkey/login/begin", strings.NewReader(`{"username":"admin"}`))
	rec = httptest.NewRecorder()
	s.APIPasskeyLoginBegin(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &begin)
	body := impostor.assertion(t, begin.Options.PublicKey.Challenge)
	req = httptest.NewRequest(http.MethodPost, "/api/passkey/login/finish?ceremony="+begin.Ceremony, strings.NewReader(string(body)))
	rec = httptest.NewRecorder()
	s.APIPasskeyLoginFinish(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("signature from an unenrolled key signed in")
	}
}
