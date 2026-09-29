//go:build unix

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixInit = `out {"type":"system","subtype":"init","permissionMode":"default","tools":["Bash","Edit","StructuredOutput"]}`

func fixTools(timeout time.Duration) *Tools {
	return &Tools{Mode: ModeFix, Approval: true, WorkTimeout: timeout,
		Read:  []Entry{{Tool: "Bash", Prefix: "kubectl get"}},
		Write: []Entry{{Tool: "Bash", Prefix: "kubectl rollout restart"}, {Tool: "Edit"}}}
}

// approvals records what the approver was asked and answers with answer.
type approvals struct {
	mu     sync.Mutex
	asked  []ApprovalRequest
	answer func(ctx context.Context, r ApprovalRequest) (Decision, error)
}

func (a *approvals) approve(ctx context.Context, r ApprovalRequest) (Decision, error) {
	a.mu.Lock()
	a.asked = append(a.asked, r)
	a.mu.Unlock()
	return a.answer(ctx, r)
}

func allowAs(user string) func(context.Context, ApprovalRequest) (Decision, error) {
	return func(context.Context, ApprovalRequest) (Decision, error) { return Decision{Allow: true, By: user}, nil }
}

func TestDecideFix(t *testing.T) {
	dir := t.TempDir()
	tools := fixTools(time.Minute)
	for input, want := range map[string]Verdict{
		`{"command":"kubectl get pods"}`:                                   Allow,
		`{"command":"kubectl rollout restart deploy/x"}`:                   Ask,
		`{"command":"kubectl rollout restart x; rm -rf /"}`:                Deny,
		`{"command":"kubectl delete pod x"}`:                               Deny,
		`{"command":"kubectl rollout restart x","run_in_background":true}`: Deny,
	} {
		if v, reason := tools.decide("Bash", json.RawMessage(input), dir); v != want {
			t.Errorf("%s: %v %q, want %v", input, v, reason, want)
		}
	}
	if v, _ := tools.decide("Edit", json.RawMessage(`{"file_path":"x.yaml","old_string":"a","new_string":"b"}`), dir); v != Ask {
		t.Errorf("Edit in the task dir: %v, want Ask", v)
	}
	if v, _ := tools.decide("Edit", json.RawMessage(`{"file_path":"/etc/hosts"}`), dir); v != Deny {
		t.Errorf("Edit outside: %v, want Deny", v)
	}
	plain := &Tools{Mode: ModeFix, Write: []Entry{{Tool: "Bash"}}}
	if v, _ := plain.decide("Bash", json.RawMessage(`{"command":"kubectl get x | head"}`), dir); v != Ask {
		t.Errorf("plain Bash write with a pipe: %v, want Ask (the approver sees it)", v)
	}
	propose := fixTools(time.Minute)
	propose.Mode = ModePropose
	if v, _ := propose.decide("Bash", json.RawMessage(`{"command":"kubectl rollout restart x"}`), dir); v != Deny {
		t.Errorf("a write entry in propose mode: %v, want Deny", v)
	}
	if got := propose.exposed(); len(got) != 1 || got[0] != "Bash" {
		t.Errorf("propose exposes %v, want only the read tools", got)
	}
}

// A write entry wins over a read entry that also covers the call: Ask in fix
// mode, Deny in propose mode. Config refuses the overlap; decide holds anyway.
func TestDecideWriteWins(t *testing.T) {
	dir := t.TempDir()
	tools := &Tools{Mode: ModeFix, Read: []Entry{{Tool: "Bash", Prefix: "kubectl"}}, Write: []Entry{{Tool: "Bash", Prefix: "kubectl rollout restart"}}}
	in := json.RawMessage(`{"command":"kubectl rollout restart deploy/api"}`)
	if v, _ := tools.decide("Bash", in, dir); v != Ask {
		t.Fatalf("fix: %v, want Ask", v)
	}
	tools.Mode = ModePropose
	if v, _ := tools.decide("Bash", in, dir); v != Deny {
		t.Fatalf("propose: %v, want Deny", v)
	}
	if v, _ := tools.decide("Bash", json.RawMessage(`{"command":"kubectl get pods"}`), dir); v != Allow {
		t.Fatalf("propose read: %v, want Allow", v)
	}
}

