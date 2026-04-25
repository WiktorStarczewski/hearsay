package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// -----------------------------------------------------------------------
// Test fixtures.
//
// Rather than spin up a real MCP server we call the handler functions
// directly. The handlers accept (ctx, *CallToolRequest, Input) and
// return (*CallToolResult, Output, error) — all ordinary Go types.
// We build a tiny ~/.claude/projects tree and point ToolContext.DataDir
// at it.

// buildFakeClaudeTree creates a ~/.claude/projects layout with one
// active session. The session JSONL, subagent JSONL, and tool-results
// sidecar mirror what Claude Code writes in reality, so tools see a
// realistic surface. Returns the data-dir root (analogous to ~/.claude).
func buildFakeClaudeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dataDir := root                         // serves as ~/.claude
	projects := filepath.Join(dataDir, "projects")
	projectDir := filepath.Join(projects, "-Users-celrisen-foo-bar-baz")
	sessionID := "11111111-2222-3333-4444-555555555555"
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	subagentsDir := filepath.Join(projectDir, sessionID, "subagents")
	toolResultsDir := filepath.Join(projectDir, sessionID, "tool-results")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	if err := os.MkdirAll(toolResultsDir, 0o755); err != nil {
		t.Fatalf("mkdir tool-results: %v", err)
	}

	// Out-of-line sidecar: a Read output large enough to have been spilled.
	sidecarPath := filepath.Join(toolResultsDir, "abc12345.txt")
	if err := os.WriteFile(sidecarPath, []byte("SIDECAR-CONTENT\nline two\n"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	// Main session with: user prompt, assistant Read tool_use, user
	// tool_result referencing the sidecar path inline, assistant Agent
	// subagent spawn, assistant final text, and an unknown event type
	// to exercise forward-compat rendering.
	sidecarMarkerLine := sidecarPath // absolute path embedded in the tool_result content text
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-04-24T10:00:00.000Z","sessionId":"` + sessionID + `","cwd":"/tmp/fake","gitBranch":"main","version":"2.1.0","message":{"role":"user","content":"hello hearsay"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-04-24T10:00:01.000Z","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"I will Read a file."},{"type":"tool_use","id":"toolu_read1","name":"Read","input":{"file_path":"/tmp/fake/foo.txt"}}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-04-24T10:00:02.000Z","sessionId":"` + sessionID + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_read1","content":"` + sidecarMarkerLine + `\n\nPreview (first 2KB):\nSIDECAR-CONTENT"}]}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","timestamp":"2026-04-24T10:00:03.000Z","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"agent-01","name":"Agent","input":{"subagent_type":"Explore","description":"search for errors","prompt":"..."}}]}}`,
		`{"type":"foobar","uuid":"fx","sessionId":"` + sessionID + `","timestamp":"2026-04-24T10:00:04.000Z","payload":"future event"}`,
		`{"type":"assistant","uuid":"a3","parentUuid":"a2","timestamp":"2026-04-24T10:00:05.000Z","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"All done — an error was located."}]}}`,
	}
	if err := os.WriteFile(jsonlPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write session jsonl: %v", err)
	}

	// Subagent: one user/assistant pair, file keyed by agent id so the
	// resolver's primary path finds it.
	subagentJSONL := filepath.Join(subagentsDir, "agent-01.jsonl")
	subagentMeta := filepath.Join(subagentsDir, "agent-01.meta.json")
	subagentLines := []string{
		`{"type":"user","uuid":"su1","timestamp":"2026-04-24T10:00:03.500Z","message":{"role":"user","content":"search for errors"}}`,
		`{"type":"assistant","uuid":"sa1","parentUuid":"su1","timestamp":"2026-04-24T10:00:03.600Z","message":{"role":"assistant","content":[{"type":"text","text":"Found one error in foo.go."}]}}`,
	}
	if err := os.WriteFile(subagentJSONL, []byte(strings.Join(subagentLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write subagent jsonl: %v", err)
	}
	if err := os.WriteFile(subagentMeta, []byte(`{"agentType":"Explore","description":"search for errors"}`), 0o644); err != nil {
		t.Fatalf("write subagent meta: %v", err)
	}

	return dataDir
}

// testCtx builds a ToolContext pointed at a fresh fake-claude tree.
func testCtx(t *testing.T) Context {
	return Context{
		PeerName:    "ivan",
		PeerVersion: "v0.0.1",
		DataDir:     buildFakeClaudeTree(t),
		LiveWindow:  10 * time.Second, // wide enough that a just-written fixture counts as live
		Log:         func(tool, status string, dur time.Duration) { /* quiet */ },
	}
}

// -----------------------------------------------------------------------
// Registration smoke: Register must wire up exactly 8 tools without panic.

func TestRegister_RegistersAllEightTools(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	Register(srv, testCtx(t))
	// There isn't a public list-tools accessor on mcp.Server, so we
	// assert indirectly via a successful Register() (it panics on
	// duplicate name) plus a round-trip against an in-memory client.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	count := 0
	for range cs.Tools(ctx, nil) {
		count++
	}
	if count != 8 {
		t.Errorf("expected 8 tools registered, got %d", count)
	}
}

// -----------------------------------------------------------------------
// In-memory client driver: connect a paired client so every handler
// goes through the real MCP code path (schema validation, content
// marshalling, structured-output packaging).

func connectPair(t *testing.T) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-hearsay", Version: "0"}, nil)
	Register(srv, testCtx(t))
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// structuredDecode pulls .StructuredContent out of a CallToolResult into
// a caller-supplied destination (map or typed struct).
func structuredDecode(t *testing.T, res *mcp.CallToolResult, dst any) {
	t.Helper()
	if res == nil {
		t.Fatalf("nil CallToolResult")
	}
	if res.StructuredContent == nil {
		t.Fatalf("no structured content in result; content=%+v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("re-marshal structured: %v", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode structured: %v", err)
	}
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

// -----------------------------------------------------------------------
// Tool-by-tool exercises.

func TestGetPeerInfo_ReportsNameAndCounts(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "get_peer_info", map[string]any{})
	var out GetPeerInfoOutput
	structuredDecode(t, res, &out)
	if out.Name != "ivan" {
		t.Errorf("Name = %q", out.Name)
	}
	if out.Version != "v0.0.1" {
		t.Errorf("Version = %q", out.Version)
	}
	if out.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1", out.SessionCount)
	}
	if out.ActiveSessionCount != 1 {
		t.Errorf("ActiveSessionCount = %d, want 1", out.ActiveSessionCount)
	}
}

