package scene

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/swiftj/goelevenlabs/internal/eleven"
)

// CastMember maps a character name to a voice, remembered across sessions.
type CastMember struct {
	Name    string          `json:"name"`
	VoiceID string          `json:"voice_id"`
	Voice   string          `json:"voice_name,omitempty"`
	Notes   string          `json:"notes,omitempty"`
	Group   string          `json:"group,omitempty"`
	Effects *eleven.Effects `json:"effects,omitempty" jsonschema:"default voice effects"`
}

// Cast is a JSON-file backed cast list.
type Cast struct {
	path string
	mu   sync.Mutex
}

// OpenCast opens (lazily) the cast file in dir.
func OpenCast(dir string) *Cast { return &Cast{path: filepath.Join(dir, "cast.json")} }

func (c *Cast) load() map[string]CastMember {
	m := map[string]CastMember{}
	if b, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (c *Cast) save(m map[string]CastMember) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func key(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// List returns all members, optionally filtered by group.
func (c *Cast) List(group string) []CastMember {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []CastMember
	for _, m := range c.load() {
		if group == "" || strings.EqualFold(m.Group, group) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Set adds or replaces a member.
func (c *Cast) Set(m CastMember) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	all := c.load()
	all[key(m.Name)] = m
	return c.save(all)
}

// Remove deletes a member; reports whether it existed.
func (c *Cast) Remove(name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	all := c.load()
	if _, ok := all[key(name)]; !ok {
		return false, nil
	}
	delete(all, key(name))
	return true, c.save(all)
}

// Lookup returns the voice_id for a name.
func (c *Cast) Lookup(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.load()[key(name)]
	return m.VoiceID, ok
}

// Get returns a member by name.
func (c *Cast) Get(name string) (CastMember, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.load()[key(name)]
	return m, ok
}
