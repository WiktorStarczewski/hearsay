package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// Config bundles everything sdkAgent needs.  Built once in main.go and
// reused for every call.
type Config struct {
	APIKey       string
	BaseURL      string // empty => default api.anthropic.com; tests set ANTHROPIC_BASE_URL
	Model        string // default "claude-opus-4-7"
	PeerName     string
	DefaultBudget Budget
	Auditor      *Auditor
	// FallbackProject is the cwd handlers see when OneShotRequest.Project is empty
	// AND no most-recent-session-cwd is available.  Defaults to os.Getwd() at
	// construction time.
	FallbackProject string
}

// New constructs a production sdkAgent backed by anthropic-sdk-go.
// Returns nil + error if the SDK init fails.
func New(cfg Config) (Agent, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("agent: APIKey is empty")
	}
	if cfg.Model == "" {
		cfg.Model = "claude-opus-4-7"
	}
	if cfg.FallbackProject == "" {
		if cwd, err := os.Getwd(); err == nil {
			cfg.FallbackProject = cwd
		}
	}

	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	client := anthropic.NewClient(opts...)
	return &sdkAgent{cfg: cfg, client: &client}, nil
}

// sdkAgent is the production Agent implementation.  Wraps the SDK and
// drives runEventLoop with translated events.
type sdkAgent struct {
	cfg    Config
	client *anthropic.Client

	// initOnce + initErr lazily provision a single Environment + Agent
	// per process.  The Managed-Agents API requires a session to live
	// inside an Environment (the model runs there even when our
	// custom-tool callback path means tools execute on Ivan's box).
	initOnce sync.Once
	initErr  error
	envID    string
	agentID  string
}

// OneShot implements Agent.OneShot.  Creates a fresh agent + session
// per call; PR B will reuse a session across multiple SendMessage
// calls.
func (a *sdkAgent) OneShot(ctx context.Context, req OneShotRequest) (Transcript, error) {
	budget := req.Budget.Resolve(a.cfg.DefaultBudget)
	project := a.resolveProject(req.Project)
	if project == "" {
		return Transcript{
			Markdown:     "_no project root resolved_\n",
			StopReason:   StopReasonError,
			ErrorSummary: ErrInvalidProject,
		}, nil
	}

	// Apply the wall-clock deadline at the context level so both
	// streaming reads and tool-result sends honor it.
	if budget.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget.Timeout)
		defer cancel()
	}

	start := time.Now()
	turnIndex := 1
	tx, invokes, err := a.runOnce(ctx, req.Prompt, project, budget)
	tx.ElapsedMs = time.Since(start).Milliseconds()

	if a.cfg.Auditor != nil {
		_ = a.cfg.Auditor.Log(AuditEntry{
			Timestamp:     time.Now().UTC(),
			PeerName:      a.cfg.PeerName,
			ConvID:        "oneshot",
			TurnIndex:     turnIndex,
			PromptBytes:   len(req.Prompt),
			ResponseBytes: len(tx.Markdown),
			ToolCalls:     invokes,
			ElapsedMs:     tx.ElapsedMs,
			StopReason:    tx.StopReason,
			ErrorSummary:  tx.ErrorSummary,
		})
	}
	return tx, err
}

// ensureInit lazily registers a single Environment + Agent for this
// process.  Both are reused across OneShot / SendMessage calls.
func (a *sdkAgent) ensureInit(ctx context.Context) error {
	a.initOnce.Do(func() {
		env, err := a.client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
			Name: fmt.Sprintf("hearsay-%s", a.cfg.PeerName),
			// Default config is fine — tools execute on Ivan's box
			// via the custom_tool_use callback path; we never use
			// the bundled toolset that would run inside the sandbox.
		})
		if err != nil {
			a.initErr = fmt.Errorf("environment create: %w", err)
			return
		}
		a.envID = env.ID

		agentResp, err := a.client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name:  fmt.Sprintf("hearsay-%s", a.cfg.PeerName),
			Model: anthropic.BetaManagedAgentsModelConfigParams{ID: a.cfg.Model},
			Tools: buildCustomToolUnion(),
			System: param.NewOpt(
				"You are an investigative assistant running on a teammate's machine. " +
					"You can read, glob, and grep files under the working directory. " +
					"You have NO ability to run shell commands or write files. " +
					"Be concise; if a question requires more inspection than the user " +
					"provided, say so and ask for more guidance rather than guessing."),
		})
		if err != nil {
			a.initErr = fmt.Errorf("agent create: %w", err)
			return
		}
		a.agentID = agentResp.ID
	})
	return a.initErr
}

