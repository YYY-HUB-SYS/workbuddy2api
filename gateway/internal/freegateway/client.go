// Package freegateway provides isolated, keyless OpenAI Chat Completions routes.
// A model must be present in a fresh, provider-confirmed free catalog before use.
package freegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ProviderConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

func (c ProviderConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Config's zero value enables both public providers. Explicit false opts out.
type Config struct {
	OpenCode ProviderConfig `json:"opencode"`
	Kilo     ProviderConfig `json:"kilo"`
}

// Options is a Go-only seam for local tests; remote URLs are not user input.
type Options struct {
	HTTP             *http.Client
	OpenCodeBase     string
	KiloBase         string
	CacheTTL         time.Duration
	FailureBackoff   time.Duration
	DiscoveryTimeout time.Duration
	ChatTimeout      time.Duration
}

type Model struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	OwnedBy       string `json:"owned_by"`
	Name          string `json:"name,omitempty"`
	Free          bool   `json:"free"`
	ContextLength int    `json:"context_length,omitempty"`
}

type Status struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	Available         bool   `json:"available"`
	ModelCount        int    `json:"model_count"`
	LastError         string `json:"last_error"`
	LastRefresh       string `json:"last_refresh,omitempty"`
	LastSuccess       string `json:"last_success,omitempty"`
	CatalogAvailable  bool   `json:"catalog_available"`
	InferenceVerified bool   `json:"inference_verified"`
}

type provider struct {
	id, name, base string
	enabled        bool
	mu             sync.Mutex
	models         []Model
	fetched        time.Time
	failed         time.Time
	listingError   string
	chatError      string
	lastSuccess    time.Time
	loading        chan struct{}
}

type Manager struct {
	providers []*provider
	http      *http.Client
	opts      Options
}

func New(cfg Config, options ...Options) *Manager {
	var o Options
	if len(options) > 0 {
		o = options[0]
	}
	if o.OpenCodeBase == "" {
		o.OpenCodeBase = "https://opencode.ai/inference/v1"
	}
	if o.KiloBase == "" {
		o.KiloBase = "https://api.kilo.ai/api/gateway"
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = 10 * time.Minute
	}
	if o.FailureBackoff <= 0 {
		o.FailureBackoff = time.Minute
	}
	if o.DiscoveryTimeout <= 0 {
		o.DiscoveryTimeout = 12 * time.Second
	}
	if o.ChatTimeout <= 0 {
		o.ChatTimeout = 5 * time.Minute
	}
	var client http.Client
	if o.HTTP != nil {
		client = *o.HTTP
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		client.Transport = transport
	}
	// Do not follow redirects with provider-specific public authorization.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Manager{http: &client, opts: o, providers: []*provider{
		{id: "opencode", name: "OpenCode", base: strings.TrimRight(o.OpenCodeBase, "/"), enabled: cfg.OpenCode.IsEnabled()},
		{id: "kilo", name: "Kilo", base: strings.TrimRight(o.KiloBase, "/"), enabled: cfg.Kilo.IsEnabled()},
	}}
}

func (m *Manager) AnyEnabled() bool {
	if m != nil {
		for _, p := range m.providers {
			if p.enabled {
				return true
			}
		}
	}
	return false
}

func SplitModel(model string) (providerID, bare string, ok bool) {
	for _, id := range []string{"opencode", "kilo"} {
		if strings.HasPrefix(model, id+":") {
			return id, strings.TrimPrefix(model, id+":"), true
		}
	}
	return "", "", false
}

