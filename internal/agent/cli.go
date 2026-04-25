package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/WiktorStarczewski/hearsay/internal/transcript"
)

// claudeAllowedToolsArg is the **hardcoded** value passed to
// `claude --print --allowed-tools …` on every invocation.  Never
// builtfrom user input; never overridable by a CLI flag.  Widening
// this list is a Phase-3 follow-up that requires a security review.
const claudeAllowedToolsArg = "Read Glob Grep"

// allowedToolNamesSet is the matching set used by the second-leg
// adversarial defense to validate the post-hoc JSONL replay.  Names
// match Claude Code's TitleCase tool naming.
var allowedToolNamesSet = map[string]bool{
	"Read": true,
	"Glob": true,
	"Grep": true,
}

// oneshotSystemPrompt is the system prompt OneShot calls bake in.
// Preserved verbatim from the SDK-era code for continuity (see plan
// section "Open questions resolved").
const oneshotSystemPrompt = "You are an investigative assistant running on a teammate's machine. " +
	"You can read, glob, and grep files under the working directory. " +
	"You have NO ability to run shell commands or write files. " +
	"Be concise; if a question requires more inspection than the user " +
	"provided, say so and ask for more guidance rather than guessing."

// subprocessGracePeriod is how long we wait between SIGTERM and
// SIGKILL when a subprocess context is canceled.
const subprocessGracePeriod = 5 * time.Second

// stderrBoundForResult caps the number of stderr bytes that travel
// back to the user-facing tool result on error.
const stderrBoundForResult = 512

// jsonlFlushRetries / jsonlFlushBackoff bound the post-exit JSONL
// flush wait.  Claude Code flushes synchronously in practice but the
// spec doesn't guarantee it.
const jsonlFlushRetries = 4
const jsonlFlushBackoff = 250 * time.Millisecond

// Config bundles everything the CLI driver needs.  Built once in
// main.go.
type Config struct {
	ClaudeBin       string  // default "claude" (validated via exec.LookPath at startup)
	PeerName        string
	DefaultBudget   Budget
	Auditor         *Auditor
	FallbackProject string
	KeepEnvAPIKey   bool   // mirrors --agent-keep-env-key; default false
	DataDir         string // Claude Code's data root for JSONL replay (defaults to ~/.claude in main.go)

	MaxConversations        int
	ConversationIdleTimeout time.Duration
}

// New constructs a CLI-driven Agent.  Default-applies `ClaudeBin =
// "claude"`, validates it's on PATH (or that the override path exists
// and is executable), starts the idle reaper, and returns.  A
// misconfigured peer fails fast at construction time rather than at
// first ask_peer_claude call.
func New(cfg Config) (Agent, error) {
	if cfg.ClaudeBin == "" {
		cfg.ClaudeBin = "claude"
	}
	if cfg.FallbackProject == "" {
		if cwd, err := os.Getwd(); err == nil {
			cfg.FallbackProject = cwd
		}
	}

	resolved, err := resolveClaudeBin(cfg.ClaudeBin)
	if err != nil {
		return nil, err
	}
	cfg.ClaudeBin = resolved

	a := &cliAgent{cfg: cfg}
	a.startReaper()
	return a, nil
}

// resolveClaudeBin returns the absolute path to the claude binary.
// If `bin` looks like a path (contains a separator), we stat it
// directly; otherwise we LookPath through $PATH.  Any failure
// surfaces as ErrClaudeBinMissing so main.go can render the friendly
// startup-refusal message.
func resolveClaudeBin(bin string) (string, error) {
	if strings.ContainsRune(bin, os.PathSeparator) {
		info, err := os.Stat(bin)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("%w: %s", ErrClaudeBinMissing, bin)
		}
		// On Unix, check the executable bit.  On Windows, exec.LookPath
		// has its own .exe / .bat handling but Stat is enough as a
		// portability bound for now (we don't ship Windows builds).
		if info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%w: %s (not executable)", ErrClaudeBinMissing, bin)
		}
		return bin, nil
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("%w: %s not found in PATH", ErrClaudeBinMissing, bin)
	}
	return resolved, nil
}

