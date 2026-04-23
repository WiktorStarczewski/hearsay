package transcript

import (
	"encoding/json"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------
// renderContent branches: empty, string, block array (text / tool_use /
// tool_result / thinking / unknown), and the non-array JSON fallback.

func TestRenderContent_EmptyAndString(t *testing.T) {
	if got := renderContent(nil); got != "_(empty)_" {
		t.Errorf("renderContent(nil) = %q", got)
	}
	if got := renderContent(json.RawMessage(`"plain string"`)); got != "plain string" {
		t.Errorf("renderContent(string) = %q", got)
	}
}

func TestRenderContent_BlockArrayWithAllTypes(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"hello"},
		{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"npm test"}},
		{"type":"tool_result","tool_use_id":"toolu_1","is_error":true},
		{"type":"thinking","thinking":"hmmm"},
		{"type":"something-new","payload":"x"}
	]`)
	got := renderContent(raw)
	for _, want := range []string{
		"hello",
		"**Tool:** `Bash`",
		"npm test",
		"tool_result for `toolu_1`",
		"error",
		"thinking: hmmm",
		"unknown content block",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in rendered content, got:\n%s", want, got)
		}
	}
}

func TestRenderContent_UnparseableFallback(t *testing.T) {
	// Not a string, not a valid array → falls through to code-fenced
	// raw JSON.
	raw := json.RawMessage(`{"this":"is an object","not":"expected"}`)
	got := renderContent(raw)
	if !strings.Contains(got, "```") {
		t.Errorf("expected code fence fallback, got %q", got)
	}
}

// -----------------------------------------------------------------------
// renderInputPreview branches: nil, string, known keys, fallback JSON.

func TestRenderInputPreview(t *testing.T) {
	if got := renderInputPreview(nil); got != "" {
		t.Errorf("nil input should produce empty preview, got %q", got)
	}
	if got := renderInputPreview("some string"); !strings.Contains(got, "some string") {
		t.Errorf("string input preview missing text: %q", got)
	}
	// Each known key lane (command, file_path, path, pattern, query).
	for _, key := range []string{"command", "file_path", "path", "pattern", "query"} {
		in := map[string]any{key: "val-" + key}
		got := renderInputPreview(in)
		if !strings.Contains(got, "val-"+key) {
			t.Errorf("key %q not surfaced: %q", key, got)
		}
	}
	// Fallback to JSON shape for an unrecognized map.
	fallback := renderInputPreview(map[string]any{"weird": "structure"})
	if !strings.Contains(fallback, "weird") {
		t.Errorf("fallback JSON preview missing key: %q", fallback)
	}
	// Non-string, non-map, non-nil input returns "".
	if got := renderInputPreview(42); got != "" {
		t.Errorf("int input should yield empty preview, got %q", got)
	}
}

// -----------------------------------------------------------------------
// RenderSystemLine branches.

func TestRenderSystemLine_AllCases(t *testing.T) {
	cases := []struct {
		name  string
		event Event
		want  string
	}{
		{"permission-mode", Event{Type: "permission-mode", PermissionMode: "default"}, "permission-mode → default"},
		{"system with subtype", Event{Type: "system", Subtype: "bridge_status"}, "bridge_status"},
		{"system without subtype", Event{Type: "system"}, "event"},
		{"attachment", Event{Type: "attachment"}, "attachment"},
		{"known-but-generic", Event{Type: "file-history-snapshot"}, "file-history-snapshot"},
		{"unknown type", Event{Type: "mystery-event-type"}, "unknown type: mystery-event-type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RenderSystemLine(&c.event)
			if !strings.Contains(got, c.want) {
				t.Errorf("RenderSystemLine(%s): want substring %q, got %q", c.name, c.want, got)
			}
		})
	}
}

// -----------------------------------------------------------------------
// shortTime branches.

func TestShortTime(t *testing.T) {
	if got := shortTime("2026-04-24T10:20:30.000Z"); got != "10:20:30" {
		t.Errorf("shortTime(full) = %q", got)
	}
	if got := shortTime(""); got != "" {
		t.Errorf("shortTime(empty) should be empty")
	}
	if got := shortTime("short"); got != "" {
		t.Errorf("shortTime(too short) should be empty, got %q", got)
	}
}

// -----------------------------------------------------------------------
// Render pagination: exercise fromTurn/toTurn edge cases that the
// original fixture test didn't cover (negative fromTurn, toTurn > total,
// fromTurn > toTurn clamping, empty event list).

