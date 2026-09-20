package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Config struct {
	BaseURL, APIKey, Model string
	Ctx                    int // 0 = from profile
	ToolMax                int
	Temperature            *float64
	MaxSteps               int
	Think                  string // auto | on | off
	Mode                   string // plan | ask | auto
	Yolo, CheckTS          bool
	Raw, Verbose           bool
	QuietThink             bool
	ModelsPath             string
	PlannerModel           string // /task: model that writes the plan; "" = the main model
	SubSteps               int    // /task: base step budget of one subtask
	Task                   bool   // -p --task: decompose the one-shot request
	LogDir                 string // session log directory; "" = off
}

// Asker asks the user a question and returns the answer line.
type Asker func(ctx context.Context, prompt string) (string, error)

type Agent struct {
	cfg       *Config
	client    *Client
	model     string
	profile   *Profile
	user      []Profile
	env       *Env
	ui        *UI
	ask       Asker // single-key confirmation
	askLine   Asker // free text
	always    map[string]bool
	cwd       string
	msgs      []Message
	turnStart int
	todos     []Todo
	mutations int
	readFiles map[string]bool
	pending   []Image // images attached to the next message
	lastUsage *Usage

	turn          *turnState // the running turn, for tools that must consult it
	todoMutations int        // a.mutations when the plan was last updated

	plan *TaskPlan   // a decomposed run in progress (see subtask.go)
	sub  *subState   // the subtask being executed
	log  *SessionLog // nil = no session log

	carry string // a harness note for the next turn (e.g. work left unfinished)
}

func NewAgent(cfg *Config, ui *UI, ask, askLine Asker, user []Profile, cwd string) *Agent {
	a := &Agent{
		cfg: cfg, client: NewClient(cfg.BaseURL, cfg.APIKey), ui: ui, ask: ask, askLine: askLine, user: user,
		always: map[string]bool{}, cwd: cwd, readFiles: map[string]bool{},
	}
	a.env = DetectEnv(cwd)
	a.SetModel(cfg.Model)
	a.Reset()
	return a
}

// Reconfigure applies a new endpoint, key and model at runtime (used by /config).
func (a *Agent) Reconfigure(base, key, model string) {
	a.cfg.BaseURL, a.cfg.APIKey = base, key
	a.client = NewClient(base, key)
	a.SetModel(model)
}

func (a *Agent) SetModel(model string) {
	a.model = model
	a.profile = SelectProfile(model, a.cfg.BaseURL, a.user)
}

func (a *Agent) Reset() {
	a.msgs = []Message{{Role: "system", Content: systemPrompt(a.env)}}
	a.todos = nil
	a.plan, a.sub = nil, nil
}

func (a *Agent) ctxLimit() int {
	if a.cfg.Ctx > 0 {
		return a.cfg.Ctx
	}
	return a.profile.Ctx
}

// RunTurn handles one user message until the model gives a final answer.
// Attach queues images to go with the next user message.
func (a *Agent) Attach(imgs ...Image) { a.pending = append(a.pending, imgs...) }

func (a *Agent) RunTurn(ctx context.Context, input string) error {
	a.turnStart = len(a.msgs)
	if a.carry != "" {
		// What the previous turn left behind rides in this message, so the
		// model sees it without a second consecutive user message.
		input = a.carry + "\n\n" + input
		a.carry = ""
	}
	msg := Message{Role: "user", Content: input, Images: a.pending}
	if n := len(a.pending); n > 0 {
		var names []string
		for _, im := range a.pending {
			names = append(names, im.label())
		}
		a.ui.Info(fmt.Sprintf("%d gambar dikirim: %s", n, strings.Join(names, ", ")))
		// A small model does not always notice an image part next to the
		// text; without this line it goes looking for the file with tools.
		msg.Content = strings.TrimSpace(msg.Content + "\n\n[attached image: " + strings.Join(names, ", ") + ". Look at it directly; do not analyze it with scripts.]")
		a.pending = nil
	}
	a.msgs = append(a.msgs, msg)
	defer a.endTurn()
	st := &turnState{needThink: true}
	a.turn, a.todoMutations = st, a.mutations
	defer func() { a.turn = nil }()
	var imgs []string
	for _, im := range msg.Images {
		imgs = append(imgs, im.label())
	}
	a.log.Event("turn", map[string]any{"input": input, "images": imgs, "model": a.model, "mode": a.cfg.Mode, "profile": a.profile.Describe()})
	start := time.Now()
	out, err := a.loop(ctx, st, a.cfg.MaxSteps)
	a.log.Event("turn_end", map[string]any{"outcome": out.String(), "steps": st.step, "seconds": time.Since(start).Seconds(),
		"err": errString(err), "checkpoints": len(st.handoffs)})
	if err != nil {
		return err
	}
	if out == outBudget {
		a.ui.Warn(fmt.Sprintf("berhenti: batas %d langkah tercapai (--max-steps)", a.cfg.MaxSteps))
		a.reportUnfinished(ctx, st)
		if a.plan == nil {
			// The reactive harness has just proven the task is too big for one
			// context; that is the one honest trigger for decomposition.
			a.ui.Info("tugas ini mungkin terlalu besar untuk satu giliran — pecah dengan /task <permintaan>")
		}
	}
	return nil
}

