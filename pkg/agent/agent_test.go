package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nice-pink/itakeit-agent/pkg/worker"
	"github.com/nice-pink/itakeit/pkg/task"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

const (
	channel = "C1"
	me      = "UAGENT"
	itakeit = "UITAKEIT"
)

// fakeAPI keeps reactions per message and records every call in order.
type fakeAPI struct {
	mu         sync.Mutex // work runs off the loop in the approval tests
	calls      []string
	reactions  map[string][]slack.ItemReaction // ts -> reactions
	threads    map[string][]slack.Message
	history    []slack.Message
	failPost   bool
	failRm     map[string]bool // reaction names whose removal fails
	posts      int
	botRoot    bool              // the task message was posted by a bot
	failRead   bool              // GetConversationReplies fails
	updates    map[string]string // message ts -> text after UpdateMessage
	historyErr error             // GetConversationHistory fails with it
}

func newFake() *fakeAPI {
	return &fakeAPI{reactions: map[string][]slack.ItemReaction{}, threads: map[string][]slack.Message{}, updates: map[string]string{}}
}

func (f *fakeAPI) PostMessage(ch string, opts ...slack.MsgOption) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, vals, _ := slack.UnsafeApplyMsgOptions("", ch, "", opts...)
	ts := vals.Get("thread_ts")
	f.calls = append(f.calls, "post "+ts+" "+vals.Get("text"))
	if f.failPost {
		return "", "", errors.New("rate_limited")
	}
	f.posts++
	msgTS := fmt.Sprintf("900.%d", f.posts)
	f.threads[ts] = append(f.threads[ts], slack.Message{Msg: slack.Msg{User: me, Text: vals.Get("text"), Timestamp: msgTS}})
	return ch, msgTS, nil
}

func (f *fakeAPI) UpdateMessage(ch, ts string, opts ...slack.MsgOption) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, vals, _ := slack.UnsafeApplyMsgOptions("", ch, "", opts...)
	f.calls = append(f.calls, "update "+ts+" "+vals.Get("text"))
	f.updates[ts] = vals.Get("text")
	return ch, ts, vals.Get("text"), nil
}

func (f *fakeAPI) AddReaction(name string, item slack.ItemRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "+"+name)
	rs := f.reactions[item.Timestamp]
	for i := range rs {
		if rs[i].Name == name {
			if slices.Contains(rs[i].Users, me) {
				return errors.New("already_reacted")
			}
			rs[i].Users = append(rs[i].Users, me)
			return nil
		}
	}
	f.reactions[item.Timestamp] = append(rs, slack.ItemReaction{Name: name, Users: []string{me}})
	return nil
}

func (f *fakeAPI) RemoveReaction(name string, item slack.ItemRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "-"+name)
	if f.failRm[name] {
		return errors.New("rate_limited")
	}
	rs := f.reactions[item.Timestamp]
	for i := range rs {
		if rs[i].Name == name && slices.Contains(rs[i].Users, me) {
			rs[i].Users = slices.DeleteFunc(rs[i].Users, func(u string) bool { return u == me })
			return nil
		}
	}
	return errors.New("no_reaction")
}

func (f *fakeAPI) GetReactions(item slack.ItemRef, _ slack.GetReactionsParameters) (slack.ReactedItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slack.ReactedItem{Reactions: f.reactions[item.Timestamp]}
	for thread, msgs := range f.threads {
		for _, m := range msgs {
			if m.Timestamp == item.Timestamp {
				m.ThreadTimestamp = thread
				if text, ok := f.updates[m.Timestamp]; ok {
					m.Text = text
				}
				out.Message = &m
			}
		}
	}
	return out, nil
}

func (f *fakeAPI) GetConversationReplies(p *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRead {
		return nil, false, "", errors.New("ratelimited")
	}
	root := slack.Message{Msg: slack.Msg{User: "UREP", Text: "task " + p.Timestamp, Timestamp: p.Timestamp}}
	if f.botRoot {
		root.User, root.BotID = "UBOTAPP", "BINTEGRATION"
	}
	return append([]slack.Message{root}, f.threads[p.Timestamp]...), false, "", nil
}

