package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/WiktorStarczewski/hearsay/internal/agent"
)

// fakeAgent satisfies agent.Agent without touching the SDK.  Tests
// inject canned Transcripts (or errors) so the tools layer's behavior
// is exercised end-to-end through real MCP.
type fakeAgent struct {
	last    agent.OneShotRequest
	respond func(req agent.OneShotRequest) (agent.Transcript, error)
}

func (f *fakeAgent) OneShot(_ context.Context, req agent.OneShotRequest) (agent.Transcript, error) {
	f.last = req
	if f.respond != nil {
		return f.respond(req)
	}
	return agent.Transcript{
		Markdown:   "## Assistant\n\nstub reply.\n",
		StopReason: agent.StopReasonEndTurn,
		ElapsedMs:  3,
	}, nil
}

// connectAgentPair builds a server with the agent tool registered and
// returns a paired client.  Mirrors connectPair in tools_test.go but
// installs an agent.
func connectAgentPair(t *testing.T, ag agent.Agent) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-hearsay", Version: "0"}, nil)
	c := testCtx(t)
	c.Agent = ag
	Register(srv, c)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// TestAskPeerClaude_OnlyRegisteredWhenAgentSet confirms the gating:
// without ctx.Agent, ask_peer_claude is not in the tool catalog.
func TestAskPeerClaude_OnlyRegisteredWhenAgentSet(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	Register(srv, testCtx(t)) // ctx.Agent == nil
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "tc", Version: "0"}, nil)
	cs, err := cli.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	for tool := range cs.Tools(ctx, nil) {
		if tool.Name == "ask_peer_claude" {
			t.Errorf("ask_peer_claude must NOT be registered when ctx.Agent is nil")
		}
	}
}

// TestAskPeerClaude_RegistersWhenAgentSet is the positive of the above.
func TestAskPeerClaude_RegistersWhenAgentSet(t *testing.T) {
	cs := connectAgentPair(t, &fakeAgent{})
	ctx := context.Background()
	found := false
	for tool := range cs.Tools(ctx, nil) {
		if tool.Name == "ask_peer_claude" {
			found = true
			if !strings.Contains(tool.Description, "parallel Claude session") {
				t.Errorf("description must include 'parallel Claude session' for routing disambiguation")
			}
			if !strings.Contains(tool.Description, "Ivan") {
				t.Errorf("description should bake the peer name in (testCtx uses 'ivan')")
			}
		}
	}
	if !found {
		t.Errorf("ask_peer_claude not in tool catalog")
	}
}

func TestAskPeerClaude_HappyPathReturnsMarkdownAndMetadata(t *testing.T) {
	ag := &fakeAgent{
		respond: func(_ agent.OneShotRequest) (agent.Transcript, error) {
			return agent.Transcript{
				Markdown:      "## Assistant\n\nFound 3 errors.\n",
				TurnCount:     1,
				ToolCallCount: 2,
				StopReason:    agent.StopReasonEndTurn,
				ElapsedMs:     150,
			}, nil
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "ask_peer_claude", map[string]any{"prompt": "hello"})
	if res.IsError {
		t.Fatalf("expected success, got isError=true: %+v", res.Content)
	}
	body := textOf(t, res.Content[0])
	if !strings.Contains(body, "Found 3 errors") {
		t.Errorf("transcript missing assistant body: %q", body)
	}
	var out AskPeerClaudeOutput
	structuredDecode(t, res, &out)
	if out.StopReason != string(agent.StopReasonEndTurn) {
		t.Errorf("StopReason=%q", out.StopReason)
	}
	if out.ToolCallCount != 2 {
		t.Errorf("ToolCallCount=%d", out.ToolCallCount)
	}
}

func TestAskPeerClaude_PassesBudgetThrough(t *testing.T) {
	ag := &fakeAgent{}
	cs := connectAgentPair(t, ag)
	callTool(t, cs, "ask_peer_claude", map[string]any{
		"prompt":           "audit",
		"max_tokens":       4096,
		"max_tool_calls":   5,
		"timeout_seconds":  45,
		"project":          "/tmp",
	})
	if ag.last.Budget.MaxTokens != 4096 {
		t.Errorf("Budget.MaxTokens=%d, want 4096", ag.last.Budget.MaxTokens)
	}
	if ag.last.Budget.MaxToolCalls != 5 {
		t.Errorf("Budget.MaxToolCalls=%d, want 5", ag.last.Budget.MaxToolCalls)
	}
	if ag.last.Budget.Timeout != 45*time.Second {
		t.Errorf("Budget.Timeout=%v, want 45s", ag.last.Budget.Timeout)
	}
	if ag.last.Project != "/tmp" {
		t.Errorf("Project=%q", ag.last.Project)
	}
	if ag.last.Prompt != "audit" {
		t.Errorf("Prompt=%q", ag.last.Prompt)
	}
}

func TestAskPeerClaude_RejectsEmptyPrompt(t *testing.T) {
	cs := connectAgentPair(t, &fakeAgent{})
	res := callTool(t, cs, "ask_peer_claude", map[string]any{})
	if !res.IsError {
		t.Errorf("expected isError=true for missing prompt")
	}
}

func TestAskPeerClaude_FlagsErrorOnAgentReturnsError(t *testing.T) {
	ag := &fakeAgent{
		respond: func(_ agent.OneShotRequest) (agent.Transcript, error) {
			return agent.Transcript{
				Markdown:     "_session error: 5" + "00 internal error_\n",
				StopReason:   agent.StopReasonError,
				ErrorSummary: agent.ErrAPIUnavailable,
			}, nil
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "ask_peer_claude", map[string]any{"prompt": "x"})
	if !res.IsError {
		t.Errorf("expected isError=true when agent reports stopReason=error")
	}
	var out AskPeerClaudeOutput
	structuredDecode(t, res, &out)
	if out.ErrorSummary != string(agent.ErrAPIUnavailable) {
		t.Errorf("ErrorSummary=%q", out.ErrorSummary)
	}
}

func TestAskPeerClaude_PropagatesAgentGoError(t *testing.T) {
	ag := &fakeAgent{
		respond: func(_ agent.OneShotRequest) (agent.Transcript, error) {
			return agent.Transcript{}, errors.New("upstream blew up")
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "ask_peer_claude", map[string]any{"prompt": "x"})
	if !res.IsError {
		t.Errorf("expected isError=true when OneShot returns a Go error")
	}
}
