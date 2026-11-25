package audiox

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestWAVRoundTrip(t *testing.T) {
	pcm := Bytes([]int16{0, 1, -1, 32767, -32768})
	wav := WrapWAV(pcm, PCMFormat{SampleRate: 16000, Channels: 1, BitsPerSample: 16})
	got, f, err := ParseWAV(wav)
	if err != nil || !bytes.Equal(got, pcm) || f.SampleRate != 16000 || f.Channels != 1 || f.BitsPerSample != 16 {
		t.Fatalf("got %v %+v %v", got, f, err)
	}
	if _, _, err := ParseWAV([]byte("nope")); err == nil {
		t.Fatal("garbage parsed as WAV")
	}
}

func TestResample(t *testing.T) {
	in := make([]int16, 160)
	for i := range in {
		in[i] = int16(i * 100)
	}
	if out := Resample(in, 16000, 24000); len(out) != 240 || out[0] != 0 {
		t.Fatalf("len=%d", len(out))
	}
	if out := Resample(in, 16000, 8000); len(out) != 80 || out[1] != 200 {
		t.Fatalf("down: len=%d out[1]=%d", len(out), out[1])
	}
	if out := Resample(in, 16000, 16000); len(out) != 160 {
		t.Fatal("same rate changes length")
	}
}

func TestSilenceAndDuration(t *testing.T) {
	if n := len(Silence(24000, 100)); n != 4800 {
		t.Fatalf("silence = %d bytes", n)
	}
	if d := DurationMs(48000, 24000); d != 1000 {
		t.Fatalf("duration = %v", d)
	}
}

// vectors.json comes from the Node server's wrapPcmAsWav, parsePcmMimeType and
// fileExtensionForMimeType (gen/vectors.js).
func TestMatchesNode(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		WAV []struct {
			PCMHex string `json:"pcmHex"`
			Format struct {
				SampleRate    int `json:"sampleRate"`
				Channels      int `json:"channels"`
				BitsPerSample int `json:"bitsPerSample"`
			} `json:"format"`
			WAVHex string `json:"wavHex"`
		} `json:"wav"`
		Mime []struct {
			Input     string     `json:"input"`
			Parsed    *PCMFormat `json:"parsed"`
			Extension string     `json:"extension"`
		} `json:"mime"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.WAV {
		pcm := unhex(t, c.PCMHex)
		got := WrapWAV(pcm, PCMFormat{SampleRate: c.Format.SampleRate, Channels: c.Format.Channels, BitsPerSample: c.Format.BitsPerSample})
		if !bytes.Equal(got, unhex(t, c.WAVHex)) {
			t.Errorf("WAV for %+v differs from Node", c.Format)
		}
	}
	for _, c := range v.Mime {
		f, ok := ParsePCMMime(c.Input)
		if ok != (c.Parsed != nil) || (ok && f != *c.Parsed) {
			t.Errorf("ParsePCMMime(%q) = %+v,%v; Node %+v", c.Input, f, ok, c.Parsed)
		}
		if got := FileExtension(c.Input); got != c.Extension {
			t.Errorf("FileExtension(%q) = %q; Node %q", c.Input, got, c.Extension)
		}
	}
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBase64LikeNodeBuffer(t *testing.T) {
	for in, want := range map[string]string{
		"AQID":    "\x01\x02\x03",
		"AQI=":    "\x01\x02",
		"AQI":     "\x01\x02",
		"AQ\nID ": "\x01\x02\x03",
		"_-8":     "\xff\xef",
		"":        "",
		"A":       "",
		"AQIDB":   "\x01\x02\x03",
	} {
		if got := string(DecodeBase64(in)); got != want {
			t.Errorf("DecodeBase64(%q) = %q, want %q", in, got, want)
		}
	}
	if EncodeBase64([]byte{1, 2}) != "AQI=" {
		t.Error("EncodeBase64 padding")
	}
}
