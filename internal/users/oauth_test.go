package users

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// grantFixture is a database with bob and a device that asks for tokens.
func grantFixture(t *testing.T) (context.Context, *sql.DB, int64, time.Time) {
	t.Helper()
	pool, err := db.Init(filepath.Join(t.TempDir(), "grant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	ctx := context.Background()
	u, err := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, pool, u.ID, time.Now()
}

// approvedGrant runs the device flow up to tokens, for the key jkt.
func approvedGrant(t *testing.T, ctx context.Context, pool *sql.DB, userID int64, now time.Time, jkt string) (DeviceAuth, Tokens) {
	t.Helper()
	auth, err := CreateDeviceAuth(ctx, pool, "phone", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApproveUserCode(ctx, pool, userID, auth.UserCode, now); err != nil {
		t.Fatal(err)
	}
	toks, err := ExchangeDeviceCode(ctx, pool, auth.DeviceCode, jkt, now.Add(DeviceInterval*2))
	if err != nil {
		t.Fatal(err)
	}
	return auth, toks
}

// The whole grant: pending, approved, tokens that work for their key only,
// rotation, and reuse of a rotated refresh token revoking the grant.
func TestDeviceGrantLifecycle(t *testing.T) {
	ctx, pool, bob, now := grantFixture(t)
	auth, err := CreateDeviceAuth(ctx, pool, "phone", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(auth.UserCode, "-") || len(auth.UserCode) != 9 {
		t.Fatalf("user code %q is not XXXX-XXXX", auth.UserCode)
	}
	if _, err := ExchangeDeviceCode(ctx, pool, auth.DeviceCode, "key-a", now); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("before approval: %v, want authorization_pending", err)
	}
	// The user types the code without its dash and in lower case.
	if err := ApproveUserCode(ctx, pool, bob, strings.ToLower(strings.ReplaceAll(auth.UserCode, "-", "")), now); err != nil {
		t.Fatalf("approve: %v", err)
	}
	toks, err := ExchangeDeviceCode(ctx, pool, auth.DeviceCode, "key-a", now.Add(DeviceInterval*2))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if toks.ExpiresIn != int(AccessTokenTTL.Seconds()) {
		t.Fatalf("expires_in = %d", toks.ExpiresIn)
	}
	// The device code is used up.
	if _, err := ExchangeDeviceCode(ctx, pool, auth.DeviceCode, "key-a", now.Add(time.Hour)); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("second exchange: %v, want invalid_grant", err)
	}

	u, err := LookupAccessToken(ctx, pool, toks.AccessToken, "key-a", now)
	if err != nil || u.ID != bob {
		t.Fatalf("access token: %v, user %v", err, u.ID)
	}
	if _, err := LookupAccessToken(ctx, pool, toks.AccessToken, "key-b", now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("another key's proof: %v, want token_invalid", err)
	}

	next, err := RefreshTokens(ctx, pool, toks.RefreshToken, "key-a", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if next.RefreshToken == toks.RefreshToken || next.AccessToken == toks.AccessToken {
		t.Fatal("refresh did not rotate both tokens")
	}
	// The old refresh token is now a reuse: the grant and its tokens are revoked.
	if _, err := RefreshTokens(ctx, pool, toks.RefreshToken, "key-a", now.Add(2*time.Minute)); !errors.Is(err, ErrRefreshReused) {
		t.Fatalf("reused refresh: %v, want reuse", err)
	}
	if _, err := RefreshTokens(ctx, pool, next.RefreshToken, "key-a", now.Add(3*time.Minute)); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("refresh after reuse: %v, want invalid_grant", err)
	}
	if _, err := LookupAccessToken(ctx, pool, next.AccessToken, "key-a", now.Add(3*time.Minute)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("access after reuse: %v, want token_invalid", err)
	}
}

// An access token is good for AccessTokenTTL and no longer.
func TestAccessTokenExpires(t *testing.T) {
	ctx, pool, bob, now := grantFixture(t)
	_, toks := approvedGrant(t, ctx, pool, bob, now, "key-a")
	// The life starts when the grant is exchanged, which approvedGrant does at
	// now + 2 x DeviceInterval.
	issued := now.Add(DeviceInterval * 2)
	if _, err := LookupAccessToken(ctx, pool, toks.AccessToken, "key-a", issued.Add(AccessTokenTTL-time.Second)); err != nil {
		t.Fatalf("inside its life: %v", err)
	}
	if _, err := LookupAccessToken(ctx, pool, toks.AccessToken, "key-a", issued.Add(AccessTokenTTL+time.Second)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("after its life: %v, want token_expired", err)
	}
	// The refresh token still works: that is how the app gets a new one.
	if _, err := RefreshTokens(ctx, pool, toks.RefreshToken, "key-a", issued.Add(AccessTokenTTL+time.Second)); err != nil {
		t.Fatalf("refresh after the access token expired: %v", err)
	}
}

// Polling faster than the interval gets slow_down; a code past its life expires.
func TestPollingAndExpiry(t *testing.T) {
	ctx, pool, bob, now := grantFixture(t)
	auth, err := CreateDeviceAuth(ctx, pool, "phone", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExchangeDeviceCode(ctx, pool, auth.DeviceCode, "key-a", now); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("first poll: %v", err)
	}
	if _, err := ExchangeDeviceCode(ctx, pool, auth.DeviceCode, "key-a", now.Add(time.Second)); !errors.Is(err, ErrSlowDown) {
		t.Fatalf("fast poll: %v, want slow_down", err)
	}
	if _, err := ExchangeDeviceCode(ctx, pool, auth.DeviceCode, "key-a", now.Add(DeviceCodeTTL+time.Second)); !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("after its life: %v, want expired_token", err)
	}
	// An expired code cannot be approved any more.
	if err := ApproveUserCode(ctx, pool, bob, auth.UserCode, now.Add(DeviceCodeTTL+time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approve after expiry: %v", err)
	}
}

// A refresh token that has gone unused for RefreshIdleTTL is no longer good.
func TestRefreshIdleExpiry(t *testing.T) {
	ctx, pool, bob, now := grantFixture(t)
	_, toks := approvedGrant(t, ctx, pool, bob, now, "key-a")
	if _, err := RefreshTokens(ctx, pool, toks.RefreshToken, "key-a", now.Add(RefreshIdleTTL+time.Hour)); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("idle refresh: %v, want invalid_grant", err)
	}
}

// Revoking a user's grants ends their access tokens and refresh tokens.
func TestRevokeUserGrants(t *testing.T) {
	ctx, pool, bob, now := grantFixture(t)
	_, toks := approvedGrant(t, ctx, pool, bob, now, "key-a")
	n, err := RevokeUserGrants(ctx, pool, bob, now)
	if err != nil || n != 1 {
		t.Fatalf("revoked %d grants, %v; want 1", n, err)
	}
	if _, err := LookupAccessToken(ctx, pool, toks.AccessToken, "key-a", now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("access after revoke: %v", err)
	}
	if _, err := RefreshTokens(ctx, pool, toks.RefreshToken, "key-a", now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("refresh after revoke: %v", err)
	}
}

// Unknown tokens and codes are refused and are not known to the replay guard.
func TestUnknownTokensAreNotKnown(t *testing.T) {
	ctx, pool, _, now := grantFixture(t)
	if ok, _ := DeviceCodeKnown(ctx, pool, "tsd_nothing", now); ok {
		t.Fatal("unknown device code is known")
	}
	if ok, _ := GrantKnown(ctx, pool, "tsr_nothing"); ok {
		t.Fatal("unknown refresh token is known")
	}
	if _, err := RefreshTokens(ctx, pool, "tsr_nothing", "key-a", now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("unknown refresh: %v", err)
	}
}
