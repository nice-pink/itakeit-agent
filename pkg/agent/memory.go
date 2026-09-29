package agent

import (
	"context"
	"crypto/hmac"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// Learner saves what a done task taught. *worker.Memory is one. Sign returns
// a MAC over the task and the text, with a key the model never sees.
type Learner interface {
	Save(ctx context.Context, taskID, text string) error
	Sign(taskID, text string) string
}

// WithMemory makes the agent save the learnings work rounds return.
func (a *Agent) WithMemory(l Learner) *Agent {
	a.learner = l
	return a
}

// learnAsk marks a learning waiting for the approver. The request carries the
// learning, its task and a signature over both, so an approval needs no state:
// a reaction after a restart still saves exactly the text the approver read.
// The signature is what tells a request from a model reply shaped like one,
// which the agent's user posts too.
const learnAsk = " save this to memory?"

// ref ends a pending request's last line: the task ts and the signature.
func ref(ts, sig string) string { return fmt.Sprintf(" (ref %s %s)", ts, sig) }

var refLine = regexp.MustCompile(`^React :.* \(ref ([0-9]{1,20}\.[0-9]{1,10}) ([0-9a-f]{64})\)$`)

// learn saves a done task's learning, or with memory.approval asks the
// approver first. Without approval the thread still shows what was saved.
func (a *Agent) learn(ts, text string) {
	if a.learner == nil || strings.TrimSpace(text) == "" {
		return
	}
	s := a.cfg.Agent
	if !s.Memory.NeedsApproval() {
		a.spawn(func() func() {
			err := a.learner.Save(a.ctx, ts, text)
			return func() {
				if err != nil {
					slog.Warn("save learning", "ts", ts, "err", err)
					a.say(ts, "I could not save what I learned to memory. The agent's log says why.")
					return
				}
				slog.Info("learning saved", "ts", ts)
				a.postMsg(ts, "Saved to memory:\n"+codeBlock(text))
			}
		})
		return
	}
	head := fmt.Sprintf("<@%s>%s\n%s", s.Approver, learnAsk, codeBlock(text))
	last := fmt.Sprintf("React :%s: to save it or :%s: to drop it.", s.ApprovalEmoji.Approve, s.ApprovalEmoji.Deny) + ref(ts, a.learner.Sign(ts, text))
	msg, err := a.postMsg(ts, head+"\n"+last)
	if err != nil {
		return
	}
	for _, e := range []string{s.ApprovalEmoji.Approve, s.ApprovalEmoji.Deny} {
		if err := a.api.AddReaction(e, a.ref(msg)); err != nil { // offered for a click, not needed
			slog.Warn("add learning reaction", "msg", msg, "err", err)
		}
	}
	slog.Info("learning waits for approval", "ts", ts, "msg", msg)
}

// onLearningReaction saves or drops a learning when the approver reacts to
// its request. The request is read back from Slack, so it counts only when
// the agent posted it, it is still pending, and its signature matches.
func (a *Agent) onLearningReaction(ev *slackevents.ReactionAddedEvent) {
	s := a.cfg.Agent
	msg := ev.Item.Timestamp
	if a.learner == nil || !s.Memory.NeedsApproval() || ev.User != s.Approver || ev.ItemUser != a.me ||
		(ev.Reaction != s.ApprovalEmoji.Approve && ev.Reaction != s.ApprovalEmoji.Deny) || a.learned[msg] {
		return
	}
	item, err := a.api.GetReactions(a.ref(msg), slack.GetReactionsParameters{Full: true})
	if err != nil || item.Message == nil {
		slog.Warn("read learning request", "msg", msg, "err", err)
		return
	}
	m := item.Message
	ts, text, sig, ok := pendingLearning(m.Text)
	switch {
	case m.User != a.me || !ok:
		return // not a pending request
	}
	a.learned[msg] = true // a second reaction before the update lands saves nothing twice
	body := m.Text[:strings.LastIndex(m.Text, "\n")]
	update := func(label string) {
		if _, _, _, err := a.api.UpdateMessage(a.cfg.Channel, msg, slack.MsgOptionText(body+"\n"+label, false)); err != nil {
			slog.Warn("update learning request", "msg", msg, "err", err)
		}
	}
	if !hmac.Equal([]byte(sig), []byte(a.learner.Sign(ts, text))) {
		// A reply posing as a request, or text Slack stored differently from what
		// was posted (a link it rewrote). Either way nothing can be saved from it.
		slog.Warn("learning request with a wrong signature, not saved", "msg", msg)
		update("Not saved: this request cannot be verified.")
		return
	}
	if ev.Reaction == s.ApprovalEmoji.Deny {
		slog.Info("learning dropped", "ts", ts, "msg", msg)
		update(fmt.Sprintf("Dropped by <@%s>", ev.User))
		return
	}
	a.spawn(func() func() {
		err := a.learner.Save(a.ctx, ts, text)
		return func() {
			if err != nil {
				slog.Warn("save learning", "ts", ts, "err", err)
				delete(a.learned, msg) // the approver can react again to retry
				// Still a pending request: its last line asks for a reaction again.
				update(fmt.Sprintf("React :%s: again to retry, saving failed (the agent's log says why), or :%s: to drop it.", s.ApprovalEmoji.Approve, s.ApprovalEmoji.Deny) + ref(ts, sig))
				return
			}
			slog.Info("learning saved", "ts", ts, "msg", msg, "by", ev.User)
			update(fmt.Sprintf("Saved to memory, approved by <@%s> at %s UTC", ev.User, time.Now().UTC().Format("15:04")))
		}
	})
}

// pendingLearning parses a request the agent posted whose last line still
// asks for a reaction: the task ts, the learning and the signature. It
// reverses codeBlock, whose body never holds a fence, so text outside the one
// block cannot ride along.
func pendingLearning(text string) (ts, learning, sig string, ok bool) {
	i := strings.LastIndex(text, "\n")
	if i < 0 {
		return
	}
	r := refLine.FindStringSubmatch(text[i+1:])
	if r == nil {
		return
	}
	head, rest, found := strings.Cut(text[:i], "\n```\n")
	if !found || !strings.HasPrefix(head, "<@") || !strings.HasSuffix(head, ">"+learnAsk) || strings.Contains(head, "\n") || !strings.HasSuffix(rest, "\n```") {
		return
	}
	code := strings.TrimSuffix(rest, "\n```")
	if strings.Contains(code, "```") {
		return
	}
	return r[1], unescape(strings.ReplaceAll(code, "``\u200b`", "```")), r[2], true
}