func TestCovers(t *testing.T) {
	e := func(s string) Entry {
		x, err := ParseEntry(s, true)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	for _, c := range []struct {
		read, write string
		want        bool
	}{
		{"Bash(kubectl *)", "Bash(kubectl rollout restart *)", true},
		{"Bash(kubectl rollout *)", "Bash(kubectl rollout restart *)", true},
		{"Bash(kubectl rollout restart deploy/api)", "Bash(kubectl rollout restart *)", true},
		{"Bash(kubectl get *)", "Bash", true},
		{"Bash(kubectl get *)", "Bash(kubectl rollout restart *)", false},
		{"Bash(kubectl rollout restartx *)", "Bash(kubectl rollout restart *)", false},
		{"Bash(kubectl rollout restart *)", "Bash(kubectl rollout restart deploy/api)", true},
		{"Read", "Edit", false},
		{"Bash(kubectl rollout *)", "Bash(kubectl  rollout restart *)", true}, // a doubled space in the config
	} {
		if got := Covers(e(c.read), e(c.write)); got != c.want {
			t.Errorf("Covers(%s, %s) = %v, want %v", c.read, c.write, got, c.want)
		}
	}
}

func TestNeedsApprover(t *testing.T) {
	for in, want := range map[string]bool{"Bash": true, "Bash(sed -i *)": true, "Bash(sh *)": true, "Bash(X=1 kubectl *)": true,
		"Bash(kubectl rollout restart *)": false, "Edit": false, "Write": false} {
		e, err := ParseEntry(in, true)
		if err != nil || e.NeedsApprover() != want {
			t.Errorf("%s: %v, %v, want %v", in, e.NeedsApprover(), err, want)
		}
	}
}

func TestStreamApprovedWrite(t *testing.T) {
	ap := &approvals{answer: allowAs("UAPP")}
	w, _, stdin, _ := fakeStream(t, fixTools(10*time.Second), "in", fixInit,
		toolUse("t1", "Bash"), request("r1", "t1", "Bash", `{"command":"kubectl rollout restart deploy/api","description":"restart it"}`), "in",
		toolResult("t1", false, "restarted"), resultLine)
	res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve})
	if err != nil {
		t.Fatal(err)
	}
	if len(ap.asked) != 1 || ap.asked[0].Seq != 1 || ap.asked[0].Display != "kubectl rollout restart deploy/api" || ap.asked[0].Redacted {
		t.Fatalf("asked %+v", ap.asked)
	}
	if id, b, in := response(stdin()[1]); id != "r1" || b != "allow" || in.(map[string]any)["command"] != "kubectl rollout restart deploy/api" {
		t.Fatalf("answer = %v", stdin()[1])
	}
	want := Action{Tool: "Bash", Input: "kubectl rollout restart deploy/api", By: "UAPP", Outcome: "ran"}
	if len(res.Actions) != 1 || res.Actions[0] != want {
		t.Fatalf("actions = %+v", res.Actions)
	}
}

func TestStreamDeniedWrites(t *testing.T) {
	cases := map[string]struct {
		approve Approve
		input   string
		reason  string
		asked   int
	}{
		"denied": {(&approvals{answer: func(context.Context, ApprovalRequest) (Decision, error) {
			return Decision{Reason: "denied by the approver"}, nil
		}}).approve, `{"command":"kubectl rollout restart x"}`, "denied by the approver", 1},
		"error":     {(&approvals{answer: func(context.Context, ApprovalRequest) (Decision, error) { return Decision{}, errors.New("slack down") }}).approve, `{"command":"kubectl rollout restart x"}`, "slack down", 1},
		"nil":       {nil, `{"command":"kubectl rollout restart x"}`, "not possible", 0},
		"too large": {(&approvals{answer: allowAs("UAPP")}).approve, `{"command":"kubectl rollout restart ` + strings.Repeat("x", maxApprovalInput) + `"}`, "too large", 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w, _, stdin, _ := fakeStream(t, fixTools(10*time.Second), "in", fixInit,
				toolUse("t1", "Bash"), request("r1", "t1", "Bash", c.input), "in", toolResult("t1", true, "denied"), resultLine)
			res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: c.approve})
			if err != nil || len(res.Actions) != 0 {
				t.Fatalf("res = %+v, err = %v", res, err)
			}
			r, _ := stdin()[1]["response"].(map[string]any)
			d, _ := r["response"].(map[string]any)
			if d["behavior"] != "deny" || !strings.Contains(d["message"].(string), c.reason) {
				t.Fatalf("answer = %v, want deny %q", d, c.reason)
			}
		})
	}
}

func TestStreamWriteCap(t *testing.T) {
	script := []string{"in", fixInit}
	for i := range MaxWrites + 1 {
		id := string(rune('a' + i))
		script = append(script, toolUse(id, "Bash"), request("r"+id, id, "Bash", `{"command":"kubectl rollout restart x"}`), "in", toolResult(id, false, "ok"))
	}
	ap := &approvals{answer: allowAs("UAPP")}
	w, _, stdin, _ := fakeStream(t, fixTools(10*time.Second), append(script, resultLine)...)
	res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve})
	if err != nil || len(ap.asked) != MaxWrites || len(res.Actions) != MaxWrites || !res.CapReached {
		t.Fatalf("asked %d, actions %d, cap %v, err %v", len(ap.asked), len(res.Actions), res.CapReached, err)
	}
	if _, b, _ := response(stdin()[MaxWrites+1]); b != "deny" {
		t.Fatalf("the write over the cap was answered %q", b)
	}
}

