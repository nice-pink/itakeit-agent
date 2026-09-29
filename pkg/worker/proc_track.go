package worker

import (
	"os/exec"
	"sync"
)

// tracked are the children os/exec started and has not reaped yet. The
// reaper must never take one of them: that would steal its exit status.
// Start runs under the same lock, so a child is registered before the reaper
// can see it.
var tracked = struct {
	sync.Mutex
	pids map[int]bool
}{pids: map[int]bool{}}

func startTracked(cmd *exec.Cmd) error {
	tracked.Lock()
	defer tracked.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	tracked.pids[cmd.Process.Pid] = true
	return nil
}

// untrack runs after cmd.Wait has reaped the child.
func untrack(cmd *exec.Cmd) {
	tracked.Lock()
	defer tracked.Unlock()
	delete(tracked.pids, cmd.Process.Pid)
}
