package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/users"
)

const (
	testClientID = "tinysonic-test"
	testBaseURL  = "https://music.example.test"
)

// fakeIDP is an OpenID Connect provider with just enough behaviour for the
// sign-in flow: discovery, JWKS, an authorization endpoint that issues a code
// for the subject set by the test, and a token endpoint that checks PKCE and
// signs an ID token with the nonce it was given.
type fakeIDP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu    sync.Mutex
	sub   string
	codes map[string]fakeCode
}

type fakeCode struct {
	subject, nonce, challenge string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIDP{key: key, codes: map[string]fakeCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		code := "code-" + q.Get("state")
		f.codes[code] = fakeCode{subject: f.sub, nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		c, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		idToken := f.signed(t, map[string]any{
			"iss":   f.srv.URL,
			"sub":   c.subject,
			"aud":   testClientID,
			"exp":   time.Now().Add(time.Hour).Unix(),
			"iat":   time.Now().Unix(),
			"nonce": c.nonce,
		})
		writeTestJSON(w, map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIDP) signed(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// oidcState returns a State wired to the fake provider.
func oidcState(t *testing.T) (*State, *fakeIDP) {
	t.Helper()
	s := newTestState(t)
	idp := newFakeIDP(t)
	s.BaseURL = testBaseURL
	s.sso = newOIDCService(config.OIDC{Issuer: idp.srv.URL, ClientID: testClientID}, testBaseURL)
	return s, idp
}

// begin starts a sign-in as body describes and returns the provider URL.
func begin(t *testing.T, s *State, token, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/oidc/begin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("begin %s: status %d: %s", body, rec.Code, rec.Body.String())
	}
	var j struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	return j.URL
}

// finish plays the browser's part: it follows the provider URL, which redirects
// back to the callback, and returns the callback's redirect location.
func finish(t *testing.T, s *State, authURL string) string {
	t.Helper()
	cb := callbackURI(t, authURL)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cb, nil))
	return rec.Header().Get("Location")
}

// callbackURI asks the fake provider for the redirect it would send the browser
// back with, and returns that callback path and query.
func callbackURI(t *testing.T, authURL string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return cb.RequestURI()
}

func exchange(t *testing.T, s *State, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/oidc/exchange", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// handoffCode pulls the one-time code out of a web redirect.
func handoffCode(t *testing.T, location string) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(location, testBaseURL+"/login#oidc=") {
		t.Fatalf("redirect %q, want the web sign-in page with a code", location)
	}
	return u.Fragment[len("oidc="):]
}

func linkedAccount(t *testing.T, s *State, name, subject string) users.User {
	t.Helper()
	ctx := context.Background()
	u, err := users.Create(ctx, s.Pool, name, "pw-"+name, users.RoleUser, users.StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.LinkOIDC(ctx, s.Pool, u.ID, s.sso.cfg.Issuer, subject); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestOIDCSignInWithLinkedIdentityIssuesSession(t *testing.T) {
	s, idp := oidcState(t)
	alice := linkedAccount(t, s, "alice", "subject-alice")
	idp.sub = "subject-alice"

	loc := finish(t, s, begin(t, s, "", `{"mode":"signin"}`))
	code, out := exchange(t, s, `{"code":"`+handoffCode(t, loc)+`","device":"web"}`)
	if code != http.StatusOK {
		t.Fatalf("exchange status %d: %v", code, out)
	}
	got, err := users.LookupSession(context.Background(), s.Pool, out["token"].(string))
	if err != nil || got.ID != alice.ID {
		t.Fatalf("session belongs to %+v, err %v; want alice", got, err)
	}
}

func TestOIDCUnlinkedIdentityCreatesNothing(t *testing.T) {
	s, idp := oidcState(t)
	idp.sub = "stranger"

	loc := finish(t, s, begin(t, s, "", `{"mode":"signin"}`))
	if !strings.Contains(loc, "/login?oidc_error=") {
		t.Fatalf("redirect %q, want an error on the sign-in page", loc)
	}
	if _, err := users.ByName(context.Background(), s.Pool, "stranger"); err == nil {
		t.Fatal("sign-in with an unlinked identity created an account")
	}
}

func TestOIDCStateAndHandoffAreSingleUse(t *testing.T) {
	s, idp := oidcState(t)
	linkedAccount(t, s, "bob", "subject-bob")
	idp.sub = "subject-bob"

	authURL := begin(t, s, "", `{"mode":"signin"}`)
	cb := callbackURI(t, authURL)
	code := handoffCode(t, finish(t, s, authURL))

	// Replaying the same callback fails: its state was consumed.
	replay := httptest.NewRecorder()
	s.routes().ServeHTTP(replay, httptest.NewRequest(http.MethodGet, cb, nil))
	if replay.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback: status %d, want 400", replay.Code)
	}

	if c, _ := exchange(t, s, `{"code":"`+code+`"}`); c != http.StatusOK {
		t.Fatalf("first exchange: status %d", c)
	}
	if c, _ := exchange(t, s, `{"code":"`+code+`"}`); c != http.StatusUnauthorized {
		t.Fatalf("second exchange: status %d, want 401", c)
	}
}

func TestOIDCAppSignInIssuesDeviceKey(t *testing.T) {
	s, idp := oidcState(t)
	linkedAccount(t, s, "carol", "subject-carol")
	idp.sub = "subject-carol"

	loc := finish(t, s, begin(t, s, "", `{"mode":"signin","client":"app"}`))
	if !strings.HasPrefix(loc, "tinysonic://oidc?code=") {
		t.Fatalf("app redirect %q, want the app scheme", loc)
	}
	u, _ := url.Parse(loc)
	code, out := exchange(t, s, `{"code":"`+u.Query().Get("code")+`","device":"Pixel 9"}`)
	if code != http.StatusOK {
		t.Fatalf("exchange status %d: %v", code, out)
	}
	key := out["apiKey"].(string)
	if !strings.HasPrefix(key, "tsk_") {
		t.Fatalf("app got %q, want an API key", key)
	}
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+key, nil)); !ok {
		t.Fatal("device key does not authenticate")
	}
}

