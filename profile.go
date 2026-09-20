package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Thinking describes how a model's reasoning is switched and returned.
type Thinking struct {
	// Control: body | chat_template_kwargs | reasoning_effort | prompt_switch | always | none
	Control      string         `json:"control"`
	OnBody       map[string]any `json:"on_body,omitempty"`  // control=body: merged into the request when thinking
	OffBody      map[string]any `json:"off_body,omitempty"` // control=body: merged when not thinking
	Kwarg        string         `json:"kwarg,omitempty"`
	OnValue      any            `json:"on_value,omitempty"`
	OffValue     any            `json:"off_value,omitempty"`
	PromptOn     string         `json:"prompt_on,omitempty"`
	PromptOff    string         `json:"prompt_off,omitempty"`
	EffortOn     string         `json:"effort_on,omitempty"`
	EffortOff    string         `json:"effort_off,omitempty"`
	Tags         []string       `json:"tags,omitempty"`
	ImplicitOpen bool           `json:"implicit_open,omitempty"`
	History      string         `json:"history,omitempty"` // drop | keep_current_turn | keep
}

type Sampling struct {
	Think   map[string]any `json:"think,omitempty"`
	NoThink map[string]any `json:"no_think,omitempty"`
}

// Profile holds everything model-specific. Nothing else in lchat looks at the
// model name.
type Profile struct {
	Name       string         `json:"name,omitempty"`
	Match      string         `json:"match"`               // glob on the model name, case-insensitive
	MatchURL   string         `json:"match_url,omitempty"` // optional glob on the base URL
	Thinking   Thinking       `json:"thinking"`
	Sampling   Sampling       `json:"sampling,omitempty"`
	ToolFormat []string       `json:"tool_format,omitempty"`
	Ctx        int            `json:"ctx,omitempty"`
	ExtraBody  map[string]any `json:"extra_body,omitempty"`

	source string
}

type profileFile struct {
	Profiles []Profile `json:"profiles"`
}

var defaultToolFormat = []string{"native", "hermes", "qwen_xml", "json_block"}

// Built-in profiles. A user file (~/.config/lchat/models.json) overrides these.
const builtinProfiles = `{"profiles": [
  {"name": "default", "match": "*", "thinking": {"control": "none"}, "ctx": 32768},
  {"name": "qwen3", "match": "*qwen3*",
   "thinking": {"control": "chat_template_kwargs", "kwarg": "enable_thinking", "on_value": true, "off_value": false},
   "sampling": {"think": {"temperature": 0.6, "top_p": 0.95, "top_k": 20},
                "no_think": {"temperature": 0.7, "top_p": 0.8, "top_k": 20}},
   "ctx": 65536},
  {"name": "qwen3-openrouter", "match": "*qwen3*", "match_url": "*openrouter.ai*",
   "thinking": {"control": "body", "on_body": {"reasoning": {"enabled": true}}, "off_body": {"reasoning": {"enabled": false}}},
   "sampling": {"think": {"temperature": 0.6, "top_p": 0.95, "top_k": 20},
                "no_think": {"temperature": 0.7, "top_p": 0.8, "top_k": 20}},
   "ctx": 65536},
  {"name": "qwen3-instruct", "match": "*qwen3*instruct*", "thinking": {"control": "none"},
   "sampling": {"no_think": {"temperature": 0.7, "top_p": 0.8, "top_k": 20}}, "ctx": 65536},
  {"name": "qwen3-thinking", "match": "*qwen3*thinking*", "thinking": {"control": "always", "implicit_open": true},
   "sampling": {"think": {"temperature": 0.6, "top_p": 0.95, "top_k": 20}}, "ctx": 65536},
  {"name": "thinking", "match": "*thinking*", "thinking": {"control": "always"}}
]}`

func loadBuiltins() []Profile {
	var f profileFile
	if err := json.Unmarshal([]byte(builtinProfiles), &f); err != nil {
		panic(err)
	}
	for i := range f.Profiles {
		f.Profiles[i].source = "bawaan"
	}
	return f.Profiles
}

