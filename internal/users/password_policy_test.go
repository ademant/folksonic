package users

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Passwords a person chooses must be 8 to 1024 bytes (#113).
func TestPasswordLengthPolicy(t *testing.T) {
	for _, n := range []int{7, 1025} {
		if err := CheckPassword(strings.Repeat("p", n)); !errors.Is(err, ErrInvalid) {
			t.Errorf("length %d: err %v, want ErrInvalid", n, err)
		}
	}
	for _, n := range []int{8, 1024} {
		if err := CheckPassword(strings.Repeat("p", n)); err != nil {
			t.Errorf("length %d refused: %v", n, err)
		}
	}
}

// Bootstrap keeps accepting the config password whatever its length.
func TestBootstrapIgnoresLengthRule(t *testing.T) {
	db := testDB(t)
	ok, err := Bootstrap(context.Background(), db, "root", "x")
	if err != nil || !ok {
		t.Fatalf("bootstrap with a short config password: ok=%v err=%v", ok, err)
	}
}

// Changing the password ends the user's sessions but keeps their API keys.
func TestSetPasswordEndsSessionsKeepsKeys(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	u, err := Create(ctx, db, "carol", "original-pass", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSession(ctx, db, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSession(ctx, db, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateAPIKey(ctx, db, u.ID, "phone", 0); err != nil {
		t.Fatal(err)
	}

	ended, err := SetPassword(ctx, db, u.ID, "new-password")
	if err != nil {
		t.Fatal(err)
	}
	if ended != 2 {
		t.Fatalf("ended %d sessions, want 2", ended)
	}
	var sessions, keys int
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE user_id = ?", u.ID).Scan(&sessions)
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_keys WHERE user_id = ?", u.ID).Scan(&keys)
	if sessions != 0 || keys != 1 {
		t.Fatalf("after change: %d sessions, %d keys; want 0 sessions and 1 key", sessions, keys)
	}
}

// A refused length leaves the old password and the sessions in place.
func TestRefusedSetPasswordChangesNothing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	u, _ := Create(ctx, db, "dave", "original-pass", RoleUser, StatusActive)
	if _, err := CreateSession(ctx, db, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPassword(ctx, db, u.ID, "short"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err %v, want ErrInvalid", err)
	}
	var sessions int
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE user_id = ?", u.ID).Scan(&sessions)
	if sessions != 1 {
		t.Fatalf("a refused change ended sessions: %d left", sessions)
	}
}
