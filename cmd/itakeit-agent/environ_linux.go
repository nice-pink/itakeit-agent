package main

import "syscall"

// hideEnviron makes this process non-dumpable, so /proc/<pid>/environ, which
// holds the Slack tokens, is unreadable to processes of the same user: a tool
// the claude CLI runs could otherwise read the tokens there. Starting the CLI
// resets the flag for it, and the CLI gets no Slack tokens anyway.
func hideEnviron() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER from linux/prctl.h, which
// the syscall package defines for some architectures only (not amd64).
const prSetChildSubreaper = 0x24

// subreaper makes the agent the parent of processes a tool session leaves
// behind when its CLI exits, so the worker can kill and reap them. In the
// image the agent is PID 1, which is that parent already.
func subreaper() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return errno
	}
	return nil
}
