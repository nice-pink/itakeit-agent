// Package agent claims tasks in itakeit's channels and reports its progress the
// way a person does: with reactions on the task message and replies in its thread.
//
// Tasks, approvals and learning requests are keyed by channel and message ts
// together (key), since a ts is unique only within its channel.
//
// It never talks to itakeit directly. itakeit sees the agent as one more user, so
// the channel shows the agent as the owner and every itakeit rule (owners only,
// needs_info pings, stale reminders) applies to it unchanged.
//
// Slack events and work results are handled on one goroutine, which owns all
// state. Model calls run on their own goroutines and hand their result back to it.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nice-pink/itakeit-agent/pkg/worker"
	"github.com/nice-pink/itakeit/pkg/task"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// API is the subset of *slack.Client the agent uses.
type API interface {
	PostMessage(channel string, opts ...slack.MsgOption) (string, string, error)
	AddReaction(name string, item slack.ItemRef) error
	RemoveReaction(name string, item slack.ItemRef) error
	GetReactions(item slack.ItemRef, p slack.GetReactionsParameters) (slack.ReactedItem, error)
	GetConversationReplies(p *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
	GetConversationHistory(p *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	UpdateMessage(channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
	GetConversationsForUser(p *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)
	GetUsersInConversation(p *slack.GetUsersInConversationParameters) ([]string, string, error)
}

// job is a task the agent owns. held are the status reactions it has on the task
// message: one in steady state, more only after a crash between add and remove.
type job struct {
	held    []task.Action
	working bool
	again   bool               // replied to while working: run another round after this one
	cancel  context.CancelFunc // stops the running round
	// resumeBy, when set, are the only users whose reply starts the next round:
	// a fix-mode task interrupted by a restart resumes only when they say so.
	resumeBy []string
}

func (j *job) holds(a task.Action) bool { return slices.Contains(j.held, a) }

type Agent struct {
	api    API
	work   worker.Worker
	cfg    *Config
	emoji  map[task.Action]string
	me     string          // the agent's user ID
	meBot  string          // the agent's bot ID
	jobs   map[string]*job // by key
	queued map[string]bool // top-level messages waiting for or in triage
	joined map[string]bool // auto_channels: the channels the agent is a member of
	sem    chan struct{}   // triage and rounds without tools
	// sessions bound the rounds with tools, running or waiting for an approval.
	// They are separate from sem, so a round waiting for the approver never
	// holds up triage or rounds without tools.
	sessions  chan struct{}
	tools     bool
	approvals map[string]*approval // pending, by the approval message's key
	learner   Learner              // nil: no memory
	learned   map[string]bool      // learning requests already answered, by message key

	ctx context.Context
	do  chan func()
	// spawn runs fn off the loop and applies the func it returns on the loop.
	// after runs f on the loop once d has passed. Tests replace both with inline calls.
	spawn func(fn func() func())
	after func(d time.Duration, f func())
}

func New(api API, w worker.Worker, cfg *Config, me, meBot string) *Agent {
	a := &Agent{api: api, work: w, cfg: cfg, emoji: cfg.Display(), me: me, meBot: meBot,
		jobs: map[string]*job{}, queued: map[string]bool{}, joined: map[string]bool{}, sem: make(chan struct{}, cfg.Agent.MaxParallel),
		sessions: make(chan struct{}, cfg.Agent.MaxSessions), tools: cfg.Agent.WorkerTools() != nil,
		approvals: map[string]*approval{}, learned: map[string]bool{}, ctx: context.Background(), do: make(chan func(), 64)}
	a.spawn = func(fn func() func()) { go func() { a.post(fn()) }() }
	a.after = func(d time.Duration, f func()) { time.AfterFunc(d, func() { a.post(f) }) }
	return a
}

func (a *Agent) post(f func()) {
	select {
	case a.do <- f:
	case <-a.ctx.Done():
	}
}

// Run recovers owned tasks from the channels, then consumes events until ctx ends.
func (a *Agent) Run(ctx context.Context, sm *socketmode.Client) error {
	a.ctx = ctx
	errc := make(chan error, 1)
	go func() { errc <- sm.RunContext(ctx) }()
	events := ackLoop(ctx, sm)
	a.Recover()
	if a.cfg.AutoChannels {
		var tick func()
		tick = func() { a.relist(); a.after(relistEvery, tick) }
		a.after(relistEvery, tick)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case e := <-events:
			a.Handle(e)
		case f := <-a.do:
			f()
		}
	}
}

func ackLoop(ctx context.Context, sm *socketmode.Client) <-chan slackevents.EventsAPIEvent {
	out := make(chan slackevents.EventsAPIEvent, 1024)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt := <-sm.Events:
				switch evt.Type {
				case socketmode.EventTypeConnected:
					slog.Info("connected to slack")
				case socketmode.EventTypeConnectionError:
					slog.Warn("slack connection error, retrying", "data", evt.Data)
				case socketmode.EventTypeEventsAPI, socketmode.EventTypeInteractive:
					if evt.Request != nil {
						sm.Ack(*evt.Request)
					}
					if e, ok := evt.Data.(slackevents.EventsAPIEvent); ok {
						select {
						case out <- e:
						default:
							slog.Error("event queue full, dropping event", "type", e.InnerEvent.Type)
						}
					}
				}
			}
		}
	}()
	return out
}

