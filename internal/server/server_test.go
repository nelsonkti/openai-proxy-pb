package server

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openai-proxy/internal/config"
)

func newTestServer(t *testing.T, upstream http.Handler) *Server {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	cfg := config.Config{
		Upstream: config.UpstreamConfig{
			BaseURL:               upstreamServer.URL,
			ConnectTimeout:        config.Duration(time.Second),
			RequestTimeout:        config.Duration(5 * time.Second),
			ResponseHeaderTimeout: config.Duration(time.Second),
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       config.Duration(time.Second),
			TLSHandshakeTimeout:   config.Duration(time.Second),
			ExpectContinueTimeout: config.Duration(time.Second),
			MaxRetries:            1,
			RetryInitialBackoff:   config.Duration(time.Millisecond),
			RetryMaxBackoff:       config.Duration(time.Millisecond),
			RetryStatusCodes:      []int{503},
			UserAgent:             "test-client",
		},
		Keys: []config.KeyConfig{{Key: "client", UpstreamKey: "upstream"}},
	}
	return newTestServerWithConfig(t, upstreamServer, cfg)
}

func newTestServerWithConfig(t *testing.T, upstreamServer *httptest.Server, cfg config.Config) *Server {
	t.Helper()
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	service, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestServeHTTPForwardsBodyAndAuth(t *testing.T) {
	service := newTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer upstream" {
			t.Errorf("authorization = %q", got)
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"model":"test","messages":[]}` {
			t.Errorf("body = %s", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"chatcmpl-test"}`))
	}))

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[]}`))
	request.Header.Set("Authorization", "Bearer client")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

func TestServeHTTPStreamsResponse(t *testing.T) {
	service := newTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher := writer.(http.Flusher)
		_, _ = writer.Write([]byte("data: first\n\n"))
		flusher.Flush()
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	request.Header.Set("Authorization", "Bearer client")
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("stream body = %q", recorder.Body.String())
	}
	reader := bufio.NewReader(strings.NewReader(recorder.Body.String()))
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first stream line = %q, err = %v", line, err)
	}
}

func TestServeHTTPRejectsUnknownKey(t *testing.T) {
	service := newTestServer(t, http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer wrong")
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestServeHTTPUsesProjectKeyOutsideKeys(t *testing.T) {
	var calls int
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamServer.Close()
	service := newTestServerWithConfig(t, upstreamServer, config.Config{
		Upstream:   config.UpstreamConfig{BaseURL: upstreamServer.URL},
		ProjectKey: "project-demo",
		Keys:       []config.KeyConfig{{UpstreamKey: "upstream"}},
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer project-demo")
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status = %d, calls = %d", recorder.Code, calls)
	}
}
