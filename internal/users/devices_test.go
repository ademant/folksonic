package users

import (
	"context"
	"database/sql"
	"fmt"
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

// pairNow completes a pairing for jkt and confirms it on both sides.
func pairNow(t *testing.T, ctx context.Context, pool *sql.DB, user int64, jkt string, now time.Time) Device {
	t.Helper()
	tok, err := CreatePairingToken(ctx, pool, user)
	if err != nil {
		t.Fatal(err)
	}
	p, err := CompleteDevicePairing(ctx, pool, tok, "Phone", jkt, "203.0.113.5", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := ConfirmPairingByOwner(ctx, pool, user, p.ID, true, now); err != nil || done {
		t.Fatalf("owner confirm: done=%v %v", done, err)
	}
	dev, done, err := ConfirmPairingByDevice(ctx, pool, jkt, true, now)
	if err != nil || !done {
		t.Fatalf("device confirm: done=%v %v", done, err)
	}
	return dev
}

// The code is part of the contract with the app, which computes it on its own
// side: SHA-256(hex(SHA-256(token)) ":" jkt), first four bytes mod 10^6.
func TestVerificationCodeVector(t *testing.T) {
	tokenHash := hashToken("tsp_example-pairing-token")
	if tokenHash != "57286dffd652ad012ec3261504463fccf379961998cc6d63bdcb362f6aabc160" {
		t.Fatalf("token hash %s", tokenHash)
	}
	if got := VerificationCode(tokenHash, "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"); got != "446284" {
		t.Fatalf("code %s, want 446284", got)
	}
	if VerificationCode(tokenHash, "other-key") == "446284" {
		t.Fatal("a different key gave the same code")
	}
}

func TestPairingNeedsBothConfirmationsInEitherOrder(t *testing.T) {
	for _, ownerFirst := range []bool{true, false} {
		ctx, pool, bob := deviceFixture(t)
		now := time.Now().UTC().Truncate(time.Second)
		tok, err := CreatePairingToken(ctx, pool, bob)
		if err != nil {
			t.Fatal(err)
		}
		p, err := CompleteDevicePairing(ctx, pool, tok, "Pixel", "jkt-1", "203.0.113.5", now)
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
		if p.Code != VerificationCode(hashToken(tok), "jkt-1") || len(p.Code) != 6 {
			t.Fatalf("code %q", p.Code)
		}
		if st, _ := DevicePairingState(ctx, pool, "jkt-1", now); st.State != PairingPending || st.WaitingFor != "both" {
			t.Fatalf("state %+v", st)
		}
		list, err := PendingPairings(ctx, pool, bob, now)
		if err != nil || len(list) != 1 || list[0].Code != p.Code || list[0].ClientIP != "203.0.113.5" {
			t.Fatalf("pending list %+v, %v", list, err)
		}

		var dev Device
		var done bool
		if ownerFirst {
			if _, done, err = ConfirmPairingByOwner(ctx, pool, bob, p.ID, true, now); err != nil || done {
				t.Fatalf("owner first: done=%v %v", done, err)
			}
			if st, _ := DevicePairingState(ctx, pool, "jkt-1", now); st.WaitingFor != "device" {
				t.Fatalf("after owner: %+v", st)
			}
			if _, _, err := DeviceByKey(ctx, pool, "jkt-1", now); err != ErrNotFound {
				t.Fatalf("one confirmation made a device: %v", err)
			}
			dev, done, err = ConfirmPairingByDevice(ctx, pool, "jkt-1", true, now)
		} else {
			if _, done, err = ConfirmPairingByDevice(ctx, pool, "jkt-1", true, now); err != nil || done {
				t.Fatalf("device first: done=%v %v", done, err)
			}
			if st, _ := DevicePairingState(ctx, pool, "jkt-1", now); st.WaitingFor != "owner" {
				t.Fatalf("after device: %+v", st)
			}
			dev, done, err = ConfirmPairingByOwner(ctx, pool, bob, p.ID, true, now)
		}
		if err != nil || !done {
			t.Fatalf("second confirmation: done=%v %v", done, err)
		}
		got, owner, err := DeviceByKey(ctx, pool, "jkt-1", now)
		if err != nil || got.ID != dev.ID || owner.ID != bob {
			t.Fatalf("device: %v %v %v", got, owner, err)
		}
		if list, _ := PendingPairings(ctx, pool, bob, now); len(list) != 0 {
			t.Fatal("confirmed pairing still pending")
		}
	}
}

func TestConfirmedPairingConsumesTheCode(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	a, err := CompleteDevicePairing(ctx, pool, tok, "Mine", "jkt-a", "", now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CompleteDevicePairing(ctx, pool, tok, "Other", "jkt-b", "", now)
	if err != nil {
		t.Fatalf("second device on the same code: %v", err)
	}
	if a.Code == b.Code {
		t.Fatal("two keys showed the same code")
	}
	if _, _, err := ConfirmPairingByDevice(ctx, pool, "jkt-a", true, now); err != nil {
		t.Fatal(err)
	}
	if _, done, err := ConfirmPairingByOwner(ctx, pool, bob, a.ID, true, now); err != nil || !done {
		t.Fatalf("confirm a: %v %v", done, err)
	}
	// The other device is rejected and the code is used up.
	if st, _ := DevicePairingState(ctx, pool, "jkt-b", now); st.State != PairingRejected || st.Reason != RejectedTokenUsed {
		t.Fatalf("other device: %+v", st)
	}
	if _, _, err := ConfirmPairingByDevice(ctx, pool, "jkt-b", true, now); err != ErrPairingRejected {
		t.Fatalf("other device confirmed after consumption: %v", err)
	}
	if _, _, err := ConfirmPairingByOwner(ctx, pool, bob, b.ID, true, now); err != ErrNotFound {
		t.Fatalf("owner confirmed the rejected pairing: %v", err)
	}
	if _, err := CompleteDevicePairing(ctx, pool, tok, "Late", "jkt-c", "", now); err != ErrPairingTokenUsed {
		t.Fatalf("scan after consumption: %v", err)
	}
}

func TestOwnerRejectionInvalidatesTheCode(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	evil, _ := CompleteDevicePairing(ctx, pool, tok, "Phone", "jkt-evil", "", now)
	mine, _ := CompleteDevicePairing(ctx, pool, tok, "Phone", "jkt-mine", "", now)
	if _, done, err := ConfirmPairingByOwner(ctx, pool, bob, evil.ID, false, now); err != nil || done {
		t.Fatalf("reject: %v %v", done, err)
	}
	if st, _ := DevicePairingState(ctx, pool, "jkt-evil", now); st.State != PairingRejected || st.Reason != RejectedByOwner {
		t.Fatalf("rejected device: %+v", st)
	}
	// No new scans of the code, but the owner's own waiting pairing goes on.
	if _, err := CompleteDevicePairing(ctx, pool, tok, "Again", "jkt-again", "", now); err != ErrPairingTokenUsed {
		t.Fatalf("scan after rejection: %v", err)
	}
	if _, _, err := ConfirmPairingByOwner(ctx, pool, bob, mine.ID, true, now); err != nil {
		t.Fatal(err)
	}
	if _, done, err := ConfirmPairingByDevice(ctx, pool, "jkt-mine", true, now); err != nil || !done {
		t.Fatalf("own pairing after rejecting another: %v %v", done, err)
	}
}

func TestDeviceCancelKeepsTheCode(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	if _, err := CompleteDevicePairing(ctx, pool, tok, "Phone", "jkt-1", "", now); err != nil {
		t.Fatal(err)
	}
	if _, done, err := ConfirmPairingByDevice(ctx, pool, "jkt-1", false, now); err != nil || done {
		t.Fatalf("cancel: %v %v", done, err)
	}
	if st, _ := DevicePairingState(ctx, pool, "jkt-1", now); st.State != PairingRejected || st.Reason != RejectedByDevice {
		t.Fatalf("cancelled: %+v", st)
	}
	if _, err := CompleteDevicePairing(ctx, pool, tok, "Phone", "jkt-2", "", now); err != nil {
		t.Fatalf("new key after cancelling: %v", err)
	}
}

func TestPendingPairingsPerCodeAreBounded(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	for i := range MaxPendingPerToken {
		if _, err := CompleteDevicePairing(ctx, pool, tok, "D", fmt.Sprintf("jkt-%d", i), "", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CompleteDevicePairing(ctx, pool, tok, "D", "jkt-x", "", now); err != ErrTooManyPending {
		t.Fatalf("one too many: %v", err)
	}
}

func TestConfirmationAfterTheWindowIsRefused(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	p, err := CompleteDevicePairing(ctx, pool, tok, "Late", "jkt-late", "", now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(PairingTTL + time.Minute)
	if _, _, err := ConfirmPairingByOwner(ctx, pool, bob, p.ID, true, later); err != ErrPairingExpired {
		t.Fatalf("owner after window: %v", err)
	}
	if _, _, err := ConfirmPairingByDevice(ctx, pool, "jkt-late", true, later); err != ErrPairingExpired {
		t.Fatalf("device after window: %v", err)
	}
	if st, _ := DevicePairingState(ctx, pool, "jkt-late", later); st.State != PairingExpired {
		t.Fatalf("state %+v", st)
	}
}

// A QR code a device has scanned must not also give out an API key through the
// pairing without a device key, which needs no confirmation.
func TestScannedCodeGivesNoAPIKey(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	if _, err := CompleteDevicePairing(ctx, pool, tok, "Phone", "jkt-1", "", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RedeemPairingToken(ctx, pool, tok, "legacy"); err != ErrPairingTokenUsed {
		t.Fatalf("legacy redemption of a scanned code: %v", err)
	}
}

func TestExpiredTokenCannotPair(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	tok, _ := CreatePairingToken(ctx, pool, bob)
	if _, err := CompleteDevicePairing(ctx, pool, tok, "Old", "jkt-old", "", now.Add(PairingTTL+time.Minute)); err != ErrPairingExpired {
		t.Fatalf("expired token: %v", err)
	}
}

func TestKeyCanPairOnce(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	t1, _ := CreatePairingToken(ctx, pool, bob)
	t2, _ := CreatePairingToken(ctx, pool, bob)
	if _, err := CompleteDevicePairing(ctx, pool, t1, "One", "same-key", "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteDevicePairing(ctx, pool, t2, "Two", "same-key", "", now); err != ErrDeviceKeyInUse {
		t.Fatalf("second pairing of one key: %v", err)
	}
}

func TestRemovingADeviceStopsItsKey(t *testing.T) {
	ctx, pool, bob := deviceFixture(t)
	now := time.Now().UTC()
	dev := pairNow(t, ctx, pool, bob, "jkt-phone", now)
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
	pairNow(t, ctx, pool, bob, "jkt-dis", now)
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
