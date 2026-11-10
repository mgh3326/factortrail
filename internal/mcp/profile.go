package mcp

import (
	"fmt"
	"sort"
	"strings"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// tool is one row of the static tool table. Adding a tool is one row here
// plus its handler: register adds the tool to a server with the SDK's typed
// AddTool, and name must equal the name it registers.
type tool struct {
	name     string
	register func(*gmcp.Server)
}

// profiles is the static profile table. A server starts with exactly one of
// these; there is no dynamic filtering and no default profile.
//
// The retro profile is empty in the bootstrap change. The retrospective tools
// are added as rows of retroTools together with their handlers.
var profiles = map[string][]tool{
	"retro": retroTools,
}

var retroTools = []tool{}

// lookupProfile returns the tools of the named profile. The name must match
// exactly: an empty or unknown name is an error, never a fallback.
func lookupProfile(name string) ([]tool, error) {
	if name == "" || strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("profile is required (known: %s)", knownProfiles())
	}
	tools, ok := profiles[name]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q (known: %s)", name, knownProfiles())
	}
	return tools, nil
}

func knownProfiles() string {
	names := make([]string, 0, len(profiles))
	for n := range profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
