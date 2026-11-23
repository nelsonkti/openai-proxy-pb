package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openai-proxy/internal/config"
)

func TestNewWritesLogFile(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(directory, "openai-proxy.log")
	logger, closer, err := New(config.LoggingConfig{Path: directory, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("test log", "request_id", "test-request")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "test-request") {
		t.Fatalf("log file does not contain request ID: %s", data)
	}
}
