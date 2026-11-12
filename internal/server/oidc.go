package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// Single sign-on with an OpenID Connect provider (the oidc config block). The
// flow is Authorization Code with PKCE and a nonce. An identity can only reach an
// account that is already linked to it, or that an invitation creates; the
// provider never creates accounts by itself.
//
// After a successful callback the server hands the browser or app a one-time
// code, which is exchanged for a session (web) or a device API key (app). Tokens
// from the provider never reach a URL.

const (
	oidcFlowTTL          = 10 * time.Minute // from begin to callback
	oidcHandoffTTL       = time.Minute      // from callback to exchange
	oidcDiscoveryTimeout = 10 * time.Second
	oidcAppScheme        = "tinysonic://oidc"
)

// oidcFlow is a sign-in started by /api/oidc/begin and not yet called back.
type oidcFlow struct {
	mode     string // signin, link or invite
	client   string // web or app
	userID   int64  // link: the account to attach the identity to
	invite   string // invite: the code, redeemed only after the identity is verified
	username string
	verifier string
	nonce    string
}

// oidcHandoff is the result of a finished callback, waiting for the client to
// exchange it.
type oidcHandoff struct {
	userID int64
	client string
}

// timedStore holds short-lived values by random key. Each value can be taken
// once; expired entries are dropped whenever the store is written.
type timedStore[T any] struct {
	mu sync.Mutex
	m  map[string]timedEntry[T]
}

type timedEntry[T any] struct {
	v       T
	expires time.Time
}

func (t *timedStore[T]) put(key string, v T, ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]timedEntry[T]{}
	}
	now := time.Now()
	for k, e := range t.m {
		if now.After(e.expires) {
			delete(t.m, k)
		}
	}
	t.m[key] = timedEntry[T]{v: v, expires: now.Add(ttl)}
}

func (t *timedStore[T]) take(key string) (T, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[key]
	delete(t.m, key)
	if !ok || time.Now().After(e.expires) {
		var zero T
		return zero, false
	}
	return e.v, true
}

// oidcService holds the provider settings. Discovery is lazy, so an identity
// provider that is down at startup does not stop the server; each request
// retries it until it succeeds.
type oidcService struct {
	cfg     config.OIDC
	baseURL string

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier

	flows    timedStore[oidcFlow]
	handoffs timedStore[oidcHandoff]
}

func newOIDCService(cfg config.OIDC, baseURL string) *oidcService {
	return &oidcService{cfg: cfg, baseURL: strings.TrimRight(baseURL, "/")}
}

// endpoints returns the OAuth2 client and the ID token verifier, running
// discovery on first use.
func (o *oidcService) endpoints(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.oauth != nil {
		return o.oauth, o.verifier, nil
	}
	dctx, cancel := context.WithTimeout(ctx, oidcDiscoveryTimeout)
	defer cancel()
	p, err := oidc.NewProvider(dctx, o.cfg.Issuer)
	if err != nil {
		return nil, nil, err
	}
	o.oauth = &oauth2.Config{
		ClientID:     o.cfg.ClientID,
		ClientSecret: o.cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  o.baseURL + "/api/oidc/callback",
		Scopes:       []string{oidc.ScopeOpenID},
	}
	o.verifier = p.Verifier(&oidc.Config{ClientID: o.cfg.ClientID})
	return o.oauth, o.verifier, nil
}

func (s *State) requireOIDC(w http.ResponseWriter) bool {
	if s.sso == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "single sign-on is not configured")
		return false
	}
	return true
}

// APIOIDCStatus tells the web UI whether to offer single sign-on.
func (s *State) APIOIDCStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, J{"enabled": s.sso != nil})
}

