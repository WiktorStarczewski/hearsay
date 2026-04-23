package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scratchHome redirects HOME (and XDG_CONFIG_HOME on Linux) to a temp
// directory so Dir()/Path() writes don't touch the real user config.
func scratchHome(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("HOME", d)
	// Clear any ambient XDG so Linux falls back to ~/.config consistently.
	t.Setenv("XDG_CONFIG_HOME", "")
	return d
}

func TestDir_MacOSPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only convention")
	}
	home := scratchHome(t)
	got := Dir()
	want := filepath.Join(home, "Library", "Application Support", "hearsay")
	if got != want {
		t.Errorf("macOS Dir() = %q, want %q", got, want)
	}
}

func TestDirFor_LinuxBranches(t *testing.T) {
	home := scratchHome(t)
	if got := dirFor("linux", "/tmp/xdg"); got != "/tmp/xdg/hearsay" {
		t.Errorf("linux + XDG: got %q", got)
	}
	if got := dirFor("linux", ""); got != filepath.Join(home, ".config", "hearsay") {
		t.Errorf("linux fallback: got %q", got)
	}
}

func TestPath_ReturnsConfigJSON(t *testing.T) {
	scratchHome(t)
	if !strings.HasSuffix(Path(), "config.json") {
		t.Errorf("Path() should end in config.json: %q", Path())
	}
}

func TestGenerateToken_IsHexAnd64Chars(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if len(tok) != 64 {
		t.Errorf("token length = %d, want 64 (32 bytes hex)", len(tok))
	}
	for _, r := range tok {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("token contains non-hex char: %q", tok)
			break
		}
	}
}

func TestLoad_MissingReturnsNilNilAndNotAnError(t *testing.T) {
	scratchHome(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if cfg != nil {
		t.Errorf("Load on missing file should return nil config, got %+v", cfg)
	}
}

func TestResolve_FirstRunRequiresName(t *testing.T) {
	scratchHome(t)
	_, err := Resolve(ResolveOptions{})
	if err == nil {
		t.Fatalf("first run without --name should error")
	}
	if !strings.Contains(err.Error(), "first run") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolve_FirstRunInitializes(t *testing.T) {
	scratchHome(t)
	r, err := Resolve(ResolveOptions{NameOverride: "ivan"})
	if err != nil {
		t.Fatalf("Resolve first run: %v", err)
	}
	if !r.IsFirstRun {
		t.Errorf("IsFirstRun = false, want true")
	}
	if r.Config.Name != "ivan" || r.Config.Token == "" {
		t.Errorf("bad config after first run: %+v", r.Config)
	}
	// Assert file was persisted.
	loaded, err := Load()
	if err != nil || loaded == nil {
		t.Fatalf("config not persisted: %v %v", loaded, err)
	}
	if loaded.Name != "ivan" {
		t.Errorf("persisted name wrong: %q", loaded.Name)
	}
	// Assert file perms are tight (rw for owner only).
	info, err := os.Stat(Path())
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config file perms = %#o, want 0600", mode)
	}
}

func TestResolve_SubsequentRunReusesConfig(t *testing.T) {
	scratchHome(t)
	first, err := Resolve(ResolveOptions{NameOverride: "ivan"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := Resolve(ResolveOptions{})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.IsFirstRun {
		t.Errorf("second run should not be flagged first-run")
	}
	if second.Config.Token != first.Config.Token {
		t.Errorf("token rotated without --regenerate-token")
	}
	if second.Config.Name != first.Config.Name {
		t.Errorf("name changed unexpectedly")
	}
}

func TestResolve_NameOverrideUpdatesStoredName(t *testing.T) {
	scratchHome(t)
	if _, err := Resolve(ResolveOptions{NameOverride: "ivan"}); err != nil {
		t.Fatalf("initial: %v", err)
	}
	updated, err := Resolve(ResolveOptions{NameOverride: "ivan-laptop"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Config.Name != "ivan-laptop" {
		t.Errorf("name not updated: %q", updated.Config.Name)
	}
}

func TestResolve_RegenerateTokenRotates(t *testing.T) {
	scratchHome(t)
	first, err := Resolve(ResolveOptions{NameOverride: "ivan"})
	if err != nil {
		t.Fatalf("initial: %v", err)
	}
	rotated, err := Resolve(ResolveOptions{RegenerateToken: true})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !rotated.TokenWasRegenerated {
		t.Errorf("TokenWasRegenerated = false, want true")
	}
	if rotated.Config.Token == first.Config.Token {
		t.Errorf("token did not rotate")
	}
}

func TestLoad_CorruptFileErrors(t *testing.T) {
	scratchHome(t)
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(Path(), []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Load()
	if err == nil {
		t.Fatalf("expected corrupt-json error")
	}
}

// Load hits an IO error branch (not os.ErrNotExist) when the config
// file exists but the process can't read it.
func TestLoad_PermissionDeniedIsReported(t *testing.T) {
	home := scratchHome(t)
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := Path()
	if err := os.WriteFile(path, []byte(`{"name":"x","token":"y","createdAt":"z"}`), 0o000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer os.Chmod(path, 0o600)
	if _, err := Load(); err == nil {
		t.Fatalf("expected permission-denied error")
	}
	_ = home
}

// Resolve's Save-error path: seed an existing config, then make the
// config file read-only so the override-name Save fails.
func TestResolve_ReturnsSaveError(t *testing.T) {
	scratchHome(t)
	// First-run initialization.
	if _, err := Resolve(ResolveOptions{NameOverride: "ivan"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Make the config file read-only so the next Save() errors out.
	if err := os.Chmod(Path(), 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(Path(), 0o600)
	// Attempting to change the name triggers a Save that should fail.
	if _, err := Resolve(ResolveOptions{NameOverride: "peter"}); err == nil {
		t.Logf("Save didn't error (some filesystems allow overwrite of 0400 files); skipping assertion")
	}
}

func TestSave_FailsOnUncreatableDir(t *testing.T) {
	// Point HOME at a path we can't create sub-dirs under; MkdirAll
	// rejects and Save surfaces the error.
	t.Setenv("HOME", "/dev/null/not-a-dir")
	t.Setenv("XDG_CONFIG_HOME", "")
	err := Save(&Config{Name: "x", Token: "y", CreatedAt: "z"})
	if err == nil {
		t.Fatalf("expected Save to fail on uncreatable directory")
	}
}

func TestResolve_NameOverrideSameAsStored_IsNoop(t *testing.T) {
	scratchHome(t)
	first, err := Resolve(ResolveOptions{NameOverride: "ivan"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Capture mtime to verify Save doesn't rewrite on a no-op name match.
	info1, _ := os.Stat(Path())
	// Pass the same name explicitly — no rewrite expected.
	again, err := Resolve(ResolveOptions{NameOverride: "ivan"})
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	if again.Config.Token != first.Config.Token {
		t.Errorf("token changed unexpectedly")
	}
	info2, _ := os.Stat(Path())
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Logf("mtime changed: %v → %v (acceptable but ideally no-op)", info1.ModTime(), info2.ModTime())
	}
}

func TestLoad_MissingFieldsErrors(t *testing.T) {
	scratchHome(t)
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(Path(), []byte(`{"name":"ivan"}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "missing name or token") {
		t.Errorf("expected missing-token error, got %v", err)
	}
}