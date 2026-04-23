package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startForTest brings up a full hearsay server on an ephemeral port,
// returns its base URL and a token the test can use. It also returns a
// teardown func that gracefully drains in-flight requests.
func startForTest(t *testing.T) (baseURL, token string, shutdown func()) {
	t.Helper()
	// Build a minimal fake ~/.claude/projects tree so tool handlers have
	// something to see.
	dataDir := t.TempDir()
	projects := filepath.Join(dataDir, "projects", "-tmp-fake")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	session := `{"type":"user","uuid":"u1","timestamp":"2026-04-24T10:00:00Z","sessionId":"abcd","message":{"role":"user","content":"hello"}}`
	if err := os.WriteFile(filepath.Join(projects, "abcd.jsonl"), []byte(session+"\n"), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	// Grab an ephemeral port by listening then immediately closing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	token = "test-token-0123456789"
	inst, err := Start(Options{
		Port:        port,
		Bind:        "127.0.0.1",
		Token:       token,
		PeerName:    "tester",
		PeerVersion: "v0.0.1",
		DataDir:     dataDir,
		LiveWindow:  30 * time.Second,
		Quiet:       true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the listener to be reachable (goroutine race on Serve).
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	return base, token, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = inst.Shutdown(ctx)
	}
}

func TestHealth_OKNoAuthRequired(t *testing.T) {
	base, _, shutdown := startForTest(t)
	defer shutdown()

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["name"] != "tester" {
		t.Errorf("health name = %v", out["name"])
	}
}

func TestMCP_Rejects401WithoutBearer(t *testing.T) {
	base, _, shutdown := startForTest(t)
	defer shutdown()

	resp, err := http.Post(base+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestMCP_Rejects401WithWrongScheme(t *testing.T) {
	base, _, shutdown := startForTest(t)
	defer shutdown()

	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Basic dGVzdDp0ZXN0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for Basic auth", resp.StatusCode)
	}
}

func TestMCP_Rejects401WithWrongToken(t *testing.T) {
	base, _, shutdown := startForTest(t)
	defer shutdown()

	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer some-other-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for wrong token", resp.StatusCode)
	}
}

func TestMCP_InitializeAndCallPeerInfo(t *testing.T) {
	base, token, shutdown := startForTest(t)
	defer shutdown()

	u, err := url.Parse(base + "/mcp")
	if err != nil {
		t.Fatal(err)
	}

	// Build an MCP client against the real HTTP transport.
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: u.String(),
		HTTPClient: &http.Client{
			Transport: &bearerTransport{token: token, next: http.DefaultTransport},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_peer_info", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call get_peer_info: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError=true; content=%+v", res.Content)
	}
	var info map[string]any
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if info["name"] != "tester" {
		t.Errorf("name=%v, want tester", info["name"])
	}
}

func TestHealth_UnknownRouteIs404(t *testing.T) {
	base, _, shutdown := startForTest(t)
	defer shutdown()

	resp, err := http.Get(base + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStart_PortInUseErrors(t *testing.T) {
	// Bind one listener, then ask Start for the same port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	_, err = Start(Options{
		Port:        port,
		Bind:        "127.0.0.1",
		Token:       "x",
		PeerName:    "x",
		PeerVersion: "0",
		DataDir:     t.TempDir(),
		LiveWindow:  time.Second,
		Quiet:       true,
	})
	if err == nil {
		t.Fatalf("expected EADDRINUSE-style error")
	}
}

// bearerTransport is a tiny http.RoundTripper that stamps Bearer auth on
// every outgoing request — enough to exercise the server's auth path
// without building a full MCP middleware stack.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(req)
}