//go:build !unix

package worker

import (
	"os"
	"os/exec"
	"syscall"
)

func ownGroup(*exec.Cmd) {}

// killTree kills only the CLI: there is no process group to reach further.
func killTree(pid int, _ syscall.Signal) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}
