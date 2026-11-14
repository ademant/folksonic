package dpop

import (
	"strings"
	"testing"
	"time"
)

// A proof with a jti longer than maxJTILen is refused (#111).
func TestOverlongJTIRefused(t *testing.T) {
	k := newKey(t)
	now := time.Now()
	proof, err := NewProof(k, testMethod, testURL, strings.Repeat("j", maxJTILen+1), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(proof, testMethod, testURL, now, time.Minute); err != ErrMalformed {
		t.Fatalf("overlong jti: %v, want ErrMalformed", err)
	}
}

// A proof whose header exceeds maxHeaderBytes is refused before it is parsed.
func TestOversizedHeaderRefused(t *testing.T) {
	k := newKey(t)
	now := time.Now()
	proof, err := NewProof(k, testMethod, testURL, "jti-ok", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(proof, ".")
	parts[0] += strings.Repeat("A", maxHeaderBytes)
	if _, _, err := Verify(strings.Join(parts, "."), testMethod, testURL, now, time.Minute); err != ErrMalformed {
		t.Fatalf("oversized header: %v, want ErrMalformed", err)
	}
	if _, _, err := Verify(strings.Repeat("a", maxProofBytes+1), testMethod, testURL, now, time.Minute); err != ErrMalformed {
		t.Fatalf("oversized proof: %v, want ErrMalformed", err)
	}
}
