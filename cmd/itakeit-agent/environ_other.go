//go:build !linux

package main

// hideEnviron has nothing to do where there is no /proc. On macOS a process of
// the same user can still read the agent's environment (ps eww), so run agents
// with tools in the Linux image.
func hideEnviron() error { return nil }

// subreaper has no equivalent off Linux: orphans go to launchd or init.
func subreaper() error { return nil }
