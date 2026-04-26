# hearsay

[![ci](https://github.com/WiktorStarczewski/hearsay/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/WiktorStarczewski/hearsay/actions/workflows/ci.yml?query=branch%3Amain)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/WiktorStarczewski/hearsay/badges/coverage.json)](https://github.com/WiktorStarczewski/hearsay/actions/workflows/ci.yml?query=branch%3Amain)

Read a teammate's Claude Code session transcripts — and optionally drive a fresh read-only agent on their box — over an authenticated MCP bridge.

When a teammate (Ivan, Peter, ...) reports "my Claude did X and Y," you don't want to human-relay follow-up questions. `hearsay` runs on their machine and exposes two things to your Claude over an authenticated MCP endpoint:

1. **Past sessions.** Their `~/.claude/projects/` (transcripts, subagent traces, tool-result sidecars) so your Claude can read what they already did. No relay, no paraphrase — primary evidence.
2. **Live queries (opt-in via `--enable-agent`).** A parallel Claude Code session you can drive with `Read` / `Glob` / `Grep` against their live filesystem, in one-shot or stateful-conversation form. Useful when their report mentions a `timeline.ndjson` you'd want to grep right now, not yesterday's conclusion. Off by default; the peer opts in by passing `--enable-agent`. Agent calls bill against the peer's Claude Code subscription quota — no Anthropic API key required.

Both capabilities ride the same Tailscale-friendly bearer-token transport. Live-query tools execute on the peer's box via a `claude --print` subprocess and are bounded per call by tool-call / wall-clock budgets.

## Prerequisites

Both sides — the teammate running the server and the reader consuming it — need:

- **[Go](https://go.dev/dl/) ≥ 1.26** — `brew install go` on macOS. Required to `go install` the binary.
- **[Claude Code](https://claude.com/claude-code)** — the whole point is bridging two Claude Code sessions. Teammates need it running so there's something to read; readers need it to consume the MCP tools. Install via `npm install -g @anthropic-ai/claude-code` or see the official docs.

For anything beyond loopback (reader and server on the same machine), you also need a network path between the two machines:

- **[Tailscale](https://tailscale.com/download)** (recommended) — each side has Tailscale installed; traffic rides the WireGuard-encrypted tailnet so plain HTTP on `:3456` is fine. In the common case each person has their own personal or org tailnet and the sender **shares** their hearsay-hosting node with the receiver — no shared-tailnet membership required. See [Tailscale setup](#tailscale-setup) below for the step-by-step. Install: `brew install --cask tailscale`.
- **[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)** (alternative) — `cloudflared tunnel --url http://localhost:3456` gives a public HTTPS URL. The bearer token is the only access control; consider stacking Cloudflare Access for IP allowlisting.
- **Loopback only** — if you just want to test against your own sessions without any network, skip both. `--bind 127.0.0.1` keeps the server private to the host.

**Platform support:** macOS is the primary target (the `brew install` commands assume it). Linux should work — the config path falls back to `$XDG_CONFIG_HOME/hearsay` (default `~/.config/hearsay`). Windows is untested.

## Tailscale setup

Both sides need Tailscale on their machine and a network path between them. The usual shape: **each person runs their own tailnet, and the hearsay-hosting side uses Tailscale's *node sharing* to let the receiver reach them.** You never have to join someone else's tailnet.

### 1. Install Tailscale (per machine, one-time)

**macOS:**
```bash
brew install --cask tailscale
```

This installs `Tailscale.app`. Other platforms: https://tailscale.com/download.

### 2. Approve the Network Extension (macOS only, easy to miss)

Open the Tailscale app. On first launch macOS needs you to approve a system network extension — do this in:

- **System Settings → General → Login Items & Extensions → Network Extensions** (macOS 15+), or
- **System Settings → Privacy & Security** (older macOS — scroll to the pending prompt).

Toggle **Tailscale Network Extension** on. Without this step the app will spin forever, `tailscaled` never comes up, and your Tailscale admin panel will show *"waiting for your first device."* Check with:

```bash
systemextensionsctl list | grep tailscale
# you want: [activated enabled]  (NOT [activated waiting for user])
```

### 3. Sign in

In the Tailscale app, sign in (or create a free account). You get your own tailnet with a suffix like `tail046457.ts.net` and your machine is assigned a MagicDNS hostname. Confirm:

```bash
tailscale status --self --json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["Self"]["DNSName"].rstrip("."))'
```

That hostname is what `hearsay invite` embeds when you run it — no additional config.

### 4. (Hearsay peer side) Run hearsay

Nothing special — by default `hearsay --name <you>` binds to your Tailscale interface IPv4 so only tailnet traffic can hit it. `hearsay invite` auto-detects your MagicDNS hostname and stamps it into the invite URI.

### 5. (Hearsay consumer side) Accept a node share

When someone sends you an invite URI (`hearsay://.../*.ts.net:3456/...`) and you're **not** already on their tailnet, they need to share their hearsay-hosting node with you:

- **You:** send them the email address you used to sign into Tailscale. Find it with:
  ```bash
  tailscale status --self --json | python3 -c 'import json,sys; d=json.load(sys.stdin); self=d["Self"]; print(d["User"][str(self["UserID"])]["LoginName"])'
  ```
- **Sender:** opens the Tailscale admin at https://login.tailscale.com/admin/machines, clicks `…` → **Share…** on their hearsay-hosting node, and pastes your email.
- **You:** get an email and an in-app notification. Accept it. The shared node shows up under **"Machines — shared with you"** in your Tailscale app.

### 6. Verify reachability before pairing

```bash
curl -I http://<their-hostname>.<their-tailnet>.ts.net:3456/health
# HTTP/1.1 200 OK  (unauthenticated probe — tunnel / reverse-proxy friendly)
```

If that responds, `hearsay pair <uri>` will succeed. If it hangs or gives a timeout, the share hasn't been accepted / propagated yet.

### Multiple peers on different tailnets

You can accept shares from any number of teammates. Each stays on their own tailnet suffix (`ivan-mac.tailAAAA.ts.net`, `peter-mbp.tailBBBB.ts.net`, ...) and each appears as a separate `mcpServers` entry in your `~/.claude.json`. You only ever maintain one Tailscale client on one tailnet.

## Install

```bash
go install github.com/WiktorStarczewski/hearsay/cmd/hearsay@latest
```

Requires Go ≥ 1.26. Single static binary; installs to `$(go env GOBIN)` (usually `~/go/bin`).

## Run (teammate side)

```bash
hearsay --name ivan --port 3456
```

First run prints a bearer token — send that to whoever will be reading your sessions over a secret channel (1Password, Signal). The token is persisted to `~/Library/Application Support/hearsay/config.json` (macOS) or `~/.config/hearsay/config.json` (Linux); subsequent startups reuse it silently.

By default the server binds to the machine's Tailscale IPv4 address (via `tailscale ip -4`) or falls back to `127.0.0.1` if Tailscale isn't detected. Override with `--bind <addr>` if you want LAN exposure (`--bind 0.0.0.0`) or a specific interface.

## Connect (consumer side, e.g. Wiktor's Claude Code)

Preferred flow: Ivan generates a one-line invite, Wiktor pairs against it.

**On Ivan's machine** (once his server is running):

```bash
hearsay invite
# → hearsay://ivan@ivan-mac.tailXXXX.ts.net:3456/mcp?token=abc123...
```

Ivan sends that line to Wiktor over a secret channel (1Password, Signal). When Tailscale is running, the host is auto-detected from MagicDNS; otherwise pass `--host <hostname>`.

**On Wiktor's machine**:

```bash
hearsay pair hearsay://ivan@ivan-mac.tailXXXX.ts.net:3456/mcp?token=abc123...
```

That's it — `pair` writes the `mcpServers` entry into `~/.claude.json` via `claude mcp add --scope user`. Restart Claude Code; `/mcp` should list `ivan` with 8 read-only tools (or 13 if Ivan started with `--enable-agent`).

### Install via a Claude prompt

If you've installed the consumer CLAUDE.md block (see the next section), you can skip the CLI entirely:

> install this hearsay invite: hearsay://ivan@ivan-mac.tailXXXX.ts.net:3456/mcp?token=abc123...

Or, if you still have raw fields:

> install the hearsay mcp server for ivan at http://ivan-mac.tailXXXX.ts.net:3456/mcp with token abc123...

Claude parses either form and runs `hearsay pair` / `hearsay add-peer`.

### Other consumer commands

- `hearsay add-peer ivan --url <url> --token <token>` — the three-field form, if you don't have an invite URI.
- `hearsay remove-peer ivan` — un-register a peer.

### Manual alternative

If you'd rather edit the config yourself, add this under `mcpServers` in `~/.claude.json`:

```json
{
  "mcpServers": {
    "ivan": {
      "type": "http",
      "url": "http://ivan-mac.tailXXXX.ts.net:3456/mcp",
      "headers": {
        "Authorization": "Bearer <token-ivan-sent-you>"
      }
    }
  }
}
```

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
| `ask_peer_claude` | **Requires `--enable-agent`.** Spawns a parallel Claude Code subprocess on the peer's box with read-only filesystem tools (`Read` / `Glob` / `Grep`). Returns a markdown transcript + `{turnCount, toolCallCount, stopReason, elapsedMs}`. |
| `start_peer_conversation` | **Requires `--enable-agent`.** Open a stateful read-only conversation. Returns `{convId, startedAt, effectiveBudget}`. |
| `send_peer_message` | **Requires `--enable-agent`.** One more turn against an existing convId. |
| `list_peer_conversations` | **Requires `--enable-agent`.** Active conversations sorted by `lastActivityAt` desc. |
| `end_peer_conversation` | **Requires `--enable-agent`.** Terminate a conversation; idempotent. |

## Interactive mode

When `--enable-agent` is set, hearsay can also *drive a fresh Claude Code session* on the peer's machine that can `Read` / `Glob` / `Grep` the live filesystem — without the teammate touching anything. Useful when you need primary data, not Ivan's after-the-fact diagnosis.

**Off by default.** A binary without `--enable-agent` exposes only the read-only transcript tools.

### How it actually works

Hearsay drives a `claude --print` subprocess on the peer's machine for every agent call. **No API key is required by default** — the subprocess inherits the peer's existing Claude Code OAuth credentials and bills against their subscription (Pro / Max / Team) the same 5-hour rolling window their interactive use draws from.

Architecturally:

- The agent runs in a **separate, parallel** Claude Code session on the peer's machine — not the one they're typing into. It never sees the peer's interactive history; the peer's interactive Claude never sees its prompts.
- `--allowed-tools "Read Glob Grep"` is hardcoded on every invocation. Claude Code itself enforces the read-only allowlist; hearsay doesn't dispatch tools manually.
- The session JSONL gets written to `~/.claude/projects/<encoded-cwd>/<convId>.jsonl` like any other Claude Code session — so the `list_sessions` / `read_session` tools surface it for post-hoc inspection.

### Enable on the peer side

```bash
hearsay --name ivan --enable-agent
```

That's it. Prerequisites: the peer is OAuth-logged-in to Claude Code (i.e. has run `claude` interactively at least once), and `claude` is on `PATH` (or pointed at via `--agent-claude-bin <path>`).

If `--enable-agent` is set but `claude` isn't on `PATH`, hearsay refuses to start with a friendly error. No half-configured state.

### Use it from the consumer side

After `hearsay pair <invite>` and a Claude Code restart, your Claude gets five new tools per peer:

- `mcp__ivan__ask_peer_claude({prompt, project?, max_tokens?, max_tool_calls?, timeout_seconds?})` — spawns a fresh Claude Code session on Ivan's box. Returns a markdown transcript + `{turnCount, toolCallCount, stopReason, elapsedMs}`. No state kept after the call.
- `mcp__ivan__start_peer_conversation({system_prompt?, project?, ...budget})` — creates a stateful read-only conversation. Returns `{convId, startedAt, effectiveBudget}`. The convID is the Claude Code session UUID.
- `mcp__ivan__send_peer_message({convId, prompt, ...budget})` — one more turn on an existing conversation. Per-turn budget overrides cascade through the conversation's defaults.
- `mcp__ivan__list_peer_conversations({})` — active conversations sorted by `lastActivityAt` desc.
- `mcp__ivan__end_peer_conversation({convId})` — terminate and free the slot. Idempotent.

Example natural-language prompt to your Claude:

> "ask Ivan's box to grep for `lock starvation` in his stress-test logs"

Routes to `ask_peer_claude` automatically. To replay what Ivan already did (read past transcripts), Claude reaches for `list_sessions` / `read_session` instead.

### What the peer's agent CAN'T do

- Run shell commands (`Bash` is **not** in the read-only allowlist).
- Edit / write / delete files.
- Read files outside the project root the agent was started in.
- Send messages back into Ivan's interactive Claude Code session — it's a separate parallel session, not a hijack.
- Run for longer than the per-call wall-clock budget; runaway sessions are bounded by SIGTERM (then SIGKILL after 5 s grace).

A two-leg defense protects the allowlist: (1) `--allowed-tools "Read Glob Grep"` is the load-bearing control — Claude Code itself enforces it. (2) After every call, hearsay replays the session JSONL through the same transcript parser the `read_session` tool uses, and rejects the turn if any `tool_use.name` is outside the allowlist (defense-in-depth against a future-Claude-Code drift / corrupted build).

### Quota & cost

Agent calls consume the **peer's** Claude Code subscription quota — same 5-hour rolling window as interactive use. A noisy hearsay session and Ivan's own chat compete for the same budget. The `MaxTokens` budget is **soft** on this path (the CLI doesn't expose a token cap; we weave the value into the system prompt as a nudge). `MaxToolCalls` and `Timeout` are hard caps, enforced by the JSONL replay and the subprocess deadline respectively.

### Using an API key instead of the subscription

If you'd rather bill against an Anthropic API account (team account / CI runner / batch jobs), set `ANTHROPIC_API_KEY` and pass `--agent-keep-env-key`:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
hearsay --name ivan --enable-agent --agent-keep-env-key
```

The flag is **off by default** — without it, hearsay strips `ANTHROPIC_API_KEY` from the subprocess env so the subscription OAuth path always wins (avoids the silent-billing footgun where a stale env var redirects costs).

### Audit log

Every agent call appends one JSON line to `~/Library/Logs/hearsay/agent.log` (macOS) or `$XDG_STATE_HOME/hearsay/agent.log` (Linux): `{timestamp, peer, convId, turnIndex, promptBytes, toolCalls: [{name, argBytes}], responseBytes, elapsedMs, stopReason, errorSummary?, envAPIKeyHandling?}`. **Sizes only — no prompt / response / tool-arg content, no hashes.** Override with `--agent-log-path <file>`.

### Known limitations

- **Wrong-account auth is unverifiable.** Hearsay can't tell whether `claude` is OAuth-logged-in to the account the peer intended (work vs personal). The first agent call surfaces a "not logged in" error if it isn't logged in at all; an account mix-up bills against whatever the local session says. Sanity-check via `claude` interactively before opting in.
- **`MaxTokens` is a soft budget.** See above — Claude Code CLI doesn't expose a token cap, so the cap rides the system prompt as a nudge. Use `--agent-default-max-tool-calls` and `--agent-default-timeout-seconds` for hard limits.
- **Conversation metadata doesn't survive a hearsay restart.** The local map (`lastActivityAt`, `turnCount`, preview) is in-memory. The underlying Claude Code session JSONL persists, so Wiktor can still inspect a dropped conversation via `read_session` — just not call `send_peer_message` against it.
- **Subprocess automation isn't explicitly endorsed in Anthropic's ToS** as of 2026-04-25. Not prohibited either. Operators should re-check before deploying widely.

### Agent flags

```
--enable-agent                          register the agent tools (off by default)
--agent-claude-bin <path>               override the `claude` binary path (default: resolve via PATH)
--agent-keep-env-key                    keep ANTHROPIC_API_KEY in the agent subprocess env
                                        (default: stripped so subscription OAuth wins)
--agent-default-max-tokens <n>          soft per-turn token budget (default 32768; nudge in system prompt)
--agent-default-max-tool-calls <n>      hard per-turn tool-call cap (default 20; via JSONL replay)
--agent-default-timeout-seconds <n>     hard per-call wall-clock cap (default 120; subprocess deadline)
--agent-log-path <path>                 audit log path (default platform-specific)
--max-conversations <n>                 concurrent-conversations cap (default 10)
--conversation-idle-timeout <dur>       reap conversations idle past this (default 15m)
```

**Not supported:** `--agent-api-key-env`, `--agent-base-url`, `--agent-model`. Each hard-fails on use with a pointer at the subscription-OAuth path. Auth is fully inherited from the peer's Claude Code install.

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

Agent flags (off by default; require --enable-agent):
  --enable-agent
  --agent-api-key-env <NAME>             (default ANTHROPIC_API_KEY)
  --agent-base-url <url>                 (test stubs / regional endpoints)
  --agent-model <id>                     (default claude-opus-4-7)
  --agent-default-max-tokens <n>         (default 32768)
  --agent-default-max-tool-calls <n>     (default 20)
  --agent-default-timeout-seconds <n>    (default 120)
  --agent-log-path <path>
  --max-conversations <n>                (default 10)
  --conversation-idle-timeout <dur>      (default 15m)
```

## Design notes

### Routing & transcripts

- Each `hearsay` instance is named (`--name ivan`). The name is baked into every tool description at registration time, so Claude Code's natural routing (user mentions "Ivan" → `mcp__ivan__*` tools) works without any consumer-side config.
- `get_current_session` returns an explicit `ambiguous` field rather than silently picking among multiple live sessions. The tool description tells the calling Claude to ASK the user when ambiguous.
- The JSONL parser tolerates truncated last lines (active sessions mid-write) and unknown event types (forward-compat).
- Tool-result sidecar paths are extracted by regex from the inline message content — the sidecar filename is *not* the `tool_use.id`.
- `read_tool_result` returns a single TextContent block with the metadata inlined as a leading line (`[source=…, bytes=…, truncated=…]`) rather than a separate `StructuredContent` block. Some MCP consumers were surfacing only the structured channel back to the calling model, which experienced as "metadata-only" reads against large sidecars.

### Agent architecture

- **Tools execute on the peer's box via `claude --print` subprocess.** Hearsay never opens an outbound connection to api.anthropic.com itself; it spawns a fresh Claude Code session per call (or per conversation) and Claude Code talks to the API. The peer's existing OAuth credentials authenticate the calls, billing the subscription.
- **Read-only allowlist with two-leg adversarial defense.** Every invocation passes `--allowed-tools "Read Glob Grep"` — Claude Code itself enforces this; that's the load-bearing control. Independently, after the subprocess returns, hearsay replays the session JSONL through the same parser the `read_session` tool uses and rejects the turn if any `tool_use.name` is outside the allowlist (defense-in-depth against future-Claude-Code drift / corrupted builds). Widening the allowlist (`Bash`, `Edit`, `Write`) would simultaneously flip the security posture and require an explicit security review — not a runtime knob.
- **Native sandboxing inherited from Claude Code.** The spawned `claude --print` runs with `cwd` set to the project root the agent was started in; Claude Code's own `Read` / `Glob` / `Grep` implementations honor that root. Per-tool caps (e.g., "64 KB per `Read`", binary-file skip in `Grep`) come from Claude Code, not hearsay.
- **Conversation state persists on disk, in Claude Code's own format.** Each conversation's JSONL lives at `~/.claude/projects/<encoded-cwd>/<convId>.jsonl` — the same files `read_session` already surfaces. Hearsay's local map (`startedAt`, `lastActivityAt`, `turnCount`, system-prompt preview) is metadata only and doesn't survive a restart, but Wiktor can still inspect a dropped conversation via `read_session` after the fact.
- **Sizes-only audit log.** Every agent call appends one JSON line with timestamps, peer name, convId, turn index, prompt-byte-size, tool-invocation `{name, argBytes}` pairs, response-byte-size, elapsed ms, stop reason, and `envAPIKeyHandling` (which auth path billed the call). **No prompt, response, or tool-arg content. No hashes.** Sizes alone are sufficient for volume + latency diagnosis.
- **`ANTHROPIC_API_KEY` is stripped from the subprocess env by default.** Claude Code's auth precedence is `ANTHROPIC_API_KEY > apiKeyHelper > OAuth/keychain`. To make subscription OAuth always win, hearsay strips the env var unless `--agent-keep-env-key` is passed. Avoids the silent-billing footgun where a stale API key in the operator's shell silently redirects costs.
- **Bounded per call.** `--agent-default-max-tool-calls` (default 20, hard cap via JSONL replay), `--agent-default-timeout-seconds` (default 120, hard cap via subprocess deadline + 5 s SIGKILL grace), `--max-conversations` (default 10) all cap blast radius. `--agent-default-max-tokens` (default 32 k) is a *soft* budget on this path — the CLI doesn't expose a token cap, so the value rides the system prompt as a nudge.
- **Single MCP surface.** Agent tools register alongside the read-only transcript tools on the same `mcp.Server` instance. Tailnet binding, bearer token, claude-md discoverability, and the rest of the operational story are unchanged whether `--enable-agent` is on or off.
- **`claude` is the only runtime dependency.** Hearsay imports just `github.com/modelcontextprotocol/go-sdk` and a handful of stdlib-adjacent helpers; everything else routes through the spawned Claude Code subprocess.

## License

MIT — see [LICENSE](./LICENSE).

