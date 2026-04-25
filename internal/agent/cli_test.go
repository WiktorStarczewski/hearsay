package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------
// Test helpers
//
// The driver tests stage a fake `claude` shell script that:
//   - records its argv to a file
//   - optionally writes a fixture session JSONL to the expected path
//     (mirroring what real Claude Code does) so the post-hoc replay
//     can extract tool_use blocks for the second-leg defense
//   - prints a canned JSON result on stdout

type fakeClaudeOpts struct {
	stdout       string // exact bytes written to stdout
	exitCode     int    // 0 = success
	jsonlPayload string // if non-empty, write this to the expected JSONL path
	sleepSeconds int    // optional sleep before exit (for timeout tests)
}

// writeFakeClaude builds a shell script at <dir>/claude that captures
// its argv to <dir>/argv.txt.  Returns the binary path, the argv
// capture path, and a function that writes the JSONL fixture into
// <dataDir>/projects/<encodeCwd>/<sessionID>.jsonl after the script
// runs (since the script doesn't know dataDir).
func writeFakeClaude(t *testing.T, dir string, opts fakeClaudeOpts) (binPath, argvPath string) {
	t.Helper()
	binPath = filepath.Join(dir, "claude")
	argvPath = filepath.Join(dir, "argv.txt")
	stdoutFile := filepath.Join(dir, "stdout.bin")
	if err := os.WriteFile(stdoutFile, []byte(opts.stdout), 0o644); err != nil {
		t.Fatalf("write fake stdout: %v", err)
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
sleep %d
cat %q
exit %d
`, argvPath, opts.sleepSeconds, stdoutFile, opts.exitCode)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return binPath, argvPath
}

// stageJSONL writes a fixture session JSONL to the path Claude Code
// would have written it to: <dataDir>/projects/<encoded-cwd>/<id>.jsonl.
// Phase-1's transcript package decodes the cwd → dir name; we mirror
// that encoding (slashes → dashes, leading dash) here for the test.
func stageJSONL(t *testing.T, dataDir, project, sessionID, jsonl string) {
	t.Helper()
	encoded := strings.ReplaceAll(project, string(filepath.Separator), "-")
	dir := filepath.Join(dataDir, "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
}

// readArgv returns the captured argv split by newlines, with empty
// trailing entries dropped.
func readArgv(t *testing.T, argvPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	return lines
}

// successJSON is a minimal-valid claude --print output that the JSON
// parser accepts.  Used by argv-shape tests that don't care about
// post-hoc detail.
func successJSON(sessionID string) string {
	return fmt.Sprintf(
		`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"ok","stop_reason":"end_turn","session_id":%q,"usage":{"input_tokens":1,"output_tokens":1},"permission_denials":[]}`,
		sessionID)
}

func newDriver(t *testing.T, claudeBin string) *cliAgent {
	t.Helper()
	dataDir := t.TempDir()
	auditor, _ := NewAuditor("")
	a, err := New(Config{
		ClaudeBin:               claudeBin,
		PeerName:                "test-peer",
		DataDir:                 dataDir,
		Auditor:                 auditor,
		FallbackProject:         t.TempDir(),
		ConversationIdleTimeout: 0,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a.(*cliAgent)
}

// -----------------------------------------------------------------------
// Argv-contract tests (verification step 2 — variants 1 / 2 / 3)

func TestRunClaude_OneShotArgvShape(t *testing.T) {
	dir := t.TempDir()
	bin, argvPath := writeFakeClaude(t, dir, fakeClaudeOpts{
		stdout: successJSON("oneshot-uuid"),
	})
	a := newDriver(t, bin)
	defer a.Close()

	tx, err := a.OneShot(context.Background(), OneShotRequest{
		Prompt:  "hello",
		Project: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("OneShot: %v", err)
	}
	if tx.StopReason != StopReasonEndTurn {
		t.Errorf("StopReason=%v, want end_turn", tx.StopReason)
	}

	argv := readArgv(t, argvPath)
	want := map[string]bool{
		"--print":         true,
		"--output-format": true,
		"--allowed-tools": true,
		"--session-id":    true,
		"--system-prompt": true,
	}
	for _, w := range []string{"--print", "--output-format", "--allowed-tools", "--session-id", "--system-prompt"} {
		found := false
		for _, a := range argv {
			if a == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv missing %q; got: %v", w, argv)
		}
	}
	_ = want

	// Adversarial-config asserts: NEVER pass --bare or
	// --no-session-persistence; --allowed-tools value is hardcoded.
	for i, a := range argv {
		if a == "--bare" || a == "--no-session-persistence" {
			t.Errorf("argv[%d] = %q which is forbidden", i, a)
		}
		if a == "--allowed-tools" && i+1 < len(argv) && argv[i+1] != "Read Glob Grep" {
			t.Errorf("--allowed-tools value = %q, want \"Read Glob Grep\"", argv[i+1])
		}
	}
}

func TestRunClaude_AllowedToolsHardcoded(t *testing.T) {
	dir := t.TempDir()
	bin, argvPath := writeFakeClaude(t, dir, fakeClaudeOpts{stdout: successJSON("uuid")})
	a := newDriver(t, bin)
	defer a.Close()

	_, _ = a.OneShot(context.Background(), OneShotRequest{Prompt: "x", Project: t.TempDir()})
	argv := readArgv(t, argvPath)

	for i, tok := range argv {
		if tok == "--allowed-tools" {
			if i+1 >= len(argv) {
				t.Fatalf("--allowed-tools without value: %v", argv)
			}
			if argv[i+1] != claudeAllowedToolsArg {
				t.Errorf("--allowed-tools = %q, want %q", argv[i+1], claudeAllowedToolsArg)
			}
			return
		}
	}
	t.Errorf("argv missing --allowed-tools: %v", argv)
}

func TestRunClaude_ResumeOnSecondTurn(t *testing.T) {
	// Need a project directory that's also where the JSONL gets written;
	// the JSONL replay won't find the JSONL otherwise.  We use the
	// driver's DataDir, not the script-side dir.
	scriptDir := t.TempDir()
	project := t.TempDir()
	bin, argvPath := writeFakeClaude(t, scriptDir, fakeClaudeOpts{stdout: successJSON("conv-1")})
	a := newDriver(t, bin)
	defer a.Close()

	convID, _, _, err := a.StartConversation(context.Background(), StartReq{
		SystemPrompt: "you are a tester",
		Project:      project,
	})
	if err != nil {
		t.Fatalf("StartConversation: %v", err)
	}

	// First turn: variant 2 (--session-id, --system-prompt).
	if _, err := a.SendMessage(context.Background(), convID, "first", Budget{}); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	argv1 := readArgv(t, argvPath)
	hasSessionID := false
	for _, tok := range argv1 {
		if tok == "--session-id" {
			hasSessionID = true
		}
		if tok == "--resume" {
			t.Errorf("first turn must NOT use --resume; argv: %v", argv1)
		}
	}
	if !hasSessionID {
		t.Errorf("first turn must use --session-id; argv: %v", argv1)
	}

	// Second turn: variant 3 (--resume, no --system-prompt).
	if _, err := a.SendMessage(context.Background(), convID, "second", Budget{}); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	argv2 := readArgv(t, argvPath)
	hasResume := false
	for _, tok := range argv2 {
		if tok == "--resume" {
			hasResume = true
		}
		if tok == "--session-id" {
			t.Errorf("resumed turn must NOT use --session-id; argv: %v", argv2)
		}
		if tok == "--system-prompt" {
			t.Errorf("resumed turn must NOT pass --system-prompt; argv: %v", argv2)
		}
	}
	if !hasResume {
		t.Errorf("second turn must use --resume; argv: %v", argv2)
	}
}

// -----------------------------------------------------------------------
// JSON parser + StopReason cascade

func TestRunClaude_ParsesUsageAndSubtype(t *testing.T) {
	cases := []struct {
		name string
		body string
		want StopReason
	}{
		{"end_turn", `{"type":"result","subtype":"success","stop_reason":"end_turn","result":"ok","num_turns":1,"session_id":"s","permission_denials":[],"usage":{}}`, StopReasonEndTurn},
		{"max_tokens", `{"type":"result","subtype":"success","stop_reason":"max_tokens","result":"...","num_turns":1,"session_id":"s","permission_denials":[],"usage":{}}`, StopReasonMaxTokens},
		{"stop_sequence_to_endturn", `{"type":"result","subtype":"success","stop_reason":"stop_sequence","result":"ok","num_turns":1,"session_id":"s","permission_denials":[],"usage":{}}`, StopReasonEndTurn},
		{"error_subtype", `{"type":"result","subtype":"error","is_error":true,"stop_reason":"end_turn","result":"rate limit hit","num_turns":1,"session_id":"s","permission_denials":[],"usage":{}}`, StopReasonError},
		{"future_subtype", `{"type":"result","subtype":"max_turns_exceeded","stop_reason":"end_turn","result":"too many","num_turns":99,"session_id":"s","permission_denials":[],"usage":{}}`, StopReasonMaxToolCalls},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed, err := parseClaudeJSON([]byte(c.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, _ := mapClaudeStopReason(parsed)
			if got != c.want {
				t.Errorf("StopReason = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseClaudeJSON_RejectsNonResultType(t *testing.T) {
	if _, err := parseClaudeJSON([]byte(`{"type":"event","subtype":"foo"}`)); err == nil {
		t.Errorf("expected rejection of non-result JSON")
	}
}

func TestParseClaudeJSON_RejectsEmpty(t *testing.T) {
	if _, err := parseClaudeJSON([]byte(``)); err == nil {
		t.Errorf("expected rejection of empty input")
	}
}

// -----------------------------------------------------------------------
// Second-leg adversarial defense (verification step 7)

func TestRunClaude_DisallowedToolInJSONLRejected(t *testing.T) {
	scriptDir := t.TempDir()
	project := t.TempDir()
	sessionID := "disallowed-test-uuid"
	bin, _ := writeFakeClaude(t, scriptDir, fakeClaudeOpts{stdout: successJSON(sessionID)})

	a := newDriver(t, bin)
	defer a.Close()

	// Stage a fixture JSONL that includes a tool_use for "Bash" — a
	// name our hardcoded allowlist must reject.
	jsonl := `{"type":"assistant","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"cmd":"echo hi"}}]}}` + "\n"
	stageJSONL(t, a.cfg.DataDir, project, sessionID, jsonl)

	// runClaude is called via OneShot — but OneShot generates its own
	// session ID.  To control the session ID we need to use a started
	// conversation.  Easier path: directly call runClaude by getting
	// the conv to use the staged ID.  Since cli_test is in the same
	// package we have access to the unexported types.
	conv := &conversation{convID: ConvID(sessionID), project: project, systemPrompt: "x"}
	tx, _, _ := a.runClaude(context.Background(), runReq{conv: conv, prompt: "hi", budget: Budget{}})

	if tx.StopReason != StopReasonError {
		t.Errorf("StopReason=%v, want error (disallowed_tool)", tx.StopReason)
	}
	if tx.ErrorSummary != ErrDisallowedTool {
		t.Errorf("ErrorSummary=%v, want disallowed_tool", tx.ErrorSummary)
	}
}

func TestRunClaude_AllowsReadGlobGrepInJSONL(t *testing.T) {
	scriptDir := t.TempDir()
	project := t.TempDir()
	sessionID := "allowed-test-uuid"
	bin, _ := writeFakeClaude(t, scriptDir, fakeClaudeOpts{stdout: successJSON(sessionID)})

	a := newDriver(t, bin)
	defer a.Close()

	jsonl := `{"type":"assistant","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Read","input":{"file_path":"x"}},{"type":"tool_use","id":"tu2","name":"Grep","input":{"pattern":"y"}}]}}` + "\n"
	stageJSONL(t, a.cfg.DataDir, project, sessionID, jsonl)

	conv := &conversation{convID: ConvID(sessionID), project: project, systemPrompt: "x"}
	tx, invokes, _ := a.runClaude(context.Background(), runReq{conv: conv, prompt: "hi", budget: Budget{}})

	if tx.StopReason != StopReasonEndTurn {
		t.Errorf("StopReason=%v, want end_turn (Read+Grep are allowed)", tx.StopReason)
	}
	if tx.ToolCallCount != 2 {
		t.Errorf("ToolCallCount=%d, want 2", tx.ToolCallCount)
	}
	if len(invokes) != 2 {
		t.Errorf("len(invokes)=%d, want 2", len(invokes))
	}
}

// -----------------------------------------------------------------------
// Budget enforcement

func TestRunClaude_MaxToolCallsCap(t *testing.T) {
	scriptDir := t.TempDir()
	project := t.TempDir()
	sessionID := "cap-test-uuid"
	bin, _ := writeFakeClaude(t, scriptDir, fakeClaudeOpts{stdout: successJSON(sessionID)})

	a := newDriver(t, bin)
	defer a.Close()

	// Stage a JSONL with 5 Read calls; cap is 2.
	jsonl := `{"type":"assistant","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[`
	for i := 0; i < 5; i++ {
		if i > 0 {
			jsonl += ","
		}
		jsonl += fmt.Sprintf(`{"type":"tool_use","id":"tu%d","name":"Read","input":{"file_path":"x"}}`, i)
	}
	jsonl += "]}}\n"
	stageJSONL(t, a.cfg.DataDir, project, sessionID, jsonl)

	conv := &conversation{convID: ConvID(sessionID), project: project, systemPrompt: "x"}
	tx, _, _ := a.runClaude(context.Background(), runReq{
		conv: conv, prompt: "hi", budget: Budget{MaxToolCalls: 2},
	})
	if tx.StopReason != StopReasonMaxToolCalls {
		t.Errorf("StopReason=%v, want max_tool_calls", tx.StopReason)
	}
	if tx.ToolCallCount != 5 {
		t.Errorf("ToolCallCount=%d (recorded count), want 5", tx.ToolCallCount)
	}
}

func TestRunClaude_TimeoutKillsSubprocess(t *testing.T) {
	scriptDir := t.TempDir()
	bin, _ := writeFakeClaude(t, scriptDir, fakeClaudeOpts{
		stdout:       successJSON("timeout-test"),
		sleepSeconds: 5, // way longer than the budget
	})
	a := newDriver(t, bin)
	defer a.Close()

	start := time.Now()
	tx, err := a.OneShot(context.Background(), OneShotRequest{
		Prompt:  "hi",
		Project: t.TempDir(),
		Budget:  Budget{Timeout: 200 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("OneShot: %v", err)
	}
	elapsed := time.Since(start)
	if tx.StopReason != StopReasonTimeout {
		t.Errorf("StopReason=%v, want timeout", tx.StopReason)
	}
	// Must have actually killed the subprocess; if it ran the full 5s
	// our context deadline didn't propagate.
	if elapsed > 4*time.Second {
		t.Errorf("subprocess apparently ran the full sleep; elapsed=%v", elapsed)
	}
}

// -----------------------------------------------------------------------
// claude binary lifecycle

func TestNew_RejectsMissingClaudeBin(t *testing.T) {
	_, err := New(Config{ClaudeBin: "/no/such/path/please/no"})
	if !errors.Is(err, ErrClaudeBinMissing) {
		t.Errorf("err=%v, want ErrClaudeBinMissing", err)
	}
}

func TestNew_AcceptsExecutableOverride(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	a, err := New(Config{ClaudeBin: bin, PeerName: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.(Closer).Close()
}

func TestNew_RejectsDirAsClaudeBin(t *testing.T) {
	_, err := New(Config{ClaudeBin: t.TempDir()})
	if !errors.Is(err, ErrClaudeBinMissing) {
		t.Errorf("err=%v, want ErrClaudeBinMissing", err)
	}
}

func TestRunClaude_HandlesClaudeMissingMidRun(t *testing.T) {
	dir := t.TempDir()
	bin, _ := writeFakeClaude(t, dir, fakeClaudeOpts{stdout: successJSON("ok")})
	a := newDriver(t, bin)
	defer a.Close()

	// Yank the binary out from under us between New and runClaude.
	if err := os.Remove(bin); err != nil {
		t.Fatalf("remove fake: %v", err)
	}

	tx, err := a.OneShot(context.Background(), OneShotRequest{
		Prompt:  "hi",
		Project: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("OneShot: %v", err)
	}
	if tx.StopReason != StopReasonError {
		t.Errorf("StopReason=%v, want error", tx.StopReason)
	}
	if tx.ErrorSummary != ErrClaudeMissing {
		t.Errorf("ErrorSummary=%v, want claude_missing", tx.ErrorSummary)
	}
}

// -----------------------------------------------------------------------
// Env-API-key handling

func TestBuildEnv_StripsByDefault(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	a := &cliAgent{cfg: Config{}}
	handling, env := a.buildEnv()
	if handling != "stripped" {
		t.Errorf("handling=%q, want stripped", handling)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Errorf("env still contains ANTHROPIC_API_KEY: %q", kv)
		}
	}
}

func TestBuildEnv_KeepsWhenOptedIn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	a := &cliAgent{cfg: Config{KeepEnvAPIKey: true}}
	handling, env := a.buildEnv()
	if handling != "honored" {
		t.Errorf("handling=%q, want honored", handling)
	}
	found := false
	for _, kv := range env {
		if kv == "ANTHROPIC_API_KEY=sk-ant-test" {
			found = true
		}
	}
	if !found {
		t.Errorf("env missing ANTHROPIC_API_KEY when --agent-keep-env-key is set")
	}
}

func TestBuildEnv_ReportsAbsentWhenUnset(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	a := &cliAgent{cfg: Config{}}
	handling, _ := a.buildEnv()
	if handling != "absent" {
		t.Errorf("handling=%q, want absent", handling)
	}
}

// -----------------------------------------------------------------------
// classifyErrorMsg + mapClaudeStopReason corner cases

func TestClassifyErrorMsg(t *testing.T) {
	cases := []struct {
		msg  string
		want ErrorSummary
	}{
		{"HTTP 401 unauthorized", ErrAPIAuth},
		{"not logged in", ErrAPIAuth},
		{"rate limit exceeded", ErrAPIRateLimit},
		{"5" + "00 internal error", ErrAPIUnavailable},
		{"context deadline exceeded", ErrTimeout},
		{"dial tcp: connection refused", ErrNetwork},
		{"surprising other error", ErrOther},
	}
	for _, c := range cases {
		if got := classifyErrorMsg(c.msg); got != c.want {
			t.Errorf("classifyErrorMsg(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

// -----------------------------------------------------------------------
// Misc

func TestNewSessionUUID_LooksLikeV4(t *testing.T) {
	id := newSessionUUID()
	// Format: 8-4-4-4-12 lowercase hex.
	if len(id) != 36 {
		t.Errorf("len(id)=%d, want 36", len(id))
	}
	if id[14] != '4' {
		t.Errorf("version nibble = %c, want '4' (UUIDv4)", id[14])
	}
}

func TestMaybeWithBudgetNudge_NoEnforcement(t *testing.T) {
	out := maybeWithBudgetNudge("", Budget{MaxTokens: 1000})
	if !strings.Contains(out, "soft budget") {
		t.Errorf("expected soft-budget nudge in: %q", out)
	}
	if maybeWithBudgetNudge("orig", Budget{}) != "orig" {
		t.Errorf("zero budget should pass through unchanged")
	}
}

func TestRunClaude_HonorsContextCanceled(t *testing.T) {
	scriptDir := t.TempDir()
	bin, _ := writeFakeClaude(t, scriptDir, fakeClaudeOpts{
		stdout:       successJSON("cancel-test"),
		sleepSeconds: 5,
	})
	a := newDriver(t, bin)
	defer a.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	conv := &conversation{convID: "cancel-test", project: t.TempDir(), systemPrompt: "x"}
	tx, _, _ := a.runClaude(ctx, runReq{conv: conv, prompt: "hi", budget: Budget{}})
	if tx.StopReason != StopReasonShutdown && tx.StopReason != StopReasonTimeout {
		// Race-conditional: depending on which fires first.  Either
		// signals cancellation propagation worked.
		t.Errorf("StopReason=%v, want shutdown or timeout", tx.StopReason)
	}
}

func TestRunClaude_NonZeroExitWithGarbageStderr(t *testing.T) {
	scriptDir := t.TempDir()
	bin := filepath.Join(scriptDir, "claude")
	script := `#!/bin/sh
echo "not even json on stderr — oops" >&2
exit 17
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	a := newDriver(t, bin)
	defer a.Close()

	tx, err := a.OneShot(context.Background(), OneShotRequest{
		Prompt:  "hi",
		Project: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("OneShot: %v", err)
	}
	if tx.StopReason != StopReasonError {
		t.Errorf("StopReason=%v, want error", tx.StopReason)
	}
	if !strings.Contains(tx.Markdown, "claude --print failed") {
		t.Errorf("Markdown should describe the non-JSON failure; got %q", tx.Markdown)
	}
}

// Unmarshalable input should round-trip cleanly through unmarshalContentBlocks.
func TestUnmarshalContentBlocks_EmptyAndInvalid(t *testing.T) {
	if blocks, err := unmarshalContentBlocks(nil); blocks != nil || err != nil {
		t.Errorf("nil input: blocks=%v err=%v", blocks, err)
	}
	if _, err := unmarshalContentBlocks(json.RawMessage(`not json`)); err == nil {
		t.Errorf("invalid input should error")
	}
}
