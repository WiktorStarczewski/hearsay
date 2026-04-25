package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/WiktorStarczewski/hearsay/internal/claudemd"
	"github.com/WiktorStarczewski/hearsay/internal/config"
)

// -----------------------------------------------------------------------
// Pure-logic helpers: extractFirstPositional, parseInvite, parseRole,
// isAddrInUse, peerNameRe. These have no side effects, so they're the
// easiest to push toward 100%.

func TestExtractFirstPositional(t *testing.T) {
	// extractFirstPositional is naive about flag signatures: any token
	// that doesn't start with `-` is a positional. That's fine for our
	// real subcommands because every flag we define takes its own value
	// via `--flag value` (so the flag token is caught by the HasPrefix
	// check and we never reach the value). Test cases reflect that
	// behavior rather than shell/getopt-style parsing.
	cases := []struct {
		name     string
		in       []string
		wantPos  string
		wantRest []string
	}{
		{"empty", nil, "", nil},
		{"no positionals", []string{"--a", "--b"}, "", []string{"--a", "--b"}},
		{"pos first", []string{"ivan", "--url", "X"}, "ivan", []string{"--url", "X"}},
		{"pos last", []string{"--url", "X", "ivan"}, "X", []string{"--url", "ivan"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPos, gotRest := extractFirstPositional(c.in)
			if gotPos != c.wantPos {
				t.Errorf("pos=%q, want %q", gotPos, c.wantPos)
			}
			if !equalStrs(gotRest, c.wantRest) {
				t.Errorf("rest=%v, want %v", gotRest, c.wantRest)
			}
		})
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPeerNameRegex(t *testing.T) {
	good := []string{"ivan", "ivan-mac", "i", "a123", "ab-cd-ef"}
	bad := []string{"", "Ivan", "123foo", "ivan_mac", "-ivan", "ivan ", strings.Repeat("a", 33)}
	for _, s := range good {
		if !peerNameRe.MatchString(s) {
			t.Errorf("expected %q to be a valid peer name", s)
		}
	}
	for _, s := range bad {
		if peerNameRe.MatchString(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestParseInvite_HappyPath(t *testing.T) {
	name, httpURL, token, err := parseInvite("hearsay://ivan@ivan-mac.tailXXXX.ts.net:3456/mcp?token=abc123")
	if err != nil {
		t.Fatalf("parseInvite: %v", err)
	}
	if name != "ivan" {
		t.Errorf("name = %q", name)
	}
	if httpURL != "http://ivan-mac.tailXXXX.ts.net:3456/mcp" {
		t.Errorf("httpURL = %q", httpURL)
	}
	if token != "abc123" {
		t.Errorf("token = %q", token)
	}
}

func TestParseInvite_DefaultsEmptyPathToMcp(t *testing.T) {
	// No path component in the URI — parser should fill in /mcp.
	_, httpURL, _, err := parseInvite("hearsay://ivan@host:3456?token=abc")
	if err != nil {
		t.Fatalf("parseInvite: %v", err)
	}
	if !strings.HasSuffix(httpURL, "/mcp") {
		t.Errorf("expected default /mcp path, got %q", httpURL)
	}
}

func TestParseInvite_RejectsBadInputs(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{"bad scheme", "http://ivan@host?token=x"},
		{"no userinfo", "hearsay://host:3456/mcp?token=x"},
		{"bad peer name", "hearsay://Ivan@host:3456/mcp?token=x"},
		{"no host", "hearsay://ivan@?token=x"},
		{"no token", "hearsay://ivan@host:3456/mcp"},
		{"unparseable", "::::not-a-uri:::"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, err := parseInvite(c.uri)
			if err == nil {
				t.Errorf("expected error for %q", c.uri)
			}
		})
	}
}

func TestIsAddrInUse(t *testing.T) {
	if isAddrInUse(nil) {
		t.Errorf("nil error should not be AddrInUse")
	}
	if !isAddrInUse(errors.New("listen tcp 127.0.0.1:8080: bind: address already in use")) {
		t.Errorf("should recognize EADDRINUSE by substring")
	}
	if isAddrInUse(errors.New("some other error")) {
		t.Errorf("false positive on unrelated error")
	}
}

func TestParseRole(t *testing.T) {
	if r, err := parseRole("consumer"); err != nil || r != claudemd.RoleConsumer {
		t.Errorf("consumer → (%v, %v)", r, err)
	}
	if r, err := parseRole("peer"); err != nil || r != claudemd.RolePeer {
		t.Errorf("peer → (%v, %v)", r, err)
	}
	if _, err := parseRole("whatever"); err == nil {
		t.Errorf("expected error on invalid role")
	}
}

// -----------------------------------------------------------------------
// Exercise runClaudeMd end-to-end against temp paths (install/print/uninstall).

func TestRunClaudeMd_InstallPrintUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "CLAUDE.md")

	if code := runClaudeMd([]string{"install", "--path", path}); code != 0 {
		t.Errorf("install: exit %d", code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %s: %v", path, err)
	}

	stdout := captureStdout(t, func() {
		if code := runClaudeMd([]string{"print"}); code != 0 {
			t.Errorf("print: exit %d", code)
		}
	})
	if !strings.Contains(stdout, "hearsay:consumer-auto-start") {
		t.Errorf("print output missing marker")
	}

	if code := runClaudeMd([]string{"uninstall", "--path", path}); code != 0 {
		t.Errorf("uninstall: exit %d", code)
	}
}

func TestDispatch_HelpAndVersion(t *testing.T) {
	// Exercise the trivial dispatch branches in one place.
	out := captureStdout(t, func() {
		if code := dispatch([]string{"help"}); code != 0 {
			t.Errorf("help exit=%d", code)
		}
	})
	if !strings.Contains(out, "hearsay — expose Claude Code") {
		t.Errorf("help output unexpected: %q", out)
	}
	out = captureStdout(t, func() {
		if code := dispatch([]string{"--version"}); code != 0 {
			t.Errorf("version exit=%d", code)
		}
	})
	if !strings.Contains(out, version) {
		t.Errorf("version output should include %q, got %q", version, out)
	}
}

func TestDispatch_UnknownSubcommand(t *testing.T) {
	if code := dispatch([]string{"nonexistent"}); code != 2 {
		t.Errorf("unknown subcommand exit=%d, want 2", code)
	}
}

// Positional forms ("help", "version") — dash-prefixed variants go
// through a separate dispatch branch tested elsewhere.
func TestDispatch_PositionalHelpAndVersion(t *testing.T) {
	out := captureStdout(t, func() {
		if code := dispatch([]string{"help"}); code != 0 {
			t.Errorf("help positional exit=%d", code)
		}
	})
	if !strings.Contains(out, "hearsay — expose Claude Code") {
		t.Errorf("help output unexpected: %q", out)
	}
	out = captureStdout(t, func() {
		if code := dispatch([]string{"version"}); code != 0 {
			t.Errorf("version positional exit=%d", code)
		}
	})
	if !strings.Contains(out, version) {
		t.Errorf("version output should include %q, got %q", version, out)
	}
}

// Exercises the "-h" alias.
func TestDispatch_ShortHelpFlag(t *testing.T) {
	out := captureStdout(t, func() {
		if code := dispatch([]string{"-h"}); code != 0 {
			t.Errorf("-h exit=%d", code)
		}
	})
	if !strings.Contains(out, "hearsay — expose") {
		t.Errorf("-h didn't print help: %q", out)
	}
}

// Dispatch delegates to subcommand-specific handlers; confirm each one
// is reachable by checking non-zero exit on deliberately bad args.
func TestDispatch_RoutesToSubcommands(t *testing.T) {
	// No Claude CLI available so add-peer / remove-peer bail with non-zero.
	t.Setenv("PATH", "/nonexistent-"+t.Name())
	for _, sub := range []string{"claude-md", "add-peer", "remove-peer", "invite", "pair"} {
		t.Run(sub, func(t *testing.T) {
			// Passing just the subcommand name exercises the dispatch
			// case and the subcommand's usage-error path.
			code := dispatch([]string{sub})
			if code == 0 {
				t.Errorf("%s alone shouldn't succeed, got 0", sub)
			}
		})
	}
}

func TestRunClaudeMd_BadArgs(t *testing.T) {
	if code := runClaudeMd(nil); code != 2 {
		t.Errorf("expected usage exit=2, got %d", code)
	}
	if code := runClaudeMd([]string{"whatever"}); code != 2 {
		t.Errorf("expected unknown-action exit=2, got %d", code)
	}
	if code := runClaudeMd([]string{"install", "--role", "bogus"}); code != 2 {
		t.Errorf("expected bad-role exit=2, got %d", code)
	}
}

// -----------------------------------------------------------------------
// Exercise add-peer input validation (pre-exec). We stub out the
// external `claude` binary with a script on PATH so the real exec
// returns 0.

func fakeClaudeOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunAddPeer_ValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"bad name", []string{"BadName", "--url", "http://x", "--token", "y"}},
		{"missing url", []string{"ivan", "--token", "y"}},
		{"missing token", []string{"ivan", "--url", "http://x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code := runAddPeer(c.args); code == 0 {
				t.Errorf("expected non-zero exit")
			}
		})
	}
}

