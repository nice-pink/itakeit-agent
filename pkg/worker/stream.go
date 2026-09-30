package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// maxLine bounds one stdout line of a stream session, so a runaway tool output
// cannot grow the agent's memory without limit. A var so tests can lower it.
var maxLine = 16 << 20

const (
	MaxWrites        = 20   // approval requests per round
	maxApprovalInput = 8000 // bytes of input an approver can review in Slack
)

// stream is one `claude -p` run in stream-json mode with tools. The CLI sends
// every permission request to this process (--permission-prompt-tool stdio),
// and decide answers it. Verified on CLI 2.1.284, and by the live tests on
// 2.1.285; see the spec's V1-V11.
type stream struct {
	bin, model, cwd, sysFile, schema, effort, user string
	env                                            []string
	tools                                          []string // the built-ins exposed, which the init line must list
	extra                                          []string // flags that expose them (Tools.argv)
	timeout                                        time.Duration
	decide                                         func(tool string, input json.RawMessage) (Verdict, string)
	approve                                        Approve  // asked about every write; nil denies them all
	mcpServers                                     []string // configured MCP servers, whose tools the init line may list
}

// streamResult is what a session did besides its answer.
type streamResult struct {
	requests   int               // permission requests that arrived
	actions    []Action          // writes the host allowed, with their outcome
	capReached bool              // the write cap denied a call
	initTools  []string          // what the CLI said the session has
	mcpStatus  map[string]string // MCP server: status, from the init line
}

// streamMsg is the union of the stream lines the host reads.
type streamMsg struct {
	Type           string     `json:"type"`
	Subtype        string     `json:"subtype"`
	PermissionMode string     `json:"permissionMode"` // system/init
	Tools          []string   `json:"tools"`          // system/init
	MCPServers     []struct { // system/init
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	RequestID string         `json:"request_id"` // control_request
	Request   controlRequest `json:"request"`    // control_request
	Message   struct {       // assistant, user
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	IsError          bool            `json:"is_error"` // result
	StopReason       string          `json:"stop_reason"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

type controlRequest struct {
	Subtype   string          `json:"subtype"`
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`   // tool_use
	Name      string          `json:"name"` // tool_use
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"` // tool_result: a string or text blocks
}

// session is the state of one run. Requests are handled on their own
// goroutines, since an approval can wait for minutes while the model's other
// calls go on (V10), and lines to the CLI go through one writer goroutine, so
// the read loop never blocks on a full stdin pipe.
type session struct {
	*stream
	ctx   context.Context // done when the run ends: pending approvals are cancelled
	lines chan any        // to the writer
	clock *clock
	wg    sync.WaitGroup // request handlers

	mu       sync.Mutex // guards the fields below
	decided  map[string]bool
	actionAt map[string]int                // tool_use_id: index in res.actions
	pending  map[string]context.CancelFunc // tool_use_id: cancels its approval wait
	resulted map[string]bool               // tool_use_id: its result arrived
	asked    int
	res      streamResult
}

// run returns what the session did, and the error of the run or of parsing
// the answer into dest. Actions come back with an error too.
func (s *stream) run(ctx context.Context, dest any) (streamResult, error) {
	// Every kill goes through runCtx, so cmd.Cancel does it, which exec never
	// calls after the process is reaped: a kill cannot hit a reused PID.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var timedOut atomic.Bool
	clk := newClock(s.timeout, func() { timedOut.Store(true); stop() })
	defer clk.stop()
	args := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--no-session-persistence", "--strict-mcp-config", "--setting-sources", "", "--restricted",
		"--permission-mode", "default", "--permission-prompt-tool", "stdio"}, s.extra...)
	args = append(args, "--model", s.model, "--effort", s.effort, "--system-prompt-file", s.sysFile, "--json-schema", s.schema)
	cmd := exec.CommandContext(runCtx, s.bin, args...)
	cmd.Dir, cmd.Env = s.cwd, s.env
	// SIGTERM to the whole tree first; exec sends SIGKILL to the CLI after
	// WaitDelay, safely, and orphans left after it are reaped (reapOrphans).
	cmd.WaitDelay = 5 * time.Second
	ownGroup(cmd)
	cmd.Cancel = func() error { killTree(cmd.Process.Pid, syscall.SIGTERM); return nil }
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return streamResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return streamResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := startTracked(cmd); err != nil {
		return streamResult{}, fmt.Errorf("claude: %w", err)
	}
	defer reapOrphans()
	defer untrack(cmd)

	sessCtx, endSession := context.WithCancel(runCtx)
	ss := &session{stream: s, ctx: sessCtx, lines: make(chan any, 64), clock: clk,
		decided: map[string]bool{}, actionAt: map[string]int{}, pending: map[string]context.CancelFunc{}, resulted: map[string]bool{}}
	written := make(chan struct{})
	go func() {
		enc := json.NewEncoder(stdin)
		for v := range ss.lines {
			enc.Encode(v) // an error means the CLI is gone, which Wait reports
		}
		close(written)
	}()
	runErr := ss.loop(stdout, dest)
	clk.stop() // the result is in: the drain below is not the round's work
	if runErr != nil {
		stop()
	}
	endSession()
	ss.wg.Wait() // no handler touches the session after this
	close(ss.lines)
	stdin.Close() // also unblocks a write the CLI stopped reading
	<-written
	// After its result the CLI exits once stdin closes; one that does not is killed
	// rather than left to hold a session until the timeout.
	stuck := time.AfterFunc(10*time.Second, stop)
	defer stuck.Stop()
	waitErr := cmd.Wait()
	res := ss.result()
	switch {
	case timedOut.Load():
		return res, fmt.Errorf("claude: timed out after %s of work", s.timeout)
	case ctx.Err() != nil:
		return res, fmt.Errorf("claude: %w", ctx.Err()) // shutting down, or the task was deleted
	case runErr == errNoResult && waitErr != nil:
		return res, fmt.Errorf("claude: %w: %s", waitErr, firstLine(stderr.String()))
	}
	return res, runErr
}