// Two writes wait at once: the first approval returns only after the second
// was asked, which would never happen if requests were handled one at a time.
// Each request gets exactly its own answer.
func TestStreamParallelApprovals(t *testing.T) {
	second := make(chan struct{})
	ap := &approvals{answer: func(ctx context.Context, r ApprovalRequest) (Decision, error) {
		if strings.HasSuffix(r.Display, " a") { // the first request, whichever Seq it got
			<-second
			return Decision{Reason: "denied by the approver"}, nil
		}
		defer close(second)
		return Decision{Allow: true, By: "UAPP"}, nil
	}}
	w, _, stdin, _ := fakeStream(t, fixTools(10*time.Second), "in", fixInit,
		toolUse("t1", "Bash"), toolUse("t2", "Bash"),
		request("r1", "t1", "Bash", `{"command":"kubectl rollout restart a"}`),
		request("r2", "t2", "Bash", `{"command":"kubectl rollout restart b"}`),
		"in", "in", toolResult("t2", false, "ok"), toolResult("t1", true, "denied"), resultLine)
	res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve})
	if err != nil {
		t.Fatal(err)
	}
	lines := stdin()
	got := map[string]string{}
	for _, l := range lines[1:] {
		id, b, _ := response(l)
		if _, dup := got[id]; dup {
			t.Fatalf("%s answered twice", id)
		}
		got[id] = b
	}
	if got["r1"] != "deny" || got["r2"] != "allow" || len(res.Actions) != 1 || res.Actions[0].Input != "kubectl rollout restart b" {
		t.Fatalf("answers %v, actions %+v", got, res.Actions)
	}
}

// A slow approver does not use up the round's working time.
func TestStreamClockPausesForApproval(t *testing.T) {
	ap := &approvals{answer: func(context.Context, ApprovalRequest) (Decision, error) {
		time.Sleep(1500 * time.Millisecond)
		return Decision{Allow: true, By: "UAPP"}, nil
	}}
	w, _, _, _ := fakeStream(t, fixTools(time.Second), "in", fixInit,
		toolUse("t1", "Bash"), request("r1", "t1", "Bash", `{"command":"kubectl rollout restart x"}`), "in", toolResult("t1", false, "ok"), resultLine)
	if res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve}); err != nil || len(res.Actions) != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	w, _, _, _ = fakeStream(t, fixTools(time.Second), "in", fixInit, "sleep 2", resultLine)
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

// An approval wait ends when the CLI moves on without the answer, and when the
// CLI exits.
func TestStreamApprovalWaitEnds(t *testing.T) {
	for name, tail := range map[string][]string{
		"CLI moved on": {toolResult("t1", true, "<tool_use_error>Permission to use Bash has been denied.</tool_use_error>"), resultLine},
		"CLI exited":   {"exit 0"},
	} {
		t.Run(name, func(t *testing.T) {
			ended := make(chan error, 1)
			ap := &approvals{answer: func(ctx context.Context, _ ApprovalRequest) (Decision, error) {
				<-ctx.Done()
				ended <- ctx.Err()
				return Decision{}, ctx.Err()
			}}
			w, _, _, _ := fakeStream(t, fixTools(10*time.Second), append([]string{"in", fixInit,
				toolUse("t1", "Bash"), request("r1", "t1", "Bash", `{"command":"kubectl rollout restart x"}`), "sleep 0.3"}, tail...)...)
			done := make(chan struct{})
			go func() {
				w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve})
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Work still waits for the approval")
			}
			if err := <-ended; err == nil {
				t.Fatal("the approval ctx was not cancelled")
			}
		})
	}
}

// The CLI moving on ends the approval at once, not only when the session ends,
// and a result that arrives before the handler runs cancels it before it waits.
func TestStreamAbandonedApproval(t *testing.T) {
	for name, script := range map[string][]string{
		"result after request":  {toolUse("t1", "Bash"), request("r1", "t1", "Bash", `{"command":"kubectl rollout restart x"}`), "sleep 0.2", toolResult("t1", true, "<tool_use_error>Permission to use Bash has been denied.</tool_use_error>"), "sleep 2", resultLine},
		"result before request": {toolUse("t1", "Bash"), toolResult("t1", true, "<tool_use_error>Permission to use Bash has been denied.</tool_use_error>"), request("r1", "t1", "Bash", `{"command":"kubectl rollout restart x"}`), "sleep 2", resultLine},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			waited := make(chan time.Duration, 1)
			ap := &approvals{answer: func(ctx context.Context, _ ApprovalRequest) (Decision, error) {
				select {
				case <-ctx.Done():
					waited <- time.Since(start)
					return Decision{}, ctx.Err()
				case <-time.After(5 * time.Second):
					waited <- time.Since(start)
					return Decision{Allow: true, By: "UAPP"}, nil
				}
			}}
			w, _, _, _ := fakeStream(t, fixTools(10*time.Second), append([]string{"in", fixInit}, script...)...)
			res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve})
			if err != nil || len(res.Actions) != 0 {
				t.Fatalf("res = %+v, err = %v", res, err)
			}
			if d := <-waited; d > 1500*time.Millisecond {
				t.Fatalf("the approval waited %s after the CLI moved on", d)
			}
		})
	}
}