// reportUnfinished says what a turn that ran out of steps left behind: which
// files changed, and whether anything verifies the last change - running the
// project's verifier itself when nothing did. A refactor cut off halfway can
// build cleanly and still be broken; silence here is how it gets committed.
// The note is carried into the next turn, so the model starts from the truth.
func (a *Agent) reportUnfinished(ctx context.Context, st *turnState) {
	if len(st.mutated) == 0 {
		return
	}
	var files []string
	for _, p := range st.mutated {
		files = append(files, a.rel(p))
	}
	a.ui.Warn("file yang diubah giliran ini: " + strings.Join(files, ", "))
	state := "verified after the last change, but the work may still be incomplete"
	if !st.verified() {
		state = "NOT verified after the last change"
		if cmd := pickVerifier(a.env.VerifyCmds); cmd != "" && a.allowed(ctx, "bash", commandRisk(cmd)) {
			a.ui.Harness("perubahan belum diverifikasi: harness menjalankan " + cmd)
			a.ui.ToolStart("bash", cmd)
			res := runBash(ctx, a, map[string]any{"command": cmd})
			res.Summary = "bash " + clip(cmd, 60)
			a.ui.ToolDone(res)
			a.logTool(res, "harness")
			if res.Failed {
				a.ui.Warn("verifikasi GAGAL: proyek dalam keadaan rusak setelah giliran ini")
				state = "`" + cmd + "` FAILS after the last change:\n" + truncate(res.Output, 1500)
			} else {
				a.ui.Info("verifikasi lulus; tapi pekerjaannya mungkin belum tuntas, periksa sebelum melanjutkan")
				state = "`" + cmd + "` passes, but the work may still be incomplete"
			}
		}
	}
	a.carry = "[harness] The previous turn ran out of steps in the middle of the work. Files changed: " +
		strings.Join(files, ", ") + ". State: " + state + ". Check for half-finished changes (declared but unused variables, removed code without its replacement) before doing anything else."
}

type loopOutcome int

const (
	outAnswered loopOutcome = iota // finishCheck said the turn is really done
	outStopped                     // denial, ladder top, no-progress stop
	outBudget                      // ran out of steps
	outSub                         // task mode: the subtask asked to close or hand back
)

func (o loopOutcome) String() string {
	return [...]string{"answered", "stopped", "budget", "sub"}[o]
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// loop is the agent loop, shared by an ordinary turn and one subtask. Whoever
// calls it owns the context it runs in; it never rewrites a.msgs[:turnStart+1]
// except through checkpoint.
func (a *Agent) loop(ctx context.Context, st *turnState, maxSteps int) (loopOutcome, error) {
	for st.step = 0; st.step < maxSteps; st.step++ {
		a.fitContext()
		think := wantThink(a.cfg.Think, st)
		st.needThink = false
		resp, err := a.call(ctx, think, st)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) && hasImages(a.msgs) && strings.Contains(strings.ToLower(ae.Body), "image") {
				a.ui.Warn("model ini sepertinya tidak menerima gambar; coba /model dengan model yang mendukung gambar, lalu /clear")
			}
			return outStopped, err
		}
		calls, content := resp.ToolCalls, resp.Content
		fromText := false
		if len(calls) == 0 {
			if c, rest := parseTextToolCalls(content, a.profile.ToolFormat, func(n string) bool { return toolByName(normalizeToolName(n)) != nil }); len(c) > 0 {
				calls, content, fromText = c, rest, true
			}
		}
		ensureIDs(calls, st.step)
		am := Message{Role: "assistant", Content: content, ToolCalls: calls}
		if a.profile.Thinking.History != "drop" {
			am.ReasoningContent = resp.Reasoning
		}
		a.msgs = append(a.msgs, am)
		if fromText {
			a.ui.Harness("tool call ditulis sebagai teks, dikonversi otomatis")
		}

		if len(calls) == 0 {
			if resp.FinishReason == "length" && st.lengthNudges < 2 {
				st.lengthNudges++
				a.harness("balasan terpotong (batas token), minta lanjut", correction(
					"Your reply was cut off by the output token limit.",
					"Long replies (or a whole big file in one call) exceed the limit.",
					"Continue from where you stopped. For big files, write them in smaller parts (write_file, then edit_file).", ""))
				continue
			}
			if a.finishCheck(ctx, st, content) {
				if a.sub != nil && a.sub.want != subNone {
					return outSub, nil
				}
				return outAnswered, nil
			}
			continue
		}
		stop := a.runCalls(ctx, calls, st)
		if len(st.images) > 0 {
			// After the whole batch, so tool results stay contiguous behind
			// the assistant message that called them.
			var names []string
			for _, im := range st.images {
				names = append(names, im.label())
			}
			a.msgs = append(a.msgs, Message{Role: "user", Images: st.images,
				Content: "[harness] Attached image: " + strings.Join(names, ", ") + ". Look at it directly; do not analyze it with scripts."})
			st.images = nil
		}
		if stop {
			if a.sub != nil && a.sub.want != subNone {
				return outSub, nil
			}
			return outStopped, nil
		}
		if len(st.milestone) > 0 {
			a.checkpoint(st)
		}
		if stop := a.checkProgress(st); stop {
			if a.sub != nil && a.sub.want != subNone {
				return outSub, nil
			}
			return outStopped, nil
		}
		if err := ctx.Err(); err != nil {
			return outStopped, err
		}
	}
	return outBudget, nil
}

