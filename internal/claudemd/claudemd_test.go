package claudemd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempFile returns a path inside t.TempDir() for test CLAUDE.md targets.
func tempFile(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// readFile is a terse test helper.
func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestDefaultPath_UsesHome(t *testing.T) {
	got := DefaultPath()
	if !strings.HasSuffix(got, "/.claude/CLAUDE.md") {
		t.Errorf("unexpected default path: %q", got)
	}
}

func TestPrint_RoundtripsByRole(t *testing.T) {
	consumer := Print(RoleConsumer)
	peer := Print(RolePeer)
	if !strings.Contains(consumer, "hearsay:consumer-auto-start") {
		t.Errorf("consumer block missing its start marker")
	}
	if !strings.Contains(peer, "hearsay:peer-auto-start") {
		t.Errorf("peer block missing its start marker")
	}
	if strings.Contains(consumer, "hearsay:peer-auto-start") {
		t.Errorf("consumer block must not contain peer markers")
	}
}

// Exercises edge case 1: target file doesn't exist → create.
func TestInstall_CreatesMissingFile(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	r, err := Install(RoleConsumer, path)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if r.Action != ActionCreated {
		t.Errorf("expected ActionCreated, got %s", r.Action)
	}
	content := readFile(t, path)
	if !strings.Contains(content, "hearsay:consumer-auto-start") {
		t.Errorf("block not written to new file")
	}
}

// Exercises edge case 2: both markers present → replace in place.
func TestInstall_ReplacesExistingBlock(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	// Seed file with an existing (older) block.
	seed := "# Top-level\n\n" +
		"<!-- hearsay:consumer-auto-start -->\nOLD BLOCK BODY\n<!-- hearsay:consumer-auto-end -->\n\n" +
		"# Bottom-level\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r, err := Install(RoleConsumer, path)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if r.Action != ActionReplaced {
		t.Errorf("expected ActionReplaced, got %s", r.Action)
	}
	content := readFile(t, path)
	if strings.Contains(content, "OLD BLOCK BODY") {
		t.Errorf("old block body should have been replaced")
	}
	if !strings.Contains(content, "# Top-level") || !strings.Contains(content, "# Bottom-level") {
		t.Errorf("surrounding content was corrupted")
	}
}

// Exercises edge case 3: orphaned marker → refuse.
func TestInstall_RefusesOnOrphanedMarker(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	seed := "# Top\n<!-- hearsay:consumer-auto-start -->\n(no end marker, user deleted it)\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Install(RoleConsumer, path)
	if err == nil {
		t.Fatalf("expected error on orphaned marker, got nil")
	}
	if !strings.Contains(err.Error(), "orphaned marker") {
		t.Errorf("error should mention orphaned marker, got: %v", err)
	}
}

// Exercises edge case 4: no markers, non-empty file → append.
func TestInstall_AppendsToExistingFile(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	seed := "# My Notes\n\nSome prose.\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r, err := Install(RolePeer, path)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if r.Action != ActionAppended {
		t.Errorf("expected ActionAppended, got %s", r.Action)
	}
	content := readFile(t, path)
	if !strings.Contains(content, "Some prose.") {
		t.Errorf("pre-existing content was lost")
	}
	if !strings.Contains(content, "hearsay:peer-auto-start") {
		t.Errorf("peer block not appended")
	}
}

// Exercises that install is idempotent — running twice yields the same state.
func TestInstall_Idempotent(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	if _, err := Install(RoleConsumer, path); err != nil {
		t.Fatalf("first install: %v", err)
	}
	once := readFile(t, path)
	if _, err := Install(RoleConsumer, path); err != nil {
		t.Fatalf("second install: %v", err)
	}
	twice := readFile(t, path)
	if once != twice {
		t.Errorf("install is not idempotent — content differs between runs")
	}
}

// Consumer and peer blocks must coexist without stepping on each other.
func TestInstall_CoexistingRolesDontCollide(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	if _, err := Install(RoleConsumer, path); err != nil {
		t.Fatalf("install consumer: %v", err)
	}
	if _, err := Install(RolePeer, path); err != nil {
		t.Fatalf("install peer: %v", err)
	}
	content := readFile(t, path)
	if !strings.Contains(content, "hearsay:consumer-auto-start") {
		t.Errorf("consumer block missing after installing peer")
	}
	if !strings.Contains(content, "hearsay:peer-auto-start") {
		t.Errorf("peer block missing")
	}
	// Uninstall one, the other must remain.
	if _, err := Uninstall(RoleConsumer, path); err != nil {
		t.Fatalf("uninstall consumer: %v", err)
	}
	content2 := readFile(t, path)
	if strings.Contains(content2, "hearsay:consumer-auto-start") {
		t.Errorf("consumer block still present after uninstall")
	}
	if !strings.Contains(content2, "hearsay:peer-auto-start") {
		t.Errorf("peer block was wrongly removed")
	}
}

