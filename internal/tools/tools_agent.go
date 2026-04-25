package tools

import (
	"context"
	"fmt"
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

// AskPeerClaudeOutput is the structured-content metadata that
// accompanies the markdown transcript.
type AskPeerClaudeOutput struct {
	TurnCount     int    `json:"turnCount"`
	ToolCallCount int    `json:"toolCallCount"`
	StopReason    string `json:"stopReason"`
	ElapsedMs     int64  `json:"elapsedMs"`
	ErrorSummary  string `json:"errorSummary,omitempty"`
}

func addAskPeerClaude(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"Spawns a NEW parallel Claude session on %s's machine with read-only filesystem tools (read / glob / grep) "+
			"and asks it `prompt`. Use this when you need %s's filesystem inspected RIGHT NOW — running fresh queries "+
			"against fresh data. This is NOT the Claude session %s is currently typing into; it cannot send messages "+
			"to %s and they won't see it. To replay what %s already did (read past transcripts), use list_sessions / "+
			"read_session instead. Returns a markdown transcript plus {turnCount, toolCallCount, stopReason, elapsedMs}.",
		peer, peer, peer, peer, peer,
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

			out := AskPeerClaudeOutput{
				TurnCount:     tx.TurnCount,
				ToolCallCount: tx.ToolCallCount,
				StopReason:    string(tx.StopReason),
				ElapsedMs:     tx.ElapsedMs,
				ErrorSummary:  string(tx.ErrorSummary),
			}
			result := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: tx.Markdown}},
				IsError: tx.StopReason == agent.StopReasonError,
			}
			return result, out, nil
		}))
}
