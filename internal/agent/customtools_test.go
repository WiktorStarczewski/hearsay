package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readToolHandler / globToolHandler / grepToolHandler are pure
// filesystem operations — they don't talk to the SDK, so testing them
// in isolation is straightforward and gives us most of the coverage.

func newProjectFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"README.md":       "# hearsay\nA tiny tool.\nERROR foo.\n",
		"src/main.go":     "package main\nfunc main() { /* ERROR test */ }\n",
		"src/util.go":     "package main\nimport \"fmt\"\nfunc Util() { fmt.Println(\"ok\") }\n",
		"docs/notes.txt":  "Some notes\nERROR: pay attention here.\n",
		".hidden/secret":  "should not be visible to walks",
		"binary.bin":      "head\x00middle",
		"large/big.txt":   strings.Repeat("a", maxReadBytes+200),
	}
	for path, body := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

func TestReadToolHandler_BoundedFile(t *testing.T) {
	root := newProjectFixture(t)
	out, err := readToolHandler(map[string]any{"file_path": "README.md"}, root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "[file=README.md") {
		t.Errorf("missing metadata header: %q", out)
	}
	if !strings.Contains(out, "# hearsay") {
		t.Errorf("missing body: %q", out)
	}
	if !strings.Contains(out, "truncated=false") {
		t.Errorf("expected truncated=false: %q", out)
	}
}

func TestReadToolHandler_TruncatesLargeFile(t *testing.T) {
	root := newProjectFixture(t)
	out, err := readToolHandler(map[string]any{"file_path": "large/big.txt"}, root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "truncated=true") {
		t.Errorf("expected truncated=true; got header: %q", out[:200])
	}
	// Body length after the header should not exceed maxReadBytes.
	idx := strings.Index(out, "\n\n")
	if idx < 0 {
		t.Fatalf("no header separator")
	}
	body := out[idx+2:]
	if len(body) > maxReadBytes {
		t.Errorf("body=%d bytes, expected <= %d", len(body), maxReadBytes)
	}
}

func TestReadToolHandler_RejectsEscape(t *testing.T) {
	root := newProjectFixture(t)
	_, err := readToolHandler(map[string]any{"file_path": "../etc/passwd"}, root)
	if err == nil {
		t.Errorf("expected escape to be rejected")
	}
}

func TestReadToolHandler_MissingArg(t *testing.T) {
	root := newProjectFixture(t)
	_, err := readToolHandler(map[string]any{}, root)
	if err == nil {
		t.Errorf("expected missing-arg error")
	}
}

func TestReadToolHandler_NotFound(t *testing.T) {
	root := newProjectFixture(t)
	_, err := readToolHandler(map[string]any{"file_path": "no/such/file"}, root)
	if err == nil {
		t.Errorf("expected stat error")
	}
}

func TestReadToolHandler_DirIsAnError(t *testing.T) {
	root := newProjectFixture(t)
	_, err := readToolHandler(map[string]any{"file_path": "src"}, root)
	if err == nil {
		t.Errorf("expected directory to be rejected")
	}
}

func TestGlobToolHandler_FindsFiles(t *testing.T) {
	root := newProjectFixture(t)
	out, err := globToolHandler(map[string]any{"pattern": "**/*.go"}, root)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(out, "src/main.go") {
		t.Errorf("expected src/main.go in matches: %q", out)
	}
	if !strings.Contains(out, "src/util.go") {
		t.Errorf("expected src/util.go in matches: %q", out)
	}
	// Hidden dirs filtered out.
	if strings.Contains(out, ".hidden") {
		t.Errorf("hidden dir leaked into results: %q", out)
	}
}

func TestGlobToolHandler_MissingPattern(t *testing.T) {
	root := newProjectFixture(t)
	_, err := globToolHandler(map[string]any{}, root)
	if err == nil {
		t.Errorf("expected missing-pattern error")
	}
}

