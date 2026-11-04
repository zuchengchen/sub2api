//go:build linux

package main

import (
	"fmt"
	"regexp"
	"strings"
)

var straceLinePrefix = regexp.MustCompile(`^(?:([0-9]+)\s+|\[pid\s+([0-9]+)\]\s+)?(.*)$`)
var straceSockaddr = regexp.MustCompile(`^connect\(-?[0-9]+,\s*(.*),\s*[0-9]+\s*\)\s+=\s+.+$`)

// Reassemble strace's per-PID unfinished/resumed records before inspecting the
// sockaddr. Unmatched continuations or unfinished calls are incomplete evidence.
// Quoted text in another syscall or an AF_UNIX path is never treated as a call.
func traceConnectCalls(data string) ([]string, []string) {
	pending := map[string]string{}
	var calls, failures []string
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		match := straceLinePrefix.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		pid := match[1]
		if pid == "" {
			pid = match[2]
		}
		body := match[3]
		if strings.HasPrefix(body, "<... connect resumed>") {
			prefix, ok := pending[pid]
			if !ok {
				failures = append(failures, "strace connect resumed without start")
				continue
			}
			delete(pending, pid)
			calls = append(calls, prefix+strings.TrimPrefix(body, "<... connect resumed>"))
		} else if strings.HasPrefix(body, "connect(") {
			if _, ok := pending[pid]; ok {
				failures = append(failures, "strace connect start before previous call resumed")
			}
			if strings.HasSuffix(body, "<unfinished ...>") {
				pending[pid] = strings.TrimSuffix(body, "<unfinished ...>")
			} else {
				calls = append(calls, body)
			}
		}
	}
	for range pending {
		failures = append(failures, "strace ends with unfinished connect call")
	}
	return calls, failures
}

func traceAddress(call string) (string, error) {
	match := straceSockaddr.FindStringSubmatch(call)
	if match == nil {
		return "", fmt.Errorf("strace connect is unparseable: %s", call)
	}
	return match[1], nil
}
