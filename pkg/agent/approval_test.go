package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nice-pink/itakeit-agent/pkg/worker"
	"github.com/nice-pink/itakeit/pkg/task"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

const approver = "UAPP"

const fixAgent = "  mode: fix\n  approver: " + approver + "\n  tools:\n    read: [\"Bash(kubectl get *)\"]\n    write: [\"Bash(kubectl rollout restart *)\"]\n"

// setupFix is an agent in fix mode whose rounds run on goroutines, as in
// production: an approval blocks its round until the loop resolves it. pump
// runs loop funcs on the test goroutine until cond holds, and fire runs the
// timers that are due.
func setupFix(t *testing.T, agentYAML string) (a *Agent, api *fakeAPI, w *fakeWorker, pump func(cond func() bool), fire func()) {
	t.Helper()
	cfg, err := Parse([]byte(fmt.Sprintf("channel: %s\nagent:\n  itakeit_user: %s\n  skills: fixing things\n%s", channel, itakeit, agentYAML)))
	if err != nil {
		t.Fatal(err)
	}
	api, w = newFake(), &fakeWorker{take: true}
	a = New(api, w, cfg, me, "BAGENT")
	var timers []func()
	a.spawn = func(fn func() func()) { go func() { a.post(fn()) }() }
	a.after = func(d time.Duration, f func()) {
		if d == 0 { // claim_delay_seconds: 0, as the triage test sets
			f()
			return
		}
		timers = append(timers, f)
	}
	pump = func(cond func() bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for !cond() {
			select {
			case f := <-a.do:
				f()
			case <-deadline:
				t.Fatalf("not reached; calls = %v", api.calls)
			}
		}
	}
	fire = func() {
		due := timers
		timers = nil
		for _, f := range due {
			f()
		}
	}
	return
}

// asksOnce makes the worker ask for one write and report what it got.
func asksOnce(w *fakeWorker) chan worker.Decision {
	got := make(chan worker.Decision, 1)
	w.onWork = func(ctx context.Context, tk worker.Task) (worker.Result, error) {
		d, err := tk.Approve(ctx, worker.ApprovalRequest{Tool: "Bash", Display: "kubectl rollout restart deploy/<@U9>", Seq: 1})
		got <- d
		if err != nil {
			return worker.Result{}, err
		}
		res := worker.Result{Status: task.Done, Reply: "Restarted."}
		if d.Allow {
			res.Actions = []worker.Action{{Tool: "Bash", Input: "kubectl rollout restart deploy/<@U9>", By: d.By, Outcome: "ran"}}
		}
		return res, nil
	}
	return got
}

func reaction(user, name, msg string) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.ReactionAddedEvent{
		User: user, Reaction: name, Item: slackevents.Item{Type: "message", Channel: channel, Timestamp: msg}}}}
}

// startTask posts a task, lets the claim delay pass, and waits until the
// worker's approval request is pending.
func startTask(t *testing.T, a *Agent, pump func(func() bool), fire func()) *approval {
	a.Handle(msg("1.0", "", "UREP", "restart the api"))
	fire()
	pump(func() bool { return len(a.approvals) == 1 })
	for _, ap := range a.approvals {
		return ap
	}
	return nil
}