func TestRunAddPeer_ShellsOutToClaudeOnPath(t *testing.T) {
	fakeClaudeOnPath(t)
	if code := runAddPeer([]string{"ivan", "--url", "http://127.0.0.1:3456/mcp", "--token", "abc"}); code != 0 {
		t.Errorf("expected success with fake claude on PATH, got %d", code)
	}
}

func TestRunAddPeer_FailsWhenClaudeNotOnPath(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-"+t.Name())
	if code := runAddPeer([]string{"ivan", "--url", "http://x:1/mcp", "--token", "abc"}); code == 0 {
		t.Errorf("expected failure when claude CLI is absent")
	}
}

func TestRunRemovePeer_ShellsOutToClaudeOnPath(t *testing.T) {
	fakeClaudeOnPath(t)
	if code := runRemovePeer([]string{"ivan"}); code != 0 {
		t.Errorf("expected success, got %d", code)
	}
}

func TestRunRemovePeer_BadArgs(t *testing.T) {
	if code := runRemovePeer(nil); code == 0 {
		t.Errorf("expected non-zero exit when no name provided")
	}
	t.Setenv("PATH", "/nonexistent-"+t.Name())
	if code := runRemovePeer([]string{"ivan"}); code == 0 {
		t.Errorf("expected failure when claude CLI is absent")
	}
}

