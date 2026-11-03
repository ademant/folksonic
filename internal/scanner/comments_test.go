package scanner

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// vorbisBlock builds a Vorbis comment block body: little-endian vendor string,
// count, then length-prefixed NAME=value entries.
func vorbisBlock(entries ...string) []byte {
	var b bytes.Buffer
	le := func(n int) { _ = binary.Write(&b, binary.LittleEndian, uint32(n)) }
	le(len("test"))
	b.WriteString("test")
	le(len(entries))
	for _, e := range entries {
		le(len(e))
		b.WriteString(e)
	}
	return b.Bytes()
}

// flacWithComments is a minimal FLAC stream: STREAMINFO followed by a
// VORBIS_COMMENT block. Audio frames are not needed by the reader.
func flacWithComments(entries ...string) []byte {
	var b bytes.Buffer
	b.WriteString("fLaC")
	b.Write([]byte{0x00, 0x00, 0x00, 34}) // STREAMINFO, not last
	b.Write(make([]byte, 34))
	body := vorbisBlock(entries...)
	b.Write([]byte{0x84, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}) // type 4, last
	b.Write(body)
	return b.Bytes()
}

// oggPage wraps one or more packets in a single Ogg page. Packets are laced
// with the usual rule: a segment of 255 means the packet continues.
func oggPage(headerType byte, packets ...[]byte) []byte {
	var segs, data []byte
	for _, p := range packets {
		n := len(p)
		for ; n >= 255; n -= 255 {
			segs = append(segs, 255)
		}
		segs = append(segs, byte(n))
		data = append(data, p...)
	}
	var b bytes.Buffer
	b.WriteString("OggS")
	b.WriteByte(0)
	b.WriteByte(headerType)
	b.Write(make([]byte, 8+4+4+4)) // granule, serial, sequence, checksum
	b.WriteByte(byte(len(segs)))
	b.Write(segs)
	b.Write(data)
	return b.Bytes()
}

func findComments(t *testing.T, got []vorbisComment, name string) []string {
	t.Helper()
	var out []string
	for _, c := range got {
		if c.Name == name {
			out = append(out, c.Value)
		}
	}
	return out
}

func TestFLACKeepsRepeatedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.flac")
	data := flacWithComments("DANCE=Waltz", "use=learn", "DANCE=Mazurka")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readVorbisComments(path, "flac")
	if err != nil {
		t.Fatal(err)
	}
	if d := findComments(t, got, "DANCE"); len(d) != 2 || d[0] != "Waltz" || d[1] != "Mazurka" {
		t.Fatalf("DANCE values: %v", d)
	}
	if u := findComments(t, got, "USE"); len(u) != 1 || u[0] != "learn" {
		t.Fatalf("USE value (names are upper-cased): %v", u)
	}
}

func TestOggOpusTagsAcrossPages(t *testing.T) {
	head := append([]byte("OpusHead"), make([]byte, 11)...)
	tags := append([]byte("OpusTags"), vorbisBlock("DANCE=Bourree", "DANCE=Gavotte")...)
	// The tags packet is larger than one segment, so it spans lacing segments.
	data := append(oggPage(2, head), oggPage(0, tags)...)
	path := filepath.Join(t.TempDir(), "a.opus")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readVorbisComments(path, "opus")
	if err != nil {
		t.Fatal(err)
	}
	if d := findComments(t, got, "DANCE"); len(d) != 2 || d[0] != "Bourree" || d[1] != "Gavotte" {
		t.Fatalf("DANCE values: %v", d)
	}
}

func TestUnknownFormatHasNoComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.mp3")
	if err := os.WriteFile(path, []byte("ID3 not a vorbis file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readVorbisComments(path, "mp3"); err == nil {
		t.Fatal("mp3 read as vorbis comments")
	}
}