// LoadUserProfiles reads the optional profile file. JSON with // comments is
// accepted. A missing file is not an error.
func LoadUserProfiles(path string) ([]Profile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f profileFile
	if err := json.Unmarshal([]byte(stripJSONComments(string(b))), &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range f.Profiles {
		f.Profiles[i].source = path
	}
	return f.Profiles, nil
}

func stripJSONComments(s string) string {
	var b strings.Builder
	in, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if in {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				in = false
			}
			b.WriteByte(c)
			continue
		}
		if c == '"' {
			in = true
		} else if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// globMatch matches a case-insensitive pattern where * matches any run of
// characters, including '/'.
func globMatch(pat, s string) bool {
	pat, s = strings.ToLower(pat), strings.ToLower(s)
	parts := strings.Split(pat, "*")
	if len(parts) == 1 {
		return pat == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

func specificity(p *Profile) int {
	n := len(strings.ReplaceAll(p.Match, "*", ""))
	if p.MatchURL != "" {
		n += 1000 // an endpoint-specific profile beats any name-only one
	}
	return n
}

// SelectProfile picks the most specific matching profile. On a tie, user
// profiles win over built-ins.
func SelectProfile(model, baseURL string, user []Profile) *Profile {
	var best *Profile
	bestScore := -1
	all := append(slices.Clone(user), loadBuiltins()...)
	for i := range all {
		p := &all[i]
		if !globMatch(p.Match, model) || (p.MatchURL != "" && !globMatch(p.MatchURL, baseURL)) {
			continue
		}
		if s := specificity(p); s > bestScore {
			best, bestScore = p, s
		}
	}
	out := *best
	out.normalize()
	return &out
}

func (p *Profile) normalize() {
	t := &p.Thinking
	if t.Control == "" {
		t.Control = "none"
	}
	if t.Kwarg == "" {
		t.Kwarg = "enable_thinking"
	}
	if t.OnValue == nil {
		t.OnValue = true
	}
	if t.OffValue == nil {
		t.OffValue = false
	}
	if t.PromptOn == "" {
		t.PromptOn = "/think"
	}
	if t.PromptOff == "" {
		t.PromptOff = "/no_think"
	}
	if t.EffortOn == "" {
		t.EffortOn = "medium"
	}
	if len(t.Tags) != 2 {
		t.Tags = []string{"<think>", "</think>"}
	}
	if t.History == "" {
		t.History = "drop"
	}
	if len(p.ToolFormat) == 0 {
		p.ToolFormat = defaultToolFormat
	}
	if p.Ctx == 0 {
		p.Ctx = 32768
	}
	if p.Name == "" {
		p.Name = p.Match
	}
}

// CanSwitch reports whether thinking can be turned on and off per request.
func (p *Profile) CanSwitch() bool {
	switch p.Thinking.Control {
	case "body", "chat_template_kwargs", "reasoning_effort", "prompt_switch":
		return true
	}
	return false
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			dm, _ := dst[k].(map[string]any)
			nm := map[string]any{}
			mergeInto(nm, dm)
			mergeInto(nm, sm)
			dst[k] = nm
			continue
		}
		dst[k] = v
	}
}

// applyThinking translates "think or not" into request parameters. It returns
// whether the model is expected to think and the messages to send (only
// prompt_switch changes them).
func (p *Profile) applyThinking(body map[string]any, msgs []Message, want bool) (bool, []Message) {
	t := p.Thinking
	switch t.Control {
	case "body":
		if want {
			mergeInto(body, t.OnBody)
		} else {
			mergeInto(body, t.OffBody)
		}
		return want, msgs
	case "chat_template_kwargs":
		v := t.OffValue
		if want {
			v = t.OnValue
		}
		mergeInto(body, map[string]any{"chat_template_kwargs": map[string]any{t.Kwarg: v}})
		return want, msgs
	case "reasoning_effort":
		if want {
			body["reasoning_effort"] = t.EffortOn
		} else if t.EffortOff != "" {
			body["reasoning_effort"] = t.EffortOff
		}
		return want, msgs
	case "prompt_switch":
		tag := t.PromptOff
		if want {
			tag = t.PromptOn
		}
		out := slices.Clone(msgs)
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Role == "user" {
				out[i].Content += "\n" + tag
				break
			}
		}
		return want, out
	case "always":
		return true, msgs
	}
	return false, msgs
}

