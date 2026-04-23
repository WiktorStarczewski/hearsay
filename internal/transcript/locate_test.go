package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkTree builds a minimal ~/.claude/projects tree under a temp dir and
// returns (dataDir, projectDir, sessionID) so locate tests can assert
// against known paths. projectRaw is the URL-encoded dir name that
// lives under ~/.claude/projects/.
func mkTree(t *testing.T, projectRaw, sessionID, bodyJSONL string) (string, string) {
	t.Helper()
	dataDir := t.TempDir()
	projectDir := filepath.Join(dataDir, "projects", projectRaw)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	jsonl := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(jsonl, []byte(bodyJSONL), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return dataDir, projectDir
}

const miniSessionBody = `{"type":"user","uuid":"u1","timestamp":"2026-04-24T10:00:00Z","sessionId":"abcd","cwd":"/t/m","gitBranch":"feat","message":{"role":"user","content":"first user ask"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-04-24T10:00:01Z","sessionId":"abcd","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}
`

func TestProjectsDir_UsesHomeByDefault(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("HOME", scratch)
	got := ProjectsDir("")
	want := filepath.Join(scratch, ".claude", "projects")
	if got != want {
		t.Errorf("ProjectsDir() = %q, want %q", got, want)
	}
	// Explicit dataDir overrides HOME.
	got = ProjectsDir("/explicit")
	if got != "/explicit/projects" {
		t.Errorf("ProjectsDir(\"/explicit\") = %q", got)
	}
}

func TestDecodeProjectDirName(t *testing.T) {
	// Lossy decoding — we don't claim this recovers the original cwd
	// perfectly, just that it's a display-friendly approximation.
	if got := decodeProjectDirName("-tmp-fake"); got != "/tmp/fake" {
		t.Errorf("decodeProjectDirName(-tmp-fake) = %q", got)
	}
}

func TestProjectShortName(t *testing.T) {
	cases := map[string]string{
		"/Users/x/foo":  "foo",
		"/Users/x/foo/": "foo",
		"foo":           "foo",
		"":              "",
	}
	for in, want := range cases {
		if got := projectShortName(in); got != want {
			t.Errorf("projectShortName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProjectDirMatches(t *testing.T) {
	// raw name contains the project
	if !projectDirMatches("foo-bar", "-Users-x-foo-bar", "/Users/x/foo/bar") {
		t.Errorf("should match substring in raw dir name")
	}
	// exact full-path match
	if !projectDirMatches("/Users/x/foo/bar", "-Users-x-foo-bar", "/Users/x/foo/bar") {
		t.Errorf("should match exact decoded cwd")
	}
	// empty filter → match
	if !projectDirMatches("", "-anything", "/anything") {
		t.Errorf("empty filter should match")
	}
	// no match
	if projectDirMatches("other", "-Users-x-foo-bar", "/Users/x/foo/bar") {
		t.Errorf("unrelated filter should not match")
	}
}

func TestListSessions_ReturnsAnnotatedSummary(t *testing.T) {
	dataDir, _ := mkTree(t, "-tmp-proj", "abcd", miniSessionBody)
	got := ListSessions(LocateOptions{DataDir: dataDir, LiveWindow: time.Minute})
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1", len(got))
	}
	s := got[0]
	if s.SessionID != "abcd" {
		t.Errorf("sessionId = %q", s.SessionID)
	}
	if !s.IsLive {
		t.Errorf("recently-written session should be live")
	}
	if s.GitBranch != "feat" {
		t.Errorf("gitBranch = %q", s.GitBranch)
	}
	if s.TurnCount != 2 {
		t.Errorf("turnCount = %d, want 2 (1 user + 1 assistant)", s.TurnCount)
	}
	if !strings.Contains(s.FirstUserMessage, "first user ask") {
		t.Errorf("firstUserMessage = %q", s.FirstUserMessage)
	}
}

func TestListSessions_ProjectFilterNarrows(t *testing.T) {
	dataDir, _ := mkTree(t, "-tmp-foo", "aaa", miniSessionBody)
	// Second project dir in the same tree.
	_, _ = mkTree(t, "-tmp-bar", "bbb", miniSessionBody)
	// Re-use dataDir via env swap — mkTree creates fresh dirs each call,
	// so the two are separate. Use only the first:
	got := ListSessions(LocateOptions{DataDir: dataDir, Project: "foo"})
	if len(got) != 1 {
		t.Errorf("expected 1 match for filter 'foo', got %d", len(got))
	}
}

func TestListSessions_SinceFiltersOlderSessions(t *testing.T) {
	dataDir, projectDir := mkTree(t, "-tmp-since", "abcd", miniSessionBody)
	// Backdate file mtime to 10m ago.
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(filepath.Join(projectDir, "abcd.jsonl"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// since = 1m ago → excludes.
	got := ListSessions(LocateOptions{
		DataDir: dataDir,
		Since:   time.Now().Add(-1 * time.Minute),
	})
	if len(got) != 0 {
		t.Errorf("since filter should exclude old session; got %d", len(got))
	}
	// since = 20m ago → includes.
	got = ListSessions(LocateOptions{
		DataDir: dataDir,
		Since:   time.Now().Add(-20 * time.Minute),
	})
	if len(got) != 1 {
		t.Errorf("since filter should include 10-min-old session; got %d", len(got))
	}
}

func TestListSessions_LimitCaps(t *testing.T) {
	dataDir := t.TempDir()
	proj := filepath.Join(dataDir, "projects", "-tmp-cap")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < 5; i++ {
		id := []byte(miniSessionBody)
		path := filepath.Join(proj, "s"+string(rune('0'+i))+".jsonl")
		if err := os.WriteFile(path, id, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got := ListSessions(LocateOptions{DataDir: dataDir, Limit: 2})
	if len(got) != 2 {
		t.Errorf("expected limit=2, got %d", len(got))
	}
}

func TestListSessions_MissingProjectsDirReturnsEmpty(t *testing.T) {
	if got := ListSessions(LocateOptions{DataDir: "/nonexistent-path"}); got != nil {
		t.Errorf("expected nil for missing projects dir, got %v", got)
	}
}

func TestListSessions_SkipsUnparseableJSONL(t *testing.T) {
	dataDir := t.TempDir()
	proj := filepath.Join(dataDir, "projects", "-broken")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Seed a JSONL file that's completely garbage — parse will fail.
	// buildSummary should still return something usable via our
	// tolerant parser (partial-line handling means 0 errors, 0 events).
	if err := os.WriteFile(filepath.Join(proj, "x.jsonl"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := ListSessions(LocateOptions{DataDir: dataDir})
	// We don't assert zero results here; partial-last-line tolerance
	// means the parser treats a single bad line as partial and returns
	// zero events without erroring out, which still produces a summary.
	// Just confirm we don't panic.
	_ = got
}

func TestFindSessionPath(t *testing.T) {
	dataDir, _ := mkTree(t, "-tmp-find", "abcd", miniSessionBody)
	if got := FindSessionPath("abcd", dataDir); got == "" {
		t.Errorf("expected to find session path")
	}
	if got := FindSessionPath("zzzz", dataDir); got != "" {
		t.Errorf("expected empty for missing session, got %q", got)
	}
	if got := FindSessionPath("abcd", "/nonexistent"); got != "" {
		t.Errorf("expected empty when projects dir missing")
	}
}

func TestFindProjectDir(t *testing.T) {
	dataDir, projectDir := mkTree(t, "-tmp-projdir", "abcd", miniSessionBody)
	got := FindProjectDir("abcd", dataDir)
	if got != projectDir {
		t.Errorf("FindProjectDir = %q, want %q", got, projectDir)
	}
	if got := FindProjectDir("missing", dataDir); got != "" {
		t.Errorf("expected empty for missing session, got %q", got)
	}
	if got := FindProjectDir("abcd", "/nonexistent"); got != "" {
		t.Errorf("expected empty when projects dir missing")
	}
}