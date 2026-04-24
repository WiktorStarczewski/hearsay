// Package e2e holds end-to-end tests that exercise a compiled hearsay
// binary over real HTTP. Unit tests in the internal packages cover the
// pieces in isolation; this file proves they assemble correctly at the
// process boundary:
//
//   - CLI flag parsing in a real os.Args environment
//   - Config file creation on disk (~/Library/Application Support/hearsay)
//   - HTTP listener on an ephemeral TCP port
//   - MCP streamable-HTTP handshake end-to-end, over the wire
//   - Bearer-token middleware on real incoming requests
//   - Invite URI generation matching the stored config
//   - Graceful shutdown on SIGTERM
//   - claude-md install/uninstall round-trip
//   - hearsay pair → stubbed `claude mcp add` invocation
//
// A single TestMain builds the binary once into a temp dir; each Test*
// function spins up (or reuses) a fresh server process with an
// isolated HOME and ~/.claude/projects/ tree.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// binaryPath is set by TestMain — a freshly-compiled hearsay binary all
// tests in this file share.
var binaryPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "hearsay-e2e-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create tempdir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	binaryPath = filepath.Join(tmp, "hearsay")
	if runtime.GOOS == "windows" {
		binaryPath += ".exe"
	}
	// Compile with the module rooted one directory up from this file
	// (the e2e/ package sits alongside cmd/ and internal/).
	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/hearsay")
	cmd.Dir = ".."
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "go build failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// -----------------------------------------------------------------------
// Test harness: start a hearsay server in a scratch HOME with a tiny
// fake ~/.claude/projects/ tree so list_sessions has something to return.

type fixture struct {
	t        *testing.T
	home     string
	dataDir  string
	port     int
	baseURL  string
	token    string
	stderr   *strings.Builder
	cmd      *exec.Cmd
	shutdown func()
}

// startServer launches hearsay as a child process on an ephemeral port,
// waits for /health to respond, reads back the generated token from its
// on-disk config, and returns a fixture tests can drive.
func startServer(t *testing.T, name string) *fixture {
	t.Helper()

	home := t.TempDir()
	dataDir := t.TempDir()
	seedFakeSession(t, dataDir)

	// Pick a port by opening a short-lived listener; we close it
	// before hearsay binds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	stderrBuf := &strings.Builder{}
	cmd := exec.Command(binaryPath,
		"--name", name,
		"--port", fmt.Sprint(port),
		"--bind", "127.0.0.1",
		"--data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME=", // force config.Dir() back to its default
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = &teeWriter{dst: stderrBuf}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start hearsay: %v", err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, baseURL, 3*time.Second)

	shutdown := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		_ = cmd.Wait()
	}
	t.Cleanup(shutdown)

	return &fixture{
		t:        t,
		home:     home,
		dataDir:  dataDir,
		port:     port,
		baseURL:  baseURL,
		token:    readToken(t, home),
		stderr:   stderrBuf,
		cmd:      cmd,
		shutdown: shutdown,
	}
}

// teeWriter duplicates child-process stderr into a test-scoped buffer
// so t.Logf and assertions can inspect it.
type teeWriter struct{ dst *strings.Builder }

func (w *teeWriter) Write(p []byte) (int, error) {
	w.dst.Write(p)
	return len(p), nil
}

