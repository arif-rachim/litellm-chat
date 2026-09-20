package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// turnState is what the harness tracks during one user turn to decide when
// to think, remind and stop.
type turnState struct {
	step           int
	needThink      bool // plan at the start, rethink after errors
	emptyNudged    bool
	lengthNudges   int
	todoReminded   bool
	planAsked      bool
	verifyReminded bool
	mutated        []string
	verifiedAfter  bool // a verification command ran after the last change
	lastCheckOK    bool
	recent         []string // hashes of recent tool calls

	// Search state: what has been tried, and how often the same kind of
	// failure came back whatever was tried.
	calls        int
	attempts     []attempt
	failSig      string
	failStreak   int
	failFirst    int // ledger entry where this failure first showed up
	rungDone     int // highest escalation rung already sent for this failure
	lastClass    string
	askDemanded  bool
	subReminded  bool    // told once that a final answer does not close a subtask
	intentNudged bool    // told once that announcing a step is not doing it
	lastText     string  // normalised last text-only reply
	sameText     int     // how often it repeated verbatim
	images       []Image // images a tool produced this step, sent after the batch

	// Progress signals: what counts as the search actually moving, and the
	// state that tells a real change from one that only looks like it.
	evidence   bool                // something new happened this step
	progressAt int                 // step of the last evidence
	idleFired  int                 // no-progress warnings sent this turn
	readPaths  map[string]bool     // files read in this context window
	unread     map[string]bool     // edited since last read or verification
	hashes     map[string][]string // path -> content hashes seen this window
	verifyCmd  string              // the last verification command that passed

	// What crosses a checkpoint. A turn can span several context windows; these
	// are the only fields that survive newWindow.
	handoffs  []handoff
	readSeen  map[string]bool // every path read this turn, across windows
	milestone []string        // todo items just closed with verified work
}

// handoff is what one verified milestone leaves behind for the next context
// window. Everything but Summary is recorded by the harness, so a model that
// claims success cannot smuggle the claim into the next window.
type handoff struct {
	Items   []string // todo items closed at this checkpoint
	Files   []string // paths the harness saw mutated in the window
	Verify  string   // the command the harness saw exit 0 ("" = auto-check)
	Summary string   // the model's last sentence before closing, clipped
}

// newWindow starts a fresh context window inside the same turn, right after a
// checkpoint. Everything that indexed evidence in the old context is dropped -
// ledger, streaks, hashes, read set - because that evidence is gone with the
// context. Only the step budget, the handoffs and the turn-level nudges stay.
func (st *turnState) newWindow() {
	*st = turnState{
		step:         st.step,
		handoffs:     st.handoffs,
		readSeen:     st.readSeen,
		planAsked:    st.planAsked,
		lengthNudges: st.lengthNudges,
		emptyNudged:  st.emptyNudged,
		needThink:    true,
		progressAt:   st.step,
	}
}

