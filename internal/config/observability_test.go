package config

import (
	"strings"
	"testing"
)

func TestMetricsListenerIsOffUnlessConfigured(t *testing.T) {
	cfg, err := Load([]string{"--dev", "--profile", "retro"}, env(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsAddr != "" {
		t.Fatalf("MetricsAddr = %q, want empty", cfg.MetricsAddr)
	}
}

func TestMetricsAddrAcceptsOnlyLoopbackIPLiterals(t *testing.T) {
	ok := []string{"127.0.0.1:9100", "[::1]:9100", "127.0.0.1:0"}
	for _, addr := range ok {
		cfg, err := Load([]string{"--dev", "--profile", "retro"}, env(map[string]string{EnvMetricsAddr: addr}))
		if err != nil {
			t.Errorf("%s: unexpected refusal: %v", addr, err)
			continue
		}
		if cfg.MetricsAddr != addr {
			t.Errorf("MetricsAddr = %q, want %q", cfg.MetricsAddr, addr)
		}
	}
	refused := []string{"0.0.0.0:9100", ":9100", "localhost:9100", "192.0.2.10:9100", "[::]:9100", "9100", "127.0.0.1"}
	for _, addr := range refused {
		_, err := Load([]string{"--dev", "--profile", "retro"}, env(map[string]string{EnvMetricsAddr: addr}))
		if err == nil {
			t.Errorf("%s: accepted, want refused", addr)
			continue
		}
		if !strings.Contains(err.Error(), EnvMetricsAddr) {
			t.Errorf("%s: error %q does not name %s", addr, err, EnvMetricsAddr)
		}
		if strings.Contains(err.Error(), addr) {
			t.Errorf("%s: error echoes the value: %q", addr, err)
		}
	}
}

func TestSentrySettingsAreReadFromEnvironment(t *testing.T) {
	cfg, err := Load([]string{"--dev", "--profile", "retro"}, env(map[string]string{
		EnvSentryDSN:         " https://synthetic-key@sentry.example.invalid/1 ",
		EnvSentryEnvironment: " staging ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sentry.DSN != "https://synthetic-key@sentry.example.invalid/1" || cfg.Sentry.Environment != "staging" {
		t.Fatalf("Sentry = %+v", cfg.Sentry)
	}
	cfg, err = Load([]string{"--dev", "--profile", "retro"}, env(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sentry.DSN != "" {
		t.Fatalf("Sentry DSN set without the env var: %+v", cfg.Sentry)
	}
}
