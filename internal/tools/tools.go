// Package tools registers the eight MCP tools hearsay exposes. Each tool
// bakes the peer's --name into its description at registration time so
// auto-routing works ("Ivan reported X" → Ivan's server). The ambiguity
// contract on get_current_session is also spelled out in the description
// so a consuming Claude doesn't need an external CLAUDE.md block to
// behave correctly on multi-live-session peers.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/WiktorStarczewski/hearsay/internal/agent"
	"github.com/WiktorStarczewski/hearsay/internal/transcript"
)

// Context bundles the settings every tool handler needs. It's built once
// in main.go and passed into Register.
type Context struct {
	PeerName    string
	PeerVersion string
	DataDir     string
	LiveWindow  time.Duration
	Log         func(tool, status string, dur time.Duration)

	// Agent is non-nil only when --enable-agent was passed; nil
	// keeps Phase-1 behavior (the 8 read-only tools, no ask_peer_claude).
	Agent agent.Agent
}

// Register wires the always-on Phase-1 tools onto an already-
// constructed mcp.Server, plus any Phase-2 tools whose dependencies
// are present in ctx.
func Register(s *mcp.Server, ctx Context) {
	addListSessions(s, ctx)
	addGetCurrentSession(s, ctx)
	addReadSession(s, ctx)
	addSearchSession(s, ctx)
	addReadSubagent(s, ctx)
	addReadToolResult(s, ctx)
	addGetSessionSummary(s, ctx)
	addGetPeerInfo(s, ctx)

	if ctx.Agent != nil {
		addAskPeerClaude(s, ctx)
		addStartPeerConversation(s, ctx)
		addSendPeerMessage(s, ctx)
		addListPeerConversations(s, ctx)
		addEndPeerConversation(s, ctx)
	}
}

// capName converts "wiktor" → "Wiktor" for display in tool descriptions.
func capName(name string) string {
	if name == "" {
		return ""
	}
	r := []rune(name)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// trace wraps a handler with tool-call logging (no content, just name + status).
func trace[In, Out any](ctx Context, name string, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(c context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		result, out, err := h(c, req, in)
		status := "ok"
		if err != nil || (result != nil && result.IsError) {
			status = "error"
		}
		if ctx.Log != nil {
			ctx.Log(name, status, time.Since(start))
		}
		return result, out, err
	}
}

// errResult builds a CallToolResult flagged as a tool-level error (per the
// plan's error contract: isError:true content block rather than a protocol
// error for things like "sessionId not found").
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// ---------------- list_sessions ----------------

// ListSessionsInput is the tool argument type. Struct tags drive both the
// JSON schema presented to MCP clients and the jsonschema-inferred docs.
type ListSessionsInput struct {
	Project string `json:"project,omitempty" jsonschema:"short project name (substring match on the decoded cwd) or full path"`
	Since   string `json:"since,omitempty" jsonschema:"ISO8601 — only return sessions with lastActivityAt >= since"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max results (default 20)"`
}

type ListSessionsOutput struct {
	Sessions []transcript.SessionSummary `json:"sessions"`
}

func addListSessions(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"List %s's recent Claude Code session transcripts. Use this when the user asks about something %s did, "+
			"reported, or found during a test run, or wants to read %s's ongoing or recent session. Returns session "+
			"IDs, first user prompts, and timestamps sorted by most recent activity; 'isLive: true' indicates a "+
			"session written within the live window (default 5 min). Pick the right session before calling read_session.",
		peer, peer, peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "list_sessions", Description: desc},
		trace(ctx, "list_sessions", func(_ context.Context, _ *mcp.CallToolRequest, in ListSessionsInput) (*mcp.CallToolResult, ListSessionsOutput, error) {
			opts := transcript.LocateOptions{
				DataDir:    ctx.DataDir,
				LiveWindow: ctx.LiveWindow,
				Project:    in.Project,
				Limit:      in.Limit,
			}
			if opts.Limit == 0 {
				opts.Limit = 20
			}
			if in.Since != "" {
				if t, err := time.Parse(time.RFC3339, in.Since); err == nil {
					opts.Since = t
				}
			}
			sessions := transcript.ListSessions(opts)
			if sessions == nil {
				sessions = []transcript.SessionSummary{}
			}
			return nil, ListSessionsOutput{Sessions: sessions}, nil
		}))
}

