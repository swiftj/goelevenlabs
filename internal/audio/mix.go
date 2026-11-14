package audio

import (
	"math"
	"sync"
)

// Track is one clip placed on the timeline.
type Track struct {
	Buf     *Buffer
	Start   float64 // seconds
	End     float64 // seconds; >0 trims or (with Loop) extends the clip to this time
	Loop    bool    // repeat the clip until End
	XFade   float64 // loop crossfade seconds (0 = butt-splice, right for seamless loops)
	GainDB  float64
	FadeIn  float64
	FadeOut float64
	Pan     float64 // -1..1, constant power
	Ducked  bool    // attenuated by the sidechain while dialogue plays
}

// MixOptions controls the final mix.
type MixOptions struct {
	DuckDB      float64 // extra attenuation of Ducked tracks under speech (negative dB, e.g. -12)
	DuckAttack  float64 // seconds
	DuckRelease float64 // seconds
	TargetLUFS  float64 // 0 disables loudness normalisation
	CeilingDB   float64 // limiter ceiling, e.g. -1
	Length      float64 // 0 = longest track
}

// Stems are the rendered sub-mixes, useful for re-mixing in a DAW.
type Stems struct {
	Dialogue, Music, Effects *Buffer
}

// MixResult is the final mix and its measurements.
type MixResult struct {
	Out           *Buffer
	Stems         Stems
	InputLUFS     float64
	OutputLUFS    float64
	GainAppliedDB float64
	PeakDB        float64
}

// Mix renders dialogue (the sidechain key) plus tracks into one buffer.
// Tracks with Ducked=true go to the music stem, others to the effects stem.
func Mix(dialogue []Track, tracks []Track, o MixOptions) *MixResult {
	length := o.Length
	if length <= 0 {
		for _, t := range append(append([]Track{}, dialogue...), tracks...) {
			end := t.Start + t.Buf.Seconds()
			if t.End > 0 && (t.Loop || t.End < end) {
				end = t.End
			}
			length = math.Max(length, end)
		}
	}
	n := FramesAt(length)
	dlg := make([]float32, n*2)
	mus := make([]float32, n*2)
	fx := make([]float32, n*2)
	var wg sync.WaitGroup
	var mu [3]sync.Mutex
	render := func(dst []float32, m *sync.Mutex, t Track) {
		defer wg.Done()
		tmp := renderTrack(t, n)
		m.Lock()
		for i, v := range tmp.S {
			dst[tmp.off*2+i] += v
		}
		m.Unlock()
	}
	for _, t := range dialogue {
		wg.Add(1)
		go render(dlg, &mu[0], t)
	}
	for _, t := range tracks {
		wg.Add(1)
		if t.Ducked {
			go render(mus, &mu[1], t)
		} else {
			go render(fx, &mu[2], t)
		}
	}
	wg.Wait()

	if o.DuckDB < 0 {
		env := sidechain(dlg, n, DB(o.DuckDB), def(o.DuckAttack, 0.08), def(o.DuckRelease, 0.6))
		for i := 0; i < n; i++ {
			mus[2*i] *= env[i]
			mus[2*i+1] *= env[i]
		}
	}
	out := make([]float32, n*2)
	for i := range out {
		out[i] = dlg[i] + mus[i] + fx[i]
	}
	res := &MixResult{Out: &Buffer{S: out}, Stems: Stems{&Buffer{S: dlg}, &Buffer{S: mus}, &Buffer{S: fx}}}
	res.InputLUFS = Loudness(res.Out)
	if o.TargetLUFS != 0 && !math.IsInf(res.InputLUFS, -1) {
		g := o.TargetLUFS - res.InputLUFS
		res.GainAppliedDB = g
		gl := float32(DB(g))
		for i := range out {
			out[i] *= gl
		}
	}
	Limit(res.Out, DB(def(o.CeilingDB, -1)))
	res.OutputLUFS = Loudness(res.Out)
	res.PeakDB = 20 * math.Log10(math.Max(res.Out.Peak(), 1e-9))
	return res
}

func def(v, d float64) float64 {
	if v == 0 {
		return d
	}
	return v
}

type placed struct {
	S   []float32
	off int
}