func (f *fakeAPI) GetConversationHistory(*slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return &slack.GetConversationHistoryResponse{Messages: f.history}, nil
}

// fakeWorker answers triage with take and each work round with the next result.
type fakeWorker struct {
	take        bool
	results     []worker.Result
	transcripts []string
	ids         []string
	cleaned     []string
	onTriage    func()
	onWork      func(context.Context, worker.Task) (worker.Result, error)
	mu          sync.Mutex // Work runs off the loop in the approval tests
}

// rounds is how many rounds Work ran, safe to read while one runs.
func (w *fakeWorker) rounds() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.transcripts)
}

func (w *fakeWorker) Cleanup(id string) error {
	w.cleaned = append(w.cleaned, id)
	return nil
}

func (w *fakeWorker) Triage(context.Context, string) (bool, string, error) {
	if w.onTriage != nil {
		w.onTriage()
	}
	return w.take, "I can answer this.", nil
}

func (w *fakeWorker) Work(ctx context.Context, t worker.Task) (worker.Result, error) {
	if w.onWork != nil {
		return w.onWork(ctx, t)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.transcripts = append(w.transcripts, t.Transcript)
	w.ids = append(w.ids, t.ID)
	if len(w.results) == 0 {
		return worker.Result{}, errors.New("no result left")
	}
	r := w.results[0]
	w.results = w.results[1:]
	return r, nil
}

func setup(t *testing.T, cfgExtra string) (*Agent, *fakeAPI, *fakeWorker) {
	t.Helper()
	cfg, err := Parse([]byte(fmt.Sprintf("channel: %s\n%s\nagent:\n  itakeit_user: %s\n  skills: answering questions\n", channel, cfgExtra, itakeit)))
	if err != nil {
		t.Fatal(err)
	}
	api, w := newFake(), &fakeWorker{take: true}
	a := New(api, w, cfg, me, "BAGENT")
	a.spawn = func(fn func() func()) { fn()() }
	a.after = func(_ time.Duration, f func()) { f() }
	return a, api, w
}

func msg(ts, thread, user, text string) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
		Data: &slackevents.MessageEvent{Channel: channel, TimeStamp: ts, ThreadTimeStamp: thread, User: user, Text: text}}}
}

func reactionsBy(api *fakeAPI, ts, user string) []string {
	var out []string
	for _, r := range api.reactions[ts] {
		if slices.Contains(r.Users, user) {
			out = append(out, r.Name)
		}
	}
	return out
}

func TestTakeAndFinish(t *testing.T) {
	a, api, w := setup(t, "")
	w.results = []worker.Result{{Status: task.Done, Reply: "Here is the answer."}}
	a.Handle(msg("1.0", "", "UREP", "what is x?"))

	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "white_check_mark"}) {
		t.Fatalf("reactions = %v", got)
	}
	// Claim before any status, and every new status added before the old one is removed.
	want := []string{"+raising_hand", "post 1.0 I'm taking this. I can answer this.", "+construction", "post 1.0 Here is the answer.", "+white_check_mark", "-construction"}
	if !slices.Equal(api.calls, want) {
		t.Fatalf("calls =\n%v\nwant\n%v", api.calls, want)
	}
}

func TestSkipsClaimedAndIgnored(t *testing.T) {
	a, api, w := setup(t, "")
	api.reactions["1.0"] = []slack.ItemReaction{{Name: "raising_hand", Users: []string{"UHUMAN"}}}
	a.Handle(msg("1.0", "", "UREP", "mine already"))
	a.Handle(msg("2.0", "", itakeit, "board"))
	a.Handle(msg("3.0", "", me, "my own"))
	w.take = false
	a.Handle(msg("4.0", "", "UREP", "not for the agent"))
	if len(api.calls) != 0 || len(a.jobs) != 0 {
		t.Fatalf("calls = %v, jobs = %v", api.calls, a.jobs)
	}
}

