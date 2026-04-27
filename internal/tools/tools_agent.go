package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/WiktorStarczewski/hearsay/internal/agent"
)

// AskPeerClaudeInput is the wire shape the calling Claude sees.  All
// budget fields are optional and cascade through call → server defaults.
type AskPeerClaudeInput struct {
	Prompt         string `json:"prompt" jsonschema:"the question or instruction to send to the peer's Claude"`
	Project        string `json:"project,omitempty" jsonschema:"working directory the agent's tools see (default: hearsay's cwd)"`
	MaxTokens      int    `json:"max_tokens,omitempty"`
	MaxToolCalls   int    `json:"max_tool_calls,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// AskPeerClaudeOutput is intentionally empty.
//
// The earlier shape carried turnCount / toolCallCount / stopReason /
// elapsedMs / errorSummary in StructuredContent alongside the markdown
// transcript in Content. Some MCP consumers (notably Claude Code) surface
// only the structured channel back to the calling model when both are
// populated, dropping the markdown body — the calling Claude sees the
// metadata footer but not the response. Mirroring the precedent set by
// `read_tool_result`, the body is now the unconditional source of truth:
// the metadata is inlined as a leading header line in the markdown,
//
//	[turns=N toolCalls=N stopReason=X elapsedMs=N]
//
//	<actual response>
//
// so it survives whichever channel the consumer prefers.
type AskPeerClaudeOutput struct{}

// transcriptHeaderLine renders the per-call metadata that used to ride
// in StructuredContent into a single bracketed line. Keys mirror the old
// JSON field names so callers parsing the transcript pick up the same
// values without a translation table.
func transcriptHeaderLine(tx agent.Transcript) string {
	parts := []string{
		fmt.Sprintf("turns=%d", tx.TurnCount),
		fmt.Sprintf("toolCalls=%d", tx.ToolCallCount),
		fmt.Sprintf("stopReason=%s", tx.StopReason),
		fmt.Sprintf("elapsedMs=%d", tx.ElapsedMs),
	}
	if tx.ErrorSummary != "" {
		parts = append(parts, fmt.Sprintf("errorSummary=%s", tx.ErrorSummary))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// withTranscriptHeader prepends transcriptHeaderLine + blank line to the
// model's body. If the body is empty (e.g. the model emitted no text
// because output_format=json swallowed it), the header alone still tells
// the caller the call happened — preferable to a totally empty Content.
func withTranscriptHeader(tx agent.Transcript) string {
	header := transcriptHeaderLine(tx)
	if tx.Markdown == "" {
		return header + "\n"
	}
	return header + "\n\n" + tx.Markdown
}

func addAskPeerClaude(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"Spawns a NEW parallel Claude Code subprocess on %s's machine with read-only filesystem tools (Read / Glob / Grep) "+
			"and asks it `prompt`. The call bills against %s's Claude Code subscription quota. Use this when you need %s's "+
			"filesystem inspected RIGHT NOW — running fresh queries against fresh data. This is NOT the Claude session "+
			"%s is currently typing into; it cannot send messages to %s and they won't see it. To replay what %s already "+
			"did (read past transcripts), use list_sessions / read_session instead. Returns a markdown transcript plus "+
			"{turnCount, toolCallCount, stopReason, elapsedMs}.",
		peer, peer, peer, peer, peer, peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "ask_peer_claude", Description: desc},
		trace(ctx, "ask_peer_claude", func(c context.Context, _ *mcp.CallToolRequest, in AskPeerClaudeInput) (*mcp.CallToolResult, AskPeerClaudeOutput, error) {
			if in.Prompt == "" {
				return errResult("ask_peer_claude: prompt is required"), AskPeerClaudeOutput{}, nil
			}

			req := agent.OneShotRequest{
				Prompt:  in.Prompt,
				Project: in.Project,
				Budget: agent.Budget{
					MaxTokens:    in.MaxTokens,
					MaxToolCalls: in.MaxToolCalls,
					Timeout:      time.Duration(in.TimeoutSeconds) * time.Second,
				},
			}
			tx, err := ctx.Agent.OneShot(c, req)
			if err != nil {
				return errResult("ask_peer_claude: " + err.Error()), AskPeerClaudeOutput{}, nil
			}

			result := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: withTranscriptHeader(tx)}},
				IsError: tx.StopReason == agent.StopReasonError,
			}
			return result, AskPeerClaudeOutput{}, nil
		}))
}

// ---------------- start_peer_conversation ----------------

type StartPeerConversationInput struct {
	SystemPrompt   string `json:"system_prompt,omitempty" jsonschema:"optional system prompt seeded on the conversation"`
	Project        string `json:"project,omitempty" jsonschema:"working directory for the agent's tools (default: hearsay's cwd)"`
	MaxTokens      int    `json:"max_tokens,omitempty"`
	MaxToolCalls   int    `json:"max_tool_calls,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// EffectiveBudgetOutput echoes the resolved budget back so callers
// know what they're inheriting before deciding to override per turn.
type EffectiveBudgetOutput struct {
	MaxTokens      int   `json:"maxTokens"`
	MaxToolCalls   int   `json:"maxToolCalls"`
	TimeoutSeconds int64 `json:"timeoutSeconds"`
}

type StartPeerConversationOutput struct {
	ConvID          string                `json:"convId"`
	StartedAt       time.Time             `json:"startedAt"`
	EffectiveBudget EffectiveBudgetOutput `json:"effectiveBudget"`
}

func addStartPeerConversation(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"Starts a stateful read-only conversation backed by a Claude Code subprocess on %s's machine, separate from "+
			"any session %s is currently typing into. Bills against %s's Claude Code subscription quota. Returns "+
			"{convId, startedAt, effectiveBudget}; pass convId to send_peer_message for follow-ups so context isn't "+
			"re-paid each turn. Use ask_peer_claude for one-off questions; use this when you expect 2+ follow-up turns.",
		peer, peer, peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "start_peer_conversation", Description: desc},
		trace(ctx, "start_peer_conversation", func(c context.Context, _ *mcp.CallToolRequest, in StartPeerConversationInput) (*mcp.CallToolResult, StartPeerConversationOutput, error) {
			req := agent.StartReq{
				SystemPrompt: in.SystemPrompt,
				Project:      in.Project,
				Budget: agent.Budget{
					MaxTokens:    in.MaxTokens,
					MaxToolCalls: in.MaxToolCalls,
					Timeout:      time.Duration(in.TimeoutSeconds) * time.Second,
				},
			}
			convID, startedAt, effective, err := ctx.Agent.StartConversation(c, req)
			if err != nil {
				if errors.Is(err, agent.ErrConvCap) {
					return errResult("max concurrent conversations reached on this peer; call end_peer_conversation on an idle one or wait for idle reap"),
						StartPeerConversationOutput{}, nil
				}
				return errResult("start_peer_conversation: " + err.Error()), StartPeerConversationOutput{}, nil
			}
			return nil, StartPeerConversationOutput{
				ConvID:    string(convID),
				StartedAt: startedAt,
				EffectiveBudget: EffectiveBudgetOutput{
					MaxTokens:      effective.MaxTokens,
					MaxToolCalls:   effective.MaxToolCalls,
					TimeoutSeconds: int64(effective.Timeout.Seconds()),
				},
			}, nil
		}))
}

