package analysis

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

// Vectors made with libchromaprint 1.5.1's chromaprint_encode_fingerprint
// (algorithm 1, base64), the encoder fpcalc and folksentia use.
func TestDecodeFingerprintChromaprintVectors(t *testing.T) {
	for _, c := range []struct {
		enc  string
		want []uint32
	}{
		{"AQAAAQE", []uint32{1}},
		{"AQAAAUkA", []uint32{7}},
		{"AQAAAQcA", []uint32{1 << 6}},
		{"AQAAAQcC", []uint32{1 << 8}},
		{"AQAAA0eSJEmSJEmSJEmSJEGSJEmSJEmSJEmSJAkAGQ", []uint32{0x80000000, 0xffffffff, 0}},
		{"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", lcg(42, 300)},
	} {
		alg, items, err := DecodeFingerprint(c.enc)
		if err != nil || alg != 1 || !reflect.DeepEqual(items, c.want) {
			t.Errorf("%.20s…: algorithm %d, err %v, items %x…, want %x…", c.enc, alg, err, head(items), head(c.want))
		}
	}
}

func TestDecodeFingerprintRejectsBadInput(t *testing.T) {
	for _, s := range []string{"", "AQ", "not base64!", "AQAAAg", "AQAAAQ", "AQ__AQE"} {
		if _, items, err := DecodeFingerprint(s); err == nil {
			t.Errorf("%q accepted: %x", s, items)
		}
	}
}

// lcg is the sequence the vector was encoded from.
func lcg(seed uint32, n int) []uint32 {
	out := make([]uint32, n)
	x := seed
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = x
	}
	return out
}

func head(s []uint32) []uint32 { return s[:min(len(s), 4)] }

// noisy copies items from offset on, flipping each bit with probability p.
func noisy(r *rand.Rand, items []uint32, offset int, p float64) []uint32 {
	out := make([]uint32, 0, len(items))
	for _, it := range items[offset:] {
		for b := range 32 {
			if r.Float64() < p {
				it ^= 1 << b
			}
		}
		out = append(out, it)
	}
	return out
}

func randomItems(r *rand.Rand, n int) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = r.Uint32()
	}
	return out
}

func TestFindDuplicates(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	const kind = "chromaprint/2"
	song := randomItems(r, 960) // 120 s
	other := randomItems(r, 960)
	tracks := []Track{
		{SongID: "so-a", Kind: kind, Items: song},
		{SongID: "so-a-mp3", Kind: kind, Items: noisy(r, song, 0, 0.05)},   // re-encode
		{SongID: "so-a-late", Kind: kind, Items: noisy(r, song, 24, 0.03)}, // starts 3 s later
		{SongID: "so-a-v1", Kind: "chromaprint/1", Items: song},            // other algorithm
		{SongID: "so-b", Kind: kind, Items: other},
		{SongID: "so-b-copy", Kind: kind, Items: append([]uint32{}, other...)}, // identical file
		{SongID: "so-c", Kind: kind, Items: noisy(r, song, 0, 0.3)},            // too different
		{SongID: "so-short", Kind: kind, Items: song[:20]},                     // too short to judge
	}
	// Unrelated tracks must not join any group.
	for i := range 200 {
		tracks = append(tracks, Track{SongID: "so-z" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Kind: kind, Items: randomItems(r, 960)})
	}
	got := FindDuplicates(tracks)
	if len(got) != 2 {
		t.Fatalf("groups: %+v", got)
	}
	if want := []string{"so-a", "so-a-late", "so-a-mp3"}; !reflect.DeepEqual(got[0].SongIDs, want) || got[0].Similarity < 0.9 || got[0].Similarity > 0.98 {
		t.Errorf("re-encodes: %+v, want %v", got[0], want)
	}
	if want := []string{"so-b", "so-b-copy"}; !reflect.DeepEqual(got[1].SongIDs, want) || got[1].Similarity != 1 {
		t.Errorf("copies: %+v", got[1])
	}
}

func TestFindDuplicatesIgnoresCommonKeys(t *testing.T) {
	// Tracks that share long stretches of one sub-fingerprint (silence) and
	// are otherwise unrelated are not duplicates.
	r := rand.New(rand.NewPCG(3, 4))
	var tracks []Track
	for i := range 60 {
		items := randomItems(r, 400)
		for j := range 100 {
			items[j] = 0x12345678
		}
		tracks = append(tracks, Track{SongID: "so-" + string(rune('A'+i)), Kind: "k", Items: items})
	}
	if got := FindDuplicates(tracks); len(got) != 0 {
		t.Fatalf("silence made duplicates: %+v", got)
	}
}

// A library of 5,000 two-minute fingerprints with 50 re-encoded pairs.
func BenchmarkFindDuplicates5000(b *testing.B) {
	r := rand.New(rand.NewPCG(5, 6))
	var tracks []Track
	for i := range 5000 {
		items := randomItems(r, 960)
		if i%100 == 1 {
			items = noisy(r, tracks[i-1].Items, 0, 0.05)
		}
		tracks = append(tracks, Track{SongID: "so-" + strings.Repeat("x", i%7) + string(rune(i)), Kind: "k", Items: items})
	}
	b.ResetTimer()
	for b.Loop() {
		if got := FindDuplicates(tracks); len(got) != 50 {
			b.Fatalf("%d groups", len(got))
		}
	}
}
