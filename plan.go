package main

import (
	"fmt"
	"regexp"
	"strings"
)

// A decomposed run (/task). The harness owns a list of subtasks, each with a
// shell command whose exit code decides whether it is done. The model works
// on one subtask at a time in a fresh context, and can only close the current
// one. Nothing here talks to a model or touches the disk: it is the shape of
// the plan, its rules, and how it is rendered.

// Subtask is one unit of work the executor can finish in a handful of steps.
// Verify is what makes it a subtask and not a wish.
type Subtask struct {
	Title  string   `json:"title"`            // one imperative line
	Detail string   `json:"detail,omitempty"` // at most a few lines
	Files  []string `json:"files,omitempty"`  // hint, never enforced
	Verify string   `json:"verify"`           // exit 0 == done
	Expect string   `json:"expect,omitempty"` // what passing looks like
	depth  int      // 0 from the first plan, 1 = product of a split
}

// TaskPlan is harness-owned. The model can close the current subtask; it can
// never replace this list.
type TaskPlan struct {
	Goal     string
	Subtasks []Subtask
	Done     []handoff // one per finished subtask, recorded by the harness
	Answers  []string  // ask_user Q -> A pairs: facts about the world
	Cur      int
	splits   int
	steps    int // executor steps spent across the whole run

	// Evidence from the subtask that last failed, for the planner's split.
	lastReason string
	lastLedger string
	lastFiles  []string
}

const (
	maxSubtasks   = 8
	maxSplitDepth = 1 // a subtask may be split; its children may not
	maxSplits     = 3 // per run
	handoffKeep   = 5 // older handoffs collapse to a single rollup line
)

func (p *TaskPlan) cur() *Subtask { return &p.Subtasks[p.Cur] }

func (p *TaskPlan) complete(h handoff) {
	p.Done = append(p.Done, h)
	p.Cur++
}

// replace splices parts in for subtask i, one level deeper.
func (p *TaskPlan) replace(i int, parts []Subtask) {
	depth := p.Subtasks[i].depth + 1
	for j := range parts {
		parts[j].depth = depth
	}
	rest := append([]Subtask{}, p.Subtasks[i+1:]...)
	p.Subtasks = append(append(p.Subtasks[:i:i], parts...), rest...)
	p.splits++
}

// ---------------------------------------------------------------------------
// Validation. acceptableVerifier is the rule the whole design rests on: a
// done-condition must be a command that can fail now and pass later.

var reNotAVerifier = regexp.MustCompile(`(?i)^\s*(cat|ls|echo|printf|head|tail|pwd|find|wc|which|tree|true)\b`)

// reScriptRun is running a shell script the plan itself produces. Its exit
// code is the script's own contract, and the preflight keeps it honest: a
// script that does not exist yet fails, so the check is red before the work.
var reScriptRun = regexp.MustCompile(`(?i)^(bash|sh)\s+\S+\.sh\b|^\.{1,2}/\S+\.sh\b`)

// reFileCheck is a verifier about a file's existence or content. Such a
// verifier names the file the subtask is supposed to change, which is not
// evidence of a weakened check.
var reFileCheck = regexp.MustCompile(`(?i)^\s*(test\s+-|\[\s+-|grep\s+-[a-zA-Z]*q|rg\s+-[a-zA-Z]*q|diff\b|cmp\b)`)

// reVerifierCheat is a verifier that cannot fail: a trailing "|| true", or
// anything after ";" or "||" whose own exit code masks the check - the classic
// being "; echo $?", which prints the failure and then succeeds. A live run
// produced exactly that, and the preflight saw a green light on a file that
// did not exist.
var reVerifierCheat = regexp.MustCompile(`(?i)(\|\||;)\s*(true|:|echo|printf)\b|\$\?|--no-verify\b`)

func acceptableVerifier(cmd string, env *Env) bool { return verifierProblem(cmd, env) == "" }

// verifierProblem says, in planner-facing words, why cmd is not a
// done-condition, or "".
func verifierProblem(cmd string, env *Env) string {
	v := strings.TrimSpace(cmd)
	switch {
	case v == "":
		return "has no verify command"
	case reNotAVerifier.MatchString(v):
		return "verify `" + v + "` only prints; its exit code proves nothing"
	case reVerifierCheat.MatchString(v):
		return "verify `" + v + "` cannot fail; a done-condition must be able to fail"
	case reScriptRun.MatchString(v):
		return ""
	case !isVerifyCmd(v, env.VerifyCmds):
		return "verify `" + v + "` is not a check this harness recognises; use one of: " + verifyExamples(env)
	}
	return ""
}

func verifyExamples(env *Env) string {
	ex := []string{"test -f <file>", "grep -q <text> <file>", "go test ./pkg -run TestName", "npm test -- path/to.test.js"}
	if len(env.VerifyCmds) > 0 {
		ex = append(append([]string{}, env.VerifyCmds...), ex...)
	}
	return strings.Join(ex, ", ")
}