// handoffBlock is the rider that carries verified facts across windows. Like
// the ledger it rides in the system message, rebuilt for every request.
func (st *turnState) handoffBlock(todos []Todo) string {
	if len(st.handoffs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[checkpoints this turn - recorded by the harness after verification, not claimed by the model]")
	for i, h := range st.handoffs {
		fmt.Fprintf(&b, "\n%d. done: %s", i+1, strings.Join(h.Items, "; "))
		if len(h.Files) > 0 {
			fmt.Fprintf(&b, " - changed: %s", strings.Join(h.Files, ", "))
		}
		if h.Verify != "" {
			fmt.Fprintf(&b, " - passed: %s", h.Verify)
		} else {
			b.WriteString(" - passed: auto-check")
		}
		if h.Summary != "" {
			fmt.Fprintf(&b, " - note: %s", h.Summary)
		}
	}
	if len(st.readSeen) > 0 {
		paths := make([]string, 0, len(st.readSeen))
		for p := range st.readSeen {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		fmt.Fprintf(&b, "\nFiles already read this turn (their content is no longer in this context; re-read only what you need): %s", strings.Join(paths, ", "))
	}
	if len(todos) > 0 {
		b.WriteString("\nPlan now:\n" + todoList(todos))
	}
	b.WriteString("\nThe context was compacted at the last checkpoint. Continue with the next open step; do not redo finished ones.")
	return b.String()
}

// noProgressSteps is how many steps may pass without evidence before the
// harness says so. Evidence is: a file read for the first time this turn, a
// change, a new kind of failure, or a verification that passed.
const noProgressSteps = 6

// noteRead records a file read. The first read of a path this turn is
// evidence; it also lifts the read-before-edit requirement for that path.
func (st *turnState) noteRead(path string) {
	if st.readPaths == nil {
		st.readPaths = map[string]bool{}
	}
	if st.readSeen == nil {
		st.readSeen = map[string]bool{}
	}
	if !st.readPaths[path] {
		st.readPaths[path] = true
		st.evidence = true
	}
	st.readSeen[path] = true
	delete(st.unread, path)
}

// noteEdited marks a path as changed under the model's feet: its copy in the
// context is stale until it reads the file again or a verification passes.
func (st *turnState) noteEdited(path string) {
	if st.unread == nil {
		st.unread = map[string]bool{}
	}
	st.unread[path] = true
}

// noteVerified is a verification that passed: the state of every file is
// known again, and the search has moved.
func (st *turnState) noteVerified() {
	st.verifiedAfter = true
	st.evidence = true
	st.unread = nil
}

// seenContent records the file's current content and reports whether exactly
// that content was already seen this turn. Called before a mutation it
// remembers the starting point; called after, a true answer means the model
// has gone back to a version it already had.
func (st *turnState) seenContent(path string) bool {
	h, ok := contentHash(path)
	if !ok {
		return false
	}
	if st.hashes == nil {
		st.hashes = map[string][]string{}
	}
	if slices.Contains(st.hashes[path], h) {
		return true
	}
	st.hashes[path] = append(st.hashes[path], h)
	return false
}

func contentHash(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:]), true
}

// noProgress is the message for a run of steps that brought nothing new. Like
// the failure ladder it changes the ask each time instead of repeating it.
func noProgress(nth int, canAsk bool) (screen, msg string, stop bool) {
	switch {
	case nth == 1:
		return fmt.Sprintf("%d langkah tanpa bukti baru: minta model menyebut apa yang kurang", noProgressSteps),
			correction(
				fmt.Sprintf("%d steps passed without new evidence: no file read for the first time, no change, no new kind of failure, no verification passed.", noProgressSteps),
				"Steps that bring nothing new are the search going in circles, whatever they look like.",
				"Say in one line what you are missing (a file, a fact, a command's output), get it in ONE call, or give your final answer.", ""),
			false
	case nth == 2 && canAsk:
		return "masih tanpa bukti baru: harus bertanya ke user",
			correction(
				fmt.Sprintf("Another %d steps without new evidence.", noProgressSteps),
				"You are guessing, and guessing costs the user more than asking does.",
				"Call ask_user now: one line on what is blocking you, and 2-4 concrete options.",
				`ask_user {"question": "Saya tidak menemukan di mana konfigurasi dibaca. Mana yang benar?", "options": ["config.yaml di root", "environment variable", "Lewati dulu"]}`),
			false
	default:
		return "tanpa bukti baru berulang: giliran dihentikan",
			correction(
				"The search is not moving: repeated runs of steps without new evidence.",
				"Continuing would spend the user's tokens on circles.",
				"Stop here. Tell the user what you tried and what you would need (a file, a decision, a command output) to get further.", ""),
			true
	}
}

// wantThink decides whether the next request should use thinking. It knows
// nothing about the model; the profile translates the answer.
func wantThink(mode string, st *turnState) bool {
	switch mode {
	case "on":
		return true
	case "off":
		return false
	}
	return st.needThink
}

func (st *turnState) noteMutation(path string, checkOK bool) {
	if !slices.Contains(st.mutated, path) {
		st.mutated = append(st.mutated, path)
	}
	st.verifiedAfter = false
	st.lastCheckOK = checkOK
}

var proseExt = map[string]bool{".md": true, ".txt": true, ".rst": true, ".log": true, ".csv": true}

// verified: a test/build ran after the last change, a single file was changed
// and its auto-check passed, or only prose files were changed.
func (st *turnState) verified() bool {
	if st.verifiedAfter || (len(st.mutated) == 1 && st.lastCheckOK) {
		return true
	}
	for _, p := range st.mutated {
		if !proseExt[strings.ToLower(filepath.Ext(p))] {
			return false
		}
	}
	return true
}

