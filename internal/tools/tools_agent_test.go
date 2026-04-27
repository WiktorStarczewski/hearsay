package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/WiktorStarczewski/hearsay/internal/agent"
)

// assertEmptyStructuredContent fails the test if the result's
// StructuredContent carries any field. The SDK may still emit `{}` (or
// nil) for an empty struct, both of which are fine — anything richer
// means the inlining regressed and we've reintroduced the metadata-only
// rendering bug for consumers that prefer the structured channel.
func assertEmptyStructuredContent(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	if res == nil || res.StructuredContent == nil {
		return
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	if s := string(raw); s != "{}" && s != "null" {
		t.Errorf("StructuredContent must be empty so the markdown body is the source of truth; got %s", s)
	}
}

// fakeAgent satisfies agent.Agent without touching the SDK.  Tests
// inject canned Transcripts (or errors) so the tools layer's behavior
// is exercised end-to-end through real MCP.  PR B added the
// conversation-lifecycle methods; the OneShot test stays here, the
// PR-B tools tests use the same struct with the relevant fields set.
type fakeAgent struct {
	last    agent.OneShotRequest
	respond func(req agent.OneShotRequest) (agent.Transcript, error)

	// PR-B fields.  Each is opt-in so the OneShot tests don't need
	// to set them.
	startResp     func(req agent.StartReq) (agent.ConvID, time.Time, agent.Budget, error)
	sendResp      func(convID agent.ConvID, prompt string, budget agent.Budget) (agent.Transcript, error)
	listResp      func() []agent.ConvMeta
	endResp       func(convID agent.ConvID, reason agent.EndReason) (agent.EndSummary, error)
	lastStart     agent.StartReq
	lastSendConv  agent.ConvID
	lastSendBody  string
	lastSendBudg  agent.Budget
	lastEndConv   agent.ConvID
	lastEndReason agent.EndReason
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

func (f *fakeAgent) StartConversation(_ context.Context, req agent.StartReq) (agent.ConvID, time.Time, agent.Budget, error) {
	f.lastStart = req
	if f.startResp != nil {
		return f.startResp(req)
	}
	return agent.ConvID("conv-stub"), time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), req.Budget, nil
}

func (f *fakeAgent) SendMessage(_ context.Context, convID agent.ConvID, prompt string, budget agent.Budget) (agent.Transcript, error) {
	f.lastSendConv = convID
	f.lastSendBody = prompt
	f.lastSendBudg = budget
	if f.sendResp != nil {
		return f.sendResp(convID, prompt, budget)
	}
	return agent.Transcript{
		Markdown:   "## Assistant\n\nstub turn reply.\n",
		StopReason: agent.StopReasonEndTurn,
		ElapsedMs:  4,
	}, nil
}

func (f *fakeAgent) ListConversations() []agent.ConvMeta {
	if f.listResp != nil {
		return f.listResp()
	}
	return nil
}

func (f *fakeAgent) EndConversation(_ context.Context, convID agent.ConvID, reason agent.EndReason) (agent.EndSummary, error) {
	f.lastEndConv = convID
	f.lastEndReason = reason
	if f.endResp != nil {
		return f.endResp(convID, reason)
	}
	return agent.EndSummary{Ended: true, EndedReason: reason}, nil
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
			if !strings.Contains(tool.Description, "parallel Claude Code subprocess") {
				t.Errorf("description must include 'parallel Claude Code subprocess' for routing disambiguation")
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
	// Body must contain BOTH the header and the assistant text — the whole
	// point of inlining is that consumers preferring StructuredContent still
	// receive the response.
	if !strings.Contains(body, "Found 3 errors") {
		t.Errorf("transcript missing assistant body: %q", body)
	}
	for _, want := range []string{
		"turns=1",
		"toolCalls=2",
		"stopReason=" + string(agent.StopReasonEndTurn),
		"elapsedMs=150",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metadata header missing %q in body: %q", want, body)
		}
	}
	// StructuredContent must be empty so consumers that prefer it don't end
	// up with a metadata-only echo and no transcript.
	assertEmptyStructuredContent(t, res)
}

// TestAskPeerClaude_BodySurvivesWhenStructuredPreferred is the explicit
// regression for the "metadata-only response" bug: when an MCP consumer
// renders only StructuredContent, the model still has to receive the
// transcript. Inlining the header into the markdown plus emitting an empty
// AskPeerClaudeOutput keeps the body the unconditional source of truth.
func TestAskPeerClaude_BodySurvivesWhenStructuredPreferred(t *testing.T) {
	ag := &fakeAgent{
		respond: func(_ agent.OneShotRequest) (agent.Transcript, error) {
			return agent.Transcript{
				Markdown:   "the answer is 42",
				TurnCount:  1,
				StopReason: agent.StopReasonEndTurn,
				ElapsedMs:  10,
			}, nil
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "ask_peer_claude", map[string]any{"prompt": "q"})
	body := textOf(t, res.Content[0])
	if !strings.Contains(body, "the answer is 42") {
		t.Errorf("body must contain transcript even when consumer ignores StructuredContent; got %q", body)
	}
	assertEmptyStructuredContent(t, res)
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
	body := textOf(t, res.Content[0])
	if !strings.Contains(body, "errorSummary="+string(agent.ErrAPIUnavailable)) {
		t.Errorf("error summary missing from markdown header: %q", body)
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

// ---------------- start_peer_conversation ----------------

func TestStartPeerConversation_HappyPath(t *testing.T) {
	ag := &fakeAgent{
		startResp: func(req agent.StartReq) (agent.ConvID, time.Time, agent.Budget, error) {
			return agent.ConvID("sess-42"),
				time.Date(2026, 4, 25, 13, 0, 0, 0, time.UTC),
				agent.Budget{MaxTokens: 16384, MaxToolCalls: 20, Timeout: 2 * time.Minute},
				nil
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "start_peer_conversation", map[string]any{
		"system_prompt": "you are an investigator",
		"max_tokens":    16384,
	})
	if res.IsError {
		t.Fatalf("isError=true unexpectedly: %+v", res.Content)
	}
	var out StartPeerConversationOutput
	structuredDecode(t, res, &out)
	if out.ConvID != "sess-42" {
		t.Errorf("convId=%q", out.ConvID)
	}
	if out.EffectiveBudget.MaxTokens != 16384 {
		t.Errorf("effectiveBudget.maxTokens=%d", out.EffectiveBudget.MaxTokens)
	}
	if out.EffectiveBudget.TimeoutSeconds != 120 {
		t.Errorf("effectiveBudget.timeoutSeconds=%d", out.EffectiveBudget.TimeoutSeconds)
	}
	if ag.lastStart.SystemPrompt != "you are an investigator" {
		t.Errorf("lastStart.SystemPrompt=%q", ag.lastStart.SystemPrompt)
	}
}

func TestStartPeerConversation_CapReached(t *testing.T) {
	ag := &fakeAgent{
		startResp: func(_ agent.StartReq) (agent.ConvID, time.Time, agent.Budget, error) {
			return "", time.Time{}, agent.Budget{}, agent.ErrConvCap
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "start_peer_conversation", map[string]any{})
	if !res.IsError {
		t.Errorf("expected isError=true on cap-reached")
	}
	body := textOf(t, res.Content[0])
	if !strings.Contains(body, "max concurrent conversations") {
		t.Errorf("error message should reference the cap; got %q", body)
	}
}

// ---------------- send_peer_message ----------------

func TestSendPeerMessage_HappyPath(t *testing.T) {
	ag := &fakeAgent{
		sendResp: func(_ agent.ConvID, prompt string, _ agent.Budget) (agent.Transcript, error) {
			return agent.Transcript{
				Markdown:      "## Assistant\n\nturn 2 reply.\n",
				TurnCount:     2,
				ToolCallCount: 1,
				StopReason:    agent.StopReasonEndTurn,
			}, nil
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "send_peer_message", map[string]any{
		"convId": "sess-42",
		"prompt": "follow up",
	})
	if res.IsError {
		t.Fatalf("isError=true: %+v", res.Content)
	}
	body := textOf(t, res.Content[0])
	if !strings.Contains(body, "turn 2 reply") {
		t.Errorf("markdown missing body: %q", body)
	}
	for _, want := range []string{"turns=2", "toolCalls=1"} {
		if !strings.Contains(body, want) {
			t.Errorf("metadata header missing %q in body: %q", want, body)
		}
	}
	assertEmptyStructuredContent(t, res)
	if ag.lastSendConv != "sess-42" || ag.lastSendBody != "follow up" {
		t.Errorf("send args lost: conv=%q body=%q", ag.lastSendConv, ag.lastSendBody)
	}
}

func TestSendPeerMessage_RejectsMissingArgs(t *testing.T) {
	cs := connectAgentPair(t, &fakeAgent{})
	for _, args := range []map[string]any{
		{},
		{"convId": "x"},
		{"prompt": "x"},
	} {
		res := callTool(t, cs, "send_peer_message", args)
		if !res.IsError {
			t.Errorf("expected isError for args=%+v", args)
		}
	}
}

func TestSendPeerMessage_HandlesReapedAndUnknown(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantBody  string
	}{
		{"reaped", agent.ErrConvReaped, "reaped after idle"},
		{"unknown", agent.ErrUnknownConv, "unknown convId"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ag := &fakeAgent{
				sendResp: func(agent.ConvID, string, agent.Budget) (agent.Transcript, error) {
					return agent.Transcript{}, c.err
				},
			}
			cs := connectAgentPair(t, ag)
			res := callTool(t, cs, "send_peer_message", map[string]any{
				"convId": "x",
				"prompt": "y",
			})
			if !res.IsError {
				t.Fatalf("expected isError=true")
			}
			if !strings.Contains(textOf(t, res.Content[0]), c.wantBody) {
				t.Errorf("error body missing %q; got %q", c.wantBody, textOf(t, res.Content[0]))
			}
		})
	}
}

// ---------------- list_peer_conversations ----------------

func TestListPeerConversations_HappyPath(t *testing.T) {
	now := time.Date(2026, 4, 25, 14, 0, 0, 0, time.UTC)
	ag := &fakeAgent{
		listResp: func() []agent.ConvMeta {
			return []agent.ConvMeta{
				{ConvID: "c1", StartedAt: now.Add(-2 * time.Minute), LastActivityAt: now, TurnCount: 3, Preview: "first prompt"},
			}
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "list_peer_conversations", map[string]any{})
	if res.IsError {
		t.Fatalf("isError=true: %+v", res.Content)
	}
	var out ListPeerConversationsOutput
	structuredDecode(t, res, &out)
	if len(out.Conversations) != 1 {
		t.Fatalf("len=%d", len(out.Conversations))
	}
	first := out.Conversations[0]
	if first.ConvID != "c1" || first.TurnCount != 3 || first.Preview != "first prompt" {
		t.Errorf("decoded wrong: %+v", first)
	}
}

func TestListPeerConversations_EmptyList(t *testing.T) {
	cs := connectAgentPair(t, &fakeAgent{})
	res := callTool(t, cs, "list_peer_conversations", map[string]any{})
	var out ListPeerConversationsOutput
	structuredDecode(t, res, &out)
	if len(out.Conversations) != 0 {
		t.Errorf("expected empty list, got %d", len(out.Conversations))
	}
}

// ---------------- end_peer_conversation ----------------

func TestEndPeerConversation_HappyPath(t *testing.T) {
	ag := &fakeAgent{
		endResp: func(_ agent.ConvID, reason agent.EndReason) (agent.EndSummary, error) {
			return agent.EndSummary{Ended: true, TotalTurns: 3, EndedReason: reason}, nil
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "end_peer_conversation", map[string]any{"convId": "c1"})
	if res.IsError {
		t.Fatalf("isError=true: %+v", res.Content)
	}
	var out EndPeerConversationOutput
	structuredDecode(t, res, &out)
	if !out.Ended || out.AlreadyEnded {
		t.Errorf("Ended/AlreadyEnded wrong: %+v", out)
	}
	if out.TotalTurns != 3 || out.EndedReason != string(agent.EndedByCaller) {
		t.Errorf("decode wrong: %+v", out)
	}
	if ag.lastEndConv != "c1" || ag.lastEndReason != agent.EndedByCaller {
		t.Errorf("end args lost: conv=%q reason=%v", ag.lastEndConv, ag.lastEndReason)
	}
}

func TestEndPeerConversation_Idempotent(t *testing.T) {
	ag := &fakeAgent{
		endResp: func(_ agent.ConvID, _ agent.EndReason) (agent.EndSummary, error) {
			return agent.EndSummary{Ended: true, AlreadyEnded: true, TotalTurns: 3, EndedReason: agent.EndedByCaller}, nil
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "end_peer_conversation", map[string]any{"convId": "c1"})
	var out EndPeerConversationOutput
	structuredDecode(t, res, &out)
	if !out.Ended || !out.AlreadyEnded {
		t.Errorf("expected Ended && AlreadyEnded; got %+v", out)
	}
}

func TestEndPeerConversation_UnknownReturnsError(t *testing.T) {
	ag := &fakeAgent{
		endResp: func(_ agent.ConvID, _ agent.EndReason) (agent.EndSummary, error) {
			return agent.EndSummary{}, agent.ErrUnknownConv
		},
	}
	cs := connectAgentPair(t, ag)
	res := callTool(t, cs, "end_peer_conversation", map[string]any{"convId": "missing"})
	if !res.IsError {
		t.Errorf("expected isError=true")
	}
	if !strings.Contains(textOf(t, res.Content[0]), "unknown conversation id") {
		t.Errorf("error body missing 'unknown conversation id': %q", textOf(t, res.Content[0]))
	}
}

func TestEndPeerConversation_RejectsMissingConvID(t *testing.T) {
	cs := connectAgentPair(t, &fakeAgent{})
	res := callTool(t, cs, "end_peer_conversation", map[string]any{})
	if !res.IsError {
		t.Errorf("expected isError on missing convId")
	}
}

// TestConversationTools_AllRegisteredWhenAgentSet confirms all four
// PR-B tools land in the catalog (and ask_peer_claude is still
// there).  Catches a regression where Register would silently drop a
// new addX call.
func TestConversationTools_AllRegisteredWhenAgentSet(t *testing.T) {
	cs := connectAgentPair(t, &fakeAgent{})
	want := map[string]bool{
		"ask_peer_claude":         false,
		"start_peer_conversation": false,
		"send_peer_message":       false,
		"list_peer_conversations": false,
		"end_peer_conversation":   false,
	}
	for tool := range cs.Tools(context.Background(), nil) {
		if _, expected := want[tool.Name]; expected {
			want[tool.Name] = true
		}
	}
	for name, present := range want {
		if !present {
			t.Errorf("tool %q not registered", name)
		}
	}
}
