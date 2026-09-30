#!/bin/sh
# Starts itakeit and itakeit-agent side by side in one tmux window, both
# reading this repo's config.yaml (itakeit ignores the agent block).
#
# Each app gets its tokens from its own .env (gitignored, mode 600, KEY=value
# or export KEY=value lines, see .env.example):
#   ./.env:              AGENT_SLACK_BOT_TOKEN, AGENT_SLACK_APP_TOKEN, and for
#                        fix mode CLAUDE_CODE_OAUTH_TOKEN and KUBECONFIG. Also
#                        ITAKEIT_BIN, the itakeit binary, and ITAKEIT_DIR (see
#                        below). Relative paths are from this repo.
#   $ITAKEIT_DIR/.env:   SLACK_BOT_TOKEN, SLACK_APP_TOKEN
# Each window starts from an empty environment (env -i) plus HOME, PATH, USER,
# LOGNAME, LANG and TMPDIR, then sources its .env. Nothing else leaks in: the
# tmux server's global environment carries whatever the shell that started it
# had (other Claude Code sessions' CLAUDE_CODE_* variables, API keys), and the
# agent passes every CLAUDE_CODE_* variable on to the claude CLI. Anything else
# an app needs (proxies, NODE_EXTRA_CA_CERTS) goes into its .env. Tokens never
# appear in a command line.
#
# itakeit runs in ITAKEIT_DIR (default ../itakeit): its db_path (itakeit.db) is
# relative to the working directory, so that is where the database lives.
#
# Override: ITAKEIT_SESSION.
# Attach: tmux attach -t itakeit. Stop: tmux kill-session -t itakeit.
set -eu

die() { echo "$0: $*" >&2; exit 1; }

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
session=${ITAKEIT_SESSION:-itakeit}
config=$repo/config.yaml

# check_env FILE VAR... refuses a file others can read or write (it is sourced
# as shell code), one with an ACL (macOS: ls -l shows only @ for it), and one
# that does not set every VAR. It sources the file in an empty environment, so
# a variable exported in this shell does not count.
check_env() {
	f=$1
	shift
	[ -f "$f" ] || die "$f missing (see the header of this script)"
	[ -z "$(find -L "$f" -prune \( ! -user "$(id -u)" -o -perm -040 -o -perm -020 -o -perm -004 -o -perm -002 \))" ] ||
		die "$f must be yours and not readable or writable by others: chmod 600 $f"
	if [ "$(uname)" = Darwin ] && [ -n "$(ls -leL "$f" | sed -n 2p)" ]; then
		die "$f has an ACL: chmod -N $f"
	fi
	[ "$(ls -lL "$f" | cut -c11)" != + ] || die "$f has an ACL: setfacl -b $f"
	env -i PATH="$PATH" /bin/sh -c 'f=$1; shift; set -a; . "$f" || { echo "$0: sourcing $f failed" >&2; exit 1; }
		for v; do eval "[ -n \"\${$v:-}\" ]" || { echo "$0: $v is not set in $f" >&2; exit 1; }; done' "$0" "$f" "$@" >/dev/null || exit 1
}

# env_value FILE VAR prints VAR as FILE sets it, sourced in an empty
# environment. Call it only after check_env FILE.
env_value() {
	env -i PATH="$PATH" /bin/sh -c 'set -a; . "$1" >/dev/null; eval "printf %s \"\${$2:-}\""' sh "$1" "$2"
}

# abs PATH makes PATH absolute, relative to this repo.
abs() { case $1 in /*) printf %s "$1" ;; *) printf %s "$repo/$1" ;; esac; }

check_env "$repo/.env" AGENT_SLACK_BOT_TOKEN AGENT_SLACK_APP_TOKEN ITAKEIT_BIN
itakeit_bin=$(abs "$(env_value "$repo/.env" ITAKEIT_BIN)")
itakeit_dir=$(env_value "$repo/.env" ITAKEIT_DIR)
itakeit_dir=$(abs "${itakeit_dir:-../itakeit}")

missing=
for b in "$itakeit_bin" "$repo/bin/itakeit-agent"; do
	[ -f "$b" ] && [ -x "$b" ] || missing="$missing
  $b"
done
[ -z "$missing" ] || die "binaries missing or not executable (ITAKEIT_BIN in $repo/.env; ./build in each repo):$missing"
[ -d "$itakeit_dir" ] || die "itakeit working directory $itakeit_dir not found: set ITAKEIT_DIR in $repo/.env"
itakeit=$(CDPATH= cd -- "$itakeit_dir" && pwd)

# Older tmux joins a multi-argument command into one string for a shell.
case $(tmux -V) in "tmux "[012].*) die "tmux 3.0 or later needed, found $(tmux -V)" ;; esac
[ -f "$config" ] || die "$config missing: start from config.example.yaml"
check_env "$itakeit/.env" SLACK_BOT_TOKEN SLACK_APP_TOKEN
! tmux has-session -t "=$session" 2>/dev/null ||
	die "tmux session $session exists (its apps may have exited): tmux attach -t $session, or tmux kill-session -t $session"
# Two instances would share the Slack apps and itakeit.db.
for p in "$(basename -- "$itakeit_bin")" itakeit-agent; do
	! pgrep -x -u "$(id -u)" "$p" >/dev/null || die "$p already runs (pid $(pgrep -x -u "$(id -u)" "$p" | paste -sd ' ' -)): stop it first"
done

# A new tmux server would keep these in its global environment.
unset SLACK_BOT_TOKEN SLACK_APP_TOKEN AGENT_SLACK_BOT_TOKEN AGENT_SLACK_APP_TOKEN CLAUDE_CODE_OAUTH_TOKEN

# Several arguments after the options make tmux run the command directly,
# without a shell, so paths are never parsed as shell code. run is the pane's
# script: $1 the .env, then the command.
run='set -a; . "$1" || exit 1; set +a; shift; exec "$@"'
u=${USER:-$(id -un)}
set -- "HOME=$HOME" "PATH=$PATH" "USER=$u" "LOGNAME=${LOGNAME:-$u}" "LANG=${LANG:-en_US.UTF-8}" "TMPDIR=${TMPDIR:-/tmp}"
# remain-on-exit keeps a crashed app's last output on screen.
tmux new-session -d -s "$session" -n itakeit -c "$itakeit" \
	env -i "$@" /bin/sh -c "$run" sh "$itakeit/.env" "$itakeit_bin" -config "$config" \; \
	set-option -w remain-on-exit on \; \
	split-window -h -c "$repo" \
	env -i "$@" /bin/sh -c "$run" sh "$repo/.env" "$repo/bin/itakeit-agent" -config "$config" \; \
	select-pane -t :.0
echo "started tmux session $session (itakeit left, agent right): tmux attach -t $session"
