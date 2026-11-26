// Package audioinfo extracts duration (ms) and bitrate (kbps) from audio files
// using pure-Go decoders for the common formats. For unsupported formats it
// returns zeroes — the scanner still indexes the file, the UI just shows no
// duration.
package audioinfo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	mp4 "github.com/abema/go-mp4"
	wavdec "github.com/go-audio/wav"
	flac "github.com/mewkiz/flac"
	mp3 "github.com/tcolgate/mp3"
)

// Info holds the audio properties we care about for the Subsonic schema.
type Info struct {
	DurationMs  int64
	BitrateKbps int64
}

// Probe returns duration + bitrate for the audio file at path.
//
// suffix is the lowercased file extension (without dot). For formats we can't
// decode in pure Go (wma, ape, wv, mpc), zeroes are returned.
func Probe(path string, suffix string, filesize int64) Info {
	if suffix == "" {
		suffix = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	}
	f, err := os.Open(path)
	if err != nil {
		return Info{}
	}
	defer f.Close()

	var dur int64
	switch suffix {
	case "mp3":
		dur = probeMP3(f)
	case "flac":
		dur = probeFLAC(f)
	case "ogg", "opus":
		dur = probeOgg(f)
	case "m4a", "mp4", "aac", "alac":
		dur = probeMP4(f)
	case "wav":
		dur = probeWAV(f)
	default:
		// aiff, wv, mpc, ape, wma — no pure-Go decoder wired up.
	}
	if dur <= 0 || filesize <= 0 {
		return Info{DurationMs: dur}
	}
	// Average bitrate from file size. (bytes*8) / (durationMs/1000) / 1000 = bytes*8 / durationMs (in kbps).
	bitrate := filesize * 8 / dur
	return Info{DurationMs: dur, BitrateKbps: bitrate}
}

func probeMP3(f *os.File) int64 {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	dec := mp3.NewDecoder(f)
	var total time.Duration
	var frame mp3.Frame
	skipped := 0
	for {
		if err := dec.Decode(&frame, &skipped); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Some MP3s have noise after the last frame; treat any decode
			// error as end-of-stream rather than failure.
			break
		}
		total += frame.Duration()
	}
	return total.Milliseconds()
}

func probeFLAC(f *os.File) int64 {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	stream, err := flac.New(f)
	if err != nil {
		return 0
	}
	defer stream.Close()
	if stream.Info == nil || stream.Info.SampleRate == 0 {
		return 0
	}
	return int64(stream.Info.NSamples) * 1000 / int64(stream.Info.SampleRate)
}

// oggHeaderLen is the fixed part of an Ogg page header.
const oggHeaderLen = 27

// oggTailBytes is how much of the end of a file is searched for the last page.
const oggTailBytes = 64 * 1024

// oggPageAt parses the Ogg page that starts at b[0]. It returns the page's length
// (header, segment table and data) and its granule position. ok is false when b
// does not start a version 0 page or does not hold its complete header.
func oggPageAt(b []byte) (length int, granule int64, ok bool) {
	if len(b) < oggHeaderLen || string(b[0:4]) != "OggS" || b[4] != 0 {
		return 0, 0, false
	}
	nSeg := int(b[26])
	if len(b) < oggHeaderLen+nSeg {
		return 0, 0, false
	}
	data := 0
	for _, seg := range b[oggHeaderLen : oggHeaderLen+nSeg] {
		data += int(seg)
	}
	return oggHeaderLen + nSeg + data, int64(binary.LittleEndian.Uint64(b[6:14])), true
}

// oggCodec reads the first packet of the stream and reports its codec by
// content: an Opus stream has "OpusHead", a Vorbis stream a Vorbis
// identification header. The suffix is not trusted, since .ogg files can hold
// Opus (#165). It returns the sample rate that granule positions count in, the
// Opus pre-skip, and ok=false for anything else.
func oggCodec(f *os.File, size int64) (rate int64, preSkip int64, opus bool, ok bool) {
	n := min(size, oggTailBytes)
	head := make([]byte, n)
	if _, err := f.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, false, false
	}
	length, _, valid := oggPageAt(head)
	if !valid || len(head) < oggHeaderLen {
		return 0, 0, false, false
	}
	segs := int(head[26])
	// The first packet starts after the segment table. Its length is the sum of
	// the segments up to the first one shorter than 255.
	pktLen := 0
	for _, seg := range head[oggHeaderLen : oggHeaderLen+segs] {
		pktLen += int(seg)
		if seg < 255 {
			break
		}
	}
	dataStart := oggHeaderLen + segs
	if dataStart+pktLen > len(head) || dataStart+pktLen > length {
		return 0, 0, false, false
	}
	pkt := head[dataStart : dataStart+pktLen]
	switch {
	case len(pkt) >= 19 && string(pkt[0:8]) == "OpusHead":
		return 48000, int64(binary.LittleEndian.Uint16(pkt[10:12])), true, true
	case len(pkt) >= 30 && pkt[0] == 0x01 && string(pkt[1:7]) == "vorbis":
		return int64(binary.LittleEndian.Uint32(pkt[12:16])), 0, false, true
	}
	return 0, 0, false, false
}

