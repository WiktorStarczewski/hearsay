package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// runLoop is the unit-test driver for runEventLoop.  It pumps the
// supplied events through a channel and returns whatever the loop
// returns.  Tests can supply ExecuteTool / SendToolResult fakes.
func runLoop(
	t *testing.T,
	events []LoopEvent,
	hooks LoopHooks,
	budget Budget,
) (Transcript, []AuditToolInvoke) {
	t.Helper()
	ch := make(chan LoopEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	allow := map[string]bool{"read": true, "glob": true, "grep": true}
	tx, invokes, err := runEventLoop(context.Background(), ch, hooks, budget, allow)
	if err != nil {
		t.Fatalf("runEventLoop: %v", err)
	}
	return tx, invokes
}

func TestLoop_EndTurnOnAgentMessageThenIdle(t *testing.T) {
	hooks := LoopHooks{
		ExecuteTool:    func(string, json.RawMessage) (string, error) { t.Fatal("unexpected ExecuteTool"); return "", nil },
		SendToolResult: func(string, string, bool) error { t.Fatal("unexpected SendToolResult"); return nil },
	}
	tx, _ := runLoop(t, []LoopEvent{
		{Kind: EventAgentMessage, MessageText: "Hello, world."},
		{Kind: EventStatusIdle, StopReasonHint: "end_turn"},
	}, hooks, Budget{})

	if tx.StopReason != StopReasonEndTurn {
		t.Errorf("StopReason=%v, want end_turn", tx.StopReason)
	}
	if !strings.Contains(tx.Markdown, "Hello, world.") {
		t.Errorf("markdown missing assistant text: %q", tx.Markdown)
	}
	if tx.TurnCount != 1 {
		t.Errorf("TurnCount=%d, want 1", tx.TurnCount)
	}
}

func TestLoop_DispatchesAllowedTool(t *testing.T) {
	gotName := ""
	gotInput := ""
	sentBody := ""
	sentIsError := false
	hooks := LoopHooks{
		ExecuteTool: func(name string, input json.RawMessage) (string, error) {
			gotName = name
			gotInput = string(input)
			return "tool body", nil
		},
		SendToolResult: func(_, body string, isErr bool) error {
			sentBody = body
			sentIsError = isErr
			return nil
		},
	}
	tx, invokes := runLoop(t, []LoopEvent{
		{Kind: EventCustomToolUse, ToolUseID: "tu1", ToolName: "read", ToolInput: []byte(`{"file_path":"x"}`)},
		{Kind: EventStatusIdle},
	}, hooks, Budget{})

	if gotName != "read" {
		t.Errorf("ExecuteTool name=%q", gotName)
	}
	if !strings.Contains(gotInput, "x") {
		t.Errorf("ExecuteTool input=%q", gotInput)
	}
	if sentBody != "tool body" {
		t.Errorf("SendToolResult body=%q", sentBody)
	}
	if sentIsError {
		t.Errorf("SendToolResult flagged isError on success")
	}
	if tx.ToolCallCount != 1 {
		t.Errorf("ToolCallCount=%d", tx.ToolCallCount)
	}
	if len(invokes) != 1 || invokes[0].Name != "read" {
		t.Errorf("audit invokes = %+v", invokes)
	}
}

func TestLoop_RejectsDisallowedTool(t *testing.T) {
	gotResultErr := false
	hooks := LoopHooks{
		ExecuteTool: func(string, json.RawMessage) (string, error) {
			t.Fatal("ExecuteTool must not run for disallowed tool")
			return "", nil
		},
		SendToolResult: func(_, body string, isErr bool) error {
			gotResultErr = isErr
			if !strings.Contains(body, "bash") {
				t.Errorf("rejection body should reference disallowed name; got %q", body)
			}
			return nil
		},
	}
	tx, _ := runLoop(t, []LoopEvent{
		{Kind: EventCustomToolUse, ToolUseID: "tu-bad", ToolName: "bash", ToolInput: []byte(`{}`)},
	}, hooks, Budget{})

	if tx.StopReason != StopReasonError {
		t.Errorf("StopReason=%v, want error", tx.StopReason)
	}
	if tx.ErrorSummary != ErrDisallowedTool {
		t.Errorf("ErrorSummary=%v, want disallowed_tool", tx.ErrorSummary)
	}
	if !gotResultErr {
		t.Errorf("expected SendToolResult to be called with isError=true so the session knows we refused")
	}
}

func TestLoop_BudgetExhaustsOnTooManyToolCalls(t *testing.T) {
	hooks := LoopHooks{
		ExecuteTool:    func(string, json.RawMessage) (string, error) { return "ok", nil },
		SendToolResult: func(string, string, bool) error { return nil },
	}
	events := []LoopEvent{
		{Kind: EventCustomToolUse, ToolUseID: "1", ToolName: "read", ToolInput: []byte(`{}`)},
		{Kind: EventCustomToolUse, ToolUseID: "2", ToolName: "read", ToolInput: []byte(`{}`)},
		{Kind: EventCustomToolUse, ToolUseID: "3", ToolName: "read", ToolInput: []byte(`{}`)},
		{Kind: EventStatusIdle},
	}
	tx, _ := runLoop(t, events, hooks, Budget{MaxToolCalls: 2})
	if tx.StopReason != StopReasonMaxToolCalls {
		t.Errorf("StopReason=%v, want max_tool_calls", tx.StopReason)
	}
}

func TestLoop_TokenBudget(t *testing.T) {
	hooks := LoopHooks{}
	tx, _ := runLoop(t, []LoopEvent{
		{Kind: EventTokenUsage, OutputTokens: 5000},
		{Kind: EventTokenUsage, OutputTokens: 6000},
	}, hooks, Budget{MaxTokens: 10000})
	if tx.StopReason != StopReasonMaxTokens {
		t.Errorf("StopReason=%v, want max_tokens", tx.StopReason)
	}
}

func TestLoop_TimeoutOnDeadline(t *testing.T) {
	hooks := LoopHooks{}
	ch := make(chan LoopEvent) // never written
	allow := map[string]bool{"read": true}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	tx, _, err := runEventLoop(ctx, ch, hooks, Budget{}, allow)
	if err != nil {
		t.Fatalf("runEventLoop returned error: %v", err)
	}
	if tx.StopReason != StopReasonTimeout {
		t.Errorf("StopReason=%v, want timeout", tx.StopReason)
	}
}

func TestLoop_ShutdownOnCancel(t *testing.T) {
	hooks := LoopHooks{}
	ch := make(chan LoopEvent)
	allow := map[string]bool{"read": true}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	tx, _, _ := runEventLoop(ctx, ch, hooks, Budget{}, allow)
	if tx.StopReason != StopReasonShutdown {
		t.Errorf("StopReason=%v, want shutdown", tx.StopReason)
	}
}

func TestLoop_PropagatesSessionError(t *testing.T) {
	hooks := LoopHooks{}
	tx, _ := runLoop(t, []LoopEvent{
		{Kind: EventError, ErrorMsg: "HTTP 429: rate limit exceeded"},
	}, hooks, Budget{})
	if tx.StopReason != StopReasonError {
		t.Errorf("StopReason=%v, want error", tx.StopReason)
	}
	if tx.ErrorSummary != ErrAPIRateLimit {
		t.Errorf("ErrorSummary=%v, want api_rate_limited", tx.ErrorSummary)
	}
}

func TestLoop_SendToolResultErrorPropagates(t *testing.T) {
	hooks := LoopHooks{
		ExecuteTool:    func(string, json.RawMessage) (string, error) { return "ok", nil },
		SendToolResult: func(string, string, bool) error { return errors.New("network: dial timeout") },
	}
	tx, _ := runLoop(t, []LoopEvent{
		{Kind: EventCustomToolUse, ToolUseID: "1", ToolName: "read", ToolInput: []byte(`{}`)},
	}, hooks, Budget{})
	if tx.StopReason != StopReasonError {
		t.Errorf("StopReason=%v, want error", tx.StopReason)
	}
	if tx.ErrorSummary != ErrNetwork {
		t.Errorf("ErrorSummary=%v, want network", tx.ErrorSummary)
	}
}

func TestLoop_HandlerErrorReportsButContinues(t *testing.T) {
	first := true
	hooks := LoopHooks{
		ExecuteTool: func(name string, _ json.RawMessage) (string, error) {
			if first {
				first = false
				return "", errors.New("read: missing file_path")
			}
			return "ok", nil
		},
		SendToolResult: func(_, _ string, _ bool) error { return nil },
	}
	tx, _ := runLoop(t, []LoopEvent{
		{Kind: EventCustomToolUse, ToolUseID: "1", ToolName: "read", ToolInput: []byte(`{}`)},
		{Kind: EventCustomToolUse, ToolUseID: "2", ToolName: "read", ToolInput: []byte(`{"file_path":"x"}`)},
		{Kind: EventStatusIdle},
	}, hooks, Budget{})
	if tx.StopReason != StopReasonEndTurn {
		t.Errorf("StopReason=%v, want end_turn (handler errors should not stop the session)", tx.StopReason)
	}
	if tx.ToolCallCount != 2 {
		t.Errorf("ToolCallCount=%d, want 2", tx.ToolCallCount)
	}
}

func TestClassifyErrorMsg(t *testing.T) {
	cases := []struct {
		msg  string
		want ErrorSummary
	}{
		{"HTTP 401 unauthorized", ErrAPIAuth},
		{"rate limit exceeded", ErrAPIRateLimit},
		{"5" + "00 internal error", ErrAPIUnavailable},
		{"context deadline exceeded", ErrTimeout},
		{"dial tcp: connection refused", ErrNetwork},
		{"some unrelated error", ErrOther},
	}
	for _, c := range cases {
		if got := classifyErrorMsg(c.msg); got != c.want {
			t.Errorf("classifyErrorMsg(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestMapStopReasonHint(t *testing.T) {
	cases := map[string]StopReason{
		"":              StopReasonEndTurn,
		"end_turn":      StopReasonEndTurn,
		"max_tokens":    StopReasonMaxTokens,
		"timeout":       StopReasonTimeout,
		"weird_unknown": StopReasonEndTurn,
	}
	for hint, want := range cases {
		if got := mapStopReasonHint(hint); got != want {
			t.Errorf("mapStopReasonHint(%q) = %v, want %v", hint, got, want)
		}
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]bool{"glob": true, "read": true, "grep": true})
	want := []string{"glob", "grep", "read"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sortedKeys = %v, want %v", got, want)
	}
}

func TestBudget_Resolve(t *testing.T) {
	def := Budget{MaxTokens: 32768, MaxToolCalls: 20, Timeout: 2 * time.Minute}
	got := Budget{MaxTokens: 8192}.Resolve(def)
	if got.MaxTokens != 8192 || got.MaxToolCalls != 20 || got.Timeout != 2*time.Minute {
		t.Errorf("Resolve cascade wrong: %+v", got)
	}
	got = Budget{}.Resolve(def)
	if got != def {
		t.Errorf("zero-Budget should inherit all defaults, got %+v", got)
	}
}
