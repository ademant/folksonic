package scanner

import (
	"bytes"
	"io"
)

// The scanner checks a file's content before any tag or audio parser sees it
// (#219): a file named like audio that is not is not indexed, and audio under
// the wrong extension is served with the type of what it is. This does not
// make parsing safe: a crafted file with a valid header still reaches the
// parsers, which is what safeExtract is for.

// Container kinds that sniffFormat recognises.
const (
	kindMP3       = "mp3"
	kindFLAC      = "flac"
	kindOggVorbis = "ogg-vorbis"
	kindOggOpus   = "ogg-opus"
	kindOggOther  = "ogg" // Ogg FLAC, Speex and other codecs
	kindMP4       = "mp4"
	kindADTS      = "adts"
	kindWAV       = "wav"
	kindAIFF      = "aiff"
	kindWavPack   = "wv"
	kindMusepack  = "mpc"
	kindAPE       = "ape"
	kindASF       = "wma"
)

// sniffHead is how much of the file sniffFormat reads at the start, and
// mp3SyncWindow how far into it an MP3 frame may begin when the file has no
// ID3v2 tag (some encoders leave padding or junk before the first frame).
const (
	sniffHead     = 512
	mp3SyncWindow = 64 << 10
)

var asfGUID = []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}

// sniffFormat reads the start of r and names its container, or "" when it is
// no audio format the scanner knows.
func sniffFormat(r io.ReaderAt) string {
	head := make([]byte, sniffHead)
	n, _ := r.ReadAt(head, 0)
	head = head[:n]
	if k := sniffContainer(head); k != "" {
		return k
	}
	// An ID3v2 tag in front: name what follows it. MP3 is the usual case,
	// and a tag followed by padding or junk is still taken as MP3, which is
	// what players do.
	if len(head) >= 10 && bytes.HasPrefix(head, []byte("ID3")) {
		size := int64(head[6]&0x7F)<<21 | int64(head[7]&0x7F)<<14 | int64(head[8]&0x7F)<<7 | int64(head[9]&0x7F)
		off := 10 + size
		if head[5]&0x10 != 0 { // footer present
			off += 10
		}
		after := make([]byte, sniffHead)
		m, _ := r.ReadAt(after, off)
		switch k := sniffContainer(after[:m]); k {
		case kindFLAC, kindADTS:
			return k
		}
		return kindMP3
	}
	// MPEG audio without a tag: two consecutive valid frames near the start.
	win := make([]byte, mp3SyncWindow)
	n, _ = r.ReadAt(win, 0)
	if mp3Frames(win[:n]) {
		return kindMP3
	}
	return ""
}

// sniffContainer names a container from its first bytes.
func sniffContainer(b []byte) string {
	has := func(off int, s string) bool { return len(b) >= off+len(s) && string(b[off:off+len(s)]) == s }
	switch {
	case has(0, "fLaC"):
		return kindFLAC
	case has(0, "OggS"):
		return oggCodec(b)
	case has(4, "ftyp"):
		return kindMP4
	case has(0, "RIFF") && has(8, "WAVE"):
		return kindWAV
	case has(0, "FORM") && (has(8, "AIFF") || has(8, "AIFC")):
		return kindAIFF
	case has(0, "wvpk"):
		return kindWavPack
	case has(0, "MPCK"), has(0, "MP+"):
		return kindMusepack
	case has(0, "MAC "):
		return kindAPE
	case len(b) >= 16 && bytes.Equal(b[:16], asfGUID):
		return kindASF
	case has(0, "ADIF"):
		return kindADTS
	case len(b) >= 7 && b[0] == 0xFF && b[1]&0xF6 == 0xF0:
		// ADTS: sync, layer 00. Frame length is 13 bits from byte 3.
		if l := int(b[3]&0x03)<<11 | int(b[4])<<3 | int(b[5])>>5; l >= 7 {
			return kindADTS
		}
	}
	return ""
}

// oggCodec names the codec of an Ogg stream from its first packet.
func oggCodec(b []byte) string {
	if len(b) < 27 {
		return kindOggOther
	}
	start := 27 + int(b[26]) // header, then the segment table
	pkt := func(s string) bool { return len(b) >= start+len(s) && string(b[start:start+len(s)]) == s }
	switch {
	case pkt("OpusHead"):
		return kindOggOpus
	case pkt("\x01vorbis"):
		return kindOggVorbis
	}
	return kindOggOther
}

