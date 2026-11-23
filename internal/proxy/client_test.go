package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openai-proxy/internal/config"
)

func testUpstreamConfig(baseURL string) config.UpstreamConfig {
	return config.UpstreamConfig{
		BaseURL:               baseURL,
		ConnectTimeout:        config.Duration(time.Second),
		RequestTimeout:        config.Duration(5 * time.Second),
		ResponseHeaderTimeout: config.Duration(time.Second),
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       config.Duration(time.Second),
		TLSHandshakeTimeout:   config.Duration(time.Second),
		ExpectContinueTimeout: config.Duration(time.Second),
		MaxRetries:            2,
		RetryInitialBackoff:   config.Duration(time.Millisecond),
		RetryMaxBackoff:       config.Duration(10 * time.Millisecond),
		RetryStatusCodes:      []int{503},
		UserAgent:             "test-client",
	}
}

func TestForwardRetriesGET(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte("try again"))
			return
		}
		writer.Header().Set("X-Upstream-Key", request.Header.Get("Authorization"))
		_, _ = writer.Write([]byte("ok"))
	}))
	defer upstream.Close()

	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream"}, testUpstreamConfig(upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Forward(context.Background(), http.MethodGet, "/v1/models", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	if got := response.Header.Get("X-Upstream-Key"); got != "Bearer upstream" {
		t.Fatalf("upstream authorization = %q", got)
	}
}

func TestForwardDoesNotRetryPOSTByDefault(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream"}, testUpstreamConfig(upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Forward(context.Background(), http.MethodPost, "/v1/chat/completions", nil, []byte(`{"model":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestForwardRetriesPOSTWhenConnectionFailsBeforeRequestWrite(t *testing.T) {
	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream"}, testUpstreamConfig("http://upstream.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client.clients[0].http.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("connection reset by peer")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    request,
		}, nil
	})

	response, err := client.Forward(context.Background(), http.MethodPost, "/v1/chat/completions", nil, []byte(`{"model":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("status = %d, calls = %d", response.StatusCode, calls.Load())
	}
}

func TestForwardDoesNotRetryPOSTAfterRequestHeadersWereWritten(t *testing.T) {
	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream"}, testUpstreamConfig("http://upstream.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client.clients[0].http.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
		return nil, errors.New("connection reset by peer")
	})

	_, err = client.Forward(context.Background(), http.MethodPost, "/v1/chat/completions", nil, []byte(`{"model":"test"}`))
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestForwardRetriesPOSTWithIdempotencyKey(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Idempotency-Key") != "request-1" {
			t.Errorf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
		}
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = writer.Write([]byte("ok"))
	}))
	defer upstream.Close()

	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream"}, testUpstreamConfig(upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", "request-1")
	response, err := client.Forward(context.Background(), http.MethodPost, "/v1/images/generations", headers, []byte(`{"prompt":"cat"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("status = %d, calls = %d", response.StatusCode, calls.Load())
	}
}

func TestForwardAvoidsDuplicateV1Path(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Errorf("path = %q", request.URL.Path)
		}
		_, _ = writer.Write([]byte("ok"))
	}))
	defer upstream.Close()

	settings := testUpstreamConfig(upstream.URL + "/v1")
	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Forward(context.Background(), http.MethodGet, "/v1/models", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestForwardUsesHTTPProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		proxyCalls.Add(1)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("proxy response"))
	}))
	defer proxyServer.Close()

	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream", Proxy: proxyServer.URL}, testUpstreamConfig("http://upstream.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Forward(context.Background(), http.MethodGet, "/v1/models", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if proxyCalls.Load() != 1 {
		t.Fatalf("proxy calls = %d, want 1", proxyCalls.Load())
	}
}