// reCheckForm matches commands whose exit code proves something about the
// work: test runners, builds, type checks, and explicit shell checks. Merely
// running a program (node x.js, python3 y.py, npx z, a bare make) proves
// nothing, so those are deliberately absent.
var reCheckForm = regexp.MustCompile(`(?i)^(` +
	`go (test|build|vet)\b|cargo (test|build|check|clippy)\b|` +
	`(npm|pnpm|yarn|bun) (test|run (test|check|lint|typecheck|build)\S*)\b|` +
	`python3? -m pytest\b|pytest\b|vitest\b|jest\b|mocha\b|tsc\b|eslint\b|` +
	`make (test|check|lint|build|vet)\b|` +
	`test -[edfrsxzLn]\b|\[ -[edfrsxzLn] |grep -[a-zA-Z]*q|rg -[a-zA-Z]*q|diff\b|cmp\b|git diff --exit-code\b` +
	`)`)

var reCmdSplit = regexp.MustCompile(`\s*(&&|\|\||;|\||\n)\s*`)
var reEnvAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*\s+`)

// isVerifyCmd reports whether running cmd verifies something: a segment of it
// is one of the project's own verification commands, or has a check form. The
// whole harness rests on this answer, so it errs on the strict side: a command
// that only runs code is not verification, however plausible it looks.
func isVerifyCmd(cmd string, verifyCmds []string) bool {
	for _, seg := range reCmdSplit.Split(cmd, -1) {
		seg = strings.TrimSpace(seg)
		for reEnvAssign.MatchString(seg) {
			seg = reEnvAssign.ReplaceAllString(seg, "")
		}
		if seg == "" {
			continue
		}
		for _, v := range verifyCmds {
			if v != "" && strings.HasPrefix(seg, v) {
				return true
			}
		}
		if reCheckForm.MatchString(seg) {
			return true
		}
	}
	return false
}

// reIntent matches a first-person announcement of a next step, in the two
// languages the model writes. A reply that ends this way and calls no tool is
// the commonest small-model stall: the plan is right, the action never comes.
var reIntent = regexp.MustCompile(`(?i)\b(mari (saya|kita)|saya akan|akan saya|saya perlu|sekarang saya|biar saya|let me|let's|i will|i'll|i need to|i am going to|i'm going to|now i|next,? i)\b`)

// looksLikeIntent reports whether a final reply only announces what the model
// is about to do. It looks at the last line, or the whole text when short.
func looksLikeIntent(content string) bool {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if reIntent.MatchString(last) {
		return true
	}
	return len([]rune(content)) < 400 && reIntent.MatchString(content)
}

// correction formats a harness message so the model learns what went wrong
// and why, not just that it did.
func correction(problem, why, next, example string) string {
	var b strings.Builder
	b.WriteString("[harness] Problem: " + problem)
	if why != "" {
		b.WriteString("\nWhy: " + why)
	}
	if next != "" {
		b.WriteString("\nNext step: " + next)
	}
	if example != "" {
		b.WriteString("\nExample: " + example)
	}
	return b.String()
}

const reflectHint = "[harness] This step failed. Before retrying, state the cause of the error in one sentence, then change your approach. Do not repeat the same command without a change."

func todoReminderLine(todos []Todo) string {
	return "[plan] " + strings.ReplaceAll(todoList(todos), "\n", " | ")
}

func verifyReminder(files []string, verifyCmds []string) string {
	how := "run the changed code (e.g. node file.js) or its tests"
	if len(verifyCmds) > 0 {
		how = "run `" + verifyCmds[0] + "`"
	}
	return correction(
		fmt.Sprintf("You changed %s but did not verify the result.", strings.Join(files, ", ")),
		"Unverified changes often contain mistakes the user will hit later.",
		"Verify now: "+how+". If verification is truly not needed, say why in one sentence and finish.", "")
}

func todoReminder(open []string) string {
	return correction(
		"Your plan still has unfinished steps: "+strings.Join(open, "; ")+".",
		"You were about to finish while the plan says work remains.",
		"Continue with the next step, or update the plan with todo (mark done or remove steps) and say why they were skipped.", "")
}

// ---------------------------------------------------------------------------
// Searching without looping.
//
// A small model repeats itself for three reasons: the failed attempt scrolled
// out of the context, nothing separates "a new idea" from "the same idea in new
// clothes", and every correction it gets looks the same. The three pieces below
// answer those in order: a ledger that is resent on every request, a failure
// signature that recognises the same wall whatever ran into it, and a ladder
// that changes the shape of the task at each repeat instead of pushing harder.

// attempt is one tool call and what came back. Kept short on purpose: the whole
// ledger travels with every request.
type attempt struct {
	n      int
	what   string
	result string
}

const maxLedger = 10 // ledger entries kept; older ones are dropped

var (
	// pathLike matches a bare file name; anything holding a "/" is treated as a
	// path too. Both are the detail of a failure, not its identity: the same
	// error about another file is the same wall.
	pathLike = regexp.MustCompile(`^[\w.+-]+\.[A-Za-z][A-Za-z0-9]{0,5}$`)
	sigHexRe = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	sigNumRe = regexp.MustCompile(`\d+`)
)

var errWords = []string{"error", "fail", "exception", "panic", "cannot", "can't", "no such", "not found", "undefined", "refused", "denied", "invalid", "unexpected"}

// failSignature reduces a failed tool output to a stable key, so that two
// different commands that hit the same wall get the same signature. Paths and
// numbers are stripped: the same error on another line is the same error.
func failSignature(out string) string {
	line := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "[harness]") || strings.HasPrefix(l, "exit_code:") {
			continue
		}
		low := strings.ToLower(l)
		if line == "" {
			line = l
		}
		for _, w := range errWords {
			if strings.Contains(low, w) {
				line = l
				goto done
			}
		}
	}