// ---------------- get_current_session ----------------

type GetCurrentSessionInput struct {
	Project string `json:"project,omitempty" jsonschema:"optional project filter (same semantics as list_sessions.project)"`
}

// GetCurrentSessionOutput honors the ambiguity contract from the plan:
//   - exactly one live session: {ambiguous: false, session: <it>}
//   - no live session: {ambiguous: false, session: null}
//   - multiple live sessions: {ambiguous: true, candidates: [...]}
// The calling Claude is instructed (via the tool description) to ASK
// the user when ambiguous=true rather than silently pick one.
type GetCurrentSessionOutput struct {
	Ambiguous  bool                          `json:"ambiguous"`
	Session    *transcript.SessionSummary    `json:"session"`
	Candidates []transcript.SessionSummary   `json:"candidates,omitempty"`
}

func addGetCurrentSession(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"Return the Claude Code session %s is working in right now. Prefer this over list_sessions when the user "+
			"asks \"what's %s doing?\" or \"what did %s just report?\". Returns {ambiguous:false, session} in the "+
			"unambiguous case, {ambiguous:false, session:null} if nothing has been touched in the last 5 min, or "+
			"{ambiguous:true, candidates} if multiple sessions are live. If ambiguous is true, show the user the "+
			"firstUserMessage previews and ASK which session — do not silently pick one.",
		peer, peer, peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "get_current_session", Description: desc},
		trace(ctx, "get_current_session", func(_ context.Context, _ *mcp.CallToolRequest, in GetCurrentSessionInput) (*mcp.CallToolResult, GetCurrentSessionOutput, error) {
			sessions := transcript.ListSessions(transcript.LocateOptions{
				DataDir:    ctx.DataDir,
				LiveWindow: ctx.LiveWindow,
				Project:    in.Project,
			})
			var live []transcript.SessionSummary
			for _, sess := range sessions {
				if sess.IsLive {
					live = append(live, sess)
				}
			}
			switch len(live) {
			case 0:
				return nil, GetCurrentSessionOutput{Ambiguous: false, Session: nil}, nil
			case 1:
				return nil, GetCurrentSessionOutput{Ambiguous: false, Session: &live[0]}, nil
			default:
				return nil, GetCurrentSessionOutput{Ambiguous: true, Session: nil, Candidates: live}, nil
			}
		}))
}

// ---------------- read_session ----------------

type ReadSessionInput struct {
	SessionID string `json:"sessionId" jsonschema:"session UUID from list_sessions or get_current_session"`
	FromTurn  int    `json:"fromTurn,omitempty" jsonschema:"inclusive start turn (default 0)"`
	ToTurn    int    `json:"toTurn,omitempty" jsonschema:"exclusive end turn (0 means end)"`
	Format    string `json:"format,omitempty" jsonschema:"'full' (default) returns markdown + JSON metadata; 'json' returns metadata only"`
}

type ReadSessionOutput struct {
	// Body holds the rendered markdown for format="full" (the default);
	// it is omitted for format="json".  See the AskPeerClaudeOutput
	// docstring for why the body lives in StructuredContent rather than
	// only in Content — Claude Code-class clients render the structured
	// channel and silently drop the Content channel when both exist.
	Body            string `json:"body,omitempty"`
	SessionID       string `json:"sessionId"`
	TotalTurns      int    `json:"totalTurns"`
	RenderedTurns   int    `json:"renderedTurns"`
	NextCursor      *int   `json:"nextCursor,omitempty"`
	PartialLastLine bool   `json:"partialLastLine"`
}

