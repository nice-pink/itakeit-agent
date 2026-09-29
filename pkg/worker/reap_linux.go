package worker

import (
	"os"
	"syscall"
)

// reapOrphans kills and reaps the agent's children that os/exec did not
// start: processes a tool session left behind, reparented to the agent because
// it is PID 1 in the image or a subreaper (cmd/itakeit-agent sets
// PR_SET_CHILD_SUBREAPER). Each is waited for by PID, never with Wait4(-1),
// which would take exit statuses os/exec is waiting for. A tracked child
// cannot be reaped by anyone else, so its PID cannot be reused under us.
//
// The wait runs outside the lock, on its own goroutine: a process stuck in
// uninterruptible sleep (a hung mount) survives SIGKILL, and waiting for it
// under the lock would stop every later CLI start. Until it is reaped its PID
// cannot be reused, so the late wait stays safe.
func reapOrphans() {
	tracked.Lock()
	defer tracked.Unlock()
	me := os.Getpid()
	for pid, parent := range parents() {
		if parent != me || tracked.pids[pid] || reaping[pid] {
			continue
		}
		syscall.Kill(pid, syscall.SIGKILL)
		reaping[pid] = true
		go func() {
			var ws syscall.WaitStatus
			syscall.Wait4(pid, &ws, 0, nil)
			tracked.Lock()
			delete(reaping, pid)
			tracked.Unlock()
		}()
	}
}

// reaping are orphans killed and being waited for, guarded by tracked.
var reaping = map[int]bool{}