func (p *Profile) applySampling(body map[string]any, thinking bool) {
	s, alt := p.Sampling.NoThink, p.Sampling.Think
	if thinking {
		s, alt = alt, s
	}
	if s == nil {
		s = alt
	}
	for k, v := range s {
		body[k] = v
	}
}

func (p *Profile) parseOpts(thinking bool) ParseOpts {
	return ParseOpts{
		ThinkOpen:    p.Thinking.Tags[0],
		ThinkClose:   p.Thinking.Tags[1],
		ImplicitOpen: p.Thinking.ImplicitOpen && thinking,
	}
}

func (p *Profile) Describe() string {
	return fmt.Sprintf("%s (%s; thinking: %s; ctx %dk)", p.Name, p.source, p.Thinking.Control, p.Ctx/1024)
}

// ---------------------------------------------------------------------------
// Probe: find out how a model behaves and suggest a profile.

type probeResult struct {
	label string
	resp  *Response
	err   error
}

func (r probeResult) thinks() bool { return r.err == nil && r.resp.Reasoning != "" }

func (r probeResult) describe() string {
	if r.err != nil {
		msg := r.err.Error()
		if len(msg) > 90 {
			msg = msg[:90] + "…"
		}
		return "error: " + msg
	}
	if r.thinks() {
		via := r.resp.ReasoningVia
		if r.resp.StrayClose {
			via = "tag tanpa pembuka"
		}
		return fmt.Sprintf("reasoning ✓ (%s, %d char)", via, len(r.resp.Reasoning))
	}
	return "reasoning ✗"
}

var probeTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "get_weather",
		"description": "Get the current weather for a city.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []string{"city"},
		},
	},
}