func addReadSession(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"Read a window of turns from one of %s's Claude Code sessions. Default format 'full' returns markdown plus "+
			"a JSON metadata block ({totalTurns, renderedTurns, nextCursor, partialLastLine}). 'json' returns only "+
			"the metadata.",
		peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "read_session", Description: desc},
		trace(ctx, "read_session", func(_ context.Context, _ *mcp.CallToolRequest, in ReadSessionInput) (*mcp.CallToolResult, ReadSessionOutput, error) {
			path := transcript.FindSessionPath(in.SessionID, ctx.DataDir)
			if path == "" {
				return errResult("sessionId not found: " + in.SessionID), ReadSessionOutput{}, nil
			}
			parsed, err := transcript.ParseFile(path)
			if err != nil {
				return errResult("parse error: " + err.Error()), ReadSessionOutput{}, nil
			}
			rendered := transcript.Render(parsed.Events, transcript.RenderOptions{FromTurn: in.FromTurn, ToTurn: in.ToTurn})
			meta := ReadSessionOutput{
				SessionID:       in.SessionID,
				TotalTurns:      rendered.TotalTurns,
				RenderedTurns:   rendered.RenderedTurns,
				NextCursor:      rendered.NextCursor,
				PartialLastLine: parsed.PartialLastLine,
			}
			if in.Format == "json" {
				return nil, meta, nil
			}
			// Full format: markdown in BOTH StructuredContent.Body and
			// Content. The duplication is deliberate — see ReadSessionOutput
			// docstring.
			meta.Body = rendered.Markdown
			result := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: rendered.Markdown}},
			}
			return result, meta, nil
		}))
}

// ---------------- search_session ----------------

type SearchSessionInput struct {
	SessionID    string `json:"sessionId"`
	Query        string `json:"query" jsonschema:"literal substring (case-insensitive)"`
	ContextTurns int    `json:"contextTurns,omitempty" jsonschema:"context turns on each side (default 3)"`
}

type SearchMatch struct {
	TurnIndex        int              `json:"turnIndex"`
	MatchedText      string           `json:"matchedText"`
	SurroundingTurns []map[string]any `json:"surroundingTurns"`
}

type SearchSessionOutput struct {
	Query        string        `json:"query"`
	TotalMatches int           `json:"totalMatches"`
	Matches      []SearchMatch `json:"matches"`
}

func addSearchSession(s *mcp.Server, ctx Context) {
	desc := "Search a session's turns for a literal substring (case-insensitive). Returns matching turns with " +
		"surrounding context. Scoped to one session; no cross-session search."
	mcp.AddTool(s, &mcp.Tool{Name: "search_session", Description: desc},
		trace(ctx, "search_session", func(_ context.Context, _ *mcp.CallToolRequest, in SearchSessionInput) (*mcp.CallToolResult, SearchSessionOutput, error) {
			path := transcript.FindSessionPath(in.SessionID, ctx.DataDir)
			if path == "" {
				return errResult("sessionId not found: " + in.SessionID), SearchSessionOutput{}, nil
			}
			parsed, err := transcript.ParseFile(path)
			if err != nil {
				return errResult("parse error: " + err.Error()), SearchSessionOutput{}, nil
			}
			rendered := transcript.Render(parsed.Events, transcript.RenderOptions{})
			blocks := strings.Split(rendered.Markdown, "\n\n")
			needle := strings.ToLower(in.Query)
			ctxSize := in.ContextTurns
			if ctxSize <= 0 {
				ctxSize = 3
			}

			var matches []SearchMatch
			for i, blk := range blocks {
				if !strings.Contains(strings.ToLower(blk), needle) {
					continue
				}
				lo, hi := i-ctxSize, i+ctxSize+1
				if lo < 0 {
					lo = 0
				}
				if hi > len(blocks) {
					hi = len(blocks)
				}
				surrounding := make([]map[string]any, 0, hi-lo)
				for _, t := range blocks[lo:hi] {
					surrounding = append(surrounding, map[string]any{"role": "turn", "text": t})
				}
				matches = append(matches, SearchMatch{
					TurnIndex:        i,
					MatchedText:      blk,
					SurroundingTurns: surrounding,
				})
			}
			return nil, SearchSessionOutput{
				Query:        in.Query,
				TotalMatches: len(matches),
				Matches:      matches,
			}, nil
		}))
}

