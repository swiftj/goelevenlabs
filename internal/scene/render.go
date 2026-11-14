package scene

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johnswift/goelevenlabs/internal/audio"
	"github.com/johnswift/goelevenlabs/internal/eleven"
	"golang.org/x/sync/errgroup"
)

// Spec describes a scene to render.
type Spec struct {
	Title    string            `json:"title,omitempty"`
	Script   string            `json:"script,omitempty" jsonschema:"screenplay text. 'NAME: line' for dialogue; {sfx: prompt | -8dB | pan=-0.5 | 2s}, {music: prompt | -16dB | fade=3}, {ambience: prompt}, {music: stop}, {pause: 1.5} on their own lines. Tags are placed where they appear."`
	Lines    []Line            `json:"lines,omitempty" jsonschema:"structured alternative/addition to script"`
	Voices   map[string]string `json:"voices,omitempty" jsonschema:"speaker name -> voice_id for this scene (else the saved cast, else voice library names)"`
	Music    []Cue             `json:"music,omitempty"`
	Ambience []Cue             `json:"ambience,omitempty"`
	SFX      []Cue             `json:"sfx,omitempty"`

	Model            string                    `json:"model,omitempty" jsonschema:"dialogue model, default eleven_v3"`
	Language         string                    `json:"language,omitempty" jsonschema:"ISO 639-1 code to force a language"`
	Stability        *float64                  `json:"stability,omitempty" jsonschema:"0 creative, 0.5 natural, 1 robust"`
	Seed             *int64                    `json:"seed,omitempty"`
	VoiceEffects     *eleven.Effects           `json:"voice_effects,omitempty" jsonschema:"default server-side voice effects for every line (reverb space, filter, distance, pan)"`
	CharacterEffects map[string]eleven.Effects `json:"character_effects,omitempty" jsonschema:"speaker -> voice effects for this scene, e.g. {\"SZASS TAM\": {\"environment_id\": \"hall\", \"send_level\": 0.6}}. Overrides the cast's saved effects."`
	NoPronunciation  bool                      `json:"no_pronunciation,omitempty" jsonschema:"skip the pronunciation lexicon for this render"`

	LeadIn     *float64 `json:"lead_in,omitempty" jsonschema:"seconds before the first line (default 0.6)"`
	Tail       *float64 `json:"tail,omitempty" jsonschema:"seconds after the last line (default 2)"`
	LengthSec  float64  `json:"length_sec,omitempty" jsonschema:"total length for a soundscape with no dialogue"`
	DuckDB     *float64 `json:"duck_db,omitempty" jsonschema:"how far music dips under speech, default -12 (0 disables)"`
	TargetLUFS *float64 `json:"target_lufs,omitempty" jsonschema:"integrated loudness target, default -16 (podcast); 0 disables"`

	Filename  string `json:"filename,omitempty" jsonschema:"output name without extension"`
	OutputDir string `json:"output_dir,omitempty"`
	Format    string `json:"format,omitempty" jsonschema:"mp3 (default) or wav"`
	Stems     bool   `json:"stems,omitempty" jsonschema:"also write dialogue/music/effects stems"`
	NoSubs    bool   `json:"no_subtitles,omitempty" jsonschema:"skip the .srt subtitle file"`

	Fresh            bool `json:"fresh,omitempty" jsonschema:"ignore the cache and generate new takes"`
	DryRun           bool `json:"dry_run,omitempty" jsonschema:"validate, resolve voices and report size without generating"`
	ParallelDialogue bool `json:"parallel_dialogue,omitempty" jsonschema:"render dialogue blocks concurrently (faster for long scripts, loses cross-block prosody continuity)"`
}

// Env is what the renderer needs from the host.
type Env struct {
	Client    *eleven.Client
	Cast      *Cast
	Lexicon   *Lexicon
	OutputDir string
	Encoder   string
	Progress  func(msg string, done, total int)
}

