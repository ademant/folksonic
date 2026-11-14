package users

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// Keys expire by default, the lifetime is kept with the key, and a key in use
// near its end is renewed to a full lifetime.
func TestKeysExpireByDefaultAndRenew(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	bob, _ := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	key, err := CreateAPIKey(ctx, pool, bob.ID, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	var expires string
	var ttl int64
	if err := pool.QueryRowContext(ctx, "SELECT expires_at, ttl_seconds FROM api_keys WHERE key_hash = ?", hashToken(key)).Scan(&expires, &ttl); err != nil {
		t.Fatal(err)
	}
	if ttl != int64(KeyTTL.Seconds()) {
		t.Fatalf("ttl_seconds = %d, want %d", ttl, int64(KeyTTL.Seconds()))
	}
	if want := ts(time.Now().Add(KeyTTL)); expires[:16] != want[:16] {
		t.Fatalf("expires_at = %s, want about %s", expires, want)
	}

	// Plenty of time left: a use does not write a new expiry.
	if _, _, err := LookupAPIKeyInfo(ctx, pool, key); err != nil {
		t.Fatal(err)
	}
	var after string
	_ = pool.QueryRowContext(ctx, "SELECT expires_at FROM api_keys WHERE key_hash = ?", hashToken(key)).Scan(&after)
	if after != expires {
		t.Fatalf("expiry changed from %s to %s with 365 days left", expires, after)
	}

	// Ten days left: the next use renews it to a full lifetime.
	near := ts(time.Now().Add(10 * 24 * time.Hour))
	if _, err := pool.ExecContext(ctx, "UPDATE api_keys SET expires_at = ? WHERE key_hash = ?", near, hashToken(key)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LookupAPIKeyInfo(ctx, pool, key); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRowContext(ctx, "SELECT expires_at FROM api_keys WHERE key_hash = ?", hashToken(key)).Scan(&after)
	if after <= near || after < ts(time.Now().Add(KeyTTL-time.Minute)) {
		t.Fatalf("expiry %s was not renewed to a full lifetime", after)
	}
}

// A key with its own lifetime renews to that lifetime, not to the default.
func TestRenewalKeepsTheKeysOwnLifetime(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	bob, _ := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	key, _ := CreateAPIKey(ctx, pool, bob.ID, "short", 7*24*time.Hour)
	if _, err := pool.ExecContext(ctx, "UPDATE api_keys SET expires_at = ? WHERE key_hash = ?", ts(time.Now().Add(time.Hour)), hashToken(key)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LookupAPIKeyInfo(ctx, pool, key); err != nil {
		t.Fatal(err)
	}
	var expires string
	_ = pool.QueryRowContext(ctx, "SELECT expires_at FROM api_keys WHERE key_hash = ?", hashToken(key)).Scan(&expires)
	if expires > ts(time.Now().Add(8*24*time.Hour)) {
		t.Fatalf("a 7-day key was renewed past 7 days: %s", expires)
	}
}

func TestIdleKeysAreRevokedWithReason(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	bob, _ := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	idle, _ := CreateAPIKey(ctx, pool, bob.ID, "old phone", 0)
	fresh, _ := CreateAPIKey(ctx, pool, bob.ID, "new phone", 0)
	long := ts(time.Now().Add(-200 * 24 * time.Hour))
	if _, err := pool.ExecContext(ctx, "UPDATE api_keys SET last_used_at = ? WHERE key_hash = ?", long, hashToken(idle)); err != nil {
		t.Fatal(err)
	}

	revoked, err := RevokeIdleKeys(ctx, pool, KeyIdle)
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0].Label != "old phone" {
		t.Fatalf("revoked %+v, want only the idle key", revoked)
	}
	if _, _, err := LookupAPIKeyInfo(ctx, pool, idle); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idle key still authenticates: %v", err)
	}
	if reason, _, _ := APIKeyProblem(ctx, pool, idle); reason != CodeKeyIdle {
		t.Fatalf("reason = %q, want %q", reason, CodeKeyIdle)
	}
	if _, _, err := LookupAPIKeyInfo(ctx, pool, fresh); err != nil {
		t.Fatalf("fresh key refused: %v", err)
	}
	// A manually revoked key reports key_revoked, not key_idle.
	if err := RevokeAPIKey(ctx, pool, bob.ID, mustKeyID(t, pool, bob.ID, "new phone")); err != nil {
		t.Fatal(err)
	}
	if reason, _, _ := APIKeyProblem(ctx, pool, fresh); reason != CodeKeyRevoked {
		t.Fatalf("reason = %q, want %q", reason, CodeKeyRevoked)
	}
}

func mustKeyID(t *testing.T, pool *sql.DB, userID int64, label string) int64 {
	t.Helper()
	keys, err := ListAllAPIKeys(context.Background(), pool, userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Label == label {
			return k.ID
		}
	}
	t.Fatalf("no key %q", label)
	return 0
}

// Keys made before lifetimes existed get one, from their last use (or their
// creation); a second run changes nothing.
func TestBackfillGivesExistingKeysAnExpiry(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	bob, _ := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	used := ts(time.Now().Add(-100 * 24 * time.Hour))
	created := ts(time.Now().Add(-300 * 24 * time.Hour))
	_, _ = pool.ExecContext(ctx, `INSERT INTO api_keys (user_id, label, prefix, key_hash, created_at, last_used_at)
		VALUES (?, 'used', 'tsk_u', 'h-used', ?, ?)`, bob.ID, created, used)
	_, _ = pool.ExecContext(ctx, `INSERT INTO api_keys (user_id, label, prefix, key_hash, created_at)
		VALUES (?, 'unused', 'tsk_n', 'h-unused', ?)`, bob.ID, created)

	if err := BackfillKeyLifetimes(ctx, pool, KeyTTL); err != nil {
		t.Fatal(err)
	}
	var usedExp, unusedExp string
	_ = pool.QueryRowContext(ctx, "SELECT expires_at FROM api_keys WHERE label='used'").Scan(&usedExp)
	_ = pool.QueryRowContext(ctx, "SELECT expires_at FROM api_keys WHERE label='unused'").Scan(&unusedExp)
	if usedExp[:16] != ts(mustParse(t, used).Add(KeyTTL))[:16] {
		t.Fatalf("used key expires %s, want last use + lifetime", usedExp)
	}
	if unusedExp[:16] != ts(mustParse(t, created).Add(KeyTTL))[:16] {
		t.Fatalf("unused key expires %s, want creation + lifetime", unusedExp)
	}

	if err := BackfillKeyLifetimes(ctx, pool, time.Hour); err != nil {
		t.Fatal(err)
	}
	var again string
	_ = pool.QueryRowContext(ctx, "SELECT expires_at FROM api_keys WHERE label='used'").Scan(&again)
	if again != usedExp {
		t.Fatalf("second backfill changed the expiry: %s -> %s", usedExp, again)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPairingTokensLastFifteenMinutes(t *testing.T) {
	if PairingTTL != 15*time.Minute {
		t.Fatalf("PairingTTL = %s, want 15m", PairingTTL)
	}
}

func TestExpiredPairingTokenIsDistinguished(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	bob, _ := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	tok, err := CreatePairingToken(ctx, pool, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE pairing_tokens SET expires_at = ?", ts(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	_, _, err = RedeemPairingToken(ctx, pool, tok, "phone")
	if !errors.Is(err, ErrPairingExpired) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrPairingExpired (which matches ErrNotFound)", err)
	}
}

func TestExpiredInviteIsDistinguished(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	code, _, err := CreateInvite(ctx, pool, RoleUser, "", "", InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE invites SET expires_at = ?", ts(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	_, err = RegisterWithInvite(ctx, pool, code, "carol", "longpass1")
	if !errors.Is(err, ErrInviteExpired) || !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("err = %v, want ErrInviteExpired (which matches ErrInviteInvalid)", err)
	}
	_, err = RegisterWithInvite(ctx, pool, "NOTAREALCODE", "dave", "longpass1")
	if errors.Is(err, ErrInviteExpired) || !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("unknown code: err = %v, want plain ErrInviteInvalid", err)
	}
}
