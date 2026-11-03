package scanner

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
)

// vorbisComment is one NAME=value entry, in file order. Unlike the tag library,
// it keeps every value of a repeated field.
type vorbisComment struct {
	Name  string // upper case
	Value string
}

// maxCommentBlock bounds the size of a comment block read from a file, so a
// corrupt length cannot make the scanner allocate gigabytes.
const maxCommentBlock = 1 << 20

var errNoComments = errors.New("no Vorbis comment block")

// readVorbisComments returns the Vorbis comments of a FLAC or Ogg file (Opus or
// Vorbis). It returns errNoComments for other formats.
func readVorbisComments(path, suffix string) ([]vorbisComment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	switch suffix {
	case "flac":
		return readFLACComments(f)
	case "ogg", "oga", "opus":
		return readOggComments(f)
	}
	return nil, errNoComments
}

// readFLACComments walks the metadata blocks until the VORBIS_COMMENT block
// (type 4), which is the only block that carries these fields.
func readFLACComments(r io.ReadSeeker) ([]vorbisComment, error) {
	var marker [4]byte
	if _, err := io.ReadFull(r, marker[:]); err != nil || string(marker[:]) != "fLaC" {
		return nil, errNoComments
	}
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, err
		}
		last := hdr[0]&0x80 != 0
		blockType := hdr[0] & 0x7f
		size := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		if blockType == 4 {
			if size > maxCommentBlock {
				return nil, errors.New("vorbis comment block too large")
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, err
			}
			return parseVorbisCommentBlock(body, 0)
		}
		if _, err := r.Seek(size, io.SeekCurrent); err != nil {
			return nil, err
		}
		if last {
			return nil, errNoComments
		}
	}
}

// readOggComments reassembles the first two packets of an Ogg stream. The second
// one is the comment header for both Opus ("OpusTags") and Vorbis ("\x03vorbis").
func readOggComments(r io.Reader) ([]vorbisComment, error) {
	var packets [][]byte
	var current []byte
	for len(packets) < 2 {
		var hdr [27]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, errNoComments
		}
		if string(hdr[:4]) != "OggS" {
			return nil, errNoComments
		}
		segments := make([]byte, hdr[26])
		if _, err := io.ReadFull(r, segments); err != nil {
			return nil, errNoComments
		}
		var total int
		for _, s := range segments {
			total += int(s)
		}
		data := make([]byte, total)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, errNoComments
		}
		pos := 0
		for _, s := range segments {
			current = append(current, data[pos:pos+int(s)]...)
			pos += int(s)
			if len(current) > maxCommentBlock {
				return nil, errors.New("ogg packet too large")
			}
			if s < 255 {
				packets = append(packets, current)
				current = nil
				if len(packets) == 2 {
					break
				}
			}
		}
	}
	switch {
	case bytes.HasPrefix(packets[1], []byte("OpusTags")):
		return parseVorbisCommentBlock(packets[1], 8)
	case bytes.HasPrefix(packets[1], []byte("\x03vorbis")):
		return parseVorbisCommentBlock(packets[1], 7)
	}
	return nil, errNoComments
}

// parseVorbisCommentBlock reads the Vorbis comment layout: a little-endian
// vendor string, then a count and that many little-endian length-prefixed
// UTF-8 strings of the form NAME=value. The block starts at offset.
func parseVorbisCommentBlock(b []byte, offset int) ([]vorbisComment, error) {
	p := b[offset:]
	take := func(n uint32) ([]byte, bool) {
		if uint64(n) > uint64(len(p)) {
			return nil, false
		}
		out := p[:n]
		p = p[n:]
		return out, true
	}
	lenBytes, ok := take(4)
	if !ok {
		return nil, errors.New("truncated vorbis comment block")
	}
	if _, ok := take(binary.LittleEndian.Uint32(lenBytes)); !ok { // vendor string
		return nil, errors.New("truncated vendor string")
	}
	countBytes, ok := take(4)
	if !ok {
		return nil, errors.New("truncated comment count")
	}
	count := binary.LittleEndian.Uint32(countBytes)
	var out []vorbisComment
	for i := uint32(0); i < count; i++ {
		lb, ok := take(4)
		if !ok {
			break
		}
		s, ok := take(binary.LittleEndian.Uint32(lb))
		if !ok {
			break
		}
		name, value, found := strings.Cut(string(s), "=")
		if !found || name == "" {
			continue
		}
		out = append(out, vorbisComment{Name: strings.ToUpper(name), Value: value})
	}
	return out, nil
}
