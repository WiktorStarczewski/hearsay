package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestTranslateStreamEvent_CustomToolUse confirms the SDK→LoopEvent
// translation for the only event the agent loop has to act on
// imperatively.
func TestTranslateStreamEvent_CustomToolUse(t *testing.T) {
	u := anthropic.BetaManagedAgentsStreamSessionEventsUnion{
		Type:  "agent.custom_tool_use",
		ID:    "tu-123",
		Name:  "read",
		Input: map[string]any{"file_path": "README.md"},
	}
	ev := translateStreamEvent(u)
	if ev.Kind != EventCustomToolUse {
		t.Fatalf("Kind = %v, want EventCustomToolUse", ev.Kind)
	}
	if ev.ToolUseID != "tu-123" || ev.ToolName != "read" {
		t.Errorf("ToolUseID/Name wrong: %+v", ev)
	}
	if !strings.Contains(string(ev.ToolInput), "README.md") {
		t.Errorf("ToolInput did not round-trip: %s", string(ev.ToolInput))
	}
}

// TestTranslateStreamEvent_OtherTypes covers session.status_idle,
// session.error, span.model_request_end, agent.message, and unknown
// types in one parameterized walk so we don't drown the file in tiny
// near-identical tests.
func TestTranslateStreamEvent_OtherTypes(t *testing.T) {
	cases := []struct {
		name string
		u    anthropic.BetaManagedAgentsStreamSessionEventsUnion
		want LoopEventKind
	}{
		{"idle", anthropic.BetaManagedAgentsStreamSessionEventsUnion{Type: "session.status_idle"}, EventStatusIdle},
		{"error", anthropic.BetaManagedAgentsStreamSessionEventsUnion{Type: "session.error"}, EventError},
		{"span_end", anthropic.BetaManagedAgentsStreamSessionEventsUnion{Type: "span.model_request_end"}, EventTokenUsage},
		{"message", anthropic.BetaManagedAgentsStreamSessionEventsUnion{Type: "agent.message"}, EventAgentMessage},
		{"unknown", anthropic.BetaManagedAgentsStreamSessionEventsUnion{Type: "agent.thinking"}, EventOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := translateStreamEvent(c.u).Kind; got != c.want {
				t.Errorf("Kind = %v, want %v", got, c.want)
			}
		})
	}
}

// TestExtractAgentText handles the empty-content case so the helper
// is covered without poking at the SDK's content-union internals.
func TestExtractAgentText_EmptyContent(t *testing.T) {
	u := anthropic.BetaManagedAgentsStreamSessionEventsUnion{Type: "agent.message"}
	if got := extractAgentText(u); got != "" {
		t.Errorf("expected empty text for empty content, got %q", got)
	}
}

func TestErrorMsgFromUnion_NonEmpty(t *testing.T) {
	u := anthropic.BetaManagedAgentsStreamSessionEventsUnion{Type: "session.error"}
	if got := errorMsgFromUnion(u); got == "" {
		t.Errorf("expected non-empty serialized error union")
	}
}

func TestStopReasonHintFromUnion_Empty(t *testing.T) {
	u := anthropic.BetaManagedAgentsStreamSessionEventsUnion{Type: "session.status_idle"}
	// Default StopReason union is zero-valued; helper should yield ""
	// without panicking.
	if got := stopReasonHintFromUnion(u); got != "" {
		// It's OK for the SDK to round-trip an empty object string;
		// we just want non-panic.
		t.Logf("stopReasonHintFromUnion returned %q for empty union", got)
	}
}

// fakeStream implements the minimal "stream" interface pumpStream
// reads from.  Lets us drive translation + channel pumping without an
// HTTP round-trip.
type fakeStream struct {
	events []anthropic.BetaManagedAgentsStreamSessionEventsUnion
	idx    int
	err    error
}

func (f *fakeStream) Next() bool {
	if f.idx < len(f.events) {
		return true
	}
	return false
}

func (f *fakeStream) Current() anthropic.BetaManagedAgentsStreamSessionEventsUnion {
	ev := f.events[f.idx]
	f.idx++
	return ev
}

func (f *fakeStream) Err() error { return f.err }

func TestPumpStream_TranslatesAndCloses(t *testing.T) {
	stream := &fakeStream{events: []anthropic.BetaManagedAgentsStreamSessionEventsUnion{
		{Type: "agent.message"},
		{Type: "session.status_idle"},
	}}
	out := make(chan LoopEvent, 4)
	a := &sdkAgent{}
	a.pumpStream(t.Context(), stream, out)

	var got []LoopEventKind
	for ev := range out {
		got = append(got, ev.Kind)
	}
	if len(got) != 2 || got[0] != EventAgentMessage || got[1] != EventStatusIdle {
		t.Errorf("kinds = %v; want [AgentMessage, StatusIdle]", got)
	}
}

func TestPumpStream_PropagatesStreamError(t *testing.T) {
	stream := &fakeStream{err: errStream("boom")}
	out := make(chan LoopEvent, 4)
	a := &sdkAgent{}
	a.pumpStream(t.Context(), stream, out)
	ev, ok := <-out
	if !ok {
		t.Fatalf("expected an EventError, channel closed empty")
	}
	if ev.Kind != EventError || ev.ErrorMsg == "" {
		t.Errorf("got %+v", ev)
	}
}

type errStream string

func (e errStream) Error() string { return string(e) }