// RunProbe sends a few small requests and returns a suggested profile.
func RunProbe(ctx context.Context, c *Client, model string, base *Profile, w io.Writer) *Profile {
	q := "What is 17+25? Answer with just the number."
	send := func(label string, extra map[string]any, content string, tools bool) probeResult {
		body := map[string]any{
			"model":      model,
			"messages":   []Message{{Role: "user", Content: content}},
			"max_tokens": 1500,
		}
		if tools {
			body["tools"] = []any{probeTool}
		}
		mergeInto(body, extra)
		r, err := c.Chat(ctx, body, ParseOpts{}, Handlers{})
		pr := probeResult{label: label, resp: r, err: err}
		fmt.Fprintf(w, "  %-34s %s\n", label, pr.describe())
		return pr
	}
	fmt.Fprintf(w, "Probe %s @ %s\n", model, c.BaseURL)
	plain := send("default (tanpa switch)", nil, q, false)
	bodyOn := send("reasoning.enabled=true", map[string]any{"reasoning": map[string]any{"enabled": true}}, q, false)
	bodyOff := send("reasoning.enabled=false", map[string]any{"reasoning": map[string]any{"enabled": false}}, q, false)
	kwOn := send("chat_template_kwargs thinking=true", map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}, q, false)
	kwOff := send("chat_template_kwargs thinking=false", map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}, q, false)
	noThink := send("prompt /no_think", nil, q+"\n/no_think", false)

	p := *base
	p.Name = "probe:" + model
	p.Match = strings.ToLower(model)
	p.MatchURL = ""
	p.source = ""
	t := Thinking{Tags: base.Thinking.Tags, History: base.Thinking.History}
	switch {
	case bodyOn.thinks() && bodyOff.err == nil && !bodyOff.thinks():
		t.Control = "body"
		t.OnBody = map[string]any{"reasoning": map[string]any{"enabled": true}}
		t.OffBody = map[string]any{"reasoning": map[string]any{"enabled": false}}
	case kwOn.thinks() && kwOff.err == nil && !kwOff.thinks():
		t.Control, t.Kwarg, t.OnValue, t.OffValue = "chat_template_kwargs", "enable_thinking", true, false
	case plain.thinks() && noThink.err == nil && !noThink.thinks():
		t.Control = "prompt_switch"
	case plain.thinks() || bodyOn.thinks() || kwOn.thinks():
		t.Control = "always"
	default:
		t.Control = "none"
	}
	for _, r := range []probeResult{plain, bodyOn, kwOn} {
		if r.err == nil && r.resp.StrayClose {
			t.ImplicitOpen = true
		}
	}
	p.Thinking = t
	// Probe results describe this model on this endpoint.
	if u, err := url.Parse(c.BaseURL); err == nil && u.Host != "" {
		p.MatchURL = "*" + u.Host + "*"
	}

	// Tool calling, with thinking off when possible.
	off := map[string]any{}
	p.normalize()
	p.applyThinking(off, nil, false)
	tool := send("tool call", off, "What's the weather in Jakarta? Use the get_weather tool.", true)
	format := p.ToolFormat
	if tool.err == nil {
		if len(tool.resp.ToolCalls) > 0 {
			format = defaultToolFormat
		} else {
			for _, f := range defaultToolFormat[1:] {
				if calls, _ := parseTextToolCalls(tool.resp.Content, []string{f}, func(string) bool { return true }); len(calls) > 0 {
					format = append([]string{"native", f}, slices.DeleteFunc(slices.Clone(defaultToolFormat[1:]), func(x string) bool { return x == f })...)
					fmt.Fprintf(w, "  %-34s tool call lewat teks (%s)\n", "", f)
					break
				}
			}
		}
		if len(tool.resp.ToolCalls) > 0 {
			fmt.Fprintf(w, "  %-34s native ✓ (%s)\n", "", tool.resp.ToolCalls[0].Function.Name)
		}
	}
	p.ToolFormat = format
	p.normalize()
	fmt.Fprintf(w, "Hasil: thinking control = %s, implicit_open = %v, tool_format = %v\n", t.Control, t.ImplicitOpen, p.ToolFormat)
	return &p
}

// compact clears thinking fields that don't apply to the chosen control, so
// saved profiles stay readable. normalize() fills them back in on load.
func (p *Profile) compact() *Profile {
	c := *p
	t := Thinking{Control: p.Thinking.Control, ImplicitOpen: p.Thinking.ImplicitOpen, History: p.Thinking.History}
	switch t.Control {
	case "body":
		t.OnBody, t.OffBody = p.Thinking.OnBody, p.Thinking.OffBody
	case "chat_template_kwargs":
		t.Kwarg, t.OnValue, t.OffValue = p.Thinking.Kwarg, p.Thinking.OnValue, p.Thinking.OffValue
	case "prompt_switch":
		t.PromptOn, t.PromptOff = p.Thinking.PromptOn, p.Thinking.PromptOff
	case "reasoning_effort":
		t.EffortOn, t.EffortOff = p.Thinking.EffortOn, p.Thinking.EffortOff
	}
	if tags := p.Thinking.Tags; len(tags) == 2 && (tags[0] != "<think>" || tags[1] != "</think>") {
		t.Tags = tags
	}
	c.Thinking = t
	return &c
}

// MarshalJSONText encodes without escaping <, > and &.
func MarshalJSONText(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return strings.TrimRight(b.String(), "\n")
}

// SaveProfile writes p into the profile file, replacing one with the same match.
func SaveProfile(path string, p *Profile) error {
	var f profileFile
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal([]byte(stripJSONComments(string(b))), &f); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	f.Profiles = slices.DeleteFunc(f.Profiles, func(x Profile) bool { return x.Match == p.Match && x.MatchURL == p.MatchURL })
	f.Profiles = append(f.Profiles, *p.compact())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(MarshalJSONText(f)+"\n"), 0o644)
}