func TestListSessions_ReturnsFixtureSession(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "list_sessions", map[string]any{})
	var out ListSessionsOutput
	structuredDecode(t, res, &out)
	if len(out.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(out.Sessions))
	}
	if !out.Sessions[0].IsLive {
		t.Errorf("fixture session should be flagged live")
	}
	if !strings.HasPrefix(out.Sessions[0].FirstUserMessage, "hello hearsay") {
		t.Errorf("first user preview unexpected: %q", out.Sessions[0].FirstUserMessage)
	}
}

func TestListSessions_ProjectFilterMatchesRawDirName(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "list_sessions", map[string]any{"project": "foo-bar-baz"})
	var out ListSessionsOutput
	structuredDecode(t, res, &out)
	if len(out.Sessions) != 1 {
		t.Errorf("filter should hit the one fixture session, got %d", len(out.Sessions))
	}
}

func TestListSessions_SinceFilter(t *testing.T) {
	cs := connectPair(t)
	// Far-future `since` → no matches.
	far := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	res := callTool(t, cs, "list_sessions", map[string]any{"since": far})
	var out ListSessionsOutput
	structuredDecode(t, res, &out)
	if len(out.Sessions) != 0 {
		t.Errorf("far-future since should filter out everything, got %d", len(out.Sessions))
	}
}

func TestGetCurrentSession_Unambiguous(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "get_current_session", map[string]any{})
	var out GetCurrentSessionOutput
	structuredDecode(t, res, &out)
	if out.Ambiguous {
		t.Errorf("single-session fixture should be unambiguous")
	}
	if out.Session == nil {
		t.Fatalf("session should be present")
	}
	if out.Session.SessionID == "" {
		t.Errorf("missing session id")
	}
}

func TestGetCurrentSession_EmptyWhenNoLive(t *testing.T) {
	// Override live window to a tiny duration so nothing is live.
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ctx := testCtx(t)
	ctx.LiveWindow = 1 * time.Nanosecond
	Register(srv, ctx)
	c, _ := connectServer(t, srv)
	res := callTool(t, c, "get_current_session", map[string]any{})
	var out GetCurrentSessionOutput
	structuredDecode(t, res, &out)
	if out.Ambiguous {
		t.Errorf("no-live-session case should not be ambiguous")
	}
	if out.Session != nil {
		t.Errorf("expected nil session when none are live")
	}
}

// connectServer is like connectPair but accepts an externally-built
// server (so tests can supply a non-default ToolContext).
func connectServer(t *testing.T, srv *mcp.Server) (*mcp.ClientSession, *mcp.Server) {
	t.Helper()
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, srv
}