// seedFakeSession writes a minimal JSONL into the scratch data dir so
// list_sessions has one session to find. File mtime is "now", so the
// session is flagged isLive.
func seedFakeSession(t *testing.T, dataDir string) {
	t.Helper()
	projects := filepath.Join(dataDir, "projects", "-tmp-e2e")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	session := `{"type":"user","uuid":"u1","timestamp":"2026-04-24T10:00:00Z","sessionId":"e2eSSS","message":{"role":"user","content":"e2e test session first prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-04-24T10:00:01Z","sessionId":"e2eSSS","message":{"role":"assistant","content":[{"type":"text","text":"assistant reply"}]}}
`
	if err := os.WriteFile(filepath.Join(projects, "e2eSSS.jsonl"), []byte(session), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
}

// waitForHealth polls /health until it returns 200 or the deadline
// passes. If the server never came up, fail the test with whatever
// stderr captured.
func waitForHealth(t *testing.T, baseURL string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		resp, err := http.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("hearsay never became healthy at %s within %s", baseURL, budget)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// readToken parses the on-disk config.json under the scratch HOME to
// retrieve the bearer token the server generated at first run.
func readToken(t *testing.T, home string) string {
	t.Helper()
	// Both platforms for the config-dir scan.
	candidates := []string{
		filepath.Join(home, "Library", "Application Support", "hearsay", "config.json"),
		filepath.Join(home, ".config", "hearsay", "config.json"),
	}
	for _, c := range candidates {
		raw, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		var cfg struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("parse config.json at %s: %v", c, err)
		}
		if cfg.Token == "" {
			t.Fatalf("config.json at %s has empty token", c)
		}
		return cfg.Token
	}
	t.Fatalf("no config.json found under %s", home)
	return ""
}

// bearerTransport stamps the hearsay Bearer token onto every outgoing
// request — the consumer analogue of what Claude Code does on Wiktor's
// side.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(req)
}

// connectMCP returns a logged-in MCP client session talking to the
// fixture's server over real HTTP (not in-memory transport).
func (f *fixture) connectMCP(t *testing.T) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint: f.baseURL + "/mcp",
		HTTPClient: &http.Client{
			Transport: &bearerTransport{token: f.token, next: http.DefaultTransport},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// -----------------------------------------------------------------------
// The actual tests.

// Exercises the full Happy Path: start server, MCP handshake,
// get_peer_info, list_sessions, read_session. Everything flows over
// real HTTP.
func TestE2E_HealthAndMCP(t *testing.T) {
	f := startServer(t, "e2e-peer")

	// /health first — confirms the server is up and returns its identity.
	resp, err := http.Get(f.baseURL + "/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if h["name"] != "e2e-peer" {
		t.Errorf("health name = %v, want e2e-peer", h["name"])
	}

	// MCP handshake + tool calls.
	cs := f.connectMCP(t)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_peer_info", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("get_peer_info: %v", err)
	}
	info := structured(t, res)
	if info["name"] != "e2e-peer" {
		t.Errorf("peer_info.name = %v", info["name"])
	}
	if info["activeSessionCount"].(float64) < 1 {
		t.Errorf("expected >=1 live session, got %v", info["activeSessionCount"])
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("list_sessions: %v", err)
	}
	lss := structured(t, res)
	sessions, ok := lss["sessions"].([]any)
	if !ok || len(sessions) != 1 {
		t.Fatalf("list_sessions returned %v", lss)
	}
	first := sessions[0].(map[string]any)
	if first["sessionId"] != "e2eSSS" {
		t.Errorf("sessionId = %v", first["sessionId"])
	}

	// read_session returns markdown content + JSON metadata.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_session",
		Arguments: map[string]any{"sessionId": "e2eSSS"},
	})
	if err != nil {
		t.Fatalf("read_session: %v", err)
	}
	if res.IsError {
		t.Fatalf("read_session flagged error; content: %v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatalf("expected markdown content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content block is not TextContent: %T", res.Content[0])
	}
	if !strings.Contains(tc.Text, "e2e test session first prompt") {
		t.Errorf("markdown missing user prompt: %s", tc.Text)
	}
}

// 401-without-token and 401-with-wrong-token paths on the real HTTP
// listener.
func TestE2E_AuthRejection(t *testing.T) {
	f := startServer(t, "auth-e2e")

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"no auth", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"wrong token", "Bearer wrong-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", f.baseURL+"/mcp", strings.NewReader("{}"))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// `hearsay invite` prints a URI that embeds the stored name + token and
// the passed host/port.
func TestE2E_InviteURI(t *testing.T) {
	f := startServer(t, "invite-e2e")

	out, err := runCLI(t, f.home, binaryPath, "invite", "--host", "127.0.0.1", "--port", fmt.Sprint(f.port))
	if err != nil {
		t.Fatalf("hearsay invite: %v\nstderr: %s", err, out.stderr)
	}
	uri := strings.TrimSpace(out.stdout)
	prefix := fmt.Sprintf("hearsay://invite-e2e@127.0.0.1:%d/mcp?token=", f.port)
	if !strings.HasPrefix(uri, prefix) {
		t.Errorf("invite URI unexpected: %q", uri)
	}
	if !strings.Contains(uri, f.token) {
		t.Errorf("invite URI missing bearer token")
	}
}

