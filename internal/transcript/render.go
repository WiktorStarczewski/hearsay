package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxArgPreviewChars caps how much of a tool-call argument we inline into
// the markdown. Full payloads are fetched via read_tool_result.
const maxArgPreviewChars = 200

// RenderOptions is a half-open turn window [FromTurn, ToTurn). Zero values
// mean "start from the beginning" / "render to the end".
type RenderOptions struct {
	FromTurn int
	ToTurn   int // exclusive; 0 means "end"
}

// RenderResult is both the markdown output and the pagination metadata
// that feeds the tool's JSON return block.
type RenderResult struct {
	Markdown      string
	TotalTurns    int
	RenderedTurns int
	NextCursor    *int // nil when we've rendered through the end
}

// Render walks the event stream and returns markdown for the requested
// turn window plus pagination bookkeeping. It is the core of read_session
// and read_subagent.
func Render(events []Event, opts RenderOptions) RenderResult {
	var turns []*Event
	for i := range events {
		e := &events[i]
		if IsUserTurn(e) || IsAssistantTurn(e) {
			turns = append(turns, e)
		}
	}
	total := len(turns)

	from := opts.FromTurn
	if from < 0 {
		from = 0
	}
	to := opts.ToTurn
	if to <= 0 || to > total {
		to = total
	}
	if from > to {
		from = to
	}

	var sb strings.Builder
	for i := from; i < to; i++ {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(renderTurn(turns[i], i))
	}

	var nextCursor *int
	if to < total {
		n := to
		nextCursor = &n
	}

	return RenderResult{
		Markdown:      sb.String(),
		TotalTurns:    total,
		RenderedTurns: to - from,
		NextCursor:    nextCursor,
	}
}

func renderTurn(e *Event, idx int) string {
	ts := shortTime(e.Timestamp)
	var role string
	if IsUserTurn(e) {
		role = "User"
	} else {
		role = "Assistant"
	}
	header := fmt.Sprintf("### %s [%d]", role, idx)
	if ts != "" {
		header += " (" + ts + ")"
	}

	var body string
	if e.Message == nil {
		body = "_(empty)_"
	} else {
		body = renderContent(e.Message.Content)
	}
	return header + "\n\n" + body
}

// renderContent walks the Anthropic-shaped content array (or string) and
// produces a flat markdown block. Tool calls are collapsed to a single
// line; tool results are referenced by toolUseId (the actual content is
// fetched via read_tool_result to keep this output bounded).
func renderContent(content json.RawMessage) string {
	if len(content) == 0 {
		return "_(empty)_"
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "```\n" + string(content) + "\n```"
	}

	var parts []string
	for _, b := range blocks {
		t, _ := b["type"].(string)
		switch t {
		case "text":
			if txt, _ := b["text"].(string); txt != "" {
				parts = append(parts, txt)
			}
		case "tool_use":
			parts = append(parts, renderToolUse(b))
		case "tool_result":
			parts = append(parts, renderToolResult(b))
		case "thinking":
			txt, _ := b["thinking"].(string)
			parts = append(parts, "> _(thinking: "+truncate(txt, 200)+")_")
		default:
			parts = append(parts, "_(unknown content block: type="+t+")_")
		}
	}
	return strings.Join(parts, "\n\n")
}

func renderToolUse(b map[string]any) string {
	name, _ := b["name"].(string)
	id, _ := b["id"].(string)
	input := b["input"]

	// Agent subagent spawns get a distinctive render so read_subagent is
	// the obvious follow-up.
	if name == "Agent" {
		if inMap, ok := input.(map[string]any); ok {
			subType, _ := inMap["subagent_type"].(string)
			desc, _ := inMap["description"].(string)
			out := fmt.Sprintf("**→ %s subagent** `%s`", subType, id)
			if desc != "" {
				out += " — \"" + truncate(desc, 120) + "\""
			}
			return out
		}
	}

	preview := renderInputPreview(input)
	if preview != "" {
		return fmt.Sprintf("**Tool:** `%s` — %s `(id: %s)`", name, preview, id)
	}
	return fmt.Sprintf("**Tool:** `%s` `(id: %s)`", name, id)
}

func renderToolResult(b map[string]any) string {
	id, _ := b["tool_use_id"].(string)
	isErr, _ := b["is_error"].(bool)
	errTag := ""
	if isErr {
		errTag = " — error"
	}
	return fmt.Sprintf("_(tool_result for `%s`%s; fetch via `read_tool_result`)_", id, errTag)
}

func renderInputPreview(input any) string {
	if input == nil {
		return ""
	}
	if s, ok := input.(string); ok {
		return "`" + truncate(s, maxArgPreviewChars) + "`"
	}
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	// Lean on a few common argument shapes so the preview is readable.
	for _, key := range []string{"command", "file_path", "path", "pattern", "query"} {
		if v, ok := m[key].(string); ok && v != "" {
			return "`" + truncate(v, maxArgPreviewChars) + "`"
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return "`" + truncate(string(b), maxArgPreviewChars) + "`"
}

// RenderSystemLine produces a single-line summary for non-turn events
// (permission-mode, attachment, system). Used when callers want a
// full timeline rather than just turns.
func RenderSystemLine(e *Event) string {
	switch e.Type {
	case "permission-mode":
		return "**System:** permission-mode → " + e.PermissionMode
	case "system":
		sub := e.Subtype
		if sub == "" {
			sub = "event"
		}
		return "**System:** " + sub
	case "attachment":
		return "**System:** attachment"
	default:
		if !IsKnownEventType(e.Type) {
			return "**System (unknown type: " + e.Type + "):** _passed through by forward-compat handler_"
		}
		return "**System:** " + e.Type
	}
}

func shortTime(ts string) string {
	if len(ts) < 19 {
		return ""
	}
	return ts[11:19]
}
