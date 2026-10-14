// Package media reads what the Dataset Viewer needs from image and audio
// files without decoding them: image width and height, audio duration,
// and the MIME type to serve them with. It parses headers only, as the
// Hub's viewer reads `duration_seconds_from_header` for audio.
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif" // registers the GIF header decoder
	_ "image/jpeg"
	_ "image/png"
	"math"
)

// ErrUnknown means the format is not recognised or the header is invalid.
var ErrUnknown = errors.New("media: unknown or invalid format")

// ImageSize returns an image's width and height and its MIME type.
func ImageSize(b []byte) (width, height int, mime string, err error) {
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(b)); err == nil {
		return cfg.Width, cfg.Height, "image/" + format, nil
	}
	switch {
	case len(b) >= 30 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		w, h, err := webpSize(b)
		return w, h, "image/webp", err
	case len(b) >= 26 && string(b[:2]) == "BM":
		w := int(int32(binary.LittleEndian.Uint32(b[18:]))) //nolint:gosec // BMP dimensions are signed 32-bit values
		h := int(int32(binary.LittleEndian.Uint32(b[22:]))) //nolint:gosec // BMP dimensions are signed 32-bit values
		if h < 0 {
			h = -h
		}
		if w <= 0 {
			return 0, 0, "", ErrUnknown
		}
		return w, h, "image/bmp", nil
	case len(b) >= 8 && (string(b[:4]) == "II*\x00" || string(b[:4]) == "MM\x00*"):
		w, h, err := tiffSize(b)
		return w, h, "image/tiff", err
	}
	return 0, 0, "", ErrUnknown
}

func webpSize(b []byte) (int, int, error) {
	chunk := string(b[12:16])
	switch chunk {
	case "VP8 ":
		if len(b) < 30 || b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, ErrUnknown
		}
		w := int(binary.LittleEndian.Uint16(b[26:]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(b[28:]) & 0x3fff)
		return w, h, nil
	case "VP8L":
		if len(b) < 25 || b[20] != 0x2f {
			return 0, 0, ErrUnknown
		}
		bits := binary.LittleEndian.Uint32(b[21:])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	case "VP8X":
		w := int(uint32(b[24])|uint32(b[25])<<8|uint32(b[26])<<16) + 1
		h := int(uint32(b[27])|uint32(b[28])<<8|uint32(b[29])<<16) + 1
		return w, h, nil
	}
	return 0, 0, ErrUnknown
}

func tiffSize(b []byte) (int, int, error) {
	var order binary.ByteOrder = binary.LittleEndian
	if b[0] == 'M' {
		order = binary.BigEndian
	}
	off := int(order.Uint32(b[4:]))
	if off < 8 || off+2 > len(b) {
		return 0, 0, ErrUnknown
	}
	n := int(order.Uint16(b[off:]))
	w, h := 0, 0
	for i := range n {
		e := off + 2 + 12*i
		if e+12 > len(b) {
			break
		}
		tag := order.Uint16(b[e:])
		typ := order.Uint16(b[e+2:])
		var v int
		if typ == 3 {
			v = int(order.Uint16(b[e+8:]))
		} else {
			v = int(order.Uint32(b[e+8:]))
		}
		switch tag {
		case 256:
			w = v
		case 257:
			h = v
		}
	}
	if w == 0 || h == 0 {
		return 0, 0, ErrUnknown
	}
	return w, h, nil
}

// AudioMIME returns the MIME type the Hub's viewer serves an audio file
// with, from its extension or its magic number, and the extension.
func AudioMIME(name string, b []byte) (string, string) {
	types := map[string]string{".wav": "audio/wav", ".mp3": "audio/mpeg", ".opus": "audio/ogg", ".flac": "audio/flac", ".ogg": "audio/ogg"}
	for ext, t := range types {
		if len(name) >= len(ext) && equalFold(name[len(name)-len(ext):], ext) {
			return t, ext
		}
	}
	switch {
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "audio/wav", ".wav"
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		return "audio/flac", ".flac"
	case len(b) >= 3 && (string(b[:3]) == "ID3" || (b[0] == 0xff && (b[1] == 0xfb || b[1] == 0xf3 || b[1] == 0xf2))):
		return "audio/mpeg", ".mp3"
	case len(b) >= 4 && string(b[:4]) == "OggS":
		return "audio/ogg", ".ogg"
	}
	return "application/octet-stream", ""
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// AudioDuration returns an audio file's duration in seconds from its
// headers: WAV, FLAC, Ogg (Vorbis and Opus) and MP3.
func AudioDuration(b []byte) (float64, error) {
	switch {
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return wavDuration(b)
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		return flacDuration(b)
	case len(b) >= 4 && string(b[:4]) == "OggS":
		return oggDuration(b)
	case len(b) >= 3 && (string(b[:3]) == "ID3" || b[0] == 0xff):
		return mp3Duration(b)
	}
	return 0, ErrUnknown
}

func wavDuration(b []byte) (float64, error) {
	var byteRate uint32
	var dataSize int64 = -1
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		size := int64(binary.LittleEndian.Uint32(b[pos+4:]))
		body := pos + 8
		switch id {
		case "fmt ":
			if body+12 > len(b) {
				return 0, ErrUnknown
			}
			byteRate = binary.LittleEndian.Uint32(b[body+8:])
		case "data":
			dataSize = size
			if rest := int64(len(b) - body); size > rest || size == 0xffffffff {
				dataSize = rest
			}
		}
		if dataSize >= 0 && byteRate > 0 {
			break
		}
		next := int64(body) + size + size%2
		if next > int64(len(b)) || next <= int64(pos) {
			break
		}
		pos = int(next)
	}
	if byteRate == 0 || dataSize < 0 {
		return 0, ErrUnknown
	}
	return float64(dataSize) / float64(byteRate), nil
}

