package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nice-pink/itakeit-agent/pkg/worker"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// approval is one write waiting for the approver's reaction. It lives on the
// event loop, keyed by the approval message's ts, so only a reaction on that
// exact message can resolve it.
type approval struct {
	task, msg string // the task's ts, the approval message's ts
	text      string // the message without its last line, which says how it ended
	reply     chan worker.Decision
	done      bool
}

// approveFunc is what a round's worker calls for every write in fix mode. It
// runs on the worker's goroutine: the request goes to the loop, which posts it,
// and the answer comes back on reply.
func (a *Agent) approveFunc(ts string) worker.Approve {
	return func(ctx context.Context, r worker.ApprovalRequest) (worker.Decision, error) {
		reply := make(chan worker.Decision, 1)
		a.post(func() { a.requestApproval(ts, r, reply) })
		select {
		case d := <-reply:
			return d, nil
		case <-ctx.Done():
			a.post(func() { a.cancelApproval(reply, "Cancelled") })
			return worker.Decision{Reason: "cancelled"}, ctx.Err()
		}
	}
}

// requestApproval posts the write in the task thread before it runs: as a
// notice when writes need no approval, otherwise as an approval request for
// the approver to react to. A post that fails denies the write, so no write
// runs without its record in the thread.
func (a *Agent) requestApproval(ts string, r worker.ApprovalRequest, reply chan worker.Decision) {
	if a.jobs[ts] == nil {
		reply <- worker.Decision{Reason: "the task was deleted"}
		return
	}
	s := a.cfg.Agent
	head := fmt.Sprintf("(write %d of max %d)", r.Seq, worker.MaxWrites)
	code := codeBlock(r.Display)
	if r.Redacted {
		code = "Input contains redacted secrets.\n" + code
	}
	if s.Approver == "" {
		if _, err := a.postMsg(ts, fmt.Sprintf("Running `%s` %s:\n%s", escapeLiteral(r.Tool), head, code)); err != nil {
			reply <- worker.Decision{Reason: "could not record the change in the task thread"}
			return
		}
		reply <- worker.Decision{Allow: true}
		return
	}
	text := fmt.Sprintf("<@%s> approval needed %s. Tool: `%s`\n%s", s.Approver, head, escapeLiteral(r.Tool), code)
	last := fmt.Sprintf("React :%s: to run it or :%s: to deny. Expires in %d min.", s.ApprovalEmoji.Approve, s.ApprovalEmoji.Deny, s.ApprovalTimeoutMinutes)
	msg, err := a.postMsg(ts, text+"\n"+last)
	if err != nil {
		reply <- worker.Decision{Reason: "could not post the approval request"}
		return
	}
	ap := &approval{task: ts, msg: msg, text: text, reply: reply}
	a.approvals[msg] = ap
	for _, e := range []string{s.ApprovalEmoji.Approve, s.ApprovalEmoji.Deny} {
		if err := a.api.AddReaction(e, a.ref(msg)); err != nil { // offered for a click, not needed
			slog.Warn("add approval reaction", "msg", msg, "err", err)
		}
	}
	a.after(time.Duration(s.ApprovalTimeoutMinutes)*time.Minute, func() { a.timeoutApproval(ap) })
	slog.Info("approval requested", "ts", ts, "msg", msg, "tool", r.Tool, "seq", r.Seq)
}

// onReaction resolves an approval. Only the approver's user ID counts, only
// with the approve or deny emoji, and only on a pending approval message.
func (a *Agent) onReaction(ev *slackevents.ReactionAddedEvent) {
	ap := a.approvals[ev.Item.Timestamp]
	if ap == nil {
		return
	}
	s := a.cfg.Agent
	if ev.User != s.Approver {
		if ev.User != a.me {
			slog.Info("reaction on a pending approval ignored", "msg", ap.msg, "user", ev.User, "reaction", ev.Reaction)
		}
		return
	}
	switch ev.Reaction {
	case s.ApprovalEmoji.Approve:
		a.resolveApproval(ap, worker.Decision{Allow: true, By: ev.User}, fmt.Sprintf("Approved by <@%s> at %s UTC", ev.User, time.Now().UTC().Format("15:04")))
	case s.ApprovalEmoji.Deny:
		a.resolveApproval(ap, worker.Decision{Reason: "denied by the approver"}, fmt.Sprintf("Denied by <@%s>", ev.User))
	}
}

