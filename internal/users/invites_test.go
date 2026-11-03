package users

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Init(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func TestInviteCodeFormat(t *testing.T) {
	code, err := newInviteCode()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^inv(-[A-Z2-7]{4}){4}$`).MatchString(code) {
		t.Fatalf("code %q does not match inv-XXXX-XXXX-XXXX-XXXX", code)
	}
}

func TestNormaliseInvite(t *testing.T) {
	const body = "ABCDEFGHIJKLMN23"
	for _, in := range []string{
		"inv-ABCD-EFGH-IJKL-MN23",
		"INV-abcd-efgh-ijkl-mn23",
		"abcdefghijklmn23",
		" abcd efgh ijkl mn23 ",
	} {
		got, ok := normaliseInvite(in)
		if !ok || got != body {
			t.Errorf("normaliseInvite(%q) = %q, %v; want %q", in, got, ok, body)
		}
	}
	for _, in := range []string{"", "inv-ABCD", "ABCDEFGHIJKLMN01", "ABCDEFGHIJKLMN2345"} {
		if _, ok := normaliseInvite(in); ok {
			t.Errorf("normaliseInvite(%q) accepted", in)
		}
	}
}

func TestInviteRegistersActiveAccountOnce(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	code, inv, err := CreateInvite(ctx, pool, RoleUser, "", "for bob", InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	if inv.State() != "open" || inv.Note != "for bob" {
		t.Fatalf("new invite: state %q, note %q", inv.State(), inv.Note)
	}

	// Typed loosely: lower case, no dashes.
	bob, err := RegisterWithInvite(ctx, pool, strings.ToLower(strings.ReplaceAll(code, "-", "")), "bob", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if bob.Status != StatusActive || bob.Role != RoleUser {
		t.Fatalf("bob is %s/%s, want active/user", bob.Status, bob.Role)
	}
	if _, err := RegisterWithInvite(ctx, pool, code, "carol", "pw"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("second use: %v, want ErrInviteInvalid", err)
	}

	all, err := ListInvites(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].State() != "used" || all[0].UsedBy.String != "bob" {
		t.Fatalf("after use: %+v", all)
	}
	if err := RevokeInvite(ctx, pool, inv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a used invite: %v, want ErrNotFound", err)
	}
}

func TestAdminInvite(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	code, _, err := CreateInvite(ctx, pool, RoleLibrarian, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := RegisterWithInvite(ctx, pool, code, "root", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleLibrarian {
		t.Fatalf("role %q, want admin", u.Role)
	}
}

func TestInviteRejectedWhenExpiredOrRevoked(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)

	expired, inv, err := CreateInvite(ctx, pool, RoleUser, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec("UPDATE invites SET expires_at = ? WHERE id = ?", ts(time.Now().Add(-time.Minute)), inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterWithInvite(ctx, pool, expired, "bob", "pw"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired: %v, want ErrInviteInvalid", err)
	}

	revoked, inv, err := CreateInvite(ctx, pool, RoleUser, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeInvite(ctx, pool, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterWithInvite(ctx, pool, revoked, "bob", "pw"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("revoked: %v, want ErrInviteInvalid", err)
	}
	if _, err := ByName(ctx, pool, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatal("account created from a rejected invite")
	}
}

// A taken username rolls the transaction back, so the invite stays usable.
func TestInviteKeptWhenUsernameTaken(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	if _, err := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive); err != nil {
		t.Fatal(err)
	}
	code, _, err := CreateInvite(ctx, pool, RoleUser, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterWithInvite(ctx, pool, code, "BOB", "pw"); !errors.Is(err, ErrExists) {
		t.Fatalf("taken name: %v, want ErrExists", err)
	}
	if _, err := RegisterWithInvite(ctx, pool, code, "carol", "pw"); err != nil {
		t.Fatalf("invite unusable after a failed attempt: %v", err)
	}
}

func TestInviteConcurrentRedeemOnce(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	code, _, err := CreateInvite(ctx, pool, RoleUser, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = RegisterWithInvite(ctx, pool, code, "user"+string(rune('a'+i)), "pw")
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrInviteInvalid):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d registrations succeeded, want exactly 1", ok)
	}
}

// A passkey-only account needs its passkey: no account may be created without a
// verified credential.
func TestRegisterPasskeyRequiresCredential(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	code, _, err := CreateInvite(ctx, pool, RoleUser, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterPasskey(ctx, pool, code, "ivan", []byte("0123456789abcdef"), ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("account created without a passkey: %v", err)
	}
	if _, err := InviteRoleFor(ctx, pool, code, "ivan"); err != nil {
		t.Fatalf("invite consumed by a rejected request: %v", err)
	}
}

// A passkey-only account has a NULL password. Its API keys, including those
// from device pairing, must still authenticate.
func TestAPIKeyForPasswordlessAccount(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	code, _, err := CreateInvite(ctx, pool, RoleUser, "", "", InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	u, err := RegisterPasskey(ctx, pool, code, "bob", []byte("handle"), `{"id":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	token, err := CreatePairingToken(ctx, pool, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := RedeemPairingToken(ctx, pool, token, "phone")
	if err != nil {
		t.Fatal(err)
	}
	got, err := LookupAPIKey(ctx, pool, key)
	if err != nil {
		t.Fatalf("LookupAPIKey for passwordless account: %v", err)
	}
	if got.ID != u.ID || got.Password != "" || string(got.WebAuthnHandle) != "handle" {
		t.Fatalf("LookupAPIKey returned %+v", got)
	}
}
