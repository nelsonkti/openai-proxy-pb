package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"

	"openai-proxy/internal/config"
)

var errRetryLimit = errors.New("upstream retry limit reached")

type Client struct {
	strategy string
	clients  []*endpointClient
	next     atomic.Uint64
}

func NewClient(key config.KeyConfig, settings config.UpstreamConfig) (*Client, error) {
	proxyURLs := append([]string(nil), key.Proxies...)
	if len(proxyURLs) == 0 {
		proxyURLs = []string{key.Proxy}
	}
	if len(proxyURLs) == 0 {
		proxyURLs = []string{""}
	}
	strategy := strings.ToLower(strings.TrimSpace(key.Strategy))
	if strategy == "" {
		strategy = "fixed"
	}
	clients := make([]*endpointClient, 0, len(proxyURLs))
	for _, proxyURL := range proxyURLs {
		client, err := newEndpointClient(key, proxyURL, settings)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	return &Client{strategy: strategy, clients: clients}, nil
}

type endpointClient struct {
	key      config.KeyConfig
	baseURL  *url.URL
	settings config.UpstreamConfig
	http     *http.Client
	proxyURL string
}

func newEndpointClient(key config.KeyConfig, proxyURL string, settings config.UpstreamConfig) (*endpointClient, error) {
	baseURL, err := url.Parse(settings.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	transport, err := newTransport(proxyURL, settings)
	if err != nil {
		return nil, err
	}
	return &endpointClient{
		key:      key,
		baseURL:  baseURL,
		settings: settings,
		proxyURL: proxyURL,
		http: &http.Client{
			Transport: transport,
			Timeout:   settings.RequestTimeout.Duration(),
		},
	}, nil
}

func newTransport(proxyURL string, settings config.UpstreamConfig) (*http.Transport, error) {
	forceHTTP2 := proxyURL == ""
	dialer := &net.Dialer{
		Timeout:   settings.ConnectTimeout.Duration(),
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     forceHTTP2,
		MaxIdleConns:          settings.MaxIdleConns,
		MaxIdleConnsPerHost:   settings.MaxIdleConnsPerHost,
		IdleConnTimeout:       settings.IdleConnTimeout.Duration(),
		TLSHandshakeTimeout:   settings.TLSHandshakeTimeout.Duration(),
		ExpectContinueTimeout: settings.ExpectContinueTimeout.Duration(),
		ResponseHeaderTimeout: settings.ResponseHeaderTimeout.Duration(),
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if proxyURL == "" {
		return transport, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
	case "socks5", "socks5h", "socket5h":
		auth := proxy.Auth{}
		var authPtr *proxy.Auth
		if parsed.User != nil {
			auth.User = parsed.User.Username()
			auth.Password, _ = parsed.User.Password()
			authPtr = &auth
		}
		socksDialer, err := proxy.SOCKS5("tcp", parsed.Host, authPtr, dialer)
		if err != nil {
			return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
		}
		contextDialer, ok := socksDialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("SOCKS5 dialer does not support context cancellation")
		}
		if strings.EqualFold(parsed.Scheme, "socks5") {
			contextDialer = localResolveDialer{dialer: contextDialer}
		}
		transport.Proxy = nil
		transport.DialContext = contextDialer.DialContext
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
	return transport, nil
}

type localResolveDialer struct {
	dialer proxy.ContextDialer
}

func (d localResolveDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return d.dialer.DialContext(ctx, network, address)
	}
	addresses, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, resolvedHost := range addresses {
		connection, dialErr := d.dialer.DialContext(ctx, network, net.JoinHostPort(resolvedHost, port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

func (c *Client) Forward(ctx context.Context, method, requestPath string, requestHeaders http.Header, body []byte) (*http.Response, error) {
	return c.forwardWithSelection(ctx, method, requestPath, requestHeaders, body, true)
}

func (c *Client) ForwardWithoutRetry(ctx context.Context, method, requestPath string, requestHeaders http.Header, body []byte) (*http.Response, error) {
	return c.forwardWithSelection(ctx, method, requestPath, requestHeaders, body, false)
}

func (c *Client) forwardWithSelection(ctx context.Context, method, requestPath string, requestHeaders http.Header, body []byte, allowRetry bool) (*http.Response, error) {
	if len(c.clients) == 0 {
		return nil, errors.New("no upstream clients configured")
	}
	if c.strategy == "round_robin" {
		index := int(c.next.Add(1)-1) % len(c.clients)
		return c.clients[index].forward(ctx, method, requestPath, requestHeaders, body, allowRetry)
	}
	if c.strategy != "failover" || len(c.clients) == 1 {
		return c.clients[0].forward(ctx, method, requestPath, requestHeaders, body, allowRetry)
	}
	canFailover := allowRetry && (c.clients[0].settings.RetryNonIdempotent || method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions || requestHeaders.Get("Idempotency-Key") != "")
	for index, endpoint := range c.clients {
		response, err := endpoint.forward(ctx, method, requestPath, requestHeaders, body, allowRetry)
		if !canFailover || !endpoint.shouldFailover(response, err, ctx) || index+1 >= len(c.clients) {
			return response, err
		}
		if response != nil {
			response.Body.Close()
		}
	}
	return nil, errors.New("upstream failover exhausted")
}

func (c *endpointClient) shouldFailover(response *http.Response, err error, ctx context.Context) bool {
	if err != nil {
		return isRetryableNetworkError(err, ctx)
	}
	if response == nil {
		return true
	}
	return c.isRetryableStatus(response.StatusCode)
}

func (c *endpointClient) forward(ctx context.Context, method, requestPath string, requestHeaders http.Header, body []byte, allowRetry bool) (*http.Response, error) {
	var cancel context.CancelFunc
	if c.settings.RequestTimeout.Duration() > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.settings.RequestTimeout.Duration())
	}
	finish := func() {
		if cancel != nil {
			cancel()
			cancel = nil
		}
	}
	target := *c.baseURL
	parsedPath, err := url.Parse(requestPath)
	if err != nil {
		finish()
		return nil, fmt.Errorf("parse request path: %w", err)
	}
	basePath := strings.TrimRight(c.baseURL.Path, "/")
	requestPathValue := parsedPath.Path
	if strings.HasSuffix(basePath, "/v1") && (requestPathValue == "/v1" || strings.HasPrefix(requestPathValue, "/v1/")) {
		requestPathValue = strings.TrimPrefix(requestPathValue, "/v1")
	}
	target.Path = basePath + "/" + strings.TrimLeft(requestPathValue, "/")
	target.RawQuery = parsedPath.RawQuery
	canRetry := allowRetry && (c.settings.RetryNonIdempotent || method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions || requestHeaders.Get("Idempotency-Key") != "")
	maxAttempts := 1
	if allowRetry && c.settings.MaxRetries > 0 {
		maxAttempts += c.settings.MaxRetries
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var requestStarted atomic.Bool
		attemptContext := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteHeaders: func() {
				requestStarted.Store(true)
			},
		})
		request, err := c.newRequest(attemptContext, method, target.String(), requestHeaders, body)
		if err != nil {
			finish()
			return nil, err
		}
		response, err := c.http.Do(request)
		if err != nil {
			wrappedErr := fmt.Errorf("upstream request via %s: %w", proxyDisplay(c.proxyURL), err)
			canRetryNetwork := canRetry || !requestStarted.Load()
			if attempt+1 >= maxAttempts || !canRetryNetwork || !isRetryableNetworkError(wrappedErr, ctx) {
				finish()
				return nil, wrappedErr
			}
			if err := waitBackoff(ctx, c.settings, attempt, nil); err != nil {
				finish()
				return nil, err
			}
			continue
		}
		if attempt+1 >= maxAttempts || !canRetry || !c.isRetryableStatus(response.StatusCode) {
			if cancel != nil {
				response.Body = &cancelOnCloseBody{ReadCloser: response.Body, cancel: finish}
			} else {
				finish()
			}
			return response, nil
		}
		_, readErr := io.CopyN(io.Discard, response.Body, 1<<20)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			response.Body.Close()
			finish()
			return nil, readErr
		}
		response.Body.Close()
		if err := waitBackoff(ctx, c.settings, attempt, response); err != nil {
			finish()
			return nil, err
		}
	}
	finish()
	return nil, errRetryLimit
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func proxyDisplay(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "direct connection"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "configured proxy"
	}
	parsed.User = nil
	return parsed.String()
}

func (c *endpointClient) newRequest(ctx context.Context, method, target string, sourceHeaders http.Header, body []byte) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyForwardHeaders(request.Header, sourceHeaders)
	request.Header.Set("Authorization", "Bearer "+c.key.UpstreamKey)
	request.Header.Set("User-Agent", c.settings.UserAgent)
	request.Header.Set("Accept-Encoding", "identity")
	return request, nil
}