func TestStatusClaimsCountsStatusReactions(t *testing.T) {
	a, api, _ := setup(t, "status_claims: true")
	api.reactions["1.0"] = []slack.ItemReaction{{Name: "eyes", Users: []string{"UHUMAN"}}}
	a.Handle(msg("1.0", "", "UREP", "x"))
	if len(a.jobs) != 0 {
		t.Fatal("took a task a human owns by status reaction")
	}
}

func TestNeedsInfoResumesOnAnswer(t *testing.T) {
	a, api, w := setup(t, "")
	w.results = []worker.Result{{Status: task.NeedsInfo, Reply: "Which env?"}, {Status: task.Done, Reply: "Done for prod."}}
	a.Handle(msg("1.0", "", "UREP", "fix it"))
	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "question"}) {
		t.Fatalf("reactions = %v", got)
	}

	a.Handle(msg("1.1", "1.0", itakeit, "<@UREP>: needs more details")) // itakeit's ping is not an answer
	if len(w.transcripts) != 1 {
		t.Fatal("resumed on itakeit's own message")
	}
	api.threads["1.0"] = append(api.threads["1.0"], slack.Message{Msg: slack.Msg{User: "UREP", Text: "prod"}})
	a.Handle(msg("1.2", "1.0", "UREP", "prod"))
	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "white_check_mark"}) {
		t.Fatalf("reactions = %v", got)
	}
	if tr := w.transcripts[1]; !strings.Contains(tr, "You:\nWhich env?") || !strings.Contains(tr, "<@UREP>:\nprod") {
		t.Fatalf("transcript =\n%s", tr)
	}
}

func TestDoneIgnoresRepliesUnlessMentioned(t *testing.T) {
	a, _, w := setup(t, "")
	w.results = []worker.Result{{Status: task.Done, Reply: "a"}, {Status: task.Done, Reply: "b"}}
	a.Handle(msg("1.0", "", "UREP", "x"))
	a.Handle(msg("1.1", "1.0", "UREP", "thanks"))
	if len(w.transcripts) != 1 {
		t.Fatal("reworked a done task on a plain reply")
	}
	a.Handle(msg("1.2", "1.0", "UREP", "<@"+me+"> one more thing"))
	if len(w.transcripts) != 2 {
		t.Fatal("mention did not rework")
	}
}

func TestErrorBlocks(t *testing.T) {
	a, api, _ := setup(t, "")
	a.Handle(msg("1.0", "", "UREP", "x")) // worker has no results: Work fails
	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "no_entry"}) {
		t.Fatalf("reactions = %v", got)
	}
}

func TestDeletedWhileWaiting(t *testing.T) {
	a, api, _ := setup(t, "")
	var pending func()
	a.after = func(_ time.Duration, f func()) { pending = f }
	a.Handle(msg("1.0", "", "UREP", "x"))
	a.Handle(slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
		Data: &slackevents.MessageEvent{Channel: channel, SubType: "message_deleted", DeletedTimeStamp: "1.0"}}})
	pending()
	if len(api.calls) != 0 {
		t.Fatalf("calls = %v", api.calls)
	}
}

