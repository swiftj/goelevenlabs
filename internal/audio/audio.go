// Package audio is a small pure-Go audio engine: MP3/WAV decode, resampling,
// timeline mixing with fades/loops/sidechain ducking, EBU R128 style loudness
// normalisation, a look-ahead peak limiter, and WAV/MP3 encoding.
package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	shine "github.com/braheezy/shine-mp3/pkg/mp3"
	gomp3 "github.com/hajimehoshi/go-mp3"
)

// Rate is the engine's working sample rate. Every asset is requested at 44.1 kHz.
const Rate = 44100

// Buffer is interleaved stereo float32 audio at Rate.
type Buffer struct {
	S []float32 // L,R,L,R...
}

// Frames is the number of stereo frames.
func (b *Buffer) Frames() int { return len(b.S) / 2 }

// Seconds is the duration.
func (b *Buffer) Seconds() float64 { return float64(b.Frames()) / Rate }

// NewSilence returns a silent buffer.
func NewSilence(seconds float64) *Buffer {
	n := int(math.Round(seconds * Rate))
	if n < 0 {
		n = 0
	}
	return &Buffer{S: make([]float32, n*2)}
}

// FramesAt converts seconds to frames.
func FramesAt(sec float64) int { return int(math.Round(sec * Rate)) }

// DecodeMP3 decodes MP3 bytes to a Buffer, resampling if needed.
func DecodeMP3(data []byte) (*Buffer, error) {
	d, err := gomp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("mp3 decode: %w", err)
	}
	pcm, err := io.ReadAll(d)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("mp3 decode: %w", err)
	}
	n := len(pcm) / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	b := &Buffer{S: out}
	if sr := d.SampleRate(); sr != Rate {
		b = resample(b, sr)
	}
	return b, nil
}

// DecodeWAV decodes 16/24/32-bit PCM or 32-bit float WAV (mono or stereo).
func DecodeWAV(data []byte) (*Buffer, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	var ch, bits, format int
	var sr int
	p := 12
	for p+8 <= len(data) {
		id := string(data[p : p+4])
		sz := int(binary.LittleEndian.Uint32(data[p+4:]))
		body := data[p+8 : min(len(data), p+8+sz)]
		switch id {
		case "fmt ":
			format = int(binary.LittleEndian.Uint16(body[0:]))
			ch = int(binary.LittleEndian.Uint16(body[2:]))
			sr = int(binary.LittleEndian.Uint32(body[4:]))
			bits = int(binary.LittleEndian.Uint16(body[14:]))
			if format == 0xFFFE && len(body) >= 26 {
				format = int(binary.LittleEndian.Uint16(body[24:]))
			}
		case "data":
			if ch == 0 {
				return nil, errors.New("WAV data before fmt chunk")
			}
			bps := bits / 8
			frames := len(body) / (bps * ch)
			out := make([]float32, frames*2)
			for f := 0; f < frames; f++ {
				for c := 0; c < 2; c++ {
					src := c
					if src >= ch {
						src = 0
					}
					o := (f*ch + src) * bps
					var v float32
					switch {
					case format == 3 && bits == 32:
						v = math.Float32frombits(binary.LittleEndian.Uint32(body[o:]))
					case bits == 16:
						v = float32(int16(binary.LittleEndian.Uint16(body[o:]))) / 32768
					case bits == 24:
						x := int32(body[o]) | int32(body[o+1])<<8 | int32(int8(body[o+2]))<<16
						v = float32(x) / 8388608
					case bits == 32:
						v = float32(int32(binary.LittleEndian.Uint32(body[o:]))) / 2147483648
					case bits == 8:
						v = (float32(body[o]) - 128) / 128
					default:
						return nil, fmt.Errorf("unsupported WAV: %d-bit format %d", bits, format)
					}
					out[f*2+c] = v
				}
			}
			b := &Buffer{S: out}
			if sr != Rate {
				b = resample(b, sr)
			}
			return b, nil
		}
		p += 8 + sz + sz%2
	}
	return nil, errors.New("WAV has no data chunk")
}

// LoadFile decodes an .mp3 or .wav file.
func LoadFile(path string) (*Buffer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(filepath.Ext(path), ".wav") {
		return DecodeWAV(data)
	}
	return DecodeMP3(data)
}

