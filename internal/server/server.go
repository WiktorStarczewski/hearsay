// Package server wires an mcp.Server behind a net/http.Server with a
// bearer-token middleware in front. This is the full HTTP surface for a
// running hearsay instance: POST /mcp for MCP traffic, GET /health for
// tunnel/reverse-proxy probes.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/WiktorStarczewski/hearsay/internal/agent"
	"github.com/WiktorStarczewski/hearsay/internal/tools"
)

type Options struct {
	Port        int
	Bind        string
	Token       string
	PeerName    string
	PeerVersion string
	DataDir     string
	LiveWindow  time.Duration
	Quiet       bool

	// Agent is non-nil iff `--enable-agent` was set.  When nil, the
	// Phase-2 agent tools (ask_peer_claude, etc.) are not registered
	// and the existing 8 read-only tools are the entire surface.
	Agent agent.Agent
}

// Instance is a running hearsay server; Shutdown() gracefully stops it.
type Instance struct {
	http       *http.Server
	listener   net.Listener
	mcpServer  *mcp.Server
}

// Start opens a TCP listener and begins serving. Returns immediately on
// listen error (most importantly EADDRINUSE, which main.go translates
// into a friendly message).
func Start(opts Options) (*Instance, error) {
	mcpSrv := buildMCP(opts)

	// The SDK wants a getServer function so it can build per-request
	// server state. We use a singleton — every request reuses the same
	// tool registry.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpSrv
	}, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"name":    opts.PeerName,
			"version": opts.PeerVersion,
		})
	})
	mux.Handle("/mcp", authMiddleware(opts.Token, handler))
	mux.Handle("/mcp/", authMiddleware(opts.Token, handler))

	addr := fmt.Sprintf("%s:%d", opts.Bind, opts.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// A serve loop error after the listener is already bound
			// is usually terminal; log to stderr so the operator sees it.
			fmt.Fprintf(os.Stderr, "hearsay: serve error: %v\n", err)
		}
	}()

	return &Instance{http: srv, listener: listener, mcpServer: mcpSrv}, nil
}

// Shutdown drains in-flight requests with a short grace period.
func (i *Instance) Shutdown(ctx context.Context) error {
	return i.http.Shutdown(ctx)
}

func buildMCP(opts Options) *mcp.Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "hearsay-" + opts.PeerName,
		Version: opts.PeerVersion,
	}, nil)

	var logFn func(string, string, time.Duration)
	if !opts.Quiet {
		logFn = func(name, status string, dur time.Duration) {
			fmt.Fprintf(os.Stderr, "[%s] tool=%s status=%s duration=%dms\n",
				time.Now().UTC().Format(time.RFC3339Nano), name, status, dur.Milliseconds())
		}
	}

	tools.Register(mcpSrv, tools.Context{
		PeerName:    opts.PeerName,
		PeerVersion: opts.PeerVersion,
		DataDir:     opts.DataDir,
		LiveWindow:  opts.LiveWindow,
		Log:         logFn,
		Agent:       opts.Agent,
	})
	return mcpSrv
}

// authMiddleware rejects anything that doesn't carry a matching Bearer
// token before forwarding to the MCP handler.
func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !(len(auth) > len(prefix) && auth[:len(prefix)] == prefix) {
			writeUnauthorized(w)
			return
		}
		if auth[len(prefix):] != token {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "unauthorized",
		"message": "missing or invalid Bearer token",
	})
}