// harness shows a note to the user and sends a correction to the model.
func (a *Agent) harness(screen, msg string) {
	a.ui.Harness(screen)
	a.log.Event("harness", map[string]any{"screen": screen, "msg": msg})
	a.msgs = append(a.msgs, Message{Role: "user", Content: msg})
}

// finishCheck runs when the model gives a reply without tool calls. It
// returns true when the turn is really done.
func (a *Agent) finishCheck(ctx context.Context, st *turnState, content string) bool {
	if strings.TrimSpace(content) == "" {
		if st.emptyNudged {
			a.ui.Warn("model tidak memberi jawaban")
			return true
		}
		st.emptyNudged = true
		st.needThink = true
		a.harness("balasan kosong, minta lanjut", correction("Your reply was empty.",
			"Every reply must either call a tool or give the final answer.",
			"Continue the task, or give your final answer to the user.", ""))
		return false
	}
	if a.sub != nil {
		// A subtask ends through subtask_done, never through a final answer.
		// Verified but forgot to close: close it for the model, its text is
		// the summary. Not verified: one reminder, then the text is the
		// reason it is handed back.
		switch {
		case a.sub.verified:
			a.sub.want, a.sub.summary = subDone, clip(strings.TrimSpace(content), 160)
			a.ui.Harness("verifikasi sudah lulus tapi subtask belum ditutup: ditutup oleh harness")
		case !st.subReminded:
			st.subReminded = true
			st.needThink = true
			a.harness("jawaban akhir tidak menutup subtask, diingatkan", correction(
				"You gave a final answer, but the subtask is not closed.",
				"A subtask is finished when `"+a.sub.task.Verify+"` exits 0 and you call subtask_done; the harness has not seen that command pass.",
				"If it can be done here: run the command, then call subtask_done. If it cannot: reply with one line starting with BLOCKED: and what is missing.", ""))
			return false
		default:
			a.sub.want, a.sub.reason = subBlocked, clip(strings.TrimSpace(content), 200)
		}
		return true
	}
	// The same sentence twice with no tool call is a stall, whatever it says.
	// A live session had "Mari saya lihat struktur folder game" three times in
	// a row, thinking off, until every one-shot nudge was spent and the third
	// copy was accepted as the answer. Never pretend that is an answer.
	if norm := strings.Join(strings.Fields(strings.ToLower(content)), " "); norm == st.lastText {
		st.sameText++
		st.needThink = true
		if st.sameText >= 2 {
			a.ui.Error("model mengulang balasan yang sama 3x tanpa bertindak: giliran dihentikan, beri instruksi yang lebih spesifik")
			return true
		}
		a.harness("balasan identik dengan sebelumnya, tanpa aksi: diminta tool call saja", correction(
			"Your last two replies were identical and neither called a tool.",
			"Repeating the sentence changes nothing; only a tool call moves the task.",
			"Reply with a tool call only, no prose. Start with the first open step of your plan; if unsure, list the project.", `list_files {"path": "."}`))
		return false
	} else {
		st.lastText, st.sameText = norm, 0
	}
	if !st.intentNudged && looksLikeIntent(content) {
		// "Mari saya perbaiki…" with no tool call ended a turn in 3.5 seconds
		// and 0 steps in a live session. Announcing a step is not doing it.
		st.intentNudged = true
		st.needThink = true
		a.harness("balasan hanya mengumumkan langkah, tidak melakukannya: diminta bertindak", correction(
			"You announced what you will do, but called no tool: nothing was read or changed.",
			"Announcing a step is not doing it; the user is left with a promise.",
			"Do it now: call the tool (read_file, edit_file, bash...). If you are actually finished, give the final answer without announcing further actions.", ""))
		return false
	}
	if open := openTodos(a.todos); len(open) > 0 && !st.todoReminded {
		st.todoReminded = true
		a.harness(fmt.Sprintf("masih ada %d langkah di plan, diingatkan", len(open)), todoReminder(open))
		return false
	}
	if len(st.mutated) > 0 && !st.verified() {
		// The gate: when the project has a verification command, the harness
		// runs it itself instead of asking the model to. A final answer is
		// accepted on exit 0; a failure goes back as evidence.
		switch ran, ok, stop := a.runVerifier(ctx, st); {
		case stop:
			a.ui.Error("verifikasi terus gagal: giliran dihentikan, beri instruksi yang lebih spesifik")
			return true
		case ran && !ok:
			return false
		case ran && ok:
			// accepted
		case !st.verifyReminded:
			st.verifyReminded = true
			var files []string
			for _, p := range st.mutated {
				files = append(files, a.rel(p))
			}
			a.harness("perubahan belum diverifikasi, diingatkan", verifyReminder(files, a.env.VerifyCmds))
			return false
		}
	}
	if a.cfg.Mode == "plan" && !st.planAsked && a.ask != nil {
		st.planAsked = true
		ans, err := a.ask(ctx, "  jalankan rencana ini? [y=ya, mode auto-edit / t=tetap plan] ")
		if err == nil && ans == "y" {
			a.cfg.Mode = "auto"
			a.ui.Info("mode: auto-edit (rencana disetujui)")
			a.msgs = append(a.msgs, Message{Role: "user", Content: "[harness] The user approved the plan. Plan mode is off: carry it out step by step, then verify."})
			st.needThink = true
			return false
		}
		a.ui.Info("tetap di mode plan")
	}
	return true
}