// Trigger MkdirAll failure by pointing the target path at a sub-path
// whose parent is a regular file, not a directory.
func TestInstall_FailsWhenParentIsNotDirectory(t *testing.T) {
	base := t.TempDir()
	// Create a regular file at "base/stub"; then target "base/stub/CLAUDE.md".
	stub := filepath.Join(base, "stub")
	if err := os.WriteFile(stub, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed stub file: %v", err)
	}
	_, err := Install(RoleConsumer, filepath.Join(stub, "CLAUDE.md"))
	if err == nil {
		t.Fatalf("expected MkdirAll failure when parent is a regular file")
	}
}

// Trigger the WriteFile-on-create error path via a read-only parent dir.
func TestInstall_FailsWhenFileNotWritable(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "ro")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(parent, 0o755) // restore so t.TempDir can clean up
	_, err := Install(RoleConsumer, filepath.Join(parent, "CLAUDE.md"))
	if err == nil {
		t.Fatalf("expected write-on-create failure for read-only parent")
	}
}

func TestUninstall_NoopWhenMissing(t *testing.T) {
	path := tempFile(t, "DoesNotExist.md")
	r, err := Uninstall(RoleConsumer, path)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if r.Action != ActionNoop {
		t.Errorf("expected ActionNoop for missing file, got %s", r.Action)
	}
}

func TestUninstall_NoopWhenNoMarkers(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("plain content\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r, err := Uninstall(RoleConsumer, path)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if r.Action != ActionNoop {
		t.Errorf("expected ActionNoop for file without markers, got %s", r.Action)
	}
}

func TestUninstall_RefusesOnOrphanedMarker(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("<!-- hearsay:consumer-auto-end -->\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Uninstall(RoleConsumer, path)
	if err == nil {
		t.Fatalf("expected error on orphaned end marker")
	}
	if !strings.Contains(err.Error(), "orphaned marker") {
		t.Errorf("error should mention orphaned marker, got: %v", err)
	}
}

// Exercises the default-path branch when path == "".
// Uses a scratch HOME so we don't touch the real ~/.claude/CLAUDE.md.
func TestInstall_EmptyPathUsesDefault(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("HOME", scratch)
	r, err := Install(RoleConsumer, "")
	if err != nil {
		t.Fatalf("install with empty path: %v", err)
	}
	expected := filepath.Join(scratch, ".claude", "CLAUDE.md")
	if r.Path != expected {
		t.Errorf("expected path %s, got %s", expected, r.Path)
	}
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("expected default-path file to exist: %v", err)
	}
	// Round-trip uninstall via the same default.
	if _, err := Uninstall(RoleConsumer, ""); err != nil {
		t.Errorf("uninstall default: %v", err)
	}
}

// Exercises the orphaned-end-marker path on Install (we tested
// orphaned-start earlier, but not end).
func TestInstall_RefusesOnOrphanedEndMarker(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("<!-- hearsay:consumer-auto-end -->\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Install(RoleConsumer, path)
	if err == nil {
		t.Fatalf("expected error on orphaned end marker")
	}
}

// Exercises the non-newline-terminated append branch.
func TestInstall_AppendsToFileWithoutTrailingNewline(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("no-trailing-newline"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Install(RoleConsumer, path); err != nil {
		t.Fatalf("install: %v", err)
	}
	content := readFile(t, path)
	if !strings.Contains(content, "no-trailing-newline") {
		t.Errorf("existing content lost")
	}
	if !strings.Contains(content, "hearsay:consumer-auto-start") {
		t.Errorf("block not appended")
	}
}

// Exercises the single-newline-terminated append branch.
func TestInstall_AppendsToFileWithSingleTrailingNewline(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Install(RoleConsumer, path); err != nil {
		t.Fatalf("install: %v", err)
	}
}

func TestUninstall_EmptyPathUsesDefault(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("HOME", scratch)
	// Install then uninstall using empty path both times.
	if _, err := Install(RoleConsumer, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	r, err := Uninstall(RoleConsumer, "")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if r.Action != ActionRemoved {
		t.Errorf("expected ActionRemoved, got %s", r.Action)
	}
}

func TestUninstall_RemovesBlockAndCollapsesBlankLines(t *testing.T) {
	path := tempFile(t, "CLAUDE.md")
	// Install then uninstall — verify surrounding blank-line collapse.
	seed := "# Top\n\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Install(RoleConsumer, path); err != nil {
		t.Fatalf("install: %v", err)
	}
	r, err := Uninstall(RoleConsumer, path)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if r.Action != ActionRemoved {
		t.Errorf("expected ActionRemoved, got %s", r.Action)
	}
	content := readFile(t, path)
	if strings.Contains(content, "hearsay:consumer-auto") {
		t.Errorf("markers still present after uninstall")
	}
	if strings.Contains(content, "\n\n\n") {
		t.Errorf("triple-newline hole left behind: %q", content)
	}
}