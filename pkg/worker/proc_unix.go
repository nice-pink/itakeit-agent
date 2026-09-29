//go:build unix

package worker

import (
	"os/exec"
	"syscall"
)

// ownGroup starts the CLI in its own process group, so killTree can signal the
// group without reaching the agent.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killTree signals the CLI, its process group and every descendant. The group
// alone is not enough: the CLI starts its Bash tool's shell detached, in a
// group of its own, so a running command would outlive a killed session.
// Callers send SIGTERM; exec's WaitDelay then sends SIGKILL to the CLI itself,
// and whatever outlives it is reaped by reapOrphans. A second signal to the
// descendants after a delay could hit a reused PID, so there is none.
func killTree(pid int, sig syscall.Signal) {
	for _, p := range descendants(pid) {
		syscall.Kill(p, sig)
	}
	syscall.Kill(-pid, sig)
	syscall.Kill(pid, sig)
}

// descendants lists every process below pid, children first.
func descendants(pid int) []int {
	children := map[int][]int{}
	for child, parent := range parents() {
		children[parent] = append(children[parent], child)
	}
	var out []int
	for queue := children[pid]; len(queue) > 0; queue = queue[1:] {
		out = append(out, queue[0])
		queue = append(queue, children[queue[0]]...)
	}
	return out
}