func TestApprovalByApprover(t *testing.T) {
	a, api, w, pump, fire := setupFix(t, fixAgent)
	got := asksOnce(w)
	ap := startTask(t, a, pump, fire)
	post := api.calls[slices.IndexFunc(api.calls, func(c string) bool { return strings.Contains(c, "approval needed") })]
	for _, want := range []string{"<@UAPP> approval needed (write 1 of max 20). Tool: `Bash`", "```\nkubectl rollout restart deploy/&lt;@U9&gt;\n```", "React :heavy_check_mark: to run it or :x: to deny. Expires in 30 min."} {
		if !strings.Contains(post, want) {
			t.Errorf("approval post lacks %q:\n%s", want, post)
		}
	}
	if !slices.Contains(api.calls, "+heavy_check_mark") || !slices.Contains(api.calls, "+x") {
		t.Errorf("approve and deny emoji not offered: %v", api.calls)
	}
	// Only the approver's reaction, with the configured emoji, on this message counts.
	a.Handle(reaction("UOTHER", "heavy_check_mark", ap.msg))
	a.Handle(reaction(me, "heavy_check_mark", ap.msg))
	a.Handle(reaction(approver, "thumbsup", ap.msg))
	a.Handle(reaction(approver, "heavy_check_mark", "900.999"))
	if len(a.approvals) != 1 || len(got) != 0 {
		t.Fatal("an approval resolved without the approver's reaction")
	}
	a.Handle(reaction(approver, "heavy_check_mark", ap.msg))
	d := <-got
	if !d.Allow || d.By != approver {
		t.Fatalf("decision = %+v", d)
	}
	pump(func() bool { return !a.jobs["1.0"].working })
	if u := api.updates[ap.msg]; !strings.HasSuffix(u, "\nApproved by <@UAPP> at "+time.Now().UTC().Format("15:04")+" UTC") && !strings.Contains(u, "Approved by <@UAPP>") {
		t.Errorf("approval message = %q", u)
	}
	reply := api.calls[slices.IndexFunc(api.calls, func(c string) bool { return strings.Contains(c, "Restarted.") })]
	if !strings.Contains(reply, "*Actions taken:*\n• `Bash` `kubectl rollout restart deploy/&lt;@U9&gt;`: approved by <@UAPP>, ran") {
		t.Errorf("reply footer:\n%s", reply)
	}
}

func TestApprovalDeniedAndTimedOut(t *testing.T) {
	cases := map[string]struct {
		act   func(a *Agent, api *fakeAPI, ap *approval, fire func())
		allow bool
		label string
	}{
		"deny reaction": {func(a *Agent, _ *fakeAPI, ap *approval, _ func()) { a.Handle(reaction(approver, "x", ap.msg)) }, false, "Denied by <@UAPP>"},
		"timeout":       {func(_ *Agent, _ *fakeAPI, _ *approval, fire func()) { fire() }, false, "Timed out: no reaction from <@UAPP> within 30 min"},
		// The event was lost, but the approver's reaction is on the message.
		"lost event": {func(_ *Agent, api *fakeAPI, ap *approval, fire func()) {
			api.mu.Lock()
			api.reactions[ap.msg] = append(api.reactions[ap.msg], slack.ItemReaction{Name: "heavy_check_mark", Users: []string{approver}})
			api.mu.Unlock()
			fire()
		}, true, "Approved by <@UAPP>"},
		"both emoji": {func(_ *Agent, api *fakeAPI, ap *approval, fire func()) {
			api.mu.Lock()
			api.reactions[ap.msg] = append(api.reactions[ap.msg], slack.ItemReaction{Name: "heavy_check_mark", Users: []string{approver}}, slack.ItemReaction{Name: "x", Users: []string{approver}})
			api.mu.Unlock()
			fire()
		}, false, "Denied by <@UAPP>"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a, api, w, pump, fire := setupFix(t, fixAgent)
			got := asksOnce(w)
			ap := startTask(t, a, pump, fire)
			c.act(a, api, ap, fire)
			if d := <-got; d.Allow != c.allow {
				t.Fatalf("decision = %+v, want allow %v", d, c.allow)
			}
			pump(func() bool { return !a.jobs["1.0"].working })
			if u := api.updates[ap.msg]; !strings.Contains(u, c.label) {
				t.Fatalf("approval message = %q, want %q", u, c.label)
			}
		})
	}
}

// While a round waits for the approver, triage of a new task goes on.
func TestTriageRunsDuringApproval(t *testing.T) {
	// No claim delay, so no timer has to fire: firing would also time out the
	// approval and free its slot.
	a, _, w, pump, fire := setupFix(t, "  claim_delay_seconds: 0\n  max_parallel: 1\n  max_sessions: 1\n"+fixAgent)
	asksOnce(w)
	triaged := make(chan string, 4)
	w.onTriage = func() { triaged <- "x" }
	startTask(t, a, pump, fire)
	<-triaged // the first task's own triage
	a.Handle(msg("2.0", "", "UREP", "another"))
	pump(func() bool { return len(triaged) == 1 })
}