// Multi-live case: seed a second session with a recent mtime.
func TestGetCurrentSession_AmbiguousWithMultipleLive(t *testing.T) {
	tctx := testCtx(t)
	// Add a second live session in the same project dir.
	projectDir := filepath.Join(tctx.DataDir, "projects", "-Users-celrisen-foo-bar-baz")
	secondID := "99999999-8888-7777-6666-555555555555"
	secondLine := `{"type":"user","uuid":"u1","timestamp":"2026-04-24T10:05:00Z","sessionId":"` + secondID + `","message":{"role":"user","content":"second hello"}}`
	if err := os.WriteFile(filepath.Join(projectDir, secondID+".jsonl"), []byte(secondLine+"\n"), 0o644); err != nil {
		t.Fatalf("seed second session: %v", err)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	Register(srv, tctx)
	c, _ := connectServer(t, srv)
	res := callTool(t, c, "get_current_session", map[string]any{})
	var out GetCurrentSessionOutput
	structuredDecode(t, res, &out)
	if !out.Ambiguous {
		t.Errorf("expected ambiguous=true with two live sessions")
	}
	if len(out.Candidates) != 2 {
		t.Errorf("expected 2 candidates, got %d", len(out.Candidates))
	}
}

func TestReadSession_ReturnsMarkdownAndMetadata(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_session", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
	})
	if len(res.Content) == 0 {
		t.Fatalf("expected markdown content block")
	}
	md := textOf(t, res.Content[0])
	if !strings.Contains(md, "hello hearsay") {
		t.Errorf("markdown missing first user message; got: %s", md)
	}
	if !strings.Contains(md, "**Tool:** `Read`") {
		t.Errorf("markdown missing tool-use rendering")
	}
	if !strings.Contains(md, "**→ Explore subagent**") {
		t.Errorf("markdown missing subagent rendering")
	}
	var meta ReadSessionOutput
	structuredDecode(t, res, &meta)
	if meta.TotalTurns < 3 {
		t.Errorf("totalTurns=%d, expected >= 3", meta.TotalTurns)
	}
}

func TestReadSession_JSONFormatOmitsMarkdown(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_session", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
		"format":    "json",
	})
	// In json mode the handler returns nil CallToolResult; the SDK
	// populates Content from the structured output.
	if res.IsError {
		t.Errorf("json mode returned isError")
	}
}

func TestReadSession_UnknownSessionErrors(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_session", map[string]any{"sessionId": "no-such-session"})
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown sessionId")
	}
}

func TestSearchSession_FindsMatch(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "search_session", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
		"query":     "error",
	})
	var out SearchSessionOutput
	structuredDecode(t, res, &out)
	if out.TotalMatches == 0 {
		t.Errorf("expected at least one match for 'error'")
	}
}

func TestSearchSession_UnknownSessionErrors(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "search_session", map[string]any{
		"sessionId": "missing",
		"query":     "whatever",
	})
	if !res.IsError {
		t.Errorf("expected IsError=true")
	}
}

func TestReadSubagent_ReturnsMarkdown(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_subagent", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
		"agentUuid": "agent-01",
	})
	if res.IsError {
		t.Errorf("unexpected isError: content=%+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatalf("no content block")
	}
	if !strings.Contains(textOf(t, res.Content[0]), "Found one error in foo.go") {
		t.Errorf("subagent markdown missing its body")
	}
}

func TestReadSubagent_UnknownErrors(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_subagent", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
		"agentUuid": "nope",
	})
	if !res.IsError {
		t.Errorf("expected IsError=true")
	}
}

func TestReadToolResult_ReadsSidecar(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_tool_result", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
		"toolUseId": "toolu_read1",
	})
	if res.IsError {
		var msgs []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msgs = append(msgs, tc.Text)
			}
		}
		t.Fatalf("tool_result errored: %s", strings.Join(msgs, " | "))
	}
	txt := textOf(t, res.Content[0])
	header, body, ok := splitMetadataHeader(txt)
	if !ok {
		t.Fatalf("response missing inlined metadata header: %q", txt)
	}
	if header.source != "sidecar" {
		t.Errorf("header.source = %q, want sidecar", header.source)
	}
	if header.truncated {
		t.Errorf("header.truncated = true unexpectedly")
	}
	if header.bytes != len(body) {
		t.Errorf("header.bytes = %d, body length = %d", header.bytes, len(body))
	}
	if !strings.Contains(body, "SIDECAR-CONTENT") {
		t.Errorf("sidecar body missing expected content: %q", body)
	}
}

func TestReadToolResult_TruncatesWhenOverMaxBytes(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_tool_result", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
		"toolUseId": "toolu_read1",
		"maxBytes":  5,
	})
	if res.IsError {
		t.Fatalf("read_tool_result errored: content=%+v", res.Content)
	}
	txt := textOf(t, res.Content[0])
	header, body, ok := splitMetadataHeader(txt)
	if !ok {
		t.Fatalf("response missing inlined metadata header: %q", txt)
	}
	if !header.truncated {
		t.Errorf("expected truncated=true with maxBytes=5")
	}
	if !strings.Contains(body, "[truncated at 5 bytes]") {
		t.Errorf("body missing truncation marker: %q", body)
	}
}

