// Package agent drives the peer-side Claude session for hearsay's
// Phase-2 interactive tools.
//
// Phase-2 originally shipped via the Anthropic Managed-Agents API; PR
// C pivoted to a subprocess driver around `claude --print` so peers
// can use their Claude Code subscription instead of needing an
// ANTHROPIC_API_KEY.  Key design points after the pivot:
//
//   - Tools execute on the peer's box.  Hearsay invokes `claude
//     --print --allowed-tools "Read Glob Grep"` with cwd set to the
//     project root; Claude Code itself enforces the read-only
//     allowlist and runs the tools natively.
//
//   - The convID is the Claude Code session UUID.  Conversation
//     persistence lives in `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`
//     — the same JSONL files Phase-1 reads via `read_session`.  The
//     hearsay-side conversation map is metadata only (lastActivityAt,
//     turnCount, system-prompt preview, etc.) and doesn't survive a
//     restart.
//
//   - cli.go holds the subprocess driver + JSON parser.  No event
//     loop — `claude --print` is synchronous, returns one JSON
//     result, and writes its session JSONL to disk.  We replay the
//     JSONL after the call to extract per-tool-call detail (the
//     stdout JSON has no `tool_calls[]` field).
package agent

import (
	"context"
	"errors"
	"time"
)

// AllowedToolNames is the hardcoded read-only allowlist for Phase 2.
// Widening this list to include `bash`, `edit`, `write`, etc. is an
// intentional Tier-3 follow-up — never a Phase-2 knob.
var AllowedToolNames = []string{"read", "glob", "grep"}

// StopReason discriminates how a turn ended.  Returned with every
// Transcript so callers can decide whether to follow up.
type StopReason string

const (
	StopReasonEndTurn      StopReason = "end_turn"
	StopReasonMaxTokens    StopReason = "max_tokens"
	StopReasonMaxToolCalls StopReason = "max_tool_calls"
	StopReasonTimeout      StopReason = "timeout"
	StopReasonError        StopReason = "error"
	StopReasonShutdown     StopReason = "shutdown"
)

// ErrorSummary categorizes an error so callers can branch without
// parsing free-text.  Always populated when StopReason == "error".
type ErrorSummary string

const (
	ErrAPIUnavailable ErrorSummary = "api_unavailable"
	ErrAPIRateLimit   ErrorSummary = "api_rate_limited"
	ErrAPIAuth        ErrorSummary = "api_auth"
	ErrNetwork        ErrorSummary = "network"
	ErrTimeout        ErrorSummary = "timeout"
	ErrDisallowedTool ErrorSummary = "disallowed_tool"
	ErrInvalidProject ErrorSummary = "invalid_project"
	ErrClaudeMissing  ErrorSummary = "claude_missing"
	ErrOther          ErrorSummary = "other"
)

// ErrClaudeBinMissing is returned by New() when the configured `claude`
// binary isn't on PATH (or the explicit override path doesn't exist or
// isn't executable).  main.go translates this into the friendly
// startup-refusal message documented in the README.
var ErrClaudeBinMissing = errors.New("agent: claude binary not found on PATH")

// Budget bounds a single turn.  Zero means "use the server default
// from the CLI flags."  See the plan for the cascade rules.
type Budget struct {
	MaxTokens    int
	MaxToolCalls int
	Timeout      time.Duration
}

// Resolve fills zero fields from the supplied default.  Used for the
// per-call ⟶ per-conversation ⟶ server cascade.
func (b Budget) Resolve(defaults Budget) Budget {
	out := b
	if out.MaxTokens == 0 {
		out.MaxTokens = defaults.MaxTokens
	}
	if out.MaxToolCalls == 0 {
		out.MaxToolCalls = defaults.MaxToolCalls
	}
	if out.Timeout == 0 {
		out.Timeout = defaults.Timeout
	}
	return out
}

// OneShotRequest is the input to Agent.OneShot.
type OneShotRequest struct {
	Prompt  string
	Project string // "" => most-recent session's cwd; falls back to hearsay's cwd
	Budget  Budget
}

// Transcript is what every prompt-sending tool returns.
type Transcript struct {
	Markdown      string
	TurnCount     int
	ToolCallCount int
	StopReason    StopReason
	ElapsedMs     int64
	ErrorSummary  ErrorSummary // populated iff StopReason == "error"
}

// ConvID is an opaque handle to a hearsay-managed conversation.  We
// use the SDK's session ID directly so there's no map-lookup
// indirection inside the agent layer.
type ConvID string

// StartReq is the input to Agent.StartConversation.
type StartReq struct {
	SystemPrompt string
	Project      string
	Budget       Budget // becomes the conversation's per-turn default
}

// ConvMeta mirrors list_peer_conversations' output one-for-one.
type ConvMeta struct {
	ConvID         ConvID
	StartedAt      time.Time
	LastActivityAt time.Time
	TurnCount      int
	// Preview is the first ~140 *runes* (not bytes) of the first user
	// message — rune-based truncation so a multi-byte codepoint at the
	// boundary doesn't yield invalid UTF-8.  When the conversation has
	// been started but no send_peer_message has happened yet, falls
	// back to the first ~140 runes of the system_prompt (or empty if
	// no system_prompt was set).
	Preview string
}

// EndReason discriminates how a conversation ended; carried into the
// audit log + the end_peer_conversation tool's output.
type EndReason string

const (
	EndedByCaller   EndReason = "caller"
	EndedByIdleReap EndReason = "idle_timeout"
	EndedByShutdown EndReason = "shutdown"
)

// EndSummary mirrors end_peer_conversation's tool output.
type EndSummary struct {
	Ended        bool
	AlreadyEnded bool      // true if the conv was already ended (idempotent re-end)
	TotalTurns   int
	EndedReason  EndReason
}

// Agent is the interface every Phase-2 tool calls into.  PR A landed
// OneShot; PR B added the conversation-lifecycle methods.
type Agent interface {
	OneShot(ctx context.Context, req OneShotRequest) (Transcript, error)
	StartConversation(ctx context.Context, req StartReq) (ConvID, time.Time /*startedAt*/, Budget /*effective*/, error)
	SendMessage(ctx context.Context, convID ConvID, prompt string, budget Budget) (Transcript, error)
	ListConversations() []ConvMeta
	EndConversation(ctx context.Context, convID ConvID, reason EndReason) (EndSummary, error)
}

// ErrUnknownConv is returned by SendMessage / EndConversation when
// the convID has no matching live conversation (typo, idle-reaped,
// or already ended).
var ErrUnknownConv = errors.New("agent: unknown conversation id")

// ErrConvCap is returned by StartConversation when --max-conversations
// is full.  The tool layer translates this into errorSummary=max_conversations.
var ErrConvCap = errors.New("agent: max conversations reached")

// ErrConvReaped is returned by SendMessage when the named conversation
// existed but was reaped after the idle timeout.
var ErrConvReaped = errors.New("agent: conversation reaped after idle timeout")

// ErrAgentDisabled is returned when callers try to use an Agent
// instance that wasn't constructed (--enable-agent off).  The tools
// layer prevents this by not registering the agent tools when the
// flag is off, but the type is here for defense-in-depth.
var ErrAgentDisabled = errors.New("agent: not enabled (use --enable-agent)")
