package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"openai-proxy/internal/config"
	"openai-proxy/internal/proxy"
)

type Server struct {
	config   config.Config
	routes   map[string]*keyRoute
	pools    []*proxy.Pool
	logger   *slog.Logger
	errors   *errorLogBuffer
	sessions *adminSessionStore
	sequence atomic.Uint64
}

type keyRoute struct {
	pool *proxy.Pool
}

func New(cfg config.Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	groups := make(map[string][]config.KeyConfig, len(cfg.Keys))
	for _, key := range cfg.Keys {
		groups[key.Key] = append(groups[key.Key], key)
	}
	routes := make(map[string]*keyRoute, len(groups)+1)
	pools := make([]*proxy.Pool, 0, len(groups))
	keyID := 1
	for clientKey, keys := range groups {
		pool, err := proxy.NewPool(keys, cfg.Upstream, keyID)
		if err != nil {
			return nil, fmt.Errorf("create client pool for key %s: %w", proxy.MaskSecret(clientKey), err)
		}
		keyID += len(keys)
		routes[clientKey] = &keyRoute{pool: pool}
		pools = append(pools, pool)
	}
	if cfg.ProjectKey != "" && len(cfg.Keys) == 1 {
		routes[cfg.ProjectKey] = routes[cfg.Keys[0].Key]
	}
	return &Server{config: cfg, routes: routes, pools: pools, logger: logger, errors: newErrorLogBuffer(200), sessions: newAdminSessionStore()}, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.ServeHTTP)
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	responseWriter := &statusWriter{ResponseWriter: writer}
	writer = responseWriter
	requestID := s.requestID()
	writer.Header().Set("X-Request-ID", requestID)
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("request panic", "request_id", requestID, "method", request.Method, "path", request.URL.Path, "panic", recovered)
			s.recordError(errorEvent{RequestID: requestID, Method: request.Method, Path: request.URL.Path, Message: "request panic: " + fmt.Sprint(recovered)})
			if !responseWriter.wroteHeaders {
				writeAPIError(writer, http.StatusInternalServerError, "internal_server_error", "Internal server error", nil)
			}
		}
		s.logger.Info("request completed", "request_id", requestID, "method", request.Method, "path", request.URL.Path, "status", responseWriter.status(), "bytes", responseWriter.bytesWritten, "duration", time.Since(started))
	}()

	if s.config.Admin.Enabled && s.isAdminPath(request.URL.Path) {
		s.handleAdmin(writer, request)
		return
	}

	if request.URL.Path == "/healthz" {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if request.URL.Path == "/" {
		writeJSON(writer, http.StatusOK, map[string]string{"service": "openai-proxy", "status": "ok"})
		return
	}
	if !strings.HasPrefix(request.URL.Path, "/v1/") {
		writeAPIError(writer, http.StatusNotFound, "not_found", "only /v1/* endpoints are supported", nil)
		return
	}
	if request.Method == http.MethodConnect || request.Method == http.MethodTrace {
		writeAPIError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "HTTP method is not supported", nil)
		return
	}

	key, ok := extractClientKey(request)
	if !ok {
		writeAPIError(writer, http.StatusUnauthorized, "invalid_api_key", "Missing or invalid API key", nil)
		return
	}
	route, ok := s.routes[key]
	if !ok {
		writeAPIError(writer, http.StatusUnauthorized, "invalid_api_key", "Incorrect API key provided", nil)
		return
	}

	body, err := readBody(request, int64(s.config.Server.MaxBodyBytes))
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body is too large", nil)
			return
		}
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Unable to read request body", nil)
		return
	}

	stream := isStreaming(body)
	var response *http.Response
	if stream {
		response, err = route.pool.ForwardWithoutRetry(request.Context(), request.Method, request.URL.RequestURI(), request.Header, body)
	} else {
		response, err = route.pool.Forward(request.Context(), request.Method, request.URL.RequestURI(), request.Header, body)
	}
	if err != nil {
		s.logger.Error("upstream request failed", "request_id", requestID, "key", maskKey(key), "error", err)
		s.recordError(errorEvent{RequestID: requestID, Method: request.Method, Path: request.URL.Path, Message: err.Error()})
		status := http.StatusBadGateway
		if errors.Is(err, proxy.ErrNoActiveKeys) {
			status = http.StatusServiceUnavailable
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(request.Context().Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeAPIError(writer, status, "upstream_error", "Unable to reach upstream OpenAI service", nil)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		details := responseErrorDetails(response)
		s.recordError(errorEvent{RequestID: requestID, Method: request.Method, Path: request.URL.Path, Status: response.StatusCode, Message: details})
	}
	copyResponseHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	if stream {
		flushResponse(writer, response.Body)
		return
	}
	if _, err := io.Copy(writer, response.Body); err != nil {
		if request.Context().Err() != nil {
			s.logger.Warn("client disconnected while copying response", "request_id", requestID, "error", request.Context().Err())
		} else {
			s.logger.Warn("response copy failed", "request_id", requestID, "error", err)
		}
	}
}

var errBodyTooLarge = errors.New("request body too large")

func readBody(request *http.Request, limit int64) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	if limit <= 0 {
		return io.ReadAll(request.Body)
	}
	limited := io.LimitReader(request.Body, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func extractClientKey(request *http.Request) (string, bool) {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		key := strings.TrimSpace(value[len("Bearer "):])
		return key, key != ""
	}
	key := strings.TrimSpace(request.Header.Get("X-API-Key"))
	if key == "" {
		key = strings.TrimSpace(request.Header.Get("api-key"))
	}
	return key, key != ""
}

func isStreaming(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Stream
}

func copyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
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

func flushResponse(writer http.ResponseWriter, reader io.Reader) {
	flusher, canFlush := writer.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if _, writeErr := writer.Write(buffer[:count]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
	wroteHeaders bool
}

func (w *statusWriter) WriteHeader(statusCode int) {
	if w.wroteHeaders {
		return
	}
	w.statusCode = statusCode
	w.wroteHeaders = true
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if !w.wroteHeaders {
		w.WriteHeader(http.StatusOK)
	}
	count, err := w.ResponseWriter.Write(data)
	w.bytesWritten += int64(count)
	return count, err
}

func (w *statusWriter) Flush() {
	if !w.wroteHeaders {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) status() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

func writeAPIError(writer http.ResponseWriter, status int, errorType, message string, code any) {
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"param":   nil,
			"code":    code,
		},
	}
	writeJSON(writer, status, payload)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (s *Server) requestID() string {
	var randomPart [8]byte
	if _, err := rand.Read(randomPart[:]); err == nil {
		return hex.EncodeToString(randomPart[:])
	}
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), s.sequence.Add(1))
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}