func TestRecover(t *testing.T) {
	a, api, w := setup(t, "")
	mine := func(names ...string) []slack.ItemReaction {
		var rs []slack.ItemReaction
		for _, n := range names {
			rs = append(rs, slack.ItemReaction{Name: n, Users: []string{me}})
		}
		return rs
	}
	api.history = []slack.Message{
		{Msg: slack.Msg{Timestamp: "1.0", Reactions: mine("raising_hand", "construction")}},     // interrupted: resume
		{Msg: slack.Msg{Timestamp: "2.0", Reactions: mine("raising_hand", "question")}},         // waiting on reporter
		{Msg: slack.Msg{Timestamp: "3.0", Reactions: mine("construction")}},                     // not claimed: not ours
		{Msg: slack.Msg{Timestamp: "4.0", Reactions: mine("raising_hand", "eyes", "no_entry")}}, // crash left two statuses
	}
	for ts, r := range map[string][]slack.ItemReaction{"1.0": api.history[0].Reactions, "2.0": api.history[1].Reactions, "4.0": api.history[3].Reactions} {
		api.reactions[ts] = slices.Clone(r)
	}
	w.results = []worker.Result{{Status: task.Done, Reply: "finished"}}
	a.Recover()

	if len(a.jobs) != 3 || len(w.transcripts) != 1 {
		t.Fatalf("jobs = %d, rounds = %d", len(a.jobs), len(w.transcripts))
	}
	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "white_check_mark"}) {
		t.Fatalf("1.0 reactions = %v", got)
	}
	w.results = []worker.Result{{Status: task.Done, Reply: "ok"}}
	a.Handle(msg("4.1", "4.0", "UREP", "unblocked now"))
	if got := reactionsBy(api, "4.0", me); !slices.Equal(got, []string{"raising_hand", "white_check_mark"}) {
		t.Fatalf("4.0 reactions = %v, want both stale statuses gone", got)
	}
}

// deferSpawn makes spawned work wait until the returned run is called, so tests
// can deliver events while a round is in flight.
func deferSpawn(a *Agent) (run func()) {
	var queue []func() func()
	a.spawn = func(fn func() func()) { queue = append(queue, fn) }
	return func() {
		for len(queue) > 0 {
			fn := queue[0]
			queue = queue[1:]
			fn()()
		}
	}
}

func TestReplyDuringRoundIsRead(t *testing.T) {
	a, api, w := setup(t, "")
	w.results = []worker.Result{{Status: task.NeedsInfo, Reply: "Which env?"}, {Status: task.NeedsInfo, Reply: "Which env, reporter?"}, {Status: task.Done, Reply: "Done for prod."}}
	a.Handle(msg("1.0", "", "UREP", "fix it"))
	run := deferSpawn(a)
	a.Handle(msg("1.1", "1.0", "UOTHER", "I think staging")) // starts round 2
	api.threads["1.0"] = append(api.threads["1.0"], slack.Message{Msg: slack.Msg{User: "UREP", Text: "prod"}})
	a.Handle(msg("1.2", "1.0", "UREP", "prod")) // arrives while round 2 runs
	run()
	if len(w.transcripts) != 3 || !strings.Contains(w.transcripts[2], "prod") {
		t.Fatalf("rounds = %d, want the answer read in a third round", len(w.transcripts))
	}
	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "white_check_mark"}) {
		t.Fatalf("reactions = %v", got)
	}
}

func TestFailedOrEmptyReplyBlocks(t *testing.T) {
	a, api, w := setup(t, "")
	w.results = []worker.Result{{Status: task.Done, Reply: "  "}}
	a.Handle(msg("1.0", "", "UREP", "x"))
	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "no_entry"}) {
		t.Fatalf("empty reply: reactions = %v", got)
	}

	a, api, w = setup(t, "")
	w.results = []worker.Result{{Status: task.Done, Reply: "answer"}}
	api.failPost = true
	a.Handle(msg("1.0", "", "UREP", "x"))
	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "no_entry"}) {
		t.Fatalf("failed post: reactions = %v", got)
	}
}

func TestReplyIsEscaped(t *testing.T) {
	a, api, w := setup(t, "")
	w.results = []worker.Result{{Status: task.Done, Reply: "use `x <- ch` if a < b & <@U1> agrees, not <!channel>"}}
	a.Handle(msg("1.0", "", "UREP", "x"))
	want := "post 1.0 use `x &lt;- ch` if a &lt; b &amp; <@U1> agrees, not &lt;!channel&gt;"
	if !slices.Contains(api.calls, want) {
		t.Fatalf("calls = %v", api.calls)
	}
}