// checkpoint compacts the context after a verified milestone: the turn's
// messages collapse back to the original request, and what was achieved
// crosses over as harness-recorded facts in the handoff, never as transcript.
// This is what keeps a long turn from growing past what a small model can hold.
func (a *Agent) checkpoint(st *turnState) {
	h := handoff{Items: st.milestone, Verify: st.verifyCmd}
	for _, p := range st.mutated {
		h.Files = append(h.Files, a.rel(p))
	}
	for i := len(a.msgs) - 1; i > a.turnStart; i-- {
		if m := a.msgs[i]; m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			h.Summary = clip(strings.TrimSpace(m.Content), 160)
			break
		}
	}
	st.handoffs = append(st.handoffs, h)
	before := len(a.msgs)
	a.msgs = slices.Clone(a.msgs[:a.turnStart+1]) // system, earlier turns, this request
	st.newWindow()
	a.ui.Harness(fmt.Sprintf("checkpoint: %s terverifikasi · konteks dipadatkan %d → %d pesan",
		strings.Join(h.Items, "; "), before, len(a.msgs)))
}

// checkProgress fires when a run of steps brought no evidence. Evidence is
// recorded by execTool and noteResult; here the window is measured in model
// steps. It never ends the turn on the first warning: like the ladder, it
// changes the ask each time.
func (a *Agent) checkProgress(st *turnState) (stop bool) {
	if st.evidence {
		st.progressAt, st.evidence = st.step, false
		return false
	}
	if st.step-st.progressAt < noProgressSteps {
		return false
	}
	st.progressAt = st.step
	st.idleFired++
	st.needThink = true
	screen, msg, stop := noProgress(st.idleFired, a.askLine != nil && a.sub == nil)
	if a.sub != nil && st.idleFired >= 2 && a.sub.want == subNone {
		a.sub.want, a.sub.reason, stop = subBlocked, fmt.Sprintf("no new evidence for %d steps", 2*noProgressSteps), true
	}
	a.harness(screen, msg)
	if stop {
		a.ui.Error("tidak ada kemajuan: giliran dihentikan, beri instruksi yang lebih spesifik")
	}
	return stop
}

// pickVerifier chooses the project's verification command: the first one that
// runs tests, else the first one known. A build passing says less than tests.
func pickVerifier(cmds []string) string {
	for _, c := range cmds {
		if strings.Contains(c, "test") {
			return c
		}
	}
	if len(cmds) > 0 {
		return cmds[0]
	}
	return ""
}

