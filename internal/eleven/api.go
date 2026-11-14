package eleven

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DefaultFormat is requested from every audio endpoint so all assets share one sample rate.
const DefaultFormat = "mp3_44100_128"

// DialogueInput is one line of a dialogue request.
type DialogueInput struct {
	Text    string `json:"text"`
	VoiceID string `json:"voice_id"`
}

// Effects maps to the API's EffectsSpec (server-side voice processing).
type Effects struct {
	FilterPresetID    string   `json:"filter_preset_id,omitempty" jsonschema:"voice filter: phone, old_radio, robot, cheap_microphone, low_quality_phone, bright_phone"`
	EnvironmentID     string   `json:"environment_id,omitempty" jsonschema:"reverb space: small_room, big_room, hall, tunnel, street, valley, forest"`
	BackgroundNoiseID string   `json:"background_noise_id,omitempty" jsonschema:"noise bed: call_center, cafe, city, keyboard"`
	Distance          *float64 `json:"distance,omitempty" jsonschema:"0 near to 1 far"`
	SendLevel         *float64 `json:"send_level,omitempty" jsonschema:"reverb send 0-1"`
	Pan               *float64 `json:"pan,omitempty" jsonschema:"-1 left to 1 right"`
}

// IsZero reports whether no effect is set.
func (e *Effects) IsZero() bool {
	return e == nil || (e.FilterPresetID == "" && e.EnvironmentID == "" && e.BackgroundNoiseID == "" && e.Distance == nil && e.SendLevel == nil && e.Pan == nil)
}

// DialogueRequest is a Text to Dialogue request.
type DialogueRequest struct {
	Inputs       []DialogueInput `json:"inputs"`
	ModelID      string          `json:"model_id,omitempty"`
	LanguageCode string          `json:"language_code,omitempty"`
	Settings     *struct {
		Stability *float64 `json:"stability,omitempty"`
	} `json:"settings,omitempty"`
	Seed               *int64        `json:"seed,omitempty"`
	PreviousRequestIDs []string      `json:"previous_request_ids,omitempty"`
	AudioEffects       *Effects      `json:"audio_effects,omitempty"`
	Dictionaries       []DictLocator `json:"pronunciation_dictionary_locators,omitempty"`
}

// DictLocator points at one version of a pronunciation dictionary.
type DictLocator struct {
	ID        string `json:"pronunciation_dictionary_id"`
	VersionID string `json:"version_id,omitempty"`
}

// VoiceSegment says which input line occupies which time span.
type VoiceSegment struct {
	VoiceID    string  `json:"voice_id"`
	Start      float64 `json:"start_time_seconds"`
	End        float64 `json:"end_time_seconds"`
	InputIndex int     `json:"dialogue_input_index"`
	CharStart  int     `json:"character_start_index"`
	CharEnd    int     `json:"character_end_index"`
}

// DialogueResult is decoded audio plus timing.
type DialogueResult struct {
	Audio     []byte
	Segments  []VoiceSegment
	RequestID string
	Cached    bool
}

