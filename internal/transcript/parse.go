// Package transcript reads Claude Code session JSONL files from
// ~/.claude/projects/ and renders them into markdown + metadata. It handles
// the three implementation gotchas documented in the hearsay plan:
//
//  1. tool_use.id → sidecar-filename mapping (the sidecar path is embedded
//     as a substring in the tool_result content text; we extract it with a
//     regex at lookup time rather than assuming filename equality).
//  2. Partial last line tolerance (active sessions may mid-write; an
//     unmarshal error on the final line is non-fatal).
//  3. Forward-compatible event type handling (unknown "type" values are
//     preserved as raw JSON and rendered by the system-line fallback path).
package transcript

import (
	"encoding/json"
	"os"
	"strings"
)

// Event is a permissive representation of a single JSONL line.
// We decode known fields strongly and keep the full raw line around for
// anything the rest of the pipeline might need (e.g. the rendering layer
// walks the message content blocks, which are parser-opaque here).
type Event struct {
	Type          string          `json:"type"`
	UUID          string          `json:"uuid,omitempty"`
	ParentUUID    string          `json:"parentUuid,omitempty"`
	Timestamp     string          `json:"timestamp,omitempty"`
	SessionID     string          `json:"sessionId,omitempty"`
	Cwd           string          `json:"cwd,omitempty"`
	GitBranch     string          `json:"gitBranch,omitempty"`
	Version       string          `json:"version,omitempty"`
	IsSidechain   bool            `json:"isSidechain,omitempty"`
	PermissionMode string         `json:"permissionMode,omitempty"`
	Subtype       string          `json:"subtype,omitempty"`

	// Message is the user/assistant message envelope. Content is kept as
	// a raw message because it's a heterogeneous array of blocks
	// (text | tool_use | tool_result | thinking | ...) that the renderer
	// walks block-by-block.
	Message *Message `json:"message,omitempty"`

	// ToolUseResult is a sibling of Message on tool_result events; we don't
	// model its fields because they vary wildly per tool.
	ToolUseResult json.RawMessage `json:"toolUseResult,omitempty"`

	// Raw is the original JSONL line, for rendering unknown event types
	// and for the sidecar-path extraction in subagent.go.
	Raw json.RawMessage `json:"-"`
}

// Message mirrors the Anthropic API shape: role + heterogeneous content.
type Message struct {
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

// ParseResult captures the output of parsing a session JSONL, including
// diagnostics needed by callers (partial tail, hard errors elsewhere).
type ParseResult struct {
	Events          []Event
	PartialLastLine bool
	ErrorCount      int
}

// KnownEventTypes lists the event types the renderer has first-class
// handling for. The parser never rejects unknown types — see flag #3.
var KnownEventTypes = map[string]struct{}{
	"user":                  {},
	"assistant":             {},
	"system":                {},
	"attachment":            {},
	"permission-mode":       {},
	"file-history-snapshot": {},
	"last-prompt":           {},
	"custom-title":          {},
	"agent-name":            {},
}

// ParseFile reads and parses a session JSONL file.
func ParseFile(path string) (*ParseResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseBytes(raw), nil
}

// ParseBytes parses a JSONL byte buffer.
func ParseBytes(raw []byte) *ParseResult {
	lines := strings.Split(string(raw), "\n")
	result := &ParseResult{}

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// Final-line tolerance: active sessions may be mid-write.
			isLast := i == len(lines)-1 ||
				(i == len(lines)-2 && strings.TrimSpace(lines[len(lines)-1]) == "")
			if isLast {
				result.PartialLastLine = true
			} else {
				result.ErrorCount++
			}
			continue
		}
		e.Raw = json.RawMessage(line)
		result.Events = append(result.Events, e)
	}
	return result
}

// IsKnownEventType reports whether the parser has first-class handling
// for a given type. Unknown types are still preserved in the event stream
// and rendered via a generic system-line fallback.
func IsKnownEventType(t string) bool {
	_, ok := KnownEventTypes[t]
	return ok
}

// IsUserTurn reports whether an event represents a top-level user turn
// (not a sidechain from a subagent call-out).
func IsUserTurn(e *Event) bool {
	return e.Type == "user" && !e.IsSidechain
}

// IsAssistantTurn reports whether an event is a top-level assistant turn.
func IsAssistantTurn(e *Event) bool {
	return e.Type == "assistant" && !e.IsSidechain
}

// CountTurns returns the number of user + assistant turns (what the user
// would see as "turns" in a transcript). Used for the totalTurns field.
func CountTurns(events []Event) int {
	n := 0
	for i := range events {
		if IsUserTurn(&events[i]) || IsAssistantTurn(&events[i]) {
			n++
		}
	}
	return n
}

// FirstUserMessage returns a short preview of the first user message in
// the session, used for disambiguation in list_sessions output.
func FirstUserMessage(events []Event, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 140
	}
	for i := range events {
		e := &events[i]
		if !IsUserTurn(e) || e.Message == nil {
			continue
		}
		return firstTextFromContent(e.Message.Content, maxChars)
	}
	return ""
}

// firstTextFromContent pulls the first textual fragment out of a content
// field. Content is either a raw string or an array of blocks.
func firstTextFromContent(content json.RawMessage, maxChars int) string {
	if len(content) == 0 {
		return ""
	}
	// Try plain string first.
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return truncate(s, maxChars)
	}
	// Otherwise, it's an array of blocks.
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if t, _ := b["type"].(string); t == "text" {
			if txt, _ := b["text"].(string); txt != "" {
				return truncate(txt, maxChars)
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	compact := strings.Join(strings.Fields(s), " ")
	if len(compact) <= n {
		return compact
	}
	return compact[:n-1] + "…"
}
