package users

import (
	"context"
	"testing"
	"time"
)

// last_used_at is written at most once a minute per key (#112).
func TestAPIKeyLastUsedWrittenOncePerMinute(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	u, err := Create(ctx, db, "erin", "long-enough-pass", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := CreateAPIKey(ctx, db, u.ID, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	lastUsed := func() string {
		var v string
		if err := db.QueryRowContext(ctx, "SELECT COALESCE(last_used_at, '') FROM api_keys WHERE user_id = ?", u.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	if _, _, err := LookupAPIKeyInfo(ctx, db, plain); err != nil {
		t.Fatal(err)
	}
	first := lastUsed()
	if first == "" {
		t.Fatal("first use did not record last_used_at")
	}
	// Within the minute a second use does not write.
	time.Sleep(10 * time.Millisecond)
	if _, _, err := LookupAPIKeyInfo(ctx, db, plain); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); got != first {
		t.Fatalf("a use within the minute rewrote last_used_at: %q -> %q", first, got)
	}
	// Once the stored time is over a minute old, the next use writes.
	old := ts(time.Now().Add(-2 * time.Minute))
	if _, err := db.ExecContext(ctx, "UPDATE api_keys SET last_used_at = ? WHERE user_id = ?", old, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LookupAPIKeyInfo(ctx, db, plain); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); got == old {
		t.Fatal("a use after a minute did not update last_used_at")
	}
}
