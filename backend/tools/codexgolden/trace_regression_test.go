//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceConnectWhitespaceAndResumption(t *testing.T) {
	ipv4 := `{sa_family=AF_INET, sin_port=htons(65535), sin_addr=inet_addr("127.0.0.1")}`
	ipv6 := `{sa_family=AF_INET6, sin6_port=htons(65535), sin6_flowinfo=htonl(0), inet_pton(AF_INET6, "::1", &sin6_addr), sin6_scope_id=0}`
	local := "91 connect(7, " + ipv4 + ", 16) = 0\n"
	for _, tc := range []struct {
		name, trace string
		allowed     bool
	}{
		{"padded IPv4", "91 connect(7, " + ipv4 + ", 16 ) = 0\n", true},
		{"padded IPv6", "91 connect(7, " + ipv6 + ", 28\t) = 0\n", true},
		{"resumed after length", "91 connect(7, " + ipv4 + ", 16 <unfinished ...>\n91 <... connect resumed> ) = 0\n", true},
		{"resumed before address", "91 connect(7, <unfinished ...>\n91 <... connect resumed> " + ipv6 + ", 28 ) = -1 EINPROGRESS (Operation now in progress)\n", true},
		{"external padded", "91 connect(7, " + strings.Replace(ipv4, "127.0.0.1", "203.0.113.9", 1) + ", 16 ) = 0\n", false},
		{"unfinished evidence", local + "91 connect(8, " + ipv4 + ", 16 <unfinished ...>\n", false},
		{"invalid length suffix", "91 connect(7, " + ipv4 + ", 16 garbage) = 0\n", false},
		{"quoted source collision", local + "91 write(2, \"connect(7, {sa_family=AF_INET}, 16 ) = 0\", 48) = 48\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "connect.strace")
			if err := os.WriteFile(path, []byte(tc.trace), 0600); err != nil {
				t.Fatal(err)
			}
			failures := checkTrace(path)
			if (len(failures) == 0) != tc.allowed {
				t.Fatalf("allowed=%v; trace validation=%v", tc.allowed, failures)
			}
		})
	}
}