// ---------------- send_peer_message ----------------

type SendPeerMessageInput struct {
	ConvID         string `json:"convId" jsonschema:"the conversation handle returned by start_peer_conversation"`
	Prompt         string `json:"prompt" jsonschema:"the next user message in the conversation"`
	MaxTokens      int    `json:"max_tokens,omitempty"`
	MaxToolCalls   int    `json:"max_tool_calls,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// SendPeerMessageOutput is intentionally empty for the same reason as
// [AskPeerClaudeOutput] — see that comment for the rationale. The
// per-turn metadata is inlined as a header line in the markdown.
type SendPeerMessageOutput struct{}

func addSendPeerMessage(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"Sends one more turn to a conversation previously created with start_peer_conversation on %s's machine. "+
			"Per-turn budget args (max_tokens / max_tool_calls / timeout_seconds) override the conversation's defaults "+
			"for just this turn. Returns the turn's markdown plus {turnCount, toolCallCount, stopReason, elapsedMs}.",
		peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "send_peer_message", Description: desc},
		trace(ctx, "send_peer_message", func(c context.Context, _ *mcp.CallToolRequest, in SendPeerMessageInput) (*mcp.CallToolResult, SendPeerMessageOutput, error) {
			if in.ConvID == "" {
				return errResult("send_peer_message: convId is required"), SendPeerMessageOutput{}, nil
			}
			if in.Prompt == "" {
				return errResult("send_peer_message: prompt is required"), SendPeerMessageOutput{}, nil
			}

			budget := agent.Budget{
				MaxTokens:    in.MaxTokens,
				MaxToolCalls: in.MaxToolCalls,
				Timeout:      time.Duration(in.TimeoutSeconds) * time.Second,
			}
			tx, err := ctx.Agent.SendMessage(c, agent.ConvID(in.ConvID), in.Prompt, budget)
			if err != nil {
				switch {
				case errors.Is(err, agent.ErrConvReaped):
					return errResult("conversation id was reaped after idle timeout; start a new one with start_peer_conversation"),
						SendPeerMessageOutput{}, nil
				case errors.Is(err, agent.ErrUnknownConv):
					return errResult("unknown convId: " + in.ConvID), SendPeerMessageOutput{}, nil
				default:
					return errResult("send_peer_message: " + err.Error()), SendPeerMessageOutput{}, nil
				}
			}

			result := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: withTranscriptHeader(tx)}},
				IsError: tx.StopReason == agent.StopReasonError,
			}
			return result, SendPeerMessageOutput{}, nil
		}))
}

// ---------------- list_peer_conversations ----------------

type ListPeerConversationsInput struct{}

type PeerConversationSummary struct {
	ConvID         string    `json:"convId"`
	StartedAt      time.Time `json:"startedAt"`
	LastActivityAt time.Time `json:"lastActivityAt"`
	TurnCount      int       `json:"turnCount"`
	Preview        string    `json:"preview"`
}

type ListPeerConversationsOutput struct {
	Conversations []PeerConversationSummary `json:"conversations"`
}

func addListPeerConversations(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"List active hearsay-managed conversations on %s's machine, sorted by lastActivityAt desc. Each entry "+
			"carries the convId you'd pass to send_peer_message or end_peer_conversation, plus a 140-rune preview "+
			"of the first user message (or system_prompt fallback when no turn has happened yet).",
		peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "list_peer_conversations", Description: desc},
		trace(ctx, "list_peer_conversations", func(_ context.Context, _ *mcp.CallToolRequest, _ ListPeerConversationsInput) (*mcp.CallToolResult, ListPeerConversationsOutput, error) {
			metas := ctx.Agent.ListConversations()
			out := ListPeerConversationsOutput{
				Conversations: make([]PeerConversationSummary, 0, len(metas)),
			}
			for _, m := range metas {
				out.Conversations = append(out.Conversations, PeerConversationSummary{
					ConvID:         string(m.ConvID),
					StartedAt:      m.StartedAt,
					LastActivityAt: m.LastActivityAt,
					TurnCount:      m.TurnCount,
					Preview:        m.Preview,
				})
			}
			return nil, out, nil
		}))
}

// ---------------- end_peer_conversation ----------------

type EndPeerConversationInput struct {
	ConvID string `json:"convId"`
}

type EndPeerConversationOutput struct {
	Ended        bool   `json:"ended"`
	AlreadyEnded bool   `json:"alreadyEnded"`
	TotalTurns   int    `json:"totalTurns"`
	EndedReason  string `json:"endedReason"`
}

func addEndPeerConversation(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"End a hearsay-managed conversation on %s's machine and free its slot.  Idempotent: a second end on an "+
			"already-ended conv returns ended:true, alreadyEnded:true.  Always pass an explicit end when you're "+
			"done — otherwise the conversation lingers until the idle reaper takes it.",
		peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "end_peer_conversation", Description: desc},
		trace(ctx, "end_peer_conversation", func(c context.Context, _ *mcp.CallToolRequest, in EndPeerConversationInput) (*mcp.CallToolResult, EndPeerConversationOutput, error) {
			if in.ConvID == "" {
				return errResult("end_peer_conversation: convId is required"), EndPeerConversationOutput{}, nil
			}
			summary, err := ctx.Agent.EndConversation(c, agent.ConvID(in.ConvID), agent.EndedByCaller)
			if err != nil {
				if errors.Is(err, agent.ErrUnknownConv) {
					return errResult("unknown conversation id"), EndPeerConversationOutput{}, nil
				}
				return errResult("end_peer_conversation: " + err.Error()), EndPeerConversationOutput{}, nil
			}
			return nil, EndPeerConversationOutput{
				Ended:        summary.Ended,
				AlreadyEnded: summary.AlreadyEnded,
				TotalTurns:   summary.TotalTurns,
				EndedReason:  string(summary.EndedReason),
			}, nil
		}))
}