// ---------------- read_subagent ----------------

type ReadSubagentInput struct {
	SessionID string `json:"sessionId"`
	AgentUUID string `json:"agentUuid" jsonschema:"the tool_use.id of an Agent-tool call shown in read_session output"`
	FromTurn  int    `json:"fromTurn,omitempty"`
	ToTurn    int    `json:"toTurn,omitempty"`
}

type ReadSubagentOutput struct {
	SessionID       string `json:"sessionId"`
	AgentID         string `json:"agentId"`
	AgentType       string `json:"agentType,omitempty"`
	Description     string `json:"description,omitempty"`
	TotalTurns      int    `json:"totalTurns"`
	RenderedTurns   int    `json:"renderedTurns"`
	NextCursor      *int   `json:"nextCursor,omitempty"`
	PartialLastLine bool   `json:"partialLastLine"`
}

func addReadSubagent(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"Fetch a subagent session spawned from one of %s's main sessions. Agent-tool invocations in read_session "+
			"output include the agentUuid. Returns markdown subagent transcript + JSON metadata.",
		peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "read_subagent", Description: desc},
		trace(ctx, "read_subagent", func(_ context.Context, _ *mcp.CallToolRequest, in ReadSubagentInput) (*mcp.CallToolResult, ReadSubagentOutput, error) {
			res := transcript.ResolveSubagent(in.SessionID, in.AgentUUID, ctx.DataDir)
			if res == nil {
				return errResult(fmt.Sprintf("subagent not found: sessionId=%s agentUuid=%s", in.SessionID, in.AgentUUID)),
					ReadSubagentOutput{}, nil
			}
			parsed, err := transcript.ParseFile(res.JSONLPath)
			if err != nil {
				return errResult("parse error: " + err.Error()), ReadSubagentOutput{}, nil
			}
			rendered := transcript.Render(parsed.Events, transcript.RenderOptions{FromTurn: in.FromTurn, ToTurn: in.ToTurn})
			meta := ReadSubagentOutput{
				SessionID:       in.SessionID,
				AgentID:         res.AgentID,
				TotalTurns:      rendered.TotalTurns,
				RenderedTurns:   rendered.RenderedTurns,
				NextCursor:      rendered.NextCursor,
				PartialLastLine: parsed.PartialLastLine,
			}
			if res.Meta != nil {
				meta.AgentType = res.Meta.AgentType
				meta.Description = res.Meta.Description
			}
			result := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: rendered.Markdown}},
			}
			return result, meta, nil
		}))
}

// ---------------- read_tool_result ----------------

type ReadToolResultInput struct {
	SessionID string `json:"sessionId"`
	ToolUseID string `json:"toolUseId"`
	MaxBytes  int    `json:"maxBytes,omitempty" jsonschema:"default 65536"`
}

// ReadToolResultOutput is intentionally empty in PR 0.
//
// Phase 1 returned the body in a TextContent block AND a populated
// StructuredContent block ({source, truncated, bytes}). Some MCP consumers
// surfaced only the structured channel back to the calling model, which
// experienced as "metadata-only" reads against large sidecars. To make the
// body the unconditional source of truth, the metadata is now inlined as a
// leading line in the body text:
//
//	[source=sidecar, bytes=12345, truncated=false]
//
//	<actual content...>
//
// The other Phase-1 tools keep markdown + StructuredContent metadata —
// this is a targeted exception, not a deprecation of the convention.
type ReadToolResultOutput struct{}