// Handle dispatches one Events API event. Exported for tests.
func (a *Agent) Handle(e slackevents.EventsAPIEvent) {
	switch ev := e.InnerEvent.Data.(type) {
	case *slackevents.ReactionAddedEvent:
		if a.serves(ev.Item.Channel) {
			a.onReaction(ev)
		}
		return
	case *slackevents.MemberJoinedChannelEvent:
		// itakeit's bot joining counts too: with auto_channels the agent serves
		// only channels itakeit is in.
		switch {
		case ev.User == a.me:
			a.onJoin(ev.Channel, "re-invited")
		case ev.User != "" && ev.User == a.cfg.Agent.ItakeitUser:
			a.onJoin(ev.Channel, "paused while itakeit was not in the channel")
		}
		return
	case *slackevents.MemberLeftChannelEvent:
		if ev.User == a.me || (ev.User != "" && ev.User == a.cfg.Agent.ItakeitUser && a.cfg.AutoChannels) {
			a.onLeave(ev.Channel)
		}
		return
	case *slackevents.ChannelUnarchiveEvent:
		a.onJoin(ev.Channel, "paused while the channel was archived")
		return
	case *slackevents.GroupUnarchiveEvent:
		a.onJoin(ev.Channel, "paused while the channel was archived")
		return
	case *slackevents.ChannelLeftEvent:
		a.onLeave(ev.Channel)
		return
	case *slackevents.GroupLeftEvent:
		a.onLeave(ev.Channel)
		return
	case *slackevents.ChannelArchiveEvent:
		a.onLeave(ev.Channel)
		return
	case *slackevents.GroupArchiveEvent:
		a.onLeave(ev.Channel)
		return
	}
	ev, ok := e.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok || !a.serves(ev.Channel) {
		return
	}
	ch := ev.Channel
	switch ev.SubType {
	case "message_deleted":
		a.forget(key(ch, ev.DeletedTimeStamp), "the task was deleted")
	case "message_changed":
		if ev.Message != nil && ev.Message.SubType == "tombstone" {
			a.forget(key(ch, ev.Message.Timestamp), "the task was deleted")
		}
	case "", "bot_message", "file_share", "thread_broadcast":
		if ev.User == a.me || (ev.BotID != "" && ev.BotID == a.meBot) || a.fromItakeit(ev) {
			return
		}
		if ev.ThreadTimeStamp != "" && ev.ThreadTimeStamp != ev.TimeStamp {
			a.onReply(key(ch, ev.ThreadTimeStamp), ev.User, ev.BotID, ev.Text)
			return
		}
		a.onTask(key(ch, ev.TimeStamp), ev.Text)
	}
}