func TestRender_PaginationEdgeCases(t *testing.T) {
	events := []Event{}
	rr := Render(events, RenderOptions{})
	if rr.TotalTurns != 0 || rr.RenderedTurns != 0 || rr.NextCursor != nil {
		t.Errorf("empty event list should produce empty render, got %+v", rr)
	}

	// Single user turn + single assistant turn.
	result, _ := ParseFile("../../testdata/fixtures/mini-session.jsonl")
	// Negative fromTurn is clamped to 0.
	rr = Render(result.Events, RenderOptions{FromTurn: -5, ToTurn: 2})
	if rr.RenderedTurns != 2 {
		t.Errorf("negative fromTurn should clamp to 0; got renderedTurns=%d", rr.RenderedTurns)
	}

	// toTurn > total is clamped to total; nextCursor stays nil.
	total := rr.TotalTurns
	rr = Render(result.Events, RenderOptions{ToTurn: total + 99})
	if rr.NextCursor != nil {
		t.Errorf("nextCursor should be nil when rendering through end")
	}

	// fromTurn > toTurn is clamped so we don't render anything but
	// don't panic either.
	rr = Render(result.Events, RenderOptions{FromTurn: total, ToTurn: 1})
	if rr.RenderedTurns != 0 {
		t.Errorf("clamped fromTurn should render zero turns")
	}

	// Partial window should yield a non-nil nextCursor.
	rr = Render(result.Events, RenderOptions{FromTurn: 0, ToTurn: 1})
	if rr.NextCursor == nil {
		t.Errorf("partial window should produce a nextCursor")
	}
}

// -----------------------------------------------------------------------
// FirstUserMessage / firstTextFromContent gap filler.

func TestFirstUserMessage_BlockArrayContent(t *testing.T) {
	events := []Event{
		{
			Type: "user",
			Message: &Message{
				Role: "user",
				Content: json.RawMessage(`[
					{"type":"text","text":"from block array"}
				]`),
			},
		},
	}
	got := FirstUserMessage(events, 100)
	if got != "from block array" {
		t.Errorf("FirstUserMessage(block) = %q", got)
	}
}

func TestFirstUserMessage_DefaultMaxChars(t *testing.T) {
	events := []Event{{Type: "user", Message: &Message{Content: json.RawMessage(`"hi"`)}}}
	// maxChars <= 0 → default 140.
	if got := FirstUserMessage(events, 0); got != "hi" {
		t.Errorf("default maxChars branch = %q", got)
	}
}

func TestFirstUserMessage_NoUserEvents(t *testing.T) {
	events := []Event{{Type: "assistant"}}
	if got := FirstUserMessage(events, 100); got != "" {
		t.Errorf("no user turns should yield empty")
	}
}

func TestFirstUserMessage_ContentWithoutTextBlock(t *testing.T) {
	events := []Event{{
		Type: "user",
		Message: &Message{
			Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"x"}]`),
		},
	}}
	if got := FirstUserMessage(events, 100); got != "" {
		t.Errorf("no text block should yield empty, got %q", got)
	}
}

func TestFirstTextFromContent_Unparseable(t *testing.T) {
	if got := firstTextFromContent(json.RawMessage(`garbage`), 10); got != "" {
		t.Errorf("unparseable content should yield empty")
	}
	if got := firstTextFromContent(nil, 10); got != "" {
		t.Errorf("nil content should yield empty")
	}
}

// -----------------------------------------------------------------------
// ParseFile error branch (non-existent file).

func TestParseFile_MissingFileErrors(t *testing.T) {
	if _, err := ParseFile("/nonexistent-path.jsonl"); err == nil {
		t.Errorf("expected error for missing file")
	}
}

// -----------------------------------------------------------------------
// truncate: short-input branch (already covered by the happy path) plus
// a multi-space-normalization check.

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("truncate(abc,10) = %q", got)
	}
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Errorf("truncate(abcdef,4) = %q", got)
	}
	if got := truncate("  lots   of    spaces  ", 30); got != "lots of spaces" {
		t.Errorf("truncate should normalize spaces; got %q", got)
	}
}

// -----------------------------------------------------------------------
// renderTurn defensive branch: nil Message → "_(empty)_" body.

func TestRenderTurn_NilMessage(t *testing.T) {
	e := Event{Type: "user", Timestamp: "2026-04-24T10:00:00Z"}
	out := renderTurn(&e, 0)
	if !strings.Contains(out, "_(empty)_") {
		t.Errorf("nil Message should render empty placeholder, got %q", out)
	}
}