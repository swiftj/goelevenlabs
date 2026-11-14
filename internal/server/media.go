package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftj/goelevenlabs/internal/config"
	"github.com/swiftj/goelevenlabs/internal/eleven"
	"github.com/swiftj/goelevenlabs/internal/scene"
)

// Model aliases for image/video generation.
var videoModels = map[string]string{
	"veo": "veo-3.1-fast-generate-001", "veo3": "veo-3.1-fast-generate-001", "veo-3.1": "veo-3.1-fast-generate-001",
	"seedance": "bytedance-seedance-v2.5", "aurora": "creatify-aurora", "lipsync": "creatify-aurora",
}
var imageModels = map[string]string{
	"gemini": "gemini-3.1-flash-image", "gemini-flash": "gemini-3.1-flash-image", "gemini-pro": "gemini-3-pro-image",
	"nano-banana": "gemini-2.5-flash-image", "seedream": "bytedance-seedream-5-pro", "seedream-lite": "bytedance-seedream-5-lite",
}

func alias(m map[string]string, v, def string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return def
	}
	if full, ok := m[v]; ok {
		return full
	}
	return v
}

// MediaOut describes an image/video generation.
type MediaOut struct {
	GenerationID string  `json:"generation_id"`
	Status       string  `json:"status"`
	File         string  `json:"file,omitempty"`
	MimeType     string  `json:"mime_type,omitempty"`
	Model        string  `json:"model,omitempty"`
	Cached       bool    `json:"cached,omitempty" jsonschema:"an identical earlier request was reused (no new charge)"`
	ElapsedSec   float64 `json:"elapsed_sec,omitempty"`
	Reference    string  `json:"reuse_as,omitempty" jsonschema:"pass this as a reference/start_frame to build on this result"`
	Note         string  `json:"note,omitempty"`
}

func (a *App) mediaDir(kind, dir string) string {
	if dir != "" {
		return config.ExpandHome(dir)
	}
	return filepath.Join(a.Cfg.OutputDir, map[eleven.MediaKind]string{eleven.KindImage: "images", eleven.KindVideo: "video"}[eleven.MediaKind(kind)])
}

// generate creates (or reuses) a generation, waits for it and downloads the result.
func (a *App) generate(ctx context.Context, req *mcp.CallToolRequest, kind eleven.MediaKind, body map[string]any, fresh bool, wait time.Duration, dir, name string) (*MediaOut, error) {
	t0 := time.Now()
	id, cached, err := a.Client.CreateMedia(ctx, kind, body, !fresh)
	var ae *eleven.APIError
	if errors.As(err, &ae) && ae.Status == 402 {
		tier := "a lower tier"
		if sub, e := a.Client.Subscription(ctx); e == nil && sub.Tier != "" {
			tier = sub.Tier
		}
		return nil, fmt.Errorf("ElevenLabs %s generation requires a Pro plan or above; this account is on %s. Nothing was charged. Voice design, speech, music and sound effects still work", kind, tier)
	}
	if err != nil {
		return nil, err
	}
	prog := progressFn(ctx, req)
	tick := func(g *eleven.Generation, el time.Duration) {
		if prog != nil {
			prog(fmt.Sprintf("%s %s (%ds)", kind, g.Status, int(el.Seconds())), int(el.Seconds()), int(wait.Seconds()))
		}
	}
	g, err := a.Client.WaitMedia(ctx, kind, id, wait, tick)
	if err != nil {
		return nil, err
	}
	if g.Status == "failed" && cached {
		// The reused generation had failed: forget it and try once more for real.
		a.Client.Forget(http.MethodPost, "/v1/flows/"+string(kind), nil, body)
		if id, _, err = a.Client.CreateMedia(ctx, kind, body, true); err != nil {
			return nil, err
		}
		cached = false
		if g, err = a.Client.WaitMedia(ctx, kind, id, wait, tick); err != nil {
			return nil, err
		}
	}
	out := &MediaOut{GenerationID: id, Status: g.Status, Cached: cached, Model: fmt.Sprint(body["model_id"]), Reference: "generation:" + id}
	return a.finish(ctx, kind, g, out, dir, name, t0)
}