// timeoutApproval denies an approval nobody answered. It first reads the
// message's reactions, since a reaction event can be lost (a full event
// queue, a Socket Mode reconnect): the approver's reaction then still counts,
// and holding both emoji denies.
func (a *Agent) timeoutApproval(ap *approval) {
	if ap.done {
		return
	}
	s := a.cfg.Agent
	item, err := a.api.GetReactions(a.ref(ap.msg), slack.GetReactionsParameters{Full: true})
	if err != nil {
		slog.Warn("reactions of an approval", "msg", ap.msg, "err", err)
	}
	by := func(name string) bool {
		return slices.ContainsFunc(item.Reactions, func(r slack.ItemReaction) bool { return r.Name == name && slices.Contains(r.Users, s.Approver) })
	}
	switch {
	case by(s.ApprovalEmoji.Deny):
		a.resolveApproval(ap, worker.Decision{Reason: "denied by the approver"}, fmt.Sprintf("Denied by <@%s>", s.Approver))
	case by(s.ApprovalEmoji.Approve):
		a.resolveApproval(ap, worker.Decision{Allow: true, By: s.Approver}, fmt.Sprintf("Approved by <@%s>", s.Approver))
	default:
		a.resolveApproval(ap, worker.Decision{Reason: "the approval timed out"}, fmt.Sprintf("Timed out: no reaction from <@%s> within %d min", s.Approver, s.ApprovalTimeoutMinutes))
	}
}

// cancelApproval ends the approval that answers on reply, if it is pending.
func (a *Agent) cancelApproval(reply chan worker.Decision, label string) {
	for _, ap := range a.approvals {
		if ap.reply == reply {
			a.resolveApproval(ap, worker.Decision{Reason: "cancelled"}, label)
		}
	}
}

// resolveApproval answers the worker once and shows the outcome in place of
// the message's last line.
func (a *Agent) resolveApproval(ap *approval, d worker.Decision, label string) {
	if ap.done {
		return
	}
	ap.done = true
	delete(a.approvals, ap.msg)
	ap.reply <- d
	slog.Info("approval resolved", "ts", ap.task, "msg", ap.msg, "allow", d.Allow, "by", d.By, "outcome", label)
	if _, _, _, err := a.api.UpdateMessage(a.cfg.Channel, ap.msg, slack.MsgOptionText(ap.text+"\n"+label, false)); err != nil {
		slog.Warn("update approval", "msg", ap.msg, "err", err)
	}
}

// codeBlock shows a call's input as written: escaped without restoring
// references, so <@U123> in a command pings nobody, and with ``` broken up so
// the input cannot close the block and pose as the agent's own text.
func codeBlock(s string) string {
	return "```\n" + strings.ReplaceAll(escapeLiteral(s), "```", "``\u200b`") + "\n```"
}

// footer lists the writes that ran, from the worker's record of the stream
// rather than from the model's reply, so later rounds and people see them.
func footer(res worker.Result) string {
	if len(res.Actions) == 0 && !res.CapReached {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n*Actions taken:*")
	for i, act := range res.Actions {
		if i == worker.MaxWrites {
			fmt.Fprintf(&b, "\n…and %d more", len(res.Actions)-i)
			break
		}
		by := "allowed by config"
		switch {
		case act.Outcome == "unapproved":
			by = "run by the CLI without the agent's decision"
		case act.By != "":
			by = "approved by <@" + act.By + ">"
		}
		input := strings.ReplaceAll(strings.ReplaceAll(act.Input, "\n", " "), "`", "'")
		fmt.Fprintf(&b, "\n• `%s` `%s`: %s, %s", escapeLiteral(act.Tool), escapeLiteral(input), by, act.Outcome)
	}
	if res.CapReached {
		fmt.Fprintf(&b, "\nWrite limit of %d reached for this round. Mention me to continue.", worker.MaxWrites)
	}
	return cutEscaped(b.String(), 4000)
}