// -----------------------------------------------------------------------
// runInvite and runPair.

func TestRunInvite_RequiresConfig(t *testing.T) {
	// Scratch HOME with no config file — invite should fail with a clean
	// "initialize first" message.
	t.Setenv("HOME", t.TempDir())
	if code := runInvite(nil); code == 0 {
		t.Errorf("expected non-zero exit when no config")
	}
}

func TestRunInvite_PrintsURIWithExplicitHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Seed a config file directly so invite has something to read.
	seedConfig(t, home, "ivan", "token-abc-xyz")

	out := captureStdout(t, func() {
		if code := runInvite([]string{"--host", "10.0.0.1", "--port", "3000"}); code != 0 {
			t.Errorf("invite exit %d", code)
		}
	})
	if !strings.Contains(out, "hearsay://ivan@10.0.0.1:3000/mcp") {
		t.Errorf("invite URI looks wrong: %q", out)
	}
	if !strings.Contains(out, "token=token-abc-xyz") {
		t.Errorf("invite URI missing token: %q", out)
	}
}

func TestRunInvite_FailsWithoutHostAndNoTailscale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedConfig(t, home, "ivan", "token-xyz")
	// No tailscale on PATH, no --host → should error.
	t.Setenv("PATH", "/nonexistent-"+t.Name())
	if code := runInvite(nil); code == 0 {
		t.Errorf("expected non-zero exit when no host available")
	}
}

