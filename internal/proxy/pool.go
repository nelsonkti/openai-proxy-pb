package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openai-proxy/internal/config"
)

var ErrNoActiveKeys = errors.New("no active upstream keys")

type Pool struct {
	entries []*poolEntry
	next    atomic.Uint64
}

type poolEntry struct {
	id         int
	key        config.KeyConfig
	client     *Client
	mu         sync.RWMutex
	disabled   bool
	disabledAt time.Time
	reason     string
}

type KeyStatus struct {
	ID           int       `json:"id"`
	ClientKey    string    `json:"client_key"`
	UpstreamKey  string    `json:"upstream_key"`
	Enabled      bool      `json:"enabled"`
	DisabledAt   time.Time `json:"disabled_at,omitempty"`
	DisableCause string    `json:"disable_cause,omitempty"`
}

func NewPool(keys []config.KeyConfig, settings config.UpstreamConfig, idOffset int) (*Pool, error) {
	if len(keys) == 0 {
		return nil, ErrNoActiveKeys
	}
	entries := make([]*poolEntry, 0, len(keys))
	for index, key := range keys {
		client, err := NewClient(key, settings)
		if err != nil {
			return nil, err
		}
		entries = append(entries, &poolEntry{id: idOffset + index, key: key, client: client})
	}
	return &Pool{entries: entries}, nil
}

func (p *Pool) Forward(ctx context.Context, method, requestPath string, headers http.Header, body []byte) (*http.Response, error) {
	return p.forward(ctx, method, requestPath, headers, body, true)
}

func (p *Pool) ForwardWithoutRetry(ctx context.Context, method, requestPath string, headers http.Header, body []byte) (*http.Response, error) {
	return p.forward(ctx, method, requestPath, headers, body, false)
}

func (p *Pool) Test(ctx context.Context, id int, model string) (*http.Response, bool, error) {
	model = strings.TrimSpace(model)
	if model == "" || strings.ContainsAny(model, "/?#") {
		return nil, false, fmt.Errorf("model must be a non-empty path segment")
	}
	for _, entry := range p.entries {
		if entry.id != id {
			continue
		}
		requestPath := "/v1/models/" + url.PathEscape(model)
		response, err := entry.client.ForwardWithoutRetry(ctx, http.MethodGet, requestPath, nil, nil)
		if err == nil && IsQuotaExhausted(response) {
			p.Disable(entry.id, quotaReason(response))
		}
		return response, true, err
	}
	return nil, false, nil
}

func (p *Pool) forward(ctx context.Context, method, requestPath string, headers http.Header, body []byte, allowRetry bool) (*http.Response, error) {
	entries := p.activeEntries()
	if len(entries) == 0 {
		return nil, ErrNoActiveKeys
	}
	start := int(p.next.Add(1)-1) % len(entries)
	for offset := range entries {
		entry := entries[(start+offset)%len(entries)]
		var response *http.Response
		var err error
		if allowRetry {
			response, err = entry.client.Forward(ctx, method, requestPath, headers, body)
		} else {
			response, err = entry.client.ForwardWithoutRetry(ctx, method, requestPath, headers, body)
		}
		if err != nil {
			return nil, err
		}
		if !IsQuotaExhausted(response) {
			return response, nil
		}
		p.Disable(entry.id, quotaReason(response))
		if offset+1 == len(entries) {
			return response, nil
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
	}
	return nil, ErrNoActiveKeys
}

func (p *Pool) activeEntries() []*poolEntry {
	active := make([]*poolEntry, 0, len(p.entries))
	for _, entry := range p.entries {
		entry.mu.RLock()
		disabled := entry.disabled
		entry.mu.RUnlock()
		if !disabled {
			active = append(active, entry)
		}
	}
	return active
}

func (p *Pool) Disable(id int, reason string) bool {
	for _, entry := range p.entries {
		if entry.id != id {
			continue
		}
		entry.mu.Lock()
		defer entry.mu.Unlock()
		if entry.disabled {
			return false
		}
		entry.disabled = true
		entry.disabledAt = time.Now()
		entry.reason = reason
		return true
	}
	return false
}

func (p *Pool) Enable(id int) bool {
	for _, entry := range p.entries {
		if entry.id != id {
			continue
		}
		entry.mu.Lock()
		defer entry.mu.Unlock()
		if !entry.disabled {
			return false
		}
		entry.disabled = false
		entry.disabledAt = time.Time{}
		entry.reason = ""
		return true
	}
	return false
}

func (p *Pool) Statuses() []KeyStatus {
	statuses := make([]KeyStatus, 0, len(p.entries))
	for _, entry := range p.entries {
		entry.mu.RLock()
		status := KeyStatus{
			ID:           entry.id,
			ClientKey:    MaskSecret(entry.key.Key),
			UpstreamKey:  MaskSecret(entry.key.UpstreamKey),
			Enabled:      !entry.disabled,
			DisabledAt:   entry.disabledAt,
			DisableCause: entry.reason,
		}
		entry.mu.RUnlock()
		statuses = append(statuses, status)
	}
	return statuses
}

func MaskSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func IsQuotaExhausted(response *http.Response) bool {
	if response == nil || (response.StatusCode != http.StatusPaymentRequired && response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusForbidden) {
		return false
	}
	body, err := readAndRestore(response)
	if err != nil {
		return false
	}
	value := strings.ToLower(string(body))
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		value = strings.ToLower(strings.Join([]string{payload.Error.Code, payload.Error.Type, payload.Error.Message}, " "))
	}
	for _, marker := range []string{"insufficient_quota", "billing_hard_limit", "quota exceeded", "quota has been exceeded", "exceeded your current quota", "monthly limit", "credits exhausted", "credit balance"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func quotaReason(response *http.Response) string {
	if response == nil {
		return "quota exhausted"
	}
	body, _ := readAndRestore(response)
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if payload.Error.Code != "" {
			return payload.Error.Code
		}
	}
	return fmt.Sprintf("upstream status %d", response.StatusCode)
}

func readAndRestore(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