func TestApprovalPostFailsDenies(t *testing.T) {
	a, api, w, pump, fire := setupFix(t, fixAgent)
	got := asksOnce(w)
	inner := w.onWork
	w.onWork = func(ctx context.Context, tk worker.Task) (worker.Result, error) {
		api.mu.Lock()
		api.failPost = true
		api.mu.Unlock()
		return inner(ctx, tk)
	}
	a.Handle(msg("1.0", "", "UREP", "restart the api"))
	fire()
	pump(func() bool { return len(got) == 1 })
	if d := <-got; d.Allow || !strings.Contains(d.Reason, "could not post") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestNoApproverPostsNotice(t *testing.T) {
	a, api, w, pump, fire := setupFix(t, "  mode: fix\n  allow_unapproved_writes: true\n  tools:\n    read: [\"Bash(kubectl get *)\"]\n    write: [\"Bash(kubectl rollout restart *)\"]\n")
	got := asksOnce(w)
	a.Handle(msg("1.0", "", "UREP", "restart the api"))
	fire()
	pump(func() bool { return len(got) == 1 })
	if d := <-got; !d.Allow || d.By != "" {
		t.Fatalf("decision = %+v", d)
	}
	pump(func() bool { return !a.jobs["1.0"].working })
	notice := slices.IndexFunc(api.calls, func(c string) bool { return strings.Contains(c, "Running `Bash` (write 1 of max 20):") })
	reply := slices.IndexFunc(api.calls, func(c string) bool { return strings.Contains(c, "Restarted.") })
	if notice < 0 || reply < notice || !strings.Contains(api.calls[reply], "allowed by config, ran") {
		t.Fatalf("calls = %v", api.calls)
	}
}

// Deleting the task cancels its round and its pending approval.
func TestForgetCancelsRound(t *testing.T) {
	a, api, w, pump, fire := setupFix(t, fixAgent)
	got := asksOnce(w)
	ap := startTask(t, a, pump, fire)
	a.Handle(slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
		Data: &slackevents.MessageEvent{Channel: channel, SubType: "message_deleted", DeletedTimeStamp: "1.0"}}})
	if d := <-got; d.Allow {
		t.Fatal("a deleted task's write was allowed")
	}
	if u := api.updates[ap.msg]; !strings.Contains(u, "Cancelled: the task was deleted") {
		t.Fatalf("approval message = %q", u)
	}
	pump(func() bool { return len(w.cleaned) >= 2 }) // on delete, and again when the round ends
}

// The footer survives the error path and a reply that fills the message.
func TestFooterOnErrorAndTruncation(t *testing.T) {
	act := []worker.Action{{Tool: "Bash", Input: "kubectl rollout restart x", By: approver, Outcome: "ran"}}
	for name, res := range map[string]struct {
		r   worker.Result
		err error
	}{
		"error": {worker.Result{Actions: act}, fmt.Errorf("claude: timed out")},
		"long":  {worker.Result{Status: task.Done, Reply: strings.Repeat("<&>", 20000), Actions: act, CapReached: true}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			a, api, w, pump, fire := setupFix(t, fixAgent)
			w.onWork = func(context.Context, worker.Task) (worker.Result, error) { return res.r, res.err }
			a.Handle(msg("1.0", "", "UREP", "x"))
			fire()
			pump(func() bool { j := a.jobs["1.0"]; return j != nil && !j.working && len(api.calls) > 3 })
			reply := api.calls[slices.IndexFunc(api.calls, func(c string) bool { return strings.Contains(c, "Actions taken") })]
			if n := len([]rune(reply)); n > 40000 || !strings.Contains(reply, "approved by <@UAPP>, ran") {
				t.Fatalf("reply of %d runes: ...%s", n, reply[max(0, len(reply)-300):])
			}
			if res.r.CapReached && !strings.Contains(reply, "Write limit of 20 reached") {
				t.Error("cap line missing")
			}
		})
	}
}

