package users

import (
	"context"
	"testing"
	"time"
)

// Worker keys and worker pairing tokens were removed (#105). A leftover
// scoped pairing token gives no key, and pairing hands out full keys only.
func TestScopedPairingTokensAreRefused(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	u, err := Create(ctx, pool, "worker", "pw", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	const plain = "tsp_leftover-worker-token"
	if _, err := pool.Exec("INSERT INTO pairing_tokens (token_hash, user_id, expires_at, scope) VALUES (?, ?, ?, 'worker')",
		hashToken(plain), u.ID, ts(time.Now().Add(PairingTTL))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RedeemPairingToken(ctx, pool, plain, "desk"); err == nil {
		t.Error("a scoped pairing token was redeemed")
	}
	token, err := CreatePairingToken(ctx, pool, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := RedeemPairingToken(ctx, pool, token, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if _, info, err := LookupAPIKeyInfo(ctx, pool, key); err != nil || info.Scope != "" {
		t.Errorf("redeemed key: %+v %v", info, err)
	}
}
