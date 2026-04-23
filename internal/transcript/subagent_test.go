package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// subagentTree lays out a parent-session + subagents/ directory with a
// JSONL using the "agent-<uuid>.jsonl" filename convention and a
// companion .meta.json.
func subagentTree(t *testing.T, prefix, agentID string, withMeta bool) (dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	sessionID := "parent-1"
	proj := filepath.Join(dataDir, "projects", "-tmp-sub")
	subdir := filepath.Join(proj, sessionID, "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	// Minimal parent session so FindProjectDir can resolve it.
	if err := os.WriteFile(filepath.Join(proj, sessionID+".jsonl"), []byte(`{"type":"user","sessionId":"parent-1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("parent session: %v", err)
	}
	subLine := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"subagent output"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(subdir, prefix+agentID+".jsonl"), []byte(subLine), 0o644); err != nil {
		t.Fatalf("subagent jsonl: %v", err)
	}
	if withMeta {
		meta := `{"agentType":"Explore","description":"test subagent"}`
		if err := os.WriteFile(filepath.Join(subdir, prefix+agentID+".meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatalf("meta: %v", err)
		}
	}
	return dataDir
}

// Exercises the primary path (exact filename match) + meta loading.
func TestResolveSubagent_PrimaryPathWithMeta(t *testing.T) {
	dataDir := subagentTree(t, "agent-", "abc123", true)
	res := ResolveSubagent("parent-1", "abc123", dataDir)
	if res == nil {
		t.Fatalf("expected resolution")
	}
	if res.Meta == nil || res.Meta.AgentType != "Explore" {
		t.Errorf("meta not loaded: %+v", res.Meta)
	}
}

// Exercises the no-meta-file path.
func TestResolveSubagent_NoMetaFile(t *testing.T) {
	dataDir := subagentTree(t, "agent-", "abc123", false)
	res := ResolveSubagent("parent-1", "abc123", dataDir)
	if res == nil {
		t.Fatalf("expected resolution")
	}
	if res.Meta != nil {
		t.Errorf("meta should be nil when file is absent")
	}
}

// Exercises the substring-match fallback (filename doesn't match either
// primary convention exactly, but includes the agent UUID).
func TestResolveSubagent_FallbackSubstringMatch(t *testing.T) {
	dataDir := subagentTree(t, "prefix-weird-", "xyz789", false)
	res := ResolveSubagent("parent-1", "xyz789", dataDir)
	if res == nil {
		t.Fatalf("fallback substring match should resolve")
	}
}

func TestResolveSubagent_UnknownSessionReturnsNil(t *testing.T) {
	dataDir := t.TempDir() // no projects dir
	if got := ResolveSubagent("missing", "whatever", dataDir); got != nil {
		t.Errorf("expected nil for missing session")
	}
}

func TestResolveSubagent_MissingSubagentsDirReturnsNil(t *testing.T) {
	// Parent session exists but no subagents/ dir.
	dataDir := t.TempDir()
	proj := filepath.Join(dataDir, "projects", "-tmp-x")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatalf("session: %v", err)
	}
	if got := ResolveSubagent("s", "a", dataDir); got != nil {
		t.Errorf("expected nil when subagents dir absent")
	}
}

func TestResolveSubagent_NoMatchAtAllReturnsNil(t *testing.T) {
	dataDir := subagentTree(t, "agent-", "exists", false)
	if got := ResolveSubagent("parent-1", "some-other-id", dataDir); got != nil {
		t.Errorf("expected nil for non-matching agent id")
	}
}

// -----------------------------------------------------------------------
// LocateToolResult — inline vs sidecar branches.

func TestLocateToolResult_InlineContent(t *testing.T) {
	dataDir, proj := mkTree(t, "-tmp-inline", "aaa", "") // empty session; we'll overwrite
	// Rewrite with an inline tool_result.
	inlineBody := `{"type":"user","sessionId":"aaa","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tui-1","content":"just inline text"}]}}` + "\n"
	path := filepath.Join(proj, "aaa.jsonl")
	if err := os.WriteFile(path, []byte(inlineBody), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loc := LocateToolResult(path, "tui-1")
	if loc == nil {
		t.Fatalf("expected location")
	}
	if loc.Kind != ToolResultInline {
		t.Errorf("expected Inline, got %v", loc.Kind)
	}
	if !strings.Contains(loc.InlineText, "just inline text") {
		t.Errorf("inline text not captured: %q", loc.InlineText)
	}
	_ = dataDir
}

func TestLocateToolResult_MissingToolUseID(t *testing.T) {
	_, proj := mkTree(t, "-tmp-miss", "aaa", `{"type":"user","sessionId":"aaa","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"other","content":"x"}]}}`+"\n")
	path := filepath.Join(proj, "aaa.jsonl")
	if got := LocateToolResult(path, "not-present"); got != nil {
		t.Errorf("expected nil for missing tool_use_id")
	}
}

func TestLocateToolResult_MissingFileReturnsNil(t *testing.T) {
	if got := LocateToolResult("/nonexistent.jsonl", "anything"); got != nil {
		t.Errorf("expected nil for missing file")
	}
}

// Exercises extractText on the block-array variant (the other branches
// of extractText are covered by LocateToolResult's string-content path).
func TestExtractText_BlockArray(t *testing.T) {
	got := extractText([]any{
		map[string]any{"type": "text", "text": "first"},
		map[string]any{"type": "other", "text": "skip-me"},
		map[string]any{"type": "text", "text": "second"},
	})
	if got != "first\nsecond" {
		t.Errorf("extractText blocks = %q", got)
	}
	if got := extractText(map[string]any{"not": "valid"}); got != "" {
		t.Errorf("extractText on unsupported shape = %q", got)
	}
	if got := extractText(nil); got != "" {
		t.Errorf("extractText(nil) = %q", got)
	}
}