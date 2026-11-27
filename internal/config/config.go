package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	if node.Tag == "!!int" {
		value, err := strconv.ParseInt(node.Value, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", node.Value, err)
		}
		*d = Duration(time.Duration(value))
		return nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(node.Value))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(value)
	return nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

type Size int64

func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("size must be a scalar")
	}
	value := strings.TrimSpace(strings.ToUpper(node.Value))
	if node.Tag == "!!int" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid size %q: %w", node.Value, err)
		}
		*s = Size(parsed)
		return nil
	}
	units := []struct {
		suffix string
		factor int64
	}{
		{"GIB", 1024 * 1024 * 1024},
		{"GB", 1000 * 1000 * 1000},
		{"MIB", 1024 * 1024},
		{"MB", 1000 * 1000},
		{"KIB", 1024},
		{"KB", 1000},
		{"B", 1},
	}
	for _, unit := range units {
		if !strings.HasSuffix(value, unit.suffix) {
			continue
		}
		number := strings.TrimSpace(strings.TrimSuffix(value, unit.suffix))
		parsed, err := strconv.ParseFloat(number, 64)
		if err != nil || parsed < 0 {
			return fmt.Errorf("invalid size %q", node.Value)
		}
		*s = Size(parsed * float64(unit.factor))
		return nil
	}
	return fmt.Errorf("invalid size %q", node.Value)
}

type Config struct {
	ProjectKey string         `yaml:"project_key"`
	Server     ServerConfig   `yaml:"server"`
	Admin      AdminConfig    `yaml:"admin"`
	Logging    LoggingConfig  `yaml:"logging"`
	Upstream   UpstreamConfig `yaml:"upstream"`
	Keys       []KeyConfig    `yaml:"keys"`
}

type ServerConfig struct {
	Host         string   `yaml:"host"`
	Port         int      `yaml:"port"`
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
	IdleTimeout  Duration `yaml:"idle_timeout"`
	MaxBodyBytes Size     `yaml:"max_body_bytes"`
}

type AdminConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
	APIKey  string `yaml:"api_key"`
}

type LoggingConfig struct {
	Path     string `yaml:"path"`
	MaxFiles int    `yaml:"max_files"`
}

