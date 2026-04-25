// Command hearsay is an MCP server that exposes Claude Code session
// transcripts from ~/.claude/projects/ over HTTP, so a teammate's Claude
// can read them directly. See the plan at
// /Users/celrisen/.claude/plans/lets-prototype-the-transcript-reader-toasty-cake.md
// for the full design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/WiktorStarczewski/hearsay/internal/agent"
	"github.com/WiktorStarczewski/hearsay/internal/claudemd"
	"github.com/WiktorStarczewski/hearsay/internal/config"
	"github.com/WiktorStarczewski/hearsay/internal/server"
	"github.com/WiktorStarczewski/hearsay/internal/tailscale"
)

// version is overridable at build time via:
//   go build -ldflags "-X main.version=<tag>" ./cmd/hearsay
var version = "0.1.0-dev"

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

// dispatch is the testable core of main(): it takes the args slice and
// returns an exit code instead of calling os.Exit directly, so unit
// tests can drive it without tearing down the test process.
func dispatch(args []string) int {
	// Top-level convenience flags (`hearsay --version`, `hearsay -h`)
	// are handled before the subcommand switch so they don't end up
	// getting interpreted as server flags.
	if len(args) > 0 {
		switch args[0] {
		case "--version":
			fmt.Println(version)
			return 0
		case "--help", "-h":
			printHelp()
			return 0
		}
	}

	// Subcommand names are checked ahead of `flag` parsing so they
	// don't get confused with flag tokens.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "claude-md":
			return runClaudeMd(args[1:])
		case "add-peer":
			return runAddPeer(args[1:])
		case "remove-peer":
			return runRemovePeer(args[1:])
		case "invite":
			return runInvite(args[1:])
		case "pair":
			return runPair(args[1:])
		case "version":
			fmt.Println(version)
			return 0
		case "help":
			printHelp()
			return 0
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", args[0])
			printHelp()
			return 2
		}
	}
	return runServer(args)
}

func printHelp() {
	fmt.Println(`hearsay — expose Claude Code session transcripts over MCP

USAGE
  hearsay [flags]                     run the MCP server (default — teammate side)
  hearsay invite                      print a hearsay:// invite URI (teammate side)
  hearsay pair <uri>                  register a peer from a hearsay:// URI (consumer)
  hearsay add-peer <name> ...         register a teammate's hearsay server (consumer, explicit)
  hearsay remove-peer <name>          un-register a teammate's hearsay server
  hearsay claude-md <action> ...      manage the ~/.claude/CLAUDE.md block
  hearsay version                     print version and exit

SERVER FLAGS
  --name <name>             peer identity; required on first run, persisted thereafter
  --port <n>                listen port (default 3456)
  --bind <addr>             bind address (default: tailscale IPv4, falls back to 127.0.0.1)
  --data-dir <path>         Claude Code data dir (default ~/.claude)
  --live-window-seconds <n> isLive threshold (default 300)
  --regenerate-token        rotate the stored bearer token and print it
  --quiet                   suppress tool-call logs

PHASE-2 AGENT FLAGS (off by default)
  --enable-agent            register interactive tools (ask_peer_claude, …)
  --agent-api-key-env <NM>  env var the Anthropic API key is read from (default ANTHROPIC_API_KEY)
  --agent-base-url <url>    override Anthropic API base URL (test stubs / regional endpoints)
  --agent-model <id>        override the Anthropic model id (default claude-opus-4-7)
  --agent-default-max-tokens <n>        per-turn token budget (default 32768)
  --agent-default-max-tool-calls <n>    per-turn tool-call budget (default 20)
  --agent-default-timeout-seconds <n>   per-call wall-clock budget in seconds (default 120)
  --agent-log-path <path>   audit log (default: ~/Library/Logs/hearsay/agent.log on macOS,
                            $XDG_STATE_HOME/hearsay/agent.log elsewhere)
  --max-conversations <n>   concurrent-conversations cap (default 10)
  --conversation-idle-timeout <dur>     reap conversations idle past this (default 15m)

ADD-PEER FLAGS
  --url <url>               peer's hearsay MCP URL (e.g. http://ivan-mac.tailXXXX.ts.net:3456/mcp)
  --token <token>           bearer token the peer issued
  --scope <scope>           claude mcp scope: user, project, or local (default user)

CLAUDE.MD SUBCOMMANDS
  hearsay claude-md install [--role consumer|peer] [--path <file>]
  hearsay claude-md print   [--role consumer|peer]
  hearsay claude-md uninstall [--role consumer|peer] [--path <file>]`)
}

