// Package fetch is a faithful Go port of the Lisa fetch-and-cache primitive
// (lisa/sdk/content/_fetcher.py). It preserves the non-bypassable SSRF gate,
// the redirect-hop re-validation, L1 in-memory caching, conditional-GET
// (ETag/Last-Modified), stale-cache fallback on transport error, the honest
// User-Agent, and the absolute max_bytes DoS ceiling.
//
// It implements the safety invariants required by the contract
// (lisa/docs/GO_CONNECTOR_SDK.md): a port that weakens the SSRF gate or the
// size ceiling is a defect, not a shortcut.
package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Security constants (mirror _fetcher.py).
const (
	absoluteMaxBytes = 1_000_000
	userAgent        = "LisaHarness/0.1.1 (+https://github.com/Lisa-Framework/lisa)"
	l2CacheSubdir    = "files/_fetch"
)

// Navigation headers a real browser sends on a top-level document request.
// Bot defenses key on these, not the User-Agent; see the benchmark note in
// _fetcher.py. Accept/Accept-Language are deliberately omitted.
var navigationHeaders = map[string]string{
	"Upgrade-Insecure-Requests": "1",
	"Sec-Fetch-Dest":            "document",
	"Sec-Fetch-Mode":            "navigate",
	"Sec-Fetch-Site":            "none",
}

// defaultPrivateIPBlocks are RFC1918 + loopback + link-local + unique-local
// IPv6. A host on a LAN fetching internal docs can relax this via
// WithPrivateIPBlocks(nil).
var defaultPrivateIPBlocks = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"0.0.0.0/8",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
}

// DefaultPolicy is the harness default Policy (ttl 24h, 200KB cap).
func DefaultPolicy() Policy {
	return Policy{
		TTLSeconds:               86_400,
		MaxBytes:                 200_000,
		Cache:                    true,
		TimeoutSeconds:           15,
		FollowRedirects:          true,
		BrowserNavigationHeaders: true,
		PersistCookies:           true,
	}
}

// Policy is the caller-owned cache + fetch policy. The harness imposes no cap
// on any field; only the absoluteMaxBytes DoS ceiling is applied as a clamp,
// because it is a security bound, not a freshness rule.
type Policy struct {
	TTLSeconds      float64
	ForceRefresh    bool
	MaxBytes        int
	Cache           bool
	RevalidateETag  bool
	TimeoutSeconds  float64
	FollowRedirects bool
	// AuthHeader is a "name: value" header or bare token (-> Authorization).
	AuthHeader string
	// ExtraHeaders merge over the default navigation set.
	ExtraHeaders map[string]string
	// BrowserNavigationHeaders sends the navigation set unless false.
	BrowserNavigationHeaders bool
	// PersistCookies remembers Set-Cookie across fetches (challenge gating).
	PersistCookies bool
}

// Result is one fetched document.
type Result struct {
	URL          string `json:"url"`
	StatusCode   int    `json:"status_code"`
	Body         []byte `json:"-"`
	Text         string `json:"text"`
	ContentType  string `json:"content_type"`
	ETag         string `json:"etag"`
	LastModified string `json:"last_modified"`
	FetchedAt    string `json:"fetched_at,omitempty"`
	FromCache    bool   `json:"from_cache"`

	fetchedTime time.Time
}

// StatusError is an HTTP error status. It carries the status code so callers
// can distinguish "the server refused this" (403/429) from a transport
// failure. It is NOT a transport error: stale cache must never mask it.
type StatusError struct {
	URL        string
	StatusCode int
	Reason     string
}

func (e *StatusError) Error() string {
	detail := ""
	if e.Reason != "" {
		detail = " (" + e.Reason + ")"
	}
	return fmt.Sprintf("HTTP %d%s for url '%s'", e.StatusCode, detail, e.URL)
}

// ssrfError is returned when the security gate rejects a URL.
type ssrfError struct{ reason string }

func (e *ssrfError) Error() string { return e.reason }

// Fetcher is the fetch-and-cache primitive. The zero value is not usable;
// construct via New.
type Fetcher struct {
	dataDir         string
	defaultPolicy   Policy
	allowedHosts    map[string]struct{} // nil = any public host
	allowHTTP       bool
	privateIPBlocks []*net.IPNet

	mu      sync.Mutex
	l1      map[string]*Result
	l2      *l2Cache
	cookies http.CookieJar
}

