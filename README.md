# hearsay

Read a teammate's Claude Code session transcripts over an MCP bridge.

When a teammate (Ivan, Peter, ...) reports "my Claude did X and Y," you don't want to human-relay follow-up questions. `hearsay` runs on their machine, exposes their `~/.claude/projects/` over an authenticated MCP endpoint, and your Claude reads the transcript directly. No relay, no paraphrase — primary evidence.

## Prerequisites

Both sides — the teammate running the server and the reader consuming it — need:

- **[Go](https://go.dev/dl/) ≥ 1.25** — `brew install go` on macOS. Required to `go install` the binary.
- **[Claude Code](https://claude.com/claude-code)** — the whole point is bridging two Claude Code sessions. Teammates need it running so there's something to read; readers need it to consume the MCP tools. Install via `npm install -g @anthropic-ai/claude-code` or see the official docs.

For anything beyond loopback (reader and server on the same machine), you also need a network path between the two machines:

- **[Tailscale](https://tailscale.com/download)** (recommended) — both machines join a shared tailnet; `hearsay` auto-detects the Tailscale IPv4 and binds there. Zero public exposure, no cert setup, and the tailnet is WireGuard-encrypted so plain HTTP is fine. Install: `brew install --cask tailscale`.
- **[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)** (alternative) — `cloudflared tunnel --url http://localhost:3456` gives a public HTTPS URL. The bearer token is the only access control; consider stacking Cloudflare Access for IP allowlisting.
- **Loopback only** — if you just want to test against your own sessions without any network, skip both. `--bind 127.0.0.1` keeps the server private to the host.

**Platform support:** macOS is the primary target (the `brew install` commands assume it). Linux should work — the config path falls back to `$XDG_CONFIG_HOME/hearsay` (default `~/.config/hearsay`). Windows is untested.

## Install

```bash
go install github.com/WiktorStarczewski/hearsay/cmd/hearsay@latest
```

Requires Go ≥ 1.25. Single static binary; installs to `$(go env GOBIN)` (usually `~/go/bin`).

## Run (teammate side)

```bash
hearsay --name ivan --port 3456
```

First run prints a bearer token — send that to whoever will be reading your sessions over a secret channel (1Password, Signal). The token is persisted to `~/Library/Application Support/hearsay/config.json` (macOS) or `~/.config/hearsay/config.json` (Linux); subsequent startups reuse it silently.

By default the server binds to the machine's Tailscale IPv4 address (via `tailscale ip -4`) or falls back to `127.0.0.1` if Tailscale isn't detected. Override with `--bind <addr>` if you want LAN exposure (`--bind 0.0.0.0`) or a specific interface.

## Connect (consumer side, e.g. Wiktor's Claude Code)

Add an entry to `~/.claude.json` under `mcpServers`, one per teammate:

```json
{
  "mcpServers": {
    "ivan": {
      "type": "http",
      "url": "http://ivan-mac.tailXXXX.ts.net:3456/mcp",
      "headers": {
        "Authorization": "Bearer ${IVAN_HEARSAY_TOKEN}"
      }
    }
  }
}
```

Export the token:

```bash
export IVAN_HEARSAY_TOKEN=<token-ivan-sent-you>
```

Restart Claude Code. `/mcp` should list `ivan` with 8 tools.

## Tools

| Tool | Purpose |
|---|---|
| `list_sessions` | List recent session transcripts, sorted by `lastActivityAt` desc. `isLive` flags sessions written in the last 5 min. |
| `get_current_session` | Return the single live session, or `{ambiguous: true, candidates}` if multiple are active. Never picks silently. |
| `read_session` | Markdown transcript + JSON pagination metadata (windowed via `fromTurn` / `toTurn`). |
| `search_session` | Literal substring match within a single session, with surrounding-turn context. |
| `read_subagent` | Fetch a nested Agent-tool session by its agentUuid. |
| `read_tool_result` | Fetch the full content of a tool result (Read outputs, long stdouts). Handles inline + sidecar storage. |
| `get_session_summary` | Compact digest: first user ask, tool-call counts, subagent list, last assistant text. |
| `get_peer_info` | `{name, version, sessionCount, activeSessionCount}` — sanity-check which peer you're talking to. |

## Optional: CLAUDE.md discoverability block

Tool descriptions on each hearsay instance bake in the peer's `--name`, which is enough for Claude to auto-route "Ivan reported X" to `mcp__ivan__*`. If you want an extra nudge (and a "don't relay through me" directive), install the block:

```bash
hearsay claude-md install                   # consumer side (for the reader)
hearsay claude-md install --role peer       # peer side (for a teammate whose Claude should share session IDs)
hearsay claude-md print                     # dump to stdout
hearsay claude-md uninstall                 # remove
```

Idempotent via HTML-comment markers (`<!-- hearsay:consumer-auto-start/end -->`, `<!-- hearsay:peer-auto-start/end -->`), so re-running `install` is safe. The two blocks use disjoint markers so both can coexist.

## CLI reference

```
hearsay [flags]                        # run the server (default)
hearsay claude-md <action> [flags]     # manage CLAUDE.md blocks
hearsay version                        # print version

Server flags:
  --name <name>              peer identity (required on first run; persisted)
  --port <n>                 listen port (default 3456)
  --bind <addr>              bind address (default: tailscale IPv4, else 127.0.0.1)
  --data-dir <path>          Claude Code data dir (default ~/.claude)
  --live-window-seconds <n>  isLive threshold (default 300)
  --regenerate-token         rotate the stored bearer token
  --quiet                    suppress tool-call logs
```

## Design notes

Read the full plan at `/Users/celrisen/.claude/plans/lets-prototype-the-transcript-reader-toasty-cake.md` (same repo at some point — currently a standalone plan file). Key decisions the plan documents:

- Each `hearsay` instance is named (`--name ivan`). The name is baked into every tool description at registration time, so Claude Code's natural routing (user mentions "Ivan" → `mcp__ivan__*` tools) works without any consumer-side config.
- `get_current_session` returns an explicit `ambiguous` field rather than silently picking among multiple live sessions. The tool description tells the calling Claude to ASK the user when ambiguous.
- The JSONL parser tolerates truncated last lines (active sessions mid-write) and unknown event types (forward-compat).
- Tool-result sidecar paths are extracted by regex from the inline message content — the sidecar filename is *not* the `tool_use.id`.

## Out of scope

This is a read-only prototype. Out of scope: redaction, write-back/interactive Q&A to the remote Claude, cross-session search, signed release binaries.