func addReadToolResult(s *mcp.Server, ctx Context) {
	desc := "Fetch the full content of a tool result (typically a Read output or long command stdout) by toolUseId. " +
		"Returns the raw text as a single content block, capped at maxBytes (default 64KB). The first line of the " +
		"returned text is a metadata header in the form `[source=<sidecar|inline>, bytes=N, truncated=<bool>]`, " +
		"followed by a blank line, followed by the body."
	mcp.AddTool(s, &mcp.Tool{Name: "read_tool_result", Description: desc},
		trace(ctx, "read_tool_result", func(_ context.Context, _ *mcp.CallToolRequest, in ReadToolResultInput) (*mcp.CallToolResult, ReadToolResultOutput, error) {
			path := transcript.FindSessionPath(in.SessionID, ctx.DataDir)
			if path == "" {
				return errResult("sessionId not found: " + in.SessionID), ReadToolResultOutput{}, nil
			}
			loc := transcript.LocateToolResult(path, in.ToolUseID)
			if loc == nil {
				return errResult("no tool_result for toolUseId=" + in.ToolUseID), ReadToolResultOutput{}, nil
			}
			cap := in.MaxBytes
			if cap <= 0 {
				cap = 65536
			}

			var body, source string
			switch loc.Kind {
			case transcript.ToolResultSidecar:
				if !fileExists(loc.SidecarPath) {
					return errResult("sidecar missing at path: " + loc.SidecarPath), ReadToolResultOutput{}, nil
				}
				raw, err := os.ReadFile(loc.SidecarPath)
				if err != nil {
					return errResult("sidecar read error: " + err.Error()), ReadToolResultOutput{}, nil
				}
				body = string(raw)
				source = "sidecar"
			case transcript.ToolResultInline:
				body = loc.InlineText
				source = "inline"
			}

			truncated := false
			if len(body) > cap {
				body = body[:cap] + fmt.Sprintf("\n\n_[truncated at %d bytes]_", cap)
				truncated = true
			}

			header := fmt.Sprintf("[source=%s, bytes=%d, truncated=%t]\n\n", source, len(body), truncated)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: header + body}},
			}, ReadToolResultOutput{}, nil
		}))
}

// ---------------- get_session_summary ----------------

type GetSessionSummaryInput struct {
	SessionID string `json:"sessionId"`
}

type SubagentEntry struct {
	AgentID     string `json:"agentId"`
	AgentType   string `json:"agentType,omitempty"`
	Description string `json:"description,omitempty"`
}

type GetSessionSummaryOutput struct {
	SessionID         string          `json:"sessionId"`
	FirstUserMessage  string          `json:"firstUserMessage,omitempty"`
	ToolCallCount     int             `json:"toolCallCount"`
	UniqueToolNames   []string        `json:"uniqueToolNames"`
	Subagents         []SubagentEntry `json:"subagents"`
	LastAssistantText string          `json:"lastAssistantText,omitempty"`
}