type UpstreamConfig struct {
	BaseURL               string   `yaml:"base_url"`
	ConnectTimeout        Duration `yaml:"connect_timeout"`
	RequestTimeout        Duration `yaml:"request_timeout"`
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout"`
	MaxIdleConns          int      `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int      `yaml:"max_idle_conns_per_host"`
	IdleConnTimeout       Duration `yaml:"idle_conn_timeout"`
	TLSHandshakeTimeout   Duration `yaml:"tls_handshake_timeout"`
	ExpectContinueTimeout Duration `yaml:"expect_continue_timeout"`
	MaxRetries            int      `yaml:"max_retries"`
	RetryInitialBackoff   Duration `yaml:"retry_initial_backoff"`
	RetryMaxBackoff       Duration `yaml:"retry_max_backoff"`
	RetryStatusCodes      []int    `yaml:"retry_status_codes"`
	RetryNonIdempotent    bool     `yaml:"retry_non_idempotent"`
	UserAgent             string   `yaml:"user_agent"`
}

type KeyConfig struct {
	Key         string   `yaml:"key"`
	UpstreamKey string   `yaml:"upstream_key"`
	Proxy       string   `yaml:"proxy"`
	Proxies     []string `yaml:"proxies"`
	Strategy    string   `yaml:"strategy"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) SetDefaultsAndValidate() error {
	c.ProjectKey = strings.TrimSpace(c.ProjectKey)
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535")
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = Duration(30 * time.Second)
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = Duration(30 * time.Minute)
	}
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = Duration(90 * time.Second)
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = Size(64 * 1024 * 1024)
	}
	if c.Server.MaxBodyBytes < 0 {
		return fmt.Errorf("server.max_body_bytes cannot be negative")
	}
	if c.Admin.Path == "" {
		c.Admin.Path = "/admin"
	}
	if !strings.HasPrefix(c.Admin.Path, "/") {
		return fmt.Errorf("admin.path must start with /")
	}
	c.Admin.Path = strings.TrimRight(c.Admin.Path, "/")
	if c.Admin.Path == "" {
		c.Admin.Path = "/admin"
	}
	c.Admin.APIKey = strings.TrimSpace(c.Admin.APIKey)
	if c.Admin.Enabled && c.Admin.APIKey == "" {
		return fmt.Errorf("admin.api_key is required when admin.enabled is true")
	}
	if c.Logging.Path == "" {
		c.Logging.Path = "./logs"
	}
	if c.Logging.MaxFiles == 0 {
		c.Logging.MaxFiles = 7
	}
	if c.Logging.MaxFiles < 1 {
		return fmt.Errorf("logging.max_files must be at least 1")
	}

	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = "https://api.openai.com"
	}
	baseURL, err := url.Parse(c.Upstream.BaseURL)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return fmt.Errorf("upstream.base_url must be an absolute http/https URL")
	}
	if baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return fmt.Errorf("upstream.base_url cannot contain query or fragment")
	}
	c.Upstream.BaseURL = strings.TrimRight(c.Upstream.BaseURL, "/")
	if c.Upstream.ConnectTimeout == 0 {
		c.Upstream.ConnectTimeout = Duration(15 * time.Second)
	}
	if c.Upstream.RequestTimeout == 0 {
		c.Upstream.RequestTimeout = Duration(30 * time.Minute)
	}
	if c.Upstream.ResponseHeaderTimeout == 0 {
		c.Upstream.ResponseHeaderTimeout = Duration(30 * time.Minute)
	}
	if c.Upstream.MaxIdleConns == 0 {
		c.Upstream.MaxIdleConns = 100
	}
	if c.Upstream.MaxIdleConnsPerHost == 0 {
		c.Upstream.MaxIdleConnsPerHost = 20
	}
	if c.Upstream.IdleConnTimeout == 0 {
		c.Upstream.IdleConnTimeout = Duration(90 * time.Second)
	}
	if c.Upstream.TLSHandshakeTimeout == 0 {
		c.Upstream.TLSHandshakeTimeout = Duration(15 * time.Second)
	}
	if c.Upstream.ExpectContinueTimeout == 0 {
		c.Upstream.ExpectContinueTimeout = Duration(time.Second)
	}
	if c.Upstream.MaxRetries < 0 {
		return fmt.Errorf("upstream.max_retries cannot be negative")
	}
	if c.Upstream.MaxRetries == 0 {
		c.Upstream.MaxRetries = 2
	}
	if c.Upstream.RetryInitialBackoff == 0 {
		c.Upstream.RetryInitialBackoff = Duration(300 * time.Millisecond)
	}
	if c.Upstream.RetryMaxBackoff == 0 {
		c.Upstream.RetryMaxBackoff = Duration(3 * time.Second)
	}
	if c.Upstream.RetryMaxBackoff < c.Upstream.RetryInitialBackoff {
		return fmt.Errorf("upstream.retry_max_backoff must be >= retry_initial_backoff")
	}
	if len(c.Upstream.RetryStatusCodes) == 0 {
		c.Upstream.RetryStatusCodes = []int{408, 425, 429, 500, 502, 503, 504}
	}
	if c.Upstream.UserAgent == "" {
		c.Upstream.UserAgent = "openai-proxy/1.0"
	}
	if len(c.Keys) == 0 {
		return fmt.Errorf("keys must contain at least one configured key")
	}
	seenExplicit := make(map[string]struct{}, len(c.Keys))
	for index := range c.Keys {
		key := &c.Keys[index]
		keyWasEmpty := strings.TrimSpace(key.Key) == ""
		key.Key = strings.TrimSpace(key.Key)
		key.UpstreamKey = strings.TrimSpace(key.UpstreamKey)
		key.Proxy = strings.TrimSpace(key.Proxy)
		key.Strategy = strings.ToLower(strings.TrimSpace(key.Strategy))
		if key.Key == "" && c.ProjectKey != "" {
			key.Key = c.ProjectKey
		}
		if key.Key == "" {
			return fmt.Errorf("keys[%d].key cannot be empty", index)
		}
		if !keyWasEmpty {
			if _, exists := seenExplicit[key.Key]; exists {
				return fmt.Errorf("duplicate configured key %q", key.Key)
			}
			seenExplicit[key.Key] = struct{}{}
		}
		if key.UpstreamKey == "" {
			key.UpstreamKey = key.Key
		}
		if key.Strategy == "" {
			key.Strategy = "fixed"
		}
		switch key.Strategy {
		case "fixed", "round_robin", "failover":
		default:
			return fmt.Errorf("keys[%d].strategy must be one of fixed, round_robin, failover", index)
		}
		if key.Proxy != "" && len(key.Proxies) > 0 {
			return fmt.Errorf("keys[%d] cannot configure both proxy and proxies", index)
		}
		for proxyIndex := range key.Proxies {
			key.Proxies[proxyIndex] = strings.TrimSpace(key.Proxies[proxyIndex])
			if err := ValidateProxyURL(key.Proxies[proxyIndex]); err != nil {
				return fmt.Errorf("keys[%d].proxies[%d]: %w", index, proxyIndex, err)
			}
		}
		if err := ValidateProxyURL(key.Proxy); err != nil {
			return fmt.Errorf("keys[%d].proxy: %w", index, err)
		}
	}
	return nil
}

func ValidateProxyURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("proxy must be an absolute URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h", "socket5h":
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
}