// fromItakeit reports a message itakeit posted: by its bot's member ID, or by
// the App ID in the message's bot_profile.
func (a *Agent) fromItakeit(ev *slackevents.MessageEvent) bool {
	s := a.cfg.Agent
	if s.ItakeitUser != "" && ev.User == s.ItakeitUser {
		return true
	}
	return s.ItakeitApp != "" && ev.Message != nil && ev.Message.BotProfile != nil && ev.Message.BotProfile.AppID == s.ItakeitApp
}

// forget drops a task: its round is cancelled and its pending approvals are
// denied with why. The approval messages show why too, unless why is
// leftChannel: the agent can no longer update messages there.
func (a *Agent) forget(ts, why string) {
	if j := a.jobs[ts]; j != nil && j.cancel != nil {
		j.cancel()
	}
	for _, ap := range a.approvals {
		if ap.task == ts {
			label := "Cancelled: " + why
			if why == leftChannel {
				label = ""
			}
			a.resolveApproval(ap, worker.Decision{Reason: why}, label)
		}
	}
	delete(a.jobs, ts)
	delete(a.queued, ts)
	a.cleanup(ts)
}

// cleanup drops what the worker kept for a task: its tool sessions' directory.
func (a *Agent) cleanup(ts string) {
	if err := a.work.Cleanup(ts); err != nil {
		slog.Warn("cleanup", "ts", ts, "err", err)
	}
}

// onTask waits claim_delay so people get the first pick, then triages the task.
func (a *Agent) onTask(ts, text string) {
	if a.jobs[ts] != nil || a.queued[ts] {
		return
	}
	a.queued[ts] = true
	a.after(a.cfg.ClaimDelay(), func() { a.consider(ts, text) })
}

func (a *Agent) consider(ts, text string) {
	if !a.queued[ts] {
		return // deleted while waiting
	}
	if a.taken(ts) {
		delete(a.queued, ts)
		return
	}
	a.spawn(func() func() {
		a.sem <- struct{}{}
		take, reason, err := a.work.Triage(a.ctx, unescape(text))
		<-a.sem
		return func() {
			if !a.queued[ts] {
				return
			}
			delete(a.queued, ts)
			switch {
			case err != nil:
				slog.Warn("triage failed, leaving task", "ts", ts, "err", err)
			case !take:
				slog.Info("task not taken", "ts", ts, "reason", reason)
			case a.taken(ts):
				slog.Info("task taken by someone during triage", "ts", ts)
			default:
				a.take(ts, reason)
			}
		}
	})
}

// taken reports whether the agent should leave a task alone: someone marked it
// done, or, unless claim_owned, someone holds a reaction that makes them an
// owner (a claim, or with status_claims any status). Errors count as taken,
// since the message may be gone.
func (a *Agent) taken(ts string) bool {
	item, err := a.api.GetReactions(a.ref(ts), slack.GetReactionsParameters{Full: true})
	if err != nil {
		slog.Warn("reactions", "ts", ts, "err", err)
		return true
	}
	for _, r := range item.Reactions {
		act, ok := a.cfg.Action(r.Name)
		others := slices.ContainsFunc(r.Users, func(u string) bool { return u != a.me })
		if !ok || !others {
			continue
		}
		if act == task.Done || (!a.cfg.Agent.ClaimOwned && (act == task.Claim || a.cfg.StatusClaims)) {
			return true
		}
	}
	return false
}

func (a *Agent) take(ts, reason string) {
	if err := a.react(ts, task.Claim); err != nil {
		return
	}
	a.jobs[ts] = &job{}
	slog.Info("task taken", "ts", ts)
	a.say(ts, "I'm taking this. "+reason)
	a.start(ts)
}

