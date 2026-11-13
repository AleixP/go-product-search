package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

const wantAllInterfacesWarning = "listening on all interfaces; the API has no authentication"

func TestRun_WarnsWhenListeningOnAllInterfaces(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		wantWarn bool
		needsV6  bool
	}{
		{name: "ipv4 loopback", addr: "127.0.0.1:0"},
		{name: "localhost", addr: "localhost:0"},
		{name: "ipv6 loopback", addr: "[::1]:0", needsV6: true},
		{name: "empty host", addr: ":0", wantWarn: true},
		{name: "ipv4 unspecified", addr: "0.0.0.0:0", wantWarn: true},
		{name: "ipv6 unspecified", addr: "[::]:0", wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.needsV6 {
				l, err := net.Listen("tcp", tt.addr)
				if err != nil {
					t.Skipf("no IPv6 loopback: %v", err)
				}
				_ = l.Close()
			}
			_, cancel, done, out := startRun(t, []string{"--addr", tt.addr}, fakeSearcher{})
			cancel()
			if code := waitCode(t, done, 5*time.Second); code != 0 {
				t.Fatalf("code = %d, want 0; stderr: %s", code, out.String())
			}
			logs := out.String()
			hasWarn := strings.Contains(logs, "level=WARN")
			if hasWarn != tt.wantWarn {
				t.Fatalf("WARN present = %v, want %v; stderr: %s", hasWarn, tt.wantWarn, logs)
			}
			if strings.Contains(logs, "level=ERROR") {
				t.Errorf("unexpected ERROR line; stderr: %s", logs)
			}
			if !tt.wantWarn {
				return
			}
			warnAt := strings.Index(logs, wantAllInterfacesWarning)
			if warnAt < 0 || !strings.Contains(lineAt(logs, warnAt), "level=WARN") {
				t.Fatalf("no WARN with %q; stderr: %s", wantAllInterfacesWarning, logs)
			}
			if listenAt := strings.Index(logs, "msg=listening"); warnAt > listenAt {
				t.Errorf("WARN comes after the listening line; stderr: %s", logs)
			}
		})
	}
}

// lineAt returns the log line that contains offset i.
func lineAt(s string, i int) string {
	start := strings.LastIndex(s[:i], "\n") + 1
	end := strings.Index(s[i:], "\n")
	if end < 0 {
		return s[start:]
	}
	return s[start : i+end]
}

type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

func TestBindsAllInterfaces(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{":8080", true},
		{"0.0.0.0:8080", true},
		{"[::]:8080", true},
		{"[::%en0]:8080", true},
		{"127.0.0.1:8080", false},
		{"[::1]:8080", false},
		{"10.0.0.5:8080", false},
		{"[2001:db8::1]:8080", false},
		{"localhost:8080", false},
		{"no-port", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := bindsAllInterfaces(stringAddr(tt.addr)); got != tt.want {
			t.Errorf("bindsAllInterfaces(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
