package config

import "testing"

func TestSetDefaultsAndValidateAllowsProjectKeyPool(t *testing.T) {
	cfg := Config{
		ProjectKey: "project-demo",
		Keys:       []KeyConfig{{UpstreamKey: "sk-first"}, {UpstreamKey: "sk-second"}},
	}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Keys[0].Key != "project-demo" || cfg.Keys[1].Key != "project-demo" {
		t.Fatalf("pool client keys = %q, %q", cfg.Keys[0].Key, cfg.Keys[1].Key)
	}
}

func TestSetDefaultsAndValidateRequiresAdminKey(t *testing.T) {
	cfg := Config{Admin: AdminConfig{Enabled: true}, Keys: []KeyConfig{{Key: "client"}}}
	if err := cfg.SetDefaultsAndValidate(); err == nil {
		t.Fatal("expected admin key validation error")
	}
}
