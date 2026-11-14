package passwords

import (
	"strings"
	"testing"
)

// The gate admits Slots computations at once and refuses the next one without
// waiting (#109).
func TestGateRefusesWhenFull(t *testing.T) {
	for i := 0; i < Slots; i++ {
		if !TryAcquire() {
			t.Fatalf("slot %d refused while free", i+1)
		}
	}
	if TryAcquire() {
		t.Fatal("a slot was granted beyond the limit")
	}
	Release()
	if !TryAcquire() {
		t.Fatal("no slot after one was released")
	}
	for i := 0; i < Slots; i++ {
		Release()
	}
}

func TestHashRoundTripAndCap(t *testing.T) {
	h, err := Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(h, "correct horse") || Verify(h, "wrong") {
		t.Fatal("verify does not match the hashed password")
	}
	if _, err := Hash(strings.Repeat("x", MaxBytes+1)); err == nil {
		t.Fatal("Hash accepted an over-long password")
	}
}