// runOnce: open a session, send the prompt, drain events through
// runEventLoop, return the assembled transcript.
func (a *sdkAgent) runOnce(
	ctx context.Context,
	prompt, project string,
	budget Budget,
) (Transcript, []AuditToolInvoke, error) {
	if err := a.ensureInit(ctx); err != nil {
		return Transcript{
			Markdown:     fmt.Sprintf("_init failed: %s_\n", err.Error()),
			StopReason:   StopReasonError,
			ErrorSummary: classifyErrorMsg(err.Error()),
		}, nil, nil
	}

	// Open a session against the cached agent + environment.
	sess, err := a.client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: param.NewOpt(a.agentID)},
		EnvironmentID: a.envID,
	})
	if err != nil {
		return Transcript{
			Markdown:     fmt.Sprintf("_session creation failed: %s_\n", err.Error()),
			StopReason:   StopReasonError,
			ErrorSummary: classifyErrorMsg(err.Error()),
		}, nil, nil
	}

	// Send the user's prompt.
	if _, err := a.client.Beta.Sessions.Events.Send(ctx, sess.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{
			{
				OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
					Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
					Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{
						{OfText: &anthropic.BetaManagedAgentsTextBlockParam{Text: prompt}},
					},
				},
			},
		},
	}); err != nil {
		return Transcript{
			Markdown:     fmt.Sprintf("_failed to send user message: %s_\n", err.Error()),
			StopReason:   StopReasonError,
			ErrorSummary: classifyErrorMsg(err.Error()),
		}, nil, nil
	}

	// Step 4: stream events, translate, drive the loop.
	stream := a.client.Beta.Sessions.Events.StreamEvents(ctx, sess.ID, anthropic.BetaSessionEventStreamParams{})
	defer stream.Close()

	events := make(chan LoopEvent, 32)
	go a.pumpStream(ctx, stream, events)

	allow := make(map[string]bool, len(AllowedToolNames))
	for _, n := range AllowedToolNames {
		allow[n] = true
	}

	hooks := LoopHooks{
		ExecuteTool: func(name string, input json.RawMessage) (string, error) {
			handler, ok := customToolHandlers[name]
			if !ok {
				return "", fmt.Errorf("no handler for tool %q", name)
			}
			var args map[string]any
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("decode args: %w", err)
			}
			return handler(args, project)
		},
		SendToolResult: func(toolUseID, content string, isError bool) error {
			_, err := a.client.Beta.Sessions.Events.Send(ctx, sess.ID, anthropic.BetaSessionEventSendParams{
				Events: []anthropic.BetaManagedAgentsEventParamsUnion{
					{
						OfUserCustomToolResult: &anthropic.BetaManagedAgentsUserCustomToolResultEventParams{
							Type:            anthropic.BetaManagedAgentsUserCustomToolResultEventParamsTypeUserCustomToolResult,
							CustomToolUseID: toolUseID,
							Content: []anthropic.BetaManagedAgentsUserCustomToolResultEventParamsContentUnion{
								{OfText: &anthropic.BetaManagedAgentsTextBlockParam{Text: content}},
							},
							IsError: param.NewOpt(isError),
						},
					},
				},
			})
			return err
		},
	}

	tx, invokes, err := runEventLoop(ctx, events, hooks, budget, allow)
	return tx, invokes, err
}

// pumpStream translates SDK union events into LoopEvents.  Closes the
// channel on stream end so runEventLoop's select unblocks.
func (a *sdkAgent) pumpStream(
	ctx context.Context,
	stream interface {
		Next() bool
		Current() anthropic.BetaManagedAgentsStreamSessionEventsUnion
		Err() error
	},
	out chan<- LoopEvent,
) {
	defer close(out)
	for stream.Next() {
		ev := translateStreamEvent(stream.Current())
		select {
		case out <- ev:
		case <-ctx.Done():
			return
		}
	}
	if err := stream.Err(); err != nil {
		select {
		case out <- LoopEvent{Kind: EventError, ErrorMsg: err.Error()}:
		case <-ctx.Done():
		}
	}
}

// translateStreamEvent narrows the SDK union into a LoopEvent.
// Unknown event types become EventOther so the loop ignores them.
func translateStreamEvent(u anthropic.BetaManagedAgentsStreamSessionEventsUnion) LoopEvent {
	switch u.Type {
	case "agent.custom_tool_use":
		input, _ := json.Marshal(u.Input)
		return LoopEvent{
			Kind:      EventCustomToolUse,
			ToolUseID: u.ID,
			ToolName:  u.Name,
			ToolInput: input,
		}
	case "agent.message":
		return LoopEvent{Kind: EventAgentMessage, MessageText: extractAgentText(u)}
	case "session.status_idle":
		return LoopEvent{Kind: EventStatusIdle, StopReasonHint: stopReasonHintFromUnion(u)}
	case "session.error":
		return LoopEvent{Kind: EventError, ErrorMsg: errorMsgFromUnion(u)}
	case "span.model_request_end":
		return LoopEvent{
			Kind:         EventTokenUsage,
			InputTokens:  int(u.ModelUsage.InputTokens),
			OutputTokens: int(u.ModelUsage.OutputTokens),
		}
	default:
		return LoopEvent{Kind: EventOther}
	}
}

