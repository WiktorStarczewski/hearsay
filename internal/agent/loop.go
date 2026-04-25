package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LoopEvent is hearsay's narrowed view of the SDK session-event union.
// Only the variants the loop reacts to are modeled; the SDK's
// thinking / tool_confirmation / span events are translated to
// EventOther and ignored.  Translation happens in sdk.go.
type LoopEvent struct {
	Kind LoopEventKind

	// Populated when Kind == EventCustomToolUse.
	ToolUseID string
	ToolName  string
	ToolInput json.RawMessage // raw so we can reflect it back without losing fields

	// Populated when Kind == EventAgentMessage.
	MessageText string

	// Populated when Kind == EventStatusIdle.
	StopReasonHint string // SDK's stop_reason text — may be empty

	// Populated when Kind == EventError.
	ErrorMsg string

	// Populated when Kind == EventTokenUsage.
	InputTokens  int
	OutputTokens int
}

// LoopEventKind enumerates the events the loop reacts to.
type LoopEventKind int

const (
	EventOther LoopEventKind = iota
	EventCustomToolUse
	EventAgentMessage
	EventStatusIdle
	EventError
	EventTokenUsage
)

// LoopHooks injects the imperative side-effects: dispatching tool
// calls and sending tool results back.  In production these wrap SDK
// calls; in tests they wrap canned channels.
type LoopHooks struct {
	// ExecuteTool is called for every legal agent.custom_tool_use.
	// Returns the body to ship as the tool_result.
	ExecuteTool func(name string, input json.RawMessage) (string, error)

	// SendToolResult ships the tool_result back to the SDK session.
	// content is what ExecuteTool returned (or the rejection message
	// if the tool name wasn't on the allowlist).  isError flags
	// disallowed-tool / handler-error cases.
	SendToolResult func(toolUseID, content string, isError bool) error
}

// loopState carries the per-call accumulators used by runEventLoop.
type loopState struct {
	markdown      strings.Builder
	turns         int
	toolCalls     int
	inputTokens   int
	outputTokens  int
	toolInvokes   []AuditToolInvoke
	responseBytes int
}

