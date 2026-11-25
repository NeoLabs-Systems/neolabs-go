// Package audiox holds the small audio helpers the voice features share:
// 16-bit PCM, WAV wrapping and parsing, audio MIME types, and linear
// resampling. It is pure Go. The wearable protocol and the live voice
// providers all speak raw PCM, so there is no Opus here.
package audiox

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// PCMFormat describes interleaved little-endian PCM.
type PCMFormat struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
}

// DefaultPCM is what live voice providers send back: 24 kHz mono 16-bit.
var DefaultPCM = PCMFormat{SampleRate: 24000, Channels: 1, BitsPerSample: 16}

// DefaultMime is assumed for audio that names no type.
const DefaultMime = "audio/pcm;rate=16000;channels=1"

// NormalizeMime lower-cases and trims a MIME type, falling back to fallback
// (DefaultMime when empty) for a blank one.
func NormalizeMime(mime, fallback string) string {
	if fallback == "" {
		fallback = DefaultMime
	}
	m := strings.ToLower(strings.TrimSpace(mime))
	if m == "" {
		return fallback
	}
	return m
}

var (
	rateParam     = regexp.MustCompile(`(?:^|[;\s])rate=(\d+)`)
	channelsParam = regexp.MustCompile(`(?:^|[;\s])channels=(\d+)`)
)

// ParsePCMMime reads "audio/pcm;rate=16000;channels=1". It reports false for
// any other type or an unusable rate or channel count. A missing rate means
// 16 kHz and a missing channel count mono, as the phone and wearable send.
func ParsePCMMime(mime string) (PCMFormat, bool) {
	m := NormalizeMime(mime, "")
	if !strings.HasPrefix(m, "audio/pcm") {
		return PCMFormat{}, false
	}
	rate, ok := intParam(rateParam, m, 16000)
	if !ok {
		return PCMFormat{}, false
	}
	channels, ok := intParam(channelsParam, m, 1)
	if !ok {
		return PCMFormat{}, false
	}
	return PCMFormat{SampleRate: rate, Channels: channels, BitsPerSample: 16}, true
}

func intParam(re *regexp.Regexp, s string, fallback int) (int, bool) {
	match := re.FindStringSubmatch(s)
	if match == nil {
		return fallback, true
	}
	n, err := strconv.ParseUint(match[1], 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return int(n), true
}

// FileExtension picks the file extension a speech-to-text upload needs.
func FileExtension(mime string) string {
	m := NormalizeMime(mime, "")
	switch {
	case strings.HasPrefix(m, "audio/mpeg") || strings.Contains(m, "mp3"):
		return "mp3"
	case strings.HasPrefix(m, "audio/webm"):
		return "webm"
	case strings.HasPrefix(m, "audio/ogg"):
		return "ogg"
	case strings.HasPrefix(m, "audio/wav"):
		return "wav"
	case strings.HasPrefix(m, "audio/mp4") || strings.Contains(m, "m4a"):
		return "m4a"
	}
	return "wav"
}

// WrapWAV puts PCM into a canonical 44-byte-header WAV. Zero fields of f
// take the DefaultPCM values.
func WrapWAV(pcm []byte, f PCMFormat) []byte {
	if f.SampleRate == 0 {
		f.SampleRate = DefaultPCM.SampleRate
	}
	if f.Channels == 0 {
		f.Channels = DefaultPCM.Channels
	}
	if f.BitsPerSample == 0 {
		f.BitsPerSample = DefaultPCM.BitsPerSample
	}
	blockAlign := f.Channels * (f.BitsPerSample / 8)
	h := make([]byte, 44, 44+len(pcm))
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(f.SampleRate*blockAlign))
	binary.LittleEndian.PutUint16(h[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[34:], uint16(f.BitsPerSample))
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return append(h, pcm...)
}

// ErrNotWAV is returned for data that is not a PCM WAV file.
var ErrNotWAV = errors.New("audiox: not a 16-bit PCM WAV file")

// ParseWAV returns the PCM data and format of a PCM WAV, walking its chunks.
func ParseWAV(data []byte) ([]byte, PCMFormat, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, PCMFormat{}, ErrNotWAV
	}
	var f PCMFormat
	haveFmt := false
	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		body := pos + 8
		end := body + size
		if end > len(data) || end < body {
			end = len(data)
		}
		switch id {
		case "fmt ":
			if end-body < 16 || binary.LittleEndian.Uint16(data[body:]) != 1 {
				return nil, PCMFormat{}, ErrNotWAV
			}
			f = PCMFormat{
				Channels:      int(binary.LittleEndian.Uint16(data[body+2:])),
				SampleRate:    int(binary.LittleEndian.Uint32(data[body+4:])),
				BitsPerSample: int(binary.LittleEndian.Uint16(data[body+14:])),
			}
			haveFmt = true
		case "data":
			if !haveFmt || f.BitsPerSample != 16 {
				return nil, PCMFormat{}, ErrNotWAV
			}
			return data[body:end], f, nil
		}
		pos = end + size%2
	}
	return nil, PCMFormat{}, ErrNotWAV
}

// Samples reads little-endian 16-bit samples; a trailing odd byte is ignored.
func Samples(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}

// Bytes is the inverse of Samples.
func Bytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

// Silence is ms milliseconds of mono 16-bit silence at rate Hz.
func Silence(rate, ms int) []byte { return make([]byte, rate*2*ms/1000) }

// DurationMs is how long mono 16-bit PCM of the given rate plays.
func DurationMs(pcmBytes, rate int) float64 { return float64(pcmBytes) / 2 / float64(rate) * 1000 }

// Resample converts mono samples between rates by linear interpolation, good
// enough for speech (the wearable does the same on the device).
func Resample(in []int16, from, to int) []int16 {
	if from == to || len(in) == 0 || from <= 0 || to <= 0 {
		return append([]int16(nil), in...)
	}
	n := int(int64(len(in)) * int64(to) / int64(from))
	out := make([]int16, n)
	ratio := float64(from) / float64(to)
	for i := range out {
		pos := float64(i) * ratio
		idx := int(pos)
		frac := pos - float64(idx)
		a := float64(in[idx])
		b := a
		if idx+1 < len(in) {
			b = float64(in[idx+1])
		}
		out[i] = int16(a + (b-a)*frac)
	}
	return out
}

// DecodeBase64 is Buffer.from(text, 'base64'): lenient about padding and the
// URL-safe alphabet, and it skips characters outside the alphabet. Clients send
// audio this way, so a stray newline must not drop it.
func DecodeBase64(text string) []byte {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/':
			return r
		case r == '-':
			return '+'
		case r == '_':
			return '/'
		}
		return -1
	}, text)
	out, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil {
		// A dangling single character carries no byte.
		out, _ = base64.RawStdEncoding.DecodeString(clean[:len(clean)-len(clean)%4])
	}
	return out
}

// EncodeBase64 is the standard padded encoding providers and clients use.
func EncodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
