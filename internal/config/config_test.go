package config

import (
	"strings"
	"testing"
	"time"
)

func TestSetDefaultsAndValidate(t *testing.T) {
	cfg := Config{Keys: []KeyConfig{{Key: "client-key", Proxy: "socks5h://127.0.0.1:1080"}}}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("SetDefaultsAndValidate() error = %v", err)
	}
	if cfg.Upstream.BaseURL != "https://api.openai.com" {
		t.Fatalf("default upstream URL = %q", cfg.Upstream.BaseURL)
	}
	if cfg.Keys[0].UpstreamKey != "client-key" {
		t.Fatalf("default upstream key = %q", cfg.Keys[0].UpstreamKey)
	}
	if cfg.Server.MaxBodyBytes != Size(64*1024*1024) {
		t.Fatalf("default max body bytes = %d", cfg.Server.MaxBodyBytes)
	}
	if cfg.Logging.Path != "./logs" || cfg.Logging.MaxFiles != 7 {
		t.Fatalf("logging defaults = %+v", cfg.Logging)
	}
	if cfg.Keys[0].Strategy != "fixed" {
		t.Fatalf("default key strategy = %q", cfg.Keys[0].Strategy)
	}
	if cfg.Server.WriteTimeout.Duration() != 30*time.Minute {
		t.Fatalf("default server write timeout = %s", cfg.Server.WriteTimeout.Duration())
	}
	if cfg.Upstream.RequestTimeout.Duration() != 30*time.Minute {
		t.Fatalf("default upstream request timeout = %s", cfg.Upstream.RequestTimeout.Duration())
	}
	if cfg.Upstream.ResponseHeaderTimeout.Duration() != 30*time.Minute {
		t.Fatalf("default upstream response header timeout = %s", cfg.Upstream.ResponseHeaderTimeout.Duration())
	}
}

func TestSetDefaultsAndValidateProxyStrategies(t *testing.T) {
	cfg := Config{Keys: []KeyConfig{
		{Key: "direct"},
		{Key: "round-robin", Strategy: "ROUND_ROBIN", Proxies: []string{"http://127.0.0.1:7890", ""}},
		{Key: "failover", Strategy: "failover", Proxies: []string{"socks5h://127.0.0.1:1080"}},
	}}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Keys[0].Strategy != "fixed" || cfg.Keys[1].Strategy != "round_robin" {
		t.Fatalf("normalized strategies = %+v", cfg.Keys)
	}
}

func TestSetDefaultsAndValidateProjectKey(t *testing.T) {
	cfg := Config{
		ProjectKey: "project-demo",
		Keys:       []KeyConfig{{UpstreamKey: "sk-upstream"}},
	}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Keys[0].Key != "project-demo" {
		t.Fatalf("normalized key = %q", cfg.Keys[0].Key)
	}
}

func TestSetDefaultsAndValidateRejectsAmbiguousProxyConfig(t *testing.T) {
	cfg := Config{Keys: []KeyConfig{{Key: "client", Proxy: "http://127.0.0.1:7890", Proxies: []string{"http://127.0.0.1:7891"}}}}
	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Fatal("expected proxy and proxies validation error")
	}
}

func TestValidateProxyURL(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:7890",
		"https://user:password@example.com:8443",
		"socks5://127.0.0.1:1080",
		"socks5h://user:password@example.com:1080",
	}
	for _, value := range valid {
		if err := ValidateProxyURL(value); err != nil {
			t.Errorf("ValidateProxyURL(%q) error = %v", value, err)
		}
	}
	for _, value := range []string{"127.0.0.1:1080", "ftp://127.0.0.1:21"} {
		if err := ValidateProxyURL(value); err == nil {
			t.Errorf("ValidateProxyURL(%q) error = nil", value)
		}
	}
}

func TestDurationAndSizeYAML(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{ReadTimeout: Duration(2 * time.Second), MaxBodyBytes: Size(1024)},
		Keys:   []KeyConfig{{Key: "client-key"}},
	}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg.Upstream.BaseURL, " ") {
		t.Fatalf("unexpected whitespace in upstream URL")
	}
}
