package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The planner is one (or two) calls to a model that writes no code and calls
// no tools: it splits a request into subtasks with checkable done-conditions.
// By default it is the main model, so a run costs nothing extra; a bigger
// model can be set with LCHAT_PLANNER_MODEL when planning is the weak point.

var (
	errPlanDirect  = errors.New("planner: do it directly")
	errPlanInvalid = errors.New("planner: no usable plan")
)

func (a *Agent) plannerModel() string {
	if a.cfg.PlannerModel != "" {
		return a.cfg.PlannerModel
	}
	return a.model
}

const plannerPrompt = `You are the planner for lchat, a coding agent in a terminal. You do not write code and you do not call tools. Your only job is to split one request into subtasks that a small model can actually finish.

Who carries them out: a SMALL model, one subtask at a time. It gets at most %d tool calls per subtask, and it starts every subtask with NO memory of the previous ones. It will see the project environment below, one line per finished subtask, and the subtask you wrote - nothing else. Write each subtask for someone who has read nothing else.

Rules:
1. Each subtask must fit in %d tool calls: roughly read one or two files, make one change, run one command. If it does not fit, split it.
2. Each subtask must carry "verify": ONE shell command whose exit code decides whether it is done. Exit 0 means done, anything else means not done. Prefer this project's own commands: %s. Narrow them when you can, so the command fails until this specific subtask is finished (go test ./pkg -run TestName, npm test -- path/to.test.js).
3. A subtask with no checkable command is not a subtask, it is a wish. If the result is a file that must exist or contain something, check that: test -f docs/api.md, grep -q "Usage" README.md. If the subtask produces a script or a command, verify by RUNNING it (bash tools/check.sh), not by grepping its text: a script that exists but is wrong must fail its own check. Never verify with cat, ls, echo, head or find: their exit code proves nothing. Never write a command that cannot fail (no "|| true", no trailing "; true").
4. The verify command must FAIL right now and PASS once the subtask is done. A command that already passes proves nothing; the harness runs it before the subtask starts and skips subtasks whose command is already green.
5. Order matters. Subtask k may assume every earlier subtask is finished and verified. No subtask may depend on a later one.
6. Between 2 and %d subtasks. Fewer is better. If the request really is one step - a single edit, a rename, a question, a file to read - answer "direct" instead. Do not invent work to fill a plan.
7. "title" is one imperative line, at most 80 characters. "detail" is at most three lines: which files to touch and what changes. Use real paths from the environment below. "files" lists the paths you expect to change.
8. Do not add subtasks the user did not ask for: no extra tests, no refactors, no documentation, unless the request asked for them.

Answer with one JSON object and nothing else. No prose, no code fence.

{"mode": "plan", "subtasks": [
  {"title": "Add sum() to src/math.js",
   "detail": "src/math.js has no sum. Add an exported sum(a, b) next to the existing helpers.",
   "files": ["src/math.js"],
   "verify": "npm test -- math",
   "expect": "exit 0, the sum test stops failing"}
]}

When the request is a single step, answer instead:

{"mode": "direct", "why": "one edit in one file"}`

type plannerReply struct {
	Mode     string    `json:"mode"`
	Why      string    `json:"why,omitempty"`
	Subtasks []Subtask `json:"subtasks,omitempty"`
}

// callPlanner asks for a plan (prev == nil) or, given the subtask that did not
// fit, for the parts to replace it with. It never touches a.msgs. A reply
// with problems gets exactly one correction; a second bad reply means no plan.
func (a *Agent) callPlanner(ctx context.Context, goal string, prev *TaskPlan) ([]Subtask, error) {
	model := a.plannerModel()
	prof := SelectProfile(model, a.cfg.BaseURL, a.user)
	sys := fmt.Sprintf(plannerPrompt, a.cfg.SubSteps, a.cfg.SubSteps, verifyExamples(a.env), maxSubtasks) + "\n\n" + a.env.Block()
	user := goal
	if prev != nil {
		user = splitRequest(prev)
	}
	msgs := []Message{{Role: "system", Content: sys}, {Role: "user", Content: user}}
	for attempt := 0; attempt < 2; attempt++ {
		// Thinking is forced on: planning is the one place where extra
		// reasoning buys shape, not variety. No tools key: the reply is JSON.
		resp, err := a.chatWith(ctx, model, prof, msgs, true, Handlers{Think: a.ui.Think})
		if err != nil {
			return nil, err
		}
		reply, problems := parsePlannerReply(resp.Content, a.env, prev != nil)
		if reply != nil && reply.Mode == "direct" {
			if prev == nil {
				return nil, errPlanDirect
			}
			problems = []string{`a split cannot be "direct": give 2 or 3 parts`}
		}
		if len(problems) == 0 {
			return reply.Subtasks, nil
		}
		if attempt == 1 {
			break
		}
		a.ui.Harness("rencana planner bermasalah, dikoreksi sekali: " + clip(strings.Join(problems, "; "), 120))
		msgs = append(msgs,
			Message{Role: "assistant", Content: resp.Content},
			Message{Role: "user", Content: correction(
				"The plan has problems: "+strings.Join(problems, "; ")+".",
				"A subtask without a real done-condition is a wish, not a subtask; the executor cannot know when to stop.",
				`Answer again with one JSON object that fixes every problem, or with {"mode": "direct"} if the request is really one step.`, "")})
	}
	return nil, errPlanInvalid
}

