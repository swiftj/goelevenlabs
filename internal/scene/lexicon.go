package scene

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/johnswift/goelevenlabs/internal/eleven"
)

// PronEntry says how to pronounce a term. Exactly one of Alias, IPA or CMU is set.
//
// Aliases are applied locally to the text before it is sent, so they work with every
// model, need no API permission, and leave subtitles untouched. IPA/CMU phonemes are
// synced to an ElevenLabs pronunciation dictionary that is attached to each request
// (honoured by eleven_v3, eleven_v4, eleven_v4_turbo and eleven_flash_v2).
type PronEntry struct {
	Term          string `json:"term" jsonschema:"word or phrase as written, e.g. Szass Tam"`
	Alias         string `json:"alias,omitempty" jsonschema:"respelling that sounds right, e.g. Sass Tahm"`
	IPA           string `json:"ipa,omitempty" jsonschema:"IPA phonemes, e.g. sæs tɑːm"`
	CMU           string `json:"cmu,omitempty" jsonschema:"CMU Arpabet phonemes, e.g. S AE1 S T AA1 M"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
	Group         string `json:"group,omitempty" jsonschema:"optional campaign/project grouping"`
	Notes         string `json:"notes,omitempty"`
}

func (e PronEntry) kind() string {
	switch {
	case e.IPA != "" || e.CMU != "":
		return "phoneme"
	default:
		return "alias"
	}
}

func (e PronEntry) rule() eleven.PronRule {
	r := eleven.PronRule{StringToReplace: e.Term, CaseSensitive: e.CaseSensitive, WordBoundaries: true}
	switch {
	case e.IPA != "":
		r.Type, r.Phoneme, r.Alphabet = "phoneme", e.IPA, "ipa"
	case e.CMU != "":
		r.Type, r.Phoneme, r.Alphabet = "phoneme", e.CMU, "cmu-arpabet"
	default:
		r.Type, r.Alias = "alias", e.Alias
	}
	return r
}

type lexFile struct {
	Entries map[string]PronEntry `json:"entries"`
	Remote  struct {
		ID        string            `json:"id,omitempty"`
		VersionID string            `json:"version_id,omitempty"`
		Synced    map[string]string `json:"synced,omitempty"` // term -> rule signature
	} `json:"remote"`
}

// Lexicon is the JSON-file backed pronunciation lexicon.
type Lexicon struct {
	path string
	mu   sync.Mutex
}

// OpenLexicon opens (lazily) dir/lexicon.json.
func OpenLexicon(dir string) *Lexicon { return &Lexicon{path: filepath.Join(dir, "lexicon.json")} }

func (l *Lexicon) load() *lexFile {
	f := &lexFile{Entries: map[string]PronEntry{}}
	if b, err := os.ReadFile(l.path); err == nil {
		_ = json.Unmarshal(b, f)
	}
	if f.Entries == nil {
		f.Entries = map[string]PronEntry{}
	}
	return f
}

func (l *Lexicon) save(f *lexFile) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// List returns entries sorted by term, optionally filtered by group.
func (l *Lexicon) List(group string) []PronEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []PronEntry
	for _, e := range l.load().Entries {
		if group == "" || strings.EqualFold(e.Group, group) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Term) < strings.ToLower(out[j].Term) })
	return out
}

// Set adds or replaces entries.
func (l *Lexicon) Set(entries ...PronEntry) error {
	for _, e := range entries {
		n := 0
		for _, v := range []string{e.Alias, e.IPA, e.CMU} {
			if strings.TrimSpace(v) != "" {
				n++
			}
		}
		if strings.TrimSpace(e.Term) == "" || n != 1 {
			return fmt.Errorf("entry %q: give a term and exactly one of alias, ipa or cmu", e.Term)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.load()
	for _, e := range entries {
		e.Term = strings.TrimSpace(e.Term)
		f.Entries[strings.ToLower(e.Term)] = e
	}
	return l.save(f)
}

// Remove deletes terms; returns how many existed.
func (l *Lexicon) Remove(terms ...string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.load()
	n := 0
	for _, t := range terms {
		k := strings.ToLower(strings.TrimSpace(t))
		if _, ok := f.Entries[k]; ok {
			delete(f.Entries, k)
			n++
		}
	}
	return n, l.save(f)
}

// Aliaser rewrites text with alias entries in one pass (longest match first).
type Aliaser struct {
	re *regexp.Regexp
	ci map[string]string // lower(term) -> alias
	cs map[string]string // term -> alias (case-sensitive)
}

// Aliaser builds the rewriter for the current alias entries (nil when none).
func (l *Lexicon) Aliaser() *Aliaser {
	a := &Aliaser{ci: map[string]string{}, cs: map[string]string{}}
	var terms []string
	for _, e := range l.List("") {
		if e.kind() != "alias" {
			continue
		}
		terms = append(terms, e.Term)
		if e.CaseSensitive {
			a.cs[e.Term] = e.Alias
		} else {
			a.ci[strings.ToLower(e.Term)] = e.Alias
		}
	}
	if len(terms) == 0 {
		return nil
	}
	sort.Slice(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	q := make([]string, len(terms))
	for i, t := range terms {
		q[i] = regexp.QuoteMeta(t)
	}
	a.re = regexp.MustCompile(`(?i)(?:` + strings.Join(q, "|") + `)|\[[^\]]*\]`)
	return a
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// Apply rewrites text. [audio tags] are left untouched.
func (a *Aliaser) Apply(text string) string {
	if a == nil {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range a.re.FindAllStringIndex(text, -1) {
		s := text[m[0]:m[1]]
		if strings.HasPrefix(s, "[") {
			continue
		}
		if r, _ := utf8.DecodeLastRuneInString(text[:m[0]]); m[0] > 0 && isWordRune(r) {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(text[m[1]:]); m[1] < len(text) && isWordRune(r) {
			continue
		}
		alias, ok := a.cs[s]
		if !ok {
			alias, ok = a.ci[strings.ToLower(s)]
		}
		if !ok {
			continue // case-sensitive term matched with the wrong case
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(alias)
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

// Sync pushes phoneme entries to an ElevenLabs pronunciation dictionary, changing only
// what differs from the last sync, and returns its locator (nil when there are none).
func (l *Lexicon) Sync(ctx context.Context, c *eleven.Client) (*eleven.DictLocator, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.load()
	want := map[string]eleven.PronRule{}
	sig := map[string]string{}
	for _, e := range f.Entries {
		if e.kind() == "phoneme" {
			r := e.rule()
			want[e.Term] = r
			b, _ := json.Marshal(r)
			sig[e.Term] = string(b)
		}
	}
	if len(want) == 0 && f.Remote.ID == "" {
		return nil, nil
	}
	var add []eleven.PronRule
	for t, r := range want {
		if f.Remote.Synced[t] != sig[t] {
			add = append(add, r)
		}
	}
	var del []string
	for t := range f.Remote.Synced {
		if _, ok := want[t]; !ok {
			del = append(del, t)
		}
	}
	sort.Strings(del)
	sort.Slice(add, func(i, j int) bool { return add[i].StringToReplace < add[j].StringToReplace })
	var v *eleven.DictVersion
	var err error
	switch {
	case f.Remote.ID == "":
		v, err = c.CreateDictionary(ctx, "goelevenlabs lexicon", "Managed by goelevenlabs; edit with manage_pronunciation", add)
	default:
		if len(del) > 0 {
			if v, err = c.RemoveRules(ctx, f.Remote.ID, del); err != nil {
				break
			}
		}
		if len(add) > 0 {
			v, err = c.AddRules(ctx, f.Remote.ID, add)
		}
	}
	if err != nil {
		var ae *eleven.APIError
		if errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403) {
			return nil, fmt.Errorf("pronunciation dictionary sync needs the API key's Pronunciation Dictionaries permission set to Write: %w", err)
		}
		return nil, err
	}
	if v != nil {
		f.Remote.ID, f.Remote.VersionID = v.ID, v.VersionID
		f.Remote.Synced = sig
		if err := l.save(f); err != nil {
			return nil, err
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	return &eleven.DictLocator{ID: f.Remote.ID, VersionID: f.Remote.VersionID}, nil
}

// Remote returns the synced dictionary id/version (empty if never synced).
func (l *Lexicon) Remote() (id, version string, synced int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.load()
	return f.Remote.ID, f.Remote.VersionID, len(f.Remote.Synced)
}

// phonemeModels honour dictionary phoneme rules.
var phonemeModels = map[string]bool{"eleven_v3": true, "eleven_v4": true, "eleven_v4_turbo": true, "eleven_flash_v2": true}