done:
	// Blank out paths and file names token by token, so a quoted name that
	// carries the meaning ("cannot find module 'auth'") survives.
	toks := strings.Fields(line)
	for i, t := range toks {
		bare := strings.Trim(t, `"'`+"`"+`,;:()[]{}<>`)
		if bare == "" {
			continue
		}
		if strings.Contains(bare, "/") || pathLike.MatchString(bare) {
			toks[i] = strings.Replace(t, bare, "<path>", 1)
		}
	}
	line = strings.Join(toks, " ")
	line = sigHexRe.ReplaceAllString(line, "<n>")
	line = sigNumRe.ReplaceAllString(line, "<n>")
	return clip(strings.ToLower(line), 80)
}

func toolClass(name string) string {
	switch name {
	case "read_file", "list_files":
		return "reading"
	case "write_file", "edit_file":
		return "editing files"
	case "bash":
		return "running commands"
	}
	return "that tool"
}

// noteResult records one call in the ledger and tracks how often the same
// failure comes back. Only real progress (a passing verification) clears the
// streak: a successful read between two identical failures is not progress.
func (st *turnState) noteResult(name string, res ToolResult) {
	st.calls++
	at := attempt{n: st.calls, what: name}
	if res.Summary != "" {
		at.what = clip(res.Summary, 56)
	}
	switch {
	case res.Denied:
		at.result = "denied by the user"
	case res.Failed || res.Invalid:
		sig := res.ErrKey
		if sig == "" {
			sig = failSignature(res.Output)
		}
		if sig == "" { // a failure with no message at all: group it per tool
			sig = "unclear failure in " + name
		}
		// Calling the very same thing again is the same wall carried on, not a
		// new kind of failure, so it moves the ladder up instead of resetting it.
		if strings.HasPrefix(sig, "loop:") && st.failSig != "" {
			sig = st.failSig
		}
		at.result = "FAILED: " + sig
		if sig == st.failSig {
			st.failStreak++
			at.result += fmt.Sprintf(" (same failure as #%d)", st.failFirst)
		} else {
			st.failSig, st.failStreak, st.failFirst, st.rungDone = sig, 1, at.n, 0
			st.evidence = true // a new kind of failure is something learned
		}
		// Going back to content the file already had is proof the idea is
		// wrong, not one more try: it starts at the diagnosis rung.
		if strings.HasPrefix(sig, "oscillation:") && st.failStreak < 2 {
			st.failStreak = 2
		}
	default:
		at.result = "ok"
		if res.Status != "" {
			at.result = clip(res.Status, 40)
		}
		if res.Verify {
			st.failSig, st.failStreak, st.rungDone = "", 0, 0
		}
	}
	st.attempts = append(st.attempts, at)
	if len(st.attempts) > maxLedger {
		st.attempts = st.attempts[len(st.attempts)-maxLedger:]
	}
	st.lastClass = toolClass(name)
}