// parsePlannerReply turns the model's text into a reply and the list of
// problems with it. repairJSON already copes with code fences, prose around
// the object, trailing commas and Python literals.
func parsePlannerReply(content string, env *Env, isSplit bool) (*plannerReply, []string) {
	m, _, err := repairJSON(content)
	if err != nil || len(m) == 0 {
		return nil, []string{"the reply is not a JSON object"}
	}
	raw, _ := json.Marshal(m)
	var r plannerReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, []string{"subtasks must be objects with title, detail, files, verify"}
	}
	r.Mode = strings.ToLower(strings.TrimSpace(r.Mode))
	if r.Mode == "direct" {
		return &r, nil
	}
	problems := validatePlan(r.Subtasks, env)
	if isSplit && len(r.Subtasks) > 0 && (len(r.Subtasks) < 2 || len(r.Subtasks) > 3) {
		problems = append(problems, "a split must have 2 or 3 parts")
	}
	return &r, problems
}

// splitRequest is the user message for a split: the subtask that did not fit
// and the evidence from its run. The ledger is reused here as evidence for a
// fresh planner context, and never carried to the executor.
func splitRequest(p *TaskPlan) string {
	t := p.cur()
	var b strings.Builder
	fmt.Fprintf(&b, "Subtask %d of %d did not fit in one context:\n%s\nverify: %s\n", p.Cur+1, len(p.Subtasks), t.Title, t.Verify)
	if d := strings.TrimSpace(t.Detail); d != "" {
		b.WriteString(d + "\n")
	}
	fmt.Fprintf(&b, "\nWhy it stopped: %s\n", p.lastReason)
	if p.lastLedger != "" {
		b.WriteString("\nWhat the executor tried:\n" + p.lastLedger + "\n")
	}
	if len(p.lastFiles) > 0 {
		fmt.Fprintf(&b, "\nFiles it changed before stopping: %s\n", strings.Join(p.lastFiles, ", "))
	}
	b.WriteString("\nReplace it with 2 or 3 smaller subtasks. Make the FIRST one the thing it kept assuming and never checked. Each part needs its own verify command that fails now and passes when that part is done. Answer with the same JSON shape: {\"mode\": \"plan\", \"subtasks\": [...]}.")
	return b.String()
}

// chatWith is one request outside the agent loop: a given model and profile,
// given messages, no tools. Used by the planner and the final report.
func (a *Agent) chatWith(ctx context.Context, model string, prof *Profile, msgs []Message, think bool, h Handlers) (*Response, error) {
	body := map[string]any{"model": model}
	mergeInto(body, prof.ExtraBody)
	thinking, msgs := prof.applyThinking(body, msgs, think)
	prof.applySampling(body, thinking)
	if a.cfg.Temperature != nil {
		body["temperature"] = *a.cfg.Temperature
	}
	body["messages"] = msgs
	a.log.Event("aux_request", map[string]any{"model": model, "think": thinking, "msgs": len(msgs), "last": logClip(msgs[len(msgs)-1].Content, 4000)})
	a.ui.BeginResponse(thinking)
	resp, err := a.client.Chat(ctx, body, prof.parseOpts(thinking), h)
	a.ui.EndResponse()
	if err != nil {
		a.log.Event("aux_reply", map[string]any{"model": model, "err": err.Error()})
		return nil, err
	}
	if resp.Usage != nil {
		a.lastUsage = resp.Usage
	}
	a.log.Event("aux_reply", map[string]any{"model": model, "content": resp.Content, "reasoning": logClip(resp.Reasoning, 2000), "usage": resp.Usage})
	return resp, nil
}