// oggLastGranule returns the granule of the last complete page with a granule
// (not -1). It searches the last oggTailBytes first, so the file is not read in
// full. Damaged pages in the middle do not matter. Pages that start in the tail
// and are cut off by the end of the file are not used. A page is accepted when
// it ends at the end of the file or is followed by another "OggS", which rules
// out a false match inside audio data.
func oggLastGranule(f *os.File, size int64) (int64, bool) {
	tailStart := max(size-oggTailBytes, 0)
	tail := make([]byte, size-tailStart)
	if _, err := f.ReadAt(tail, tailStart); err != nil && !errors.Is(err, io.EOF) {
		return 0, false
	}
	for p := len(tail) - oggHeaderLen; p >= 0; p-- {
		if tail[p] != 'O' || string(tail[p:p+4]) != "OggS" {
			continue
		}
		length, granule, ok := oggPageAt(tail[p:])
		if !ok || granule == -1 {
			continue
		}
		end := tailStart + int64(p) + int64(length)
		if end > size {
			continue
		}
		if end < size {
			next := tail[end-tailStart:]
			if len(next) < 4 || string(next[:4]) != "OggS" {
				continue
			}
		}
		return granule, true
	}
	return 0, false
}

// Bounds of the forward Ogg scan (#198): it reads in chunks, so memory stays
// at about one chunk plus a page, and it stops after oggForwardMax bytes.
const (
	oggChunk      = 1 << 20
	oggMaxPage    = 27 + 255 + 255*255 // header, segment table, payload
	oggForwardMax = 1 << 30
)

// oggForwardGranule is the fallback when the tail has no usable page: it walks
// the file and resyncs on the next "OggS" after any damaged bytes, so one bad
// page does not end the scan. It reads in chunks of oggChunk, keeping the
// bytes of a page that runs past a chunk for the next one, and stops after
// oggForwardMax bytes; a longer damaged file gets the duration up to there.
func oggForwardGranule(f *os.File, size int64) (int64, bool) {
	if size > oggForwardMax {
		size = oggForwardMax
	}
	last := int64(-1)
	var buf []byte
	var read int64 // bytes of the file read so far
	for {
		eof := read >= size
		if !eof {
			n := min(int64(oggChunk), size-read)
			chunk := make([]byte, n)
			got, err := f.ReadAt(chunk, read)
			if got <= 0 && err != nil && !errors.Is(err, io.EOF) {
				break
			}
			buf = append(buf, chunk[:got]...)
			read += int64(got)
			eof = read >= size || got == 0
		}
		p := 0
		for p < len(buf) {
			i := bytes.Index(buf[p:], []byte("OggS"))
			if i < 0 {
				// Keep a possible "Ogg" prefix of a capture pattern at the end.
				p = max(p, len(buf)-3)
				break
			}
			p += i
			if !eof && len(buf)-p < oggMaxPage {
				break // the page may continue in the next chunk
			}
			length, granule, ok := oggPageAt(buf[p:])
			if ok && granule != -1 && p+length <= len(buf) {
				last = granule
				p += length
				continue
			}
			p++
		}
		if eof {
			break
		}
		buf = append(buf[:0], buf[min(p, len(buf)):]...)
	}
	return last, last > 0
}

// probeOgg returns the duration in ms of an Ogg stream of any codec this package
// knows (Vorbis or Opus). For Opus, the pre-skip is taken off the granule (#165).
func probeOgg(f *os.File) int64 {
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	size := st.Size()
	rate, preSkip, opus, ok := oggCodec(f, size)
	if !ok || rate <= 0 {
		return 0
	}
	granule, ok := oggLastGranule(f, size)
	if !ok {
		granule, ok = oggForwardGranule(f, size)
	}
	if !ok {
		return 0
	}
	if opus {
		granule -= preSkip
	}
	if granule <= 0 {
		return 0
	}
	return granule * 1000 / rate
}

func probeMP4(f *os.File) int64 {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	info, err := mp4.Probe(f)
	if err != nil || info.Timescale == 0 {
		return 0
	}
	return int64(info.Duration) * 1000 / int64(info.Timescale)
}

func probeWAV(f *os.File) int64 {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	d := wavdec.NewDecoder(f)
	if !d.IsValidFile() {
		return 0
	}
	d.ReadInfo()
	dur, err := d.Duration()
	if err != nil {
		return 0
	}
	return dur.Milliseconds()
}