// onReply resumes a task the agent is waiting on (needs_info or blocked) when a
// person answers in its thread, and any owned task when the agent is mentioned.
// Any reply during a round queues one more round, so the new reply is read.
func (a *Agent) onReply(threadTS, user, botID, text string) {
	j := a.jobs[threadTS]
	if j == nil || botID != "" {
		return
	}
	if j.resumeBy != nil {
		if !slices.Contains(j.resumeBy, user) {
			slog.Info("reply ignored: only the approver resumes this task", "ts", threadTS, "user", user)
			return
		}
		j.resumeBy = nil
		a.start(threadTS)
		return
	}
	mentioned := strings.Contains(text, "<@"+a.me+">")
	switch {
	case j.working:
		j.again = true
	case mentioned || j.holds(task.NeedsInfo) || j.holds(task.Blocked):
		a.start(threadTS)
	}
}

// start runs one round of work on the whole thread.
func (a *Agent) start(ts string) {
	j := a.jobs[ts]
	j.working = true
	ctx, cancel := context.WithCancel(a.ctx)
	j.cancel = cancel
	a.setStatus(ts, task.InProgress)
	slot := a.sem
	if a.tools {
		slot = a.sessions
	}
	approve := a.approveFunc(ts)
	a.spawn(func() func() {
		defer cancel()
		slot <- struct{}{}
		defer func() { <-slot }()
		transcript, err := a.transcript(ts)
		var res worker.Result
		if err == nil {
			res, err = a.work.Work(ctx, worker.Task{ID: ts, Transcript: transcript, Approve: approve})
		}
		return func() { a.finish(ts, j, res, err) }
	})
}

// finish applies a round's result to j, the job that started it. A job that
// is gone, or was replaced (the agent left the channel and was invited back,
// which recovered the task as a new job), drops the result: the new job runs
// its own rounds.
func (a *Agent) finish(ts string, j *job, res worker.Result, err error) {
	if cur := a.jobs[ts]; cur != j {
		if cur == nil {
			a.cleanup(ts) // deleted while working: the round may have recreated its directory
		}
		return
	}
	j.working, j.cancel = false, nil
	switch {
	case err != nil:
		slog.Warn("work failed", "ts", ts, "err", err)
		res.Status, res.Reply = task.Blocked, fmt.Sprintf("I stopped on an error: %v\nMention me to retry.", err)
	case strings.TrimSpace(res.Reply) == "":
		res.Status, res.Reply = task.Blocked, "I ended up with no answer for this. It needs a human, or mention me to retry."
	}
	// Reply first, so itakeit's needs_info ping lands under the question. A status
	// without its reply would claim work nobody can see. The footer lists the
	// writes that ran, on the error path too; both parts are escaped before the
	// cut, so the footer always fits.
	f := footer(res)
	if _, err := a.postMsg(ts, cutEscaped(escape(res.Reply), maxReply-len([]rune(f)))+f); err != nil {
		res.Status = task.Blocked
		a.say(ts, fmt.Sprintf("Slack rejected my reply (%v). Mention me to retry.", err))
	}
	slog.Info("work round finished", "ts", ts, "status", res.Status)
	if j.again {
		// Someone replied during the round: go again with the status left at
		// in_progress, so a question already answered does not ping the reporter.
		j.again = false
		a.start(ts)
		return
	}
	a.setStatus(ts, res.Status)
	if res.Status == task.Done {
		a.cleanup(ts)
		a.learn(ts, res.Learning)
	}
}

// transcript renders the task and its thread for the worker. Other bots' replies
// (itakeit's card and pings) are left out.
func (a *Agent) transcript(ts string) (string, error) {
	msgs, err := a.thread(ts)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	reporter := "<@" + msgs[0].User + ">"
	if msgs[0].User == "" {
		reporter = "an integration"
	}
	fmt.Fprintf(&b, "Task reported by %s:\n%s\n", reporter, unescape(msgs[0].Text))
	for _, m := range msgs[1:] {
		switch {
		case m.User == a.me:
			fmt.Fprintf(&b, "\nYou:\n%s\n", unescape(m.Text))
		case m.BotID == "" && m.User != a.cfg.Agent.ItakeitUser:
			fmt.Fprintf(&b, "\n<@%s>:\n%s\n", m.User, unescape(m.Text))
		}
	}
	return b.String(), nil
}

