package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// key names a message across channels: the channel ID and the message ts,
// joined by a dash. Channel IDs have no dash and a ts has none either, and the
// key is also a task's directory name (worker.Task.ID), so it stays a plain
// file name.
func key(channel, ts string) string { return channel + "-" + ts }

// split undoes key.
func split(k string) (channel, ts string) {
	channel, ts, _ = strings.Cut(k, "-")
	return channel, ts
}

// relistEvery is how often auto_channels lists the agent's channels again, to
// catch a join or leave whose event was lost. itakeit re-lists at most this
// often (more often with a short stale_after_hours).
const relistEvery = 15 * time.Minute

// serves reports whether events in the channel are the agent's: a configured
// channel, or with auto_channels one the agent is a member of.
func (a *Agent) serves(channel string) bool {
	if a.cfg.AutoChannels {
		return a.joined[channel]
	}
	return slices.Contains(a.cfg.Channels, channel)
}

// channels are the channels the agent serves now, sorted.
func (a *Agent) channels() []string {
	if a.cfg.AutoChannels {
		return slices.Sorted(maps.Keys(a.joined))
	}
	return a.cfg.Channels
}

// memberOf lists the public and private channels the agent's bot is a member
// of, archived ones left out.
func memberOf(api API) ([]string, error) {
	var out []string
	p := &slack.GetConversationsForUserParameters{Types: []string{"public_channel", "private_channel"}, ExcludeArchived: true, Limit: 200}
	for {
		chans, next, err := api.GetConversationsForUser(p)
		if err != nil {
			return nil, err
		}
		for _, c := range chans {
			out = append(out, c.ID)
		}
		if next == "" {
			return out, nil
		}
		p.Cursor = next
	}
}

// relist brings auto_channels' channels up to date: a new one is served from
// now on and its owned tasks recovered, and one the agent left drops its jobs.
// A listing that fails keeps the channels as they were.
func (a *Agent) relist() {
	if !a.cfg.AutoChannels {
		return
	}
	chans, err := memberOf(a.api)
	if err != nil {
		slog.Warn("list channels", "err", err)
		return
	}
	for _, ch := range chans {
		a.onJoin(ch, "re-invited")
	}
	for _, ch := range a.channels() {
		// Left, or itakeit's bot left and that event was lost. A failed member
		// read keeps the channel: a rate limit must not drop its tasks.
		if in, ok := a.itakeitIn(ch); !slices.Contains(chans, ch) || (ok && !in) {
			a.onLeave(ch)
		}
	}
}

// leftChannel is forget's reason when the agent lost a channel.
const leftChannel = "the agent left the channel"

// onJoin starts serving a channel the agent was added to (or that was
// unarchived), and recovers what it owns there: after a rejoin that can be
// tasks it took before it left. A configured channel is served anyway; with
// auto_channels a channel is served only while the agent is a member and,
// when agent.itakeit_user is set, itakeit's bot is too.
//
// why is what the recovered threads are told interrupted them.
func (a *Agent) onJoin(channel, why string) {
	if !a.cfg.AutoChannels {
		if slices.Contains(a.cfg.Channels, channel) {
			a.recoverChannel(channel, why)
		}
		return
	}
	if a.joined[channel] {
		return
	}
	if in, _ := a.itakeitIn(channel); !in {
		return
	}
	a.joined[channel] = true
	slog.Info("serving channel", "channel", channel)
	a.recoverChannel(channel, why)
}

// itakeitIn reports whether itakeit's bot is a member of the channel, and ok
// false when the members could not be read (in is then false too, so a new
// channel waits for the next listing). Without agent.itakeit_user it cannot
// tell and says yes.
func (a *Agent) itakeitIn(channel string) (in, ok bool) {
	u := a.cfg.Agent.ItakeitUser
	if u == "" {
		return true, true
	}
	p := &slack.GetUsersInConversationParameters{ChannelID: channel, Limit: 1000}
	for {
		users, next, err := a.api.GetUsersInConversation(p)
		if err != nil {
			slog.Warn("channel members", "channel", channel, "err", err)
			return false, false
		}
		if slices.Contains(users, u) {
			return true, true
		}
		if next == "" {
			slog.Info("not serving channel: itakeit's bot is not in it", "channel", channel)
			return false, true
		}
		p.Cursor = next
	}
}

// onLeave drops the jobs in a channel the agent can no longer work in: it was
// removed, or the channel was archived. With auto_channels the channel is no
// longer served; a configured channel stays served, so a new invite resumes it.
func (a *Agent) onLeave(channel string) {
	for k := range a.jobs {
		if ch, _ := split(k); ch == channel {
			a.forget(k, leftChannel)
		}
	}
	for k := range a.queued {
		if ch, _ := split(k); ch == channel {
			delete(a.queued, k)
		}
	}
	if a.cfg.AutoChannels {
		if !a.joined[channel] {
			return
		}
		delete(a.joined, channel)
		slog.Info("stopped serving channel", "channel", channel)
		return
	}
	if slices.Contains(a.cfg.Channels, channel) {
		slog.Warn("lost a configured channel (left or archived): its tasks are dropped, and recovered when the agent is back", "channel", channel)
	}
}

// CheckChannels stops a setup the agent cannot work with at startup: a
// configured channel it cannot read, or with auto_channels a channel listing
// that fails. An agent in no channel yet starts, and serves channels as it is
// invited.
func CheckChannels(api API, cfg *Config) error {
	if !cfg.AutoChannels {
		for _, ch := range cfg.Channels {
			if err := CheckChannel(api, ch); err != nil {
				return err
			}
		}
		return nil
	}
	chans, err := memberOf(api)
	switch {
	case err != nil && err.Error() == "missing_scope":
		return errors.New("listing channels: missing_scope: auto_channels needs channels:read and groups:read, see slack-app-manifest.yaml")
	case err != nil:
		return fmt.Errorf("listing channels: %w", err)
	case len(chans) == 0:
		slog.Warn("auto_channels: the agent is in no channel yet; invite it to itakeit's channels")
	}
	for _, ch := range chans {
		if err := CheckChannel(api, ch); err != nil {
			return err
		}
	}
	return nil
}
