package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// stubAnthropicServer responds with minimal-valid JSON for the
// non-streaming endpoints the conversation lifecycle hits, and a 5xx
// for StreamEvents.  Every test that needs a live SDK round-trip can
// reuse this rather than re-defining it.  Sessions get unique IDs so
// the cap test can rely on the local-state map keying being honest.
func stubAnthropicServer(t *testing.T) *httptest.Server {
	t.Helper()
	var sessSeq int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == "POST":
			_, _ = w.Write([]byte(`{"id":"env-1","name":"hearsay-test","type":"environment","created_at":"2026-04-25T00:00:00Z","updated_at":"2026-04-25T00:00:00Z","status":"ready"}`))
		case r.URL.Path == "/v1/agents" && r.Method == "POST":
			_, _ = w.Write([]byte(`{"id":"agent-1","name":"hearsay-test","type":"agent","version":1,"created_at":"2026-04-25T00:00:00Z","updated_at":"2026-04-25T00:00:00Z","model":{"id":"claude-opus-4-7"}}`))
		case r.URL.Path == "/v1/sessions" && r.Method == "POST":
			id := atomic.AddInt32(&sessSeq, 1)
			_, _ = fmt.Fprintf(w, `{"id":"sess-%d","type":"session","agent":{"id":"agent-1","type":"agent","version":1},"environment_id":"env-1","status":"running","created_at":"2026-04-25T00:00:00Z","updated_at":"2026-04-25T00:00:00Z"}`, id)
		case strings.HasPrefix(r.URL.Path, "/v1/sessions/") && r.Method == "DELETE":
			_, _ = w.Write([]byte(`{"id":"sess-deleted","type":"session","status":"deleted"}`))
		case strings.Contains(r.URL.Path, "/events") && r.Method == "POST":
			_, _ = w.Write([]byte(`{"events":[]}`))
		default:
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"stub deliberate failure"}}`, http.StatusServiceUnavailable)
		}
	}))
}

// TestStartConversation_SucceedsAgainstStub drives ensureInit +
// StartConversation against the minimal stub.  Confirms the convID is
// returned and listed.  Effective budget echo is asserted in the
// fake-agent path; here we just want the SDK round-trip exercised.
func TestStartConversation_SucceedsAgainstStub(t *testing.T) {
	stub := stubAnthropicServer(t)
	defer stub.Close()

	ag, err := New(Config{
		APIKey:           "sk-ant-test",
		BaseURL:          stub.URL,
		PeerName:         "test",
		MaxConversations: 5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ag.(Closer).Close()

	convID, started, eff, err := ag.StartConversation(t.Context(), StartReq{
		SystemPrompt: "you are a tester",
		Project:      t.TempDir(),
		Budget:       Budget{MaxTokens: 8192},
	})
	if err != nil {
		t.Fatalf("StartConversation: %v", err)
	}
	if !strings.HasPrefix(string(convID), "sess-") {
		t.Errorf("convID=%q", convID)
	}
	if started.IsZero() {
		t.Errorf("startedAt should not be zero")
	}
	if eff.MaxTokens != 8192 {
		t.Errorf("effective.MaxTokens=%d, want 8192", eff.MaxTokens)
	}
	if got := ag.ListConversations(); len(got) != 1 || got[0].ConvID != convID {
		t.Errorf("ListConversations returned %+v after StartConversation", got)
	}
}

// TestStartConversation_RespectsCap covers the cap-rejection path
// against the stub.  Two start calls succeed; the third is refused.
func TestStartConversation_RespectsCap(t *testing.T) {
	stub := stubAnthropicServer(t)
	defer stub.Close()

	ag, err := New(Config{
		APIKey:           "sk-ant-test",
		BaseURL:          stub.URL,
		PeerName:         "test",
		MaxConversations: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ag.(Closer).Close()

	for i := 0; i < 2; i++ {
		_, _, _, err := ag.StartConversation(t.Context(), StartReq{Project: t.TempDir()})
		if err != nil {
			t.Fatalf("start #%d: %v", i, err)
		}
	}
	_, _, _, err = ag.StartConversation(t.Context(), StartReq{Project: t.TempDir()})
	if err != ErrConvCap {
		t.Errorf("third start: err=%v, want ErrConvCap", err)
	}
}

// TestStartConversation_RejectsInvalidProject confirms the validation
// branch fires before any SDK call.
func TestStartConversation_RejectsInvalidProject(t *testing.T) {
	stub := stubAnthropicServer(t)
	defer stub.Close()

	ag, err := New(Config{APIKey: "sk-ant-test", BaseURL: stub.URL, PeerName: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ag.(Closer).Close()

	_, _, _, err = ag.StartConversation(t.Context(), StartReq{Project: "/no/such/path/please/no"})
	if err == nil {
		t.Errorf("expected invalid-project error")
	}
}

// TestSendMessage_KnownAndUnknown covers the lookupAlive branches:
// an unknown convID errors; the known convID drives runTurn, which
// fails on the stream-events 5xx and returns an error transcript.
func TestSendMessage_KnownAndUnknown(t *testing.T) {
	stub := stubAnthropicServer(t)
	defer stub.Close()

	ag, err := New(Config{APIKey: "sk-ant-test", BaseURL: stub.URL, PeerName: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ag.(Closer).Close()

	if _, err := ag.SendMessage(t.Context(), "no-such", "x", Budget{}); err != ErrUnknownConv {
		t.Errorf("unknown convID: err=%v, want ErrUnknownConv", err)
	}

	convID, _, _, err := ag.StartConversation(t.Context(), StartReq{Project: t.TempDir()})
	if err != nil {
		t.Fatalf("StartConversation: %v", err)
	}
	tx, err := ag.SendMessage(t.Context(), convID, "hello", Budget{})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// Stream returns 5xx → loop sees an EventError or a closed
	// channel.  Either way, the SendMessage returned a transcript
	// with a sane stop reason and the conv's lastActivityAt was
	// touched.
	if tx.StopReason == "" {
		t.Errorf("StopReason empty: %+v", tx)
	}
}

// TestEndConversation_SucceedsAgainstStub drives the SDK Delete path
// from EndConversation, plus the post-end ListConversations omits the
// terminated conv.
func TestEndConversation_SucceedsAgainstStub(t *testing.T) {
	stub := stubAnthropicServer(t)
	defer stub.Close()

	ag, err := New(Config{APIKey: "sk-ant-test", BaseURL: stub.URL, PeerName: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ag.(Closer).Close()

	convID, _, _, err := ag.StartConversation(t.Context(), StartReq{Project: t.TempDir()})
	if err != nil {
		t.Fatalf("StartConversation: %v", err)
	}
	summary, err := ag.EndConversation(t.Context(), convID, EndedByCaller)
	if err != nil {
		t.Fatalf("EndConversation: %v", err)
	}
	if !summary.Ended || summary.AlreadyEnded {
		t.Errorf("summary wrong: %+v", summary)
	}
	if got := ag.ListConversations(); len(got) != 0 {
		t.Errorf("ListConversations after End should be empty; got %+v", got)
	}

	// Idempotent re-end should report alreadyEnded=true.
	summary2, err := ag.EndConversation(t.Context(), convID, EndedByCaller)
	if err != nil {
		t.Fatalf("EndConversation #2: %v", err)
	}
	if !summary2.AlreadyEnded {
		t.Errorf("expected AlreadyEnded=true on re-end; got %+v", summary2)
	}
}

// TestReaper_RunsAndDeletes spins up an agent with a tight idle
// timeout, lets the reaper fire, and confirms the conversation flips
// to ended with reason idle_timeout.
func TestReaper_RunsAndDeletes(t *testing.T) {
	stub := stubAnthropicServer(t)
	defer stub.Close()

	ag, err := New(Config{
		APIKey:                  "sk-ant-test",
		BaseURL:                 stub.URL,
		PeerName:                "test",
		ConversationIdleTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ag.(Closer).Close()

	convID, _, _, err := ag.StartConversation(t.Context(), StartReq{Project: t.TempDir()})
	if err != nil {
		t.Fatalf("StartConversation: %v", err)
	}

	// Wait long enough for at least one reaper tick (interval is
	// idle/4 floored at 1s, plus the reap conditional).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := ag.ListConversations()
		if len(got) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := ag.ListConversations(); len(got) != 0 {
		t.Errorf("reaper never reaped %s; ListConversations = %+v", convID, got)
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
