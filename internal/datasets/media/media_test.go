package media_test

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/media"
)

// The fixtures were written by soundfile (libsndfile) and Pillow; the
// durations in durations.json are soundfile's frames / samplerate.
func TestAudioDuration(t *testing.T) {
	raw, err := os.ReadFile("testdata/durations.json")
	require.NoError(t, err)
	var want map[string]float64
	require.NoError(t, json.Unmarshal(raw, &want))
	for name, d := range want {
		b, err := os.ReadFile("testdata/" + name)
		require.NoError(t, err)
		got, err := media.AudioDuration(b)
		require.NoError(t, err, name)
		switch name {
		case "a.mp3":
			// The Xing header counts whole frames, including the encoder's
			// delay and padding, as ffmpeg's header duration does: 24
			// frames of 576 samples at 16 kHz.
			require.InDelta(t, 24*576/16000.0, got, 1e-9, name)
		case "a.opus":
			// Opus durations come from the last granule position less
			// the pre-skip.
			require.InDelta(t, d, got, 0.02, name)
		default:
			require.InDelta(t, d, got, 1e-6*math.Max(1, d), name)
		}
	}
	_, err = media.AudioDuration([]byte("nope"))
	require.ErrorIs(t, err, media.ErrUnknown)
	_, err = media.AudioDuration([]byte("RIFF\x00\x00\x00\x00WAVE"))
	require.ErrorIs(t, err, media.ErrUnknown)
	_, err = media.AudioDuration([]byte("fLaC\x80"))
	require.ErrorIs(t, err, media.ErrUnknown)
	_, err = media.AudioDuration([]byte("OggS\x00\x00"))
	require.ErrorIs(t, err, media.ErrUnknown)
	_, err = media.AudioDuration([]byte{0xff, 0x00, 0x00})
	require.ErrorIs(t, err, media.ErrUnknown)
}

func TestImageSize(t *testing.T) {
	for _, name := range []string{"i.png", "i.jpg", "i.gif", "i.bmp", "i.webp", "i.tiff", "l.webp"} {
		b, err := os.ReadFile("testdata/" + name)
		require.NoError(t, err)
		w, h, mime, err := media.ImageSize(b)
		require.NoError(t, err, name)
		require.Equal(t, 37, w, name)
		require.Equal(t, 21, h, name)
		require.Contains(t, mime, "image/", name)
	}
	_, _, _, err := media.ImageSize([]byte("not an image"))
	require.ErrorIs(t, err, media.ErrUnknown)
}

func TestAudioMIME(t *testing.T) {
	cases := map[string]string{"a.wav": "audio/wav", "B.MP3": "audio/mpeg", "c.opus": "audio/ogg", "d.flac": "audio/flac", "e.ogg": "audio/ogg"}
	for name, want := range cases {
		got, _ := media.AudioMIME(name, nil)
		require.Equal(t, want, got, name)
	}
	for _, name := range []string{"a.wav", "a.flac", "a.mp3", "a.ogg"} {
		b, err := os.ReadFile("testdata/" + name)
		require.NoError(t, err)
		got, ext := media.AudioMIME("", b)
		require.NotEqual(t, "application/octet-stream", got, name)
		require.NotEmpty(t, ext)
	}
	got, _ := media.AudioMIME("x", []byte("??"))
	require.Equal(t, "application/octet-stream", got)
}

func FuzzHeaders(f *testing.F) {
	for _, name := range []string{"a.wav", "a.flac", "a.ogg", "a.mp3", "i.webp", "i.tiff", "i.bmp"} {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = media.AudioDuration(b)
		_, _, _, _ = media.ImageSize(b)
	})
}
