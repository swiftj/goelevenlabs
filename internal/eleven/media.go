package eleven

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------- voice design

// VoiceDesignRequest asks for voice previews from a description.
type VoiceDesignRequest struct {
	Description    string   `json:"voice_description"`
	ModelID        string   `json:"model_id,omitempty"`
	Text           string   `json:"text,omitempty"`
	AutoText       bool     `json:"auto_generate_text,omitempty"`
	Loudness       *float64 `json:"loudness,omitempty"`
	Seed           *int64   `json:"seed,omitempty"`
	GuidanceScale  *float64 `json:"guidance_scale,omitempty"`
	ShouldEnhance  bool     `json:"should_enhance,omitempty"`
	ReferenceAudio string   `json:"reference_audio_base64,omitempty"`
	PromptStrength *float64 `json:"prompt_strength,omitempty"`
}

// VoicePreview is one designed voice candidate.
type VoicePreview struct {
	GeneratedVoiceID string  `json:"generated_voice_id"`
	AudioBase64      string  `json:"audio_base_64"`
	MediaType        string  `json:"media_type"`
	Duration         float64 `json:"duration_secs"`
	Language         string  `json:"language,omitempty"`
}

// DesignVoice returns previews and the text they speak.
func (c *Client) DesignVoice(ctx context.Context, req VoiceDesignRequest, cache bool) ([]VoicePreview, string, bool, error) {
	r, err := c.Do(ctx, http.MethodPost, "/v1/text-to-voice/design", url.Values{"output_format": {DefaultFormat}}, req, cache)
	if err != nil {
		return nil, "", false, err
	}
	var out struct {
		Previews []VoicePreview `json:"previews"`
		Text     string         `json:"text"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil {
		return nil, "", false, err
	}
	return out.Previews, out.Text, r.Cached, nil
}

// SaveDesignedVoice turns a preview into a permanent library voice and returns its voice_id.
func (c *Client) SaveDesignedVoice(ctx context.Context, name, description, generatedID string, labels map[string]string) (string, error) {
	body := map[string]any{"voice_name": name, "voice_description": description, "generated_voice_id": generatedID}
	if len(labels) > 0 {
		body["labels"] = labels
	}
	r, err := c.Do(ctx, http.MethodPost, "/v1/text-to-voice", nil, body, false)
	if err != nil {
		return "", err
	}
	var v struct {
		VoiceID string `json:"voice_id"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil || v.VoiceID == "" {
		return "", fmt.Errorf("unexpected response saving voice: %s", truncate(string(r.Body), 300))
	}
	return v.VoiceID, nil
}

// ---------------------------------------------------------------- image & video

// MediaKind is "image" or "video".
type MediaKind string

const (
	KindImage MediaKind = "image"
	KindVideo MediaKind = "video"
)

// Ref builds a media reference from a local file path, "generation:<id>" or "asset:<id>".
// Files are sent inline (base64, up to 25 MB).
func Ref(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	if id, ok := strings.CutPrefix(s, "generation:"); ok {
		return map[string]any{"type": "generation", "generation_id": id}, nil
	}
	if id, ok := strings.CutPrefix(s, "asset:"); ok {
		return map[string]any{"type": "asset", "asset_id": id}, nil
	}
	if strings.HasPrefix(s, "~/") {
		h, _ := os.UserHomeDir()
		s = filepath.Join(h, s[2:])
	}
	fi, err := os.Stat(s)
	if err != nil {
		return nil, fmt.Errorf("reference %q: not a file, generation:<id> or asset:<id>", s)
	}
	if fi.Size() > 25<<20 {
		return nil, fmt.Errorf("reference %s is %.1f MB; inline references are limited to 25 MB", filepath.Base(s), float64(fi.Size())/1e6)
	}
	mime, ok := map[string]string{
		".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp", ".heic": "image/heic", ".heif": "image/heif",
		".mp3": "audio/mpeg", ".wav": "audio/wav", ".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
	}[strings.ToLower(filepath.Ext(s))]
	if !ok {
		return nil, fmt.Errorf("reference %s: unsupported file type (jpg, png, webp, heic, mp3, wav, mp4, mov, webm)", filepath.Base(s))
	}
	b, err := os.ReadFile(s)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "inline_base64", "content_base64": base64.StdEncoding.EncodeToString(b), "mime_type": mime}, nil
}

// CreateMedia starts an image or video generation and returns its generation id.
// With cache, an identical earlier request returns the same generation (no new charge).
func (c *Client) CreateMedia(ctx context.Context, kind MediaKind, body map[string]any, cache bool) (string, bool, error) {
	r, err := c.Do(ctx, http.MethodPost, "/v1/flows/"+string(kind), nil, body, cache)
	if err != nil {
		return "", false, err
	}
	var v struct {
		ID           string `json:"id"`
		GenerationID string `json:"generation_id"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return "", false, err
	}
	id := v.ID
	if id == "" {
		id = v.GenerationID
	}
	if id == "" {
		return "", false, fmt.Errorf("no generation id in response: %s", truncate(string(r.Body), 300))
	}
	return id, r.Cached, nil
}

// Generation is the state of an image/video generation.
type Generation struct {
	ID            string `json:"id"`
	Status        string `json:"status"` // pending | generating | completed | failed
	ContentURL    string `json:"content_url,omitempty"`
	MimeType      string `json:"content_mime_type,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
}

// GetMedia fetches a generation's status.
func (c *Client) GetMedia(ctx context.Context, kind MediaKind, id string) (*Generation, error) {
	var g Generation
	err := c.GetJSON(ctx, "/v1/flows/"+string(kind)+"/"+url.PathEscape(id), nil, &g)
	return &g, err
}

// WaitMedia polls until the generation finishes, fails or timeout elapses.
// On timeout it returns the last state with no error.
func (c *Client) WaitMedia(ctx context.Context, kind MediaKind, id string, timeout time.Duration, tick func(*Generation, time.Duration)) (*Generation, error) {
	start := time.Now()
	interval := 2 * time.Second
	if kind == KindVideo {
		interval = 5 * time.Second
	}
	for {
		g, err := c.GetMedia(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		if tick != nil {
			tick(g, time.Since(start))
		}
		if g.Status == "completed" || g.Status == "failed" || time.Since(start) >= timeout {
			return g, nil
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return g, ctx.Err()
		}
	}
}

// Download fetches a signed content URL. The API key is deliberately NOT sent:
// the URL points at a storage host, not the ElevenLabs API.
func (c *Client) Download(ctx context.Context, u string) ([]byte, error) {
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" {
		return nil, errors.New("refusing to download from a non-https URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 500))
		return nil, fmt.Errorf("download failed: %d %s", res.StatusCode, b)
	}
	return io.ReadAll(io.LimitReader(res.Body, 2<<30))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