// Option configures a Fetcher.
type Option func(*Fetcher)

// WithPolicy sets the default policy.
func WithPolicy(p Policy) Option { return func(f *Fetcher) { f.defaultPolicy = p } }

// WithAllowedHosts restricts fetching to the given lowercased hosts.
func WithAllowedHosts(hosts []string) Option {
	return func(f *Fetcher) {
		set := make(map[string]struct{}, len(hosts))
		for _, h := range hosts {
			set[strings.ToLower(h)] = struct{}{}
		}
		f.allowedHosts = set
	}
}

// WithAllowHTTP permits http:// URLs (default HTTPS-only).
func WithAllowHTTP(allow bool) Option {
	return func(f *Fetcher) { f.allowHTTP = allow }
}

// WithPrivateIPBlocks overrides the SSRF CIDR blocks (nil disables the check).
func WithPrivateIPBlocks(blocks []string) Option {
	return func(f *Fetcher) { f.privateIPBlocks = parseBlocks(blocks) }
}

// New builds a Fetcher rooted at dataDir. The L2 disk cache lives under
// dataDir/files/_fetch.
func New(dataDir string, opts ...Option) (*Fetcher, error) {
	if dataDir == "" {
		return nil, errors.New("data dir is required")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	f := &Fetcher{
		dataDir:         dataDir,
		defaultPolicy:   DefaultPolicy(),
		privateIPBlocks: parseBlocks(defaultPrivateIPBlocks),
		l1:              make(map[string]*Result),
		cookies:         jar,
	}
	for _, o := range opts {
		o(f)
	}
	return f, nil
}

func parseBlocks(blocks []string) []*net.IPNet {
	var out []*net.IPNet
	for _, b := range blocks {
		if _, n, err := net.ParseCIDR(b); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ── URL validation (the SSRF security gate) ────────────────────────────────

func isPrivateIP(host string, blocks []*net.IPNet) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range blocks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// validateURL is the single source of truth for SSRF rejection. It returns the
// normalized URL or an error describing the rejection. Mirrors validate_url.
func (f *Fetcher) validateURL(rawURL string) (string, error) {
	cleaned := strings.TrimSpace(rawURL)
	if cleaned == "" {
		return "", &ssrfError{"URL is required."}
	}
	parsed, err := url.Parse(cleaned)
	if err != nil {
		return "", &ssrfError{fmt.Sprintf("Invalid URL '%s'.", cleaned)}
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", &ssrfError{fmt.Sprintf("URL must be http or https, got '%s'.", cleaned)}
	}
	if scheme == "http" && !f.allowHTTP {
		return "", &ssrfError{fmt.Sprintf("Blocked HTTP URL '%s'. HTTPS is required by default.", cleaned)}
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", &ssrfError{fmt.Sprintf("URL has no host: %s", cleaned)}
	}
	if f.allowedHosts != nil {
		if _, ok := f.allowedHosts[host]; !ok {
			return "", &ssrfError{fmt.Sprintf("Blocked URL from host '%s'. Not in allowed hosts.", host)}
		}
	}
	if isPrivateIP(host, f.privateIPBlocks) {
		return "", &ssrfError{fmt.Sprintf("Blocked URL to private/loopback host '%s' (SSRF guard).", host)}
	}
	return cleaned, nil
}

// ── Request headers ─────────────────────────────────────────────────────────

func (f *Fetcher) buildHeaders(u string, p Policy) http.Header {
	h := http.Header{}
	h.Set("User-Agent", userAgent)
	if p.BrowserNavigationHeaders {
		for k, v := range navigationHeaders {
			h.Set(k, v)
		}
	}
	for k, v := range p.ExtraHeaders {
		h.Set(k, v)
	}
	if p.AuthHeader != "" {
		if i := strings.Index(p.AuthHeader, ":"); i >= 0 {
			h.Set(strings.TrimSpace(p.AuthHeader[:i]), strings.TrimSpace(p.AuthHeader[i+1:]))
		} else {
			h.Set("Authorization", p.AuthHeader)
		}
	}
	// Conditional GET: attach stale validators (suppressed by force_refresh).
	if p.RevalidateETag && !p.ForceRefresh {
		if stale := f.peekRaw(u); stale != nil {
			if stale.ETag != "" {
				h.Set("If-None-Match", stale.ETag)
			}
			if stale.LastModified != "" {
				h.Set("If-Modified-Since", stale.LastModified)
			}
		}
	}
	return h
}

// ── Public API ──────────────────────────────────────────────────────────────

// Fetch resolves one URL with L1 → L2 → conditional-GET → network. The SSRF
// gate runs first and cannot be bypassed by policy; it is re-applied to every
// redirect hop via CheckRedirect. On a transport error with a valid stale
// cache it returns the stale entry; an HTTP error status raises *StatusError.
func (f *Fetcher) Fetch(rawURL string, policy *Policy) (*Result, error) {
	effective := f.defaultPolicy
	if policy != nil {
		effective = *policy
	}

	cleaned, err := f.validateURL(rawURL)
	if err != nil {
		return nil, err
	}

	maxBytes := effective.MaxBytes
	if maxBytes > absoluteMaxBytes {
		maxBytes = absoluteMaxBytes
	}

	if effective.Cache && !effective.ForceRefresh {
		if cached := f.lookup(cleaned, effective.TTLSeconds); cached != nil {
			return cached, nil
		}
	}

	result, err := f.fetchFromNetwork(cleaned, &effective, maxBytes)
	if err != nil {
		return nil, err
	}
	if effective.Cache {
		f.store(result)
	}
	return result, nil
}

// Invalidate clears the L1 cache and (optionally) one L2 entry. A nil url
// clears everything.
func (f *Fetcher) Invalidate(rawURL *string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rawURL == nil {
		f.l1 = make(map[string]*Result)
		if l2 := f.l2CacheLocked(); l2 != nil {
			l2.clear()
		}
		return
	}
	delete(f.l1, strings.TrimSpace(*rawURL))
	if l2 := f.l2CacheLocked(); l2 != nil {
		l2.delete(cacheKey(*rawURL))
	}
}

// ── Cache ───────────────────────────────────────────────────────────────────

func cacheKey(rawURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawURL)))
	return hex.EncodeToString(sum[:])[:16]
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func decodeBody(body []byte) string { return string(body) }

func (f *Fetcher) l2CacheLocked() *l2Cache {
	if f.l2 == nil {
		dir := filepath.Join(f.dataDir, l2CacheSubdir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil
		}
		f.l2 = &l2Cache{dir: dir}
	}
	return f.l2
}

func (f *Fetcher) lookup(rawURL string, ttlSeconds float64) *Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	cached := f.l1[rawURL]
	if cached == nil {
		if l2 := f.l2CacheLocked(); l2 != nil {
			if r := l2.get(cacheKey(rawURL)); r != nil {
				r.FromCache = true
				cached = r
				f.l1[rawURL] = cached
			}
		}
	}
	if cached == nil {
		return nil
	}
	if isExpired(cached, ttlSeconds) {
		return nil
	}
	if !cached.FromCache {
		cp := *cached
		cp.FromCache = true
		f.l1[rawURL] = &cp
		return &cp
	}
	return cached
}