func (a *App) finish(ctx context.Context, kind eleven.MediaKind, g *eleven.Generation, out *MediaOut, dir, name string, t0 time.Time) (*MediaOut, error) {
	out.ElapsedSec = r2(time.Since(t0).Seconds())
	switch g.Status {
	case "failed":
		return nil, fmt.Errorf("%s generation failed (%s): %s — failed generations are not charged", kind, g.FailureReason, g.ErrorMessage)
	case "completed":
	default:
		out.Note = fmt.Sprintf("still %s after %.0fs; call check_generation with kind=%s and this generation_id to collect it", g.Status, out.ElapsedSec, kind)
		return out, nil
	}
	data, err := a.Client.Download(ctx, g.ContentURL)
	if err != nil {
		return nil, err
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "video/mp4": ".mp4", "video/quicktime": ".mov", "video/webm": ".webm"}[g.MimeType]
	if ext == "" {
		ext = map[eleven.MediaKind]string{eleven.KindImage: ".png", eleven.KindVideo: ".mp4"}[kind]
	}
	p := filepath.Join(a.mediaDir(string(kind), dir), nameOr(name, string(kind)+"_"+time.Now().Format("2006-01-02_150405"))+ext)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return nil, err
	}
	out.File, out.MimeType = p, g.MimeType
	return out, nil
}

