// Package scene turns a script (screenplay text or structured lines) plus music,
// ambience and sound-effect cues into one mixed audio file.
package scene

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/swiftj/goelevenlabs/internal/eleven"
)

// Line is one spoken line.
type Line struct {
	Speaker    string          `json:"speaker,omitempty" jsonschema:"speaker name; resolved through voices, then the saved cast, then voice library names"`
	VoiceID    string          `json:"voice_id,omitempty" jsonschema:"explicit voice_id (overrides speaker lookup)"`
	Text       string          `json:"text" jsonschema:"what is said; eleven_v3 audio tags like [whispers] [angry] [laughs] [sighs] work inline"`
	PauseAfter float64         `json:"pause_after,omitempty" jsonschema:"seconds of silence after this line"`
	Effects    *eleven.Effects `json:"effects,omitempty" jsonschema:"voice effects for just this line (overrides character effects; {} means none)"`
}

// Cue is a music bed, ambience bed or one-shot sound effect.
type Cue struct {
	Prompt          string   `json:"prompt,omitempty" jsonschema:"text prompt to generate the sound or music"`
	File            string   `json:"file,omitempty" jsonschema:"use an existing local .mp3/.wav instead of generating"`
	At              string   `json:"at,omitempty" jsonschema:"SFX position. Anchors: start | end | line:N (start of line N, 1-based) | after:N (end of line N) | seconds, each optionally +/-offset, e.g. after:3+0.4"`
	From            string   `json:"from,omitempty" jsonschema:"bed start anchor (default start)"`
	To              string   `json:"to,omitempty" jsonschema:"bed end anchor (default end)"`
	GainDB          *float64 `json:"gain_db,omitempty" jsonschema:"level in dB (defaults: music -15, ambience -24, sfx -6)"`
	FadeIn          *float64 `json:"fade_in,omitempty"`
	FadeOut         *float64 `json:"fade_out,omitempty"`
	Pan             float64  `json:"pan,omitempty" jsonschema:"-1 left .. 1 right"`
	DurationSec     float64  `json:"duration_sec,omitempty" jsonschema:"SFX length 0.5-30 s (default: model decides)"`
	PromptInfluence *float64 `json:"prompt_influence,omitempty" jsonschema:"SFX prompt adherence 0-1"`
	Vocals          bool     `json:"vocals,omitempty" jsonschema:"music only: allow vocals (default instrumental)"`
	Model           string   `json:"model,omitempty" jsonschema:"override model (music_v1/music_v2/music_v2_5; eleven_text_to_sound_v2/v3)"`
	Seed            *int64   `json:"seed,omitempty"`
	Duck            *bool    `json:"duck,omitempty" jsonschema:"duck under dialogue (default true for music, false otherwise)"`
}

var (
	tagRe     = regexp.MustCompile(`^\s*\{\s*([a-zA-Z]+)\s*:\s*(.*?)\s*\}\s*$`)
	speakerRe = regexp.MustCompile(`^\s*([^:\[\]{}]{1,40}?)\s*:\s*(.+)$`)
	dbRe      = regexp.MustCompile(`^([+-]?\d+(?:\.\d+)?)\s*db$`)
	secRe     = regexp.MustCompile(`^(\d+(?:\.\d+)?)s$`)
)

// Parsed is the result of parsing a screenplay script.
type Parsed struct {
	Lines    []Line
	SFX      []Cue
	Music    []Cue
	Ambience []Cue
}

