// Package eleven is a small, fast ElevenLabs API client with a content-addressed
// response cache, bounded concurrency and retry with backoff.
package eleven

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Client talks to the ElevenLabs REST API.
type Client struct {
	key     string
	base    string
	http    *http.Client
	sem     chan struct{}
	cache   string
	Hits    atomic.Int64
	Misses  atomic.Int64
	Credits atomic.Int64 // characters/credits billed for requests made in this process (from headers when present)

	catMu   sync.Mutex
	catalog *EffectsCatalog
}

// New returns a client. cacheDir may be empty to disable the cache.
func New(key, base, cacheDir string, maxConcurrency int) *Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     120 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	return &Client{
		key:   key,
		base:  base,
		http:  &http.Client{Transport: tr, Timeout: 10 * time.Minute},
		sem:   make(chan struct{}, maxConcurrency),
		cache: cacheDir,
	}
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	b := e.Body
	if len(b) > 800 {
		b = b[:800] + "…"
	}
	return fmt.Sprintf("ElevenLabs %s %s -> %d: %s", e.Method, e.Path, e.Status, b)
}

// Response is a fetched (or cached) response.
type Response struct {
	Body      []byte
	RequestID string
	Cached    bool
}

type meta struct {
	RequestID string `json:"request_id"`
	Path      string `json:"path"`
	Time      string `json:"time"`
}

// Do performs a request. jsonBody may be nil. When cacheable is true an identical
// earlier request (same path, query and body) is answered from disk at no credit cost.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, jsonBody any, cacheable bool) (*Response, error) {
	if c.key == "" {
		return nil, errors.New("no ElevenLabs API key: set ELEVENLABS_API_KEY, add api_key to ~/.config/goelevenlabs/config.json, or configure the ElevenLabs Player extension in Claude Desktop")
	}
	var body []byte
	if jsonBody != nil {
		var err error
		if body, err = json.Marshal(jsonBody); err != nil {
			return nil, err
		}
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var keyFile string
	if cacheable && c.cache != "" {
		h := sha256.New()
		fmt.Fprintf(h, "%s\n%s\n", method, u)
		h.Write(body)
		sum := hex.EncodeToString(h.Sum(nil))
		keyFile = filepath.Join(c.cache, sum[:2], sum)
		if b, err := os.ReadFile(keyFile + ".bin"); err == nil {
			var m meta
			if mb, err := os.ReadFile(keyFile + ".json"); err == nil {
				_ = json.Unmarshal(mb, &m)
			}
			c.Hits.Add(1)
			return &Response{Body: b, RequestID: m.RequestID, Cached: true}, nil
		}
		c.Misses.Add(1)
	}

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			d := time.Duration(1<<attempt)*400*time.Millisecond + time.Duration(rand.IntN(400))*time.Millisecond
			if ae, ok := lastErr.(*retryAfter); ok && ae.d > 0 {
				d = ae.d
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("xi-api-key", c.key)
		req.Header.Set("User-Agent", "goelevenlabs-mcp/1.0")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if res.StatusCode == 429 || res.StatusCode >= 500 {
			ra, _ := strconv.Atoi(res.Header.Get("Retry-After"))
			lastErr = &retryAfter{d: time.Duration(ra) * time.Second, err: &APIError{res.StatusCode, method, path, string(b)}}
			continue
		}
		if res.StatusCode >= 300 {
			return nil, &APIError{res.StatusCode, method, path, string(b)}
		}
		if cc, err := strconv.Atoi(res.Header.Get("character-cost")); err == nil {
			c.Credits.Add(int64(cc))
		}
		r := &Response{Body: b, RequestID: res.Header.Get("request-id")}
		if keyFile != "" {
			_ = os.MkdirAll(filepath.Dir(keyFile), 0o755)
			tmp := keyFile + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, keyFile+".bin")
				mb, _ := json.Marshal(meta{RequestID: r.RequestID, Path: path, Time: time.Now().Format(time.RFC3339)})
				_ = os.WriteFile(keyFile+".json", mb, 0o644)
			}
		}
		return r, nil
	}
	if ra, ok := lastErr.(*retryAfter); ok {
		return nil, ra.err
	}
	return nil, lastErr
}

type retryAfter struct {
	d   time.Duration
	err error
}

func (r *retryAfter) Error() string { return r.err.Error() }

// GetJSON fetches and decodes a JSON endpoint (never cached).
func (c *Client) GetJSON(ctx context.Context, path string, q url.Values, out any) error {
	r, err := c.Do(ctx, http.MethodGet, path, q, nil, false)
	if err != nil {
		return err
	}
	return json.Unmarshal(r.Body, out)
}

// CacheStats reports cache size on disk.
func (c *Client) CacheStats() (files int, bytes int64) {
	if c.cache == "" {
		return
	}
	_ = filepath.WalkDir(c.cache, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".bin" {
			if fi, e := d.Info(); e == nil {
				files++
				bytes += fi.Size()
			}
		}
		return nil
	})
	return
}
