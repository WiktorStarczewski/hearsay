package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// conversation is the per-conv state hearsay tracks.  The SDK holds
// the full message history server-side; we just track metadata for
// listing / idle-reaping / cap enforcement.
type conversation struct {
	mu sync.Mutex // serializes turns within this conversation

	convID         ConvID
	sessionID      string
	startedAt      time.Time
	lastActivityAt time.Time
	turnCount      int
	perTurnBudget  Budget
	project        string

	// Both previews are stored at start time so listing is cheap.
	// firstUserPreview is filled on the first SendMessage; until then
	// systemPreview is what list_peer_conversations surfaces.
	systemPreview    string
	firstUserPreview string

	ended        bool
	endedReason  EndReason
	endedAt      time.Time
}

// startConversation registers a new SDK session against the cached
// agent + environment, allocates a conversation slot, and returns
// the metadata three-tuple.  Refuses if --max-conversations is full.
func (a *sdkAgent) StartConversation(ctx context.Context, req StartReq) (ConvID, time.Time, Budget, error) {
	if err := a.ensureInit(ctx); err != nil {
		return "", time.Time{}, Budget{}, fmt.Errorf("ensureInit: %w", err)
	}

	project := a.resolveProject(req.Project)
	if project == "" && req.Project != "" {
		// req.Project was explicitly set but invalid.
		return "", time.Time{}, Budget{}, fmt.Errorf("%w: %q", errInvalidProject, req.Project)
	}

	a.convsMu.Lock()
	if a.cfg.MaxConversations > 0 && a.aliveCountLocked() >= a.cfg.MaxConversations {
		a.convsMu.Unlock()
		return "", time.Time{}, Budget{}, ErrConvCap
	}
	a.convsMu.Unlock()

	sess, err := a.client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: param.NewOpt(a.agentID)},
		EnvironmentID: a.envID,
	})
	if err != nil {
		return "", time.Time{}, Budget{}, fmt.Errorf("session create: %w", err)
	}

	now := time.Now()
	effective := req.Budget.Resolve(a.cfg.DefaultBudget)
	conv := &conversation{
		convID:         ConvID(sess.ID),
		sessionID:      sess.ID,
		startedAt:      now,
		lastActivityAt: now,
		perTurnBudget:  effective,
		project:        project,
		systemPreview:  truncateRunes(req.SystemPrompt, 140),
	}

	a.convsMu.Lock()
	if a.convs == nil {
		a.convs = make(map[ConvID]*conversation)
	}
	a.convs[conv.convID] = conv
	a.convsMu.Unlock()

	return conv.convID, now, effective, nil
}

// SendMessage drives one more turn of an existing conversation.
// Holds the conversation's own lock for the full turn so concurrent
// callers serialize.
func (a *sdkAgent) SendMessage(ctx context.Context, convID ConvID, prompt string, budget Budget) (Transcript, error) {
	conv, err := a.lookupAlive(convID)
	if err != nil {
		return Transcript{}, err
	}

	conv.mu.Lock()
	defer conv.mu.Unlock()

	// Re-check liveness inside the lock so a mid-turn reap+restart
	// race produces ErrConvReaped, not a confusing "session not found"
	// from the SDK.
	a.convsMu.Lock()
	live, ok := a.convs[convID]
	a.convsMu.Unlock()
	if !ok || live.ended {
		return Transcript{}, ErrConvReaped
	}

	turnBudget := budget.Resolve(conv.perTurnBudget)
	if turnBudget.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, turnBudget.Timeout)
		defer cancel()
	}

	conv.turnCount++
	if conv.firstUserPreview == "" {
		conv.firstUserPreview = truncateRunes(prompt, 140)
	}
	turnIndex := conv.turnCount

	start := time.Now()
	tx, invokes, _ := a.runTurn(ctx, conv, prompt, turnBudget)
	tx.ElapsedMs = time.Since(start).Milliseconds()
	tx.TurnCount = turnIndex // overall turn index, not per-call (the loop counts agent_messages within a turn)

	conv.lastActivityAt = time.Now()

	if a.cfg.Auditor != nil {
		_ = a.cfg.Auditor.Log(AuditEntry{
			Timestamp:     time.Now().UTC(),
			PeerName:      a.cfg.PeerName,
			ConvID:        string(convID),
			TurnIndex:     turnIndex,
			PromptBytes:   len(prompt),
			ResponseBytes: len(tx.Markdown),
			ToolCalls:     invokes,
			ElapsedMs:     tx.ElapsedMs,
			StopReason:    tx.StopReason,
			ErrorSummary:  tx.ErrorSummary,
		})
	}

	return tx, nil
}

