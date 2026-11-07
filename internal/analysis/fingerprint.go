package analysis

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strings"
)

// Fingerprint is a Chromaprint audio fingerprint as folksentia prints it.
type Fingerprint struct {
	Algorithm   string  `json:"algorithm"`
	Version     int     `json:"version"`
	DurationSec float64 `json:"durationSec"`
	// Value is the compressed fingerprint, base64 as fpcalc prints it.
	Value string `json:"value"`
}

// DecodeFingerprint decompresses a Chromaprint fingerprint into its 32-bit
// sub-fingerprints (about eight per second of audio).
//
// The format, from Chromaprint's fingerprint_compressor.cpp: URL-safe base64
// without padding; one byte algorithm, three bytes item count (big endian);
// then each item XOR the previous one, written as the gaps between its set
// bits in 3-bit values packed LSB first, each item ended by a 0, a gap of 7
// or more written as 7 plus a 5-bit value in a second packed section.
func DecodeFingerprint(s string) (algorithm int, items []uint32, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return 0, nil, fmt.Errorf("fingerprint: %w", err)
	}
	if len(raw) < 4 {
		return 0, nil, errors.New("fingerprint: too short")
	}
	algorithm = int(raw[0])
	n := int(raw[1])<<16 | int(raw[2])<<8 | int(raw[3])
	body := raw[4:]
	// Each item takes at least one 3-bit value, so n is bounded by the data.
	if n > len(body)*8/3 {
		return 0, nil, errors.New("fingerprint: item count exceeds data")
	}

	// Normal values until n items are complete.
	var normal []byte
	exceptions := 0
	for pos, done := 0, 0; done < n; pos++ {
		if (pos+1)*3 > len(body)*8 {
			return 0, nil, errors.New("fingerprint: truncated")
		}
		v := unpackBits(body, pos*3, 3)
		normal = append(normal, v)
		switch v {
		case 0:
			done++
		case 7:
			exceptions++
		}
	}
	excStart := (len(normal)*3 + 7) / 8
	if (excStart*8)+exceptions*5 > len(body)*8 {
		return 0, nil, errors.New("fingerprint: truncated exceptions")
	}

	items = make([]uint32, 0, n)
	var prev, cur uint32
	bit, exc := 0, 0
	for _, v := range normal {
		if v == 0 {
			prev ^= cur
			items = append(items, prev)
			cur, bit = 0, 0
			continue
		}
		gap := int(v)
		if v == 7 {
			gap += int(unpackBits(body[excStart:], exc*5, 5))
			exc++
		}
		bit += gap
		if bit > 32 {
			return 0, nil, errors.New("fingerprint: bit position out of range")
		}
		cur |= 1 << (bit - 1)
	}
	return algorithm, items, nil
}

// unpackBits reads width bits starting at bit offset off, LSB first.
func unpackBits(b []byte, off, width int) byte {
	var v byte
	for i := range width {
		p := off + i
		if b[p/8]>>(p%8)&1 != 0 {
			v |= 1 << i
		}
	}
	return v
}

// ── Duplicate search ───────────────────────────────────────────────────────

// Matching parameters. A sub-fingerprint's top keyBits bits are its index
// key; between re-encodes of one recording a large share of keys survive,
// between unrelated tracks only a chance few do.
const (
	keyBits = 20
	// minSharedKeys distinct shared keys make a pair a candidate.
	minSharedKeys = 10
	// commonKeyShare: a key present in more tracks than this share (and at
	// least commonKeyMin) is ignored, e.g. the sub-fingerprint of silence.
	commonKeyShare = 0.02
	commonKeyMin   = 50
	// minOverlap sub-fingerprints (about 5 s) must be compared.
	minOverlap = 40
	// MaxBitErrorRate is the largest share of differing bits at the best
	// alignment for two tracks to count as the same recording. Unrelated
	// audio is near 0.5; re-encodes of one master are far below 0.15.
	MaxBitErrorRate = 0.15
)

// Track is one fingerprinted song for FindDuplicates.
type Track struct {
	SongID string
	// Kind is algorithm and version; only equal kinds are compared.
	Kind  string
	Items []uint32
}