// runVerifier is the harness running the project's verification itself, when
// the model claims to be done without having verified. It goes through the
// same permission gate as a bash call by the model, and its result enters the
// ledger and the ladder like any other failure. ran is false when there was
// nothing to run or the user declined; stop is the ladder's top.
func (a *Agent) runVerifier(ctx context.Context, st *turnState) (ran, ok, stop bool) {
	cmd := pickVerifier(a.env.VerifyCmds)
	if cmd == "" {
		return false, false, false
	}
	a.ui.Harness("model bilang selesai tanpa verifikasi: harness menjalankan " + cmd)
	if !a.allowed(ctx, "bash", commandRisk(cmd)) {
		return false, false, false
	}
	a.ui.ToolStart("bash", cmd)
	res := runBash(ctx, a, map[string]any{"command": cmd})
	res.Summary = "bash " + clip(cmd, 60)
	a.ui.ToolDone(res)
	a.logTool(res, "harness")
	st.noteResult("bash", res)
	if !res.Failed {
		st.verifiedAfter = true
		a.ui.Info("verifikasi lulus, jawaban diterima")
		return true, true, false
	}
	st.needThink = true
	msg := correction(
		"You said the work was done, but the project's verification fails: `"+cmd+"` exited non-zero.",
		"The harness ran it because nothing verified your last change. A final answer is only accepted once this command passes.",
		"Read the failure below, fix its cause, run `"+cmd+"` yourself, then answer.", "") +
		"\n\n" + truncate(res.Output, 3000)
	screen := "verifikasi harness gagal, hasilnya dikirim ke model"
	if s, m, top := st.escalate(a.askLine != nil); m != "" {
		screen, msg = s, msg+"\n\n"+m
		stop = top
	}
	a.harness(screen, msg)
	return true, false, stop
}

func (a *Agent) call(ctx context.Context, think bool, st *turnState) (*Response, error) {
	body := map[string]any{"model": a.model, "tools": toolSchemas(a.sub != nil)}
	mergeInto(body, a.profile.ExtraBody)
	msgs := a.msgs
	// The mode note and the attempt ledger ride along in the system message:
	// it is rebuilt for every request, so fitContext can never elide them.
	var extra []string
	if note := modeNote(a.cfg.Mode); note != "" {
		extra = append(extra, note)
	}
	if hb := st.handoffBlock(a.todos); hb != "" {
		extra = append(extra, hb)
	}
	if a.sub != nil {
		extra = append(extra, a.sub.note(st.step))
	}
	if led := st.ledger(); led != "" {
		extra = append(extra, led)
	}
	if len(extra) > 0 {
		msgs = slices.Clone(msgs)
		msgs[0].Content += "\n\n" + strings.Join(extra, "\n\n")
	}
	msgs = staleImagesAsText(msgs)
	thinking, msgs := a.profile.applyThinking(body, msgs, think)
	a.profile.applySampling(body, thinking)
	if a.cfg.Temperature != nil {
		body["temperature"] = *a.cfg.Temperature
	}
	body["messages"] = msgs
	a.log.Event("request", map[string]any{"step": st.step, "think": thinking, "msgs": len(msgs), "tokens_est": estTokens(msgs),
		"extra": strings.Join(extra, "\n\n"), "tools": len(toolSchemas(a.sub != nil))})
	a.ui.BeginResponse(thinking)
	resp, err := a.client.Chat(ctx, body, a.profile.parseOpts(thinking), Handlers{Text: a.ui.Text, Think: a.ui.Think})
	a.ui.EndResponse()
	if err != nil {
		a.log.Event("reply", map[string]any{"step": st.step, "err": err.Error()})
		return nil, err
	}
	if resp.Usage != nil {
		a.lastUsage = resp.Usage
	}
	var calls []map[string]string
	for _, c := range resp.ToolCalls {
		calls = append(calls, map[string]string{"name": c.Function.Name, "args": logClip(c.Function.Arguments, 2000)})
	}
	a.log.Event("reply", map[string]any{"step": st.step, "content": resp.Content, "reasoning": logClip(resp.Reasoning, 2000),
		"calls": calls, "finish": resp.FinishReason, "usage": resp.Usage})
	return resp, nil
}

// staleImagesAsText sends an image only in the request right after it was
// attached; once the model has answered past it, the image part becomes a
// text note. Four live sessions with qwen3.5-35b-a3b via OpenRouter never
// produced a native tool call while an image part was in the request, and
// every session without one did. The model has its own notes on what it saw;
// read_file on the image attaches it again when it needs another look.
func staleImagesAsText(msgs []Message) []Message {
	lastAssistant := -1
	for i, m := range msgs {
		if m.Role == "assistant" {
			lastAssistant = i
		}
	}
	var out []Message
	for i, m := range msgs {
		if len(m.Images) == 0 || i > lastAssistant {
			if out != nil {
				out = append(out, m)
			}
			continue
		}
		if out == nil {
			out = append([]Message{}, msgs[:i]...)
		}
		var names []string
		for _, im := range m.Images {
			names = append(names, im.Name)
		}
		m.Images = nil
		m.Content = strings.TrimSpace(m.Content) + "\n[image " + strings.Join(names, ", ") + " was shown to you earlier; rely on your notes about it, or read_file its path to see it again.]"
		out = append(out, m)
	}
	if out == nil {
		return msgs
	}
	return out
}

