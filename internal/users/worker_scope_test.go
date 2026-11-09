package users

import (
	"context"
	"testing"
)

func TestWorkerKeysCarryTheirScope(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	u, err := Create(ctx, pool, "worker", "pw", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	full, err := CreateAPIKey(ctx, pool, u.ID, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := CreateScopedAPIKey(ctx, pool, u.ID, "worker: desk", ScopeWorker, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, info, err := LookupAPIKeyInfo(ctx, pool, full); err != nil || info.Scope != "" {
		t.Errorf("full key: %+v %v", info, err)
	}
	if _, info, err := LookupAPIKeyInfo(ctx, pool, worker); err != nil || info.Scope != ScopeWorker || info.ID == 0 {
		t.Errorf("worker key: %+v %v", info, err)
	}
	keys, err := ListAllAPIKeys(ctx, pool, u.ID)
	if err != nil || len(keys) != 2 || keys[0].Scope != "" || keys[1].Scope != ScopeWorker {
		t.Errorf("listed scopes: %+v %v", keys, err)
	}
}

func TestPairingTokenIssuesScopedKey(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	u, err := Create(ctx, pool, "worker", "pw", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	token, err := CreateScopedPairingToken(ctx, pool, u.ID, ScopeWorker)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := RedeemPairingToken(ctx, pool, token, "desk")
	if err != nil {
		t.Fatal(err)
	}
	if _, info, err := LookupAPIKeyInfo(ctx, pool, key); err != nil || info.Scope != ScopeWorker {
		t.Errorf("redeemed key: %+v %v", info, err)
	}
	if _, _, err := RedeemPairingToken(ctx, pool, token, "again"); err == nil {
		t.Error("a pairing token was redeemed twice")
	}
	// A plain token still gives a full key.
	plain, err := CreatePairingToken(ctx, pool, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err = RedeemPairingToken(ctx, pool, plain, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if _, info, _ := LookupAPIKeyInfo(ctx, pool, key); info.Scope != "" {
		t.Errorf("plain pairing gave scope %q", info.Scope)
	}
}
