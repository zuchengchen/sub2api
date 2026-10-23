package service

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// singleflightFollowers counts goroutines running callerFrame (for example
// "(*OllamaCloudUsageService).refreshAccount") that have joined an in-flight
// singleflight.Group.Do call as duplicates.
//
// singleflight.Group exposes no "a duplicate is waiting" signal, and the
// observable side effects of the usage refresh paths (anchor GetByID, group
// key) all happen before the caller enters Group.Do. A follower is only
// provably joined once it is parked in the WaitGroup.Wait inside Group.Do,
// which singleflight reaches strictly after registering the duplicate under
// its mutex. From there it is guaranteed to receive the leader's result no
// matter how soon the leader is released. Leaders block elsewhere (in the
// upstream stub), so they never match.
//
// The match depends on golang.org/x/sync/singleflight frame names. If they
// change, waitForSingleflightFollowers times out with an explicit failure
// instead of silently passing.
func singleflightFollowers(callerFrame string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	followers := 0
	for _, goroutine := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(goroutine, callerFrame) {
			continue
		}
		var frames []string
		for _, line := range strings.Split(goroutine, "\n") {
			if line != "" && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "goroutine ") {
				frames = append(frames, line)
			}
		}
		for i := 0; i+1 < len(frames); i++ {
			if strings.HasPrefix(frames[i], "sync.(*WaitGroup).Wait(") &&
				strings.HasPrefix(frames[i+1], "golang.org/x/sync/singleflight.(*Group).Do(") {
				followers++
				break
			}
		}
	}
	return followers
}

// waitForSingleflightFollowers blocks until want callers running callerFrame
// are parked as duplicates inside a singleflight group.
func waitForSingleflightFollowers(t *testing.T, callerFrame string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for singleflightFollowers(callerFrame) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d caller(s) of %s to join the in-flight singleflight call; got %d",
				want, callerFrame, singleflightFollowers(callerFrame))
		}
		time.Sleep(time.Millisecond)
	}
}
