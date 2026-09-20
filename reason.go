package main

import (
	"fmt"
	"path/filepath"
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
	errKey         string
	errStreak      int
	recent         []string // hashes of recent tool calls
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
