// Package tailscale discovers the Tailscale IPv4 address of the current
// host by shelling out to `tailscale ip -4`. If the CLI isn't installed
// or can't resolve (common on Mac App Store Tailscale where the CLI
// isn't on PATH by default), this returns "" and callers fall back.
package tailscale

import (
	"context"
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