// Models discovers providers concurrently. One failure cannot erase the other.
func (m *Manager) Models(ctx context.Context) []Model {
	out := make([]Model, 0)
	if m == nil {
		return out
	}
	type result struct{ models []Model }
	ch := make(chan result, len(m.providers))
	for _, p := range m.providers {
		go func(p *provider) { models, _ := m.catalog(ctx, p); ch <- result{models} }(p)
	}
	for range m.providers {
		out = append(out, (<-ch).models...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) Status() []Status {
	out := make([]Status, 0)
	if m == nil {
		return out
	}
	for _, p := range m.providers {
		p.mu.Lock()
		s := Status{ID: p.id, Name: p.name, Enabled: p.enabled, LastError: p.listingError}
		if p.chatError != "" {
			s.LastError = p.chatError
		}
		if p.enabled && !p.fetched.IsZero() && time.Since(p.fetched) < m.opts.CacheTTL {
			s.ModelCount = len(p.models)
			s.CatalogAvailable = len(p.models) > 0
			s.InferenceVerified = !p.lastSuccess.IsZero()
			s.Available = s.CatalogAvailable && s.InferenceVerified && s.LastError == ""
		}
		if !p.fetched.IsZero() {
			s.LastRefresh = p.fetched.UTC().Format(time.RFC3339)
		}
		if !p.lastSuccess.IsZero() {
			s.LastSuccess = p.lastSuccess.UTC().Format(time.RFC3339)
		}
		p.mu.Unlock()
		out = append(out, s)
	}
	return out
}

// catalog never uses an expired free roster after refresh failure: a model may
// have become paid since the last listing. loading coalesces concurrent callers.
func (m *Manager) catalog(ctx context.Context, p *provider) ([]Model, error) {
	if !p.enabled {
		return nil, gatewayError(http.StatusServiceUnavailable, "free_gateway_disabled", p.name+" is disabled")
	}
	for {
		p.mu.Lock()
		if !p.fetched.IsZero() && time.Since(p.fetched) < m.opts.CacheTTL {
			models := append([]Model(nil), p.models...)
			p.mu.Unlock()
			return models, nil
		}
		if !p.failed.IsZero() && time.Since(p.failed) < m.opts.FailureBackoff {
			p.mu.Unlock()
			return nil, gatewayError(http.StatusServiceUnavailable, "free_gateway_catalog_unavailable", p.name+" free model catalog is temporarily unavailable")
		}
		if loading := p.loading; loading != nil {
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-loading:
				continue
			}
		}
		p.loading = make(chan struct{})
		p.mu.Unlock()
		models, err := m.fetchCatalog(ctx, p)
		p.mu.Lock()
		if err == nil {
			p.models, p.fetched, p.failed, p.listingError = models, time.Now(), time.Time{}, ""
		} else if ctx.Err() == nil {
			p.failed, p.listingError = time.Now(), "free model catalog is temporarily unavailable"
		}
		close(p.loading)
		p.loading = nil
		p.mu.Unlock()
		return models, err
	}
}

var freeMarker = regexp.MustCompile(`(?:^|[-_])free(?:$|[-_.])`)
var museSpark = regexp.MustCompile(`(?i)^muse[-_]?spark(?:$|[-_:.\s])`)

func openCodeChatFree(id string) bool {
	base := id
	if n := strings.LastIndex(base, "/"); n >= 0 {
		base = base[n+1:]
	}
	// These free models use /responses or /messages, not Chat Completions.
	if base == "union-alpha" || museSpark.MatchString(base) {
		return false
	}
	return base == "big-pickle" || freeMarker.MatchString(base)
}

func (m *Manager) request(ctx context.Context, p *provider, method, path string, body []byte) (*http.Response, error) {
	return m.requestWithHeaders(ctx, p, method, path, body, nil)
}

// The caller supplies provider-owned headers only; inbound client credentials
// never enter this path. Discovery keeps its separate, generic request shape.
func (m *Manager) requestWithHeaders(ctx context.Context, p *provider, method, path string, body []byte, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "workbuddy2api-freegateway")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	if p.id == "opencode" {
		req.Header.Set("Authorization", "Bearer public")
	}
	for key, values := range headers {
		req.Header[key] = append([]string(nil), values...)
	}
	return m.http.Do(req)
}

