package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	msg := Message{Role: "user", Content: input, Images: a.pending}
	if n := len(a.pending); n > 0 {
		var names []string
		for _, im := range a.pending {
			names = append(names, im.label())
		}
		a.ui.Info(fmt.Sprintf("%d gambar dikirim: %s", n, strings.Join(names, ", ")))
		a.pending = nil
	}
	a.msgs = append(a.msgs, msg)
	defer a.endTurn()
	st := &turnState{needThink: true}
	for st.step = 0; st.step < a.cfg.MaxSteps; st.step++ {
		a.fitContext()
		think := wantThink(a.cfg.Think, st)
		st.needThink = false
		resp, err := a.call(ctx, think)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) && hasImages(a.msgs) && strings.Contains(strings.ToLower(ae.Body), "image") {
				a.ui.Warn("model ini sepertinya tidak menerima gambar; coba /model dengan model yang mendukung gambar, lalu /clear")
			}
			return err
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
				return nil
			}
			continue
		}
		if stop := a.runCalls(ctx, calls, st); stop {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	a.ui.Warn(fmt.Sprintf("berhenti: batas %d langkah tercapai (--max-steps)", a.cfg.MaxSteps))
	return nil
}

// harness shows a note to the user and sends a correction to the model.
func (a *Agent) harness(screen, msg string) {
	a.ui.Harness(screen)
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
	if open := openTodos(a.todos); len(open) > 0 && !st.todoReminded {
		st.todoReminded = true
		a.harness(fmt.Sprintf("masih ada %d langkah di plan, diingatkan", len(open)), todoReminder(open))
		return false
	}
	if len(st.mutated) > 0 && !st.verified() && !st.verifyReminded {
		st.verifyReminded = true
		var files []string
		for _, p := range st.mutated {
			files = append(files, a.rel(p))
		}
		a.harness("perubahan belum diverifikasi, diingatkan", verifyReminder(files, a.env.VerifyCmds))
		return false
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

func (a *Agent) call(ctx context.Context, think bool) (*Response, error) {
	body := map[string]any{"model": a.model, "tools": toolSchemas()}
	mergeInto(body, a.profile.ExtraBody)
	msgs := a.msgs
	if note := modeNote(a.cfg.Mode); note != "" {
		msgs = slices.Clone(msgs)
		msgs[0].Content += "\n\n" + note
	}
	thinking, msgs := a.profile.applyThinking(body, msgs, think)
	a.profile.applySampling(body, thinking)
	if a.cfg.Temperature != nil {
		body["temperature"] = *a.cfg.Temperature
	}
	body["messages"] = msgs
	a.ui.BeginResponse(thinking)
	resp, err := a.client.Chat(ctx, body, a.profile.parseOpts(thinking), Handlers{Text: a.ui.Text, Think: a.ui.Think})
	a.ui.EndResponse()
	if err != nil {
		return nil, err
	}
	if resp.Usage != nil {
		a.lastUsage = resp.Usage
	}
	return resp, nil
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
		if res.Invalid || res.Failed {
			st.needThink = true
		}
		if res.Invalid {
			if res.ErrKey == st.errKey {
				st.errStreak++
			} else {
				st.errKey, st.errStreak = res.ErrKey, 1
			}
			if st.errStreak >= 3 {
				a.ui.Error(fmt.Sprintf("model mengulang kesalahan yang sama 3x (%s). Giliran dihentikan; coba perjelas instruksi.", res.ErrKey))
				for _, rest := range calls[i+1:] {
					a.msgs = append(a.msgs, Message{Role: "tool", ToolCallID: rest.ID, Content: "Skipped.", summary: rest.Function.Name})
				}
				return true
			}
		} else {
			st.errKey, st.errStreak = "", 0
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
		for _, t := range tools {
			sigs = append(sigs, t.signature())
		}
		return a.invalid("unknown_tool:"+raw, fmt.Sprintf("tool %q tidak dikenal", raw), correction(
			fmt.Sprintf("There is no tool named %q.", raw),
			"Only these tools exist: "+strings.Join(sigs, "; ")+".",
			"Call one of them. For shell commands (grep, ls, npm...) use bash.", `bash {"command": "grep -rn \"TODO\" src"}`))
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
	if a.cfg.Mode == "plan" && (name == "write_file" || name == "edit_file") {
		return a.invalid("plan_mode", "mode plan: "+name+" diblokir", correction(
			"Plan mode is on, so "+name+" is blocked.",
			"The user wants to see and approve a plan before any file changes.",
			"Finish investigating, then reply with a short plan (files, changes, how to verify). The user approves it with Tab or by answering the prompt.", ""))
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
	if res.Mutated != "" {
		a.mutations++
		st.noteMutation(res.Mutated, res.CheckOK)
	}
	if res.Verify && !res.Failed {
		st.verifiedAfter = true
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
			n += len(im.Data) / 2 // rough: base64 bytes weigh less than tokens
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
