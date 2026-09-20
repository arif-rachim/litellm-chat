package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectProfile(t *testing.T) {
	cases := []struct{ model, url, want string }{
		{"qwen3.5-35b-a3b", "http://localhost:4000", "qwen3"},
		{"qwen3.8-27b", "http://localhost:4000", "qwen3"}, // future versions fall into qwen3*
		{"Qwen/Qwen3.5-35B-A3B", "http://localhost:4000", "qwen3"},
		{"qwen/qwen3.5-35b-a3b", "https://openrouter.ai/api/v1", "qwen3-openrouter"},
		{"qwen3-30b-a3b-instruct-2507", "http://x", "qwen3-instruct"},
		{"qwen3-30b-a3b-thinking-2507", "http://x", "qwen3-thinking"},
		{"some-thinking-model", "http://x", "thinking"},
		{"gpt-oss", "http://x", "default"},
	}
	for _, c := range cases {
		if p := SelectProfile(c.model, c.url, nil); p.Name != c.want {
			t.Errorf("%s @ %s: got %s, want %s", c.model, c.url, p.Name, c.want)
		}
	}
}

func TestUserProfileOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	os.WriteFile(path, []byte(`{
  // comments are allowed
  "profiles": [
    {"name": "mine", "match": "*qwen3*", "thinking": {"control": "prompt_switch"}, "ctx": 131072}
  ]
}`), 0o644)
	user, err := LoadUserProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	p := SelectProfile("qwen3.5-35b-a3b", "http://localhost:4000", user)
	if p.Name != "mine" || p.Thinking.Control != "prompt_switch" || p.Ctx != 131072 {
		t.Fatalf("user profile should win a tie: %+v", p)
	}
	if p.Thinking.History != "drop" || len(p.ToolFormat) == 0 {
		t.Fatal("defaults not filled in")
	}
	if user, _ := LoadUserProfiles(filepath.Join(t.TempDir(), "none.json")); user != nil {
		t.Fatal("missing file should give no profiles")
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		ok     bool
	}{
		{"*qwen3*", "hosted_vllm/Qwen/Qwen3.5", true},
		{"*qwen3*instruct*", "qwen3-coder-instruct", true},
		{"*qwen3*instruct*", "qwen3-thinking", false},
		{"qwen", "qwen3", false},
		{"*", "", true},
	} {
		if globMatch(c.pat, c.s) != c.ok {
			t.Errorf("globMatch(%q, %q) != %v", c.pat, c.s, c.ok)
		}
	}
}

func bodyJSON(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func TestApplyThinking(t *testing.T) {
	msgs := []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "hi"}, {Role: "assistant", Content: "a"}, {Role: "tool", Content: "t"}}
	mk := func(th Thinking) *Profile {
		p := &Profile{Match: "*", Thinking: th}
		p.normalize()
		return p
	}
	cases := []struct {
		name string
		p    *Profile
		want bool
		on   string
		off  string
	}{
		{"kwargs", mk(Thinking{Control: "chat_template_kwargs"}), true,
			`{"chat_template_kwargs":{"enable_thinking":true}}`, `{"chat_template_kwargs":{"enable_thinking":false}}`},
		{"body", mk(Thinking{Control: "body", OnBody: map[string]any{"reasoning": map[string]any{"enabled": true}}, OffBody: map[string]any{"reasoning": map[string]any{"enabled": false}}}), true,
			`{"reasoning":{"enabled":true}}`, `{"reasoning":{"enabled":false}}`},
		{"effort", mk(Thinking{Control: "reasoning_effort", EffortOff: "low"}), true,
			`{"reasoning_effort":"medium"}`, `{"reasoning_effort":"low"}`},
		{"always", mk(Thinking{Control: "always"}), true, `{}`, `{}`},
		{"none", mk(Thinking{Control: "none"}), false, `{}`, `{}`},
	}
	for _, c := range cases {
		on, off := map[string]any{}, map[string]any{}
		gotOn, _ := c.p.applyThinking(on, msgs, true)
		c.p.applyThinking(off, msgs, false)
		if bodyJSON(on) != c.on || bodyJSON(off) != c.off || gotOn != c.want {
			t.Errorf("%s: on=%s off=%s thinking=%v", c.name, bodyJSON(on), bodyJSON(off), gotOn)
		}
	}
	// Existing chat_template_kwargs from extra_body are kept, not mutated.
	extra := map[string]any{"chat_template_kwargs": map[string]any{"foo": 1}}
	p := mk(Thinking{Control: "chat_template_kwargs"})
	body := map[string]any{}
	mergeInto(body, extra)
	p.applyThinking(body, msgs, true)
	if bodyJSON(body) != `{"chat_template_kwargs":{"enable_thinking":true,"foo":1}}` || len(extra["chat_template_kwargs"].(map[string]any)) != 1 {
		t.Fatalf("merge: %s / %v", bodyJSON(body), extra)
	}
	// prompt_switch appends to the last user message of a copy.
	ps := mk(Thinking{Control: "prompt_switch"})
	_, out := ps.applyThinking(map[string]any{}, msgs, false)
	if out[1].Content != "hi\n/no_think" || msgs[1].Content != "hi" {
		t.Fatalf("prompt_switch: %q (original %q)", out[1].Content, msgs[1].Content)
	}
}

func TestApplySampling(t *testing.T) {
	p := SelectProfile("qwen3.5", "http://x", nil)
	on, off := map[string]any{}, map[string]any{}
	p.applySampling(on, true)
	p.applySampling(off, false)
	if on["temperature"] != 0.6 || off["temperature"] != 0.7 || off["top_p"] != 0.8 {
		t.Fatalf("on=%v off=%v", on, off)
	}
	d := SelectProfile("unknown", "http://x", nil)
	b := map[string]any{}
	d.applySampling(b, false)
	if len(b) != 0 {
		t.Fatal("default profile should leave sampling to the server")
	}
}
