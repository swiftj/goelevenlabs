// Package config resolves the API key, output folder, cache folder and cast file.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Config is the runtime configuration. Every field has a sensible default.
type Config struct {
	APIKey         string `json:"api_key,omitempty"`
	OutputDir      string `json:"output_dir,omitempty"`
	CacheDir       string `json:"cache_dir,omitempty"`
	MaxConcurrency int    `json:"max_concurrency,omitempty"`
	// Encoder: "auto" (ffmpeg/libmp3lame when found, else built-in), "builtin", "ffmpeg".
	Encoder string `json:"encoder,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

// Dir is the per-user config directory (~/.config/goelevenlabs).
func Dir() string {
	if d := os.Getenv("GOELEVENLABS_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "goelevenlabs")
}

// claudeExtensionSettings reads the ElevenLabs Player extension's saved settings in Claude
// Desktop so users who already entered their key there need not enter it again.
func claudeExtensionSettings() (key, out string) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, "Library", "Application Support", "Claude", "Claude Extensions Settings", "ant.dir.gh.elevenlabs.elevenlabs-player.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", ""
	}
	var s struct {
		UserConfig struct {
			APIKey    string `json:"api_key"`
			OutputDir string `json:"output_dir"`
		} `json:"userConfig"`
	}
	if json.Unmarshal(b, &s) != nil {
		return "", ""
	}
	return s.UserConfig.APIKey, s.UserConfig.OutputDir
}

// Load merges, in order of precedence: environment, ~/.config/goelevenlabs/config.json,
// the Claude Desktop ElevenLabs Player extension settings, and defaults.
func Load() (*Config, error) {
	c := &Config{}
	if b, err := os.ReadFile(filepath.Join(Dir(), "config.json")); err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, errors.New("config.json: " + err.Error())
		}
	}
	extKey, extOut := claudeExtensionSettings()
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	home, _ := os.UserHomeDir()
	c.APIKey = pick(os.Getenv("ELEVENLABS_API_KEY"), c.APIKey, extKey)
	c.OutputDir = ExpandHome(pick(os.Getenv("ELEVENLABS_OUTPUT_DIR"), c.OutputDir, extOut, filepath.Join(home, "Audio")))
	cacheBase, _ := os.UserCacheDir()
	c.CacheDir = ExpandHome(pick(os.Getenv("GOELEVENLABS_CACHE_DIR"), c.CacheDir, filepath.Join(cacheBase, "goelevenlabs")))
	c.Encoder = pick(os.Getenv("GOELEVENLABS_ENCODER"), c.Encoder, "auto")
	c.BaseURL = pick(os.Getenv("ELEVENLABS_BASE_URL"), c.BaseURL, "https://api.elevenlabs.io")
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = 4
	}
	return c, nil
}

// ExpandHome expands a leading ~.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