// ParseScript parses screenplay-style text:
//
//	NARRATOR: [softly] The fire crackles low.
//	SKERRIT: [chuckles] Sit, child.
//	{sfx: wolf howl in the distance | -10dB | pan=-0.6}
//	{music: low tense strings, dark fantasy | -16dB | fade=3}
//	{ambience: night forest, crickets, fire crackle}
//	{pause: 1.5}
//	{music: stop}
//
// Lines without "NAME:" continue the previous speaker. "#" and "//" start comments.
func ParseScript(src string) (*Parsed, error) {
	p := &Parsed{}
	openMusic, openAmb := -1, -1
	voicefx := map[string]*eleven.Effects{} // lower(speaker) or "all" -> effects for following lines
	boundary := func() string {             // anchor for "here", between lines
		if len(p.Lines) == 0 {
			return "start"
		}
		return "after:" + strconv.Itoa(len(p.Lines))
	}
	for ln, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "//") {
			continue
		}
		if m := tagRe.FindStringSubmatch(s); m != nil {
			kind := strings.ToLower(m[1])
			parts := strings.Split(m[2], "|")
			body := strings.TrimSpace(parts[0])
			if kind == "voicefx" || kind == "vfx" {
				who := strings.ToLower(body)
				if who == "" {
					return nil, fmt.Errorf("line %d: {voicefx: NAME | env=hall ...} needs a speaker name (or all)", ln+1)
				}
				fx, err := parseEffects(parts[1:])
				if err != nil {
					return nil, fmt.Errorf("line %d: %v", ln+1, err)
				}
				voicefx[who] = fx
				continue
			}
			if kind == "pause" {
				sec, err := strconv.ParseFloat(strings.TrimSuffix(body, "s"), 64)
				if err != nil {
					return nil, fmt.Errorf("line %d: bad pause %q", ln+1, body)
				}
				if len(p.Lines) > 0 {
					p.Lines[len(p.Lines)-1].PauseAfter += sec
				}
				continue
			}
			c := Cue{}
			if strings.HasPrefix(body, "file=") {
				c.File = strings.TrimSpace(strings.TrimPrefix(body, "file="))
			} else {
				c.Prompt = body
			}
			at := boundary()
			if err := applyOpts(&c, parts[1:], &at); err != nil {
				return nil, fmt.Errorf("line %d: %v", ln+1, err)
			}
			stop := strings.EqualFold(body, "stop") || strings.EqualFold(body, "off")
			switch kind {
			case "sfx", "sound", "fx":
				c.At = at
				p.SFX = append(p.SFX, c)
			case "music":
				if openMusic >= 0 {
					p.Music[openMusic].To = at
					openMusic = -1
				}
				if !stop {
					c.From = at
					p.Music = append(p.Music, c)
					openMusic = len(p.Music) - 1
				}
			case "ambience", "ambient", "amb", "bg":
				if openAmb >= 0 {
					p.Ambience[openAmb].To = at
					openAmb = -1
				}
				if !stop {
					c.From = at
					p.Ambience = append(p.Ambience, c)
					openAmb = len(p.Ambience) - 1
				}
			default:
				return nil, fmt.Errorf("line %d: unknown tag {%s:} (use sfx, music, ambience, voicefx, pause)", ln+1, kind)
			}
			continue
		}
		if m := speakerRe.FindStringSubmatch(s); m != nil && !strings.HasPrefix(s, "[") {
			l := Line{Speaker: strings.TrimSpace(m[1]), Text: strings.TrimSpace(m[2])}
			if fx, ok := voicefx[strings.ToLower(l.Speaker)]; ok {
				l.Effects = fx
			} else if fx, ok := voicefx["all"]; ok {
				l.Effects = fx
			}
			p.Lines = append(p.Lines, l)
			continue
		}
		if len(p.Lines) == 0 {
			return nil, fmt.Errorf("line %d: %q has no speaker; start lines with NAME:", ln+1, s)
		}
		last := &p.Lines[len(p.Lines)-1]
		last.Text += " " + s
	}
	return p, nil
}

func applyOpts(c *Cue, opts []string, at *string) error {
	for _, o := range opts {
		o = strings.ToLower(strings.TrimSpace(o))
		if o == "" {
			continue
		}
		f := func(s string) (float64, error) { return strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64) }
		k, v, hasEq := strings.Cut(o, "=")
		switch {
		case dbRe.MatchString(o):
			g, _ := strconv.ParseFloat(dbRe.FindStringSubmatch(o)[1], 64)
			c.GainDB = &g
		case secRe.MatchString(o):
			d, _ := f(o)
			c.DurationSec = d
		case o == "vocals":
			c.Vocals = true
		case o == "duck":
			t := true
			c.Duck = &t
		case o == "noduck":
			fl := false
			c.Duck = &fl
		case hasEq:
			x, err := f(v)
			if err != nil && k != "model" && k != "at" {
				return fmt.Errorf("bad option %q", o)
			}
			switch k {
			case "fade":
				c.FadeIn, c.FadeOut = &x, &x
			case "fadein", "fade_in":
				c.FadeIn = &x
			case "fadeout", "fade_out":
				c.FadeOut = &x
			case "dur", "duration":
				c.DurationSec = x
			case "pan":
				c.Pan = x
			case "influence":
				c.PromptInfluence = &x
			case "gain":
				c.GainDB = &x
			case "seed":
				s := int64(x)
				c.Seed = &s
			case "model":
				c.Model = v
			case "at":
				if strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-") {
					*at += v // nudge relative to where the tag sits, e.g. at=+0.4
				} else {
					*at = v // absolute anchor, e.g. at=line:3 or at=12.5
				}
			default:
				return fmt.Errorf("unknown option %q", o)
			}
		default:
			return fmt.Errorf("unknown option %q", o)
		}
	}
	return nil
}

// parseEffects parses voicefx options: env=hall filter=phone noise=cafe distance=0.5
// send=0.6 pan=-0.3, or "none"/"off" to clear (an empty, non-nil Effects).
func parseEffects(opts []string) (*eleven.Effects, error) {
	fx := &eleven.Effects{}
	for _, o := range opts {
		o = strings.ToLower(strings.TrimSpace(o))
		if o == "" || o == "none" || o == "off" || o == "dry" {
			continue
		}
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			return nil, fmt.Errorf("voicefx option %q: use key=value (env, filter, noise, distance, send, pan) or none", o)
		}
		num := func() (*float64, error) {
			x, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, fmt.Errorf("voicefx %s: %q is not a number", k, v)
			}
			return &x, nil
		}
		var err error
		switch k {
		case "env", "environment", "room", "reverb":
			fx.EnvironmentID = v
		case "filter", "preset":
			fx.FilterPresetID = v
		case "noise", "bg", "background":
			fx.BackgroundNoiseID = v
		case "distance", "dist":
			fx.Distance, err = num()
		case "send", "wet":
			fx.SendLevel, err = num()
		case "pan":
			fx.Pan, err = num()
		default:
			return nil, fmt.Errorf("unknown voicefx option %q (env, filter, noise, distance, send, pan)", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return fx, nil
}