// thread reads the task message and all its replies.
func (a *Agent) thread(k string) ([]slack.Message, error) {
	ch, ts := split(k)
	var msgs []slack.Message
	cursor := ""
	for {
		page, more, next, err := a.api.GetConversationReplies(&slack.GetConversationRepliesParameters{ChannelID: ch, Timestamp: ts, Cursor: cursor, Limit: 200})
		if err != nil {
			return nil, fmt.Errorf("read thread: %w", err)
		}
		msgs = append(msgs, page...)
		if !more || next == "" {
			break
		}
		cursor = next
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("read thread: task message %s not found", ts)
	}
	return msgs, nil
}

// setStatus adds the new status reaction before removing the old ones. itakeit
// takes the latest added status, and removing the current one would clear it.
func (a *Agent) setStatus(ts string, s task.Action) {
	j := a.jobs[ts]
	if _, ok := a.emoji[s]; !ok || (len(j.held) == 1 && j.held[0] == s) {
		return // disabled in config, or already set
	}
	// A status held alongside stale ones may not be itakeit's latest: take it off
	// and add it again, so itakeit sees it as the newest reaction.
	if j.holds(s) && !a.unreact(ts, s) {
		return
	}
	if err := a.react(ts, s); err != nil {
		return
	}
	held := []task.Action{s}
	for _, old := range j.held {
		if old != s && !a.unreact(ts, old) {
			held = append(held, old) // still on the message: retry on the next change
		}
	}
	j.held = held
}

func (a *Agent) react(ts string, act task.Action) error {
	err := a.api.AddReaction(a.emoji[act], a.ref(ts))
	if err != nil && err.Error() != "already_reacted" {
		slog.Warn("add reaction", "ts", ts, "action", act, "err", err)
		return err
	}
	return nil
}

// unreact reports whether the reaction is off the message.
func (a *Agent) unreact(ts string, act task.Action) bool {
	if err := a.api.RemoveReaction(a.emoji[act], a.ref(ts)); err != nil && err.Error() != "no_reaction" {
		slog.Warn("remove reaction", "ts", ts, "action", act, "err", err)
		return false
	}
	return true
}

// maxReply keeps a reply under Slack's 40,000 character limit for message text.
const maxReply = 39000

func (a *Agent) say(ts, text string) error {
	_, err := a.postMsg(ts, cutEscaped(escape(text), maxReply))
	return err
}

// postMsg posts text that is already escaped in the task's thread and returns
// the new message's key.
func (a *Agent) postMsg(k, escaped string) (string, error) {
	ch, ts := split(k)
	_, msg, err := a.api.PostMessage(ch, slack.MsgOptionText(escaped, false), slack.MsgOptionTS(ts))
	if err != nil {
		slog.Warn("post", "ts", k, "err", err)
		return "", err
	}
	return key(ch, msg), nil
}

// cutEscaped cuts escaped text to n runes without splitting an entity. It
// runs after escaping, since escaping grows the text.
func cutEscaped(text string, n int) string {
	if r := []rune(text); len(r) > n {
		text = string(r[:max(n, 0)])
		if amp := strings.LastIndex(text, "&"); amp > strings.LastIndex(text, ";") {
			text = text[:amp] // do not cut an entity in half
		}
		text += "\n…(cut off)"
	}
	return text
}

