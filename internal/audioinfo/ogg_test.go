package audioinfo

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// oggPage builds one Ogg page holding one packet (longer packets are split
// across segments, as Ogg does). The CRC is left at zero; the reader does not
// check it, just as the probe does not.
func oggPage(granule int64, packet []byte) []byte {
	var segs []byte
	n := len(packet)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	segs = append(segs, byte(n))
	var b bytes.Buffer
	b.WriteString("OggS")
	b.WriteByte(0) // version
	b.WriteByte(0) // header type
	_ = binary.Write(&b, binary.LittleEndian, granule)
	_ = binary.Write(&b, binary.LittleEndian, uint32(1)) // serial
	_ = binary.Write(&b, binary.LittleEndian, uint32(0)) // sequence
	_ = binary.Write(&b, binary.LittleEndian, uint32(0)) // crc
	b.WriteByte(byte(len(segs)))
	b.Write(segs)
	b.Write(packet)
	return b.Bytes()
}

// opusHead is the identification packet of an Opus stream.
func opusHead(preSkip uint16) []byte {
	p := []byte("OpusHead")
	p = append(p, 1, 2) // version, channels
	p = binary.LittleEndian.AppendUint16(p, preSkip)
	p = binary.LittleEndian.AppendUint32(p, 48000) // input sample rate
	p = binary.LittleEndian.AppendUint16(p, 0)     // gain
	p = append(p, 0)                               // mapping family
	return p
}

// vorbisIdent is the identification header of a Vorbis stream.
func vorbisIdent(rate uint32) []byte {
	p := []byte{0x01}
	p = append(p, "vorbis"...)
	p = binary.LittleEndian.AppendUint32(p, 0) // version
	p = append(p, 2)                           // channels
	p = binary.LittleEndian.AppendUint32(p, rate)
	p = append(p, make([]byte, 12)...) // bitrates
	p = append(p, 0xB8, 0x01)          // blocksizes, framing
	return p
}

// audioPage is a page of filler audio with the given granule.
func audioPage(granule int64, size int) []byte {
	return oggPage(granule, bytes.Repeat([]byte{0x5A}, size))
}

func writeOgg(t *testing.T, name string, parts ...[]byte) string {
	t.Helper()
	var b bytes.Buffer
	for _, p := range parts {
		b.Write(p)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Opus in a .ogg file (294 files on production, #165): the suffix says ogg, the
// codec is Opus, and the duration counts 48 kHz granules.
func TestOggOpusInOggSuffix(t *testing.T) {
	path := writeOgg(t, "song.ogg",
		oggPage(0, opusHead(0)),
		oggPage(0, []byte("OpusTags\x00\x00\x00\x00\x00\x00\x00\x00")),
		audioPage(48000*4, 100),
		audioPage(48000*10, 100))
	if got := Probe(path, "ogg", 0).DurationMs; got != 10000 {
		t.Fatalf("duration %d ms, want 10000", got)
	}
}

// The pre-skip comes off the granule, so the duration is exact.
func TestOggOpusPreSkip(t *testing.T) {
	path := writeOgg(t, "pre.opus",
		oggPage(0, opusHead(312)),
		oggPage(0, []byte("OpusTags")),
		audioPage(48000*10+312, 100))
	if got := Probe(path, "opus", 0).DurationMs; got != 10000 {
		t.Fatalf("duration %d ms, want 10000", got)
	}
}

// Vorbis is read at its own sample rate, not at 48 kHz.
func TestOggVorbis(t *testing.T) {
	path := writeOgg(t, "vorbis.ogg",
		oggPage(0, vorbisIdent(44100)),
		oggPage(0, []byte("\x03vorbis")),
		audioPage(44100*5, 100))
	if got := Probe(path, "ogg", 0).DurationMs; got != 5000 {
		t.Fatalf("duration %d ms, want 5000", got)
	}
}

// A damaged page in the middle (the case of Arquethi/EP, #165): garbage with no
// OggS does not end the read, because the duration comes from the tail.
func TestOggDamagedPageInTheMiddle(t *testing.T) {
	garbage := bytes.Repeat([]byte("CRC mismatch!"), 40)
	path := writeOgg(t, "damaged.opus",
		oggPage(0, opusHead(0)),
		oggPage(0, []byte("OpusTags")),
		audioPage(48000*2, 100),
		garbage,
		audioPage(48000*10, 100))
	if got := Probe(path, "opus", 0).DurationMs; got != 10000 {
		t.Fatalf("duration %d ms, want 10000", got)
	}
}

// The file's last page is cut off: the last complete page is used instead.
func TestOggTailPageCutOff(t *testing.T) {
	cut := audioPage(48000*9, 400)[:200]
	path := writeOgg(t, "cut.opus",
		oggPage(0, opusHead(0)),
		oggPage(0, []byte("OpusTags")),
		audioPage(48000*2, 100),
		cut)
	if got := Probe(path, "opus", 0).DurationMs; got != 2000 {
		t.Fatalf("duration %d ms, want 2000", got)
	}
}

// Pages with granule -1 (no packet ends in them) are skipped.
func TestOggSkipsGranuleMinusOne(t *testing.T) {
	path := writeOgg(t, "minus.opus",
		oggPage(0, opusHead(0)),
		oggPage(0, []byte("OpusTags")),
		audioPage(48000*3, 100),
		audioPage(-1, 100))
	if got := Probe(path, "opus", 0).DurationMs; got != 3000 {
		t.Fatalf("duration %d ms, want 3000", got)
	}
}

// A long file: the tail holds only audio, so the last granule is still found
// without reading the whole file.
func TestOggLongFile(t *testing.T) {
	parts := [][]byte{oggPage(0, opusHead(0)), oggPage(0, []byte("OpusTags"))}
	for i := 1; i <= 200; i++ {
		parts = append(parts, audioPage(int64(i)*48000/10, 1000))
	}
	path := writeOgg(t, "long.opus", parts...)
	if got := Probe(path, "opus", 0).DurationMs; got != 20000 {
		t.Fatalf("duration %d ms, want 20000", got)
	}
}

// When the tail has no usable page, the forward scan resyncs past the junk and
// still finds the last page before it.
func TestOggTailUnusableFallsBackToForwardScan(t *testing.T) {
	junk := bytes.Repeat([]byte{0x11}, oggTailBytes+1000)
	path := writeOgg(t, "junk.opus",
		oggPage(0, opusHead(0)),
		oggPage(0, []byte("OpusTags")),
		audioPage(48000*4, 100),
		junk)
	if got := Probe(path, "opus", 0).DurationMs; got != 4000 {
		t.Fatalf("duration %d ms, want 4000", got)
	}
}

// An unknown codec is not guessed at.
func TestOggUnknownCodecIsZero(t *testing.T) {
	path := writeOgg(t, "other.ogg",
		oggPage(0, []byte("\x7fFLAC something")),
		audioPage(48000, 100))
	if got := Probe(path, "ogg", 0).DurationMs; got != 0 {
		t.Fatalf("duration %d ms, want 0", got)
	}
}
