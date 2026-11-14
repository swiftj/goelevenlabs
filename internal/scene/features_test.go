package scene

import (
	"testing"

	"github.com/swiftj/goelevenlabs/internal/eleven"
)

func TestAliaser(t *testing.T) {
	l := OpenLexicon(t.TempDir())
	if err := l.Set(
		PronEntry{Term: "Szass Tam", Alias: "Sass Tahm"},
		PronEntry{Term: "Thay", Alias: "Tay"},
		PronEntry{Term: "Tam", Alias: "WRONG"}, // shorter overlapping term must lose to "Szass Tam"
		PronEntry{Term: "NASA", Alias: "Nassa", CaseSensitive: true},
		PronEntry{Term: "Pyarados", IPA: "paɪˈærədoʊs"},
	); err != nil {
		t.Fatal(err)
	}
	a := l.Aliaser()
	cases := map[string]string{
		"Szass Tam rules Thay.":         "Sass Tahm rules Tay.",
		"szass tam, of THAY!":           "Sass Tahm, of Tay!",
		"Thayan wizards":                "Thayan wizards", // no partial-word match
		"[whispers] Thay [Thay] Thay":   "[whispers] Tay [Thay] Tay",
		"nasa vs NASA":                  "nasa vs Nassa",
		"Pyarados stays (phoneme rule)": "Pyarados stays (phoneme rule)",
		"Tam alone":                     "WRONG alone",
	}
	for in, want := range cases {
		if got := a.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
	if err := l.Set(PronEntry{Term: "x", Alias: "a", IPA: "b"}); err == nil {
		t.Error("expected error for two kinds")
	}
	if n, _ := l.Remove("thay", "nope"); n != 1 {
		t.Errorf("removed %d", n)
	}
}

func TestVoiceFXParsingAndBlocks(t *testing.T) {
	p, err := ParseScript(`
A: one
{voicefx: SZASS TAM | env=hall | send=0.6 | distance=0.3}
SZASS TAM: two
A: three
SZASS TAM: four
{voicefx: szass tam | none}
SZASS TAM: five
`)
	if err != nil {
		t.Fatal(err)
	}
	fx := p.Lines[1].Effects
	if fx == nil || fx.EnvironmentID != "hall" || *fx.SendLevel != 0.6 || *fx.Distance != 0.3 {
		t.Fatalf("line 2 fx: %+v", fx)
	}
	if p.Lines[0].Effects != nil || p.Lines[2].Effects != nil {
		t.Error("other speakers must not inherit effects")
	}
	if p.Lines[3].Effects == nil || p.Lines[4].Effects == nil || !p.Lines[4].Effects.IsZero() {
		t.Errorf("none should give an explicit empty effects: %+v", p.Lines[4].Effects)
	}
	if _, err := ParseScript("{voicefx: A | wobble=3}\nA: hi"); err == nil {
		t.Error("expected error for unknown option")
	}
	// blocks split whenever the effect signature changes
	var in []input
	for i, l := range p.Lines {
		f := l.Effects
		if f.IsZero() {
			f = nil
		}
		in = append(in, input{eleven.DialogueInput{Text: l.Text, VoiceID: "v"}, i, f})
	}
	if b := makeBlocks(in, p.Lines); len(b) != 5 {
		t.Errorf("blocks = %d, want 5 (plain, fx, plain, fx, plain)", len(b))
	}
}

func TestCatalogValidate(t *testing.T) {
	cat := &eleven.EffectsCatalog{Environments: []eleven.EffectEntry{{ID: "hall"}}, Filters: []eleven.EffectEntry{{ID: "phone"}}}
	if err := cat.Validate(&eleven.Effects{EnvironmentID: "hall", FilterPresetID: "phone"}); err != nil {
		t.Error(err)
	}
	if err := cat.Validate(&eleven.Effects{EnvironmentID: "cathedral"}); err == nil {
		t.Error("expected unknown environment error")
	}
}