// runEventLoop drains events until a terminal condition is reached,
// returning the assembled Transcript.  Decoupled from the SDK so the
// adversarial test (verification step 7) can drive it with canned
// events.
func runEventLoop(
	ctx context.Context,
	events <-chan LoopEvent,
	hooks LoopHooks,
	budget Budget,
	allow map[string]bool,
) (Transcript, []AuditToolInvoke, error) {
	start := time.Now()
	var st loopState

	stop := func(reason StopReason, errSummary ErrorSummary) Transcript {
		return Transcript{
			Markdown:      strings.TrimRight(st.markdown.String(), "\n") + "\n",
			TurnCount:     st.turns,
			ToolCallCount: st.toolCalls,
			StopReason:    reason,
			ElapsedMs:     time.Since(start).Milliseconds(),
			ErrorSummary:  errSummary,
		}
	}

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return stop(StopReasonTimeout, ""), st.toolInvokes, nil
			}
			return stop(StopReasonShutdown, ""), st.toolInvokes, nil

		case ev, ok := <-events:
			if !ok {
				// Stream closed without an explicit idle / error.
				// Treat as a benign end-of-turn for now.
				return stop(StopReasonEndTurn, ""), st.toolInvokes, nil
			}
			switch ev.Kind {

			case EventAgentMessage:
				st.turns++
				if ev.MessageText != "" {
					st.markdown.WriteString("## Assistant\n\n")
					st.markdown.WriteString(ev.MessageText)
					st.markdown.WriteString("\n\n")
					st.responseBytes += len(ev.MessageText)
				}

			case EventCustomToolUse:
				st.toolCalls++
				argBytes := len(ev.ToolInput)
				st.toolInvokes = append(st.toolInvokes, AuditToolInvoke{
					Name:     ev.ToolName,
					ArgBytes: argBytes,
				})
				st.markdown.WriteString(fmt.Sprintf("### Tool: `%s` (input %d bytes)\n\n", ev.ToolName, argBytes))

				if !allow[ev.ToolName] {
					// Adversarial defense: an upstream emitting
					// a tool_use for a name we never registered.
					// Send an error result so the session knows
					// we refused, then end the turn with an error.
					_ = hooks.SendToolResult(ev.ToolUseID,
						fmt.Sprintf("hearsay refused: tool %q is not in the allowlist (%v)",
							ev.ToolName, sortedKeys(allow)), true)
					st.markdown.WriteString(fmt.Sprintf(
						"_rejected: tool `%s` is not in the read-only allowlist_\n\n",
						ev.ToolName))
					return stop(StopReasonError, ErrDisallowedTool), st.toolInvokes, nil
				}

				if budget.MaxToolCalls > 0 && st.toolCalls > budget.MaxToolCalls {
					_ = hooks.SendToolResult(ev.ToolUseID,
						"hearsay refused: max_tool_calls budget exhausted", true)
					return stop(StopReasonMaxToolCalls, ""), st.toolInvokes, nil
				}

				body, err := hooks.ExecuteTool(ev.ToolName, ev.ToolInput)
				if err != nil {
					body = fmt.Sprintf("error: %s", err.Error())
					_ = hooks.SendToolResult(ev.ToolUseID, body, true)
					st.markdown.WriteString(fmt.Sprintf("_error: %s_\n\n", err.Error()))
					continue
				}
				if err := hooks.SendToolResult(ev.ToolUseID, body, false); err != nil {
					return stop(StopReasonError, ErrNetwork), st.toolInvokes, nil
				}
				preview := body
				if len(preview) > 240 {
					preview = preview[:240] + "…"
				}
				st.markdown.WriteString("```\n")
				st.markdown.WriteString(preview)
				st.markdown.WriteString("\n```\n\n")

			case EventTokenUsage:
				st.inputTokens += ev.InputTokens
				st.outputTokens += ev.OutputTokens
				if budget.MaxTokens > 0 && st.outputTokens >= budget.MaxTokens {
					return stop(StopReasonMaxTokens, ""), st.toolInvokes, nil
				}

			case EventStatusIdle:
				return stop(mapStopReasonHint(ev.StopReasonHint), ""), st.toolInvokes, nil

			case EventError:
				st.markdown.WriteString(fmt.Sprintf("_session error: %s_\n\n", ev.ErrorMsg))
				return stop(StopReasonError, classifyErrorMsg(ev.ErrorMsg)), st.toolInvokes, nil

			case EventOther:
				// Ignore — span events, thinking, tool_confirmation
				// (Phase-2 doesn't surface tool confirmations).
			}
		}
	}
}

// mapStopReasonHint converts the SDK's stop_reason text on a
// session.status_idle into our StopReason enum.  Unknown values
// degrade to EndTurn rather than Error — the session genuinely went
// idle, the categorization is just lossy.
func mapStopReasonHint(hint string) StopReason {
	switch strings.ToLower(strings.TrimSpace(hint)) {
	case "max_tokens":
		return StopReasonMaxTokens
	case "timeout":
		return StopReasonTimeout
	case "", "end_turn", "stop_sequence", "tool_use":
		return StopReasonEndTurn
	default:
		return StopReasonEndTurn
	}
}

// classifyErrorMsg maps a free-text upstream error into one of our
// coarse errorSummary categories.  Substring match — the SDK's
// errors carry the HTTP status text.
func classifyErrorMsg(msg string) ErrorSummary {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "429"):
		return ErrAPIRateLimit
	case strings.Contains(lower, "401"), strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "authentication"):
		return ErrAPIAuth
	case strings.Contains(lower, "5"+"00"), strings.Contains(lower, "5"+"02"),
		strings.Contains(lower, "5"+"03"), strings.Contains(lower, "5"+"04"),
		strings.Contains(lower, "service unavailable"), strings.Contains(lower, "internal error"):
		return ErrAPIUnavailable
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline exceeded"):
		return ErrTimeout
	case strings.Contains(lower, "connection"), strings.Contains(lower, "network"),
		strings.Contains(lower, "dial"), strings.Contains(lower, "tls"):
		return ErrNetwork
	default:
		return ErrOther
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Tiny set; insertion sort.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