func copyForwardHeaders(destination, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "X-API-Key") || strings.EqualFold(key, "api-key") || strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Accept-Encoding") {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func isHopByHopHeader(header string) bool {
	switch strings.ToLower(header) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func (c *endpointClient) isRetryableStatus(status int) bool {
	for _, allowed := range c.settings.RetryStatusCodes {
		if status == allowed {
			return true
		}
	}
	return false
}

func isRetryableNetworkError(err error, ctx context.Context) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ctx.Err() == nil
	}
	var netError net.Error
	if errors.As(err, &netError) {
		if netError.Timeout() {
			return ctx.Err() == nil
		}
		return true
	}
	return true
}

func waitBackoff(ctx context.Context, settings config.UpstreamConfig, attempt int, response *http.Response) error {
	delay := settings.RetryInitialBackoff.Duration()
	for index := 0; index < attempt; index++ {
		delay *= 2
		if delay >= settings.RetryMaxBackoff.Duration() {
			delay = settings.RetryMaxBackoff.Duration()
			break
		}
	}
	if response != nil {
		if retryAfter := parseRetryAfter(response.Header.Get("Retry-After")); retryAfter > delay {
			delay = retryAfter
		}
	}
	if delay > settings.RetryMaxBackoff.Duration() {
		delay = settings.RetryMaxBackoff.Duration()
	}
	if delay > 0 {
		jitter := time.Duration(rand.Float64() * float64(delay/4))
		delay += jitter
		if delay > settings.RetryMaxBackoff.Duration() {
			delay = settings.RetryMaxBackoff.Duration()
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}