// Full claude-md install → uninstall round-trip driven through the
// compiled binary.
func TestE2E_ClaudeMdRoundtrip(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "CLAUDE.md")

	if _, err := runCLI(t, home, binaryPath, "claude-md", "install", "--path", target); err != nil {
		t.Fatalf("install: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(body), "hearsay:consumer-auto-start") {
		t.Errorf("block not written: %s", body)
	}

	if _, err := runCLI(t, home, binaryPath, "claude-md", "uninstall", "--path", target); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	body, _ = os.ReadFile(target)
	if strings.Contains(string(body), "hearsay:consumer-auto-start") {
		t.Errorf("markers still present after uninstall: %s", body)
	}
}

// `hearsay pair` shells out to `claude mcp add`. We stub `claude` with
// a script that records its argv so we can confirm the invocation was
// shaped correctly. Sidesteps needing real Claude Code on the CI runner.
func TestE2E_PairWithStubbedClaude(t *testing.T) {
	f := startServer(t, "pair-e2e")

	stubDir := t.TempDir()
	argsCapturePath := filepath.Join(stubDir, "args.txt")
	stubScript := "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + argsCapturePath + "\nexit 0\n"
	stubPath := filepath.Join(stubDir, "claude")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write claude stub: %v", err)
	}

	invite := fmt.Sprintf("hearsay://pair-e2e@127.0.0.1:%d/mcp?token=%s", f.port, f.token)
	out, err := runCLIWithExtraPath(t, f.home, stubDir, binaryPath, "pair", invite)
	if err != nil {
		t.Fatalf("hearsay pair: %v\nstdout: %s\nstderr: %s", err, out.stdout, out.stderr)
	}

	raw, err := os.ReadFile(argsCapturePath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	// Split null-separated argv tokens the stub wrote.
	argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")

	// Expect: ["mcp","add","--scope","user","--transport","http","pair-e2e","<url>","-H","Authorization: Bearer <token>"]
	wantPrefix := []string{"mcp", "add", "--scope", "user", "--transport", "http", "pair-e2e"}
	for i, w := range wantPrefix {
		if i >= len(argv) || argv[i] != w {
			t.Fatalf("argv[%d] = %q, want %q (full argv: %v)", i, safeIdx(argv, i), w, argv)
		}
	}
	if !strings.HasPrefix(argv[7], fmt.Sprintf("http://127.0.0.1:%d", f.port)) {
		t.Errorf("url arg unexpected: %q", argv[7])
	}
	if !strings.Contains(argv[9], "Bearer "+f.token) {
		t.Errorf("authorization header arg missing token: %q", argv[9])
	}
}

// -----------------------------------------------------------------------
// Small helpers shared across tests.

type cliOutput struct {
	stdout string
	stderr string
}

func runCLI(t *testing.T, home, bin string, args ...string) (cliOutput, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME=")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return cliOutput{stdout: stdout.String(), stderr: stderr.String()}, err
}

func runCLIWithExtraPath(t *testing.T, home, extraPathDir, bin string, args ...string) (cliOutput, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	// Prepend the stub dir to PATH so `exec.LookPath("claude")` finds
	// our fake binary first.
	envPath := extraPathDir + string(os.PathListSeparator) + os.Getenv("PATH")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME=", "PATH="+envPath)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return cliOutput{stdout: stdout.String(), stderr: stderr.String()}, err
}

func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatalf("no structured content in result")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return m
}

func safeIdx(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return "<out of range>"
	}
	return s[i]
}