// A restart during a fix-mode round expires its approvals and waits for the
// approver: nobody else's reply resumes it.
func TestRecoverFixModeAsks(t *testing.T) {
	a, api, w, pump, _ := setupFix(t, fixAgent)
	api.history = []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", Reactions: []slack.ItemReaction{
		{Name: "raising_hand", Users: []string{me}}, {Name: "construction", Users: []string{me}}}}}}
	pending := "<@UAPP> approval needed (write 1 of max 20). Tool: `Bash`\n```\nx\n```\nReact :heavy_check_mark: to run it or :x: to deny. Expires in 30 min."
	api.threads["1.0"] = []slack.Message{{Msg: slack.Msg{User: me, Text: pending, Timestamp: "1.5"}}}
	a.Recover()
	if u := api.updates["1.5"]; !strings.HasSuffix(u, "\nExpired (agent restarted)") || strings.Contains(u, "React :") {
		t.Fatalf("pending approval = %q", u)
	}
	if !slices.ContainsFunc(api.calls, func(c string) bool { return strings.Contains(c, "<@UAPP>: "+resumeAsk) }) {
		t.Fatalf("no resume question: %v", api.calls)
	}
	if got := reactionsBy(api, "1.0", me); !slices.Contains(got, "question") || slices.Contains(got, "construction") {
		t.Fatalf("status = %v, want needs_info", got)
	}
	w.results = []worker.Result{{Status: task.Done, Reply: "Resumed."}}
	a.Handle(msg("1.6", "1.0", "UREP", "go on"))
	a.Handle(msg("1.7", "1.0", "UREP", "<@"+me+"> go on"))
	if j := a.jobs["1.0"]; j.working || w.rounds() != 0 { // a resumed round sets working at once
		t.Fatal("the reporter resumed a task only the approver may resume")
	}
	a.Handle(msg("1.8", "1.0", approver, "yes, resume"))
	pump(func() bool { return w.rounds() == 1 && !a.jobs["1.0"].working })
}

// A second restart keeps the gate while the question is unanswered.
func TestRecoverRearmsResumeGate(t *testing.T) {
	a, api, w, _, _ := setupFix(t, fixAgent)
	api.history = []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", Reactions: []slack.ItemReaction{
		{Name: "raising_hand", Users: []string{me}}, {Name: "question", Users: []string{me}}}}}}
	api.threads["1.0"] = []slack.Message{{Msg: slack.Msg{User: me, Text: "I was restarted ... <@UAPP>: " + resumeAsk, Timestamp: "1.5"}},
		{Msg: slack.Msg{User: "UREP", Text: "hurry", Timestamp: "1.6"}}}
	a.Recover()
	if got := a.jobs["1.0"].resumeBy; !slices.Equal(got, []string{approver}) {
		t.Fatalf("resumeBy = %v", got)
	}
	w.results = []worker.Result{{Status: task.Done, Reply: "ok"}}
	a.Handle(msg("1.7", "1.0", "UREP", "please"))
	if j := a.jobs["1.0"]; j.working || w.rounds() != 0 { // a resumed round sets working at once
		t.Fatal("the reporter resumed the task")
	}
	// Answered by the approver before the restart: no gate.
	api.threads["2.0"] = []slack.Message{{Msg: slack.Msg{User: me, Text: "<@UAPP>: " + resumeAsk, Timestamp: "2.5"}}, {Msg: slack.Msg{User: approver, Text: "wait", Timestamp: "2.6"}}}
	api.history = []slack.Message{{Msg: slack.Msg{Timestamp: "2.0", Reactions: []slack.ItemReaction{
		{Name: "raising_hand", Users: []string{me}}, {Name: "question", Users: []string{me}}}}}}
	a.Recover()
	if got := a.jobs["2.0"].resumeBy; got != nil {
		t.Fatalf("answered question kept its gate: %v", got)
	}
}