// runTurn is the shared SDK round-trip used by both SendMessage and
// (the reused-from-PR-A) OneShot path.  Sends one user.message,
// drains events through runEventLoop, returns the assembled
// Transcript.
func (a *sdkAgent) runTurn(
	ctx context.Context,
	conv *conversation,
	prompt string,
	budget Budget,
) (Transcript, []AuditToolInvoke, error) {
	if _, err := a.client.Beta.Sessions.Events.Send(ctx, conv.sessionID, anthropic.BetaSessionEventSendParams{
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

	stream := a.client.Beta.Sessions.Events.StreamEvents(ctx, conv.sessionID, anthropic.BetaSessionEventStreamParams{})
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
			return handler(args, conv.project)
		},
		SendToolResult: func(toolUseID, content string, isError bool) error {
			_, err := a.client.Beta.Sessions.Events.Send(ctx, conv.sessionID, anthropic.BetaSessionEventSendParams{
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

	return runEventLoop(ctx, events, hooks, budget, allow)
}

// ListConversations returns the metadata view of every alive
// conversation, sorted by lastActivityAt desc.
func (a *sdkAgent) ListConversations() []ConvMeta {
	a.convsMu.Lock()
	defer a.convsMu.Unlock()

	out := make([]ConvMeta, 0, len(a.convs))
	for _, c := range a.convs {
		if c.ended {
			continue
		}
		preview := c.firstUserPreview
		if preview == "" {
			preview = c.systemPreview
		}
		out = append(out, ConvMeta{
			ConvID:         c.convID,
			StartedAt:      c.startedAt,
			LastActivityAt: c.lastActivityAt,
			TurnCount:      c.turnCount,
			Preview:        preview,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivityAt.After(out[j].LastActivityAt)
	})
	return out
}

// EndConversation terminates a conversation.  Idempotent: a second
// call with the same convID returns AlreadyEnded=true rather than an
// error.  Calls Beta.Sessions.Delete server-side so we don't leak
// a phantom session.
func (a *sdkAgent) EndConversation(ctx context.Context, convID ConvID, reason EndReason) (EndSummary, error) {
	a.convsMu.Lock()
	conv, ok := a.convs[convID]
	a.convsMu.Unlock()
	if !ok {
		return EndSummary{}, ErrUnknownConv
	}

	conv.mu.Lock()
	if conv.ended {
		summary := EndSummary{
			Ended:        true,
			AlreadyEnded: true,
			TotalTurns:   conv.turnCount,
			EndedReason:  conv.endedReason,
		}
		conv.mu.Unlock()
		return summary, nil
	}
	conv.ended = true
	conv.endedReason = reason
	conv.endedAt = time.Now()
	totalTurns := conv.turnCount
	sessionID := conv.sessionID
	conv.mu.Unlock()

	// Best-effort server-side delete.  Failure here doesn't roll
	// back local state — the operator probably wants the local map
	// to reflect intent even if the upstream call failed.
	_, _ = a.client.Beta.Sessions.Delete(ctx, sessionID, anthropic.BetaSessionDeleteParams{})

	return EndSummary{
		Ended:        true,
		AlreadyEnded: false,
		TotalTurns:   totalTurns,
		EndedReason:  reason,
	}, nil
}

// lookupAlive returns the conversation iff it exists and hasn't been
// ended.  Returns ErrUnknownConv on miss, ErrConvReaped if the conv
// was already terminated by the idle reaper.
func (a *sdkAgent) lookupAlive(convID ConvID) (*conversation, error) {
	a.convsMu.Lock()
	conv, ok := a.convs[convID]
	a.convsMu.Unlock()
	if !ok {
		return nil, ErrUnknownConv
	}
	conv.mu.Lock()
	ended := conv.ended
	endedReason := conv.endedReason
	conv.mu.Unlock()
	if ended {
		if endedReason == EndedByIdleReap {
			return nil, ErrConvReaped
		}
		return nil, ErrUnknownConv
	}
	return conv, nil
}

// aliveCountLocked counts non-ended conversations.  Caller MUST hold
// a.convsMu.
func (a *sdkAgent) aliveCountLocked() int {
	count := 0
	for _, c := range a.convs {
		if !c.ended {
			count++
		}
	}
	return count
}

// startReaper launches the background idle-reaper.  Closing
// a.reaperStop stops it; a.reaperDone is signaled when the goroutine
// has fully exited.
func (a *sdkAgent) startReaper() {
	if a.cfg.ConversationIdleTimeout <= 0 {
		return
	}
	a.reaperStop = make(chan struct{})
	a.reaperDone = make(chan struct{})
	go a.reapLoop()
}

func (a *sdkAgent) reapLoop() {
	defer close(a.reaperDone)

	// Tick at 1/4 the idle timeout so we don't reap N seconds late;
	// floor at 1s so very short configured timeouts don't spin.
	interval := a.cfg.ConversationIdleTimeout / 4
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-a.reaperStop:
			return
		case <-ticker.C:
			a.reapStaleConversations()
		}
	}
}

// reapStaleConversations marks any conversation whose lastActivityAt
// is older than --conversation-idle-timeout as ended (with reason
// "idle_timeout") and best-effort-deletes the upstream session.
func (a *sdkAgent) reapStaleConversations() {
	cutoff := time.Now().Add(-a.cfg.ConversationIdleTimeout)
	var toDelete []string

	a.convsMu.Lock()
	for _, conv := range a.convs {
		conv.mu.Lock()
		if !conv.ended && conv.lastActivityAt.Before(cutoff) {
			conv.ended = true
			conv.endedReason = EndedByIdleReap
			conv.endedAt = time.Now()
			toDelete = append(toDelete, conv.sessionID)
		}
		conv.mu.Unlock()
	}
	a.convsMu.Unlock()

	for _, sessionID := range toDelete {
		_, _ = a.client.Beta.Sessions.Delete(context.Background(), sessionID, anthropic.BetaSessionDeleteParams{})
	}
}

// stopReaper signals the reaper goroutine to exit and waits for it.
// Safe to call multiple times.
func (a *sdkAgent) stopReaper() {
	if a.reaperStop == nil {
		return
	}
	select {
	case <-a.reaperStop:
		// Already closed.
	default:
		close(a.reaperStop)
	}
	if a.reaperDone != nil {
		<-a.reaperDone
	}
}

// truncateRunes returns the first n runes of s as a new string.
// UTF-8 safe — it iterates by rune so a multi-byte codepoint at the
// boundary doesn't yield invalid UTF-8 in the output.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// errInvalidProject is the typed sentinel the StartConversation /
// OneShot paths return when the caller passes an invalid project arg.
// Wrapped with %w in the actual error so callers can errors.Is it.
var errInvalidProject = errors.New("invalid project")