func TestClaimOwnedSkipsDone(t *testing.T) {
	a, api, _ := setup(t, "")
	a.cfg.Agent.ClaimOwned = true
	api.reactions["1.0"] = []slack.ItemReaction{{Name: "raising_hand", Users: []string{"UHUMAN"}}, {Name: "white_check_mark", Users: []string{"UHUMAN"}}}
	api.reactions["2.0"] = []slack.ItemReaction{{Name: "raising_hand", Users: []string{"UHUMAN"}}}
	a.Handle(msg("1.0", "", "UREP", "done already"))
	a.Handle(msg("2.0", "", "UREP", "co-own this"))
	if a.jobs["1.0"] != nil || a.jobs["2.0"] == nil {
		t.Fatalf("jobs = %v", a.jobs)
	}
}

func TestClaimDuringTriage(t *testing.T) {
	a, api, w := setup(t, "")
	w.onTriage = func() {
		api.reactions["1.0"] = []slack.ItemReaction{{Name: "raising_hand", Users: []string{"UHUMAN"}}}
	}
	a.Handle(msg("1.0", "", "UREP", "x"))
	if len(a.jobs) != 0 || len(api.calls) != 0 {
		t.Fatalf("claimed a task a human took during triage: %v", api.calls)
	}
}

func TestFailedRemovalRetried(t *testing.T) {
	a, api, w := setup(t, "")
	w.results = []worker.Result{{Status: task.NeedsInfo, Reply: "q"}, {Status: task.Done, Reply: "a"}}
	api.failRm = map[string]bool{"construction": true}
	a.Handle(msg("1.0", "", "UREP", "x"))
	if got := a.jobs["1.0"].held; !slices.Equal(got, []task.Action{task.NeedsInfo, task.InProgress}) {
		t.Fatalf("held = %v", got)
	}
	api.failRm = nil
	a.Handle(msg("1.1", "1.0", "UREP", "answer"))
	if got := reactionsBy(api, "1.0", me); !slices.Equal(got, []string{"raising_hand", "white_check_mark"}) {
		t.Fatalf("reactions = %v", got)
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	cases := map[string]string{
		"x := <-ch && a > b":              "x := &lt;-ch &amp;&amp; a &gt; b",
		"ask <@U1> or <@U2|bob>":          "ask <@U1> or <@U2|bob>",
		"see <#C123> and <#C123|general>": "see <#C123> and <#C123|general>",
		"<!channel> <!subteam^S1>":        "&lt;!channel&gt; &lt;!subteam^S1&gt;",
		"<https://x.io|docs>":             "&lt;https://x.io|docs&gt;",
	}
	for in, want := range cases {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
	// Slack delivers "x := <-ch && y" as below: the model reads it as typed, and a
	// verbatim copy posts back to the same text.
	slackText := "x := &lt;-ch &amp;&amp; y"
	if got := escape(unescape(slackText)); got != slackText {
		t.Errorf("round trip = %q", got)
	}
}

func TestTranscriptUnescapes(t *testing.T) {
	a, api, w := setup(t, "")
	w.results = []worker.Result{{Status: task.Done, Reply: "ok"}}
	api.threads["1.0"] = []slack.Message{{Msg: slack.Msg{User: "UREP", Text: "log: a &lt; b &amp;&amp; c"}}}
	a.Handle(msg("1.0", "", "UREP", "x"))
	if !strings.Contains(w.transcripts[0], "log: a < b && c") {
		t.Fatalf("transcript =\n%s", w.transcripts[0])
	}
}

func TestStaleHeldStatusReadded(t *testing.T) {
	a, api, _ := setup(t, "")
	a.jobs["1.0"] = &job{held: []task.Action{task.Done, task.InProgress}} // a removal of in_progress failed earlier
	api.calls = nil
	a.setStatus("1.0", task.InProgress)
	want := []string{"-construction", "+construction", "-white_check_mark"}
	if !slices.Equal(api.calls, want) {
		t.Fatalf("calls = %v, want %v", api.calls, want)
	}
}

func TestRejectedReplyExplained(t *testing.T) {
	a, api, w := setup(t, "")
	w.results = []worker.Result{{Status: task.Done, Reply: "answer"}}
	api.failPost = true
	a.Handle(msg("1.0", "", "UREP", "x"))
	if !slices.ContainsFunc(api.calls, func(c string) bool { return strings.Contains(c, "Slack rejected my reply (rate_limited)") }) {
		t.Fatalf("calls = %v", api.calls)
	}
}

// The worker keeps a directory per task for tool sessions: it gets the task's
// ts, and drops the directory once the task is done or deleted, not while the
// task waits for an answer.
func TestTaskIDAndCleanup(t *testing.T) {
	a, _, w := setup(t, "")
	w.results = []worker.Result{{Status: task.NeedsInfo, Reply: "Which env?"}, {Status: task.Done, Reply: "Done."}}
	a.Handle(msg("1.0", "", "UREP", "fix it"))
	if !slices.Equal(w.ids, []string{"1.0"}) || len(w.cleaned) != 0 {
		t.Fatalf("after needs_info: ids = %v, cleaned = %v", w.ids, w.cleaned)
	}
	a.Handle(msg("1.1", "1.0", "UREP", "prod"))
	if !slices.Equal(w.cleaned, []string{"1.0"}) {
		t.Fatalf("after done: cleaned = %v", w.cleaned)
	}
	w.results = []worker.Result{{Status: task.NeedsInfo, Reply: "?"}}
	a.Handle(msg("2.0", "", "UREP", "other"))
	a.Handle(slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
		Data: &slackevents.MessageEvent{Channel: channel, SubType: "message_deleted", DeletedTimeStamp: "2.0"}}})
	if !slices.Equal(w.cleaned, []string{"1.0", "2.0"}) {
		t.Fatalf("after delete: cleaned = %v", w.cleaned)
	}
}