// renderTrack produces the track's samples (gain, pan, loop, fades) and its frame offset.
func renderTrack(t Track, total int) placed {
	start := FramesAt(t.Start)
	if start >= total || t.Buf.Frames() == 0 {
		return placed{nil, 0}
	}
	if start < 0 {
		start = 0
	}
	src := t.Buf.S
	srcF := len(src) / 2
	length := srcF
	if t.End > 0 {
		want := FramesAt(t.End) - start
		if t.Loop || want < length {
			length = want
		}
	}
	if start+length > total {
		length = total - start
	}
	if length <= 0 {
		return placed{nil, 0}
	}
	out := make([]float32, length*2)
	xf := FramesAt(t.XFade)
	if xf*2 > srcF {
		xf = srcF / 4
	}
	period := srcF - xf
	for i := 0; i < length; i++ {
		var l, r float32
		if !t.Loop || i < srcF {
			if i < srcF {
				l, r = src[2*i], src[2*i+1]
			}
		} else {
			// position inside the repeating region (after the first full pass)
			k := xf + (i-srcF)%period
			l, r = src[2*k], src[2*k+1]
		}
		// equal-power crossfade into each loop seam
		if t.Loop && xf > 0 && i >= srcF-xf {
			var pos int
			if i < srcF {
				pos = i - (srcF - xf)
			} else {
				pos = (i-srcF)%period - (period - xf)
			}
			if pos >= 0 && pos < xf {
				a := float64(pos) / float64(xf)
				k := pos // head of the clip fades in
				l = l*float32(math.Cos(a*math.Pi/2)) + src[2*k]*float32(math.Sin(a*math.Pi/2))
				r = r*float32(math.Cos(a*math.Pi/2)) + src[2*k+1]*float32(math.Sin(a*math.Pi/2))
			}
		}
		out[2*i], out[2*i+1] = l, r
	}
	g := DB(t.GainDB)
	pl := math.Cos((t.Pan + 1) * math.Pi / 4)
	pr := math.Sin((t.Pan + 1) * math.Pi / 4)
	if t.Pan == 0 {
		pl, pr = 1, 1
	}
	fi, fo := FramesAt(t.FadeIn), FramesAt(t.FadeOut)
	for i := 0; i < length; i++ {
		f := g
		if fi > 0 && i < fi {
			f *= sCurve(float64(i) / float64(fi))
		}
		if fo > 0 && i >= length-fo {
			f *= sCurve(float64(length-i) / float64(fo))
		}
		out[2*i] *= float32(f * pl)
		out[2*i+1] *= float32(f * pr)
	}
	return placed{out, start}
}

func sCurve(x float64) float64 { return 0.5 - 0.5*math.Cos(math.Pi*math.Max(0, math.Min(1, x))) }

// sidechain returns a per-frame gain that drops to duck while dialogue is audible.
func sidechain(dlg []float32, n int, duck, attack, release float64) []float32 {
	const win = Rate / 100 // 10 ms
	const thresh = 0.0056  // ≈ -45 dBFS RMS
	hold := 25             // 250 ms hold after speech stops, bridges word gaps
	blocks := (n + win - 1) / win
	target := make([]float64, blocks)
	h := 0
	for b := 0; b < blocks; b++ {
		var s float64
		cnt := 0
		for i := b * win; i < min(n, (b+1)*win); i++ {
			l, r := float64(dlg[2*i]), float64(dlg[2*i+1])
			s += (l*l + r*r) / 2
			cnt++
		}
		speech := cnt > 0 && math.Sqrt(s/float64(cnt)) > thresh
		if speech {
			h = hold
		} else if h > 0 {
			h--
		}
		if speech || h > 0 {
			target[b] = duck
		} else {
			target[b] = 1
		}
	}
	// Look ahead by the attack time so the music is already down when the first syllable lands.
	la := int(attack * 100)
	for b := 0; b < blocks; b++ {
		for k := 1; k <= la && b+k < blocks; k++ {
			if target[b+k] < target[b] {
				target[b] = target[b+k]
			}
		}
	}
	env := make([]float32, n)
	cur := 1.0
	ca := math.Exp(-1 / (attack * Rate))
	cr := math.Exp(-1 / (release * Rate))
	for i := 0; i < n; i++ {
		tg := target[i/win]
		if tg < cur {
			cur = tg + (cur-tg)*ca
		} else {
			cur = tg + (cur-tg)*cr
		}
		env[i] = float32(cur)
	}
	return env
}

// ------------------------------------------------------------- loudness

type biquad struct{ b0, b1, b2, a1, a2, z1, z2 float64 }

func (q *biquad) run(x float64) float64 {
	y := q.b0*x + q.z1
	q.z1 = q.b1*x - q.a1*y + q.z2
	q.z2 = q.b2*x - q.a2*y
	return y
}