// Closer is implemented by cliAgent so the binary can shut the
// background reaper down on SIGTERM.  Same shape as PR B.
type Closer interface {
	Close()
}

func (a *cliAgent) Close() { a.stopReaper() }

// cliAgent is the production Agent implementation.  Drives `claude
// --print` as a subprocess and replays the resulting session JSONL
// to extract per-tool-call detail.
type cliAgent struct {
	cfg Config

	convsMu sync.Mutex
	convs   map[ConvID]*conversation

	reaperStop chan struct{}
	reaperDone chan struct{}
}

// OneShot runs a single short-lived conversation: fresh session-id,
// hardcoded read-only system prompt, no state carried over.
func (a *cliAgent) OneShot(ctx context.Context, req OneShotRequest) (Transcript, error) {
	budget := req.Budget.Resolve(a.cfg.DefaultBudget)
	project := a.resolveProject(req.Project)
	if project == "" {
		return Transcript{
			Markdown:     "_no project root resolved_\n",
			StopReason:   StopReasonError,
			ErrorSummary: ErrInvalidProject,
		}, nil
	}
	if budget.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget.Timeout)
		defer cancel()
	}

	conv := &conversation{
		convID:       ConvID(newSessionUUID()),
		project:      project,
		systemPrompt: oneshotSystemPrompt,
	}
	tx, invokes, _ := a.runClaude(ctx, runReq{
		conv:        conv,
		prompt:      req.Prompt,
		budget:      budget,
		convStarted: false, // OneShot is always a "first turn"
		isOneShot:   true,
	})

	if a.cfg.Auditor != nil {
		_ = a.cfg.Auditor.Log(AuditEntry{
			Timestamp:     time.Now().UTC(),
			PeerName:      a.cfg.PeerName,
			ConvID:        "oneshot",
			TurnIndex:     1,
			PromptBytes:   len(req.Prompt),
			ResponseBytes: len(tx.Markdown),
			ToolCalls:     invokes,
			ElapsedMs:     tx.ElapsedMs,
			StopReason:    tx.StopReason,
			ErrorSummary:  tx.ErrorSummary,
		})
	}
	return tx, nil
}

// runReq is the input bundle for runClaude — the single entry point
// that builds + executes a `claude --print` invocation for both
// OneShot (isOneShot=true, convStarted=false) and SendMessage paths.
type runReq struct {
	conv        *conversation
	prompt      string
	budget      Budget
	convStarted bool
	isOneShot   bool
}