// mp3Frames reports whether b holds two consecutive valid MPEG audio frame
// headers, the first one starting anywhere in b.
func mp3Frames(b []byte) bool {
	for i := 0; i+4 <= len(b); i++ {
		if b[i] != 0xFF {
			continue
		}
		l := mpegFrameLen(b[i:])
		if l <= 0 {
			continue
		}
		next := i + l
		if next+4 > len(b) {
			// The window ends inside this frame: one valid header right at
			// the start is enough for a file that short.
			return i == 0 && next >= len(b)
		}
		if mpegFrameLen(b[next:]) > 0 {
			return true
		}
	}
	return false
}

var (
	mpegBitrates = [5][15]int{
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}, // V1 L1
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},    // V1 L2
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},     // V1 L3
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},    // V2 L1
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},         // V2 L2, L3
	}
	mpegRates = map[byte][3]int{3: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 0: {11025, 12000, 8000}}
)

// mpegFrameLen is the length of the MPEG audio frame whose header starts b,
// or 0 when b does not start with a valid header (free bitrate included,
// which no common encoder writes).
func mpegFrameLen(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return 0
	}
	version := (b[1] >> 3) & 3 // 0: 2.5, 1: reserved, 2: 2, 3: 1
	layer := (b[1] >> 1) & 3   // 0: reserved, 1: III, 2: II, 3: I
	bri := int(b[2] >> 4)
	sri := int(b[2]>>2) & 3
	if version == 1 || layer == 0 || bri == 0 || bri == 15 || sri == 3 {
		return 0
	}
	var table int
	switch {
	case version == 3 && layer == 3:
		table = 0
	case version == 3 && layer == 2:
		table = 1
	case version == 3:
		table = 2
	case layer == 3:
		table = 3
	default:
		table = 4
	}
	bitrate := mpegBitrates[table][bri] * 1000
	rate := mpegRates[version][sri]
	pad := int(b[2]>>1) & 1
	switch {
	case layer == 3: // Layer I
		return (12*bitrate/rate + pad) * 4
	case layer == 1 && version != 3: // Layer III, MPEG 2 and 2.5
		return 72*bitrate/rate + pad
	default:
		return 144*bitrate/rate + pad
	}
}

// accepts lists, per extension, the containers that file may hold without a
// warning.
var accepts = map[string][]string{
	"mp3":  {kindMP3},
	"flac": {kindFLAC},
	"ogg":  {kindOggVorbis, kindOggOther, kindOggOpus},
	"opus": {kindOggOpus},
	"m4a":  {kindMP4},
	"mp4":  {kindMP4},
	"alac": {kindMP4},
	"aac":  {kindMP4, kindADTS},
	"wav":  {kindWAV},
	"aiff": {kindAIFF},
	"aif":  {kindAIFF},
	"wv":   {kindWavPack},
	"mpc":  {kindMusepack},
	"ape":  {kindAPE},
	"wma":  {kindASF},
}

// kindSuffix is the suffix a container is indexed under when its file's
// extension names something else.
var kindSuffix = map[string]string{
	kindMP3: "mp3", kindFLAC: "flac", kindOggVorbis: "ogg", kindOggOpus: "opus",
	kindOggOther: "ogg", kindMP4: "m4a", kindADTS: "aac", kindWAV: "wav",
	kindAIFF: "aiff", kindWavPack: "wv", kindMusepack: "mpc", kindAPE: "ape", kindASF: "wma",
}

// resolveSuffix decides the suffix a file is indexed under, from its
// extension and its sniffed container. ok is false for a file that is no
// known audio; mismatch is true when the extension names another format.
// Ogg Opus under .ogg is indexed as opus without a warning: it is a common,
// valid way to name it, and the suffix only picks the content type.
func resolveSuffix(ext, kind string) (suffix string, ok, mismatch bool) {
	if kind == "" {
		return "", false, false
	}
	for _, k := range accepts[ext] {
		if k == kind {
			if ext == "ogg" && kind == kindOggOpus {
				return "opus", true, false
			}
			return ext, true, false
		}
	}
	return kindSuffix[kind], true, true
}