// escapeLiteral makes &, < and > literal without restoring references, for
// text that is not the model's reply, such as a tool's input: <@U123> in a
// command is shown as written and pings nobody.
func escapeLiteral(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

var reference = regexp.MustCompile(`&lt;([@#][UWC][A-Z0-9]+(?:\|[^&]*)?)&gt;`)

// escape makes &, < and > literal, so "a < b" and "<-ch" in model output render
// as written and "<!channel>" or "<!subteam^…>" cannot ping anyone. User and
// channel references (<@U…>, <#C…|name>) stay live. Links render as text.
func escape(text string) string {
	text = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	return reference.ReplaceAllString(text, "<$1>")
}

// unescape turns Slack's message text back into what the person typed, so the
// model reads "x := <-ch" and not "x := &lt;-ch", and does not copy the entities
// into a reply that escape would then escape twice.
func unescape(text string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(text)
}

func (a *Agent) ref(k string) slack.ItemRef { return slack.NewRefToMessage(split(k)) }

// CheckChannel reads one message of the channel, so a channel the agent
// cannot read stops it at startup instead of being ignored.
func CheckChannel(api API, channel string) error {
	_, err := api.GetConversationHistory(&slack.GetConversationHistoryParameters{ChannelID: channel, Limit: 1})
	switch {
	case err == nil:
		return nil
	case err.Error() == "channel_not_found":
		return fmt.Errorf("channel %s not found: list the IDs of itakeit's channels in the config's channels (in Slack: the channel's name -> About -> Channel ID), the same as in itakeit's config. A private channel needs the agent invited first", channel)
	case err.Error() == "not_in_channel":
		return fmt.Errorf("the agent is not in channel %s: run /invite @<the agent's app> in it", channel)
	case err.Error() == "missing_scope":
		return fmt.Errorf("reading channel %s: missing_scope: the agent's app needs channels:history (and groups:history for a private channel), see slack-app-manifest.yaml", channel)
	}
	return fmt.Errorf("reading channel %s: %w", channel, err)
}

// Recover rebuilds the owned tasks from the agent's own reactions in the last
// recover_messages messages of each channel, so no database is needed. Tasks
// that were being worked on when the agent stopped are worked on again. With
// auto_channels the channel listing does it, one channel at a time.
func (a *Agent) Recover() {
	if a.cfg.AutoChannels {
		a.relist()
	} else {
		for _, ch := range a.cfg.Channels {
			a.recoverChannel(ch, "restarted")
		}
	}
	slog.Info("recovered", "tasks", len(a.jobs), "channels", len(a.channels()))
}

// recoverChannel rebuilds the owned tasks of one channel. why is what
// interrupted them, as the thread is told: "restarted", or "re-invited" when
// the agent comes back to a channel it left.
func (a *Agent) recoverChannel(channel, why string) {
	var msgs []slack.Message
	cursor := ""
	for len(msgs) < a.cfg.Agent.RecoverMessages {
		resp, err := a.api.GetConversationHistory(&slack.GetConversationHistoryParameters{ChannelID: channel, Cursor: cursor, Limit: min(200, a.cfg.Agent.RecoverMessages-len(msgs))})
		if err != nil {
			slog.Warn("recover: history", "channel", channel, "err", err)
			return
		}
		msgs = append(msgs, resp.Messages...)
		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
			break
		}
		cursor = resp.ResponseMetaData.NextCursor
	}
	for _, m := range msgs {
		claimed, held := false, []task.Action{}
		for _, r := range m.Reactions {
			act, ok := a.cfg.Action(r.Name)
			if !ok || !slices.Contains(r.Users, a.me) {
				continue
			}
			if act == task.Claim {
				claimed = true
			} else {
				held = append(held, act)
			}
		}
		k := key(channel, m.Timestamp)
		if !claimed || a.jobs[k] != nil {
			continue
		}
		a.jobs[k] = &job{held: held}
		switch {
		case !slices.Contains(held, task.Done) && !slices.Contains(held, task.NeedsInfo) && !slices.Contains(held, task.Blocked):
			a.recoverRound(k, why)
		case a.fix() && (slices.Contains(held, task.NeedsInfo) || slices.Contains(held, task.Blocked)):
			a.rearmResume(k)
		}
	}
}

func (a *Agent) fix() bool { return a.cfg.Agent.Mode == string(worker.ModeFix) }

// resumeAsk and humanNow mark what Recover posts, so a later restart finds it
// and keeps the gate.
const (
	resumeAsk = "reply here to have me resume, or say what to do instead."
	humanNow  = "writes may already have run: see the notices above. It needs a human now."
)