// runClaude builds and executes the `claude --print` subprocess, then
// replays the resulting JSONL to extract per-tool-call detail.
func (a *cliAgent) runClaude(ctx context.Context, r runReq) (Transcript, []AuditToolInvoke, error) {
	args := a.buildArgs(r)
	envHandling, env := a.buildEnv()

	start := time.Now()
	cmd := exec.CommandContext(ctx, a.cfg.ClaudeBin, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = subprocessGracePeriod
	cmd.Dir = r.conv.project
	cmd.Env = env
	cmd.Stdin = nil // explicitly no interactive read

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		// claude went missing between New()'s LookPath and now.
		// The Start() call can return either *exec.Error wrapping
		// exec.ErrNotFound (binary not on PATH) or *fs.PathError /
		// os.ErrNotExist (binary deleted at the absolute path).
		// Both signal claude_missing; substring-matching the message
		// is brittle, so we walk the typed errors.
		summary := ErrOther
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			summary = ErrClaudeMissing
		}
		return Transcript{
			Markdown:      fmt.Sprintf("_failed to start claude: %s_\n", err.Error()),
			StopReason:    StopReasonError,
			ElapsedMs:     time.Since(start).Milliseconds(),
			ErrorSummary:  summary,
		}, nil, nil
	}

	waitErr := cmd.Wait()
	elapsed := time.Since(start).Milliseconds()

	tx := Transcript{
		ElapsedMs: elapsed,
	}

	// Map context errors first — they take precedence over JSON parsing
	// because if we shut down or timed out, the JSON may be partial.
	if ctxErr := ctx.Err(); ctxErr != nil {
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			tx.StopReason = StopReasonTimeout
			tx.Markdown = "_subprocess timed out_\n"
		default:
			tx.StopReason = StopReasonShutdown
			tx.Markdown = "_subprocess canceled (peer shutting down)_\n"
		}
		// Audit-side env-API-key signal still applies.
		invokes := a.tryReplayJSONL(r.conv, "")
		_ = envHandling // emitted via Auditor at the call site
		return tx, invokes, nil
	}

	parsed, parseErr := parseClaudeJSON(stdout.Bytes())
	if parseErr != nil || parsed == nil {
		// Non-JSON output.  Surface stderr (truncated) for debugging.
		tx.StopReason = StopReasonError
		tx.ErrorSummary = ErrOther
		tx.Markdown = renderStderrBody(stderr.Bytes(), waitErr)
		return tx, nil, nil
	}

	// Real-session ID may differ from what we passed in (e.g. if
	// claude rewrites it).  Trust the JSON for replay path.
	if parsed.SessionID != "" && parsed.SessionID != string(r.conv.convID) {
		// Defensive: this shouldn't happen with --session-id, but if
		// it does, the replay won't find anything.  Update the conv
		// so future --resume calls match.
		r.conv.convID = ConvID(parsed.SessionID)
	}

	tx.Markdown = parsed.Result
	tx.TurnCount = parsed.NumTurns
	tx.StopReason, tx.ErrorSummary = mapClaudeStopReason(parsed)

	// Replay the session JSONL for per-tool-call detail + second-leg defense.
	invokes := a.tryReplayJSONL(r.conv, parsed.SessionID)
	tx.ToolCallCount = len(invokes)

	// Second-leg adversarial defense.
	if disallowed := disallowedToolName(invokes); disallowed != "" && tx.StopReason != StopReasonError {
		tx.StopReason = StopReasonError
		tx.ErrorSummary = ErrDisallowedTool
		tx.Markdown = fmt.Sprintf(
			"_hearsay refused: tool %q is not in the read-only allowlist (Read, Glob, Grep)_\n\n%s",
			disallowed, tx.Markdown)
	}

	// MaxToolCalls hard cap.
	if r.budget.MaxToolCalls > 0 && tx.ToolCallCount > r.budget.MaxToolCalls {
		tx.StopReason = StopReasonMaxToolCalls
		tx.Markdown = fmt.Sprintf(
			"_max_tool_calls budget (%d) exceeded; %d tool calls observed_\n\n%s",
			r.budget.MaxToolCalls, tx.ToolCallCount, tx.Markdown)
	}

	_ = envHandling // emitted via Auditor at the call site
	return tx, invokes, nil
}

// buildArgs assembles the claude --print argv per the three argv
// variants (see plan).  All three share --print --output-format json
// --allowed-tools <const>; they differ in the session/system-prompt
// block.
func (a *cliAgent) buildArgs(r runReq) []string {
	args := []string{
		"--print",
		"--output-format", "json",
		"--allowed-tools", claudeAllowedToolsArg,
	}
	if r.convStarted {
		// Variant 3: resumed turn.  No --system-prompt.
		args = append(args, "--resume", string(r.conv.convID))
	} else {
		// Variant 1 (OneShot) or Variant 2 (first turn of a conv):
		// fresh session-id + system-prompt.
		args = append(args, "--session-id", string(r.conv.convID))
		if r.conv.systemPrompt != "" {
			args = append(args, "--system-prompt", maybeWithBudgetNudge(r.conv.systemPrompt, r.budget))
		} else if r.budget.MaxTokens > 0 {
			// No system_prompt provided but budget set — emit a
			// minimal nudge-only system prompt.
			args = append(args, "--system-prompt", maybeWithBudgetNudge("", r.budget))
		}
	}
	args = append(args, "--", r.prompt)
	return args
}

