package worker

import (
	"os"
	"strconv"
	"strings"
)

// parents maps every visible PID to its parent, from /proc/<pid>/stat. The
// command name in field 2 may hold spaces and parentheses, so fields are read
// after its last ')'.
func parents() map[int]int {
	out := map[int]int{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(raw)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) > 1 {
			if ppid, err := strconv.Atoi(fields[1]); err == nil {
				out[pid] = ppid
			}
		}
	}
	return out
}