func addGetSessionSummary(s *mcp.Server, ctx Context) {
	desc := "Compact digest of a single session — first user ask, notable tool calls, subagent summaries, and the " +
		"final assistant answer. Useful when you want context without a full read."
	mcp.AddTool(s, &mcp.Tool{Name: "get_session_summary", Description: desc},
		trace(ctx, "get_session_summary", func(_ context.Context, _ *mcp.CallToolRequest, in GetSessionSummaryInput) (*mcp.CallToolResult, GetSessionSummaryOutput, error) {
			path := transcript.FindSessionPath(in.SessionID, ctx.DataDir)
			if path == "" {
				return errResult("sessionId not found: " + in.SessionID), GetSessionSummaryOutput{}, nil
			}
			parsed, err := transcript.ParseFile(path)
			if err != nil {
				return errResult("parse error: " + err.Error()), GetSessionSummaryOutput{}, nil
			}

			firstUser := transcript.FirstUserMessage(parsed.Events, 300)
			toolCallCount := 0
			uniqueNames := map[string]struct{}{}
			var subagents []SubagentEntry
			var lastAssistant string

			for i := range parsed.Events {
				e := &parsed.Events[i]
				if e.Message == nil {
					continue
				}
				// Walk content blocks.
				blocks, err := unmarshalBlocks(e.Message.Content)
				if err != nil {
					continue
				}
				for _, b := range blocks {
					t, _ := b["type"].(string)
					switch t {
					case "tool_use":
						toolCallCount++
						name, _ := b["name"].(string)
						uniqueNames[name] = struct{}{}
						if name == "Agent" {
							if input, ok := b["input"].(map[string]any); ok {
								id, _ := b["id"].(string)
								entry := SubagentEntry{AgentID: id}
								if at, ok := input["subagent_type"].(string); ok {
									entry.AgentType = at
								}
								if d, ok := input["description"].(string); ok {
									entry.Description = d
								}
								subagents = append(subagents, entry)
							}
						}
					case "text":
						if e.Type == "assistant" {
							if txt, ok := b["text"].(string); ok {
								lastAssistant = txt
							}
						}
					}
				}
			}

			names := make([]string, 0, len(uniqueNames))
			for n := range uniqueNames {
				names = append(names, n)
			}

			out := GetSessionSummaryOutput{
				SessionID:        in.SessionID,
				FirstUserMessage: firstUser,
				ToolCallCount:    toolCallCount,
				UniqueToolNames:  names,
				Subagents:        subagents,
			}
			if lastAssistant != "" {
				if len(lastAssistant) > 600 {
					lastAssistant = lastAssistant[:599] + "…"
				}
				out.LastAssistantText = lastAssistant
			}
			return nil, out, nil
		}))
}

// ---------------- get_peer_info ----------------

type GetPeerInfoInput struct{}

type GetPeerInfoOutput struct {
	Name                string `json:"name"`
	Version             string `json:"version"`
	SessionCount        int    `json:"sessionCount"`
	ActiveSessionCount  int    `json:"activeSessionCount"`
}

func addGetPeerInfo(s *mcp.Server, ctx Context) {
	peer := capName(ctx.PeerName)
	desc := fmt.Sprintf(
		"Return identity and health for this hearsay server instance: {name, version, sessionCount, activeSessionCount}. "+
			"Call this to confirm which peer you're talking to (e.g. %s's server vs someone else's). "+
			"activeSessionCount is the number of sessions with isLive:true.",
		peer,
	)
	mcp.AddTool(s, &mcp.Tool{Name: "get_peer_info", Description: desc},
		trace(ctx, "get_peer_info", func(_ context.Context, _ *mcp.CallToolRequest, _ GetPeerInfoInput) (*mcp.CallToolResult, GetPeerInfoOutput, error) {
			sessionCount := 0
			root := transcript.ProjectsDir(ctx.DataDir)
			if dirs, err := os.ReadDir(root); err == nil {
				for _, d := range dirs {
					if !d.IsDir() {
						continue
					}
					entries, err := os.ReadDir(filepath.Join(root, d.Name()))
					if err != nil {
						continue
					}
					for _, e := range entries {
						if strings.HasSuffix(e.Name(), ".jsonl") {
							sessionCount++
						}
					}
				}
			}

			active := 0
			for _, s := range transcript.ListSessions(transcript.LocateOptions{
				DataDir:    ctx.DataDir,
				LiveWindow: ctx.LiveWindow,
			}) {
				if s.IsLive {
					active++
				}
			}

			return nil, GetPeerInfoOutput{
				Name:               ctx.PeerName,
				Version:            ctx.PeerVersion,
				SessionCount:       sessionCount,
				ActiveSessionCount: active,
			}, nil
		}))
}

// ---------------- helpers ----------------

func unmarshalBlocks(raw []byte) ([]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