// A result that arrives with little time left is kept: the drain after it is
// not the round's work.
func TestStreamResultBeatsClock(t *testing.T) {
	w, _, _, _ := fakeStream(t, fixTools(time.Second), "in", fixInit, resultLine, "sleep 1.5")
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err != nil {
		t.Fatalf("a finished round failed: %v", err)
	}
}

// The path is checked again after the approval: a symlink swapped in while it
// waited is refused.
func TestStreamRecheckAfterApproval(t *testing.T) {
	outside := t.TempDir()
	var w *Claude
	var stdin func() []map[string]any
	ap := &approvals{}
	ap.answer = func(context.Context, ApprovalRequest) (Decision, error) {
		dir := filepath.Join(w.tasks, testTask, "conf")
		os.RemoveAll(dir)
		os.Symlink(outside, dir)
		return Decision{Allow: true, By: "UAPP"}, nil
	}
	w, _, stdin, _ = fakeStream(t, fixTools(10*time.Second), "in", fixInit,
		toolUse("t1", "Edit"), request("r1", "t1", "Edit", `{"file_path":"conf/app.yaml","old_string":"a","new_string":"b"}`), "in",
		toolResult("t1", true, "denied"), resultLine)
	os.MkdirAll(filepath.Join(w.tasks, testTask, "conf"), 0o700)
	res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve})
	if err != nil || len(ap.asked) != 1 || len(res.Actions) != 0 {
		t.Fatalf("res = %+v, asked %d, err %v", res, len(ap.asked), err)
	}
	if _, b, _ := response(stdin()[1]); b != "deny" {
		t.Fatalf("the swapped path was answered %q", b)
	}
}

// Fix-mode sessions run with a fresh HOME of their own and without
// CLAUDE_CONFIG_DIR; propose-mode sessions keep the agent's.
func TestStreamFixModeFreshHome(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/shared/config")
	w, args, _, _ := fakeStream(t, fixTools(10*time.Second), "in", fixInit, resultLine)
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err != nil {
		t.Fatal(err)
	}
	read := func(ext string) string {
		b, _ := os.ReadFile(os.Getenv("FAKE_ARGS") + ext)
		return strings.TrimSpace(string(b))
	}
	home := read(".home")
	if home == os.Getenv("HOME") || filepath.Base(filepath.Dir(home)) != "home" || read(".ccd") != "unset" {
		t.Fatalf("HOME = %s, CLAUDE_CONFIG_DIR = %s", home, read(".ccd"))
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("session HOME %s left behind", home)
	}
	_ = args
	w, _, _, _ = fakeStream(t, nil, "in", initLine, resultLine)
	if _, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := read(".home"); got != os.Getenv("HOME") || read(".ccd") != "/shared/config" {
		t.Fatalf("propose mode: HOME = %s, CLAUDE_CONFIG_DIR = %s", got, read(".ccd"))
	}
}

// What the approver sees is scrubbed, and says so.
func TestApprovalDisplayScrubbed(t *testing.T) {
	ap := &approvals{answer: allowAs("UAPP")}
	w, _, _, _ := fakeStream(t, fixTools(10*time.Second), "in", fixInit,
		toolUse("t1", "Bash"), request("r1", "t1", "Bash", `{"command":"kubectl rollout restart x --token=sk-ant-oat01-hidden","timeout":60000}`), "in",
		toolResult("t1", false, "ok"), resultLine)
	w.secret = append(w.secret, "sk-ant-oat01-hidden")
	res, err := w.Work(context.Background(), Task{ID: testTask, Transcript: "x", Approve: ap.approve})
	if err != nil {
		t.Fatal(err)
	}
	if d := ap.asked[0]; !d.Redacted || strings.Contains(d.Display, "hidden") || !strings.Contains(d.Display, "(timeout 60000 ms)") {
		t.Fatalf("display %+v", d)
	}
	if strings.Contains(res.Actions[0].Input, "hidden") {
		t.Fatalf("action input %q", res.Actions[0].Input)
	}
}
