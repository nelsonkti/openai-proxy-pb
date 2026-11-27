package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIsQuotaExhaustedRestoresBody(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`)),
	}
	if !IsQuotaExhausted(response) {
		t.Fatal("expected quota response")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "insufficient_quota") {
		t.Fatalf("restored body = %q", body)
	}
}

func TestIsQuotaExhaustedDoesNotDisableRateLimit(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`)),
	}
	if IsQuotaExhausted(response) {
		t.Fatal("temporary rate limit must not be treated as exhausted quota")
	}
}

func TestMaskSecret(t *testing.T) {
	if got := MaskSecret("sk-1234567890"); got != "sk-1...7890" {
		t.Fatalf("masked secret = %q", got)
	}
}
