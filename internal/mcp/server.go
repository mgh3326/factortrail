// Package mcp serves the Factortrail MCP server: the SDK's stateless
// Streamable HTTP handler, a static profile table and the Host and Origin
// checks that belong to the transport. Authentication is not here; callers
// wrap the handler (see internal/auth) so a request is verified before the
// SDK sees it.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DisplayName is the server name MCP clients show.
const DisplayName = "Factortrail"

// maxRequestBody bounds one JSON-RPC request. Tool arguments are small.
const maxRequestBody = 1 << 20

// Options configures NewHandler.
type Options struct {
	// Profile names the tool profile to serve. It is required.
	Profile string
	// Version is the build revision reported to clients.
	Version string
	// PublicHost is the one trusted public Host header value (for example
	// "tools.example.com") when the server runs behind a reverse proxy on
	// loopback. Empty keeps the SDK's DNS-rebinding protection on.
	PublicHost string
	// Retro is the upstream of the retro tools. Nil leaves the tools listed;
	// each then answers that the upstream is not configured.
	Retro  Retro
	Logger *slog.Logger
}

var publicHostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// NewHandler returns the /mcp handler for one profile.
//
// Host and Origin: the SDK refuses a request that reaches a loopback listener
// with a non-loopback Host (DNS rebinding protection). A reverse proxy on
// loopback forwards the public Host, so that guard would block it. The guard
// is therefore disabled only when PublicHost is set, and then this handler
// admits exactly that Host and refuses every other. Cross-origin browser
// requests are refused in both cases.
func NewHandler(o Options) (http.Handler, error) {
	tools, err := lookupProfile(o.Profile)
	if err != nil {
		return nil, err
	}
	if o.PublicHost != "" && !publicHostRE.MatchString(o.PublicHost) {
		return nil, fmt.Errorf("public host %q must be a bare host name with an optional port", o.PublicHost)
	}
	srv := newServer(o.Version, tools, o.Retro)
	var h http.Handler = gmcp.NewStreamableHTTPHandler(func(*http.Request) *gmcp.Server { return srv }, &gmcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		MaxRequestBodyBytes:        maxRequestBody,
		DisableLocalhostProtection: o.PublicHost != "",
		Logger:                     o.Logger,
	})
	h = http.NewCrossOriginProtection().Handler(h)
	if o.PublicHost != "" {
		h = exactHost(o.PublicHost, h)
	}
	return h, nil
}

func exactHost(public string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !equalASCIIFold(r.Host, public) {
			http.Error(w, "Forbidden: unexpected Host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// equalASCIIFold compares a and b ignoring case for A-Z only.
// strings.EqualFold would also equate the Kelvin sign with "k".
func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// newServer builds a server that serves exactly tools. The receiving
// middleware repeats the profile check at call time, so a handler that was
// registered on the server by mistake is neither listed nor callable.
func newServer(version string, tools []tool, retro Retro) *gmcp.Server {
	s := gmcp.NewServer(&gmcp.Implementation{Name: DisplayName, Version: version}, &gmcp.ServerOptions{
		// Advertise tools even when a profile has none.
		Capabilities: &gmcp.ServerCapabilities{Tools: &gmcp.ToolCapabilities{}},
	})
	allowed := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		t.register(s, retro)
		allowed[t.name] = struct{}{}
	}
	s.AddReceivingMiddleware(profileGuard(allowed))
	return s
}

func profileGuard(allowed map[string]struct{}) gmcp.Middleware {
	return func(next gmcp.MethodHandler) gmcp.MethodHandler {
		return func(ctx context.Context, method string, req gmcp.Request) (gmcp.Result, error) {
			switch method {
			case "tools/call":
				call, ok := req.(*gmcp.CallToolRequest)
				if !ok || call.Params == nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid tool call"}
				}
				if _, ok := allowed[call.Params.Name]; !ok {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q", call.Params.Name)}
				}
			case "tools/list":
				res, err := next(ctx, method, req)
				if err != nil {
					return res, err
				}
				list, ok := res.(*gmcp.ListToolsResult)
				if !ok {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "unexpected tools/list result"}
				}
				kept := make([]*gmcp.Tool, 0, len(list.Tools))
				for _, t := range list.Tools {
					if _, ok := allowed[t.Name]; ok {
						kept = append(kept, t)
					}
				}
				list.Tools = kept
				return list, nil
			}
			return next(ctx, method, req)
		}
	}
}