// APIOIDCBegin starts a sign-in and returns the provider's authorization URL.
// Mode signin uses an existing link, link adds an identity to the signed-in
// account, and invite creates an account with the invitation.
func (s *State) APIOIDCBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireOIDC(w) {
		return
	}
	var in struct {
		Mode     string `json:"mode"`
		Client   string `json:"client"`
		Invite   string `json:"invite"`
		Username string `json:"username"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Mode == "" {
		in.Mode = "signin"
	}
	if in.Client == "" {
		in.Client = "web"
	}
	if in.Client != "web" && in.Client != "app" {
		writeAPIError(w, http.StatusBadRequest, "client must be web or app")
		return
	}
	flow := oidcFlow{mode: in.Mode, client: in.Client}
	switch in.Mode {
	case "signin":
	case "link":
		u, ok := s.requireSession(w, r)
		if !ok {
			return
		}
		flow.userID = u.ID
	case "invite":
		_, err := users.InviteRoleFor(r.Context(), s.Pool, in.Invite, in.Username)
		switch {
		case errors.Is(err, users.ErrInviteInvalid):
			time.Sleep(200 * time.Millisecond) // slow down online guessing
			writeAPIError(w, http.StatusForbidden, err.Error())
			return
		case errors.Is(err, users.ErrExists):
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		case errors.Is(err, users.ErrInvalid):
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeAPIError(w, http.StatusInternalServerError, "database error")
			return
		}
		flow.invite, flow.username = in.Invite, in.Username
	default:
		writeAPIError(w, http.StatusBadRequest, "mode must be signin, link or invite")
		return
	}

	oauthCfg, _, err := s.sso.endpoints(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "identity provider is unavailable; try again")
		return
	}
	state := newCeremonyID()
	flow.verifier = oauth2.GenerateVerifier()
	flow.nonce = newCeremonyID()
	s.sso.flows.put(state, flow, oidcFlowTTL)
	authURL := oauthCfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(flow.verifier), oidc.Nonce(flow.nonce))
	writeJSON(w, http.StatusOK, J{"url": authURL})
}

// APIOIDCCallback receives the provider's redirect. The state parameter is
// the only credential here: it is single-use and was issued by begin.
func (s *State) APIOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireOIDC(w) {
		return
	}
	q := r.URL.Query()
	flow, ok := s.sso.flows.take(q.Get("state"))
	if !ok {
		http.Error(w, "sign-in expired or unknown; start again", http.StatusBadRequest)
		return
	}
	if q.Get("error") != "" {
		s.oidcFail(w, r, flow, "the identity provider did not complete the sign-in")
		return
	}
	ctx := r.Context()
	oauthCfg, verifier, err := s.sso.endpoints(ctx)
	if err != nil {
		s.oidcFail(w, r, flow, "the identity provider is unavailable; try again")
		return
	}
	tok, err := oauthCfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(flow.verifier))
	if err != nil {
		s.oidcFail(w, r, flow, "the identity provider rejected the sign-in")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idToken, err := verifier.Verify(ctx, raw)
	if err != nil || idToken.Nonce != flow.nonce {
		s.oidcFail(w, r, flow, "the identity provider's reply could not be verified")
		return
	}
	issuer, subject := s.sso.cfg.Issuer, idToken.Subject

	var userID int64
	switch flow.mode {
	case "link":
		err = users.LinkOIDC(ctx, s.Pool, flow.userID, issuer, subject)
		if errors.Is(err, users.ErrExists) {
			s.oidcFail(w, r, flow, "that identity is already linked to another account")
			return
		}
		if err != nil {
			s.oidcFail(w, r, flow, "database error")
			return
		}
		userID = flow.userID
	case "invite":
		u, err := users.RegisterOIDC(ctx, s.Pool, flow.invite, flow.username, issuer, subject)
		switch {
		case errors.Is(err, users.ErrInviteInvalid):
			s.oidcFail(w, r, flow, "the invitation is no longer valid")
			return
		case errors.Is(err, users.ErrExists):
			s.oidcFail(w, r, flow, "that username or identity is already taken")
			return
		case err != nil:
			s.oidcFail(w, r, flow, "database error")
			return
		}
		userID = u.ID
	default:
		u, err := users.UserByOIDC(ctx, s.Pool, issuer, subject)
		if errors.Is(err, users.ErrNotFound) {
			s.oidcFail(w, r, flow, "no account is linked to this identity; sign in another way and link it on your account page")
			return
		}
		if err != nil || u.Status != users.StatusActive {
			s.oidcFail(w, r, flow, "this account is not active")
			return
		}
		userID = u.ID
	}

	if flow.mode == "link" {
		s.oidcRedirect(w, r, flow, url.Values{"linked": {"1"}})
		return
	}
	code := newCeremonyID()
	s.sso.handoffs.put(code, oidcHandoff{userID: userID, client: flow.client}, oidcHandoffTTL)
	s.oidcRedirect(w, r, flow, url.Values{"code": {code}})
}

// oidcRedirect sends the client back to where the sign-in started. The success
// code goes in the web fragment, which the browser does not send to the server.
// Errors and link results go in the query. The app always gets its scheme.
func (s *State) oidcRedirect(w http.ResponseWriter, r *http.Request, flow oidcFlow, q url.Values) {
	base := s.sso.baseURL
	var target string
	switch {
	case flow.client == "app":
		target = oidcAppScheme + "?" + q.Encode()
	case flow.mode == "link":
		target = base + "/account?" + q.Encode()
	case q.Get("code") != "":
		target = base + "/login#oidc=" + url.QueryEscape(q.Get("code"))
	default:
		target = base + "/login?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// oidcFail reports a failed sign-in to the client that started it.
func (s *State) oidcFail(w http.ResponseWriter, r *http.Request, flow oidcFlow, msg string) {
	s.oidcRedirect(w, r, flow, url.Values{"oidc_error": {msg}})
}

// APIOIDCExchange trades a handoff code for a session (web) or a device API key
// (app). The code is single-use and expires within a minute.
func (s *State) APIOIDCExchange(w http.ResponseWriter, r *http.Request) {
	if !s.requireOIDC(w) {
		return
	}
	var in struct {
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	h, ok := s.sso.handoffs.take(in.Code)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "sign-in expired; start again")
		return
	}
	u, err := users.ByID(r.Context(), s.Pool, h.userID)
	if err != nil || u.Status != users.StatusActive {
		writeAPIError(w, http.StatusUnauthorized, "sign-in failed")
		return
	}
	if h.client == "app" {
		label := strings.TrimSpace(in.Device)
		if label == "" {
			label = "app"
		}
		if len(label) > 64 {
			label = label[:64]
		}
		key, err := users.CreateAPIKey(r.Context(), s.Pool, u.ID, label, 0)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "database error")
			return
		}
		writeJSON(w, http.StatusOK, J{"apiKey": key, "username": u.Username, "server": s.serverURL(r)})
		return
	}
	token, err := users.CreateSession(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"token": token, "username": u.Username, "role": u.Role})
}

// APIListIdentities lists the single sign-on identities linked to the caller.
func (s *State) APIListIdentities(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	ids, err := users.ListOIDCIdentities(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := make([]J, 0, len(ids))
	for _, i := range ids {
		out = append(out, J{"id": i.ID, "issuer": i.Issuer, "subject": i.Subject, "createdAt": i.CreatedAt})
	}
	writeJSON(w, http.StatusOK, J{"identities": out})
}

// APIUnlinkIdentity removes one linked identity, unless it is the account's
// last way to sign in.
func (s *State) APIUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ids, err := users.ListOIDCIdentities(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	if !slices.ContainsFunc(ids, func(i users.OIDCIdentity) bool { return i.ID == id }) {
		writeAPIError(w, http.StatusNotFound, "identity not found")
		return
	}
	if !s.keepsSignIn(w, r, u.ID) {
		return
	}
	if err := users.RemoveOIDCIdentity(r.Context(), s.Pool, u.ID, id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"status": "unlinked"})
}

// keepsSignIn reports whether one sign-in method can be removed. It writes the
// refusal itself when the account would be left with none.
func (s *State) keepsSignIn(w http.ResponseWriter, r *http.Request, userID int64) bool {
	n, err := users.SignInMethods(r.Context(), s.Pool, userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return false
	}
	if n <= 1 {
		writeAPIError(w, http.StatusConflict, "that is your last sign-in method; add a passkey, set a password or link an identity first")
		return false
	}
	return true
}
