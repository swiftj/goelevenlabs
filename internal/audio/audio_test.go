package audio

import (
	"math"
	"testing"
)

func sine(freq, amp, secs float64) *Buffer {
	n := FramesAt(secs)
	b := &Buffer{S: make([]float32, n*2)}
	for i := 0; i < n; i++ {
		v := float32(amp * math.Sin(2*math.Pi*freq*float64(i)/Rate))
		b.S[2*i], b.S[2*i+1] = v, v
	}
	return b
}

func TestLoudnessReference(t *testing.T) {
	// BS.1770: a 997 Hz stereo sine at -20 dBFS peak per channel ≈ -20 LUFS (within ~0.3 dB).
	got := Loudness(sine(997, DB(-20), 5))
	if math.Abs(got-(-20)) > 0.3 {
		t.Fatalf("loudness = %.2f LUFS, want ≈ -20", got)
	}
}

func TestLimiterCeiling(t *testing.T) {
	b := sine(440, 1.6, 2)
	Limit(b, DB(-1))
	if p := b.Peak(); p > DB(-1)+1e-4 {
		t.Fatalf("peak %.4f exceeds ceiling %.4f", p, DB(-1))
	}
}

func TestMixLoopLengthAndDucking(t *testing.T) {
	speech := sine(300, 0.3, 2)
	music := sine(110, 0.3, 1.5)
	m := Mix([]Track{{Buf: speech, Start: 1}}, []Track{{Buf: music, Start: 0, End: 6, Loop: true, XFade: 0.2, Ducked: true}},
		MixOptions{DuckDB: -20})
	if s := m.Out.Seconds(); math.Abs(s-6) > 0.01 {
		t.Fatalf("length %.2f, want 6", s)
	}
	rms := func(b *Buffer, a, z float64) float64 {
		var s float64
		for i := FramesAt(a); i < FramesAt(z); i++ {
			s += float64(b.S[2*i] * b.S[2*i])
		}
		return math.Sqrt(s / float64(FramesAt(z)-FramesAt(a)))
	}
	under := rms(m.Stems.Music, 1.5, 2.5) // while speaking
	clear := rms(m.Stems.Music, 4.5, 5.5) // after speech + release
	if under > clear*0.2 {
		t.Fatalf("music not ducked: under %.4f vs clear %.4f", under, clear)
	}
	if clear < 0.15 { // looped music must still be present at 5 s
		t.Fatalf("loop dropped out: rms %.4f", clear)
	}
}

func TestWAVRoundTripAndMP3(t *testing.T) {
	b := sine(440, 0.5, 1)
	w, err := DecodeWAV(EncodeWAV(b))
	if err != nil || w.Frames() != b.Frames() {
		t.Fatalf("wav roundtrip: %v frames %d", err, w.Frames())
	}
	mp3, used, err := EncodeMP3(b, "builtin")
	if err != nil || len(mp3) < 1000 {
		t.Fatalf("mp3 encode (%s): %v %d bytes", used, err, len(mp3))
	}
	d, err := DecodeMP3(mp3)
	if err != nil || math.Abs(d.Seconds()-1) > 0.1 {
		t.Fatalf("mp3 decode: %v %.2fs", err, d.Seconds())
	}
}

func BenchmarkMix60s(b *testing.B) {
	speech := sine(300, 0.3, 60)
	music := sine(110, 0.3, 20)
	for b.Loop() {
		Mix([]Track{{Buf: speech}}, []Track{{Buf: music, End: 60, Loop: true, XFade: 1, Ducked: true}}, MixOptions{DuckDB: -12, TargetLUFS: -16})
	}
}