func TestForwardUsesDirectConnectionWhenProxyIsEmpty(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		_, _ = writer.Write([]byte("direct response"))
	}))
	defer upstream.Close()

	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream", Proxy: ""}, testUpstreamConfig(upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Forward(context.Background(), http.MethodGet, "/v1/models", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || upstreamCalls.Load() != 1 {
		t.Fatalf("status = %d, upstream calls = %d", response.StatusCode, upstreamCalls.Load())
	}
}

func TestForwardKeepsRequestContextUntilResponseBodyCloses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("response body"))
	}))
	defer upstream.Close()

	settings := testUpstreamConfig(upstream.URL)
	settings.RequestTimeout = config.Duration(time.Second)
	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Forward(context.Background(), http.MethodGet, "/v1/models", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "response body" {
		t.Fatalf("response body = %q", body)
	}
}

func TestForwardDoesNotPassAcceptEncodingToUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Accept-Encoding"); got != "identity" {
			t.Fatalf("upstream Accept-Encoding = %q, want identity", got)
		}
		_, _ = writer.Write([]byte("ok"))
	}))
	defer upstream.Close()

	client, err := NewClient(config.KeyConfig{Key: "client", UpstreamKey: "upstream"}, testUpstreamConfig(upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	header := make(http.Header)
	header.Set("Accept-Encoding", "gzip, deflate, br")
	response, err := client.Forward(context.Background(), http.MethodGet, "/v1/models", header, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
}

func TestForwardRoundRobinUsesEachProxy(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	firstProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		firstCalls.Add(1)
		_, _ = writer.Write([]byte("first"))
	}))
	defer firstProxy.Close()
	secondProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secondCalls.Add(1)
		_, _ = writer.Write([]byte("second"))
	}))
	defer secondProxy.Close()

	client, err := NewClient(config.KeyConfig{
		Key:      "client",
		Strategy: "round_robin",
		Proxies:  []string{firstProxy.URL, secondProxy.URL},
	}, testUpstreamConfig("http://upstream.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		response, forwardErr := client.Forward(context.Background(), http.MethodGet, "/v1/models", nil, nil)
		if forwardErr != nil {
			t.Fatal(forwardErr)
		}
		response.Body.Close()
	}
	if firstCalls.Load() != 2 || secondCalls.Load() != 2 {
		t.Fatalf("proxy calls = (%d, %d), want (2, 2)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestForwardFailoverUsesNextProxyAfterRetryableFailure(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	firstProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		firstCalls.Add(1)
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer firstProxy.Close()
	secondProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secondCalls.Add(1)
		_, _ = writer.Write([]byte("failover"))
	}))
	defer secondProxy.Close()

	settings := testUpstreamConfig("http://upstream.invalid")
	settings.MaxRetries = 0
	settings.RetryStatusCodes = []int{http.StatusBadGateway}
	client, err := NewClient(config.KeyConfig{
		Key:      "client",
		Strategy: "failover",
		Proxies:  []string{firstProxy.URL, secondProxy.URL},
	}, settings)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Forward(context.Background(), http.MethodGet, "/v1/models", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("status = %d, proxy calls = (%d, %d)", response.StatusCode, firstCalls.Load(), secondCalls.Load())
	}
}

func TestForwardFailoverDoesNotRepeatNonIdempotentPost(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	firstProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		firstCalls.Add(1)
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer firstProxy.Close()
	secondProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secondCalls.Add(1)
		_, _ = writer.Write([]byte("should not be called"))
	}))
	defer secondProxy.Close()

	settings := testUpstreamConfig("http://upstream.invalid")
	settings.MaxRetries = 0
	settings.RetryStatusCodes = []int{http.StatusBadGateway}
	client, err := NewClient(config.KeyConfig{
		Key:      "client",
		Strategy: "failover",
		Proxies:  []string{firstProxy.URL, secondProxy.URL},
	}, settings)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Forward(context.Background(), http.MethodPost, "/v1/images/generations", nil, []byte(`{"prompt":"cat"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("status = %d, proxy calls = (%d, %d)", response.StatusCode, firstCalls.Load(), secondCalls.Load())
	}
}