// ---------------- run server ----------------

// defaultSignalChan wires up a channel that fires on SIGINT/SIGTERM.
// Factored out so tests can pass an explicit channel to runServerWithSignals.
func defaultSignalChan() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	return ch
}

func runServer(args []string) int {
	return runServerWithSignals(args, defaultSignalChan())
}

func runServerWithSignals(args []string, sigCh <-chan os.Signal) int {
	fs := flag.NewFlagSet("hearsay", flag.ContinueOnError)
	var (
		name            = fs.String("name", "", "peer identity (required on first run)")
		port            = fs.Int("port", 3456, "listen port")
		bind            = fs.String("bind", "", "bind address (default: tailscale IPv4, else 127.0.0.1)")
		dataDir         = fs.String("data-dir", "", "Claude Code data dir (default ~/.claude)")
		liveWindowSecs  = fs.Int("live-window-seconds", 300, "isLive threshold")
		regenerateToken = fs.Bool("regenerate-token", false, "rotate the stored bearer token")
		quiet           = fs.Bool("quiet", false, "suppress tool-call logs")

		// --- Phase-2 agent flags. All gated by --enable-agent; off by
		// default so a Phase-1 install upgrading to a v0.2 binary
		// behaves bit-for-bit like before until the operator opts in.
		enableAgent     = fs.Bool("enable-agent", false, "enable Phase-2 interactive agent tools (ask_peer_claude, etc.)")
		agentAPIKeyEnv  = fs.String("agent-api-key-env", "ANTHROPIC_API_KEY", "env var the Anthropic API key is read from")
		agentBaseURL    = fs.String("agent-base-url", "", "override Anthropic API base URL (test stubs / regional endpoints)")
		agentModel      = fs.String("agent-model", "", "override the Anthropic model id (default claude-opus-4-7)")
		agentMaxTokens  = fs.Int("agent-default-max-tokens", 32768, "default per-turn max_tokens budget")
		agentMaxTools   = fs.Int("agent-default-max-tool-calls", 20, "default per-turn max_tool_calls budget")
		agentTimeoutSec = fs.Int("agent-default-timeout-seconds", 120, "default per-call wall-clock budget in seconds")
		agentLogPath    = fs.String("agent-log-path", "", "audit-log path (default: platform-specific — see DefaultAuditPath)")
		maxConvs        = fs.Int("max-conversations", 10, "concurrent-conversations cap")
		convIdle        = fs.Duration("conversation-idle-timeout", 15*time.Minute, "reap conversations idle past this duration")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Resolve config (possibly first-run or token rotation).
	resolved, err := config.Resolve(config.ResolveOptions{
		NameOverride:    *name,
		RegenerateToken: *regenerateToken,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearsay: %v\n", err)
		return 1
	}

	// If --enable-agent was set, we MUST have an API key before we bind
	// the listener; refusing to start avoids a half-configured server
	// where ask_peer_claude is registered but every call fails.
	var ag agent.Agent
	if *enableAgent {
		envName := *agentAPIKeyEnv
		key := os.Getenv(envName)
		if key == "" {
			fmt.Fprintf(os.Stderr,
				"hearsay: --enable-agent set but %s is empty (override the env var name with --agent-api-key-env)\n",
				envName)
			return 1
		}
		auditPath := *agentLogPath
		if auditPath == "" {
			auditPath = agent.DefaultAuditPath()
		}
		auditor, err := agent.NewAuditor(auditPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearsay: agent audit log: %v\n", err)
			return 1
		}
		ag, err = agent.New(agent.Config{
			APIKey:   key,
			BaseURL:  *agentBaseURL,
			Model:    *agentModel,
			PeerName: resolved.Config.Name,
			DefaultBudget: agent.Budget{
				MaxTokens:    *agentMaxTokens,
				MaxToolCalls: *agentMaxTools,
				Timeout:      time.Duration(*agentTimeoutSec) * time.Second,
			},
			Auditor:                 auditor,
			MaxConversations:        *maxConvs,
			ConversationIdleTimeout: *convIdle,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearsay: agent init: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "agent enabled (key: %s)  audit log: %s\n", maskKey(key), auditPath)
	}

	effectiveBind := *bind
	bindSource := "explicit --bind"
	if effectiveBind == "" {
		if ip := tailscale.DetectIPv4(); ip != "" {
			effectiveBind = ip
			bindSource = "tailscale"
		} else {
			effectiveBind = "127.0.0.1"
			bindSource = "tailscale not detected — pass --bind to expose"
		}
	}

	srv, err := server.Start(server.Options{
		Port:        *port,
		Bind:        effectiveBind,
		Token:       resolved.Config.Token,
		PeerName:    resolved.Config.Name,
		PeerVersion: version,
		DataDir:     *dataDir,
		LiveWindow:  time.Duration(*liveWindowSecs) * time.Second,
		Quiet:       *quiet,
		Agent:       ag,
	})
	if err != nil {
		if isAddrInUse(err) {
			fmt.Fprintf(os.Stderr, "port %d already in use — pass --port to pick another\n", *port)
		} else {
			fmt.Fprintf(os.Stderr, "hearsay: listen error: %v\n", err)
		}
		return 1
	}

	fmt.Fprintf(os.Stderr, "hearsay listening on %s:%d (%s)\n", effectiveBind, *port, bindSource)
	fmt.Fprintf(os.Stderr, "peer name: %s  version: %s\n", resolved.Config.Name, version)
	if resolved.IsFirstRun {
		fmt.Fprintf(os.Stderr, "first-run token (save this — Wiktor will need it):\n  %s\n", resolved.Config.Token)
	} else if resolved.TokenWasRegenerated {
		fmt.Fprintf(os.Stderr, "NEW token (distribute and update CLAUDE_QA_TOKEN-style envs):\n  %s\n", resolved.Config.Token)
	}

	// Graceful shutdown on signal (usually SIGINT / SIGTERM; tests may
	// deliver a synthetic close).
	<-sigCh
	fmt.Fprintln(os.Stderr, "hearsay: shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if closer, ok := ag.(agent.Closer); ok && closer != nil {
		closer.Close()
	}
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "hearsay: shutdown error: %v\n", err)
		return 1
	}
	return 0
}

// maskKey returns a privacy-safe rendering of an Anthropic API key for
// startup logs.  Shows the first few chars + last four of the key so
// the operator can identify which key is loaded without leaking the
// secret in case the log file is shared.
func maskKey(k string) string {
	if len(k) <= 12 {
		return "***"
	}
	return k[:7] + "...." + k[len(k)-4:]
}

// isAddrInUse treats EADDRINUSE specially so the operator gets a
// friendlier nudge than a raw stack trace.
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	// Inspect error string — net.OpError wraps the underlying syscall
	// error in a way that's easier to substring-match than to errors.As.
	return strings.Contains(err.Error(), "address already in use")
}