// maybeWithBudgetNudge returns the system prompt with a soft-budget
// hint appended when r.budget.MaxTokens > 0.  Best-effort only —
// Claude Code's CLI doesn't enforce token caps, so we lean on the
// model to respect the nudge.
func maybeWithBudgetNudge(systemPrompt string, budget Budget) string {
	if budget.MaxTokens <= 0 {
		return systemPrompt
	}
	nudge := fmt.Sprintf("(soft budget: keep total response tokens under ~%d)", budget.MaxTokens)
	if systemPrompt == "" {
		return nudge
	}
	return systemPrompt + "\n\n" + nudge
}

// buildEnv constructs the subprocess env per the auth-precedence
// footgun mitigation: strips ANTHROPIC_API_KEY by default to ensure
// OAuth/subscription auth wins.  Returns the audit-handling tag
// alongside the env slice.
func (a *cliAgent) buildEnv() (envHandling string, env []string) {
	parent := os.Environ()
	if a.cfg.KeepEnvAPIKey {
		// Operator opted in: pass-through.
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			return "honored", parent
		}
		return "absent", parent
	}
	// Default: strip the var.
	envHandling = "absent"
	env = make([]string, 0, len(parent))
	for _, kv := range parent {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			if len(kv) > len("ANTHROPIC_API_KEY=") {
				envHandling = "stripped"
			}
			continue
		}
		env = append(env, kv)
	}
	return envHandling, env
}

// claudeJSONResult mirrors the verified `claude --print
// --output-format json` payload shape.  Fields not used by the driver
// are kept as RawMessage (or omitted) so unknown additions don't
// crash the parse.
type claudeJSONResult struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	IsError          bool   `json:"is_error"`
	APIErrorStatus   any    `json:"api_error_status"`
	DurationMs       int64  `json:"duration_ms"`
	NumTurns         int    `json:"num_turns"`
	Result           string `json:"result"`
	StopReason       string `json:"stop_reason"`
	SessionID        string `json:"session_id"`
	PermissionDenied []any  `json:"permission_denials"`
	TerminalReason   string `json:"terminal_reason"`
}

// parseClaudeJSON tolerates extra leading/trailing whitespace but
// requires a single JSON object.  Returns (nil, err) on failure so
// the caller falls back to the stderr-bytes path.
func parseClaudeJSON(raw []byte) (*claudeJSONResult, error) {
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		return nil, errors.New("empty stdout")
	}
	var r claudeJSONResult
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Type != "result" {
		return nil, fmt.Errorf("unexpected type %q", r.Type)
	}
	return &r, nil
}

// mapClaudeStopReason cascades subtype + stop_reason into hearsay's
// StopReason enum.  See plan section "Parsing the JSON result" for
// the full table.
func mapClaudeStopReason(p *claudeJSONResult) (StopReason, ErrorSummary) {
	if p.Subtype == "error" || p.IsError {
		errText := ""
		if s, ok := p.APIErrorStatus.(string); ok {
			errText = s
		} else if p.Result != "" {
			errText = p.Result
		}
		return StopReasonError, classifyErrorMsg(errText)
	}
	// Non-success / non-error subtypes (e.g. a future "max_turns_exceeded")
	// take precedence — they describe a *terminal* condition that
	// stop_reason can't override.
	if p.Subtype != "success" && p.Subtype != "" {
		return StopReasonMaxToolCalls, ""
	}
	switch p.StopReason {
	case "max_tokens":
		return StopReasonMaxTokens, ""
	case "end_turn", "stop_sequence", "tool_use", "":
		return StopReasonEndTurn, ""
	}
	return StopReasonEndTurn, ""
}

