// Package server exposes the engine as MCP tools.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swiftj/goelevenlabs/internal/audio"
	"github.com/swiftj/goelevenlabs/internal/config"
	"github.com/swiftj/goelevenlabs/internal/eleven"
	"github.com/swiftj/goelevenlabs/internal/scene"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is set at build time.
var Version = "dev"

// App holds shared state.
type App struct {
	Cfg    *config.Config
	Client *eleven.Client
	Cast   *scene.Cast
	Lex    *scene.Lexicon
	player struct {
		sync.Mutex
		cmd *exec.Cmd
	}
}

// NewApp loads config and builds the client.
func NewApp() (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return &App{
		Cfg:    cfg,
		Client: eleven.New(cfg.APIKey, cfg.BaseURL, cfg.CacheDir, cfg.MaxConcurrency),
		Cast:   scene.OpenCast(config.Dir()),
		Lex:    scene.OpenLexicon(config.Dir()),
	}, nil
}

// Env returns a scene environment with an optional progress callback.
func (a *App) Env(progress func(string, int, int)) scene.Env {
	return scene.Env{Client: a.Client, Cast: a.Cast, Lexicon: a.Lex, OutputDir: a.Cfg.OutputDir, Encoder: a.Cfg.Encoder, Progress: progress}
}

const instructions = `ElevenLabs audio production. Use render_scene to turn a script into ONE finished audio file
with multiple voices, a music bed that ducks under speech, ambience and timed sound effects,
loudness-normalised, with .srt subtitles and a cue sheet. Workflow:
1. search_voices / manage_cast to give each character a voice (the cast is remembered). Characters can
   also carry default voice effects (e.g. a lich in hall reverb, a sending stone through the phone filter).
2. manage_pronunciation for names the voices get wrong; preview to check, then it applies to every render.
3. render_scene with dry_run:true to check voices and size, then without it to render.
4. Identical requests are cached, so re-rendering after changing levels or cues costs no credits;
   pass fresh:true for a new take. Use play_audio to listen (macOS).
Read the prompt "scene_script_guide" for the script syntax.`

const scriptGuide = `# Scene script syntax (render_scene "script")

NARRATOR: [softly] Night falls over the ruins of Thay.
SZASS TAM: [cold, measured] You came all this way to die.
HERO: [defiant] Not today.
{sfx: thunder crack overhead | -4dB}
{music: dark choral dread, low strings, slow | -17dB | fade=4}
{ambience: wind through stone ruins, distant crows}
{pause: 1.2}
{voicefx: SZASS TAM | env=hall | send=0.7}
SZASS TAM: [whispers] Then kneel.
{sfx: sword unsheathed | at=+0.3 | pan=0.4 | 1.5s}
{music: stop}

- "NAME: text" is a line. Untagged lines continue the previous speaker. "#" comments.
- eleven_v3 audio tags inside text: [whispers] [shouts] [laughs] [sighs] [sarcastic] [excited] [crying] [pause] …
- Tags sit where they appear in the script (between lines). Options after "|":
  -6dB (gain), fade=3 / fadein= / fadeout=, pan=-1..1, 2s or dur=2 (sfx length), influence=0.7,
  at=+0.4 (nudge) or at=line:3 / at=after:3 / at=12.5 (absolute), vocals (music), duck / noduck, model=…
- {music: …} runs until the next {music: …}, {music: stop} or the end, and ducks under dialogue.
- {ambience: …} loops seamlessly until replaced/stopped. {sfx: file=/path/door.wav} uses a local file.
- {pause: 1.5} inserts silence after the previous line.
- {voicefx: SZASS TAM | env=hall | send=0.6 | distance=0.3} gives that speaker voice effects from here on;
  {voicefx: NAME | none} turns them off; NAME can be "all". Options: env (reverb space), filter
  (phone, old_radio, …), noise (background bed), distance 0-1, send 0-1, pan -1..1.
  See voice_effects_catalog for valid names. Characters can also keep default effects in manage_cast.
- Names in the pronunciation lexicon (manage_pronunciation) are fixed automatically.`