func TestRunPair_HappyPath(t *testing.T) {
	fakeClaudeOnPath(t)
	uri := "hearsay://ivan@127.0.0.1:3456/mcp?token=abc"
	if code := runPair([]string{uri}); code != 0 {
		t.Errorf("pair exit %d", code)
	}
}

func TestRunPair_BadArgs(t *testing.T) {
	if code := runPair(nil); code == 0 {
		t.Errorf("expected non-zero exit for no args")
	}
	if code := runPair([]string{"not-a-uri"}); code == 0 {
		t.Errorf("expected non-zero exit for bad URI")
	}
}

// -----------------------------------------------------------------------
// Helpers that emulate captureStdout / seed a config.json in scratch HOME.

// captureStdout redirects fmt.Println-style writes (via os.Stdout) into
// a buffer for the duration of fn. Needed because our subcommand helpers
// write directly to Println.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan []byte)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.Bytes()
	}()

	fn()
	w.Close()
	return string(<-done)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan []byte)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.Bytes()
	}()

	fn()
	w.Close()
	return string(<-done)
}

// seedConfig drops a fake config.json at whatever location
// internal/config.Dir() resolves to for the current platform, so the
// tests work on both macOS (Library/Application Support) and Linux
// (XDG / ~/.config). HOME must already be pointed at a scratch dir
// by the caller (which controls Dir()'s output).
func seedConfig(t *testing.T, home, name, token string) {
	t.Helper()
	// Clear XDG so config.Dir() picks the default path, not an
	// inherited ambient override from the outer environment.
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := config.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := []byte(`{"name":"` + name + `","token":"` + token + `","createdAt":"2026-04-24T00:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	_ = home
}

// TestIsAddrInUse_WithRealListenError wires a real EADDRINUSE error
// through the predicate to make sure the substring-match heuristic
// holds against actual Go listen errors.
func TestIsAddrInUse_WithRealListenError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	_, err = net.Listen("tcp", l.Addr().String())
	if err == nil {
		t.Skipf("couldn't reproduce EADDRINUSE on this platform")
	}
	if !isAddrInUse(err) {
		t.Errorf("isAddrInUse missed real listen error: %v", err)
	}
}

// Sanity: assert that exec.ErrNotFound-style errors don't fool the
// predicate into thinking they're EADDRINUSE.
func TestIsAddrInUse_NotFoundIsFalse(t *testing.T) {
	if isAddrInUse(exec.ErrNotFound) {
		t.Errorf("exec.ErrNotFound misclassified as AddrInUse")
	}
	if isAddrInUse(syscall.EACCES) {
		t.Errorf("EACCES misclassified as AddrInUse")
	}
}

// -----------------------------------------------------------------------
// runServer end-to-end. We stand up the full flag-parse → config-resolve
// → listen → shutdown path against a synthetic signal channel. Covers
// the bulk of runServerWithSignals' statements.

func TestRunServer_StartsAndShutsDownCleanly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := t.TempDir()

	// Ephemeral port so parallel test runs don't clash.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	args := []string{
		"--name", "server-e2e",
		"--bind", "127.0.0.1",
		"--port", fmt.Sprint(port),
		"--data-dir", dataDir,
		"--quiet",
	}
	go func() {
		done <- runServerWithSignals(args, sigCh)
	}()

	// Give the server a moment to bind, then poke it to confirm it's alive.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			sigCh <- syscall.SIGTERM
			t.Fatalf("server never became reachable on port %d", port)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	sigCh <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("runServer exit=%d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("runServer did not exit after SIGTERM")
	}
}

// Exercises the EADDRINUSE branch in runServerWithSignals.
func TestRunServer_ReportsPortInUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Occupy a port first so the server can't bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	args := []string{
		"--name", "busy-port",
		"--bind", "127.0.0.1",
		"--port", fmt.Sprint(port),
		"--data-dir", t.TempDir(),
		"--quiet",
	}
	code := runServerWithSignals(args, nil)
	if code != 1 {
		t.Errorf("expected exit=1 on EADDRINUSE, got %d", code)
	}
}

// Bad args → flag parser hits usage error → exit=2.
func TestRunServer_BadFlagArgs(t *testing.T) {
	if code := runServerWithSignals([]string{"--not-a-flag"}, nil); code == 0 {
		t.Errorf("expected non-zero for bad flag")
	}
}

// First run without --name fails fast out of config.Resolve.
func TestRunServer_FirstRunWithoutNameErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Neutralize any inherited XDG_CONFIG_HOME so Dir() resolves strictly
	// under the scratch HOME. Without this, some CI images leave
	// XDG_CONFIG_HOME set to a populated path and Resolve finds a
	// pre-existing config there — the server then boots for real and,
	// with a nil signal channel, the test hangs on `<-nil`.
	t.Setenv("XDG_CONFIG_HOME", "")
	// Belt-and-suspenders: pre-signal the channel so that if the
	// no-name→error path ever regresses, the server shuts down instead
	// of hanging the test for 10 minutes.
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM
	// Pick an ephemeral port so the default :3456 doesn't collide with
	// other tests running in parallel on the same runner.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	args := []string{"--bind", "127.0.0.1", "--port", fmt.Sprint(port), "--data-dir", t.TempDir(), "--quiet"}
	if code := runServerWithSignals(args, sigCh); code != 1 {
		t.Errorf("expected exit=1 when --name is missing on first run, got %d", code)
	}
}

// Covers the short pass-through runServer()+defaultSignalChan() entry
// points. We exercise them with deliberately bad args so they return
// quickly without actually binding a port.
func TestRunServer_EntryPointWithBadArgs(t *testing.T) {
	if code := runServer([]string{"--no-such-flag"}); code == 0 {
		t.Errorf("runServer should reject unknown flags")
	}
	ch := defaultSignalChan()
	if ch == nil {
		t.Errorf("defaultSignalChan returned nil")
	}
}

// Exercises the tailscale-detected bind path in runServerWithSignals
// (non-explicit --bind). We stub `tailscale ip -4` via PATH so the
// function takes the tailscale branch deterministically.
func TestRunServer_UsesTailscaleBindWhenNoExplicitBind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Fake tailscale returns 127.0.0.1 so the server can actually bind
	// (a real 100.x.y.z isn't assigned on this host).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte("#!/bin/sh\necho 127.0.0.1\n"), 0o755); err != nil {
		t.Fatalf("fake tailscale: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		done <- runServerWithSignals([]string{
			"--name", "ts-bind",
			"--port", fmt.Sprint(port),
			"--data-dir", t.TempDir(),
			"--quiet",
		}, sigCh)
	}()

	// Wait briefly for the server to bind.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			break
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	sigCh <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("server didn't exit")
	}
}

// Exercises the bindSource="tailscale not detected" fallback in
// runServerWithSignals: no --bind flag, no tailscale CLI on PATH.
func TestRunServer_FallsBackTo127WhenNoTailscale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedConfig(t, home, "fallback", "tok")

	t.Setenv("PATH", "/nonexistent-"+t.Name())

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		done <- runServerWithSignals([]string{
			"--port", fmt.Sprint(port),
			"--data-dir", t.TempDir(),
			"--quiet",
		}, sigCh)
	}()
	// Give server a beat to bind, then signal shutdown.
	time.Sleep(200 * time.Millisecond)
	sigCh <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("server didn't exit")
	}
}

// Covers the --regenerate-token branch in runServerWithSignals.
func TestRunServer_RegenerateTokenBranch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedConfig(t, home, "rotator", "old-token")

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		done <- runServerWithSignals([]string{
			"--bind", "127.0.0.1",
			"--port", fmt.Sprint(port),
			"--regenerate-token",
			"--data-dir", t.TempDir(),
			"--quiet",
		}, sigCh)
	}()
	time.Sleep(200 * time.Millisecond)
	sigCh <- syscall.SIGTERM
	<-done
}

// TestRunServer_EnableAgentRefusesWithoutAPIKey is the load-bearing
// guard from the plan's error contract: hearsay must refuse to start
// (no half-configured state) when --enable-agent is set but the API
// key env is empty.
func TestRunServer_EnableAgentRefusesWithoutAPIKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	stderrCapture := captureStderr(t, func() {
		// runServerWithSignals exits before binding when init fails;
		// pre-fill sigCh so the (unused) shutdown branch can't deadlock.
		sigCh := make(chan os.Signal, 1)
		sigCh <- syscall.SIGTERM
		code := runServerWithSignals([]string{
			"--name", "ivan",
			"--bind", "127.0.0.1",
			"--port", "0",
			"--quiet",
			"--enable-agent",
		}, sigCh)
		if code == 0 {
			t.Errorf("expected non-zero exit when --enable-agent and key is empty")
		}
	})
	if !strings.Contains(stderrCapture, "ANTHROPIC_API_KEY") {
		t.Errorf("expected error mentioning ANTHROPIC_API_KEY; got: %s", stderrCapture)
	}
}

// TestRunServer_EnableAgentCustomKeyEnv exercises the --agent-api-key-env
// override branch.  We point the flag at a non-default env var name and
// confirm the *missing* var causes the same refusal.
func TestRunServer_EnableAgentCustomKeyEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HEARSAY_TEST_KEY_DOES_NOT_EXIST", "")

	stderrCapture := captureStderr(t, func() {
		sigCh := make(chan os.Signal, 1)
		sigCh <- syscall.SIGTERM
		code := runServerWithSignals([]string{
			"--name", "ivan",
			"--bind", "127.0.0.1",
			"--port", "0",
			"--quiet",
			"--enable-agent",
			"--agent-api-key-env", "HEARSAY_TEST_KEY_DOES_NOT_EXIST",
		}, sigCh)
		if code == 0 {
			t.Errorf("expected non-zero exit")
		}
	})
	if !strings.Contains(stderrCapture, "HEARSAY_TEST_KEY_DOES_NOT_EXIST") {
		t.Errorf("expected error mentioning the override env var; got: %s", stderrCapture)
	}
}

// TestMaskKey covers the rendering used in startup-log key masking.
func TestMaskKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "***"},
		{"sk-ant", "***"},
		{"sk-ant-x12345abcd", "sk-ant-....abcd"},
	}
	for _, c := range cases {
		if got := maskKey(c.in); got != c.want {
			t.Errorf("maskKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// runInvite with a fake tailscale on PATH exercises the tailscale DNS
// auto-detect branch.
func TestRunInvite_AutoDetectsTailscaleHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedConfig(t, home, "ivan", "tok-123")

	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  ip) echo 100.64.1.2 ;;
  status) cat <<EOF
{"Self":{"DNSName":"ivan-mac.tail9876.ts.net.","HostName":"ivan-mac"}}
EOF
;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatalf("fake tailscale: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out := captureStdout(t, func() {
		if code := runInvite(nil); code != 0 {
			t.Errorf("invite exit %d", code)
		}
	})
	if !strings.Contains(out, "ivan-mac.tail9876.ts.net") {
		t.Errorf("auto-detect URI missing tailscale host: %q", out)
	}
}