func TestOIDCInviteCreatesLinkedAccount(t *testing.T) {
	s, idp := oidcState(t)
	ctx := context.Background()
	code, _, err := users.CreateInvite(ctx, s.Pool, users.RoleUser, "dave", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	idp.sub = "subject-dave"

	loc := finish(t, s, begin(t, s, "", `{"mode":"invite","invite":"`+code+`","username":"dave"}`))
	exCode, _ := exchange(t, s, `{"code":"`+handoffCode(t, loc)+`"}`)
	if exCode != http.StatusOK {
		t.Fatalf("exchange status %d", exCode)
	}
	dave, err := users.ByName(ctx, s.Pool, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if dave.Password != "" {
		t.Fatal("OIDC account has a password")
	}
	if linked, err := users.UserByOIDC(ctx, s.Pool, s.sso.cfg.Issuer, "subject-dave"); err != nil || linked.ID != dave.ID {
		t.Fatalf("identity not linked to dave: %v", err)
	}

	// The invitation is spent, so another identity cannot reuse it.
	idp.sub = "subject-eve"
	begun := httptest.NewRecorder()
	s.routes().ServeHTTP(begun, httptest.NewRequest(http.MethodPost, "/api/oidc/begin",
		strings.NewReader(`{"mode":"invite","invite":"`+code+`","username":"eve"}`)))
	if begun.Code != http.StatusForbidden {
		t.Fatalf("reused invite: status %d, want 403", begun.Code)
	}
}

func TestOIDCLinkAttachesIdentityToSignedInAccount(t *testing.T) {
	s, idp := oidcState(t)
	ctx := context.Background()
	frank, err := users.Create(ctx, s.Pool, "frank", "pw", users.RoleUser, users.StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	token, err := users.CreateSession(ctx, s.Pool, frank.ID)
	if err != nil {
		t.Fatal(err)
	}
	idp.sub = "subject-frank"

	loc := finish(t, s, begin(t, s, token, `{"mode":"link"}`))
	if loc != testBaseURL+"/account?linked=1" {
		t.Fatalf("link redirect %q", loc)
	}
	if got, err := users.UserByOIDC(ctx, s.Pool, s.sso.cfg.Issuer, "subject-frank"); err != nil || got.ID != frank.ID {
		t.Fatalf("identity not linked: %v", err)
	}
}

func TestOIDCLinkRefusesIdentityOfAnotherAccount(t *testing.T) {
	s, idp := oidcState(t)
	ctx := context.Background()
	linkedAccount(t, s, "grace", "subject-shared")
	hal, err := users.Create(ctx, s.Pool, "hal", "pw", users.RoleUser, users.StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := users.CreateSession(ctx, s.Pool, hal.ID)
	idp.sub = "subject-shared"

	loc := finish(t, s, begin(t, s, token, `{"mode":"link"}`))
	if !strings.Contains(loc, "oidc_error=") {
		t.Fatalf("redirect %q, want an error", loc)
	}
	got, err := users.UserByOIDC(ctx, s.Pool, s.sso.cfg.Issuer, "subject-shared")
	if err != nil || got.Username != "grace" {
		t.Fatalf("identity now belongs to %q (err %v), want grace", got.Username, err)
	}
}

func TestOIDCDisabledWithoutIssuer(t *testing.T) {
	s := newTestState(t)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/oidc/begin", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("begin without oidc: status %d, want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/oidc/status", nil))
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("status %s", rec.Body.String())
	}
}
