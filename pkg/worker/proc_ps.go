//go:build unix && !linux

package worker

import (
	"os/exec"
	"strconv"
	"strings"
)

// parents maps every PID to its parent, from ps (macOS and the BSDs have no
// /proc).
func parents() map[int]int {
	out := map[int]int{}
	raw, _ := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			out[pid] = ppid
		}
	}
	return out
}