func refs(list []string) ([]map[string]any, error) {
	var out []map[string]any
	for _, s := range list {
		r, err := eleven.Ref(s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (a *App) addMediaTools(s *mcp.Server) {
	// ------------------------------------------------------------ voice design
	type designIn struct {
		Description    string   `json:"description" jsonschema:"who the voice is: age, gender, accent, timbre, pace, attitude, e.g. 'ancient lich, dry rasping whisper, cold aristocratic calm, slow'"`
		Text           string   `json:"text,omitempty" jsonschema:"line the previews speak (100-1000 characters); omit to auto-generate one"`
		Model          string   `json:"model,omitempty" jsonschema:"eleven_ttv_v3 (default, most expressive) or eleven_multilingual_ttv_v2"`
		GuidanceScale  *float64 `json:"guidance_scale,omitempty" jsonschema:"how literally to follow the description (default 5); lower with long, detailed descriptions"`
		Loudness       *float64 `json:"loudness,omitempty" jsonschema:"-1 quiet .. 1 loud (default 0.5)"`
		Seed           *int64   `json:"seed,omitempty"`
		Enhance        bool     `json:"enhance,omitempty" jsonschema:"let ElevenLabs expand a short description"`
		ReferenceAudio string   `json:"reference_audio,omitempty" jsonschema:"eleven_ttv_v3 only: local audio file whose voice to steer toward"`
		PromptStrength *float64 `json:"prompt_strength,omitempty" jsonschema:"with reference_audio: 0 = mostly reference, 1 = mostly description"`
		Play           int      `json:"play,omitempty" jsonschema:"play preview N (1-based) right away on the Mac"`
		Fresh          bool     `json:"fresh,omitempty" jsonschema:"new candidates instead of the cached ones for this description"`
	}
	type preview struct {
		N                int     `json:"n"`
		GeneratedVoiceID string  `json:"generated_voice_id"`
		File             string  `json:"file"`
		Seconds          float64 `json:"seconds"`
	}
	type designOut struct {
		Previews []preview `json:"previews"`
		Text     string    `json:"text"`
		Cached   bool      `json:"cached,omitempty"`
		Next     string    `json:"next"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "design_voice", Description: "Design a brand-new voice from a description (Text to Voice). Returns several previews saved as MP3s; listen, then keep one with save_designed_voice (which can also add it to the cast)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in designIn) (*mcp.CallToolResult, *designOut, error) {
			if strings.TrimSpace(in.Description) == "" {
				return nil, nil, errors.New("description is required")
			}
			if n := len(in.Text); n > 0 && (n < 100 || n > 1000) {
				return nil, nil, fmt.Errorf("text must be 100-1000 characters (got %d), or omit it to auto-generate", n)
			}
			req := eleven.VoiceDesignRequest{Description: in.Description, ModelID: def(in.Model, "eleven_ttv_v3"), Text: in.Text, AutoText: in.Text == "",
				Loudness: in.Loudness, Seed: in.Seed, GuidanceScale: in.GuidanceScale, ShouldEnhance: in.Enhance, PromptStrength: in.PromptStrength}
			if in.ReferenceAudio != "" {
				b, err := os.ReadFile(config.ExpandHome(in.ReferenceAudio))
				if err != nil {
					return nil, nil, err
				}
				req.ReferenceAudio = base64.StdEncoding.EncodeToString(b)
			}
			ps, text, cached, err := a.Client.DesignVoice(ctx, req, !in.Fresh)
			if err != nil {
				return nil, nil, err
			}
			out := &designOut{Text: text, Cached: cached, Next: "save_designed_voice with the generated_voice_id you like (add cast_as to give a character this voice)"}
			dir := filepath.Join(a.Cfg.OutputDir, "voice_previews")
			_ = os.MkdirAll(dir, 0o755)
			base := slug(in.Description)
			for i, p := range ps {
				b, err := base64.StdEncoding.DecodeString(p.AudioBase64)
				if err != nil {
					return nil, nil, err
				}
				f := filepath.Join(dir, fmt.Sprintf("%s_%d.mp3", base, i+1))
				if err := os.WriteFile(f, b, 0o644); err != nil {
					return nil, nil, err
				}
				out.Previews = append(out.Previews, preview{i + 1, p.GeneratedVoiceID, f, r2(p.Duration)})
			}
			if in.Play > 0 && in.Play <= len(out.Previews) {
				a.play(out.Previews[in.Play-1].File)
			}
			return nil, out, nil
		})

	type saveIn struct {
		GeneratedVoiceID string          `json:"generated_voice_id"`
		Name             string          `json:"name" jsonschema:"library name, e.g. 'TW - Szass Tam'"`
		Description      string          `json:"description" jsonschema:"what the voice is (stored with it)"`
		CastAs           string          `json:"cast_as,omitempty" jsonschema:"also make this the voice of a character in the cast"`
		Group            string          `json:"group,omitempty"`
		Effects          *eleven.Effects `json:"effects,omitempty" jsonschema:"default voice effects for cast_as"`
	}
	type saveOut struct {
		VoiceID string `json:"voice_id"`
		Name    string `json:"name"`
		CastAs  string `json:"cast_as,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "save_designed_voice", Description: "Keep a design_voice preview as a permanent voice in your ElevenLabs library, optionally assigning it to a cast character in one step."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in saveIn) (*mcp.CallToolResult, *saveOut, error) {
			if in.GeneratedVoiceID == "" || in.Name == "" {
				return nil, nil, errors.New("generated_voice_id and name are required")
			}
			id, err := a.Client.SaveDesignedVoice(ctx, in.Name, def(in.Description, in.Name), in.GeneratedVoiceID, nil)
			if err != nil {
				return nil, nil, err
			}
			out := &saveOut{VoiceID: id, Name: in.Name}
			if in.CastAs != "" {
				m, ok := a.Cast.Get(in.CastAs)
				if !ok {
					m = scene.CastMember{Name: in.CastAs}
				}
				m.VoiceID, m.Voice = id, in.Name
				if in.Group != "" {
					m.Group = in.Group
				}
				if in.Effects != nil {
					m.Effects = in.Effects
				}
				if err := a.Cast.Set(m); err != nil {
					return nil, nil, err
				}
				out.CastAs = in.CastAs
			}
			return nil, out, nil
		})

	// ------------------------------------------------------------ images
	type imageIn struct {
		Prompt      string   `json:"prompt"`
		Model       string   `json:"model,omitempty" jsonschema:"gemini-3.1-flash-image (default), gemini-3-pro-image, gemini-2.5-flash-image, bytedance-seedream-5-pro, bytedance-seedream-5-lite (aliases: gemini, gemini-pro, nano-banana, seedream, seedream-lite)"`
		AspectRatio string   `json:"aspect_ratio,omitempty" jsonschema:"e.g. 16:9 (default), 1:1, 3:4, 9:16, 21:9, auto"`
		Resolution  string   `json:"resolution,omitempty" jsonschema:"model-dependent: 1K, 2K, 3K, 4K"`
		References  []string `json:"references,omitempty" jsonschema:"images to edit or draw from: local files, generation:<id> or asset:<id>"`
		Seed        *int64   `json:"seed,omitempty" jsonschema:"Seedream only"`
		Filename    string   `json:"filename,omitempty"`
		OutputDir   string   `json:"output_dir,omitempty" jsonschema:"default <output>/images"`
		WaitSec     int      `json:"wait_sec,omitempty" jsonschema:"how long to wait for the result (default 180)"`
		Fresh       bool     `json:"fresh,omitempty" jsonschema:"new image even if this exact request was made before"`
		Open        bool     `json:"open,omitempty" jsonschema:"open the image in Preview when done (macOS)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "generate_image", Description: "Generate or edit an image with ElevenLabs image models (Gemini, Seedream): character portraits, locations, scene cards. Saves the file and returns a generation reference you can reuse for edits or as a video's start_frame / talking_portrait image."},
		func(ctx context.Context, req *mcp.CallToolRequest, in imageIn) (*mcp.CallToolResult, *MediaOut, error) {
			if strings.TrimSpace(in.Prompt) == "" {
				return nil, nil, errors.New("prompt is required")
			}
			body := map[string]any{"model_id": alias(imageModels, in.Model, "gemini-3.1-flash-image"), "prompt": in.Prompt}
			if in.AspectRatio != "" {
				body["aspect_ratio"] = in.AspectRatio
			}
			if in.Resolution != "" {
				body["resolution"] = strings.ToUpper(in.Resolution)
			}
			if in.Seed != nil {
				body["seed"] = *in.Seed
			}
			if len(in.References) > 0 {
				r, err := refs(in.References)
				if err != nil {
					return nil, nil, err
				}
				body["images"] = r
			}
			wait := time.Duration(max(in.WaitSec, 0)) * time.Second
			if wait == 0 {
				wait = 180 * time.Second
			}
			out, err := a.generate(ctx, req, eleven.KindImage, body, in.Fresh, wait, in.OutputDir, nameOr(in.Filename, "image_"+slug(in.Prompt)))
			if err == nil && in.Open && out.File != "" {
				openFile(out.File)
			}
			return nil, out, err
		})

	// ------------------------------------------------------------ video
	type videoIn struct {
		Model         string   `json:"model,omitempty" jsonschema:"veo (default: veo-3.1-fast, 4/6/8 s, text or first/last frame), seedance (bytedance-seedance-v2.5: up to 1080p, reference images/videos/audio), aurora (creatify-aurora: lip-sync an image to speech; prefer talking_portrait)"`
		Prompt        string   `json:"prompt,omitempty"`
		DurationSec   int      `json:"duration_sec,omitempty" jsonschema:"veo: 4, 6 or 8 (default 8); seedance: default 5"`
		AspectRatio   string   `json:"aspect_ratio,omitempty" jsonschema:"16:9 (default) or 9:16; seedance also 21:9, 4:3, 1:1, 3:4, auto"`
		Resolution    string   `json:"resolution,omitempty" jsonschema:"480p, 720p, 1080p (model-dependent)"`
		GenerateAudio *bool    `json:"generate_audio,omitempty" jsonschema:"let the model add sound (default true)"`
		EnhancePrompt *bool    `json:"enhance_prompt,omitempty" jsonschema:"veo: allow prompt rewriting (default true)"`
		StartFrame    string   `json:"start_frame,omitempty" jsonschema:"first frame: local image, generation:<id> or asset:<id>"`
		EndFrame      string   `json:"end_frame,omitempty" jsonschema:"last frame (veo/seedance)"`
		Images        []string `json:"images,omitempty" jsonschema:"seedance: reference images (subjects); aurora: the character image"`
		Videos        []string `json:"videos,omitempty" jsonschema:"seedance: reference videos (motion/subjects)"`
		Audios        []string `json:"audios,omitempty" jsonschema:"seedance: reference audio; aurora: the speech to lip-sync"`
		Confirm       bool     `json:"confirm,omitempty" jsonschema:"video generation is billed: without confirm:true nothing is sent and a summary is returned"`
		WaitSec       int      `json:"wait_sec,omitempty" jsonschema:"how long to wait (default 300); longer jobs return a generation_id for check_generation"`
		Filename      string   `json:"filename,omitempty"`
		OutputDir     string   `json:"output_dir,omitempty" jsonschema:"default <output>/video"`
		Fresh         bool     `json:"fresh,omitempty"`
		Open          bool     `json:"open,omitempty" jsonschema:"open the video when done (macOS)"`
	}
	buildVideo := func(in videoIn) (map[string]any, string, error) {
		model := alias(videoModels, in.Model, "veo-3.1-fast-generate-001")
		body := map[string]any{"model_id": model}
		one := func(key, v string) error {
			if v == "" {
				return nil
			}
			r, err := eleven.Ref(v)
			if err == nil {
				body[key] = r
			}
			return err
		}
		many := func(key string, v []string) error {
			if len(v) == 0 {
				return nil
			}
			r, err := refs(v)
			if err == nil {
				body[key] = r
			}
			return err
		}
		switch model {
		case "creatify-aurora":
			img := in.StartFrame
			if img == "" && len(in.Images) > 0 {
				img = in.Images[0]
			}
			if img == "" || len(in.Audios) == 0 {
				return nil, model, errors.New("aurora needs a character image (images[0] or start_frame) and speech audio (audios[0]); talking_portrait does this for you")
			}
			if err := one("image", img); err != nil {
				return nil, model, err
			}
			if err := one("audio", in.Audios[0]); err != nil {
				return nil, model, err
			}
		default:
			if strings.TrimSpace(in.Prompt) == "" {
				return nil, model, errors.New("prompt is required")
			}
			body["prompt"] = in.Prompt
			for _, e := range []error{one("start_frame", in.StartFrame), one("end_frame", in.EndFrame), many("images", in.Images), many("videos", in.Videos), many("audios", in.Audios)} {
				if e != nil {
					return nil, model, e
				}
			}
			if in.DurationSec > 0 {
				body["duration_secs"] = in.DurationSec
			}
			if in.AspectRatio != "" {
				body["aspect_ratio"] = in.AspectRatio
			}
			if in.GenerateAudio != nil {
				body["generate_audio"] = *in.GenerateAudio
			}
			if in.EnhancePrompt != nil && model == "veo-3.1-fast-generate-001" {
				body["enhance_prompt"] = *in.EnhancePrompt
			}
		}
		if in.Resolution != "" {
			body["resolution"] = strings.ToLower(in.Resolution)
		}
		return body, model, nil
	}
	summary := func(body map[string]any) string {
		var parts []string
		for k, v := range body {
			switch x := v.(type) {
			case map[string]any:
				parts = append(parts, k+"="+fmt.Sprint(x["type"]))
			case []map[string]any:
				parts = append(parts, fmt.Sprintf("%s=%d refs", k, len(x)))
			default:
				s := fmt.Sprint(v)
				if len(s) > 80 {
					s = s[:80] + "…"
				}
				parts = append(parts, k+"="+s)
			}
		}
		return strings.Join(parts, ", ")
	}
	mcp.AddTool(s, &mcp.Tool{Name: "generate_video", Description: "Generate a short video with ElevenLabs video models: Veo 3.1 Fast (text or first/last frame, 4-8 s), Seedance 2.5 (reference images/videos/audio, up to 1080p) or Creatify Aurora (lip-sync). Video is billed: the first call without confirm:true only returns what would be sent."},
		func(ctx context.Context, req *mcp.CallToolRequest, in videoIn) (*mcp.CallToolResult, *MediaOut, error) {
			body, model, err := buildVideo(in)
			if err != nil {
				return nil, nil, err
			}
			if !in.Confirm {
				return nil, &MediaOut{Status: "not_sent", Model: model, Note: "Video generation uses credits. Would send: " + summary(body) + ". Call again with confirm:true to generate."}, nil
			}
			wait := time.Duration(max(in.WaitSec, 0)) * time.Second
			if wait == 0 {
				wait = 300 * time.Second
			}
			out, err := a.generate(ctx, req, eleven.KindVideo, body, in.Fresh, wait, in.OutputDir, nameOr(in.Filename, "video_"+slug(def(in.Prompt, model))))
			if err == nil && in.Open && out.File != "" {
				openFile(out.File)
			}
			return nil, out, err
		})

	type portraitIn struct {
		Image      string          `json:"image" jsonschema:"character portrait: local image, generation:<id> (e.g. from generate_image) or asset:<id>"`
		Text       string          `json:"text,omitempty" jsonschema:"line to speak; rendered with the speaker's voice, pronunciation lexicon and effects"`
		Speaker    string          `json:"speaker,omitempty" jsonschema:"cast name, library voice name or voice_id"`
		Audio      string          `json:"audio,omitempty" jsonschema:"use an existing speech file instead of text"`
		Effects    *eleven.Effects `json:"voice_effects,omitempty" jsonschema:"voice effects for the line (default: the character's cast effects)"`
		Resolution string          `json:"resolution,omitempty" jsonschema:"480p or 720p (default 720p)"`
		Confirm    bool            `json:"confirm,omitempty" jsonschema:"video is billed: without confirm:true only the speech is rendered and a summary returned"`
		WaitSec    int             `json:"wait_sec,omitempty"`
		Filename   string          `json:"filename,omitempty"`
		Open       bool            `json:"open,omitempty"`
	}
	type portraitOut struct {
		*MediaOut
		AudioFile string `json:"audio_file"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "talking_portrait", Description: "Make a character portrait speak: renders the line in the character's voice (cast, lexicon and voice effects apply), then lip-syncs the image to it with Creatify Aurora. The first call without confirm:true renders only the speech (cheap, cached) so you can check it."},
		func(ctx context.Context, req *mcp.CallToolRequest, in portraitIn) (*mcp.CallToolResult, *portraitOut, error) {
			if in.Image == "" {
				return nil, nil, errors.New("image is required")
			}
			audioFile := config.ExpandHome(in.Audio)
			name := nameOr(in.Filename, "portrait_"+slug(def(in.Speaker, "")+" "+in.Text))
			if audioFile == "" {
				if in.Text == "" || in.Speaker == "" {
					return nil, nil, errors.New("give text and speaker, or an audio file")
				}
				zero, short := 0.0, 0.15
				sp := scene.Spec{Lines: []scene.Line{{Speaker: in.Speaker, Text: in.Text, Effects: in.Effects}}, LeadIn: &short, Tail: &short,
					DuckDB: &zero, NoSubs: true, Filename: name, OutputDir: filepath.Join(a.Cfg.OutputDir, "portraits")}
				r, err := scene.Render(ctx, a.Env(nil), sp)
				if err != nil {
					return nil, nil, err
				}
				_ = os.Remove(r.CueSheet)
				audioFile = r.File
			}
			vin := videoIn{Model: "aurora", Images: []string{in.Image}, Audios: []string{audioFile}, Resolution: in.Resolution}
			body, model, err := buildVideo(vin)
			if err != nil {
				return nil, nil, err
			}
			if !in.Confirm {
				return nil, &portraitOut{&MediaOut{Status: "not_sent", Model: model, Note: "Speech is ready (listen with play_audio). Lip-sync video uses credits: call again with confirm:true to generate it."}, audioFile}, nil
			}
			wait := time.Duration(max(in.WaitSec, 0)) * time.Second
			if wait == 0 {
				wait = 300 * time.Second
			}
			out, err := a.generate(ctx, req, eleven.KindVideo, body, false, wait, filepath.Join(a.Cfg.OutputDir, "portraits"), name)
			if err != nil {
				return nil, nil, err
			}
			if in.Open && out.File != "" {
				openFile(out.File)
			}
			return nil, &portraitOut{out, audioFile}, nil
		})

	type checkIn struct {
		Kind         string `json:"kind" jsonschema:"image or video"`
		GenerationID string `json:"generation_id"`
		WaitSec      int    `json:"wait_sec,omitempty" jsonschema:"keep waiting up to this long (default: check once)"`
		Filename     string `json:"filename,omitempty"`
		OutputDir    string `json:"output_dir,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "check_generation", Description: "Check an image/video generation that was still running, and download it when complete.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in checkIn) (*mcp.CallToolResult, *MediaOut, error) {
			kind := eleven.MediaKind(strings.ToLower(in.Kind))
			if kind != eleven.KindImage && kind != eleven.KindVideo {
				return nil, nil, errors.New("kind must be image or video")
			}
			t0 := time.Now()
			g, err := a.Client.WaitMedia(ctx, kind, in.GenerationID, time.Duration(max(in.WaitSec, 0))*time.Second, nil)
			if err != nil {
				return nil, nil, err
			}
			out := &MediaOut{GenerationID: in.GenerationID, Status: g.Status, Reference: "generation:" + in.GenerationID}
			out, err = a.finish(ctx, kind, g, out, in.OutputDir, nameOr(in.Filename, string(kind)+"_"+in.GenerationID), t0)
			return nil, out, err
		})
}

// openFile opens a file with the default macOS app (no-op elsewhere).
func openFile(p string) {
	if _, err := os.Stat("/usr/bin/open"); err == nil {
		cmd := exec.Command("/usr/bin/open", p)
		if cmd.Start() == nil {
			go func() { _ = cmd.Wait() }()
		}
	}
}