// recoverRound handles a task whose round the restart interrupted. Approval
// requests of that round can no longer be answered, so they are marked
// expired. In propose mode nothing can have been changed, and the round runs
// again. In fix mode writes may already have run, so the task waits until the
// approver (the reporter when there is none) says to resume.
func (a *Agent) recoverRound(ts, why string) {
	msgs, err := a.thread(ts)
	if err != nil {
		slog.Warn("recover: thread", "ts", ts, "err", err)
	}
	ch, _ := split(ts)
	for _, m := range msgs {
		if m.User == a.me && pendingApproval(m.Text) {
			text := m.Text[:strings.LastIndex(m.Text, "\n")] + "\nExpired (agent " + why + ")"
			if _, _, _, err := a.api.UpdateMessage(ch, m.Timestamp, slack.MsgOptionText(text, false)); err != nil {
				slog.Warn("recover: expire approval", "msg", m.Timestamp, "err", err)
			}
		}
	}
	if !a.fix() {
		slog.Info("recover: resuming", "ts", ts)
		a.start(ts)
		return
	}
	who := a.resumer(msgs)
	a.jobs[ts].resumeBy = []string{who} // [""] when unknown: nobody's reply resumes it
	if who == "" {
		// No approver, and the reporter is a bot or unknown: a human takes over.
		a.say(ts, "I was "+why+" during this task, and "+humanNow)
		a.setStatus(ts, task.Blocked)
		return
	}
	slog.Info("recover: asking to resume", "ts", ts, "who", who)
	if _, err := a.postMsg(ts, fmt.Sprintf("I was %s during this task, and writes may already have run: see the notices and approvals above. <@%s>: %s", why, who, resumeAsk)); err != nil {
		// Without the question in the thread a later restart could not find the
		// gate again, so the task goes to a human instead.
		a.jobs[ts].resumeBy = []string{""}
		a.setStatus(ts, task.Blocked)
		return
	}
	a.setStatus(ts, task.NeedsInfo)
}

// rearmResume keeps the resume gate across a later restart. It looks at the
// agent's last message: the resume question means the task only resumes for
// the user it names, and resumes now if that user already answered while the
// agent was down; the hand-over notice means nobody's reply resumes it.
func (a *Agent) rearmResume(ts string) {
	msgs, err := a.thread(ts)
	if err != nil {
		slog.Warn("recover: thread", "ts", ts, "err", err)
		a.jobs[ts].resumeBy = []string{""} // cannot tell: fail closed
		return
	}
	last := -1
	for i := len(msgs) - 1; i > 0 && last < 0; i-- {
		if msgs[i].User == a.me {
			last = i
		}
	}
	switch {
	case last < 0:
	case strings.Contains(msgs[last].Text, humanNow):
		a.jobs[ts].resumeBy = []string{""}
	case strings.Contains(msgs[last].Text, resumeAsk):
		who := a.resumer(msgs)
		if slices.ContainsFunc(msgs[last+1:], func(m slack.Message) bool { return m.User == who }) {
			slog.Info("recover: resume already approved", "ts", ts, "who", who)
			a.start(ts)
			return
		}
		a.jobs[ts].resumeBy = []string{who}
	}
}

// resumer is who may resume an interrupted fix-mode task: the approver, or
// without one the reporter.
func (a *Agent) resumer(msgs []slack.Message) string {
	if a.cfg.Agent.Approver != "" || len(msgs) == 0 || msgs[0].BotID != "" {
		return a.cfg.Agent.Approver // a bot reporter cannot answer: its replies are ignored
	}
	return msgs[0].User
}

// pendingApproval reports an approval request the agent posted that has no
// outcome yet: its last line still asks for a reaction.
func pendingApproval(text string) bool {
	i := strings.LastIndex(text, "\n")
	return i > 0 && strings.Contains(text, " approval needed (write ") && strings.HasPrefix(text[i+1:], "React :")
}