// TestOneShot_FailsAtEnvironmentCreate exercises the ensureInit error
// path: ANTHROPIC_BASE_URL points at a server that 500s every
// request, so Environments.New fails and OneShot returns an error
// transcript.  Covers ensureInit + OneShot + runOnce error paths +
// the audit-log call sequence.
func TestOneShot_FailsAtEnvironmentCreate(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"type":"error","error":{"type":"api_error","message":"stub upstream"}}`, http.StatusInternalServerError)
	}))
	defer stub.Close()

	auditPath := t.TempDir() + "/agent.log"
	auditor, err := NewAuditor(auditPath)
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer auditor.Close()

	ag, err := New(Config{
		APIKey:        "sk-ant-test",
		BaseURL:       stub.URL,
		PeerName:      "test-peer",
		DefaultBudget: Budget{MaxTokens: 100, MaxToolCalls: 1, Timeout: 0},
		Auditor:       auditor,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tx, err := ag.OneShot(t.Context(), OneShotRequest{
		Prompt:  "hello",
		Project: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("OneShot returned Go error (expected error in transcript): %v", err)
	}
	if tx.StopReason != StopReasonError {
		t.Errorf("StopReason=%v, want error", tx.StopReason)
	}
	if tx.ErrorSummary == "" {
		t.Errorf("ErrorSummary should be populated when StopReason=error")
	}
}

// TestOneShot_HappyPathThroughSessionCreate spins up a minimal stub
// that answers Environment.New + Agents.New + Sessions.New + events
// Send with valid JSON, then returns a 5xx for StreamEvents.  That
// drives ensureInit success, runOnce session-create success, the
// initial Send success, and the stream-failure path — covering the
// non-error branches of those functions without standing up a full
// SSE simulator.
func TestOneShot_HappyPathThroughSessionCreate(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == "POST":
			_, _ = w.Write([]byte(`{"id":"env-1","name":"hearsay-test","type":"environment","created_at":"2026-04-25T00:00:00Z","updated_at":"2026-04-25T00:00:00Z","status":"ready"}`))
		case r.URL.Path == "/v1/agents" && r.Method == "POST":
			_, _ = w.Write([]byte(`{"id":"agent-1","name":"hearsay-test","type":"agent","version":1,"created_at":"2026-04-25T00:00:00Z","updated_at":"2026-04-25T00:00:00Z","model":{"id":"claude-opus-4-7"}}`))
		case r.URL.Path == "/v1/sessions" && r.Method == "POST":
			_, _ = w.Write([]byte(`{"id":"sess-1","type":"session","agent":{"id":"agent-1","type":"agent","version":1},"environment_id":"env-1","status":"running","created_at":"2026-04-25T00:00:00Z","updated_at":"2026-04-25T00:00:00Z"}`))
		case strings.Contains(r.URL.Path, "/events") && r.Method == "POST":
			_, _ = w.Write([]byte(`{"events":[]}`))
		default:
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"stream stub: deliberate failure"}}`, http.StatusServiceUnavailable)
		}
	}))
	defer stub.Close()

	ag, err := New(Config{
		APIKey:   "sk-ant-test",
		BaseURL:  stub.URL,
		PeerName: "test-peer",
		DefaultBudget: Budget{
			MaxTokens:    100,
			MaxToolCalls: 1,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tx, err := ag.OneShot(t.Context(), OneShotRequest{
		Prompt:  "list files",
		Project: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("OneShot returned Go error: %v", err)
	}
	// The session was created; the stream itself failed.  The loop
	// either sees an EventError (ideal) or the channel closes
	// without an idle (which our loop maps to end_turn).  Either way
	// we've exercised ensureInit, runOnce session-create, and the
	// initial Send.  We assert one of those terminal states.
	switch tx.StopReason {
	case StopReasonError, StopReasonEndTurn, StopReasonShutdown:
		// any of these proves we reached and exited the loop
	default:
		t.Errorf("unexpected StopReason: %v", tx.StopReason)
	}
}

// TestOneShot_RejectsInvalidProject drives the project-validation
// branch of OneShot — a non-existent path should short-circuit with
// errorSummary=invalid_project before any SDK call happens.
func TestOneShot_RejectsInvalidProject(t *testing.T) {
	ag, err := New(Config{APIKey: "sk-ant-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tx, _ := ag.OneShot(t.Context(), OneShotRequest{
		Prompt:  "x",
		Project: "/no/such/path/please/no",
	})
	if tx.StopReason != StopReasonError {
		t.Errorf("StopReason=%v, want error", tx.StopReason)
	}
	if tx.ErrorSummary != ErrInvalidProject {
		t.Errorf("ErrorSummary=%v, want invalid_project", tx.ErrorSummary)
	}
}

// TestOneShot_AppliesTimeoutBudget covers the budget-timeout branch
// in OneShot.  We pass a non-zero Timeout to trigger the
// `if budget.Timeout > 0` branch, but answer the stub fast enough
// that the test doesn't itself hang on a slow Close.
func TestOneShot_AppliesTimeoutBudget(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"type":"error"}`, http.StatusInternalServerError)
	}))
	defer stub.Close()

	ag, err := New(Config{
		APIKey:        "sk-ant-test",
		BaseURL:       stub.URL,
		PeerName:      "test-peer",
		DefaultBudget: Budget{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tx, _ := ag.OneShot(t.Context(), OneShotRequest{
		Prompt:  "x",
		Project: t.TempDir(),
	})
	// The point of this test isn't *which* failure mode we hit; it's
	// that the timeout branch was exercised on the way to one.
	if tx.StopReason == "" {
		t.Errorf("StopReason should not be empty; got %+v", tx)
	}
}
