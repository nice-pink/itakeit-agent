// Package agent claims tasks in an itakeit channel and reports its progress the
// way a person does: with reactions on the task message and replies in its thread.
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
}

// job is a task the agent owns. held are the status reactions it has on the task
// message: one in steady state, more only after a crash between add and remove.
type job struct {
	held    []task.Action
	working bool
	again   bool // replied to while working: run another round after this one
}

func (j *job) holds(a task.Action) bool { return slices.Contains(j.held, a) }

type Agent struct {
	api    API
	work   worker.Worker
	cfg    *Config
	emoji  map[task.Action]string
	me     string // the agent's user ID
	meBot  string // the agent's bot ID
	jobs   map[string]*job
	queued map[string]bool // top-level messages waiting for or in triage
	sem    chan struct{}

	ctx context.Context
	do  chan func()
	// spawn runs fn off the loop and applies the func it returns on the loop.
	// after runs f on the loop once d has passed. Tests replace both with inline calls.
	spawn func(fn func() func())
	after func(d time.Duration, f func())
}

func New(api API, w worker.Worker, cfg *Config, me, meBot string) *Agent {
	a := &Agent{api: api, work: w, cfg: cfg, emoji: cfg.Display(), me: me, meBot: meBot,
		jobs: map[string]*job{}, queued: map[string]bool{}, sem: make(chan struct{}, cfg.Agent.MaxParallel),
		ctx: context.Background(), do: make(chan func(), 64)}
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

// Run recovers owned tasks from the channel, then consumes events until ctx ends.
func (a *Agent) Run(ctx context.Context, sm *socketmode.Client) error {
	a.ctx = ctx
	errc := make(chan error, 1)
	go func() { errc <- sm.RunContext(ctx) }()
	events := ackLoop(ctx, sm)
	a.Recover()
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
	ev, ok := e.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok || ev.Channel != a.cfg.Channel {
		return
	}
	switch ev.SubType {
	case "message_deleted":
		a.forget(ev.DeletedTimeStamp)
	case "message_changed":
		if ev.Message != nil && ev.Message.SubType == "tombstone" {
			a.forget(ev.Message.Timestamp)
		}
	case "", "bot_message", "file_share", "thread_broadcast":
		if ev.User == a.me || (ev.BotID != "" && ev.BotID == a.meBot) || ev.User == a.cfg.Agent.ItakeitUser {
			return
		}
		if ev.ThreadTimeStamp != "" && ev.ThreadTimeStamp != ev.TimeStamp {
			a.onReply(ev.ThreadTimeStamp, ev.BotID, ev.Text)
			return
		}
		a.onTask(ev.TimeStamp, ev.Text)
	}
}

func (a *Agent) forget(ts string) {
	delete(a.jobs, ts)
	delete(a.queued, ts)
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
func (a *Agent) onReply(threadTS, botID, text string) {
	j := a.jobs[threadTS]
	if j == nil || botID != "" {
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
	a.setStatus(ts, task.InProgress)
	a.spawn(func() func() {
		a.sem <- struct{}{}
		defer func() { <-a.sem }()
		transcript, err := a.transcript(ts)
		var res worker.Result
		if err == nil {
			res, err = a.work.Work(a.ctx, transcript)
		}
		return func() { a.finish(ts, res, err) }
	})
}

func (a *Agent) finish(ts string, res worker.Result, err error) {
	j := a.jobs[ts]
	if j == nil {
		return // deleted while working
	}
	j.working = false
	switch {
	case err != nil:
		slog.Warn("work failed", "ts", ts, "err", err)
		res = worker.Result{Status: task.Blocked, Reply: fmt.Sprintf("I stopped on an error: %v\nMention me to retry.", err)}
	case strings.TrimSpace(res.Reply) == "":
		res = worker.Result{Status: task.Blocked, Reply: "I ended up with no answer for this. It needs a human, or mention me to retry."}
	}
	// Reply first, so itakeit's needs_info ping lands under the question. A status
	// without its reply would claim work nobody can see.
	if err := a.say(ts, res.Reply); err != nil {
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
}

// transcript renders the task and its thread for the worker. Other bots' replies
// (itakeit's card and pings) are left out.
func (a *Agent) transcript(ts string) (string, error) {
	var msgs []slack.Message
	cursor := ""
	for {
		page, more, next, err := a.api.GetConversationReplies(&slack.GetConversationRepliesParameters{ChannelID: a.cfg.Channel, Timestamp: ts, Cursor: cursor, Limit: 200})
		if err != nil {
			return "", fmt.Errorf("read thread: %w", err)
		}
		msgs = append(msgs, page...)
		if !more || next == "" {
			break
		}
		cursor = next
	}
	if len(msgs) == 0 {
		return "", fmt.Errorf("read thread: task message %s not found", ts)
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
	text = escape(text)
	if r := []rune(text); len(r) > maxReply {
		text = string(r[:maxReply])
		if amp := strings.LastIndex(text, "&"); amp > strings.LastIndex(text, ";") {
			text = text[:amp] // do not cut an entity in half
		}
		text += "\n…(cut off)"
	}
	_, _, err := a.api.PostMessage(a.cfg.Channel, slack.MsgOptionText(text, false), slack.MsgOptionTS(ts))
	if err != nil {
		slog.Warn("post", "ts", ts, "err", err)
	}
	return err
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

func (a *Agent) ref(ts string) slack.ItemRef { return slack.NewRefToMessage(a.cfg.Channel, ts) }

// Recover rebuilds the owned tasks from the agent's own reactions in the last
// recover_messages channel messages, so no database is needed. Tasks that were
// being worked on when the agent stopped are worked on again.
func (a *Agent) Recover() {
	var msgs []slack.Message
	cursor := ""
	for len(msgs) < a.cfg.Agent.RecoverMessages {
		resp, err := a.api.GetConversationHistory(&slack.GetConversationHistoryParameters{ChannelID: a.cfg.Channel, Cursor: cursor, Limit: min(200, a.cfg.Agent.RecoverMessages-len(msgs))})
		if err != nil {
			slog.Warn("recover: history", "err", err)
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
		if !claimed {
			continue
		}
		a.jobs[m.Timestamp] = &job{held: held}
		if !slices.Contains(held, task.Done) && !slices.Contains(held, task.NeedsInfo) && !slices.Contains(held, task.Blocked) {
			slog.Info("recover: resuming", "ts", m.Timestamp)
			a.start(m.Timestamp)
		}
	}
	slog.Info("recovered", "tasks", len(a.jobs))
}
