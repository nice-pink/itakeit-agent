package worker

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A CLI that exits leaving a child behind: the child reparents to the agent
// (a subreaper here, PID 1 in the image) and is killed and reaped, so neither
// it nor a zombie stays.
func TestReapsOrphans(t *testing.T) {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, 0x24, 1, 0); errno != 0 { // PR_SET_CHILD_SUBREAPER
		t.Skipf("cannot become a subreaper: %v", errno)
	}
	w, _, _, _ := fakeStream(t, nil, "in", initLine, "child", resultLine)
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(os.Getenv("FAKE_CHILD"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.Fields(string(raw))[0])
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); os.IsNotExist(err) {
			return // gone, and reaped: a zombie would still have its /proc entry
		}
		if time.Now().After(deadline) {
			stat, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d still there: %s", pid, stat)
		}
	}
}