// A bot reporter cannot answer, so without an approver nobody's reply resumes
// the task, and that gate survives another restart.
func TestRecoverBotReporter(t *testing.T) {
	a, api, w, _, _ := setupFix(t, "  mode: fix\n  allow_unapproved_writes: true\n  tools:\n    write: [\"Bash(kubectl rollout restart *)\"]\n")
	api.history = []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", Reactions: []slack.ItemReaction{
		{Name: "raising_hand", Users: []string{me}}, {Name: "construction", Users: []string{me}}}}}}
	api.botRoot = true
	a.Recover()
	if got := reactionsBy(api, "1.0", me); !slices.Contains(got, "no_entry") {
		t.Fatalf("status = %v, want blocked", got)
	}
	a.Handle(msg("1.6", "1.0", "UOTHER", "resume"))
	if j := a.jobs["1.0"]; j.working || w.rounds() != 0 { // a resumed round sets working at once
		t.Fatal("someone resumed a task handed to a human")
	}
	b, api2, _, _, _ := setupFix(t, "  mode: fix\n  allow_unapproved_writes: true\n  tools:\n    write: [\"Bash(kubectl rollout restart *)\"]\n")
	api2.history = []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", Reactions: []slack.ItemReaction{
		{Name: "raising_hand", Users: []string{me}}, {Name: "no_entry", Users: []string{me}}}}}}
	api2.threads["1.0"] = []slack.Message{{Msg: slack.Msg{User: me, Text: "I was restarted during this task, and " + humanNow, Timestamp: "1.5"}}}
	b.Recover()
	if got := b.jobs["1.0"].resumeBy; !slices.Equal(got, []string{""}) {
		t.Fatalf("after a second restart resumeBy = %v", got)
	}
}

// A thread that cannot be read on a later restart keeps the task closed.
func TestRecoverRearmFailsClosed(t *testing.T) {
	a, api, w, _, _ := setupFix(t, fixAgent)
	api.history = []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", Reactions: []slack.ItemReaction{
		{Name: "raising_hand", Users: []string{me}}, {Name: "question", Users: []string{me}}}}}}
	api.failRead = true
	a.Recover()
	api.failRead = false
	a.Handle(msg("1.6", "1.0", "UREP", "resume"))
	if j := a.jobs["1.0"]; j.working || w.rounds() != 0 { // a resumed round sets working at once
		t.Fatal("a reply resumed a task whose gate could not be checked")
	}
}

// The approver answered the question while the agent was down: the task resumes.
func TestRecoverResumeAlreadyApproved(t *testing.T) {
	a, api, w, pump, _ := setupFix(t, fixAgent)
	api.history = []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", Reactions: []slack.ItemReaction{
		{Name: "raising_hand", Users: []string{me}}, {Name: "question", Users: []string{me}}}}}}
	api.threads["1.0"] = []slack.Message{{Msg: slack.Msg{User: me, Text: "<@UAPP>: " + resumeAsk, Timestamp: "1.5"}},
		{Msg: slack.Msg{User: approver, Text: "go ahead", Timestamp: "1.6"}}}
	w.results = []worker.Result{{Status: task.Done, Reply: "ok"}}
	a.Recover()
	pump(func() bool { return w.rounds() == 1 && !a.jobs["1.0"].working })
}

// Without an approver, the reporter may resume.
func TestRecoverFixModeReporterResumes(t *testing.T) {
	a, api, w, pump, _ := setupFix(t, "  mode: fix\n  allow_unapproved_writes: true\n  tools:\n    write: [\"Bash(kubectl rollout restart *)\"]\n")
	api.history = []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", Reactions: []slack.ItemReaction{
		{Name: "raising_hand", Users: []string{me}}, {Name: "construction", Users: []string{me}}}}}}
	a.Recover()
	if got := a.jobs["1.0"].resumeBy; !slices.Equal(got, []string{"UREP"}) {
		t.Fatalf("resumeBy = %v", got)
	}
	w.results = []worker.Result{{Status: task.Done, Reply: "ok"}}
	a.Handle(msg("1.6", "1.0", "UREP", "resume"))
	pump(func() bool { return w.rounds() == 1 && !a.jobs["1.0"].working })
}

func TestCodeBlockAndEscapeLiteral(t *testing.T) {
	if got := codeBlock("a <@U1> & ``` b"); got != "```\na &lt;@U1&gt; &amp; ``\u200b` b\n```" {
		t.Fatalf("codeBlock = %q", got)
	}
	if got := cutEscaped(strings.Repeat("a", 5)+"&lt;", 7); got != "aaaaa\n…(cut off)" {
		t.Fatalf("cutEscaped = %q", got)
	}
}