// Dialogue renders a multi-voice dialogue and returns MP3 bytes with per-line timings.
func (c *Client) Dialogue(ctx context.Context, req DialogueRequest, cache bool) (*DialogueResult, error) {
	q := url.Values{"output_format": {DefaultFormat}}
	r, err := c.Do(ctx, http.MethodPost, "/v1/text-to-dialogue/with-timestamps", q, req, cache)
	if err != nil {
		return nil, err
	}
	var out struct {
		AudioBase64 string         `json:"audio_base64"`
		Segments    []VoiceSegment `json:"voice_segments"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil {
		return nil, fmt.Errorf("dialogue response: %w", err)
	}
	audio, err := base64.StdEncoding.DecodeString(out.AudioBase64)
	if err != nil {
		return nil, fmt.Errorf("dialogue audio: %w", err)
	}
	return &DialogueResult{Audio: audio, Segments: out.Segments, RequestID: r.RequestID, Cached: r.Cached}, nil
}

// SFXRequest is a sound effect request.
type SFXRequest struct {
	Text            string   `json:"text"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	PromptInfluence *float64 `json:"prompt_influence,omitempty"`
	Loop            bool     `json:"loop,omitempty"`
	ModelID         string   `json:"model_id,omitempty"`
}

// SoundEffect returns MP3 bytes.
func (c *Client) SoundEffect(ctx context.Context, req SFXRequest, cache bool) (*Response, error) {
	return c.Do(ctx, http.MethodPost, "/v1/sound-generation", url.Values{"output_format": {DefaultFormat}}, req, cache)
}

// MusicRequest is a music composition request.
type MusicRequest struct {
	Prompt            string          `json:"prompt,omitempty"`
	CompositionPlan   json.RawMessage `json:"composition_plan,omitempty"`
	MusicLengthMS     int             `json:"music_length_ms,omitempty"`
	ModelID           string          `json:"model_id,omitempty"`
	ForceInstrumental bool            `json:"force_instrumental,omitempty"`
	Seed              *int64          `json:"seed,omitempty"`
}

// Music returns MP3 bytes.
func (c *Client) Music(ctx context.Context, req MusicRequest, cache bool) (*Response, error) {
	return c.Do(ctx, http.MethodPost, "/v1/music", url.Values{"output_format": {DefaultFormat}}, req, cache)
}

// TTSRequest is a single-voice speech request.
type TTSRequest struct {
	Text          string         `json:"text"`
	ModelID       string         `json:"model_id,omitempty"`
	LanguageCode  string         `json:"language_code,omitempty"`
	VoiceSettings map[string]any `json:"voice_settings,omitempty"`
	Seed          *int64         `json:"seed,omitempty"`
}

// TTS returns MP3 bytes for one voice.
func (c *Client) TTS(ctx context.Context, voiceID string, req TTSRequest, cache bool) (*Response, error) {
	return c.Do(ctx, http.MethodPost, "/v1/text-to-speech/"+url.PathEscape(voiceID), url.Values{"output_format": {DefaultFormat}}, req, cache)
}

// Voice is a library voice.
type Voice struct {
	VoiceID     string            `json:"voice_id"`
	Name        string            `json:"name"`
	Category    string            `json:"category,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	PreviewURL  string            `json:"preview_url,omitempty"`
}

// Voices searches the user's voice library.
func (c *Client) Voices(ctx context.Context, search, category string, pageSize int) ([]Voice, error) {
	q := url.Values{}
	if search != "" {
		q.Set("search", search)
	}
	if category != "" {
		q.Set("category", category)
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	q.Set("page_size", strconv.Itoa(pageSize))
	var out struct {
		Voices []Voice `json:"voices"`
	}
	if err := c.GetJSON(ctx, "/v2/voices", q, &out); err != nil {
		return nil, err
	}
	return out.Voices, nil
}

// Subscription is the account's plan and usage.
type Subscription struct {
	Tier                   string `json:"tier"`
	CharacterCount         int64  `json:"character_count"`
	CharacterLimit         int64  `json:"character_limit"`
	NextCharacterResetUnix int64  `json:"next_character_count_reset_unix"`
	Status                 string `json:"status"`
}

// Subscription returns plan and usage.
func (c *Client) Subscription(ctx context.Context) (*Subscription, error) {
	var s Subscription
	return &s, c.GetJSON(ctx, "/v1/user/subscription", nil, &s)
}

// PronRule is a pronunciation dictionary rule (alias or phoneme).
type PronRule struct {
	StringToReplace string `json:"string_to_replace"`
	Type            string `json:"type"` // alias | phoneme
	Alias           string `json:"alias,omitempty"`
	Phoneme         string `json:"phoneme,omitempty"`
	Alphabet        string `json:"alphabet,omitempty"` // ipa | cmu-arpabet
	CaseSensitive   bool   `json:"case_sensitive"`
	WordBoundaries  bool   `json:"word_boundaries"`
}

// DictVersion identifies a dictionary version after a change.
type DictVersion struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
	Rules     int    `json:"version_rules_num"`
}

// CreateDictionary creates a pronunciation dictionary from rules.
func (c *Client) CreateDictionary(ctx context.Context, name, desc string, rules []PronRule) (*DictVersion, error) {
	body := map[string]any{"name": name, "description": desc, "rules": rules}
	r, err := c.Do(ctx, http.MethodPost, "/v1/pronunciation-dictionaries/add-from-rules", nil, body, false)
	if err != nil {
		return nil, err
	}
	var v DictVersion
	return &v, json.Unmarshal(r.Body, &v)
}

// AddRules adds or replaces rules (same string_to_replace replaces) and returns the new version.
func (c *Client) AddRules(ctx context.Context, id string, rules []PronRule) (*DictVersion, error) {
	r, err := c.Do(ctx, http.MethodPost, "/v1/pronunciation-dictionaries/"+url.PathEscape(id)+"/add-rules", nil, map[string]any{"rules": rules}, false)
	if err != nil {
		return nil, err
	}
	var v DictVersion
	return &v, json.Unmarshal(r.Body, &v)
}

// RemoveRules removes rules by their string_to_replace and returns the new version.
func (c *Client) RemoveRules(ctx context.Context, id string, strs []string) (*DictVersion, error) {
	r, err := c.Do(ctx, http.MethodPost, "/v1/pronunciation-dictionaries/"+url.PathEscape(id)+"/remove-rules", nil, map[string]any{"rule_strings": strs}, false)
	if err != nil {
		return nil, err
	}
	var v DictVersion
	return &v, json.Unmarshal(r.Body, &v)
}

// EffectEntry is one item of the audio effects catalog.
type EffectEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// EffectsCatalog lists valid audio_effects IDs.
type EffectsCatalog struct {
	Filters      []EffectEntry `json:"filter_presets"`
	Environments []EffectEntry `json:"environments"`
	Noises       []EffectEntry `json:"background_noises"`
}

// Catalog fetches the audio effects catalog (memoised for the process lifetime).
func (c *Client) Catalog(ctx context.Context) (*EffectsCatalog, error) {
	c.catMu.Lock()
	defer c.catMu.Unlock()
	if c.catalog != nil {
		return c.catalog, nil
	}
	var raw struct {
		Filters []struct {
			ID   string `json:"filter_preset_id"`
			Name string `json:"name"`
			Desc string `json:"description"`
		} `json:"filter_presets"`
		Envs []struct {
			ID   string `json:"environment_id"`
			Name string `json:"name"`
			Desc string `json:"description"`
		} `json:"environments"`
		Noises []struct {
			ID   string `json:"background_noise_id"`
			Name string `json:"name"`
			Desc string `json:"description"`
		} `json:"background_noises"`
	}
	if err := c.GetJSON(ctx, "/v1/audio-effects/catalog", nil, &raw); err != nil {
		return nil, err
	}
	cat := &EffectsCatalog{}
	for _, x := range raw.Filters {
		cat.Filters = append(cat.Filters, EffectEntry{x.ID, x.Name, x.Desc})
	}
	for _, x := range raw.Envs {
		cat.Environments = append(cat.Environments, EffectEntry{x.ID, x.Name, x.Desc})
	}
	for _, x := range raw.Noises {
		cat.Noises = append(cat.Noises, EffectEntry{x.ID, x.Name, x.Desc})
	}
	c.catalog = cat
	return cat, nil
}

// Validate checks effect IDs against the catalog.
func (cat *EffectsCatalog) Validate(e *Effects) error {
	if e == nil || cat == nil {
		return nil
	}
	has := func(list []EffectEntry, id string) bool {
		for _, x := range list {
			if x.ID == id {
				return true
			}
		}
		return false
	}
	ids := func(list []EffectEntry) string {
		var s []string
		for _, x := range list {
			s = append(s, x.ID)
		}
		return strings.Join(s, ", ")
	}
	if e.FilterPresetID != "" && !has(cat.Filters, e.FilterPresetID) {
		return fmt.Errorf("unknown filter %q (valid: %s)", e.FilterPresetID, ids(cat.Filters))
	}
	if e.EnvironmentID != "" && !has(cat.Environments, e.EnvironmentID) {
		return fmt.Errorf("unknown environment %q (valid: %s)", e.EnvironmentID, ids(cat.Environments))
	}
	if e.BackgroundNoiseID != "" && !has(cat.Noises, e.BackgroundNoiseID) {
		return fmt.Errorf("unknown background noise %q (valid: %s)", e.BackgroundNoiseID, ids(cat.Noises))
	}
	return nil
}

// Key is a stable signature of the effect settings ("" when none).
func (e *Effects) Key() string {
	if e.IsZero() {
		return ""
	}
	b, _ := json.Marshal(e)
	return string(b)
}