func TestGrepToolHandler_FindsMatches(t *testing.T) {
	root := newProjectFixture(t)
	out, err := grepToolHandler(map[string]any{"pattern": "ERROR"}, root)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out, "README.md:") {
		t.Errorf("README hit missing: %q", out)
	}
	if !strings.Contains(out, "docs/notes.txt:") {
		t.Errorf("docs hit missing: %q", out)
	}
	if !strings.Contains(out, "src/main.go:") {
		t.Errorf("source hit missing: %q", out)
	}
	// Binary file should NOT show up.
	if strings.Contains(out, "binary.bin:") {
		t.Errorf("binary file should not be grepped: %q", out)
	}
}

func TestGrepToolHandler_InvalidRegex(t *testing.T) {
	root := newProjectFixture(t)
	_, err := grepToolHandler(map[string]any{"pattern": "[unterminated"}, root)
	if err == nil {
		t.Errorf("expected regex compile error")
	}
}

func TestGrepToolHandler_ScopedPath(t *testing.T) {
	root := newProjectFixture(t)
	out, err := grepToolHandler(map[string]any{
		"pattern": "ERROR",
		"path":    "src",
	}, root)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if strings.Contains(out, "README.md") {
		t.Errorf("scoped grep returned out-of-scope hit: %q", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("scoped grep missed in-scope hit: %q", out)
	}
}

func TestGrepToolHandler_InvalidScope(t *testing.T) {
	root := newProjectFixture(t)
	_, err := grepToolHandler(map[string]any{
		"pattern": "ERROR",
		"path":    "../escape",
	}, root)
	if err == nil {
		t.Errorf("expected escape-rejection error")
	}
}

func TestMatchGlob_BareBasename(t *testing.T) {
	if !matchGlob("*.go", "src/main.go", false) {
		t.Errorf("expected src/main.go to match *.go via basename fallback")
	}
}

func TestMatchGlob_DoublestarVariants(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"**/*.go", "src/main.go", true},
		{"**/*.go", "deep/nested/main.go", true},
		{"src/**/util.go", "src/x/y/util.go", true},
		{"src/**/util.go", "lib/util.go", false},
		{"src/**", "src/foo/bar.txt", true},
		{"**/no/such/path", "totally/different.go", false},
	}
	for _, c := range cases {
		got := matchGlob(c.pattern, c.name, true)
		if got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestGlobToolHandler_NoMatches(t *testing.T) {
	root := newProjectFixture(t)
	out, err := globToolHandler(map[string]any{"pattern": "**/no-such-file.xyz"}, root)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(out, "matches=0") {
		t.Errorf("expected matches=0; got %q", out)
	}
}

func TestGrepToolHandler_NoMatches(t *testing.T) {
	root := newProjectFixture(t)
	out, err := grepToolHandler(map[string]any{"pattern": "ZZZNEVER"}, root)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out, "matches=0") {
		t.Errorf("expected matches=0; got %q", out)
	}
}

func TestGrepToolHandler_RespectsMaxResults(t *testing.T) {
	dir := t.TempDir()
	// Many lines, all matching.
	body := strings.Repeat("hit\n", 50)
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := grepToolHandler(map[string]any{
		"pattern":     "hit",
		"max_results": float64(3),
	}, dir)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out, "matches=3") || !strings.Contains(out, "truncated=true") {
		t.Errorf("expected truncation at 3 matches; got %q", out)
	}
}

func TestResolveUnderRoot_CleansRelative(t *testing.T) {
	root := t.TempDir()
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootResolved = root
	}
	full, err := resolveUnderRoot(root, "subdir/../file.txt")
	if err != nil {
		t.Fatalf("resolveUnderRoot: %v", err)
	}
	if !strings.HasPrefix(full, rootResolved) {
		t.Errorf("expected resolved path under root (%q): %q", rootResolved, full)
	}
}

func TestIsLikelyText(t *testing.T) {
	if !isLikelyText([]byte("hello world\n")) {
		t.Errorf("expected text to be detected as text")
	}
	if isLikelyText([]byte("hello\x00world")) {
		t.Errorf("expected NUL-bearing data to be detected as binary")
	}
}