// TimedLine is a line with its resolved timing.
type TimedLine struct {
	N       int     `json:"n"`
	Speaker string  `json:"speaker"`
	VoiceID string  `json:"voice_id"`
	Text    string  `json:"text"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
}

// TimedCue is a cue with its resolved timing.
type TimedCue struct {
	Kind   string  `json:"kind"`
	Label  string  `json:"label"`
	Start  float64 `json:"start"`
	End    float64 `json:"end,omitempty"`
	GainDB float64 `json:"gain_db"`
	Cached bool    `json:"cached"`
}

// Result describes the rendered scene.
type Result struct {
	File       string             `json:"file,omitempty"`
	Seconds    float64            `json:"seconds"`
	LUFS       float64            `json:"lufs,omitempty"`
	PeakDBFS   float64            `json:"peak_dbfs,omitempty"`
	Encoder    string             `json:"encoder,omitempty"`
	Subtitles  string             `json:"subtitles,omitempty"`
	CueSheet   string             `json:"cue_sheet,omitempty"`
	Stems      []string           `json:"stems,omitempty"`
	Cast       map[string]string  `json:"cast"`
	Lines      int                `json:"lines"`
	Characters int                `json:"dialogue_characters"`
	Requests   map[string]int     `json:"requests"`
	CacheHits  int64              `json:"cache_hits"`
	Timeline   []TimedLine        `json:"timeline,omitempty"`
	Cues       []TimedCue         `json:"cues,omitempty"`
	TimingsSec map[string]float64 `json:"timings_sec,omitempty"`
	DryRun     bool               `json:"dry_run,omitempty"`
	Pronounce  *PronReport        `json:"pronunciation,omitempty"`
	Effects    map[string]string  `json:"voice_effects,omitempty"`
	Warnings   []string           `json:"warnings,omitempty"`
}

// PronReport says which lexicon entries were used.
type PronReport struct {
	Aliased    []string `json:"aliased,omitempty"`
	Dictionary string   `json:"dictionary,omitempty"`
	Version    string   `json:"version,omitempty"`
	Phonemes   int      `json:"phoneme_rules,omitempty"`
}

// blockGap is the silence between dialogue blocks (measured from the end of speech,
// so reverb tails from the previous block overlap the next line naturally).
const blockGap = 0.3

const maxBlockChars = 1900

// noContinuity records models that reject previous_request_ids. Their dialogue
// blocks are independent, so they are rendered concurrently.
var noContinuity sync.Map

func init() { noContinuity.Store("eleven_v3", true) }

var tagStrip = regexp.MustCompile(`\[[^\]]*\]`)
var voiceIDRe = regexp.MustCompile(`^[A-Za-z0-9]{20}$`)

type input struct {
	eleven.DialogueInput
	line int
	fx   *eleven.Effects
}

// Render renders a scene.
func Render(ctx context.Context, env Env, sp Spec) (*Result, error) {
	t0 := time.Now()
	timings := map[string]float64{}
	lines := append([]Line{}, sp.Lines...)
	music, amb, sfx := sp.Music, sp.Ambience, sp.SFX
	if strings.TrimSpace(sp.Script) != "" {
		p, err := ParseScript(sp.Script)
		if err != nil {
			return nil, err
		}
		if len(lines) > 0 && len(p.Lines) > 0 {
			return nil, errors.New("give dialogue in either script or lines, not both")
		}
		lines = append(lines, p.Lines...)
		music = append(p.Music, music...)
		amb = append(p.Ambience, amb...)
		sfx = append(p.SFX, sfx...)
	}
	if len(lines) == 0 && sp.LengthSec <= 0 {
		return nil, errors.New("nothing to render: add dialogue lines/script, or set length_sec for a soundscape")
	}

	// ---- resolve voices
	cast, err := resolveVoices(ctx, env, sp.Voices, lines)
	if err != nil {
		return nil, err
	}
	res := &Result{Cast: cast, Lines: len(lines), Requests: map[string]int{}}
	model := def(sp.Model, "eleven_v3")

	// ---- pronunciation: aliases rewrite the text locally; phonemes go via a dictionary
	var aliaser *Aliaser
	if env.Lexicon != nil && !sp.NoPronunciation {
		aliaser = env.Lexicon.Aliaser()
		res.Pronounce = &PronReport{}
		for _, e := range env.Lexicon.List("") {
			if e.kind() == "phoneme" {
				res.Pronounce.Phonemes++
			}
		}
	}

	// ---- per-line voice effects: line > scene character_effects > cast > scene default
	charFX := map[string]eleven.Effects{}
	for k, v := range sp.CharacterEffects {
		charFX[strings.ToLower(strings.TrimSpace(k))] = v
	}
	fxFor := func(l Line) *eleven.Effects {
		if l.Effects != nil {
			return l.Effects
		}
		if v, ok := charFX[strings.ToLower(l.Speaker)]; ok {
			return &v
		}
		if env.Cast != nil && l.Speaker != "" {
			if m, ok := env.Cast.Get(l.Speaker); ok && m.Effects != nil {
				return m.Effects
			}
		}
		return sp.VoiceEffects
	}
	var inputs []input
	seenAlias := map[string]bool{}
	for i, l := range lines {
		vid := l.VoiceID
		if vid == "" {
			vid = cast[l.Speaker]
		}
		res.Characters += len(l.Text)
		text := l.Text
		if aliaser != nil {
			if t := aliaser.Apply(text); t != text {
				for _, e := range env.Lexicon.List("") {
					if e.kind() == "alias" && !seenAlias[e.Term] && strings.Contains(strings.ToLower(text), strings.ToLower(e.Term)) {
						seenAlias[e.Term] = true
						res.Pronounce.Aliased = append(res.Pronounce.Aliased, e.Term+" → "+e.Alias)
					}
				}
				text = t
			}
		}
		fx := fxFor(l)
		if !fx.IsZero() {
			if res.Effects == nil {
				res.Effects = map[string]string{}
			}
			res.Effects[def(l.Speaker, vid)] = fx.Key()
		} else {
			fx = nil
		}
		for _, part := range splitText(text, maxBlockChars) {
			inputs = append(inputs, input{eleven.DialogueInput{Text: part, VoiceID: vid}, i, fx})
		}
	}
	if res.Effects != nil {
		if cat, err := env.Client.Catalog(ctx); err == nil {
			for _, in := range inputs {
				if err := cat.Validate(in.fx); err != nil {
					return nil, fmt.Errorf("voice effects for %s: %w", def(lines[in.line].Speaker, in.VoiceID), err)
				}
			}
		} else {
			res.Warnings = append(res.Warnings, "could not fetch the effects catalog to validate effect names: "+err.Error())
		}
	}
	blocks := makeBlocks(inputs, lines)
	res.Requests["dialogue"] = len(blocks)
	for _, c := range sfx {
		if c.File == "" {
			res.Requests["sfx"]++
		}
	}
	for _, c := range amb {
		if c.File == "" {
			res.Requests["ambience"]++
		}
	}
	for _, c := range music {
		if c.File == "" {
			res.Requests["music"]++
		}
	}
	// validate anchors early (with a dummy timeline)
	dummy := make([]TimedLine, len(lines))
	for _, c := range sfx {
		if _, err := anchor(def(c.At, "start"), dummy, 1); err != nil {
			return nil, err
		}
	}
	for _, c := range append(append([]Cue{}, music...), amb...) {
		for _, a := range []string{def(c.From, "start"), def(c.To, "end")} {
			if _, err := anchor(a, dummy, 1); err != nil {
				return nil, err
			}
		}
	}
	if res.Pronounce != nil && res.Pronounce.Phonemes > 0 && !phonemeModels[model] {
		res.Warnings = append(res.Warnings, fmt.Sprintf("model %s ignores phoneme rules; only alias rules apply", model))
	}
	if sp.DryRun {
		res.DryRun = true
		for i, l := range lines {
			res.Timeline = append(res.Timeline, TimedLine{N: i + 1, Speaker: l.Speaker, VoiceID: cast[l.Speaker], Text: l.Text})
		}
		return res, nil
	}

	total := len(blocks) + res.Requests["sfx"] + res.Requests["ambience"] + res.Requests["music"] + 1
	var done atomic.Int32
	step := func(msg string) {
		if env.Progress != nil {
			env.Progress(msg, int(done.Add(1)), total)
		}
	}
	cache := !sp.Fresh
	hits0 := env.Client.Hits.Load()

	// ---- phase A: dialogue chain || sfx || ambience (none depend on each other)
	g, gctx := errgroup.WithContext(ctx)
	type blockOut struct {
		buf  *audio.Buffer
		segs []eleven.VoiceSegment
	}
	bouts := make([]blockOut, len(blocks))
	var dict []eleven.DictLocator
	if res.Pronounce != nil && res.Pronounce.Phonemes > 0 && phonemeModels[model] {
		loc, err := env.Lexicon.Sync(ctx, env.Client)
		if err != nil {
			res.Warnings = append(res.Warnings, "phoneme rules not applied: "+err.Error())
		} else if loc != nil {
			dict = []eleven.DictLocator{*loc}
			res.Pronounce.Dictionary, res.Pronounce.Version = loc.ID, loc.VersionID
		}
	}
	g.Go(func() error {
		ta := time.Now()
		defer func() { timings["dialogue"] = time.Since(ta).Seconds() }()
		var prev []string
		var mu sync.Mutex
		dg, dctx := errgroup.WithContext(gctx)
		_, known := noContinuity.Load(model)
		parallel := sp.ParallelDialogue || known
		for bi, b := range blocks {
			req := eleven.DialogueRequest{ModelID: model, LanguageCode: sp.Language, Seed: sp.Seed, Dictionaries: dict, AudioEffects: b[0].fx}
			for _, in := range b {
				req.Inputs = append(req.Inputs, in.DialogueInput)
			}
			if sp.Stability != nil {
				req.Settings = &struct {
					Stability *float64 `json:"stability,omitempty"`
				}{sp.Stability}
			}
			run := func(req eleven.DialogueRequest) error {
				r, err := env.Client.Dialogue(dctx, req, cache)
				if err != nil {
					return fmt.Errorf("dialogue block %d: %w", bi+1, err)
				}
				buf, err := audio.DecodeMP3(r.Audio)
				if err != nil {
					return err
				}
				mu.Lock()
				bouts[bi] = blockOut{buf, r.Segments}
				if r.RequestID != "" {
					prev = append(prev, r.RequestID)
				}
				mu.Unlock()
				step(fmt.Sprintf("dialogue %d/%d", bi+1, len(blocks)))
				return nil
			}
			if parallel {
				dg.Go(func() error { return run(req) })
				continue
			}
			if len(prev) > 0 {
				req.PreviousRequestIDs = prev[max(0, len(prev)-3):]
			}
			err := run(req)
			var ae *eleven.APIError
			if err != nil && req.PreviousRequestIDs != nil && errors.As(err, &ae) && ae.Status == 400 && strings.Contains(ae.Body, "not supported") {
				// This model can't take continuity hints: remember, drop them, go parallel.
				noContinuity.Store(model, true)
				parallel = true
				req.PreviousRequestIDs = nil
				dg.Go(func() error { return run(req) })
				continue
			}
			if err != nil {
				return err
			}
		}
		return dg.Wait()
	})
	sfxBufs := make([]*audio.Buffer, len(sfx))
	sfxCached := make([]bool, len(sfx))
	for i, c := range sfx {
		g.Go(func() error {
			b, cached, err := loadOrGenSFX(gctx, env, c, false, cache)
			if err != nil {
				return fmt.Errorf("sfx %q: %w", label(c), err)
			}
			sfxBufs[i], sfxCached[i] = b, cached
			if c.File == "" {
				step("sfx: " + label(c))
			}
			return nil
		})
	}
	ambBufs := make([]*audio.Buffer, len(amb))
	ambCached := make([]bool, len(amb))
	for i, c := range amb {
		g.Go(func() error {
			if c.DurationSec == 0 {
				c.DurationSec = 22
			}
			b, cached, err := loadOrGenSFX(gctx, env, c, true, cache)
			if err != nil {
				return fmt.Errorf("ambience %q: %w", label(c), err)
			}
			ambBufs[i], ambCached[i] = b, cached
			if c.File == "" {
				step("ambience: " + label(c))
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// ---- timeline
	leadIn := deref(sp.LeadIn, 0.6)
	tail := deref(sp.Tail, 2)
	timed := make([]TimedLine, len(lines))
	for i, l := range lines {
		timed[i] = TimedLine{N: i + 1, Speaker: l.Speaker, VoiceID: cast[l.Speaker], Text: l.Text, Start: -1, End: -1}
		if l.VoiceID != "" {
			timed[i].VoiceID = l.VoiceID
		}
	}
	var dlgTracks []audio.Track
	off := leadIn
	end := 0.0
	for bi, b := range blocks {
		bo := bouts[bi]
		dlgTracks = append(dlgTracks, audio.Track{Buf: bo.buf, Start: off})
		for _, s := range bo.segs {
			if s.InputIndex < 0 || s.InputIndex >= len(b) {
				continue
			}
			tl := &timed[b[s.InputIndex].line]
			if tl.Start < 0 || off+s.Start < tl.Start {
				tl.Start = off + s.Start
			}
			tl.End = math.Max(tl.End, off+s.End)
		}
		// Advance from where the speech ends, not where the audio ends: a reverb tail
		// then rings under the next line instead of leaving a gap.
		speechEnd := 0.0
		for _, s := range bo.segs {
			speechEnd = math.Max(speechEnd, s.End)
		}
		if speechEnd <= 0 || speechEnd > bo.buf.Seconds() {
			speechEnd = bo.buf.Seconds()
		} else {
			speechEnd += blockGap // a natural breath between turns, even if the clip ends abruptly
		}
		end = math.Max(end, off+bo.buf.Seconds()) // keep reverb tails inside the scene
		off += speechEnd + lines[b[len(b)-1].line].PauseAfter
	}
	for i := range timed { // fill any line the API returned no segment for
		if timed[i].Start < 0 {
			prevEnd := leadIn
			if i > 0 {
				prevEnd = timed[i-1].End
			}
			timed[i].Start, timed[i].End = prevEnd, prevEnd
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d had no timing data; cues anchored to it are approximate", i+1))
		}
	}
	end = math.Max(end, off) + tail
	if len(lines) == 0 {
		end = sp.LengthSec
	}

	// ---- phase B: music needs the final length
	tb := time.Now()
	musBufs := make([]*audio.Buffer, len(music))
	musCached := make([]bool, len(music))
	mspan := make([][2]float64, len(music))
	g2, g2ctx := errgroup.WithContext(ctx)
	for i, c := range music {
		from, _ := anchor(def(c.From, "start"), timed, end)
		to, _ := anchor(def(c.To, "end"), timed, end)
		if to <= from {
			return nil, fmt.Errorf("music %q ends (%.1fs) before it starts (%.1fs)", label(c), to, from)
		}
		mspan[i] = [2]float64{from, to}
		g2.Go(func() error {
			if c.File != "" {
				b, err := audio.LoadFile(c.File)
				musBufs[i] = b
				return err
			}
			// Round length up to 5 s so small script edits still hit the cache.
			ms := int(math.Ceil((to-from)/5)*5) * 1000
			ms = min(max(ms, 10000), 600000)
			req := eleven.MusicRequest{Prompt: c.Prompt, MusicLengthMS: ms, ModelID: def(c.Model, "music_v1"), ForceInstrumental: !c.Vocals}
			r, err := env.Client.Music(g2ctx, req, cache)
			if err != nil {
				return fmt.Errorf("music %q: %w", label(c), err)
			}
			b, err := audio.DecodeMP3(r.Body)
			musBufs[i], musCached[i] = b, r.Cached
			step("music: " + label(c))
			return err
		})
	}
	if err := g2.Wait(); err != nil {
		return nil, err
	}
	timings["music"] = time.Since(tb).Seconds()

	// ---- mix
	tm := time.Now()
	var tracks []audio.Track
	for i, c := range music {
		g := deref(c.GainDB, -15)
		tracks = append(tracks, audio.Track{Buf: musBufs[i], Start: mspan[i][0], End: mspan[i][1], Loop: true, XFade: 2,
			GainDB: g, FadeIn: deref(c.FadeIn, 2), FadeOut: deref(c.FadeOut, 3), Pan: c.Pan, Ducked: c.Duck == nil || *c.Duck})
		res.Cues = append(res.Cues, TimedCue{"music", label(c), r2(mspan[i][0]), r2(mspan[i][1]), g, musCached[i]})
	}
	for i, c := range amb {
		from, _ := anchor(def(c.From, "start"), timed, end)
		to, _ := anchor(def(c.To, "end"), timed, end)
		g := deref(c.GainDB, -24)
		tracks = append(tracks, audio.Track{Buf: ambBufs[i], Start: from, End: to, Loop: true, XFade: 0.5,
			GainDB: g, FadeIn: deref(c.FadeIn, 1.5), FadeOut: deref(c.FadeOut, 2), Pan: c.Pan, Ducked: c.Duck != nil && *c.Duck})
		res.Cues = append(res.Cues, TimedCue{"ambience", label(c), r2(from), r2(to), g, ambCached[i]})
	}
	for i, c := range sfx {
		at, _ := anchor(def(c.At, "start"), timed, end)
		g := deref(c.GainDB, -6)
		tracks = append(tracks, audio.Track{Buf: sfxBufs[i], Start: math.Max(0, at), GainDB: g,
			FadeIn: deref(c.FadeIn, 0), FadeOut: deref(c.FadeOut, 0.05), Pan: c.Pan, Ducked: c.Duck != nil && *c.Duck})
		res.Cues = append(res.Cues, TimedCue{"sfx", label(c), r2(at), 0, g, sfxCached[i]})
		if at+sfxBufs[i].Seconds() > end {
			end = at + sfxBufs[i].Seconds()
		}
	}
	mix := audio.Mix(dlgTracks, tracks, audio.MixOptions{DuckDB: deref(sp.DuckDB, -12), TargetLUFS: deref(sp.TargetLUFS, -16), CeilingDB: -1, Length: end})
	timings["mix"] = time.Since(tm).Seconds()

	// ---- write outputs
	te := time.Now()
	dir := config(sp.OutputDir, env.OutputDir)
	ext := "mp3"
	if strings.EqualFold(sp.Format, "wav") {
		ext = "wav"
	}
	name := safeName(def(sp.Filename, def(sp.Title, "scene_"+time.Now().Format("2006-01-02_150405"))))
	res.File = filepath.Join(dir, name+"."+ext)
	if res.Encoder, err = audio.Save(mix.Out, res.File, env.Encoder); err != nil {
		return nil, err
	}
	if sp.Stems {
		var sg errgroup.Group
		stems := map[string]*audio.Buffer{"dialogue": mix.Stems.Dialogue, "music": mix.Stems.Music, "effects": mix.Stems.Effects}
		for _, k := range []string{"dialogue", "music", "effects"} {
			p := filepath.Join(dir, name+"_stems", k+"."+ext)
			res.Stems = append(res.Stems, p)
			sg.Go(func() error { _, err := audio.Save(stems[k], p, env.Encoder); return err })
		}
		if err := sg.Wait(); err != nil {
			return nil, err
		}
	}
	if len(lines) > 0 && !sp.NoSubs {
		res.Subtitles = filepath.Join(dir, name+".srt")
		if err := os.WriteFile(res.Subtitles, []byte(srt(timed)), 0o644); err != nil {
			return nil, err
		}
	}
	res.Seconds = r2(mix.Out.Seconds())
	res.LUFS = r2(mix.OutputLUFS)
	res.PeakDBFS = r2(mix.PeakDB)
	res.Timeline = timed
	for i := range res.Timeline {
		res.Timeline[i].Start, res.Timeline[i].End = r2(res.Timeline[i].Start), r2(res.Timeline[i].End)
	}
	sort.SliceStable(res.Cues, func(i, j int) bool { return res.Cues[i].Start < res.Cues[j].Start })
	res.CacheHits = env.Client.Hits.Load() - hits0
	res.CueSheet = filepath.Join(dir, name+".cues.json")
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		_ = os.WriteFile(res.CueSheet, b, 0o644)
	}
	timings["encode"] = time.Since(te).Seconds()
	timings["total"] = time.Since(t0).Seconds()
	for k, v := range timings {
		timings[k] = r2(v)
	}
	res.TimingsSec = timings
	step("done")
	return res, nil
}

// ---------------------------------------------------------------- helpers

func loadOrGenSFX(ctx context.Context, env Env, c Cue, loop, cache bool) (*audio.Buffer, bool, error) {
	if c.File != "" {
		b, err := audio.LoadFile(c.File)
		return b, false, err
	}
	if c.Prompt == "" {
		return nil, false, errors.New("cue needs a prompt or a file")
	}
	req := eleven.SFXRequest{Text: c.Prompt, Loop: loop, PromptInfluence: c.PromptInfluence, ModelID: c.Model}
	if c.DurationSec > 0 {
		d := math.Min(30, math.Max(0.5, c.DurationSec))
		req.DurationSeconds = &d
	}
	r, err := env.Client.SoundEffect(ctx, req, cache)
	if err != nil {
		return nil, false, err
	}
	b, err := audio.DecodeMP3(r.Body)
	return b, r.Cached, err
}

func resolveVoices(ctx context.Context, env Env, voices map[string]string, lines []Line) (map[string]string, error) {
	out := map[string]string{}
	lc := map[string]string{}
	for k, v := range voices {
		lc[strings.ToLower(strings.TrimSpace(k))] = v
	}
	var missing []string
	for _, l := range lines {
		if l.VoiceID != "" || l.Speaker == "" {
			if l.VoiceID == "" {
				return nil, fmt.Errorf("line %q has neither speaker nor voice_id", l.Text)
			}
			continue
		}
		if _, ok := out[l.Speaker]; ok {
			continue
		}
		k := strings.ToLower(l.Speaker)
		switch {
		case lc[k] != "":
			out[l.Speaker] = lc[k]
		case env.Cast != nil && func() bool { v, ok := env.Cast.Lookup(l.Speaker); out[l.Speaker] = v; return ok }():
		case voiceIDRe.MatchString(l.Speaker):
			out[l.Speaker] = l.Speaker
		default:
			delete(out, l.Speaker)
			missing = append(missing, l.Speaker)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	// Last resort: match voice library names ("Skerrit" matches "TW - Skerrit").
	lib, err := env.Client.Voices(ctx, "", "", 100)
	if err != nil {
		return nil, fmt.Errorf("unknown speakers %v and voice library lookup failed: %w", missing, err)
	}
	var still []string
	for _, sp := range missing {
		k := strings.ToLower(sp)
		found := ""
		for _, v := range lib {
			n := strings.ToLower(v.Name)
			if n == k || strings.HasSuffix(n, " - "+k) || strings.HasPrefix(n, k+" - ") {
				found = v.VoiceID
				break
			}
		}
		if found == "" {
			still = append(still, sp)
			continue
		}
		out[sp] = found
	}
	if len(still) > 0 {
		return nil, fmt.Errorf("no voice for speaker(s) %s: add them to \"voices\", save them with manage_cast, or use search_voices to find a voice_id", strings.Join(still, ", "))
	}
	return out, nil
}

// makeBlocks groups inputs into dialogue requests: break on pauses, size and the 10-voice limit.
func makeBlocks(inputs []input, lines []Line) [][]input {
	var blocks [][]input
	var cur []input
	chars := 0
	voices := map[string]bool{}
	for i, in := range inputs {
		nv := !voices[in.VoiceID]
		fxChange := len(cur) > 0 && cur[len(cur)-1].fx.Key() != in.fx.Key()
		if len(cur) > 0 && (fxChange || chars+len(in.Text) > maxBlockChars || (nv && len(voices) >= 10)) {
			blocks = append(blocks, cur)
			cur, chars, voices = nil, 0, map[string]bool{}
		}
		cur = append(cur, in)
		chars += len(in.Text)
		voices[in.VoiceID] = true
		lastPart := i == len(inputs)-1 || inputs[i+1].line != in.line
		if lastPart && lines[in.line].PauseAfter > 0 {
			blocks = append(blocks, cur)
			cur, chars, voices = nil, 0, map[string]bool{}
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks
}

var sentenceRe = regexp.MustCompile(`[^.!?…]+[.!?…]+["')\]]*\s*|[^.!?…]+$`)

func splitText(s string, maxLen int) []string {
	if len(s) <= maxLen {
		return []string{s}
	}
	var out []string
	cur := ""
	for _, p := range sentenceRe.FindAllString(s, -1) {
		if len(cur)+len(p) > maxLen && cur != "" {
			out = append(out, strings.TrimSpace(cur))
			cur = ""
		}
		for len(p) > maxLen {
			cut := strings.LastIndex(p[:maxLen], " ")
			if cut <= 0 {
				cut = maxLen
			}
			out = append(out, strings.TrimSpace(p[:cut]))
			p = p[cut:]
		}
		cur += p
	}
	if strings.TrimSpace(cur) != "" {
		out = append(out, strings.TrimSpace(cur))
	}
	return out
}

var anchorRe = regexp.MustCompile(`^(start|end|line:(\d+)|after:(\d+)|(\d+(?:\.\d+)?)s?)\s*([+-]\s*\d+(?:\.\d+)?)?s?$`)

// anchor resolves an anchor expression to seconds.
func anchor(a string, lines []TimedLine, end float64) (float64, error) {
	m := anchorRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(a)))
	if m == nil {
		return 0, fmt.Errorf("bad anchor %q: use start, end, line:N, after:N or seconds, optionally +/-offset", a)
	}
	var t float64
	switch {
	case m[1] == "start":
	case m[1] == "end":
		t = end
	case m[2] != "" || m[3] != "":
		n, _ := strconv.Atoi(m[2] + m[3])
		if n < 1 || n > len(lines) {
			return 0, fmt.Errorf("anchor %q: line %d does not exist (script has %d lines)", a, n, len(lines))
		}
		if m[2] != "" {
			t = lines[n-1].Start
		} else {
			t = lines[n-1].End
		}
	default:
		t, _ = strconv.ParseFloat(m[4], 64)
	}
	if m[5] != "" {
		d, _ := strconv.ParseFloat(strings.ReplaceAll(m[5], " ", ""), 64)
		t += d
	}
	return t, nil
}

