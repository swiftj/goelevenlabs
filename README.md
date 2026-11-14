# goelevenlabs

A single static Go binary (`CGO_ENABLED=0`, ~9 MB, no runtime dependencies) that turns a script into **one finished audio scene**: several voices, a music bed that automatically dips under speech, looping ambience and timed sound effects. The result is loudness-normalised and comes with subtitles and a cue sheet. It runs as an MCP server for Claude Desktop and Claude Code, and also as a CLI.

## Tools

| Tool | What it does |
|---|---|
| `render_scene` | Script → mixed scene. Dialogue (eleven_v3) + `{music:}` + `{ambience:}` + `{sfx:}` cues. Writes `.mp3`/`.wav`, `.srt` subtitles, a `.cues.json` cue sheet and optional stems |
| `generate_dialogue` | Multi-voice dialogue only, as one file with per-line timings |
| `generate_sound_effect` / `generate_music` | A single sound effect (or loop) / a music track, saved as MP3 |
| `mix_audio` | Joins or mixes existing local files: concatenate clips, or lay music under dialogue with ducking. **Costs no credits** |
| `search_voices`, `manage_cast` | Find voices; remember which voice plays which character, and their default voice effects |
| `manage_pronunciation` | Lexicon of names to always pronounce a certain way: `set`, `remove`, `list`, `sync`, and `preview` (speaks a test sentence) |
| `voice_effects_catalog` | Valid effect names: reverb rooms, filters (phone, old_radio…), background noises |
| `account_status`, `list_outputs`, `play_audio` | Credits and cache; recent renders; play a file on the Mac's speakers (`afplay`) |

There is also a prompt, `scene_script_guide`, describing the script syntax.

## Script syntax

```
{ambience: night forest, crickets, crackling campfire | -22dB}
{music: slow fingerpicked folk guitar | -17dB | fade=3}
TRAVELER: [nervous] We came a long way to find you, old one.
SKERRIT: [chuckles warmly] Everyone does, child. Sit.
{sfx: distant wolf howl | -12dB | pan=-0.6 | 3s | at=+0.3}
{pause: 1.2}
{voicefx: SKERRIT | env=hall | send=0.6}
SKERRIT: [whispers] Listen.
TRAVELER: [relieved] Thank you.
{music: stop}
```

- **Cue options:** `-6dB`, `fade=` / `fadein=` / `fadeout=`, `pan=`, `2s`, `influence=`, `vocals`, `duck` / `noduck`, `model=`, and `at=` (`+0.4`, `line:3`, `after:3`, `12.5`).
- **Local files:** `{sfx: file=/path/door.wav}` uses an existing file instead of generating one.
- **Voice effects:** `{voicefx: NAME | env=hall | filter=phone | noise=cafe | distance=0.4 | send=0.6 | pan=-0.3}` applies from that point on; `{voicefx: NAME | none}` turns them off; NAME can be `all`.
- **Speaker names** are looked up in this order: the scene's `voices`, then the saved cast, then a match against your voice library names (e.g. `SKERRIT` finds `TW - Skerrit`).

## Pronunciation

The lexicon (`~/.config/goelevenlabs/lexicon.json`) is applied to every render; pass `no_pronunciation: true` to skip it for one render.

- **Alias rules** (`Szass Tam` → `Sass Tahm`) rewrite the text before it's sent. They work with every model, need no extra API permission, and never touch the subtitles or `[audio tags]`. Matching is whole-word and case-insensitive unless you say otherwise, and the longest term wins.
- **IPA / CMU phoneme rules** sync to one ElevenLabs pronunciation dictionary, "goelevenlabs lexicon". Only changed rules are sent, and the current version is attached to every dialogue request. They need the API key's Pronunciation Dictionaries permission set to Write, and only `eleven_v3`, `eleven_v4`, `eleven_v4_turbo` and `eleven_flash_v2` use them; other models get a warning.
- **`preview`** speaks a sentence in any voice, saves it to `pronunciation_previews/`, and can play it right away.

## Voice effects per character

Effects come from, highest priority first: the line itself, then `{voicefx:}` in the script, then the scene's `character_effects`, then the cast's saved effects, then the scene's `voice_effects`.

The `audio_effects` setting applies to a whole request, so lines with different effects are rendered as separate blocks. With eleven_v3 those blocks render in parallel. Each block starts 0.3 s after the previous block's speech ends, measured from voice timestamps rather than the end of the clip, so reverb tails ring naturally under the next line. Effect names are checked against the live catalog before anything is generated.

## Performance

These numbers come from a test render: 3 lines, music, ambience and one SFX.

- **First render: 8.5 s total.** Dialogue, sound effects and ambience are generated at the same time; music starts as soon as the scene length is known.
- **Re-render after a level change: 0.94 s, 0 credits.** Every API response is cached by its content (`~/Library/Caches/goelevenlabs`), so changing levels, cue positions or mix settings re-uses audio already generated. Pass `fresh: true` for a new take.
- **Mixing is fast:** a 60 s mix with ducking, loudness measurement and limiting takes about 0.11 s.
- **Long scripts** are split into blocks of 1,900 characters or fewer. With `eleven_v3`, which doesn't accept continuity hints, those blocks render in parallel.
- **API calls** reuse HTTP/2 connections, retry with backoff on 429/5xx (honouring `Retry-After`), and run at most 4 at a time (`max_concurrency`).
- **MP3 encoding** uses ffmpeg/LAME (VBR V2) when it is installed, and otherwise a built-in pure-Go encoder (128k CBR). WAV output is always pure Go.

## Configuration

**API key** — the first one found is used:
1. `$ELEVENLABS_API_KEY`
2. `~/.config/goelevenlabs/config.json` (`{"api_key": "...", "output_dir": "~/Audio", "max_concurrency": 4, "encoder": "auto"}`)
3. The ElevenLabs Player extension already set up in Claude Desktop

The cast is stored in `~/.config/goelevenlabs/cast.json`. To see credit usage, the API key needs the `user_read` permission.

## Build and install

```bash
go install github.com/swiftj/goelevenlabs@latest   # or build from a clone:
make build      # bin/goelevenlabs (CGO_ENABLED=0, -trimpath, stripped)
make test
make release    # static binaries for darwin/linux/windows
```

- **Claude Desktop:** `claude_desktop_config.json` → add `"goelevenlabs": {"command": "/absolute/path/to/goelevenlabs"}` under `mcpServers`
- **Claude Code:** `claude mcp add --scope user goelevenlabs -- /absolute/path/to/goelevenlabs`
- **CLI:** `goelevenlabs render scene.txt`, `goelevenlabs render -dry scene.json`, `goelevenlabs status`

## Roadmap ideas

1. **Line retakes:** regenerate line N with a new seed, keep a history of takes, and re-mix the rest from cache.
2. **Automatic voice design for new characters:** a cast entry written as a description gets a voice designed (Text to Voice) and saved.
3. **Speech-to-text QA:** transcribe the finished render with Scribe and diff it against the script to catch dropped or garbled lines.
4. **Episode assembly:** join scenes into one episode with chapter markers (ID3 CHAP) and intro/outro stingers.
5. **Live DM soundboard:** crossfade between ambience/music presets during play; send output to a Discord voice channel.
6. **Music that follows the scene:** composition plans whose sections line up with scene beats.
7. **Source import:** render read-aloud boxes straight from Obsidian/LegendKeeper notes.
8. **Budget guard:** a pre-render credit estimate, and a per-render spending cap.
9. **Streamable HTTP transport**, so the server can be used from other machines and claude.ai.

## License

MIT. See [LICENSE](LICENSE).