// modeNote tells the model what the current mode allows.
func modeNote(mode string) string {
	if mode == "plan" {
		return "Current mode: PLAN. write_file and edit_file are blocked. Investigate with read_file, list_files and bash, use ask_user when something is genuinely the user's decision, then answer with a short plan: which files change, one line per change, and how it will be verified. Do not write code yet; the user approves the plan first."
	}
	return ""
}

func (a *Agent) runCalls(ctx context.Context, calls []ToolCall, st *turnState) (stop bool) {
	for i, c := range calls {
		if ctx.Err() != nil {
			for _, rest := range calls[i:] {
				a.msgs = append(a.msgs, Message{Role: "tool", ToolCallID: rest.ID, Content: "Cancelled by the user.", summary: rest.Function.Name})
			}
			return false
		}
		res := a.execTool(ctx, c, st)
		a.logTool(res, "model")
		a.msgs = append(a.msgs, Message{Role: "tool", ToolCallID: c.ID, Content: res.Output, summary: res.Summary})
		if res.Denied {
			// Like Claude Code: a denial ends the turn so the user can say
			// what they want instead of the model trying workarounds.
			for _, rest := range calls[i+1:] {
				a.msgs = append(a.msgs, Message{Role: "tool", ToolCallID: rest.ID, Content: "Skipped: the user denied an earlier call.", summary: rest.Function.Name})
			}
			a.ui.Info("izin ditolak: giliran dihentikan, ketik instruksi berikutnya")
			return true
		}
		st.noteResult(normalizeToolName(c.Function.Name), res)
		if res.Invalid || res.Failed {
			st.needThink = true
		}
		if res.Image != nil {
			st.images = append(st.images, *res.Image)
		}
		if a.sub != nil {
			// Inside a subtask the ladder is two rungs: diagnosis, then hand
			// the subtask back to the orchestrator, which owns the next move.
			if st.failStreak >= 3 && a.sub.want == subNone {
				a.sub.want, a.sub.reason = subBlocked, "the same failure kept coming back: "+st.failSig
				a.ui.Harness("subtask mentok pada kegagalan yang sama: dikembalikan ke orkestrator")
			}
			if a.sub.want != subNone {
				for _, rest := range calls[i+1:] {
					a.msgs = append(a.msgs, Message{Role: "tool", ToolCallID: rest.ID, Content: "Skipped: the subtask is closing.", summary: rest.Function.Name})
				}
				return true
			}
		}
		if screen, msg, stop := st.escalate(a.askLine != nil && a.sub == nil); msg != "" {
			for _, rest := range calls[i+1:] {
				a.msgs = append(a.msgs, Message{Role: "tool", ToolCallID: rest.ID, Content: "Skipped: the harness interrupted this batch.", summary: rest.Function.Name})
			}
			a.harness(screen, msg)
			if stop {
				a.ui.Error("pencarian tidak konvergen: giliran dihentikan, beri instruksi yang lebih spesifik")
				return true
			}
			st.needThink = true
			return false
		}
	}
	if len(a.todos) > 0 {
		last := &a.msgs[len(a.msgs)-1]
		last.Content += "\n\n" + todoReminderLine(a.todos)
	}
	return false
}

func callSummary(name string, args map[string]any) string {
	switch name {
	case "bash":
		return str(args, "command")
	case "read_file":
		s := str(args, "path")
		if o := num(args, "offset", 0); o > 0 {
			s += fmt.Sprintf(" (dari baris %d)", o)
		}
		return s
	case "list_files":
		s := str(args, "path")
		if s == "" {
			s = "."
		}
		if p := str(args, "pattern"); p != "" {
			s += " " + p
		}
		return s
	case "write_file", "edit_file":
		return str(args, "path")
	case "ask_user":
		return str(args, "question")
	case "todo":
		items, _ := args["items"].([]any)
		return fmt.Sprintf("%d langkah", len(items))
	}
	return ""
}

func (a *Agent) invalid(key, screen, msg string) ToolResult {
	a.ui.Harness(screen + " (koreksi dikirim)")
	return ToolResult{Output: msg, Invalid: true, ErrKey: key, Summary: key}
}