// resample converts with windowed-sinc interpolation (8 zero crossings each side).
func resample(b *Buffer, from int) *Buffer {
	if from == Rate || b.Frames() == 0 {
		return b
	}
	ratio := float64(Rate) / float64(from)
	inF := b.Frames()
	outF := int(float64(inF) * ratio)
	out := make([]float32, outF*2)
	const taps = 8
	cutoff := math.Min(1, ratio)
	for i := 0; i < outF; i++ {
		t := float64(i) / ratio
		c := int(t)
		var l, r, wsum float64
		for k := c - taps + 1; k <= c+taps; k++ {
			if k < 0 || k >= inF {
				continue
			}
			x := (t - float64(k)) * cutoff
			w := sinc(x) * (0.5 + 0.5*math.Cos(math.Pi*(t-float64(k))/taps)) // Hann window
			l += float64(b.S[2*k]) * w
			r += float64(b.S[2*k+1]) * w
			wsum += w
		}
		if wsum != 0 {
			l /= wsum
			r /= wsum
		}
		out[2*i], out[2*i+1] = float32(l), float32(r)
	}
	return &Buffer{S: out}
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// Append concatenates buffers.
func Append(bufs ...*Buffer) *Buffer {
	n := 0
	for _, b := range bufs {
		n += len(b.S)
	}
	out := make([]float32, 0, n)
	for _, b := range bufs {
		out = append(out, b.S...)
	}
	return &Buffer{S: out}
}

// Peak returns the absolute sample peak.
func (b *Buffer) Peak() float64 {
	var p float32
	for _, v := range b.S {
		if v < 0 {
			v = -v
		}
		if v > p {
			p = v
		}
	}
	return float64(p)
}

// DB converts decibels to linear gain.
func DB(db float64) float64 { return math.Pow(10, db/20) }

// ---------------------------------------------------------------- encoding

// EncodeWAV writes 16-bit stereo PCM WAV with TPDF dither.
func EncodeWAV(b *Buffer) []byte {
	n := len(b.S)
	buf := make([]byte, 44+n*2)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+n*2))
	copy(buf[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[22:], 2)
	binary.LittleEndian.PutUint32(buf[24:], Rate)
	binary.LittleEndian.PutUint32(buf[28:], Rate*4)
	binary.LittleEndian.PutUint16(buf[32:], 4)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(n*2))
	pcm := toInt16(b)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(buf[44+2*i:], uint16(v))
	}
	return buf
}

func toInt16(b *Buffer) []int16 {
	out := make([]int16, len(b.S))
	var seed uint32 = 22222
	rnd := func() float64 { // xorshift, cheap TPDF dither
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		return float64(seed)/4294967296.0 - 0.5
	}
	for i, v := range b.S {
		x := float64(v)*32767 + rnd() + rnd()
		if x > 32767 {
			x = 32767
		} else if x < -32768 {
			x = -32768
		}
		out[i] = int16(math.Round(x))
	}
	return out
}

// FFmpegPath returns ffmpeg's path if installed (used only for higher-quality MP3).
func FFmpegPath() string {
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// EncodeMP3 encodes to MP3. encoder is "auto", "builtin" or "ffmpeg".
// It returns the encoder actually used.
func EncodeMP3(b *Buffer, encoder string) ([]byte, string, error) {
	if encoder != "builtin" {
		if ff := FFmpegPath(); ff != "" {
			cmd := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0",
				"-c:a", "libmp3lame", "-q:a", "2", "-f", "mp3", "pipe:1")
			pcm := toInt16(b)
			raw := make([]byte, len(pcm)*2)
			for i, v := range pcm {
				binary.LittleEndian.PutUint16(raw[2*i:], uint16(v))
			}
			cmd.Stdin = bytes.NewReader(raw)
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err == nil {
				return out.Bytes(), "ffmpeg-lame-v2", nil
			} else if encoder == "ffmpeg" {
				return nil, "", fmt.Errorf("ffmpeg: %v %s", err, errb.String())
			}
		} else if encoder == "ffmpeg" {
			return nil, "", errors.New("ffmpeg not found")
		}
	}
	// Built-in pure-Go Shine encoder (128 kbps CBR). Encode frame by frame:
	// the library's own Write() mis-sizes stereo chunks.
	enc := shine.NewEncoder(Rate, 2)
	pcm := toInt16(b)
	const frame = 1152 * 2
	var out bytes.Buffer
	chunk := make([]int16, frame)
	for i := 0; i < len(pcm); i += frame {
		n := copy(chunk, pcm[i:min(len(pcm), i+frame)])
		for j := n; j < frame; j++ {
			chunk[j] = 0
		}
		data, written := enc.EncodeBufferInterleaved(chunk)
		out.Write(data[:written])
	}
	return out.Bytes(), "builtin-shine-128k", nil
}

// Save writes b to path, choosing the codec from the extension.
func Save(b *Buffer, path, encoder string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	var data []byte
	used := "wav-16"
	if strings.EqualFold(filepath.Ext(path), ".wav") {
		data = EncodeWAV(b)
	} else {
		var err error
		if data, used, err = EncodeMP3(b, encoder); err != nil {
			return "", err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	return used, os.Rename(tmp, path)
}