// validatePlan returns every problem with the plan, in words that go straight
// into a correction to the planner. An empty result means the plan is usable.
func validatePlan(sts []Subtask, env *Env) []string {
	if len(sts) == 0 {
		return []string{"the plan has no subtasks"}
	}
	var problems []string
	if len(sts) > maxSubtasks {
		problems = append(problems, fmt.Sprintf("%d subtasks, at most %d are allowed: merge the small ones", len(sts), maxSubtasks))
	}
	for i, s := range sts {
		n := i + 1
		t := strings.TrimSpace(s.Title)
		switch {
		case t == "":
			problems = append(problems, fmt.Sprintf("subtask %d has no title", n))
		case len([]rune(t)) > 100:
			problems = append(problems, fmt.Sprintf("subtask %d: the title is longer than 100 characters", n))
		}
		if p := verifierProblem(s.Verify, env); p != "" {
			problems = append(problems, fmt.Sprintf("subtask %d (%s) %s", n, clip(t, 40), p))
		}
	}
	return problems
}

// ---------------------------------------------------------------------------
// Rendering. English for the model, Indonesian for the user.

// taskBlock rides in the system message of every subtask: the request word
// for word, what is finished (recorded by the harness), the user's answers.
func taskBlock(p *TaskPlan) string {
	var b strings.Builder
	b.WriteString("<task>\nThe user's request, word for word:\n" + p.Goal + "\n\n")
	b.WriteString("The harness owns the plan for this run: subtasks run one at a time, each in a fresh context, and you can only close the current one with subtask_done once its verify command has passed.\n")
	if len(p.Done) == 0 {
		b.WriteString("Finished so far: nothing yet.\n")
	} else {
		b.WriteString("Finished so far (recorded by the harness after verification):\n" + handoffLines(p.Done))
	}
	if len(p.Answers) > 0 {
		b.WriteString("Decisions the user already made:\n")
		for _, a := range p.Answers {
			b.WriteString("- " + a + "\n")
		}
	}
	b.WriteString("</task>")
	return b.String()
}

// handoffLines lists finished subtasks; beyond handoffKeep the oldest ones
// collapse into one line so the block stays small.
func handoffLines(hs []handoff) string {
	var b strings.Builder
	start := 0
	if len(hs) > handoffKeep {
		start = len(hs) - handoffKeep
		var titles []string
		for _, h := range hs[:start] {
			titles = append(titles, strings.Join(h.Items, "; "))
		}
		fmt.Fprintf(&b, "1-%d. done and verified: %s\n", start, strings.Join(titles, "; "))
	}
	for i := start; i < len(hs); i++ {
		h := hs[i]
		fmt.Fprintf(&b, "%d. %s", i+1, strings.Join(h.Items, "; "))
		if h.Verify != "" {
			fmt.Fprintf(&b, " - passed: %s", h.Verify)
		}
		if len(h.Files) > 0 {
			fmt.Fprintf(&b, " - changed: %s", strings.Join(h.Files, ", "))
		}
		if h.Summary != "" {
			fmt.Fprintf(&b, " - note: %s", h.Summary)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// subtaskBlock is the user message a subtask starts with: one task, and only it.
func subtaskBlock(p *TaskPlan, budget int) string {
	t := p.cur()
	var b strings.Builder
	fmt.Fprintf(&b, "<subtask %d of %d>\n%s\n", p.Cur+1, len(p.Subtasks), t.Title)
	if d := strings.TrimSpace(t.Detail); d != "" {
		b.WriteString(d + "\n")
	}
	if len(t.Files) > 0 {
		fmt.Fprintf(&b, "Files likely involved: %s\n", strings.Join(t.Files, ", "))
	}
	fmt.Fprintf(&b, "Done when this command exits 0: %s\n", t.Verify)
	if e := strings.TrimSpace(t.Expect); e != "" {
		fmt.Fprintf(&b, "What passing looks like: %s\n", e)
	}
	fmt.Fprintf(&b, "You have at most %d tool calls. Earlier subtasks' work is already in the files; read only what you need.\n", budget)
	b.WriteString("When the verify command has passed, stop working and call subtask_done with a one-line summary: that line is all the next subtask will know about your work.\n")
	b.WriteString("If this cannot be finished here (it is really two changes, or something it assumes is missing), reply with one line starting with BLOCKED: and what is missing; the harness will split it.\n")
	b.WriteString("Do not touch anything outside this subtask; if you notice another problem, mention it in your summary.\n</subtask>")
	return b.String()
}

// planLines is the plan as shown to the user before it runs.
func planLines(p *TaskPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rencana: %d subtask", len(p.Subtasks))
	for i, s := range p.Subtasks {
		fmt.Fprintf(&b, "\n  %d) %s\n     verifikasi: %s", i+1, s.Title, s.Verify)
	}
	return b.String()
}