// ---------------- claude-md subcommand ----------------

func runClaudeMd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hearsay claude-md {install|print|uninstall} [--role consumer|peer] [--path <file>]")
		return 2
	}
	action := args[0]
	fs := flag.NewFlagSet("hearsay claude-md", flag.ContinueOnError)
	var (
		role = fs.String("role", "consumer", "block role: consumer or peer")
		path = fs.String("path", "", "target CLAUDE.md path (default ~/.claude/CLAUDE.md)")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	r, err := parseRole(*role)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearsay claude-md: %v\n", err)
		return 2
	}

	switch action {
	case "install":
		result, err := claudemd.Install(r, *path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearsay claude-md: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "claude-md: %s block → %s (%s)\n", *role, result.Path, result.Action)
		return 0
	case "print":
		fmt.Print(claudemd.Print(r))
		return 0
	case "uninstall":
		result, err := claudemd.Uninstall(r, *path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearsay claude-md: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "claude-md: %s block → %s (%s)\n", *role, result.Path, result.Action)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown claude-md action %q\n", action)
		return 2
	}
}

func parseRole(s string) (claudemd.Role, error) {
	switch s {
	case "consumer":
		return claudemd.RoleConsumer, nil
	case "peer":
		return claudemd.RolePeer, nil
	default:
		return "", errors.New("--role must be 'consumer' or 'peer'")
	}
}