func flacDuration(b []byte) (float64, error) {
	if len(b) < 8+34 {
		return 0, ErrUnknown
	}
	// The first metadata block must be STREAMINFO.
	if b[4]&0x7f != 0 {
		return 0, ErrUnknown
	}
	si := b[8:]
	rate := uint32(si[10])<<12 | uint32(si[11])<<4 | uint32(si[12])>>4
	total := (uint64(si[13]&0x0f) << 32) | uint64(binary.BigEndian.Uint32(si[14:]))
	if rate == 0 {
		return 0, ErrUnknown
	}
	return float64(total) / float64(rate), nil
}

func oggDuration(b []byte) (float64, error) {
	// The identification header in the first page gives the codec and rate.
	if len(b) < 28 {
		return 0, ErrUnknown
	}
	segs := int(b[26])
	start := 27 + segs
	if start+19 > len(b) {
		return 0, ErrUnknown
	}
	payload := b[start:]
	var rate float64
	var preSkip int64
	switch {
	case len(payload) >= 16 && string(payload[1:7]) == "vorbis":
		rate = float64(binary.LittleEndian.Uint32(payload[12:]))
	case len(payload) >= 19 && string(payload[:8]) == "OpusHead":
		rate = 48000
		preSkip = int64(binary.LittleEndian.Uint16(payload[10:]))
	default:
		return 0, ErrUnknown
	}
	// The last page's granule position is the number of samples.
	last := bytes.LastIndex(b, []byte("OggS"))
	if last < 0 || last+14 > len(b) || rate == 0 {
		return 0, ErrUnknown
	}
	granule := int64(binary.LittleEndian.Uint64(b[last+6:])) //nolint:gosec // negative granules are rejected below
	if granule < 0 {
		return 0, ErrUnknown
	}
	return float64(granule-preSkip) / rate, nil
}

var (
	mp3Bitrates = [2][3][16]int{
		{ // MPEG 1: layer I, II, III
			{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
			{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
		},
		{ // MPEG 2 and 2.5
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		},
	}
	mp3Rates = [4][3]int{{11025, 12000, 8000}, {0, 0, 0}, {22050, 24000, 16000}, {44100, 48000, 32000}}
)

func mp3Duration(b []byte) (float64, error) {
	pos := 0
	if len(b) >= 10 && string(b[:3]) == "ID3" {
		size := int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f)
		pos = 10 + size
		if b[5]&0x10 != 0 {
			pos += 10
		}
	}
	for pos+4 <= len(b) && (b[pos] != 0xff || b[pos+1]&0xe0 != 0xe0) {
		pos++
	}
	if pos+4 > len(b) {
		return 0, ErrUnknown
	}
	h := binary.BigEndian.Uint32(b[pos:])
	version := (h >> 19) & 3 // 0: 2.5, 2: 2, 3: 1
	layer := 4 - int((h>>17)&3)
	brIdx := (h >> 12) & 0xf
	rateIdx := (h >> 10) & 3
	if version == 1 || layer == 4 || brIdx == 0 || brIdx == 15 || rateIdx == 3 {
		return 0, ErrUnknown
	}
	vi := 0
	if version != 3 {
		vi = 1
	}
	bitrate := mp3Bitrates[vi][layer-1][brIdx] * 1000
	rate := mp3Rates[version][rateIdx]
	samples := 1152
	switch {
	case layer == 1:
		samples = 384
	case layer == 3 && version != 3:
		samples = 576
	}
	// A Xing or Info header gives the frame count of VBR files.
	mono := (h>>6)&3 == 3
	side := 32
	switch {
	case version == 3 && mono:
		side = 17
	case version != 3 && mono:
		side = 9
	case version != 3:
		side = 17
	}
	x := pos + 4 + side
	if x+12 <= len(b) && (string(b[x:x+4]) == "Xing" || string(b[x:x+4]) == "Info") && b[x+7]&1 != 0 {
		frames := binary.BigEndian.Uint32(b[x+8:])
		return float64(frames) * float64(samples) / float64(rate), nil
	}
	if v := pos + 4 + 32; v+18 <= len(b) && string(b[v:v+4]) == "VBRI" {
		frames := binary.BigEndian.Uint32(b[v+14:])
		return float64(frames) * float64(samples) / float64(rate), nil
	}
	if bitrate == 0 {
		return 0, ErrUnknown
	}
	audio := len(b) - pos
	if len(b) >= 128 && string(b[len(b)-128:len(b)-125]) == "TAG" {
		audio -= 128
	}
	return math.Max(0, float64(audio)*8/float64(bitrate)), nil
}
