package scene

import (
	"strings"
	"testing"
)

const sample = `
# Campfire scene
NARRATOR: [softly] The fire crackles.
{ambience: night forest, crickets}
{music: warm folk guitar | -18dB | fade=4}
SKERRIT: [chuckles] Sit, child.
  The road will wait.
{sfx: wolf howl in the distance | -10dB | pan=-0.6 | 3s | at=+0.4}
{pause: 1.5}
TRAVELER: Thank you.
{music: stop}
{sfx: log collapses into embers | at=line:3}
`

func TestParseScript(t *testing.T) {
	p, err := ParseScript(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lines) != 3 {
		t.Fatalf("lines = %d", len(p.Lines))
	}
	if p.Lines[1].Text != "[chuckles] Sit, child. The road will wait." {
		t.Errorf("continuation: %q", p.Lines[1].Text)
	}
	if p.Lines[1].PauseAfter != 1.5 {
		t.Errorf("pause = %v", p.Lines[1].PauseAfter)
	}
	if len(p.Music) != 1 || p.Music[0].From != "after:1" || p.Music[0].To != "after:3" || *p.Music[0].GainDB != -18 || *p.Music[0].FadeIn != 4 {
		t.Errorf("music: %+v", p.Music)
	}
	if len(p.Ambience) != 1 || p.Ambience[0].From != "after:1" || p.Ambience[0].To != "" {
		t.Errorf("ambience: %+v", p.Ambience)
	}
	s := p.SFX[0]
	if s.At != "after:2+0.4" || s.Pan != -0.6 || s.DurationSec != 3 || *s.GainDB != -10 {
		t.Errorf("sfx: %+v", s)
	}
	if p.SFX[1].At != "line:3" {
		t.Errorf("sfx2 at %q", p.SFX[1].At)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"no speaker here", "A: hi\n{boom: x}", "A: hi\n{sfx: x | wat}", "A: hi\n{pause: soon}"} {
		if _, err := ParseScript(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestAnchor(t *testing.T) {
	lines := []TimedLine{{Start: 1, End: 3}, {Start: 4, End: 6.5}}
	cases := map[string]float64{"start": 0, "end": 10, "line:2": 4, "after:2": 6.5, "after:1+0.4": 3.4, "line:2-0.5": 3.5, "12.5": 12.5, "3s": 3, "end-2": 8}
	for a, want := range cases {
		got, err := anchor(a, lines, 10)
		if err != nil || got != want {
			t.Errorf("anchor(%q) = %v, %v; want %v", a, got, err, want)
		}
	}
	for _, bad := range []string{"line:3", "after:0", "middle", "line:x"} {
		if _, err := anchor(bad, lines, 10); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestBlocksAndSplit(t *testing.T) {
	long := strings.Repeat("This is a sentence that goes on. ", 100) // ~3300 chars
	parts := splitText(long, maxBlockChars)
	if len(parts) < 2 {
		t.Fatalf("expected split, got %d", len(parts))
	}
	for _, p := range parts {
		if len(p) > maxBlockChars {
			t.Fatalf("part too long: %d", len(p))
		}
	}
	lines := []Line{{Text: "a"}, {Text: "b", PauseAfter: 1}, {Text: "c"}}
	var in []input
	for i, l := range lines {
		in = append(in, input{line: i})
		in[i].Text, in[i].VoiceID = l.Text, "v"
	}
	if b := makeBlocks(in, lines); len(b) != 2 || len(b[0]) != 2 {
		t.Errorf("pause should split blocks: %d", len(b))
	}
}

func TestSRT(t *testing.T) {
	s := srt([]TimedLine{{Speaker: "A", Text: "[whispers] hello  there", Start: 1.25, End: 3661.5}})
	if !strings.Contains(s, "00:00:01,250 --> 01:01:01,500\nA: hello there") {
		t.Errorf("srt: %q", s)
	}
}