// classifyErrorMsg substring-matches Claude Code's stderr / error
// text to produce one of the coarse ErrorSummary categories.  Same
// shape as the PR-A SDK-era classifier — the categories don't change
// just because the error source moved from the SDK to the CLI.
func classifyErrorMsg(msg string) ErrorSummary {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "429"):
		return ErrAPIRateLimit
	case strings.Contains(lower, "401"), strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "authentication"), strings.Contains(lower, "not logged in"):
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

// renderStderrBody packages the first stderrBoundForResult bytes of
// stderr (plus the wait-error if available) into a Markdown body so
// the operator can debug a non-JSON failure.
func renderStderrBody(stderr []byte, waitErr error) string {
	body := stderr
	if len(body) > stderrBoundForResult {
		body = body[:stderrBoundForResult]
	}
	exitMsg := ""
	if waitErr != nil {
		exitMsg = fmt.Sprintf(" (exit: %s)", waitErr.Error())
	}
	return fmt.Sprintf("_claude --print failed%s_\n\n```\n%s\n```\n", exitMsg, body)
}

// tryReplayJSONL locates the session JSONL on disk and parses it via
// the existing transcript package, returning per-turn tool_use
// invocations as AuditToolInvoke records.  Retries up to
// jsonlFlushRetries to absorb the (rare) case where the subprocess
// hasn't finished flushing on exit.
func (a *cliAgent) tryReplayJSONL(conv *conversation, sessionIDOverride string) []AuditToolInvoke {
	convID := string(conv.convID)
	if sessionIDOverride != "" {
		convID = sessionIDOverride
	}
	if convID == "" || a.cfg.DataDir == "" {
		return nil
	}

	var path string
	for i := 0; i < jsonlFlushRetries; i++ {
		path = transcript.FindSessionPath(convID, a.cfg.DataDir)
		if path != "" {
			break
		}
		time.Sleep(jsonlFlushBackoff)
	}
	if path == "" {
		return nil
	}

	parsed, err := transcript.ParseFile(path)
	if err != nil || parsed == nil {
		return nil
	}

	// Walk all events; we want only *this turn's* tool_use blocks.
	// Without a per-turn delimiter in the JSONL, the simplest
	// approximation is: tool_use blocks emitted in the most recent
	// chain of assistant events ending with a user message.  For
	// PR C we approximate by taking ALL tool_use blocks in the
	// JSONL belonging to this session (every turn appended to the
	// same file), because turn-scoping is a rabbit hole.  Audit
	// fidelity = "all tool calls so far in this conversation."
	// Worth refining in a follow-up if it matters.
	var invokes []AuditToolInvoke
	for i := range parsed.Events {
		ev := &parsed.Events[i]
		if ev.Message == nil {
			continue
		}
		blocks, err := unmarshalContentBlocks(ev.Message.Content)
		if err != nil {
			continue
		}
		for _, b := range blocks {
			if t, _ := b["type"].(string); t == "tool_use" {
				name, _ := b["name"].(string)
				inputBytes := 0
				if input, ok := b["input"]; ok {
					if raw, err := json.Marshal(input); err == nil {
						inputBytes = len(raw)
					}
				}
				invokes = append(invokes, AuditToolInvoke{
					Name:     name,
					ArgBytes: inputBytes,
				})
			}
		}
	}
	return invokes
}

// unmarshalContentBlocks decodes a Message.Content RawMessage into
// the heterogeneous block array Claude Code emits.
func unmarshalContentBlocks(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// disallowedToolName scans the audit-invokes for the first tool name
// outside the read-only allowlist.  Returns "" if all calls pass.
func disallowedToolName(invokes []AuditToolInvoke) string {
	for _, inv := range invokes {
		if !allowedToolNamesSet[inv.Name] {
			return inv.Name
		}
	}
	return ""
}