func (m *Manager) fetchCatalog(ctx context.Context, p *provider) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, m.opts.DiscoveryTimeout)
	defer cancel()
	resp, err := m.request(ctx, p, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, gatewayError(http.StatusServiceUnavailable, "free_gateway_catalog_unavailable", p.name+" catalog request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, gatewayError(http.StatusServiceUnavailable, "free_gateway_catalog_unavailable", fmt.Sprintf("%s catalog returned HTTP %d", p.name, resp.StatusCode))
	}
	const maxListing = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListing+1))
	if err != nil || len(body) > maxListing {
		return nil, fmt.Errorf("invalid model catalog")
	}
	var listing struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			IsFree        bool   `json:"isFree"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil || listing.Data == nil {
		return nil, fmt.Errorf("invalid model catalog")
	}
	models := make([]Model, 0)
	seen := make(map[string]bool)
	for _, row := range listing.Data {
		id := strings.TrimSpace(row.ID)
		if id == "" || seen[id] || strings.ContainsAny(id, "\r\n\t") {
			continue
		}
		if (p.id == "opencode" && !openCodeChatFree(id)) || (p.id == "kilo" && !row.IsFree) {
			continue
		}
		seen[id] = true
		models = append(models, Model{ID: p.id + ":" + id, Object: "model", OwnedBy: p.id, Name: row.Name, Free: true, ContextLength: row.ContextLength})
	}
	return models, nil
}

type Error struct {
	Status        int
	Code, Message string
	RetryAfter    string
}

func gatewayError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func (e *Error) Error() string { return e.Message }

// Chat accepts no inbound headers. OpenCode receives its dedicated request
// adapter; all untouched JSON values stay RawMessage, including large integers.
func (m *Manager) Chat(ctx context.Context, model string, body []byte) (*http.Response, error) {
	id, bare, ok := SplitModel(model)
	if m == nil || !ok {
		return nil, gatewayError(http.StatusNotFound, "model_not_found", "unknown free gateway model")
	}
	var p *provider
	for _, candidate := range m.providers {
		if candidate.id == id {
			p = candidate
			break
		}
	}
	models, err := m.catalog(ctx, p)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, candidate := range models {
		if candidate.ID == model {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, gatewayError(http.StatusNotFound, "model_not_found", "model is not in the current verified free catalog")
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, gatewayError(http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
	}
	payload["model"], _ = json.Marshal(bare)
	var plan *openCodePlan
	var headers http.Header
	if p.id == "opencode" {
		var session string
		plan, session, err = prepareOpenCode(payload)
		if err != nil {
			return nil, err
		}
		headers = openCodeHeaders(session, bytes.Equal(bytes.TrimSpace(payload["stream"]), []byte("true")))
	}
	outbound, err := json.Marshal(payload)
	if err != nil {
		return nil, gatewayError(http.StatusBadRequest, "invalid_request", "invalid JSON request")
	}
	ctx, cancel := context.WithTimeout(ctx, m.opts.ChatTimeout)
	resp, err := m.requestWithHeaders(ctx, p, http.MethodPost, "/chat/completions", outbound, headers)
	if err != nil {
		cancel()
		err = gatewayError(http.StatusBadGateway, "free_gateway_transport", p.name+" request failed or timed out")
		m.noteChat(p, err.Error())
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		resp.Body.Close()
		cancel()
		msg := fmt.Sprintf("%s returned HTTP %d", p.name, resp.StatusCode)
		code := "free_gateway_upstream_error"
		if p.id == "opencode" && bytes.Contains(data, []byte("FreeTierError")) {
			code, msg = "free_tier_restricted", fmt.Sprintf("OpenCode HTTP %d / FreeTierError: upstream free-tier access check rejected this request", resp.StatusCode)
		}
		status := resp.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		failure := gatewayError(status, code, msg)
		if status == http.StatusTooManyRequests {
			failure.RetryAfter = safeRetryAfter(resp.Header.Get("Retry-After"))
		}
		err = failure
		m.noteChat(p, msg)
		return nil, err
	}
	if plan != nil {
		if err := adaptOpenCodeResponse(resp, plan); err != nil {
			resp.Body.Close()
			cancel()
			m.noteChat(p, "upstream response was interrupted or invalid")
			return nil, gatewayError(http.StatusBadGateway, "free_gateway_invalid_response", "OpenCode response was interrupted or invalid")
		}
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func safeRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 80 {
		return ""
	}
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil && seconds <= 7*24*60*60 {
		return strconv.FormatUint(seconds, 10)
	}
	if date, err := http.ParseTime(value); err == nil {
		return date.UTC().Format(http.TimeFormat)
	}
	return ""
}

// NoteSuccess is called only after a complete, valid inference response.
func (m *Manager) NoteSuccess(model string) {
	id, _, ok := SplitModel(model)
	if m == nil || !ok {
		return
	}
	for _, p := range m.providers {
		if p.id == id {
			m.noteChat(p, "")
			return
		}
	}
}

func (m *Manager) noteChat(p *provider, message string) {
	p.mu.Lock()
	p.chatError = message
	if message == "" {
		p.lastSuccess = time.Now()
	}
	p.mu.Unlock()
}

// NoteFailure records a body/read failure after HTTP headers were accepted.
func (m *Manager) NoteFailure(model string) {
	id, _, ok := SplitModel(model)
	if m == nil || !ok {
		return
	}
	for _, p := range m.providers {
		if p.id == id {
			m.noteChat(p, "upstream response was interrupted or invalid")
			return
		}
	}
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { b.cancel(); return b.ReadCloser.Close() }