// extractFirstPositional returns the first non-flag argument and the
// remaining slice (with that arg removed) so callers can run a flag
// parser on the rest. Necessary because Go's stdlib flag package stops
// parsing at the first positional, but we want users to be able to
// write `hearsay add-peer ivan --url X --token Y` with the name first.
func extractFirstPositional(args []string) (string, []string) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a, append(append([]string{}, args[:i]...), args[i+1:]...)
	}
	return "", args
}

// ---------------- add-peer / remove-peer (consumer side) ----------------

// peerNameRe constrains peer names to what Claude Code accepts as an
// mcpServers label and what reads cleanly as "teammate's first name"
// (lowercase, alphanumeric + dash, 1–32 chars).
var peerNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// runAddPeer registers a teammate's hearsay server with Claude Code's
// MCP config by shelling out to `claude mcp add`. This turns the
// "install the hearsay mcp server for Ivan at X with token Y" prompt
// into a single CLI call (see consumer CLAUDE.md block for the mapping).
func runAddPeer(args []string) int {
	// Pull the first positional (the name) out of args before flag.Parse
	// so `hearsay add-peer ivan --url X --token Y` works regardless of
	// arg order — Go's stdlib flag package otherwise stops at the first
	// non-flag token.
	name, rest := extractFirstPositional(args)

	fs := flag.NewFlagSet("hearsay add-peer", flag.ContinueOnError)
	var (
		peerURL = fs.String("url", "", "peer's hearsay MCP URL (http://host:port/mcp)")
		token   = fs.String("token", "", "bearer token the peer issued")
		scope   = fs.String("scope", "user", "claude mcp scope (user|project|local)")
	)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if name == "" {
		name = fs.Arg(0)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: hearsay add-peer <name> --url <url> --token <token> [--scope user]")
		return 2
	}
	if !peerNameRe.MatchString(name) {
		fmt.Fprintf(os.Stderr, "hearsay add-peer: name %q is invalid — must be lowercase alphanumeric + dash, starting with a letter (e.g. 'ivan')\n", name)
		return 2
	}
	if *peerURL == "" {
		fmt.Fprintln(os.Stderr, "hearsay add-peer: --url is required")
		return 2
	}
	if _, err := url.Parse(*peerURL); err != nil {
		fmt.Fprintf(os.Stderr, "hearsay add-peer: invalid --url: %v\n", err)
		return 2
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "hearsay add-peer: --token is required")
		return 2
	}

	return addPeerExec(name, *peerURL, *token, *scope)
}

// ---------------- invite / pair (one-URI handshake) ----------------
//
// The invite flow lets Ivan paste one line to Wiktor rather than three
// fields. Ivan runs `hearsay invite` which synthesizes a URI like:
//
//   hearsay://ivan@ivan-mac.tail1234.ts.net:3456/mcp?token=<hex>
//
// Wiktor runs `hearsay pair <uri>` (or tells his Claude "install this
// hearsay invite: <uri>" if the consumer CLAUDE.md block is installed).
// Pair parses the URI and delegates to the same `claude mcp add`
// plumbing that `add-peer` uses.

// runInvite prints a hearsay:// URI Ivan can share with Wiktor over a
// secret channel (1Password, Signal, etc.). The host is auto-detected
// from Tailscale (MagicDNS name) when available; --host overrides.
func runInvite(args []string) int {
	fs := flag.NewFlagSet("hearsay invite", flag.ContinueOnError)
	var (
		host = fs.String("host", "", "hostname to embed (default: Tailscale MagicDNS name)")
		port = fs.Int("port", 3456, "port the server is listening on")
		path = fs.String("path", "/mcp", "HTTP path")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearsay invite: %v\n", err)
		return 1
	}
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "hearsay invite: no config found. Run `hearsay --name <name>` at least once to initialize.")
		return 1
	}

	effectiveHost := *host
	if effectiveHost == "" {
		if dns := tailscale.SelfDNSName(); dns != "" {
			effectiveHost = dns
		} else if ip := tailscale.DetectIPv4(); ip != "" {
			effectiveHost = ip
		}
	}
	if effectiveHost == "" {
		fmt.Fprintln(os.Stderr, "hearsay invite: could not detect host — Tailscale isn't running or the CLI is not on PATH. Pass --host <hostname>.")
		return 1
	}

	invite := url.URL{
		Scheme:   "hearsay",
		User:     url.User(cfg.Name),
		Host:     fmt.Sprintf("%s:%d", effectiveHost, *port),
		Path:     *path,
		RawQuery: url.Values{"token": []string{cfg.Token}}.Encode(),
	}
	fmt.Println(invite.String())
	return 0
}

