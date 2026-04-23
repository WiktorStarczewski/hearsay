// Package tailscale discovers the Tailscale identity of the current host
// by shelling out to `tailscale ip -4` and `tailscale status --json`. If
// the CLI isn't installed or can't resolve (common on Mac App Store
// Tailscale where the CLI isn't on PATH by default), detection functions
// return "" and callers fall back to explicit flags.
package tailscale

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var ipv4Re = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`)

// DetectIPv4 runs `tailscale ip -4` with a short timeout and returns the
// first IPv4 address printed. Returns "" when Tailscale is unavailable.
func DetectIPv4() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tailscale", "ip", "-4")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if ipv4Re.MatchString(line) {
			return line
		}
	}
	return ""
}

// SelfDNSName returns the current host's MagicDNS name (e.g.
// "ivan-mac.tail1234.ts.net") from `tailscale status --json`. The
// trailing dot in the raw DNSName field is stripped. Returns "" when
// Tailscale is unavailable or the daemon is disconnected.
func SelfDNSName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tailscale", "status", "--json")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var status struct {
		Self struct {
			DNSName  string `json:"DNSName"`
			HostName string `json:"HostName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return ""
	}
	if dns := strings.TrimSuffix(status.Self.DNSName, "."); dns != "" {
		return dns
	}
	return status.Self.HostName
}