func TestReadToolResult_NoStructuredContent(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_tool_result", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
		"toolUseId": "toolu_read1",
	})
	// PR 0 deliberately drops the populated StructuredContent block from
	// this tool — metadata is inlined into the body header instead. The
	// SDK may still emit `{}` for an empty struct, but it must not carry
	// the old {source, truncated, bytes} fields back to the caller.
	if res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal structured: %v", err)
		}
		// Empty struct serializes to "{}". Anything richer means the
		// PR-0 inlining regressed.
		if s := string(raw); s != "{}" && s != "null" {
			t.Errorf("read_tool_result must not return populated StructuredContent; got %s", s)
		}
	}
}

func TestReadToolResult_UnknownErrors(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_tool_result", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
		"toolUseId": "not-a-real-id",
	})
	if !res.IsError {
		t.Errorf("expected IsError=true")
	}
}

func TestReadToolResult_UnknownSessionErrors(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "read_tool_result", map[string]any{
		"sessionId": "missing",
		"toolUseId": "toolu_read1",
	})
	if !res.IsError {
		t.Errorf("expected IsError=true")
	}
}

func TestGetSessionSummary_CountsAndLastText(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "get_session_summary", map[string]any{
		"sessionId": "11111111-2222-3333-4444-555555555555",
	})
	var out GetSessionSummaryOutput
	structuredDecode(t, res, &out)
	if out.ToolCallCount < 2 {
		t.Errorf("toolCallCount=%d, want >= 2", out.ToolCallCount)
	}
	if len(out.Subagents) != 1 {
		t.Errorf("subagents=%d, want 1", len(out.Subagents))
	}
	if !strings.Contains(out.LastAssistantText, "All done") {
		t.Errorf("lastAssistantText missing final text: %q", out.LastAssistantText)
	}
}

func TestGetSessionSummary_UnknownErrors(t *testing.T) {
	cs := connectPair(t)
	res := callTool(t, cs, "get_session_summary", map[string]any{"sessionId": "missing"})
	if !res.IsError {
		t.Errorf("expected IsError=true")
	}
}

// Directly exercise capName's early-return branch (name="").
func TestCapName_Empty(t *testing.T) {
	if got := capName(""); got != "" {
		t.Errorf("capName(\"\") = %q", got)
	}
}

func TestUnmarshalBlocks_EmptyInput(t *testing.T) {
	blocks, err := unmarshalBlocks(nil)
	if err != nil {
		t.Errorf("empty input shouldn't error: %v", err)
	}
	if blocks != nil {
		t.Errorf("expected nil blocks for empty input")
	}
}

// textOf extracts the .Text field from a TextContent block. The Content
// interface is implemented by several types; for our tool outputs
// markdown always arrives as *mcp.TextContent.
func textOf(t *testing.T, c mcp.Content) string {
	t.Helper()
	tc, ok := c.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", c)
	}
	return tc.Text
}

// readToolResultHeader captures the parsed leading line that PR 0 inlines
// into read_tool_result responses: `[source=<sidecar|inline>, bytes=N, truncated=<bool>]`.
type readToolResultHeader struct {
	source    string
	bytes     int
	truncated bool
}

// splitMetadataHeader parses the inlined metadata header that PR 0
// prepends to every read_tool_result body, separated from the body by a
// blank line. Returns ok=false if the input does not match the expected
// shape — caller decides whether that's a fatal test failure.
func splitMetadataHeader(s string) (readToolResultHeader, string, bool) {
	const sep = "\n\n"
	idx := strings.Index(s, sep)
	if idx < 0 {
		return readToolResultHeader{}, "", false
	}
	line := s[:idx]
	body := s[idx+len(sep):]
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return readToolResultHeader{}, "", false
	}
	inner := line[1 : len(line)-1] // strip [ ]
	var h readToolResultHeader
	for _, kv := range strings.Split(inner, ", ") {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			return readToolResultHeader{}, "", false
		}
		k, v := kv[:eq], kv[eq+1:]
		switch k {
		case "source":
			h.source = v
		case "bytes":
			n, err := strconv.Atoi(v)
			if err != nil {
				return readToolResultHeader{}, "", false
			}
			h.bytes = n
		case "truncated":
			switch v {
			case "true":
				h.truncated = true
			case "false":
				h.truncated = false
			default:
				return readToolResultHeader{}, "", false
			}
		}
	}
	return h, body, true
}