// runPair accepts a hearsay:// URI and registers the peer. Thin wrapper
// around the add-peer logic — parses the URI, validates it, then shells
// out to `claude mcp add` the same way add-peer does.
func runPair(args []string) int {
	uri, rest := extractFirstPositional(args)
	fs := flag.NewFlagSet("hearsay pair", flag.ContinueOnError)
	scope := fs.String("scope", "user", "claude mcp scope (user|project|local)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if uri == "" {
		uri = fs.Arg(0)
	}
	if uri == "" {
		fmt.Fprintln(os.Stderr, "usage: hearsay pair <hearsay://…-uri> [--scope user]")
		return 2
	}

	name, httpURL, token, err := parseInvite(uri)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hearsay pair: %v\n", err)
		return 2
	}
	return addPeerExec(name, httpURL, token, *scope)
}

// parseInvite unpacks a hearsay:// URI into the three fields add-peer
// needs. The scheme-to-HTTP translation is deterministic: hearsay://
// always maps to http:// (WireGuard encrypts the tailnet transport).
func parseInvite(raw string) (name, httpURL, token string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", "", fmt.Errorf("invalid URI: %w", perr)
	}
	if u.Scheme != "hearsay" {
		return "", "", "", fmt.Errorf("expected scheme 'hearsay://', got %q", u.Scheme)
	}
	if u.User == nil || u.User.Username() == "" {
		return "", "", "", errors.New("URI must carry a peer name (hearsay://<name>@host:port/…)")
	}
	name = u.User.Username()
	if !peerNameRe.MatchString(name) {
		return "", "", "", fmt.Errorf("peer name %q is invalid — must be lowercase alphanumeric + dash starting with a letter", name)
	}
	if u.Host == "" {
		return "", "", "", errors.New("URI must carry a host")
	}
	token = u.Query().Get("token")
	if token == "" {
		return "", "", "", errors.New("URI must carry a token query parameter")
	}
	path := u.Path
	if path == "" {
		path = "/mcp"
	}
	httpURL = (&url.URL{Scheme: "http", Host: u.Host, Path: path}).String()
	return name, httpURL, token, nil
}

// addPeerExec is the `claude mcp add` shell-out, factored so both
// add-peer and pair reuse it.
func addPeerExec(name, httpURL, token, scope string) int {
	claude, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "hearsay: `claude` CLI not on PATH — install Claude Code first (https://claude.com/claude-code)")
		return 1
	}
	cmd := exec.Command(claude, "mcp", "add",
		"--scope", scope,
		"--transport", "http",
		name, httpURL,
		"-H", "Authorization: Bearer "+token,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "hearsay: `claude mcp add` failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "hearsay: registered peer %q (scope=%s). Restart Claude Code (or run /mcp) to see %q with its tools.\n", name, scope, name)
	return 0
}

// runRemovePeer deletes a previously-added peer entry.
func runRemovePeer(args []string) int {
	name, rest := extractFirstPositional(args)
	fs := flag.NewFlagSet("hearsay remove-peer", flag.ContinueOnError)
	scope := fs.String("scope", "user", "claude mcp scope (user|project|local)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if name == "" {
		name = fs.Arg(0)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: hearsay remove-peer <name> [--scope user]")
		return 2
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "hearsay remove-peer: `claude` CLI not on PATH")
		return 1
	}
	cmd := exec.Command(claude, "mcp", "remove", "--scope", *scope, name)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "hearsay remove-peer: `claude mcp remove` failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "hearsay: removed peer %q (scope=%s). Restart Claude Code to drop its tools.\n", name, *scope)
	return 0
}
