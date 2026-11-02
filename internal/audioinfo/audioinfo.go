// Package audioinfo extracts duration (ms) and bitrate (kbps) from audio files
// using pure-Go decoders for the common formats. For unsupported formats it
// returns zeroes — the scanner still indexes the file, the UI just shows no
// duration.
package audioinfo

import (
	"bufio"
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
	case "ogg":
		dur = probeOgg(f, "vorbis")
	case "opus":
		dur = probeOgg(f, "opus")
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

// probeOgg parses the OGG container and returns the duration in ms.
// For codec="opus" the granule is always 48 kHz samples. For "vorbis" we read
// the first page's identification header to discover the sample rate.
func probeOgg(f *os.File, codec string) int64 {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	br := bufio.NewReaderSize(f, 1<<15)

	var sampleRate int64
	if codec == "opus" {
		sampleRate = 48000
	}
	var lastGranule int64 = -1
	firstPage := true
	const noGranule = int64(-1)

	for {
		hdr := make([]byte, 27)
		if _, err := io.ReadFull(br, hdr); err != nil {
			break
		}
		if string(hdr[0:4]) != "OggS" {
			break
		}
		granule := int64(binary.LittleEndian.Uint64(hdr[6:14]))
		nSeg := int(hdr[26])
		segTable := make([]byte, nSeg)
		if _, err := io.ReadFull(br, segTable); err != nil {
			break
		}
		dataSize := 0
		for _, s := range segTable {
			dataSize += int(s)
		}

		if firstPage && codec == "vorbis" && dataSize >= 30 {
			data := make([]byte, dataSize)
			if _, err := io.ReadFull(br, data); err != nil {
				break
			}
			if data[0] == 0x01 && string(data[1:7]) == "vorbis" {
				sampleRate = int64(binary.LittleEndian.Uint32(data[12:16]))
			}
		} else {
			if _, err := br.Discard(dataSize); err != nil {
				break
			}
		}
		firstPage = false
		if granule != noGranule {
			lastGranule = granule
		}
	}
	if sampleRate <= 0 || lastGranule <= 0 {
		return 0
	}
	return lastGranule * 1000 / sampleRate
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