func kWeighting() (shelf, hp biquad) {
	fs := float64(Rate)
	{ // high shelf +4 dB @ 1500 Hz
		A := math.Pow(10, 4.0/40)
		w0 := 2 * math.Pi * 1500 / fs
		alpha := math.Sin(w0) / (2 * (1 / math.Sqrt2))
		c := math.Cos(w0)
		sa := 2 * math.Sqrt(A) * alpha
		a0 := (A + 1) - (A-1)*c + sa
		shelf = biquad{
			b0: A * ((A + 1) + (A-1)*c + sa) / a0,
			b1: -2 * A * ((A - 1) + (A+1)*c) / a0,
			b2: A * ((A + 1) + (A-1)*c - sa) / a0,
			a1: 2 * ((A - 1) - (A+1)*c) / a0,
			a2: ((A + 1) - (A-1)*c - sa) / a0,
		}
	}
	{ // high pass @ 38 Hz, Q 0.5
		w0 := 2 * math.Pi * 38 / fs
		alpha := math.Sin(w0) / (2 * 0.5)
		c := math.Cos(w0)
		a0 := 1 + alpha
		hp = biquad{b0: (1 + c) / 2 / a0, b1: -(1 + c) / a0, b2: (1 + c) / 2 / a0, a1: -2 * c / a0, a2: (1 - alpha) / a0}
	}
	return
}

// Loudness returns integrated loudness in LUFS (ITU-R BS.1770-4 gating).
func Loudness(b *Buffer) float64 {
	n := b.Frames()
	if n < Rate/2 {
		return math.Inf(-1)
	}
	sl, hl := kWeighting()
	sr, hr := kWeighting()
	sq := make([]float64, n)
	for i := 0; i < n; i++ {
		l := hl.run(sl.run(float64(b.S[2*i])))
		r := hr.run(sr.run(float64(b.S[2*i+1])))
		sq[i] = l*l + r*r
	}
	// prefix sums for O(1) block energy
	ps := make([]float64, n+1)
	for i, v := range sq {
		ps[i+1] = ps[i] + v
	}
	blk, step := int(0.4*Rate), int(0.1*Rate)
	var zs []float64
	for s := 0; s+blk <= n; s += step {
		zs = append(zs, (ps[s+blk]-ps[s])/float64(blk))
	}
	lufs := func(z float64) float64 { return -0.691 + 10*math.Log10(z) }
	var abs []float64
	for _, z := range zs {
		if z > 0 && lufs(z) > -70 {
			abs = append(abs, z)
		}
	}
	if len(abs) == 0 {
		return math.Inf(-1)
	}
	mean := func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s / float64(len(v))
	}
	rel := lufs(mean(abs)) - 10
	var gated []float64
	for _, z := range abs {
		if lufs(z) > rel {
			gated = append(gated, z)
		}
	}
	if len(gated) == 0 {
		return math.Inf(-1)
	}
	return lufs(mean(gated))
}

// Limit applies a 5 ms look-ahead peak limiter in place.
func Limit(b *Buffer, ceiling float64) {
	n := b.Frames()
	if n == 0 {
		return
	}
	need := make([]float64, n)
	for i := 0; i < n; i++ {
		p := math.Max(math.Abs(float64(b.S[2*i])), math.Abs(float64(b.S[2*i+1])))
		if p > ceiling {
			need[i] = ceiling / p
		} else {
			need[i] = 1
		}
	}
	// sliding-window minimum over the look-ahead (monotonic deque, O(n))
	la := Rate * 5 / 1000
	gmin := make([]float64, n)
	dq := make([]int, 0, la+1)
	for i := n - 1; i >= 0; i-- {
		for len(dq) > 0 && need[dq[len(dq)-1]] >= need[i] {
			dq = dq[:len(dq)-1]
		}
		dq = append(dq, i)
		for dq[0] > i+la {
			dq = dq[1:]
		}
		gmin[i] = need[dq[0]]
	}
	rel := math.Exp(-1 / (0.08 * Rate))
	att := math.Exp(-1 / (0.001 * Rate))
	cur := 1.0
	for i := 0; i < n; i++ {
		t := gmin[i]
		if t < cur {
			cur = t + (cur-t)*att
			if cur > need[i] { // never let a peak through
				cur = need[i]
			}
		} else {
			cur = t + (cur-t)*rel
		}
		b.S[2*i] *= float32(cur)
		b.S[2*i+1] *= float32(cur)
	}
}

// TrimSilence removes leading/trailing silence below -60 dBFS (keeps pad seconds).
func TrimSilence(b *Buffer, pad float64) *Buffer {
	n := b.Frames()
	th := float32(0.001)
	s, e := 0, n
	for s < n && abs32(b.S[2*s]) < th && abs32(b.S[2*s+1]) < th {
		s++
	}
	for e > s && abs32(b.S[2*e-2]) < th && abs32(b.S[2*e-1]) < th {
		e--
	}
	p := FramesAt(pad)
	s = max(0, s-p)
	e = min(n, e+p)
	return &Buffer{S: b.S[2*s : 2*e]}
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}