func isExpired(r *Result, ttlSeconds float64) bool {
	if ttlSeconds <= 0 {
		return false
	}
	if r.fetchedTime.IsZero() {
		return true
	}
	return time.Since(r.fetchedTime).Seconds() > ttlSeconds
}

func (f *Fetcher) store(r *Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fresh := *r
	fresh.FromCache = false
	f.l1[r.URL] = &fresh
	if l2 := f.l2CacheLocked(); l2 != nil {
		l2.set(cacheKey(r.URL), &fresh)
	}
}

// peekRaw returns a cached entry ignoring TTL (stale-fallback / validators).
func (f *Fetcher) peekRaw(rawURL string) *Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cached := f.l1[rawURL]; cached != nil {
		return cached
	}
	if l2 := f.l2CacheLocked(); l2 != nil {
		if r := l2.get(cacheKey(rawURL)); r != nil {
			r.FromCache = true
			return r
		}
	}
	return nil
}

// ── Network ─────────────────────────────────────────────────────────────────

func (f *Fetcher) fetchFromNetwork(rawURL string, policy *Policy, maxBytes int) (*Result, error) {
	// Re-run the SSRF gate on every redirect hop via CheckRedirect: a public
	// URL that 302s to a private address must be refused.
	client := &http.Client{
		Timeout: time.Duration(policy.TimeoutSeconds * float64(time.Second)),
		Jar:     f.cookies,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !policy.FollowRedirects {
				return http.ErrUseLastResponse
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if _, err := f.validateURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = f.buildHeaders(rawURL, *policy)

	resp, err := client.Do(req)
	if err != nil {
		// A redirect-guard rejection surfaces here wrapped in *url.Error.
		var ssrf *ssrfError
		if errors.As(err, &ssrf) {
			return nil, ssrf
		}
		// Transport error: fall back to stale cache when available.
		if stale := f.peekRaw(rawURL); stale != nil {
			cp := *stale
			cp.FromCache = true
			return &cp, nil
		}
		return nil, fmt.Errorf("failed to fetch '%s': %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && policy.RevalidateETag {
		return f.handleNotModified(rawURL, policy), nil
	}

	// An HTTP error status is NOT a transport failure: serving stale content
	// for a 403/429 would present a blocked fetch as fresh success.
	if resp.StatusCode >= 400 {
		return nil, &StatusError{URL: rawURL, StatusCode: resp.StatusCode, Reason: http.StatusText(resp.StatusCode)}
	}

	body, err := readLimited(resp.Body, int64(maxBytes)+1)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > int64(maxBytes) {
		body = body[:maxBytes]
	}

	return resultFromResponse(resp, rawURL, body), nil
}

func (f *Fetcher) handleNotModified(rawURL string, policy *Policy) *Result {
	stale := f.peekRaw(rawURL)
	if stale == nil {
		return &Result{URL: rawURL, StatusCode: http.StatusNotModified, FetchedAt: nowISO(), fetchedTime: time.Now().UTC()}
	}
	refreshed := *stale
	refreshed.FetchedAt = nowISO()
	refreshed.fetchedTime = time.Now().UTC()
	refreshed.FromCache = false
	if policy.Cache {
		f.store(&refreshed)
	}
	return &refreshed
}

func resultFromResponse(resp *http.Response, rawURL string, body []byte) *Result {
	now := time.Now().UTC()
	return &Result{
		URL:          rawURL,
		StatusCode:   resp.StatusCode,
		Body:         body,
		Text:         decodeBody(body),
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("Etag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt:    now.Format(time.RFC3339Nano),
		fetchedTime:  now,
	}
}

// readLimited reads up to n bytes, enforcing the max_bytes DoS ceiling.
func readLimited(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}

// ── L2 disk cache (JSON per entry, keyed by URL hash) ───────────────────────

type l2Cache struct {
	dir string
	mu  sync.Mutex
}

// l2Entry is the on-disk serialized form. Body is stored alongside text for
// parity with the diskcache dict; FetchedTime is the TTL reference.
type l2Entry struct {
	URL          string `json:"url"`
	StatusCode   int    `json:"status_code"`
	Body         []byte `json:"body"`
	Text         string `json:"text"`
	ContentType  string `json:"content_type"`
	ETag         string `json:"etag"`
	LastModified string `json:"last_modified"`
	FetchedAt    string `json:"fetched_at"`
	FetchedTime  int64  `json:"fetched_time_unix"`
}

func (c *l2Cache) path(key string) string {
	return filepath.Join(c.dir, key+".json")
}

func (c *l2Cache) get(key string) *Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil
	}
	var e l2Entry
	if json.Unmarshal(data, &e) != nil {
		return nil
	}
	return &Result{
		URL:          e.URL,
		StatusCode:   e.StatusCode,
		Body:         e.Body,
		Text:         e.Text,
		ContentType:  e.ContentType,
		ETag:         e.ETag,
		LastModified: e.LastModified,
		FetchedAt:    e.FetchedAt,
		FromCache:    true,
		fetchedTime:  time.Unix(e.FetchedTime, 0).UTC(),
	}
}

func (c *l2Cache) set(key string, r *Result) {
	data, err := json.Marshal(l2Entry{
		URL:          r.URL,
		StatusCode:   r.StatusCode,
		Body:         r.Body,
		Text:         r.Text,
		ContentType:  r.ContentType,
		ETag:         r.ETag,
		LastModified: r.LastModified,
		FetchedAt:    r.FetchedAt,
		FetchedTime:  r.fetchedTime.Unix(),
	})
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = os.WriteFile(c.path(key), data, 0o600)
}

func (c *l2Cache) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = os.Remove(c.path(key))
}

func (c *l2Cache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(c.dir, e.Name()))
	}
}