// DuplicateGroup is a set of songs that hold the same recording.
type DuplicateGroup struct {
	SongIDs []string `json:"songIds"`
	// Similarity is 1 - bit error rate of the least similar matched pair.
	Similarity float64 `json:"similarity"`
}

// FindDuplicates groups tracks whose fingerprints match. It looks only at
// candidate pairs from an inverted index of sub-fingerprint keys, then
// compares each candidate pair at its most likely alignment.
func FindDuplicates(tracks []Track) []DuplicateGroup {
	byKind := map[string][]int{}
	for i, t := range tracks {
		if len(t.Items) >= minOverlap {
			byKind[t.Kind] = append(byKind[t.Kind], i)
		}
	}
	type edge struct {
		a, b int
		sim  float64
	}
	var edges []edge

	for _, members := range byKind {
		postings := make([][]int32, 1<<keyBits)
		keysOf := make([][]uint32, len(members))
		for m, ti := range members {
			keys := make([]uint32, len(tracks[ti].Items))
			for i, it := range tracks[ti].Items {
				keys[i] = it >> (32 - keyBits)
			}
			slices.Sort(keys)
			keys = slices.Compact(keys)
			keysOf[m] = keys
			for _, k := range keys {
				postings[k] = append(postings[k], int32(m))
			}
		}
		limit := max(commonKeyMin, int(commonKeyShare*float64(len(members))))
		shared := map[int32]int{}
		for m, keys := range keysOf {
			clear(shared)
			for _, k := range keys {
				p := postings[k]
				if len(p) > limit {
					continue
				}
				for _, o := range p {
					if int(o) > m {
						shared[o]++
					}
				}
			}
			for o, n := range shared {
				if n < minSharedKeys {
					continue
				}
				a, b := members[m], members[o]
				if ber, ok := compare(tracks[a].Items, tracks[b].Items); ok && ber <= MaxBitErrorRate {
					edges = append(edges, edge{a, b, 1 - ber})
				}
			}
		}
	}

	// Union-find over the matched pairs; a group's similarity is that of its
	// least similar matched pair.
	parent := make([]int, len(tracks))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for _, e := range edges {
		parent[find(e.b)] = find(e.a)
	}
	low := map[int]float64{}
	for _, e := range edges {
		r := find(e.a)
		if s, ok := low[r]; !ok || e.sim < s {
			low[r] = e.sim
		}
	}
	members := map[int][]string{}
	for i := range tracks {
		if r := find(i); low[r] > 0 {
			members[r] = append(members[r], tracks[i].SongID)
		}
	}
	var out []DuplicateGroup
	for r, ids := range members {
		slices.Sort(ids)
		out = append(out, DuplicateGroup{SongIDs: ids, Similarity: math.Round(low[r]*1000) / 1000})
	}
	slices.SortFunc(out, func(x, y DuplicateGroup) int { return strings.Compare(x.SongIDs[0], y.SongIDs[0]) })
	return out
}

// compare returns the bit error rate of a and b at their best alignment,
// found by voting on the offsets of equal keys, and false when they overlap
// too little.
func compare(a, b []uint32) (float64, bool) {
	first := map[uint32]int{}
	for i, it := range a {
		k := it >> (32 - keyBits)
		if _, ok := first[k]; !ok {
			first[k] = i
		}
	}
	votes := map[int]int{}
	for j, it := range b {
		if i, ok := first[it>>(32-keyBits)]; ok {
			votes[i-j]++
		}
	}
	best, bestVotes := 0, -1
	for off, v := range votes {
		if v > bestVotes || (v == bestVotes && abs(off) < abs(best)) {
			best, bestVotes = off, v
		}
	}
	lowest, found := 1.0, false
	for off := best - 1; off <= best+1; off++ {
		if ber, ok := berAt(a, b, off); ok && ber < lowest {
			lowest, found = ber, true
		}
	}
	return lowest, found
}

// berAt compares a[i] with b[i-off] over their overlap.
func berAt(a, b []uint32, off int) (float64, bool) {
	start, end := max(0, off), min(len(a), len(b)+off)
	if end-start < minOverlap {
		return 0, false
	}
	diff := 0
	for i := start; i < end; i++ {
		diff += bits.OnesCount32(a[i] ^ b[i-off])
	}
	return float64(diff) / float64(32*(end-start)), true
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
