// Command factortrail is the Factortrail service.
//
//	factortrail serve    run the MCP server
//	factortrail version  print the build revision
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/mgh3326/factortrail/internal/auth"
	"github.com/mgh3326/factortrail/internal/config"
	"github.com/mgh3326/factortrail/internal/mcp"
)

// revision is the source revision, set at build time with
// -ldflags "-X main.revision=<sha>". Without it, the VCS revision recorded by
// `go build` is used, and "unknown" when neither exists.
var revision string

const usage = `usage: factortrail <command>

commands:
  serve     run the MCP server (factortrail serve -h for flags)
  version   print the build revision
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil)
	stop()
	os.Exit(code)
}

// run is main with its inputs passed in. It returns the process exit code:
// 0 on a clean stop, 1 on a runtime failure and 2 when the invocation or
// configuration is refused. listen opens the listener; nil means TCP.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, listen func(addr string) (net.Listener, error)) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, buildRevision())
		return 0
	case "serve":
		return serve(ctx, args[1:], getenv, stdout, stderr, listen)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "factortrail: unknown command %q\n%s", args[0], usage)
		return 2
	}
}

func serve(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, listen func(addr string) (net.Listener, error)) int {
	refuse := func(err error) int {
		fmt.Fprintf(stderr, "factortrail: refusing to start: %v\n", err)
		return 2
	}
	cfg, err := config.Load(args, getenv)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, config.Usage)
		return 0
	}
	if err != nil {
		return refuse(err)
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	handler, err := newHandler(cfg, log)
	if err != nil {
		return refuse(err)
	}

	if listen == nil {
		listen = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
	}
	ln, err := listen(cfg.Addr)
	if err != nil {
		fmt.Fprintf(stderr, "factortrail: listen: %v\n", err)
		return 1
	}
	log.Info("serve.start", "addr", ln.Addr().String(), "profile", cfg.Profile, "dev", cfg.Dev,
		"access", cfg.Access.Configured(), "public_host_set", cfg.PublicHost != "", "revision", buildRevision())

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 16,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		fmt.Fprintf(stderr, "factortrail: serve: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(stderr, "factortrail: shutdown: %v\n", err)
		return 1
	}
	log.Info("serve.stop")
	return 0
}

// newHandler builds the MCP handler and, unless dev mode runs without Access
// settings, the verifier that guards it.
func newHandler(cfg config.Config, log *slog.Logger) (http.Handler, error) {
	mcpHandler, err := mcp.NewHandler(mcp.Options{
		Profile:    cfg.Profile,
		Version:    buildRevision(),
		PublicHost: cfg.PublicHost,
		Logger:     log,
	})
	if err != nil {
		return nil, err
	}
	if !cfg.Access.Configured() {
		// config.Load only lets this through in dev mode, on loopback.
		log.Warn("dev mode: Cloudflare Access is not configured; /mcp is unauthenticated and bound to loopback")
		return assemble(mcpHandler, nil), nil
	}
	verifier, err := auth.NewVerifier(auth.Config{
		TeamDomain:    cfg.Access.TeamDomain,
		AUD:           cfg.Access.AUD,
		AllowedEmails: cfg.Access.AllowedEmails,
		Logger:        log,
	})
	if err != nil {
		return nil, err
	}
	return assemble(mcpHandler, verifier), nil
}

// assemble wires the routes. /mcp sits behind the verifier, which runs before
// the SDK handler sees a request; a nil verifier (dev mode only) leaves it
// open. /healthz is open and returns no content.
func assemble(mcpHandler http.Handler, v *auth.Verifier) http.Handler {
	if v != nil {
		mcpHandler = v.Require(mcpHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/mcp", mcpHandler)
	return mux
}

func buildRevision() string {
	if revision != "" {
		return revision
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}
