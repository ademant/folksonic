package users

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSetRole(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	u, err := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{RoleLibrarian, RoleUser} {
		if err := SetRole(ctx, pool, u.ID, role); err != nil {
			t.Fatalf("SetRole %s: %v", role, err)
		}
		if got, _ := ByID(ctx, pool, u.ID); got.Role != role {
			t.Fatalf("role = %q, want %q", got.Role, role)
		}
	}
	if err := SetRole(ctx, pool, u.ID, "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid role: err = %v, want ErrInvalid", err)
	}
	if err := SetRole(ctx, pool, u.ID+100, RoleUser); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: err = %v, want ErrNotFound", err)
	}
}

func TestRenameKeepsData(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	u, err := Create(ctx, pool, "bobb", "pw", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, pool, "alice", "pw", RoleUser, StatusActive); err != nil {
		t.Fatal(err)
	}
	key, err := CreateAPIKey(ctx, pool, u.ID, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddPasskey(ctx, pool, u.ID, `{"id":"x"}`, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO starred (user_id, id, starred_at) VALUES (?, 'so-1', ?)", u.ID, Now()); err != nil {
		t.Fatal(err)
	}

	if err := Rename(ctx, pool, u.ID, "ALICE"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto existing name (other case): err = %v, want ErrExists", err)
	}
	if err := Rename(ctx, pool, u.ID, "b ob"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid name: err = %v, want ErrInvalid", err)
	}
	if err := Rename(ctx, pool, u.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := Rename(ctx, pool, u.ID, "Bob"); err != nil {
		t.Fatalf("case-only rename: %v", err)
	}

	if _, err := ByName(ctx, pool, "bobb"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old name still resolves: %v", err)
	}
	got, err := LookupAPIKey(ctx, pool, key)
	if err != nil || got.ID != u.ID || got.Username != "Bob" {
		t.Fatalf("key after rename: %+v, %v", got, err)
	}
	pk, err := ListPasskeys(ctx, pool, u.ID)
	if err != nil || len(pk) != 1 {
		t.Fatalf("passkeys after rename: %d, %v", len(pk), err)
	}
	var stars int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM starred WHERE user_id = ?", u.ID).Scan(&stars); err != nil || stars != 1 {
		t.Fatalf("stars after rename: %d, %v", stars, err)
	}
}

func TestListAllAPIKeysAndRevoke(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	bob, _ := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	eve, _ := Create(ctx, pool, "eve", "pw", RoleUser, StatusActive)
	k1, _ := CreateAPIKey(ctx, pool, bob.ID, "phone", 0)
	if _, err := CreateAPIKey(ctx, pool, bob.ID, "tablet", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateAPIKey(ctx, pool, eve.ID, "eve", 0); err != nil {
		t.Fatal(err)
	}

	keys, err := ListAllAPIKeys(ctx, pool, bob.ID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("bob's keys: %d, %v", len(keys), err)
	}
	eveKeys, _ := ListAllAPIKeys(ctx, pool, eve.ID)
	if err := RevokeAPIKey(ctx, pool, bob.ID, eveKeys[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking another user's key: err = %v, want ErrNotFound", err)
	}
	if err := RevokeAPIKey(ctx, pool, bob.ID, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupAPIKey(ctx, pool, k1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key still authenticates: %v", err)
	}
	keys, _ = ListAllAPIKeys(ctx, pool, bob.ID)
	if keys[0].State() != "revoked" || keys[1].State() != "active" {
		t.Fatalf("states = %s, %s", keys[0].State(), keys[1].State())
	}
}

func TestListSummaries(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	pwOnly, _ := Create(ctx, pool, "a-password", "pw", RoleUser, StatusActive)
	code, _, err := CreateInvite(ctx, pool, RoleUser, "", "", InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	pkOnly, err := RegisterPasskey(ctx, pool, code, "b-passkey", []byte("h"), `{"id":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	mixed, _ := Create(ctx, pool, "c-mixed", "pw", RoleLibrarian, StatusActive)
	_ = AddPasskey(ctx, pool, mixed.ID, `{"id":"y"}`, "")
	_ = AddPasskey(ctx, pool, mixed.ID, `{"id":"z"}`, "")
	_, _ = CreateAPIKey(ctx, pool, pkOnly.ID, "phone", 0)
	revoked, _ := CreateAPIKey(ctx, pool, pkOnly.ID, "old", 0)
	_, _ = pool.ExecContext(ctx, "UPDATE api_keys SET revoked_at = ? WHERE key_hash = ?", Now(), hashToken(revoked))
	_, _ = pool.ExecContext(ctx, `INSERT INTO api_keys (user_id, label, prefix, key_hash, created_at, expires_at)
		VALUES (?, 'expired', 'tsk_x', 'h-expired', ?, '2000-01-01T00:00:00Z')`, pkOnly.ID, Now())

	all, err := ListSummaries(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d summaries", len(all))
	}
	type row struct {
		id       int64
		pw       bool
		passkeys int
		keys     int
	}
	want := []row{{pwOnly.ID, true, 0, 0}, {pkOnly.ID, false, 1, 1}, {mixed.ID, true, 2, 0}}
	for i, w := range want {
		s := all[i]
		if (row{s.ID, s.HasPassword, s.Passkeys, s.ActiveKeys}) != w {
			t.Errorf("%s: got pw=%v passkeys=%d keys=%d, want %+v", s.Username, s.HasPassword, s.Passkeys, s.ActiveKeys, w)
		}
	}
}
