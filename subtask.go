package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The orchestrator and executor of a decomposed run. RunTask holds the plan
// and decides what happens when a subtask does not fit; runSubtask runs the
// very same loop as an ordinary turn, against a fresh context and a small
// budget. "Done" is decided by the exit code the harness saw, never by the
// model's word.

type subOutcome int

const (
	subNone    subOutcome = iota
	subDone               // subtask_done accepted, or closed by the harness after a pass
	subBlocked            // BLOCKED from the model, or the ladder / no-progress handed it back
	subBudget             // out of steps
	subStopped            // denial or cancellation: the whole task stops
)

// subState is what the harness tracks inside one subtask. It lives exactly as
// long as the subtask's context window does.
type subState struct {
	task     *Subtask
	idx, n   int
	budget   int
	verified bool // the declared verify command exited 0 in THIS subtask
	failedV  int  // how often it ran and failed
	want     subOutcome
	summary  string
	reason   string
}

// note is the rider in the system message: which subtask, how many steps are
// left, and whether the done-condition has passed - the one fact a small
// model keeps forgetting.
func (s *subState) note(step int) string {
	state := fmt.Sprintf("has NOT passed yet in this subtask (%d failed runs). subtask_done will be refused until it does", s.failedV)
	if s.verified {
		state = "has PASSED. Stop working and call subtask_done with a one-line summary"
	}
	return fmt.Sprintf("[subtask %d of %d] %s\nStep %d of %d. The done-condition `%s` %s.", s.idx, s.n, s.task.Title, step+1, s.budget, s.task.Verify, state)
}

// noteVerify is how the harness sees the exit code of the verify command: the
// model runs it through bash like any other command and the harness only
// watches. A different command that happens to pass proves nothing.
func (s *subState) noteVerify(cmd string, r ToolResult) {
	if !runsVerifier(cmd, s.task.Verify) {
		return
	}
	if r.Failed {
		s.failedV++
		return
	}
	s.verified = true
}

