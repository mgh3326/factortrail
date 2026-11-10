package mcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type reportedCall struct {
	name   string
	failed bool
}

func TestToolCallHookSeesProfileToolsOnly(t *testing.T) {
	var mu sync.Mutex
	var seen []reportedCall
	hook := func(name string, d time.Duration, failed bool) {
		if d < 0 {
			t.Errorf("negative duration for %s", name)
		}
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, reportedCall{name, failed})
	}

	var inCalls atomic.Int32
	failing := tool{name: "fails", register: func(s *gmcp.Server, _ Retro) {
		gmcp.AddTool(s, &gmcp.Tool{Name: "fails", Description: "test tool"}, func(_ context.Context, _ *gmcp.CallToolRequest, _ echoIn) (*gmcp.CallToolResult, any, error) {
			return nil, nil, errors.New("upstream said no")
		})
	}}
	srv := newServer("test", []tool{testTool("in_profile", &inCalls), failing}, nil, hook)
	cs := connect(t, srv)

	if _, err := cs.CallTool(ctxT(t), &gmcp.CallToolParams{Name: "in_profile", Arguments: map[string]any{"text": "hi"}}); err != nil {
		t.Fatal(err)
	}
	// A tool-level failure is an isError result, which the hook counts as failed.
	// The SDK may answer with isError rather than a protocol error; both are
	// failures for the hook, so the error is not checked here.
	_, _ = cs.CallTool(ctxT(t), &gmcp.CallToolParams{Name: "fails", Arguments: map[string]any{"text": "x"}})
	// A name outside the profile is refused before any handler and is not reported.
	if _, err := cs.CallTool(ctxT(t), &gmcp.CallToolParams{Name: "place_order", Arguments: map[string]any{}}); err == nil {
		t.Fatal("unknown tool call succeeded")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []reportedCall{{"in_profile", false}, {"fails", true}}
	if len(seen) != len(want) {
		t.Fatalf("hook saw %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("hook saw %v, want %v", seen, want)
		}
	}
}

func TestServerWithoutHookStillServes(t *testing.T) {
	var calls atomic.Int32
	cs := connect(t, newServer("test", []tool{testTool("in_profile", &calls)}, nil, nil))
	if _, err := cs.CallTool(ctxT(t), &gmcp.CallToolParams{Name: "in_profile", Arguments: map[string]any{"text": "hi"}}); err != nil || calls.Load() != 1 {
		t.Fatalf("err = %v calls = %d", err, calls.Load())
	}
}
