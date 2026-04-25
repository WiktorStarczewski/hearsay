// Package agent wraps the Anthropic Managed-Agents API
// (`client.Beta.Agents` + `client.Beta.Sessions`) for hearsay's Phase-2
// interactive tools.
//
// Key design points:
//
//   - The agent runs on Ivan's box, not in Anthropic's cloud sandbox.
//     We register read / glob / grep as **custom** tools on the
//     Managed-Agents agent (NOT the bundled `agent_toolset_20260401`,
//     which would route execution to an Anthropic-hosted Environment).
//     The session emits `agent.custom_tool_use`; we execute on Ivan's
//     filesystem and reply with `user.custom_tool_result`.
//
//   - The hearsay-side state map is metadata only — full message
//     history lives server-side on the SDK session.
//
//   - The event loop (loop.go) is decoupled from the SDK so unit tests
//     can drive it with canned event streams.  sdk.go is the only place
//     we touch the SDK directly.
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
	ErrOther          ErrorSummary = "other"
)

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

// Agent is the interface ask_peer_claude (and PR-B's conversation
// tools) call into.  PR A only implements OneShot; PR B will extend
// the same package with the conversation-lifecycle methods.
type Agent interface {
	OneShot(ctx context.Context, req OneShotRequest) (Transcript, error)
}

// ErrAgentDisabled is returned when callers try to use an Agent
// instance that wasn't constructed (--enable-agent off).  The tools
// layer prevents this by not registering the agent tools when the
// flag is off, but the type is here for defense-in-depth.
var ErrAgentDisabled = errors.New("agent: not enabled (use --enable-agent)")
