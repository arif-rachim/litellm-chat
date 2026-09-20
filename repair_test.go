package main

import (
	"strings"
	"testing"
)

func TestRepairJSON(t *testing.T) {
	cases := []struct {
		name, in string
		key      string
		want     any
		repaired bool
	}{
		{"valid", `{"path": "a.js"}`, "path", "a.js", false},
		{"fenced", "```json\n{\"path\": \"a.js\"}\n```", "path", "a.js", true},
		{"trailing comma", `{"path": "a.js",}`, "path", "a.js", true},
		{"double encoded", `"{\"path\": \"a.js\"}"`, "path", "a.js", true},
		{"raw newline in string", "{\"content\": \"line1\nline2\"}", "content", "line1\nline2", true},
		{"single quotes", `{'path': 'a.js'}`, "path", "a.js", true},
		{"python literal", `{"path": "a", "x": True}`, "x", true, true},
		{"text around", `here: {"path": "a.js"} ok`, "path", "a.js", true},
	}
	for _, c := range cases {
		m, rep, err := repairJSON(c.in)
		if err != nil {
			t.Errorf("%s: error %v", c.name, err)
			continue
		}
		if m[c.key] != c.want || rep != c.repaired {
			t.Errorf("%s: got %v (repaired=%v), want %v (repaired=%v)", c.name, m[c.key], rep, c.want, c.repaired)
		}
	}
	if _, _, err := repairJSON(`{"path": `); err == nil {
		t.Error("truncated JSON should fail")
	}
	if m, _, err := repairJSON(""); err != nil || len(m) != 0 {
		t.Error("empty arguments should give an empty object")
	}
}

// stream feeds content to an accumulator in the given chunks.
func stream(po ParseOpts, chunks ...string) (*Response, string, string) {
	var shownText, shownThink strings.Builder
	acc := newAccumulator(po, Handlers{
		Text:  func(s string) { shownText.WriteString(s) },
		Think: func(s string) { shownThink.WriteString(s) },
	})
	for _, c := range chunks {
		acc.content(c)
	}
	return acc.result(), shownText.String(), shownThink.String()
}

func TestThinkTagsSplitAcrossChunks(t *testing.T) {
	r, shown, think := stream(ParseOpts{}, "<thi", "nk>plan it</th", "ink>\n\nhello ", "world")
	if r.Reasoning != "plan it" || r.Content != "hello world" {
		t.Fatalf("reasoning=%q content=%q", r.Reasoning, r.Content)
	}
	if strings.Contains(shown, "think") || think != "plan it" {
		t.Fatalf("screen text=%q think=%q", shown, think)
	}
}

func TestStrayCloseTag(t *testing.T) {
	// The chat template opened <think> in the prompt, so only </think> arrives.
	r, _, _ := stream(ParseOpts{}, "let me see", "</think>", "answer")
	if r.Reasoning != "let me see" || r.Content != "answer" || !r.StrayClose {
		t.Fatalf("got reasoning=%q content=%q stray=%v", r.Reasoning, r.Content, r.StrayClose)
	}
}

func TestImplicitOpen(t *testing.T) {
	r, shown, _ := stream(ParseOpts{ImplicitOpen: true}, "reason", "ing</think>ans", "wer")
	if r.Reasoning != "reasoning" || r.Content != "answer" || shown != "answer" {
		t.Fatalf("got reasoning=%q content=%q shown=%q", r.Reasoning, r.Content, shown)
	}
	// Never closed: it was the answer after all.
	r, _, _ = stream(ParseOpts{ImplicitOpen: true}, "just an answer")
	if r.Content != "just an answer" || r.Reasoning != "" {
		t.Fatalf("unclosed implicit: content=%q reasoning=%q", r.Content, r.Reasoning)
	}
}

func TestReasoningFieldResetsImplicit(t *testing.T) {
	acc := newAccumulator(ParseOpts{ImplicitOpen: true}, Handlers{})
	acc.reasoning("thought")
	acc.content("answer")
	r := acc.result()
	if r.Reasoning != "thought" || r.Content != "answer" || r.ReasoningVia != "field" {
		t.Fatalf("got %+v", r)
	}
}

func TestToolCallTextHiddenFromScreen(t *testing.T) {
	r, shown, _ := stream(ParseOpts{}, "Reading it. <tool_", `call>{"name": "read_file", "arguments": {"path": "a"}}</tool_call>`)
	if shown != "Reading it. " {
		t.Fatalf("screen shows %q", shown)
	}
	if !strings.Contains(r.Content, "<tool_call>") {
		t.Fatalf("content lost the tool call: %q", r.Content)
	}
}

func known(n string) bool { return toolByName(normalizeToolName(n)) != nil }

func TestParseTextToolCalls(t *testing.T) {
	cases := []struct {
		name, content, tool, argKey, argVal, rest string
	}{
		{"hermes", "Reading.\n<tool_call>\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.js\"}}\n</tool_call>", "read_file", "path", "a.js", "Reading."},
		{"hermes string args", `<tool_call>{"name": "bash", "arguments": "{\"command\": \"ls\"}"}</tool_call>`, "bash", "command", "ls", ""},
		{"qwen xml", "<tool_call>\n<function=bash>\n<parameter=command>\nnpm test\n</parameter>\n</function>\n</tool_call>", "bash", "command", "npm test", ""},
		{"json block", "I'll run it:\n```json\n{\"name\": \"bash\", \"arguments\": {\"command\": \"ls\"}}\n```", "bash", "command", "ls", "I'll run it:"},
		{"bare json", `{"name": "read_file", "parameters": {"path": "x"}}`, "read_file", "path", "x", ""},
	}
	for _, c := range cases {
		calls, rest := parseTextToolCalls(c.content, defaultToolFormat, known)
		if len(calls) != 1 {
			t.Errorf("%s: got %d calls", c.name, len(calls))
			continue
		}
		args, _, _ := repairJSON(calls[0].Function.Arguments)
		if calls[0].Function.Name != c.tool || args[c.argKey] != c.argVal || rest != c.rest {
			t.Errorf("%s: got %s %v rest=%q", c.name, calls[0].Function.Name, args, rest)
		}
	}
	// Ordinary JSON in an answer is not a tool call.
	calls, _ := parseTextToolCalls("```json\n{\"name\": \"demo\", \"version\": \"1.0.0\"}\n```", defaultToolFormat, known)
	if len(calls) != 0 {
		t.Errorf("package.json snippet parsed as a tool call")
	}
}