var errNoResult = errors.New("claude: exited without a result")

func (ss *session) result() streamResult {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return streamResult{requests: ss.res.requests, actions: slices.Clone(ss.res.actions), capReached: ss.res.capReached, initTools: ss.res.initTools, mcpStatus: ss.res.mcpStatus}
}

func (ss *session) send(v any) {
	select {
	case ss.lines <- v:
	case <-ss.ctx.Done():
	}
}

func (ss *session) loop(out io.Reader, dest any) error {
	ss.send(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": ss.user}})
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, min(64<<10, maxLine)), maxLine) // the limit is the larger of the two
	var initTools []string                                    // nil until the init line
	names := map[string]string{}
	for sc.Scan() {
		var m streamMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "system":
			if m.Subtype != "init" {
				continue
			}
			if err := ss.checkInit(m); err != nil {
				return err
			}
			initTools = m.Tools
			ss.mu.Lock()
			ss.res.initTools = slices.Clone(m.Tools)
			ss.res.mcpStatus = map[string]string{}
			for _, srv := range m.MCPServers {
				ss.res.mcpStatus[srv.Name] = srv.Status
				if srv.Status != "connected" {
					slog.Warn("MCP server not connected: its tools are missing this round", "server", srv.Name, "status", srv.Status)
				}
			}
			ss.mu.Unlock()
		case "control_request":
			if initTools == nil {
				return errors.New("claude: permission request before the init line")
			}
			if m.Request.Subtype != "can_use_tool" {
				// Answered so the CLI does not wait on it; the live test checks the shape.
				ss.send(map[string]any{"type": "control_response", "response": map[string]string{"subtype": "error", "request_id": m.RequestID, "error": "unsupported"}})
				continue
			}
			ss.mu.Lock()
			ss.res.requests++
			ss.mu.Unlock()
			ss.wg.Add(1)
			go ss.handle(m.RequestID, m.Request)
		case "assistant", "user":
			var blocks []contentBlock
			if json.Unmarshal(m.Message.Content, &blocks) != nil {
				continue // plain text content
			}
			for _, b := range blocks {
				switch b.Type {
				case "tool_use":
					names[b.ID] = b.Name
				case "tool_result":
					if err := ss.onResult(names[b.ToolUseID], b, initTools); err != nil {
						return err
					}
				}
			}
		case "result":
			res := cliResult{IsError: m.IsError, Subtype: m.Subtype, StopReason: m.StopReason, Result: m.Result, StructuredOutput: m.StructuredOutput}
			return res.answer(dest)
		}
	}
	if errors.Is(sc.Err(), bufio.ErrTooLong) {
		return fmt.Errorf("claude: a stream line exceeded %d bytes", maxLine)
	}
	if ss.ctx.Err() != nil {
		return ss.ctx.Err()
	}
	return errNoResult
}

// onResult records a write's outcome, cancels an approval the CLI stopped
// waiting for, and stops the session on a call the host never decided.
func (ss *session) onResult(name string, b contentBlock, initTools []string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.resulted[b.ToolUseID] = true
	if cancel, ok := ss.pending[b.ToolUseID]; ok {
		cancel() // the CLI moved on without the answer
	}
	if i, ok := ss.actionAt[b.ToolUseID]; ok {
		ss.res.actions[i].Outcome = map[bool]string{false: "ran", true: "failed"}[b.IsError]
	}
	if !unapproved(name, b, ss.decided[b.ToolUseID], initTools) {
		return nil
	}
	ss.res.actions = append(ss.res.actions, Action{Tool: toolLabel(name), Outcome: "unapproved"})
	return fmt.Errorf("claude: %s ran without a decision by the agent, so the session was stopped", toolLabel(name))
}

// handle answers one permission request.
func (ss *session) handle(id string, r controlRequest) {
	defer ss.wg.Done()
	verdict, reason := ss.decide(r.ToolName, r.Input)
	switch verdict {
	case Allow:
		ss.answer(id, r)
	case Ask:
		ss.ask(id, r)
	default:
		ss.deny(id, r, reason)
	}
}

// ask gets a write approved, within the round's limits. The clock stops while
// it waits, so a slow approver does not eat the round's working time.
func (ss *session) ask(id string, r controlRequest) {
	if len(r.Input) > maxApprovalInput {
		ss.deny(id, r, "input too large to review in Slack (over 8,000 bytes): split the change")
		return
	}
	ss.mu.Lock()
	if ss.asked >= MaxWrites {
		ss.res.capReached = true
		ss.mu.Unlock()
		ss.deny(id, r, fmt.Sprintf("write limit of %d reached for this round: stop and report what is left", MaxWrites))
		return
	}
	ss.asked++
	seq := ss.asked
	ctx, cancel := context.WithCancel(ss.ctx)
	ss.pending[r.ToolUseID] = cancel
	if ss.resulted[r.ToolUseID] {
		cancel() // the CLI moved on before this handler ran
	}
	ss.mu.Unlock()
	defer func() {
		ss.mu.Lock()
		delete(ss.pending, r.ToolUseID)
		ss.mu.Unlock()
		cancel()
	}()
	if ss.approve == nil {
		ss.deny(id, r, "writes are not possible in this round")
		return
	}
	ss.clock.pause()
	d, err := ss.approve(ctx, ApprovalRequest{Tool: r.ToolName, Input: r.Input, Seq: seq})
	ss.clock.resume()
	switch {
	case err != nil:
		ss.deny(id, r, "not approved: "+err.Error())
		return
	case ctx.Err() != nil:
		return // the CLI stopped waiting: nothing runs, so nothing is recorded
	case !d.Allow:
		ss.deny(id, r, cmpOr(d.Reason, "not approved"))
		return
	}
	// Checked again: a parallel write can have swapped a symlink in meanwhile.
	if verdict, reason := ss.decide(r.ToolName, r.Input); verdict == Deny {
		ss.deny(id, r, reason)
		return
	}
	ss.mu.Lock()
	ss.actionAt[r.ToolUseID] = len(ss.res.actions)
	ss.res.actions = append(ss.res.actions, Action{Tool: r.ToolName, Input: display(r.ToolName, r.Input), By: d.By, Outcome: "unknown"})
	ss.mu.Unlock()
	ss.answer(id, r)
}

func (ss *session) answer(id string, r controlRequest) {
	ss.mark(r)
	ss.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id,
		"response": map[string]any{"behavior": "allow", "updatedInput": r.Input}}})
}

func (ss *session) deny(id string, r controlRequest, reason string) {
	ss.mark(r)
	ss.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id,
		"response": map[string]any{"behavior": "deny", "message": reason}}})
}

// mark records the decision before it is sent, so the call's result can
// never arrive first.
func (ss *session) mark(r controlRequest) {
	ss.mu.Lock()
	ss.decided[r.ToolUseID] = true
	ss.mu.Unlock()
}

// display is what an approver and the footer see of a call: a Bash call's
// command (and timeout), never the model's description of it, or the input.
func display(tool string, input json.RawMessage) string {
	if tool == "Bash" {
		var in struct {
			Command string `json:"command"`
			Timeout *int   `json:"timeout"`
		}
		if json.Unmarshal(input, &in) == nil {
			if in.Timeout != nil {
				return fmt.Sprintf("%s\n(timeout %d ms)", in.Command, *in.Timeout)
			}
			return in.Command
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, input) != nil {
		return string(input)
	}
	return compact.String()
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// clock is a deadline that stands still while at least one approval is
// pending, so work_timeout_minutes counts the round's own work only.
type clock struct {
	mu      sync.Mutex
	left    time.Duration
	since   time.Time
	paused  int
	stopped bool
	t       *time.Timer
	fire    func()
}

func newClock(d time.Duration, fire func()) *clock {
	return &clock{left: d, since: time.Now(), fire: fire, t: time.AfterFunc(d, fire)}
}

func (c *clock) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused++; c.paused == 1 && c.t.Stop() {
		c.left -= time.Since(c.since)
	}
}

func (c *clock) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused--; c.paused == 0 && !c.stopped {
		c.since = time.Now()
		c.t = time.AfterFunc(max(c.left, 0), c.fire)
	}
}

func (c *clock) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	c.t.Stop()
}

// checkInit stops a session the CLI started differently than asked: a
// permission mode other than default (a managed setting could auto-allow
// calls), or built-in tools other than the exposed ones plus StructuredOutput,
// which carries the answer and never prompts (V9). MCP tool names are not
// known in advance, so those must only belong to a configured server; the
// resource tools may show up only with servers configured.
func (s *stream) checkInit(m streamMsg) error {
	want := append(slices.Clone(s.tools), "StructuredOutput")
	var got []string
	for _, name := range m.Tools {
		switch {
		case isMCP(name):
			if !slices.ContainsFunc(s.mcpServers, func(srv string) bool { return strings.HasPrefix(name, "mcp__"+srv+"__") }) {
				return fmt.Errorf("claude: session has MCP tool %s from a server that is not configured", name)
			}
		case slices.Contains(resourceTools, name) && len(s.mcpServers) > 0:
		default:
			got = append(got, name)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if m.PermissionMode != "default" || !slices.Equal(got, want) {
		return fmt.Errorf("claude: session started with permission mode %q and tools %v, want default and %v", m.PermissionMode, m.Tools, want)
	}
	return nil
}

// unapproved is the backstop behind the ask rules: a tool result the host
// never decided means the CLI ran a call on its own. It fires after the call
// ran, so it limits the damage to one call rather than preventing it. Exempt
// are the structured answer, tools the session does not have, the CLI's own
// rejections before anything runs, and file-tool errors, which --restricted
// raises without asking the host (V6).
func unapproved(name string, b contentBlock, decided bool, initTools []string) bool {
	text := resultText(b.Content)
	switch {
	case decided || name == "StructuredOutput":
		return false
	case name != "" && !slices.Contains(initTools, name):
		return false
	case cliRejection(text):
		return false
	case slices.Contains(fileTools, name) && b.IsError:
		return false
	}
	return true
}

// cliRejections open the results the CLI writes when it refuses a call before
// anything runs (strings in the 2.1.284 and 2.1.285 binaries). "Error calling tool" is not
// one: it wraps an exception thrown after the call started.
var cliRejections = []string{"<tool_use_error>InputValidationError", "<tool_use_error>Error: No such tool available",
	"<tool_use_error>Permission to use", "<tool_use_error>Blocked: "}

func cliRejection(text string) bool {
	return slices.ContainsFunc(cliRejections, func(p string) bool { return strings.HasPrefix(text, p) })
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &blocks)
	var b strings.Builder
	for _, x := range blocks {
		b.WriteString(x.Text)
	}
	return strings.TrimSpace(b.String())
}

func toolLabel(name string) string {
	if name == "" {
		return "a tool call"
	}
	return name
}