func (a *Agent) execTool(ctx context.Context, c ToolCall, st *turnState) ToolResult {
	raw := c.Function.Name
	name := normalizeToolName(raw)
	tool := toolByName(name)
	if tool == nil {
		var sigs []string
		for _, t := range activeTools(a.sub != nil) {
			sigs = append(sigs, t.signature())
		}
		return a.invalid("unknown_tool:"+raw, fmt.Sprintf("tool %q tidak dikenal", raw), correction(
			fmt.Sprintf("There is no tool named %q.", raw),
			"Only these tools exist: "+strings.Join(sigs, "; ")+".",
			"Call one of them. For shell commands (grep, ls, npm...) use bash.", `bash {"command": "grep -rn \"TODO\" src"}`))
	}
	if a.sub == nil && isTaskTool(name) {
		return a.invalid("out_of_scope:"+name, name+" di luar /task", correction(
			name+" only exists inside a /task run.",
			"There is no subtask to close in an ordinary turn.",
			"Give your final answer, or track steps with todo.", ""))
	}
	if a.sub != nil && name == "todo" {
		return a.invalid("out_of_scope:todo", "todo tidak ada di dalam subtask", correction(
			"todo is not available inside a subtask.",
			"The harness owns the plan in this run; the subtask at the end of the system prompt is the only plan there is.",
			"Work on that subtask; close it with subtask_done when its verify command passes.", ""))
	}
	args, repaired, err := repairJSON(c.Function.Arguments)
	if err != nil {
		return a.invalid("bad_json:"+name, name+": argumen bukan JSON valid", correction(
			fmt.Sprintf("The arguments for %s are not valid JSON (%v).", name, err),
			"Tool arguments must be one JSON object; strings need double quotes and escaped newlines (\\n).",
			"Call "+name+" again with a valid JSON object.", tool.Example))
	}
	if repaired {
		a.ui.Info("  (argumen JSON diperbaiki otomatis)")
	}
	args, problem := validateArgs(tool, args)
	if problem != "" {
		return a.invalid("bad_args:"+name, name+": "+problem, correction(
			fmt.Sprintf("Invalid arguments for %s: %s.", name, problem),
			"The expected signature is "+tool.signature()+".",
			"Call "+name+" again with the right arguments.", tool.Example))
	}
	summary := callSummary(name, args)
	if p := str(args, "path"); p != "" && filepath.IsAbs(p) {
		// The model often pastes the absolute cwd from <env>; the screen, the
		// ledger and the handoff all read better relative to the project.
		summary = strings.Replace(summary, p, a.rel(p), 1)
	}
	if a.cfg.Mode == "plan" && (name == "write_file" || name == "edit_file") {
		return a.invalid("plan_mode", "mode plan: "+name+" diblokir", correction(
			"Plan mode is on, so "+name+" is blocked.",
			"The user wants to see and approve a plan before any file changes.",
			"Finish investigating, then reply with a short plan (files, changes, how to verify). The user approves it with Tab or by answering the prompt.", ""))
	}

	if name == "edit_file" || name == "write_file" {
		p := a.resolve(str(args, "path"))
		if name == "edit_file" && st.unread[p] {
			rel := a.rel(p)
			return a.invalid("edit_unread:"+rel, rel+": diedit lagi tanpa dibaca ulang", correction(
				"You already edited "+rel+" and have not read it since.",
				"After an edit the file differs from the copy in your context; editing that old copy again is how old_string mismatches and duplicated code happen.",
				"Read the part you want to change (read_file with offset and limit), or run the verification, then edit.",
				`read_file {"path": "`+rel+`", "offset": 1, "limit": 80}`))
		}
		st.seenContent(p) // remember what the file held before this change
	}

	h := callHash(name, args, a.mutations)
	st.recent = append(st.recent, h)
	if len(st.recent) > 6 {
		st.recent = st.recent[len(st.recent)-6:]
	}
	if n := countOf(st.recent, h); n >= 3 {
		return a.invalid("loop:"+name, fmt.Sprintf("%s %s diulang %dx tanpa perubahan", name, clip(summary, 40), n), correction(
			fmt.Sprintf("You called %s with the same arguments %d times and nothing changed in between.", name, n),
			"Repeating it will give the same result.",
			"Use the result you already have: try a different approach, or give your final answer.", ""))
	}

	a.ui.ToolStart(name, summary)
	risk := a.toolRisk(name, args)
	if (tool.Perm || risk != "") && !a.allowed(ctx, name, risk) {
		res := ToolResult{Denied: true, Summary: name + " " + summary, Status: "tidak diizinkan",
			Output: "[harness] The user denied this call and the turn was stopped. Wait for the user's next message; do not retry this action or work around it unless the user asks."}
		a.ui.ToolDone(res)
		return res
	}
	res := tool.Run(ctx, a, args)
	res.Summary = name + " " + clip(summary, 60)
	a.ui.ToolDone(res)
	if a.sub != nil && name == "bash" {
		was := a.sub.verified
		a.sub.noteVerify(str(args, "command"), res) // the harness sees the exit code itself
		if a.sub.verified && !was {
			res.Output += "\n\n[harness] The done-condition has passed. Call subtask_done now with a one-line summary. Do not start other work: anything outside this subtask belongs to a later one."
		}
	}
	if res.Mutated != "" {
		a.mutations++
		st.noteMutation(res.Mutated, res.CheckOK)
		st.evidence = true
		if name == "edit_file" {
			st.noteEdited(res.Mutated)
		}
		if st.seenContent(res.Mutated) {
			rel := a.rel(res.Mutated)
			res.Failed, res.ErrKey = true, "oscillation: "+rel+" is back to content it already had"
			res.Status += " · kembali ke isi sebelumnya"
			res.Output += "\n\n" + correction(
				rel+" now holds content it already had earlier this turn.",
				"You undid your own change (A -> B -> A). Going back and forth means the idea behind both versions is wrong, not the wording of either.",
				"Do not touch this file again until you can say which assumption both versions shared. Then test that assumption instead of editing again.", "")
			a.ui.Harness("osilasi: " + rel + " kembali ke isi sebelumnya")
		}
	}
	if name == "read_file" && !res.Failed {
		st.noteRead(a.resolve(str(args, "path")))
	}
	if res.Verify && !res.Failed {
		st.noteVerified()
		if name == "bash" {
			st.verifyCmd = str(args, "command")
		}
	}
	if res.Failed && !res.Invalid {
		res.Output += "\n\n" + reflectHint
	}
	return res
}