// ledger is the block resent with every request, so what was already tried
// cannot scroll out of the context window.
func (st *turnState) ledger() string {
	if len(st.attempts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[already attempted this turn - these results are final, running them again returns the same thing]")
	for _, at := range st.attempts {
		fmt.Fprintf(&b, "\n%d. %s -> %s", at.n, at.what, at.result)
	}
	if st.failStreak >= 2 {
		fmt.Fprintf(&b, "\nThe same failure has now survived %d attempts, so the idea behind them is what is wrong, not the details.", st.failStreak)
	}
	return b.String()
}

// escalate returns the harness message for the rung this failure has reached,
// and whether the turn should stop. Each rung asks for a different kind of
// step; the same nudge repeated is what a small model learns to ignore.
func (st *turnState) escalate(canAsk bool) (screen, msg string, stop bool) {
	if st.failStreak < 2 || st.failStreak <= st.rungDone {
		return "", "", false
	}
	// A protocol failure (broken JSON, unknown tool, wrong arguments) is the
	// model failing to speak, not to think: asking it to diagnose its idea
	// helps nothing, and the tool signature it needs was already sent with the
	// first correction. Give it one more try, then stop.
	if isProtocolFailure(st.failSig) {
		if st.failStreak < 3 {
			return "", "", false
		}
		st.rungDone = st.failStreak
		return fmt.Sprintf("kesalahan format yang sama %dx: giliran dihentikan", st.failStreak),
			correction(
				fmt.Sprintf("You made the same call-format mistake %d times (%s).", st.failStreak, st.failSig),
				"The tool signature and an example were already sent; repeating the call will not change the result.",
				"Stop calling tools. Tell the user in plain words what you were trying to do, so they can rephrase the request.", ""),
			true
	}
	st.rungDone = st.failStreak
	switch st.failStreak {
	case 2:
		return "kegagalan yang sama 2x: minta diagnosis",
			correction(
				"Two attempts now failed the same way: "+st.failSig+".",
				"Variations of one idea keep hitting the same wall, so the idea is what is wrong, not the wording of the command.",
				"Before any other tool call, write three short lines: (a) what you expected, (b) what actually happened, (c) which of your assumptions is now proven wrong. Then state ONE new hypothesis and test only that.", ""),
			false
	case 3:
		return "kegagalan yang sama 3x: paksa ganti jenis langkah",
			correction(
				fmt.Sprintf("The same failure (%s) has survived %d attempts, mostly by %s.", st.failSig, st.failStreak, st.lastClass),
				"You keep working at the same level, so you keep getting the same answer. What is missing is evidence, not another attempt.",
				"Change the kind of step: "+switchAdvice(st.lastClass)+" Gather that evidence before touching the same thing again.", ""),
			false
	case 4:
		if canAsk {
			st.askDemanded = true
			return "kegagalan yang sama 4x: harus bertanya ke user",
				correction(
					"Four attempts failed the same way ("+st.failSig+").",
					"At this point you are guessing, and guessing costs the user more than asking does.",
					"Call ask_user now. Say in one line what is blocking you, and offer 2-4 concrete options (for example: a different approach, a file you may be missing, or a decision only the user can make).", `ask_user {"question": "Test masih gagal di modul auth. Mana yang benar?", "options": ["Token dibuat di server", "Token dibuat di client", "Lewati dulu, kerjakan bagian lain"]}`),
				false
		}
		fallthrough
	default:
		return fmt.Sprintf("kegagalan yang sama %dx: giliran dihentikan", st.failStreak),
			correction(
				"The same failure repeated "+fmt.Sprint(st.failStreak)+" times and the approach is not converging.",
				"Continuing would burn the user's tokens on the same wall.",
				"Stop here. Tell the user in a few lines: what you tried, what the failure was, and what you would need (a file, a decision, a command output) to get further.", ""),
			true
	}
}

// isProtocolFailure reports whether the signature is about how the call was
// written rather than about the task itself.
func isProtocolFailure(sig string) bool {
	for _, p := range []string{"bad_json:", "bad_args:", "unknown_tool:", "plan_mode"} {
		if strings.HasPrefix(sig, p) {
			return true
		}
	}
	return false
}

func switchAdvice(class string) string {
	switch class {
	case "editing files":
		return "read the file and the exact place the error points to, instead of editing it again."
	case "running commands":
		return "open the file the error names with read_file, or list the directory, instead of running another command."
	case "reading":
		return "reproduce the problem with bash (run the test, the file, or the failing command) instead of reading more."
	}
	return "use a different kind of tool than the one you have been using."
}