// New builds the MCP server.
func (a *App) New() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "goelevenlabs", Title: "ElevenLabs Studio (Go)", Version: Version},
		&mcp.ServerOptions{Instructions: instructions})

	s.AddPrompt(&mcp.Prompt{Name: "scene_script_guide", Description: "Syntax for render_scene scripts: dialogue, audio tags, music/ambience/sfx cues."},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: scriptGuide}}}}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "render_scene", Description: "Render a scripted scene into ONE mixed audio file: multi-voice dialogue (eleven_v3, up to 10 voices per block) + music beds that duck under speech + looping ambience + sound effects placed relative to lines. Loudness-normalised (-16 LUFS), writes .srt subtitles and a .cues.json cue sheet, optional stems. Generation runs in parallel and is cached by content. Script syntax:\n" + scriptGuide},
		func(ctx context.Context, req *mcp.CallToolRequest, sp scene.Spec) (*mcp.CallToolResult, *scene.Result, error) {
			r, err := scene.Render(ctx, a.Env(progressFn(ctx, req)), sp)
			return nil, r, err
		})

	type dialogueIn struct {
		Script           string                    `json:"script,omitempty" jsonschema:"'NAME: text' lines"`
		Lines            []scene.Line              `json:"lines,omitempty"`
		Voices           map[string]string         `json:"voices,omitempty"`
		Filename         string                    `json:"filename,omitempty"`
		OutputDir        string                    `json:"output_dir,omitempty"`
		Format           string                    `json:"format,omitempty"`
		Model            string                    `json:"model,omitempty"`
		Stability        *float64                  `json:"stability,omitempty"`
		VoiceEffects     *eleven.Effects           `json:"voice_effects,omitempty"`
		CharacterEffects map[string]eleven.Effects `json:"character_effects,omitempty" jsonschema:"speaker -> voice effects"`
		Fresh            bool                      `json:"fresh,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "generate_dialogue", Description: "Multi-voice dialogue only (no music/effects) as ONE audio file with per-line timings and .srt. Long scripts are split into continuity-linked requests automatically. Same script syntax as render_scene."},
		func(ctx context.Context, req *mcp.CallToolRequest, in dialogueIn) (*mcp.CallToolResult, *scene.Result, error) {
			sp := scene.Spec{Script: in.Script, Lines: in.Lines, Voices: in.Voices, Filename: in.Filename, OutputDir: in.OutputDir,
				Format: in.Format, Model: in.Model, Stability: in.Stability, VoiceEffects: in.VoiceEffects, Fresh: in.Fresh}
			zero, short := 0.0, 0.3
			sp.LeadIn, sp.Tail = &short, &short
			sp.DuckDB = &zero
			if in.Script != "" {
				if p, err := scene.ParseScript(in.Script); err == nil && len(p.Music)+len(p.Ambience)+len(p.SFX) > 0 {
					return nil, nil, errors.New("script contains music/ambience/sfx cues: use render_scene")
				}
			}
			sp.CharacterEffects = in.CharacterEffects
			r, err := scene.Render(ctx, a.Env(progressFn(ctx, req)), sp)
			return nil, r, err
		})

	type sfxIn struct {
		Prompt          string   `json:"prompt" jsonschema:"what the sound is, e.g. 'heavy wooden door creaks open, echoing stone hall'"`
		DurationSec     float64  `json:"duration_sec,omitempty" jsonschema:"0.5-30; omit to let the model decide"`
		Loop            bool     `json:"loop,omitempty" jsonschema:"seamlessly loopable (ambience)"`
		PromptInfluence *float64 `json:"prompt_influence,omitempty"`
		Model           string   `json:"model,omitempty"`
		Filename        string   `json:"filename,omitempty"`
		OutputDir       string   `json:"output_dir,omitempty"`
		Fresh           bool     `json:"fresh,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "generate_sound_effect", Description: "Generate one sound effect (or a seamless loop) and save it as MP3."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in sfxIn) (*mcp.CallToolResult, *FileOut, error) {
			r := eleven.SFXRequest{Text: in.Prompt, Loop: in.Loop, PromptInfluence: in.PromptInfluence, ModelID: in.Model}
			if in.DurationSec > 0 {
				d := math.Min(30, math.Max(0.5, in.DurationSec))
				r.DurationSeconds = &d
			}
			res, err := a.Client.SoundEffect(ctx, r, !in.Fresh)
			if err != nil {
				return nil, nil, err
			}
			return nil, a.writeRaw(res, in.OutputDir, nameOr(in.Filename, "sfx_"+slug(in.Prompt))), nil
		})

	type musicIn struct {
		Prompt          string          `json:"prompt,omitempty" jsonschema:"genre, mood, instruments, tempo"`
		CompositionPlan json.RawMessage `json:"composition_plan,omitempty" jsonschema:"advanced: ElevenLabs composition plan (sections with styles and durations) instead of prompt"`
		LengthSec       float64         `json:"length_sec,omitempty" jsonschema:"3-600; omit to let the model decide"`
		Vocals          bool            `json:"vocals,omitempty" jsonschema:"allow vocals (default instrumental)"`
		Model           string          `json:"model,omitempty" jsonschema:"music_v1 (default), music_v2, music_v2_5"`
		Filename        string          `json:"filename,omitempty"`
		OutputDir       string          `json:"output_dir,omitempty"`
		Fresh           bool            `json:"fresh,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "generate_music", Description: "Compose music (instrumental by default) and save it as MP3."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in musicIn) (*mcp.CallToolResult, *FileOut, error) {
			r := eleven.MusicRequest{Prompt: in.Prompt, CompositionPlan: in.CompositionPlan, ModelID: in.Model}
			if len(in.CompositionPlan) == 0 {
				r.ForceInstrumental = !in.Vocals
				if in.LengthSec > 0 {
					r.MusicLengthMS = int(math.Min(600, math.Max(3, in.LengthSec)) * 1000)
				}
			}
			if in.Prompt == "" && len(in.CompositionPlan) == 0 {
				return nil, nil, errors.New("give a prompt or a composition_plan")
			}
			res, err := a.Client.Music(ctx, r, !in.Fresh)
			if err != nil {
				return nil, nil, err
			}
			return nil, a.writeRaw(res, in.OutputDir, nameOr(in.Filename, "music_"+slug(in.Prompt))), nil
		})

	type voicesIn struct {
		Search   string `json:"search,omitempty" jsonschema:"filter by name/description/labels"`
		Category string `json:"category,omitempty" jsonschema:"premade, cloned, generated, professional"`
		Limit    int    `json:"limit,omitempty" jsonschema:"max results (default 30, max 100)"`
	}
	type voicesOut struct {
		Voices []eleven.Voice `json:"voices"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "search_voices", Description: "Search your ElevenLabs voice library (premade, designed, cloned, professional) for voice_ids.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in voicesIn) (*mcp.CallToolResult, *voicesOut, error) {
			lim := in.Limit
			if lim <= 0 {
				lim = 30
			}
			v, err := a.Client.Voices(ctx, in.Search, in.Category, min(lim, 100))
			for i := range v {
				v[i].PreviewURL = ""
				if len(v[i].Description) > 200 {
					v[i].Description = v[i].Description[:200] + "…"
				}
			}
			return nil, &voicesOut{v}, err
		})

	type castIn struct {
		Action    string          `json:"action" jsonschema:"list | set | remove"`
		Name      string          `json:"name,omitempty" jsonschema:"character name as used in scripts"`
		VoiceID   string          `json:"voice_id,omitempty"`
		VoiceName string          `json:"voice_name,omitempty" jsonschema:"set by voice library name instead of id"`
		Notes     string          `json:"notes,omitempty"`
		Group     string          `json:"group,omitempty" jsonschema:"campaign/project grouping; also filters list"`
		Effects   *eleven.Effects `json:"effects,omitempty" jsonschema:"default voice effects for this character, e.g. {\"environment_id\":\"hall\",\"send_level\":0.6} or {\"filter_preset_id\":\"phone\"}. See voice_effects_catalog."`
		ClearFX   bool            `json:"clear_effects,omitempty" jsonschema:"remove the character's saved effects"`
	}
	type castOut struct {
		Cast    []scene.CastMember `json:"cast,omitempty"`
		Message string             `json:"message,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "manage_cast", Description: "Remember which voice (and optional voice effects) plays which character across sessions, so scripts can just say 'SKERRIT: …'. set updates only the fields you give."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in castIn) (*mcp.CallToolResult, *castOut, error) {
			switch strings.ToLower(in.Action) {
			case "list", "":
				return nil, &castOut{Cast: a.Cast.List(in.Group)}, nil
			case "set":
				if in.Name == "" {
					return nil, nil, errors.New("name is required")
				}
				// Update in place: fields not given keep their saved values.
				m, exists := a.Cast.Get(in.Name)
				if !exists {
					m = scene.CastMember{Name: in.Name}
				}
				if in.VoiceID != "" {
					m.VoiceID, m.Voice = in.VoiceID, ""
				}
				for _, p := range []struct {
					dst *string
					v   string
				}{{&m.Notes, in.Notes}, {&m.Group, in.Group}} {
					if p.v != "" {
						*p.dst = p.v
					}
				}
				if in.ClearFX {
					m.Effects = nil
				}
				if in.Effects != nil {
					if cat, err := a.Client.Catalog(ctx); err == nil {
						if err := cat.Validate(in.Effects); err != nil {
							return nil, nil, err
						}
					}
					m.Effects = in.Effects
				}
				if in.VoiceID == "" && in.VoiceName != "" {
					m.VoiceID = ""
				}
				if m.VoiceID == "" {
					if in.VoiceName == "" {
						return nil, nil, errors.New("give voice_id or voice_name")
					}
					vs, err := a.Client.Voices(ctx, in.VoiceName, "", 20)
					if err != nil {
						return nil, nil, err
					}
					for _, v := range vs {
						if strings.EqualFold(v.Name, in.VoiceName) || m.VoiceID == "" {
							m.VoiceID, m.Voice = v.VoiceID, v.Name
							if strings.EqualFold(v.Name, in.VoiceName) {
								break
							}
						}
					}
					if m.VoiceID == "" {
						return nil, nil, fmt.Errorf("no voice named %q", in.VoiceName)
					}
				}
				if err := a.Cast.Set(m); err != nil {
					return nil, nil, err
				}
				return nil, &castOut{Cast: []scene.CastMember{m}, Message: "saved"}, nil
			case "remove":
				ok, err := a.Cast.Remove(in.Name)
				msg := "removed"
				if !ok {
					msg = "not in cast"
				}
				return nil, &castOut{Message: msg}, err
			}
			return nil, nil, fmt.Errorf("unknown action %q", in.Action)
		})

	type mixTrack struct {
		File    string  `json:"file"`
		Role    string  `json:"role,omitempty" jsonschema:"dialogue (sidechain key) | music (ducked) | effects (default)"`
		Start   float64 `json:"start,omitempty" jsonschema:"seconds"`
		End     float64 `json:"end,omitempty" jsonschema:"trim or (with loop) extend to this time"`
		Loop    bool    `json:"loop,omitempty"`
		GainDB  float64 `json:"gain_db,omitempty"`
		FadeIn  float64 `json:"fade_in,omitempty"`
		FadeOut float64 `json:"fade_out,omitempty"`
		Pan     float64 `json:"pan,omitempty"`
	}
	type mixIn struct {
		Tracks     []mixTrack `json:"tracks" jsonschema:"clips to place on the timeline; use start = previous end to concatenate"`
		Sequence   bool       `json:"sequence,omitempty" jsonschema:"ignore start and play tracks back to back (simple concatenation)"`
		Gap        float64    `json:"gap,omitempty" jsonschema:"seconds between tracks when sequence is true"`
		DuckDB     *float64   `json:"duck_db,omitempty" jsonschema:"music dip under dialogue, default -12"`
		TargetLUFS *float64   `json:"target_lufs,omitempty" jsonschema:"default -16; 0 disables"`
		Filename   string     `json:"filename,omitempty"`
		OutputDir  string     `json:"output_dir,omitempty"`
		Format     string     `json:"format,omitempty"`
	}
	type mixOut struct {
		File    string  `json:"file"`
		Seconds float64 `json:"seconds"`
		LUFS    float64 `json:"lufs"`
		PeakDB  float64 `json:"peak_dbfs"`
		Encoder string  `json:"encoder"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "mix_audio", Description: "Mix or join existing local audio files (mp3/wav) on a timeline: concatenate clips, lay music under dialogue with automatic ducking, add effects. Runs locally, costs no credits."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mixIn) (*mcp.CallToolResult, *mixOut, error) {
			if len(in.Tracks) == 0 {
				return nil, nil, errors.New("no tracks")
			}
			bufs := make([]*audio.Buffer, len(in.Tracks))
			var wg sync.WaitGroup
			errs := make([]error, len(in.Tracks))
			for i, t := range in.Tracks {
				wg.Add(1)
				go func() { defer wg.Done(); bufs[i], errs[i] = audio.LoadFile(config.ExpandHome(t.File)) }()
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				return nil, nil, err
			}
			var dlg, other []audio.Track
			cursor := 0.0
			for i, t := range in.Tracks {
				start := t.Start
				if in.Sequence {
					start = cursor
					cursor += bufs[i].Seconds() + in.Gap
				}
				tr := audio.Track{Buf: bufs[i], Start: start, End: t.End, Loop: t.Loop, XFade: 1, GainDB: t.GainDB, FadeIn: t.FadeIn, FadeOut: t.FadeOut, Pan: t.Pan}
				switch strings.ToLower(t.Role) {
				case "dialogue", "voice", "speech":
					dlg = append(dlg, tr)
				case "music", "bed":
					tr.Ducked = true
					other = append(other, tr)
				default:
					other = append(other, tr)
				}
			}
			duck, lufs := -12.0, -16.0
			if in.DuckDB != nil {
				duck = *in.DuckDB
			}
			if in.TargetLUFS != nil {
				lufs = *in.TargetLUFS
			}
			m := audio.Mix(dlg, other, audio.MixOptions{DuckDB: duck, TargetLUFS: lufs, CeilingDB: -1})
			ext := ".mp3"
			if strings.EqualFold(in.Format, "wav") {
				ext = ".wav"
			}
			p := filepath.Join(a.dir(in.OutputDir), nameOr(in.Filename, "mix_"+time.Now().Format("2006-01-02_150405"))+ext)
			enc, err := audio.Save(m.Out, p, a.Cfg.Encoder)
			if err != nil {
				return nil, nil, err
			}
			return nil, &mixOut{p, r2(m.Out.Seconds()), r2(m.OutputLUFS), r2(m.PeakDB), enc}, nil
		})

	type acctOut struct {
		Tier          string  `json:"tier"`
		CreditsUsed   int64   `json:"credits_used"`
		CreditsLimit  int64   `json:"credits_limit"`
		CreditsLeft   int64   `json:"credits_left"`
		ResetsAt      string  `json:"resets_at,omitempty"`
		CacheFiles    int     `json:"cache_files"`
		CacheMB       float64 `json:"cache_mb"`
		CacheHitsRun  int64   `json:"cache_hits_this_session"`
		OutputDir     string  `json:"output_dir"`
		CacheDir      string  `json:"cache_dir"`
		Encoder       string  `json:"mp3_encoder"`
		ServerVersion string  `json:"server_version"`
		Note          string  `json:"note,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "account_status", Description: "Show ElevenLabs plan and credits left, plus this server's cache and settings.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *acctOut, error) {
			sub, err := a.Client.Subscription(ctx)
			note := ""
			if err != nil {
				sub = &eleven.Subscription{Tier: "unknown"}
				note = "credits unavailable: " + err.Error()
				if strings.Contains(err.Error(), "user_read") {
					note = "credits unavailable: this API key lacks the user_read permission (enable it at elevenlabs.io/app/settings/api-keys)"
				}
			}
			files, bytes := a.Client.CacheStats()
			enc := "builtin (pure Go, 128k CBR)"
			if a.Cfg.Encoder != "builtin" && audio.FFmpegPath() != "" {
				enc = "ffmpeg libmp3lame VBR V2 (found " + audio.FFmpegPath() + ")"
			}
			o := &acctOut{Tier: sub.Tier, CreditsUsed: sub.CharacterCount, CreditsLimit: sub.CharacterLimit, CreditsLeft: max(0, sub.CharacterLimit-sub.CharacterCount),
				CacheFiles: files, CacheMB: r2(float64(bytes) / 1e6), CacheHitsRun: a.Client.Hits.Load(), OutputDir: a.Cfg.OutputDir, CacheDir: a.Cfg.CacheDir, Encoder: enc, ServerVersion: Version, Note: note}
			if sub.NextCharacterResetUnix > 0 {
				o.ResetsAt = time.Unix(sub.NextCharacterResetUnix, 0).Format(time.RFC1123)
			}
			return nil, o, nil
		})

	type playIn struct {
		File string `json:"file,omitempty" jsonschema:"audio file to play; omit with stop:true to stop playback"`
		Stop bool   `json:"stop,omitempty"`
	}
	type playOut struct {
		Message string `json:"message"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "play_audio", Description: "Play an audio file on this Mac's speakers (afplay), or stop playback. Returns immediately."},
		func(_ context.Context, _ *mcp.CallToolRequest, in playIn) (*mcp.CallToolResult, *playOut, error) {
			a.player.Lock()
			defer a.player.Unlock()
			if a.player.cmd != nil && a.player.cmd.Process != nil {
				_ = a.player.cmd.Process.Kill()
				a.player.cmd = nil
			}
			if in.Stop || in.File == "" {
				return nil, &playOut{"stopped"}, nil
			}
			if runtime.GOOS != "darwin" {
				return nil, nil, errors.New("play_audio uses macOS afplay; open the file in a player instead")
			}
			f := config.ExpandHome(in.File)
			if _, err := os.Stat(f); err != nil {
				return nil, nil, err
			}
			cmd := exec.Command("/usr/bin/afplay", f)
			if err := cmd.Start(); err != nil {
				return nil, nil, err
			}
			a.player.cmd = cmd
			go func() { _ = cmd.Wait() }()
			return nil, &playOut{"playing " + filepath.Base(f)}, nil
		})

	type listIn struct {
		Dir   string `json:"dir,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	type listItem struct {
		File     string `json:"file"`
		Modified string `json:"modified"`
		KB       int64  `json:"kb"`
	}
	type listOut struct {
		Files []listItem `json:"files"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "list_outputs", Description: "List recently rendered audio files (newest first).", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, *listOut, error) {
			root := a.dir(in.Dir)
			var items []listItem
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() && p != root && strings.Count(strings.TrimPrefix(p, root), string(os.PathSeparator)) > 2 {
					return filepath.SkipDir
				}
				ext := strings.ToLower(filepath.Ext(p))
				if !d.IsDir() && (ext == ".mp3" || ext == ".wav") {
					if fi, err := d.Info(); err == nil {
						items = append(items, listItem{p, fi.ModTime().Format("2006-01-02 15:04"), fi.Size() / 1024})
					}
				}
				return nil
			})
			sort.Slice(items, func(i, j int) bool { return items[i].Modified > items[j].Modified })
			lim := in.Limit
			if lim <= 0 {
				lim = 25
			}
			return nil, &listOut{items[:min(lim, len(items))]}, nil
		})
	type pronIn struct {
		Action        string            `json:"action" jsonschema:"list | set | remove | sync | preview"`
		Term          string            `json:"term,omitempty" jsonschema:"word or phrase as written, e.g. Szass Tam"`
		Alias         string            `json:"alias,omitempty" jsonschema:"respelling that sounds right (works with every model)"`
		IPA           string            `json:"ipa,omitempty" jsonschema:"IPA phonemes (eleven_v3/v4/flash_v2 only)"`
		CMU           string            `json:"cmu,omitempty" jsonschema:"CMU Arpabet phonemes (eleven_v3/v4/flash_v2 only)"`
		CaseSensitive bool              `json:"case_sensitive,omitempty"`
		Group         string            `json:"group,omitempty" jsonschema:"campaign/project grouping; also filters list"`
		Notes         string            `json:"notes,omitempty"`
		Entries       []scene.PronEntry `json:"entries,omitempty" jsonschema:"bulk set: several entries at once"`
		Terms         []string          `json:"terms,omitempty" jsonschema:"bulk remove"`
		Text          string            `json:"text,omitempty" jsonschema:"preview: sentence to speak (default: the term)"`
		Voice         string            `json:"voice,omitempty" jsonschema:"preview: speaker name (cast/library) or voice_id"`
		Model         string            `json:"model,omitempty" jsonschema:"preview model (default eleven_v3)"`
		Play          bool              `json:"play,omitempty" jsonschema:"preview: play it on the Mac's speakers"`
	}
	type pronOut struct {
		Entries    []scene.PronEntry `json:"entries,omitempty"`
		Message    string            `json:"message,omitempty"`
		Dictionary string            `json:"dictionary_id,omitempty"`
		Version    string            `json:"version_id,omitempty"`
		File       string            `json:"file,omitempty"`
		SpokenAs   string            `json:"sent_text,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "manage_pronunciation", Description: "Pronunciation lexicon applied to every render so names are always said the same way (Szass Tam, Thay, Pyarados…). " +
		"Alias rules (respellings) are applied locally and work with every model; IPA/CMU phoneme rules sync to an ElevenLabs pronunciation dictionary " +
		"(needs the API key's Pronunciation Dictionaries permission = Write) and work with eleven_v3/v4/flash_v2. preview speaks a term or sentence so you can check it."},
		func(ctx context.Context, req *mcp.CallToolRequest, in pronIn) (*mcp.CallToolResult, *pronOut, error) {
			switch strings.ToLower(in.Action) {
			case "", "list":
				id, ver, _ := a.Lex.Remote()
				return nil, &pronOut{Entries: a.Lex.List(in.Group), Dictionary: id, Version: ver}, nil
			case "set":
				es := in.Entries
				if in.Term != "" {
					es = append(es, scene.PronEntry{Term: in.Term, Alias: in.Alias, IPA: in.IPA, CMU: in.CMU, CaseSensitive: in.CaseSensitive, Group: in.Group, Notes: in.Notes})
				}
				if len(es) == 0 {
					return nil, nil, errors.New("give term plus alias/ipa/cmu, or entries")
				}
				if err := a.Lex.Set(es...); err != nil {
					return nil, nil, err
				}
				return nil, &pronOut{Entries: es, Message: fmt.Sprintf("saved %d entr%s; used automatically by every render", len(es), map[bool]string{true: "y", false: "ies"}[len(es) == 1])}, nil
			case "remove":
				terms := append(in.Terms, in.Term)
				n, err := a.Lex.Remove(terms...)
				return nil, &pronOut{Message: fmt.Sprintf("removed %d", n)}, err
			case "sync":
				loc, err := a.Lex.Sync(ctx, a.Client)
				if err != nil {
					return nil, nil, err
				}
				if loc == nil {
					return nil, &pronOut{Message: "no phoneme rules to sync (alias rules are applied locally and need no sync)"}, nil
				}
				return nil, &pronOut{Message: "synced", Dictionary: loc.ID, Version: loc.VersionID}, nil
			case "preview":
				text := def(in.Text, in.Term)
				if text == "" {
					return nil, nil, errors.New("give text or term to preview")
				}
				voice := in.Voice
				if voice == "" {
					if cast := a.Cast.List(""); len(cast) > 0 {
						voice = cast[0].Name
					} else {
						return nil, nil, errors.New("give a voice (speaker name or voice_id) for the preview")
					}
				}
				zero, short := 0.0, 0.2
				sp := scene.Spec{Lines: []scene.Line{{Speaker: voice, Text: text}}, Model: in.Model, LeadIn: &short, Tail: &short, DuckDB: &zero,
					NoSubs: true, Filename: "pronounce_" + slug(text), OutputDir: filepath.Join(a.Cfg.OutputDir, "pronunciation_previews")}
				r, err := scene.Render(ctx, a.Env(nil), sp)
				if err != nil {
					return nil, nil, err
				}
				_ = os.Remove(r.CueSheet)
				o := &pronOut{File: r.File, SpokenAs: text}
				if al := a.Lex.Aliaser(); al != nil {
					o.SpokenAs = al.Apply(text)
				}
				if r.Pronounce != nil {
					o.Dictionary, o.Version = r.Pronounce.Dictionary, r.Pronounce.Version
				}
				if len(r.Warnings) > 0 {
					o.Message = strings.Join(r.Warnings, "; ")
				}
				if in.Play && runtime.GOOS == "darwin" {
					a.play(r.File)
				}
				return nil, o, nil
			}
			return nil, nil, fmt.Errorf("unknown action %q", in.Action)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "voice_effects_catalog", Description: "List the valid voice effect IDs (filter presets, reverb environments, background noises) for voicefx / character_effects / cast effects.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *eleven.EffectsCatalog, error) {
			c, err := a.Client.Catalog(ctx)
			return nil, c, err
		})
	return s
}

// play starts afplay in the background, replacing any current playback.
func (a *App) play(f string) {
	a.player.Lock()
	defer a.player.Unlock()
	if a.player.cmd != nil && a.player.cmd.Process != nil {
		_ = a.player.cmd.Process.Kill()
	}
	cmd := exec.Command("/usr/bin/afplay", f)
	if cmd.Start() == nil {
		a.player.cmd = cmd
		go func() { _ = cmd.Wait() }()
	}
}

func def(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func (a *App) dir(d string) string {
	if d == "" {
		return a.Cfg.OutputDir
	}
	return config.ExpandHome(d)
}

// FileOut describes a saved file.
type FileOut struct {
	File    string  `json:"file"`
	Seconds float64 `json:"seconds"`
	Cached  bool    `json:"cached" jsonschema:"served from cache at no credit cost"`
}

// writeRaw saves API MP3 bytes untouched (no re-encode: fastest and lossless).
func (a *App) writeRaw(r *eleven.Response, dir, name string) *FileOut {
	p := filepath.Join(a.dir(dir), name+".mp3")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, r.Body, 0o644)
	secs := 0.0
	if b, err := audio.DecodeMP3(r.Body); err == nil {
		secs = r2(b.Seconds())
	}
	return &FileOut{p, secs, r.Cached}
}

func progressFn(ctx context.Context, req *mcp.CallToolRequest) func(string, int, int) {
	tok := req.Params.GetProgressToken()
	if tok == nil {
		return nil
	}
	return func(msg string, done, total int) {
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Message: msg, Progress: float64(done), Total: float64(total)})
	}
}

func nameOr(n, d string) string {
	n = strings.TrimSpace(n)
	if n == "" {
		n = d
	}
	n = strings.TrimSuffix(strings.TrimSuffix(n, ".mp3"), ".wav")
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, n)
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	return strings.Trim(b.String(), "_") + "_" + time.Now().Format("150405")
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }
