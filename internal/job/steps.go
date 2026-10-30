package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/text"
)

// runTimeout is how long a workflow command may run (the Bash tool's max).
const runTimeout = 600

// failureKey holds a failed command's output for the next agent step.
const failureKey = "FAILURE"

// runWorkflow runs one round of a workflow job: from the saved step (after
// a restart) or the first one, until a step leads to done (the job is done;
// a repeating job only waits) or back to the first step (the round is over). The step, values and runs
// are saved before each step, so a restart runs that step again.
func (s *Scheduler) runWorkflow(ctx context.Context, j api.Job) round {
	w, err := LoadWorkflow(ListWorkflowDirs(s.opts.Workflows, j.Dir), j.Workflow)
	if err != nil {
		return round{failure: "workflow " + j.Workflow + ": " + err.Error(), fatal: true}
	}
	first := w.Steps[0].ID
	r := wfRound{s: s, j: j}
	step := j.Step
	if step == "" { // a new round: a new session, fresh values
		r.prev = j.SessionID
		j.Values, j.Visits, j.Stall = map[string]string{}, map[string]int{}, api.StallState{}
		if j.Prompt != "" {
			j.Values["GOAL"] = j.Prompt
		}
		step = first
	} else {
		r.cur = j.SessionID
		if j.Values == nil {
			j.Values = map[string]string{}
		}
		if j.Visits == nil {
			j.Visits = map[string]int{}
		}
	}
	r.j = j
	for {
		st, ok := w.step(step)
		if !ok {
			return round{failure: fmt.Sprintf("workflow %s has no step %q any more", j.Workflow, step), fatal: true}
		}
		r.j.Visits[step]++
		if r.j.Visits[step] > st.tries() {
			return r.end(round{failure: fmt.Sprintf("step %s ran %d times this round", step, st.tries())})
		}
		if r.isStalled(ctx, step, w.stallAfter()) {
			if res, ok := r.recover(ctx, step); !ok {
				return r.end(res)
			}
		}
		if err := r.save(ctx, step); err != nil {
			return round{failure: err.Error()}
		}
		next, res, ok := r.runStep(ctx, st)
		if ctx.Err() != nil {
			return round{} // stopped: the saved step runs again
		}
		if !ok {
			return r.end(res)
		}
		switch next {
		case stepDone:
			return r.end(round{done: true})
		case first:
			return r.end(round{})
		}
		step = next
	}
}

// wfRound is one workflow round in progress.
type wfRound struct {
	s    *Scheduler
	j    api.Job // Values and Visits change as the round goes
	prev string  // the round before's session: the new session works in its folder
	cur  string  // this round's session; "" until the first agent step
}

// save records the step about to run.
func (r *wfRound) save(ctx context.Context, step string) error {
	values, visits, cur := maps.Clone(r.j.Values), maps.Clone(r.j.Visits), r.cur
	stall := api.StallState{Steps: maps.Clone(r.j.Stall.Steps), Recoveries: r.j.Stall.Recoveries}
	_, err := r.s.update(ctx, r.j.ID, func(j *api.Job) error {
		j.Step, j.Values, j.Visits, j.Stall = step, values, visits, stall
		if cur != "" {
			j.SessionID = cur
		}
		return nil
	})
	return err
}

// end clears the step: the next round starts at the first one.
func (r *wfRound) end(res round) round {
	if _, err := r.s.update(context.Background(), r.j.ID, func(j *api.Job) error {
		j.Step, j.Visits, j.Stall = "", nil, api.StallState{}
		return nil
	}); err != nil {
		return round{failure: err.Error()}
	}
	return res
}

// maxRecoveries is how often a round may change its approach before it
// counts as blocked.
const maxRecoveries = 2

// Tokens of the recovery step.
const (
	tokenRetry   = "RETRY"
	tokenBlocked = "BLOCKED"
)

// stateCommand prints a checksum of the round folder's work: HEAD, changed
// and new files and the diff. PROGRESS.md is left out: notes are not
// progress. Hashed in the shell, so a big diff stays one short line.
const stateCommand = `{ git rev-parse HEAD; git status --porcelain -- . ':(exclude)PROGRESS.md'; git diff HEAD -- . ':(exclude)PROGRESS.md'; } 2>/dev/null | cksum`

// isStalled records the state the step starts in and reports whether it
// started after times repeats with nothing changed: the same files and the
// same values (command output and failures left out, their timings vary).
func (r *wfRound) isStalled(ctx context.Context, step string, times int) bool {
	if times == 0 {
		return false
	}
	state, ok := r.bash(ctx, stateCommand)
	if !ok {
		slog.Warn("workflow state check failed: file changes are not seen", "job", r.j.ID, "session", r.session(), "output", state)
	}
	sum := sha256.Sum256([]byte(state + "\x00" + strings.Join(r.results(), "\x00")))
	fp := hex.EncodeToString(sum[:8])
	if r.j.Stall.Steps == nil {
		r.j.Stall.Steps = map[string]api.Stall{}
	}
	st := r.j.Stall.Steps[step]
	if st.Fingerprint == fp {
		st.Repeats++
	} else {
		st = api.Stall{Fingerprint: fp}
	}
	r.j.Stall.Steps[step] = st
	return st.Repeats >= times
}

// results lists the round's values from replies, sorted, as "KEY: value".
// Command output and failures are left out: their timings vary.
func (r *wfRound) results() []string {
	var out []string
	for k, v := range r.j.Values {
		if k != "OUTPUT" && k != failureKey {
			out = append(out, k+": "+v)
		}
	}
	slices.Sort(out)
	return out
}

// recover asks the agent to change its approach after steps repeated with
// nothing changed, or to name what blocks it. ok false ends the round with
// res: blocked, or failed.
func (r *wfRound) recover(ctx context.Context, step string) (res round, ok bool) {
	r.j.Stall.Recoveries++
	if r.j.Stall.Recoveries > maxRecoveries {
		return round{blocked: fmt.Sprintf("no progress: steps keep repeating with nothing changed after %d new approaches", maxRecoveries)}, false
	}
	var repeated []string
	for id, st := range r.j.Stall.Steps {
		if st.Repeats > 0 {
			repeated = append(repeated, fmt.Sprintf("%s (%d times)", id, st.Repeats+1))
		}
	}
	slices.Sort(repeated)
	results := strings.Join(r.results(), "\n")
	if results == "" {
		results = "none"
	}
	prompt := "No progress: these steps ran again with no change to the files or the results: " + strings.Join(repeated, ", ") + ".\n" +
		"Results so far:\n" + results + "\n\n" +
		"Do not repeat the same checks. Either change your approach to what is still open (say how in one sentence, then do it), " +
		"or, if something outside your control blocks you (a decision only the user can make, a missing permission, a broken service), say exactly what. " +
		"Messages from this job are not the user's answer to a question you asked. Do not bypass reviews, permissions or branch protections.\n" +
		"Use RESULT: RETRY <your new approach> or RESULT: BLOCKED <what blocks you>."
	st := Step{ID: "recover", Agent: prompt, Next: Next{ByToken: map[string]string{tokenRetry: step, tokenBlocked: stepDone}}}
	next, res, ok := r.runAgent(ctx, st)
	switch {
	case !ok:
		return res, false
	case next == stepDone:
		reason := r.j.Values[tokenBlocked]
		if reason == "" {
			reason = "the agent named no reason; see its session"
		}
		return round{blocked: reason}, false
	}
	r.j.Stall.Steps = nil // a new approach starts counting again
	return round{}, true
}

// runStep runs one step and says where to go next. ok false ends the round
// with res.
func (r *wfRound) runStep(ctx context.Context, st Step) (next string, res round, ok bool) {
	if st.Agent != "" {
		return r.runAgent(ctx, st)
	}
	out, cmdOK := r.runCommand(ctx, st)
	r.j.Values["OUTPUT"] = out
	if cmdOK {
		return st.Next.To, round{}, true
	}
	if st.OnFail == "" {
		return "", round{failure: fmt.Sprintf("step %s failed: %s", st.ID, out)}, false
	}
	r.j.Values[failureKey] = fmt.Sprintf("Step %s (`%s`) failed:\n```\n%s\n```", st.ID, st.Run, out)
	return st.OnFail, round{}, true
}

// runAgent sends the step's prompt to the round's session and reads where
// the reply leads.
func (r *wfRound) runAgent(ctx context.Context, st Step) (string, round, bool) {
	text := fmt.Sprintf("Job %s, round %d, step %s.\n\n%s", r.j.Name, r.j.Runs, st.ID, fill(st.Agent, r.j.Values))
	if f := r.j.Values[failureKey]; f != "" {
		text += "\n\n" + f
		delete(r.j.Values, failureKey)
	}
	if len(st.Next.ByToken) > 0 {
		text += "\n\n" + resultAsk(st)
	}
	reply, failure := r.send(ctx, text)
	if failure != "" {
		return "", round{failure: fmt.Sprintf("step %s: %s", st.ID, failure)}, false
	}
	if st.Next.To != "" {
		return st.Next.To, round{}, true
	}
	tok, value, found := parseResult(reply)
	if _, known := st.Next.ByToken[tok]; !found || !known {
		// Ask once more.
		reply, failure = r.send(ctx, "Your reply must end with the RESULT line. "+resultAsk(st))
		tok, value, found = parseResult(reply)
		if _, known := st.Next.ByToken[tok]; failure != "" || !found || !known {
			return "", round{failure: fmt.Sprintf("step %s: no RESULT line with one of %s", st.ID, strings.Join(st.tokens(), ", "))}, false
		}
	}
	r.j.Values[tok] = value
	return st.Next.ByToken[tok], round{}, true
}

