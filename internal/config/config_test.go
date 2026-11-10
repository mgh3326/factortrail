package config

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var fullAccess = map[string]string{
	EnvAccessTeamDomain:    "example-team.cloudflareaccess.com",
	EnvAccessAUD:           "aud-tag",
	EnvAccessAllowedEmails: "operator@example.com, second@example.com",
}

func with(base map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestLoadProduction(t *testing.T) {
	cfg, err := Load([]string{"--profile", "retro"}, env(fullAccess))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "retro" || cfg.Dev {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Addr != DefaultAddr {
		t.Fatalf("addr = %q, want the loopback default %q", cfg.Addr, DefaultAddr)
	}
	if !strings.HasPrefix(DefaultAddr, "127.0.0.1:") {
		t.Fatalf("default address %q is not loopback", DefaultAddr)
	}
	if got := cfg.Access.AllowedEmails; len(got) != 2 || got[0] != "operator@example.com" || got[1] != "second@example.com" {
		t.Fatalf("emails = %q", got)
	}
}

func TestEnvSuppliesProfileAndAddress(t *testing.T) {
	cfg, err := Load(nil, env(with(fullAccess, EnvProfile, " retro\n", EnvAddr, "0.0.0.0:9000", EnvPublicHost, "tools.example.com")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "retro" || cfg.Addr != "0.0.0.0:9000" || cfg.PublicHost != "tools.example.com" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	cfg, err := Load([]string{"--profile", "retro", "--addr", "127.0.0.1:9100", "--public-host", "b.example.com"},
		env(with(fullAccess, EnvProfile, "other", EnvAddr, "127.0.0.1:9000", EnvPublicHost, "a.example.com")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "retro" || cfg.Addr != "127.0.0.1:9100" || cfg.PublicHost != "b.example.com" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestProductionRequiresAllThreeAccessSettings(t *testing.T) {
	all := []string{EnvAccessTeamDomain, EnvAccessAUD, EnvAccessAllowedEmails}
	// Every proper subset of the three settings, including none, is refused.
	for mask := 0; mask < 7; mask++ {
		m := map[string]string{}
		for i, k := range all {
			if mask&(1<<i) != 0 {
				m[k] = fullAccess[k]
			}
		}
		_, err := Load([]string{"--profile", "retro"}, env(m))
		if err == nil {
			t.Errorf("mask %03b: production start accepted an incomplete Access configuration", mask)
			continue
		}
		for i, k := range all {
			if mask&(1<<i) == 0 && !strings.Contains(err.Error(), k) {
				t.Errorf("mask %03b: error %q does not name the missing %s", mask, err, k)
			}
		}
	}
}

func TestBlankAccessValuesCountAsUnset(t *testing.T) {
	m := with(fullAccess, EnvAccessAUD, "   ", EnvAccessAllowedEmails, " , ,")
	_, err := Load([]string{"--profile", "retro"}, env(m))
	if err == nil || !strings.Contains(err.Error(), EnvAccessAUD) || !strings.Contains(err.Error(), EnvAccessAllowedEmails) {
		t.Fatalf("err = %v, want both blank settings named", err)
	}
}

func TestErrorsDoNotEchoValues(t *testing.T) {
	m := map[string]string{EnvAccessTeamDomain: "secret-team.example.com", EnvAccessAUD: "secret-aud"}
	_, err := Load([]string{"--profile", "retro"}, env(m))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret-team") || strings.Contains(err.Error(), "secret-aud") {
		t.Fatalf("error leaks a configured value: %v", err)
	}
}

func TestDevModeLoopbackOnly(t *testing.T) {
	tests := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:8820", true},
		{"127.0.0.1:0", true},
		{"[::1]:8820", true},
		{"localhost:8820", true},
		{"127.5.5.5:8820", true},
		{"0.0.0.0:8820", false},
		{":8820", false},
		{"[::]:8820", false},
		{"192.168.1.10:8820", false},
		{"10.0.0.1:8820", false},
		{"example.com:8820", false},
		{"localhost.example.com:8820", false},
		{"127.0.0.1", false},
		{"127.0.0.1:notaport", false},
		{"127.0.0.1:70000", false},
		{"", true}, // empty means the loopback default
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			args := []string{"--dev", "--profile", "retro"}
			if tt.addr != "" {
				args = append(args, "--addr", tt.addr)
			}
			_, err := Load(args, env(nil))
			if (err == nil) != tt.ok {
				t.Fatalf("dev addr %q: err = %v, want ok=%v", tt.addr, err, tt.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "address") {
				t.Fatalf("error %q does not say what is wrong with the address", err)
			}
		})
	}
}

func TestDevModeAcceptsNoAccessButNotAPartialSet(t *testing.T) {
	if _, err := Load([]string{"--dev", "--profile", "retro"}, env(nil)); err != nil {
		t.Fatalf("dev without Access refused: %v", err)
	}
	cfg, err := Load([]string{"--dev", "--profile", "retro"}, env(fullAccess))
	if err != nil || !cfg.Access.Configured() {
		t.Fatalf("dev with full Access: cfg = %+v err = %v", cfg, err)
	}
	if _, err := Load([]string{"--dev", "--profile", "retro"}, env(map[string]string{EnvAccessAUD: "x"})); err == nil {
		t.Fatal("dev accepted a partial Access configuration")
	}
}

func TestDevIsFlagOnly(t *testing.T) {
	cfg, err := Load([]string{"--profile", "retro"}, env(with(fullAccess, "FACTORTRAIL_DEV", "1")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dev {
		t.Fatal("dev mode switched on by the environment")
	}
}

func TestLoadRejectsBadArguments(t *testing.T) {
	if _, err := Load([]string{"--nope"}, env(fullAccess)); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if _, err := Load([]string{"--profile", "retro", "extra"}, env(fullAccess)); err == nil {
		t.Fatal("positional argument accepted")
	}
	if _, err := Load([]string{"-h"}, env(fullAccess)); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h err = %v, want flag.ErrHelp", err)
	}
}

func TestProfileIsPassedThroughUnvalidated(t *testing.T) {
	// Profile names are owned by the profile table in internal/mcp; config
	// only trims. An empty name reaches that table and is refused there.
	cfg, err := Load(nil, env(fullAccess))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "" {
		t.Fatalf("profile = %q, want empty", cfg.Profile)
	}
}
