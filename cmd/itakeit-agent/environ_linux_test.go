package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// After hideEnviron a child of the same user cannot read this process's
// environment, where the agent keeps its Slack tokens, but its own still works.
func TestHideEnviron(t *testing.T) {
	// /proc/<pid>/environ is the environment the process started with, so the
	// check looks for HOME there, which every run of this test has.
	if os.Geteuid() == 0 {
		t.Skip("root reads every environ; the agent runs as a normal user")
	}
	path := "/proc/" + strconv.Itoa(os.Getpid()) + "/environ"
	if out, err := exec.Command("cat", path).Output(); err != nil || !strings.Contains(string(out), "HOME=") {
		t.Skipf("environ not readable even before: %v", err)
	}
	if err := hideEnviron(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cat", path).CombinedOutput(); err == nil || strings.Contains(string(out), "HOME=") {
		t.Fatalf("child read the environment: %q, %v", out, err)
	}
	t.Setenv("AGENT_SLACK_BOT_TOKEN", "xoxb-test")
	if out, err := exec.Command("sh", "-c", `echo "$AGENT_SLACK_BOT_TOKEN"`).Output(); err != nil || strings.TrimSpace(string(out)) != "xoxb-test" {
		t.Fatalf("child env broken: %q, %v", out, err)
	}
}