// Slack shows an app no member ID, so itakeit is also known by its App ID,
// which every message it posts carries in bot_profile.
func TestSkipsItakeitByAppID(t *testing.T) {
	cfg, err := Parse([]byte(fmt.Sprintf("channel: %s\nagent:\n  itakeit_app: AITAKEIT\n  skills: answering questions\n", channel)))
	if err != nil {
		t.Fatal(err)
	}
	api, w := newFake(), &fakeWorker{take: true, results: []worker.Result{{Status: task.Done, Reply: "ok"}}}
	a := New(api, w, cfg, me, "BAGENT")
	a.spawn = func(fn func() func()) { fn()() }
	a.after = func(_ time.Duration, f func()) { f() }
	// Raw Events API JSON through slack-go's parser, which fills Message (and
	// its bot_profile) for a plain message: the match relies on that.
	botMsg := func(ts, user, app string) slackevents.EventsAPIEvent {
		raw := fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel":%q,"ts":%q,"user":%q,"bot_id":"B%s","text":"board","bot_profile":{"app_id":%q}}}`, channel, ts, user, app, app)
		e, err := slackevents.ParseEvent(json.RawMessage(raw), slackevents.OptionNoVerifyToken())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	a.Handle(botMsg("1.0", "UITAKEITBOT", "AITAKEIT"))
	if len(api.calls) != 0 || len(a.jobs) != 0 {
		t.Fatalf("took itakeit's message: calls = %v", api.calls)
	}
	// Another integration's message, with no user, is still a task.
	a.Handle(botMsg("2.0", "", "AALERTS"))
	if a.jobs["2.0"] == nil {
		t.Fatalf("integration task not taken: calls = %v", api.calls)
	}
}

func TestCheckChannel(t *testing.T) {
	api := newFake()
	if err := CheckChannel(api, channel); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[string]string{"channel_not_found": "Channel ID", "not_in_channel": "/invite", "missing_scope": "channels:history", "ratelimited": "ratelimited"} {
		api.historyErr = slack.SlackErrorResponse{Err: code}
		if err := CheckChannel(api, channel); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", code, err)
		}
	}
}