// runsVerifier reports whether cmd runs the subtask's verify command: the
// verify's segments appear in order and contiguously among cmd's segments. An
// appended "&& echo ok" or a leading "cd x;" still counts; a subset of the
// segments, or the same command with other paths, does not. A live run showed
// why this matters: a compound verifier compared against single segments can
// never match, and the model burns its whole budget re-running a command
// that the harness refuses to see.
func runsVerifier(cmd, verify string) bool {
	want, got := segments(verify), segments(cmd)
	if len(want) == 0 || len(got) < len(want) {
		return false
	}
	for i := 0; i+len(want) <= len(got); i++ {
		if slices.Equal(got[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// segments splits a shell line on &&, ||, ; and | into trimmed pieces.
func segments(s string) []string {
	var out []string
	for _, seg := range reCmdSplit.Split(s, -1) {
		if seg = strings.TrimSpace(seg); seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

var taskTools = []*Tool{{
	Name:     "subtask_done",
	Desc:     "Close the current subtask. Accepted only after its verify command has exited 0 in this subtask. summary: one line on what changed and how it was verified, for the next subtask.",
	Params:   []Param{{Name: "summary", Type: "string", Desc: "One line: what changed and how it was verified"}},
	Required: []string{"summary"},
	Example:  `{"summary": "Added sum() to src/math.js; npm test -- math passes"}`,
	Run:      runSubtaskDone,
}}

func runSubtaskDone(ctx context.Context, a *Agent, args map[string]any) ToolResult {
	s := a.sub
	if !s.verified {
		// ErrKey gives failSignature a stable key: insisting "it is done"
		// climbs the ladder and lands in the orchestrator's hands.
		return ToolResult{Invalid: true, ErrKey: "subtask_unverified", Status: "done-condition belum lulus", Output: correction(
			"subtask_done was refused: the done-condition has not passed.",
			"This subtask counts as finished when `"+s.task.Verify+"` exits 0, and the harness has not seen that happen in this subtask.",
			"Run it with bash. If it passes, call subtask_done again. If it cannot pass here, reply with one line starting with BLOCKED: and what is missing.",
			`bash {"command": "`+s.task.Verify+`"}`)}
	}
	s.want, s.summary = subDone, clip(strings.TrimSpace(str(args, "summary")), 160)
	return ToolResult{Output: "Subtask closed.", Status: "subtask ditutup"}
}

// ---------------------------------------------------------------------------
// Orchestrator.

// RunTask is /task: plan, then run the subtasks one at a time, each in a
// fresh context, and decide what happens when one does not fit.
func (a *Agent) RunTask(ctx context.Context, goal string) error {
	a.ui.Info("menyusun rencana dengan " + a.plannerModel() + "…")
	sts, err := a.callPlanner(ctx, goal, nil)
	switch {
	case errors.Is(err, errPlanDirect):
		a.ui.Info("permintaan ini satu langkah, dikerjakan langsung")
		return a.RunTurn(ctx, goal)
	case errors.Is(err, errPlanInvalid):
		a.ui.Warn("planner tidak menghasilkan rencana yang bisa dipakai; dikerjakan seperti biasa")
		return a.RunTurn(ctx, goal)
	case err != nil:
		return err
	}
	p := &TaskPlan{Goal: goal, Subtasks: sts}
	a.plan = p
	a.log.Event("plan", map[string]any{"goal": goal, "subtasks": sts, "planner": a.plannerModel()})
	defer func() { a.plan, a.sub = nil, nil }()
	a.ui.Info(planLines(p))
	if a.ask != nil && !a.cfg.Task { // -p --task has nobody to ask: printing the plan is the approval
		ans, err := a.ask(ctx, fmt.Sprintf("  jalankan %d subtask ini? [y/N] ", len(p.Subtasks)))
		if err != nil {
			return err
		}
		if ans != "y" {
			a.ui.Info("rencana dibatalkan")
			return nil
		}
	}
	a.todos = nil
	fails := 0 // consecutive subtasks that did not fit
	for p.Cur < len(p.Subtasks) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.steps > a.cfg.MaxSteps*3 {
			a.ui.Warn("anggaran langkah total habis; tugas dihentikan")
			break
		}
		t := p.cur()
		if a.preflightGreen(ctx, t) {
			p.complete(handoff{Items: []string{t.Title}, Verify: t.Verify, Summary: "already satisfied before the subtask started; skipped"})
			continue
		}
		out, err := a.runSubtask(ctx)
		if err != nil {
			return err
		}
		switch out {
		case subDone:
			fails = 0
			continue
		case subStopped:
			a.ui.Info("tugas dihentikan")
			return a.finishTask(ctx)
		}
		fails++
		if fails >= 2 {
			// Two subtasks in a row that did not fit: the plan is wrong, not
			// the executor. Grinding on a wrong plan is the worst outcome
			// available, so it is made unreachable.
			a.ui.Warn("dua subtask beruntun tidak muat: rencana dibuang, dikerjakan sebagai satu giliran biasa")
			return a.abandonPlan(ctx)
		}
		if !a.escalateSubtask(ctx, out) {
			break
		}
	}
	return a.finishTask(ctx)
}

// preflightGreen runs the subtask's verify command before the subtask starts.
// A command that already passes is not a done-condition: the subtask is
// skipped rather than handed to a model that could never tell when to stop.
func (a *Agent) preflightGreen(ctx context.Context, t *Subtask) bool {
	if !a.allowed(ctx, "bash", commandRisk(t.Verify)) {
		return false
	}
	res := runBash(ctx, a, map[string]any{"command": t.Verify})
	res.Summary = "bash " + clip(t.Verify, 60)
	a.logTool(res, "preflight")
	if res.Failed {
		return false // red, as a done-condition should be
	}
	for _, f := range t.Files {
		if _, err := os.Stat(a.resolve(f)); err != nil {
			// Green while the files it is meant to produce do not exist: the
			// verifier is lying, not the work done. Run the subtask anyway.
			a.ui.Warn(fmt.Sprintf("verifier subtask %q sudah hijau padahal %s belum ada: verifier dicurigai, subtask tetap dijalankan", clip(t.Title, 40), f))
			return false
		}
	}
	a.ui.Harness(fmt.Sprintf("subtask %q sudah hijau sebelum dimulai: dilewati", clip(t.Title, 50)))
	return true
}

// runSubtask executes the current subtask in a fresh context with the shared
// loop and a budget that grows with the number of files it names.
func (a *Agent) runSubtask(ctx context.Context) (subOutcome, error) {
	p := a.plan
	t := p.cur()
	budget := max(a.cfg.SubSteps+2*len(t.Files), 1)
	a.sub = &subState{task: t, idx: p.Cur + 1, n: len(p.Subtasks), budget: budget}
	defer func() { a.sub = nil }()
	a.msgs = []Message{
		{Role: "system", Content: taskSystemPrompt(a.env) + "\n\n" + taskBlock(p)},
		{Role: "user", Content: subtaskBlock(p, budget)},
	}
	a.turnStart = 1
	st := &turnState{needThink: true}
	a.turn, a.todoMutations = st, a.mutations
	defer func() { a.turn = nil }()
	a.ui.Info(fmt.Sprintf("── subtask %d/%d: %s · verifikasi: %s · anggaran %d langkah", p.Cur+1, len(p.Subtasks), t.Title, t.Verify, budget))
	a.log.Event("subtask", map[string]any{"idx": p.Cur + 1, "of": len(p.Subtasks), "title": t.Title, "verify": t.Verify, "budget": budget, "depth": t.depth})

	out, err := a.loop(ctx, st, budget)
	p.steps += st.step
	if err != nil {
		return subStopped, err
	}
	var files []string
	for _, m := range st.mutated {
		files = append(files, a.rel(m))
	}
	s := a.sub
	if s.verified && s.want != subDone {
		// The harness saw the verifier pass; the model ran out of steps, or
		// handed the subtask back, without saying so. The fact wins over the
		// silence: close it, with the model's last words as the summary.
		s.want = subDone
		for i := len(a.msgs) - 1; i > a.turnStart; i-- {
			if m := a.msgs[i]; m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
				s.summary = clip(strings.TrimSpace(m.Content), 160)
				break
			}
		}
		a.ui.Harness(fmt.Sprintf("subtask %d: verifier sudah lulus tapi tidak ditutup model: ditutup oleh harness", p.Cur+1))
		out = outSub
	}
	switch out {
	case outSub:
		if s.want == subDone {
			a.flagVerifierTouched(t, files)
			p.complete(handoff{Items: []string{t.Title}, Files: files, Verify: t.Verify, Summary: s.summary})
			return subDone, nil
		}
		p.lastReason, p.lastLedger, p.lastFiles = s.reason, st.ledger(), files
		return s.want, nil
	case outBudget:
		p.lastReason, p.lastLedger, p.lastFiles = fmt.Sprintf("ran out of its %d steps without the verify command passing", budget), st.ledger(), files
		a.ui.Harness(fmt.Sprintf("subtask %d kehabisan %d langkah tanpa lulus verifikasi", p.Cur+1, budget))
		return subBudget, nil
	case outAnswered:
		p.lastReason, p.lastLedger, p.lastFiles = "answered without closing the subtask", st.ledger(), files
		return subBlocked, nil
	}
	return subStopped, nil
}

// flagVerifierTouched tells the user when a subtask changed a file its own
// verify command names: the cheapest way to make a test pass is to edit it.
func (a *Agent) flagVerifierTouched(t *Subtask, files []string) {
	if reFileCheck.MatchString(t.Verify) {
		return // a file check names the very file the subtask is meant to produce
	}
	for _, f := range files {
		if strings.Contains(t.Verify, filepath.Base(f)) {
			a.ui.Warn(fmt.Sprintf("subtask mengubah %s, file yang disebut perintah verifikasinya sendiri — periksa apakah ceknya dilemahkan", f))
		}
	}
}

// escalateSubtask decides what happens when the current subtask did not fit.
// Like the ladder inside a turn, each rung changes the shape: split it, ask
// the user, or stop with a report. It reports whether the run continues.
func (a *Agent) escalateSubtask(ctx context.Context, out subOutcome) bool {
	p := a.plan
	t := p.cur()
	if t.depth < maxSplitDepth && p.splits < maxSplits {
		a.ui.Info("meminta planner memecah subtask ini…")
		parts, err := a.callPlanner(ctx, p.Goal, p)
		if err == nil {
			p.replace(p.Cur, parts)
			a.ui.Harness(fmt.Sprintf("subtask %d tidak muat (%s): dipecah jadi %d", p.Cur+1, clip(p.lastReason, 60), len(parts)))
			return true
		}
		a.ui.Warn("planner tidak bisa memecahnya: " + err.Error())
	}
	if a.askLine != nil {
		opts := []string{"lewati subtask ini", "hentikan, saya yang lanjutkan", "coba tanpa rencana (satu giliran biasa)"}
		a.ui.Warn(fmt.Sprintf("subtask %d tidak bisa diselesaikan: %s", p.Cur+1, clip(p.lastReason, 80)))
		a.ui.Options(opts)
		ans, err := a.askLine(ctx, "  pilih (nomor) › ")
		if err != nil {
			return false
		}
		switch strings.TrimSpace(ans) {
		case "1":
			p.complete(handoff{Items: []string{t.Title}, Summary: "skipped by the user: " + clip(p.lastReason, 100)})
			return true
		case "3":
			p.lastReason = "abandon"
			return false
		}
	}
	return false
}

// abandonPlan drops the plan and hands the request to one ordinary turn, with
// a note on what was already finished so the work is not redone.
func (a *Agent) abandonPlan(ctx context.Context) error {
	p := a.plan
	a.plan, a.sub = nil, nil
	a.msgs = []Message{{Role: "system", Content: systemPrompt(a.env)}}
	note := "[harness] An earlier attempt with a step-by-step plan was abandoned before anything was finished. Work on the request directly."
	if len(p.Done) > 0 {
		note = "[harness] An earlier attempt with a step-by-step plan was abandoned. Already finished and verified:\n" + handoffLines(p.Done) + "Continue the request from there; do not redo those steps."
	}
	return a.RunTurn(ctx, p.Goal+"\n\n"+note)
}

// finishTask re-verifies the last subtask, asks the model for a report with
// only {system, goal, handoff} in view, and rewrites a.msgs into one clean
// synthetic turn so the user's next message lands in a context that knows
// what was asked.
func (a *Agent) finishTask(ctx context.Context) error {
	p := a.plan
	if p.lastReason == "abandon" {
		return a.abandonPlan(ctx)
	}
	if n := len(p.Done); n > 0 && p.Done[n-1].Verify != "" && p.Cur >= len(p.Subtasks) {
		// The cheapest check of all: did the last subtask break the previous one?
		v := p.Done[n-1].Verify
		if a.allowed(ctx, "bash", commandRisk(v)) {
			res := runBash(ctx, a, map[string]any{"command": v})
			res.Summary = "bash " + clip(v, 60)
			a.logTool(res, "recheck")
			if res.Failed {
				a.ui.Warn("verifikasi ulang terakhir GAGAL: " + v)
			} else {
				a.ui.Info("verifikasi ulang terakhir lulus: " + v)
			}
		}
	}
	remaining := p.Subtasks[min(p.Cur, len(p.Subtasks)):]
	var b strings.Builder
	b.WriteString("Write the final answer for the user about this request. Say what was finished and how it was verified, what was not finished and why, and what the user should do next. Short and terminal-friendly; no headings, no tables.\n\nFinished:\n")
	if len(p.Done) == 0 {
		b.WriteString("nothing\n")
	} else {
		b.WriteString(handoffLines(p.Done))
	}
	if len(remaining) > 0 {
		b.WriteString("\nNot finished:\n")
		for _, s := range remaining {
			fmt.Fprintf(&b, "- %s (verify: %s)\n", s.Title, s.Verify)
		}
		if p.lastReason != "" {
			b.WriteString("Last reason: " + p.lastReason + "\n")
		}
	}
	msgs := []Message{
		{Role: "system", Content: systemPrompt(a.env) + "\n\n" + taskBlock(p)},
		{Role: "user", Content: b.String()},
	}
	resp, err := a.chatWith(ctx, a.model, a.profile, msgs, false, Handlers{Text: a.ui.Text, Think: a.ui.Think})
	if err != nil && isCanceled(err) {
		return err
	}
	answer := ""
	if err == nil {
		answer = resp.Content
	}
	if strings.TrimSpace(answer) == "" {
		// The report never depends on the model: the harness knows the facts.
		answer = "Selesai dan terverifikasi:\n" + handoffLines(p.Done)
		if len(remaining) > 0 {
			answer += fmt.Sprintf("Belum selesai: %d subtask (%s).", len(remaining), p.lastReason)
		}
		a.ui.BeginResponse(false)
		a.ui.Text(answer + "\n")
		a.ui.EndResponse()
	}
	a.msgs = []Message{
		{Role: "system", Content: systemPrompt(a.env)},
		{Role: "user", Content: p.Goal},
		{Role: "assistant", Content: answer},
	}
	a.turnStart = 1
	return nil
}