// resultAsk tells the agent how to end its reply.
func resultAsk(st Step) string {
	return fmt.Sprintf("End your reply with one line: RESULT: <TOKEN> [value], where TOKEN is one of %s. Add the value only when the step asks for one.",
		strings.Join(st.tokens(), ", "))
}

// send sends text to the round's session (the first one starts it) and
// waits for the reply.
func (r *wfRound) send(ctx context.Context, text string) (reply, failure string) {
	if r.cur == "" {
		j := r.j
		j.SessionID = r.prev
		id, err := r.s.sendRound(ctx, j, text)
		if err != nil {
			return "", err.Error()
		}
		r.cur = id
		return r.s.waitIdle(ctx, id, 0)
	}
	after, err := r.lastSeq(ctx)
	if err != nil {
		return "", err.Error()
	}
	if _, err := r.s.sessions.SendMessage(ctx, api.MessageRequest{SessionID: r.cur, Text: text}); err != nil {
		return "", err.Error()
	}
	return r.s.waitIdle(ctx, r.cur, after)
}

// lastSeq is the round session's last event, so a wait sees only the new
// turn.
func (r *wfRound) lastSeq(ctx context.Context) (int64, error) {
	events, err := api.ListAllEvents(ctx, r.s.sessions, api.EventFilter{SessionID: r.cur})
	if err != nil || len(events) == 0 {
		return 0, err
	}
	return events[len(events)-1].Seq, nil
}

// session is the session whose folder the round works in.
func (r *wfRound) session() string {
	if r.cur != "" {
		return r.cur
	}
	return r.prev
}

// runCommand runs a run step: a built-in or a shell command (values are
// quoted for the shell). It reports the output and whether it worked.
func (r *wfRound) runCommand(ctx context.Context, st Step) (string, bool) {
	run := strings.TrimSpace(st.Run)
	name, rest, _ := strings.Cut(run, " ")
	switch {
	case run == builtinMerge:
		return r.merge(ctx)
	case run == builtinCleanup:
		id := r.session()
		if id == "" {
			return "nothing to clean up", true
		}
		if err := r.s.sessions.UpdateSession(ctx, api.SessionUpdate{ID: id, LeaveWorktree: true}); err != nil {
			return err.Error(), false
		}
		return "worktree removed", true
	case name == builtinNotify:
		text := fill(strings.TrimSpace(rest), r.j.Values)
		if u, err := strconv.Unquote(text); err == nil {
			text = u
		}
		r.s.notify(r.session(), "job "+r.j.Name+": "+text)
		return "sent", true
	}
	return r.bash(ctx, fillCommand(run, r.j.Values))
}

// merge rebases the round's worktree branch onto the project's current
// branch and fast-forwards that branch. The worktree stays for the next
// round. A conflict aborts the rebase and fails the step.
func (r *wfRound) merge(ctx context.Context) (string, bool) {
	id := r.session()
	if id == "" {
		return "nothing to merge yet", true
	}
	sess, err := r.s.sessions.GetSession(ctx, id)
	if err != nil {
		return err.Error(), false
	}
	if sess.Branch == "" {
		return "nothing to merge: the job works in the project folder", true
	}
	project := text.QuoteShell(sess.Project)
	script := fmt.Sprintf(`set -e
main=$(git -C %[1]s rev-parse --abbrev-ref HEAD)
git rebase "$main" || { git rebase --abort; echo "rebase onto $main failed: resolve it, commit, and merge again"; exit 1; }
git -C %[1]s merge --ff-only %[2]s
echo "merged %[3]s into $main"`, project, text.QuoteShell(sess.Branch), sess.Branch)
	return r.bash(ctx, script)
}

// bash runs a command in the round's folder through the Bash tool.
func (r *wfRound) bash(ctx context.Context, command string) (string, bool) {
	args, _ := json.Marshal(map[string]any{"command": command, "timeout_seconds": runTimeout})
	req := api.ToolRequest{SessionID: r.session(), Name: "Bash", Args: args}
	if req.SessionID == "" {
		req.Dir = r.j.Dir
	}
	res, err := r.s.opts.Tools.RunTool(ctx, req)
	if err != nil {
		return err.Error(), false
	}
	return res.Output, !res.IsError
}