// allowed asks the user for permission. A risky call (see guard.go) is asked
// every time, even with --yolo or "always", and blocked when nobody can answer.
func (a *Agent) allowed(ctx context.Context, tool, risk string) bool {
	autoEdit := a.cfg.Mode == "auto" && (tool == "write_file" || tool == "edit_file")
	if risk == "" && (a.cfg.Yolo || a.always[tool] || autoEdit) {
		return true
	}
	if a.ask == nil {
		if risk != "" {
			a.ui.Warn("diblokir (" + risk + "): butuh konfirmasi manual di terminal")
		} else {
			a.ui.Warn("butuh izin tapi input tidak interaktif; pakai --yolo")
		}
		return false
	}
	prompt := fmt.Sprintf("  izinkan %s? [y/N/a=selalu] ", tool)
	if risk != "" {
		prompt = fmt.Sprintf("  ⚠ %s. izinkan %s kali ini? [y/N] ", risk, tool)
	}
	ans, err := a.ask(ctx, prompt)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "y", "ya", "yes":
		return true
	case "a", "always", "selalu":
		if risk != "" {
			return true // this call only
		}
		a.always[tool] = true
		return true
	}
	return false
}

func callHash(name string, args map[string]any, mutations int) string {
	b, _ := json.Marshal(args) // map keys are sorted, so this is stable
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%d", name, b, mutations)))
	return hex.EncodeToString(sum[:8])
}

func countOf(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}

func hasImages(msgs []Message) bool {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

func (a *Agent) endTurn() {
	if a.profile.Thinking.History == "keep_current_turn" {
		for i := range a.msgs {
			a.msgs[i].ReasoningContent = ""
		}
	}
}

// ---------------------------------------------------------------------------
// Context budget.

func estTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) + len(m.ReasoningContent) + 16
		for _, im := range m.Images {
			n += len(im.Data) / 12 // measured: a 39 KB PNG was ~900 tokens (see session logs)
		}
		for _, c := range m.ToolCalls {
			n += len(c.Function.Name) + len(c.Function.Arguments) + 16
		}
	}
	return int(float64(n)/3.5) + 1500 // tool schemas
}

// fitContext elides old tool output past 70% of the budget and drops the
// oldest turns past 90%. The system prompt and the first user message stay.
func (a *Agent) fitContext() {
	limit := a.ctxLimit()
	if estTokens(a.msgs) < limit*70/100 {
		return
	}
	// Images are the most expensive thing in the history: drop old ones first.
	lastImg := -1
	for i, m := range a.msgs {
		if len(m.Images) > 0 {
			lastImg = i
		}
	}
	for i := range a.msgs {
		if i != lastImg && len(a.msgs[i].Images) > 0 {
			a.msgs[i].Content = strings.TrimSpace(a.msgs[i].Content + "\n[gambar lama dihapus dari konteks]")
			a.msgs[i].Images = nil
		}
	}
	var toolIdx []int
	for i, m := range a.msgs {
		if m.Role == "tool" && !m.elided {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) > 4 {
		for _, i := range toolIdx[:len(toolIdx)-4] {
			a.msgs[i].Content = fmt.Sprintf("[old output removed: %s]", a.msgs[i].summary)
			a.msgs[i].elided = true
		}
		a.ui.Info(fmt.Sprintf("konteks >70%%: %d output tool lama diringkas", len(toolIdx)-4))
	}
	dropped := 0
	for estTokens(a.msgs) >= limit*90/100 && a.turnStart > 2 {
		j := 3
		for j < len(a.msgs) && a.msgs[j].Role == "tool" {
			j++
		}
		a.msgs = append(a.msgs[:2], a.msgs[j:]...)
		a.turnStart -= j - 2
		dropped += j - 2
	}
	if dropped > 0 {
		a.ui.Info(fmt.Sprintf("konteks >90%%: %d pesan lama dibuang", dropped))
	}
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}
