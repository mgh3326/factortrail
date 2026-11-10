// Package config reads the service configuration from flags and environment
// variable names. Values never live in Git; the names are documented in
// docs/auth.md and in Usage.
package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// Environment variable names.
const (
	EnvProfile             = "FACTORTRAIL_PROFILE"
	EnvAddr                = "FACTORTRAIL_ADDR"
	EnvPublicHost          = "FACTORTRAIL_PUBLIC_HOST"
	EnvAccessTeamDomain    = "FACTORTRAIL_ACCESS_TEAM_DOMAIN"
	EnvAccessAUD           = "FACTORTRAIL_ACCESS_AUD"
	EnvAccessAllowedEmails = "FACTORTRAIL_ACCESS_ALLOWED_EMAILS"
	EnvRetroBaseURL        = "FACTORTRAIL_RETRO_BASE_URL"
	EnvRetroTokenFile      = "FACTORTRAIL_RETRO_TOKEN_FILE"
)

// DefaultAddr is the listen address when none is given: loopback only.
const DefaultAddr = "127.0.0.1:8820"

// Usage documents the serve flags and environment names.
const Usage = `usage: factortrail serve [flags]

flags (a flag overrides its environment variable):
  --profile NAME       tool profile to serve (required; env ` + EnvProfile + `)
  --addr HOST:PORT     listen address (default ` + DefaultAddr + `; env ` + EnvAddr + `)
  --public-host HOST   the one trusted public Host header value behind a
                       reverse proxy (env ` + EnvPublicHost + `)
  --dev                local testing only: loopback bind only, Access optional

environment only:
  ` + EnvAccessTeamDomain + `   Cloudflare Access team domain
  ` + EnvAccessAUD + `           Access application audience tag
  ` + EnvAccessAllowedEmails + `  comma-separated operator email allowlist

All three Access settings are required unless --dev is given; a partial set
is always refused.

  ` + EnvRetroBaseURL + `       base URL of the auto_trader retro/v1 API
  ` + EnvRetroTokenFile + `     path of the file that holds the read token

The two upstream settings go together: both set, or both unset (the retro
tools then answer that the upstream is not configured). One without the other
is refused. The token is read from the file once at start and never from
argv or the environment.
`

// Access holds the Cloudflare Access settings.
type Access struct {
	TeamDomain    string
	AUD           string
	AllowedEmails []string
}

// Configured reports whether all three settings are present.
func (a Access) Configured() bool {
	return a.TeamDomain != "" && a.AUD != "" && len(a.AllowedEmails) > 0
}

// Upstream holds the auto_trader retro/v1 settings.
type Upstream struct {
	BaseURL   string
	TokenFile string
}

// Configured reports whether both settings are present.
func (u Upstream) Configured() bool {
	return u.BaseURL != "" && u.TokenFile != ""
}

// Config is the validated serve configuration.
type Config struct {
	// Profile is the requested tool profile name, trimmed. The profile table
	// in internal/mcp decides whether it exists.
	Profile    string
	Addr       string
	PublicHost string
	// Dev is local-testing mode: loopback bind only, Access optional.
	Dev      bool
	Access   Access
	Upstream Upstream
}

// Load parses the arguments after "serve" and the environment, and refuses
// any combination the service must not start with. Errors name settings, not
// their values.
func Load(args []string, getenv func(string) string) (Config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	profile := fs.String("profile", "", "")
	addr := fs.String("addr", "", "")
	publicHost := fs.String("public-host", "", "")
	dev := fs.Bool("dev", false, "")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	pick := func(flagValue, envName string) string {
		if v := strings.TrimSpace(flagValue); v != "" {
			return v
		}
		return strings.TrimSpace(getenv(envName))
	}
	cfg := Config{
		Profile:    pick(*profile, EnvProfile),
		Addr:       pick(*addr, EnvAddr),
		PublicHost: pick(*publicHost, EnvPublicHost),
		Dev:        *dev,
		Access: Access{
			TeamDomain:    strings.TrimSpace(getenv(EnvAccessTeamDomain)),
			AUD:           strings.TrimSpace(getenv(EnvAccessAUD)),
			AllowedEmails: splitList(getenv(EnvAccessAllowedEmails)),
		},
		Upstream: Upstream{
			BaseURL:   strings.TrimSpace(getenv(EnvRetroBaseURL)),
			TokenFile: strings.TrimSpace(getenv(EnvRetroTokenFile)),
		},
	}
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}

	if _, err := splitHostPort(cfg.Addr); err != nil {
		return Config{}, err
	}
	if cfg.Dev {
		if err := checkLoopback(cfg.Addr); err != nil {
			return Config{}, fmt.Errorf("dev mode: %w", err)
		}
	}

	if (cfg.Upstream.BaseURL == "") != (cfg.Upstream.TokenFile == "") {
		return Config{}, fmt.Errorf("incomplete upstream configuration: set both %s and %s or neither",
			EnvRetroBaseURL, EnvRetroTokenFile)
	}

	var missing []string
	if cfg.Access.TeamDomain == "" {
		missing = append(missing, EnvAccessTeamDomain)
	}
	if cfg.Access.AUD == "" {
		missing = append(missing, EnvAccessAUD)
	}
	if len(cfg.Access.AllowedEmails) == 0 {
		missing = append(missing, EnvAccessAllowedEmails)
	}
	switch {
	case len(missing) == 0:
	case len(missing) < 3:
		return Config{}, fmt.Errorf("incomplete Access configuration: %s not set; set all of %s, %s and %s or none (none only with --dev)",
			strings.Join(missing, ", "), EnvAccessTeamDomain, EnvAccessAUD, EnvAccessAllowedEmails)
	case !cfg.Dev:
		return Config{}, fmt.Errorf("Access configuration is required: set %s, %s and %s (or run with --dev for local testing)",
			EnvAccessTeamDomain, EnvAccessAUD, EnvAccessAllowedEmails)
	}
	return cfg, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitHostPort(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("listen address must be HOST:PORT")
	}
	if n, perr := strconv.Atoi(port); perr != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("listen address has an invalid port")
	}
	return host, nil
}

// checkLoopback accepts only "localhost" or a loopback IP literal. An empty
// host (":8820") and "0.0.0.0" bind every interface and are refused.
func checkLoopback(addr string) error {
	host, err := splitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listen address must be loopback (127.0.0.1, [::1] or localhost), got a non-loopback host")
}