func srt(lines []TimedLine) string {
	var b strings.Builder
	ts := func(s float64) string {
		ms := int(math.Round(math.Max(0, s) * 1000))
		return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
	}
	for i, l := range lines {
		text := strings.Join(strings.Fields(tagStrip.ReplaceAllString(l.Text, "")), " ")
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s: %s\n\n", i+1, ts(l.Start), ts(l.End), l.Speaker, text)
	}
	return b.String()
}

func label(c Cue) string {
	if c.File != "" {
		return filepath.Base(c.File)
	}
	if len(c.Prompt) > 60 {
		return c.Prompt[:60] + "…"
	}
	return c.Prompt
}

func def(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func deref(p *float64, d float64) float64 {
	if p == nil {
		return d
	}
	return *p
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

func config(dir, fallback string) string {
	if dir == "" {
		dir = fallback
	}
	if strings.HasPrefix(dir, "~/") {
		h, _ := os.UserHomeDir()
		dir = filepath.Join(h, dir[2:])
	}
	return dir
}

var unsafeRe = regexp.MustCompile(`[/\\:*?"<>|]+`)

func safeName(s string) string {
	s = strings.TrimSpace(unsafeRe.ReplaceAllString(s, "_"))
	s = strings.TrimSuffix(strings.TrimSuffix(s, ".mp3"), ".wav")
	if s == "" {
		s = "scene"
	}
	return s
}