func extractAgentText(u anthropic.BetaManagedAgentsStreamSessionEventsUnion) string {
	// agent.message events carry the text under .Content as a
	// []BetaManagedAgentsTextBlock.  Marshal+unmarshal to read it
	// without diving into the SDK's internal union machinery.
	type textBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	raw, err := json.Marshal(u.Content)
	if err != nil {
		return ""
	}
	var blocks []textBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			out.WriteString(b.Text)
		}
	}
	return out.String()
}

func stopReasonHintFromUnion(u anthropic.BetaManagedAgentsStreamSessionEventsUnion) string {
	// stop_reason on session.status_idle is itself a union; the
	// stringified form is what we need.
	raw, err := json.Marshal(u.StopReason)
	if err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		if t, ok := m["type"].(string); ok {
			return t
		}
	}
	return ""
}

func errorMsgFromUnion(u anthropic.BetaManagedAgentsStreamSessionEventsUnion) string {
	raw, err := json.Marshal(u.Error)
	if err != nil {
		return "unknown error"
	}
	return string(raw)
}

// resolveProject implements the OneShotRequest.Project default-and-
// fallback rules:
//
//   1. If req.Project is non-empty AND points at an existing dir, use it.
//   2. If req.Project is non-empty but invalid, return "" so the caller
//      surfaces ErrInvalidProject.
//   3. If req.Project is empty, use FallbackProject (set to os.Getwd()
//      at construction time).
//
// PR B will extend this with the "most-recent session's cwd" fallback
// using the transcript package; for PR A we keep it simple.
func (a *sdkAgent) resolveProject(p string) string {
	if p == "" {
		return a.cfg.FallbackProject
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return ""
	}
	return p
}

// buildCustomToolUnion constructs the BetaAgentNewParams.Tools slice
// containing exactly read / glob / grep — and nothing else.  The
// adversarial test asserts on the result of this function.
func buildCustomToolUnion() []anthropic.BetaAgentNewParamsToolUnion {
	out := make([]anthropic.BetaAgentNewParamsToolUnion, 0, len(AllowedToolNames))
	for _, name := range AllowedToolNames {
		out = append(out, anthropic.BetaAgentNewParamsToolUnion{
			OfCustom: customToolParam(name),
		})
	}
	return out
}

// customToolParam returns the SDK-shaped BetaManagedAgentsCustomToolParams
// for one of our three read-only tools.  Schemas are minimal — agents
// figure out the file path / pattern shape from the description.
func customToolParam(name string) *anthropic.BetaManagedAgentsCustomToolParams {
	switch name {
	case "read":
		return &anthropic.BetaManagedAgentsCustomToolParams{
			Name:        "read",
			Type:        anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
			Description: "Read the contents of a file under the project root. Returns up to 64KB of UTF-8 text with a leading metadata line.",
			InputSchema: anthropic.BetaManagedAgentsCustomToolInputSchemaParam{
				Type: anthropic.BetaManagedAgentsCustomToolInputSchemaTypeObject,
				Properties: map[string]any{
					"file_path": map[string]any{
						"type":        "string",
						"description": "Absolute or root-relative path to the file.",
					},
				},
				Required: []string{"file_path"},
			},
		}
	case "glob":
		return &anthropic.BetaManagedAgentsCustomToolParams{
			Name:        "glob",
			Type:        anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
			Description: "List files under the project root matching a glob pattern. Supports `**` for recursive matches. Returns up to 200 paths.",
			InputSchema: anthropic.BetaManagedAgentsCustomToolInputSchemaParam{
				Type: anthropic.BetaManagedAgentsCustomToolInputSchemaTypeObject,
				Properties: map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Glob pattern (e.g. `**/*.go`, `src/*.ts`).",
					},
				},
				Required: []string{"pattern"},
			},
		}
	case "grep":
		return &anthropic.BetaManagedAgentsCustomToolParams{
			Name:        "grep",
			Type:        anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
			Description: "Search files under the project root for lines matching a Go regular expression. Skips binary files. Returns up to 200 file:line:text matches.",
			InputSchema: anthropic.BetaManagedAgentsCustomToolInputSchemaParam{
				Type: anthropic.BetaManagedAgentsCustomToolInputSchemaTypeObject,
				Properties: map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Go-syntax regular expression.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Optional sub-path under the project root to scope the search.",
					},
					"max_results": map[string]any{
						"type":        "integer",
						"description": "Optional cap on matches (default 200).",
					},
				},
				Required: []string{"pattern"},
			},
		}
	}
	return nil
}
