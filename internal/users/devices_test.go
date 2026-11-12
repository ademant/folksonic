package users

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/db"
)

func deviceFixture(t *testing.T) (context.Context, *sql.DB, int64) {
	t.Helper()
	pool, err := db.Init(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	ctx := context.Background()
	u, err := Create(ctx, pool, "bob", "pw", RoleUser, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, pool, u.ID
}

func TestPairingWaitsForApprovalAndGrantsADevice(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	tok, err := CreatePairingToken(ctx, pool, bob)
	if err != nil {
		t.Fatal(err)
	}

	p, err := CompleteDevicePairing(ctx, pool, tok, "Pixel", "jkt-1", `{"kty":"EC"}`, now)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if state, _ := DevicePairingState(ctx, pool, "jkt-1", now); state != "pending" {
		t.Fatalf("state %q, want pending", state)
	}
	if _, _, err := DeviceByKey(ctx, pool, "jkt-1", now); err != ErrNotFound {
		t.Fatalf("pending key authenticates: %v", err)
	}
	list, err := PendingPairings(ctx, pool, bob, now)
	if err != nil || len(list) != 1 || list[0].Name != "Pixel" {
		t.Fatalf("pending list %v, %v", list, err)
	}

	dev, err := ApproveDevicePairing(ctx, pool, bob, p.ID, now)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, owner, err := DeviceByKey(ctx, pool, "jkt-1", now)
	if err != nil || got.ID != dev.ID || owner.ID != bob {
		t.Fatalf("approved key: %v %v %v", got, owner, err)
	}
	if list, _ := PendingPairings(ctx, pool, bob, now); len(list) != 0 {
		t.Fatal("approved pairing still pending")
	}
}

func TestPairingTokenIsSingleUse(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	if _, err := CompleteDevicePairing(ctx, pool, tok, "A", "jkt-a", "{}", now); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteDevicePairing(ctx, pool, tok, "B", "jkt-b", "{}", now); err != ErrNotFound {
		t.Fatalf("second use: %v", err)
	}
}

func TestApprovalAfterTheWindowIsRefused(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	p, err := CompleteDevicePairing(ctx, pool, tok, "Late", "jkt-late", "{}", now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(PairingTTL + time.Minute)
	if _, err := ApproveDevicePairing(ctx, pool, bob, p.ID, later); err != ErrPairingExpired {
		t.Fatalf("approve after window: %v", err)
	}
	// The refused approval discards the expired pairing.
	if state, _ := DevicePairingState(ctx, pool, "jkt-late", later); state != "" {
		t.Fatalf("expired pairing still recorded as %q", state)
	}
}

func TestExpiredTokenCannotPair(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	if _, err := CompleteDevicePairing(ctx, pool, tok, "Old", "jkt-old", "{}", now.Add(PairingTTL+time.Minute)); err != ErrPairingExpired {
		t.Fatalf("expired token: %v", err)
	}
}

func TestKeyCanPairOnce(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	t1, _ := CreatePairingToken(ctx, pool, bob)
	t2, _ := CreatePairingToken(ctx, pool, bob)
	if _, err := CompleteDevicePairing(ctx, pool, t1, "One", "same-key", "{}", now); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteDevicePairing(ctx, pool, t2, "Two", "same-key", "{}", now); err != ErrDeviceKeyInUse {
		t.Fatalf("second pairing of one key: %v", err)
	}
}

func TestRemovingADeviceStopsItsKey(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	p, _ := CompleteDevicePairing(ctx, pool, tok, "Phone", "jkt-phone", "{}", now)
	dev, err := ApproveDevicePairing(ctx, pool, bob, p.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveDevice(ctx, pool, bob+1, dev.ID); err != ErrNotFound {
		t.Fatalf("another account removed the device: %v", err)
	}
	if err := RemoveDevice(ctx, pool, bob, dev.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DeviceByKey(ctx, pool, "jkt-phone", now); err != ErrNotFound {
		t.Fatalf("removed device still authenticates: %v", err)
	}
}

func TestDisabledOwnerDoesNotAuthenticate(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	p, _ := CompleteDevicePairing(ctx, pool, tok, "Phone", "jkt-dis", "{}", now)
	if _, err := ApproveDevicePairing(ctx, pool, bob, p.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec("UPDATE users SET status = 'disabled' WHERE id = ?", bob); err != nil {
		t.Fatal(err)
	}
	_, owner, err := DeviceByKey(ctx, pool, "jkt-dis", now)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Status == StatusActive {
		t.Fatal("owner should be reported disabled to the caller")
	}
}

func TestProofJTIIsRefusedTheSecondTime(t *testing.T) {
	ctx, pool, _ := deviceFixture(t)
	now := time.Now().UTC()
	if err := RecordProofJTI(ctx, pool, "abc", now); err != nil {
		t.Fatal(err)
	}
	if err := RecordProofJTI(ctx, pool, "abc", now); err != ErrProofReplayed {
		t.Fatalf("replay: %v", err)
	}
	// Once outside the window the entry is pruned, so the jti may be used again.
	if err := RecordProofJTI(ctx, pool, "abc", now.Add(ProofWindow+time.Minute)); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}
