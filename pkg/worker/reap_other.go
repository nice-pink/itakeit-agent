//go:build !linux

package worker

// reapOrphans has nothing to do off Linux: orphans go to launchd or init,
// which reap them. Agents with tools run in the Linux image.
func reapOrphans() {}
