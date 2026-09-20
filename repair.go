package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Streaming tag splitter: separates <think>…</think> (reasoning) and
// <tool_call>…</tool_call> (hidden from screen, kept in content) from answer
// text, even when a tag is split across chunks.

const (
	kindText = iota
	kindThink
	kindHidden
)

type tagPair struct {
	open, close string
	kind        int
}

type splitter struct {
	pairs    []tagPair
	cur      int // index into pairs while inside a tagged block, else -1
	buf      string
	emit     func(kind int, s string)
	onStray  func()
	stray    bool
	sawClose bool
}

func newSplitter(pairs []tagPair, emit func(int, string)) *splitter {
	return &splitter{pairs: pairs, cur: -1, emit: emit}
}

func (s *splitter) write(chunk string) {
	s.buf += chunk
	s.drain(false)
}

func (s *splitter) flush() { s.drain(true) }

func (s *splitter) out(kind int, text string) {
	if text != "" {
		s.emit(kind, text)
	}
}

// partialSuffix returns the length of the longest suffix of buf that is a
// proper prefix of tag (a tag that may still be arriving).
func partialSuffix(buf, tag string) int {
	for k := min(len(tag)-1, len(buf)); k > 0; k-- {
		if strings.HasSuffix(buf, tag[:k]) {
			return k
		}
	}
	return 0
}

func (s *splitter) drain(final bool) {
	for s.buf != "" {
		if s.cur >= 0 {
			p := s.pairs[s.cur]
			if i := strings.Index(s.buf, p.close); i >= 0 {
				s.out(p.kind, s.buf[:i])
				if p.kind == kindHidden {
					s.out(kindHidden, p.close)
				} else {
					s.sawClose = true
				}
				s.buf = s.buf[i+len(p.close):]
				s.cur = -1
				continue
			}
			k := 0
			if !final {
				k = partialSuffix(s.buf, p.close)
			}
			s.out(p.kind, s.buf[:len(s.buf)-k])
			s.buf = s.buf[len(s.buf)-k:]
			return
		}
		best, bi, isClose := -1, -1, false
		for i, p := range s.pairs {
			if j := strings.Index(s.buf, p.open); j >= 0 && (best < 0 || j < best) {
				best, bi, isClose = j, i, false
			}
			if p.kind == kindThink {
				if j := strings.Index(s.buf, p.close); j >= 0 && (best < 0 || j < best) {
					best, bi, isClose = j, i, true
				}
			}
		}
		if best >= 0 {
			p := s.pairs[bi]
			if isClose {
				// Closing tag without an opening one: the template opened the
				// think block in the prompt, so what came before was reasoning.
				s.out(kindThink, s.buf[:best])
				s.stray, s.sawClose = true, true
				if s.onStray != nil {
					s.onStray()
				}
				s.buf = s.buf[best+len(p.close):]
				continue
			}
			s.out(kindText, s.buf[:best])
			if p.kind == kindHidden {
				s.out(kindHidden, p.open)
			}
			s.buf = s.buf[best+len(p.open):]
			s.cur = bi
			continue
		}
		k := 0
		if !final {
			for _, p := range s.pairs {
				k = max(k, partialSuffix(s.buf, p.open))
				if p.kind == kindThink {
					k = max(k, partialSuffix(s.buf, p.close))
				}
			}
		}
		s.out(kindText, s.buf[:len(s.buf)-k])
		s.buf = s.buf[len(s.buf)-k:]
		return
	}
}

// ---------------------------------------------------------------------------
// JSON argument repair.

var (
	reFence         = regexp.MustCompile("(?s)^\\s*```[a-zA-Z]*\\s*(.*?)\\s*```\\s*$")
	reTrailingComma = regexp.MustCompile(`,\s*([}\]])`)
	rePyLiteral     = regexp.MustCompile(`\b(True|False|None)\b`)
)

// repairJSON parses tool-call arguments, fixing the mistakes small models make
// most often. It returns the parsed object and whether a repair was needed.
func repairJSON(s string) (map[string]any, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return map[string]any{}, false, nil
	}
	var m map[string]any
	firstErr := json.Unmarshal([]byte(s), &m)
	if firstErr == nil && m != nil {
		return m, false, nil
	}
	try := func(c string) bool {
		var v any
		if json.Unmarshal([]byte(c), &v) != nil {
			return false
		}
		switch t := v.(type) {
		case map[string]any:
			m = t
			return true
		case string: // double-encoded
			var inner map[string]any
			if json.Unmarshal([]byte(t), &inner) == nil {
				m = inner
				return true
			}
		}
		return false
	}
	c := s
	if f := reFence.FindStringSubmatch(c); f != nil {
		c = f[1]
	}
	steps := []func(string) string{
		func(x string) string { return x },
		escapeControlInStrings,
		func(x string) string { return reTrailingComma.ReplaceAllString(x, "$1") },
		func(x string) string {
			if i, j := strings.Index(x, "{"), strings.LastIndex(x, "}"); i >= 0 && j > i {
				return x[i : j+1]
			}
			return x
		},
		func(x string) string {
			if !strings.Contains(x, `"`) {
				return strings.ReplaceAll(x, "'", `"`)
			}
			return x
		},
		func(x string) string {
			return rePyLiteral.ReplaceAllStringFunc(x, func(w string) string {
				return map[string]string{"True": "true", "False": "false", "None": "null"}[w]
			})
		},
	}
	for _, step := range steps {
		c = step(c)
		if try(c) {
			return m, true, nil
		}
	}
	return nil, false, firstErr
}

// escapeControlInStrings escapes raw newlines/tabs inside JSON strings, which
// models emit when writing file contents.
func escapeControlInStrings(s string) string {
	var b strings.Builder
	in, esc := false, false
	for _, r := range s {
		if in {
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == '"':
				in = false
			case r == '\n':
				b.WriteString(`\n`)
				continue
			case r == '\r':
				b.WriteString(`\r`)
				continue
			case r == '\t':
				b.WriteString(`\t`)
				continue
			}
		} else if r == '"' {
			in = true
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Tool calls written as text (when the server's tool parser is off or the
// model ignored the native format).

var (
	reHermes     = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*(?:</tool_call>|$)`)
	reQwenFunc   = regexp.MustCompile(`(?s)<function=([^>\s]+)>(.*?)(?:</function>|$)`)
	reQwenParam  = regexp.MustCompile(`(?s)<parameter=([^>\s]+)>\n?(.*?)\n?</parameter>`)
	reJSONBlock  = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")
	reQwenCallTx = regexp.MustCompile(`(?s)<tool_call>.*?(?:</tool_call>|$)`)
)

type textCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Params    json.RawMessage `json:"parameters"`
}

func jsonCall(raw string) (FunctionCall, bool) {
	var tc textCall
	if json.Unmarshal([]byte(raw), &tc) != nil || tc.Name == "" {
		m, _, err := repairJSON(raw)
		if err != nil {
			return FunctionCall{}, false
		}
		b, _ := json.Marshal(m)
		if json.Unmarshal(b, &tc) != nil || tc.Name == "" {
			return FunctionCall{}, false
		}
	}
	args := tc.Arguments
	if len(args) == 0 {
		args = tc.Params
	}
	a := string(args)
	if len(args) > 0 && args[0] == '"' {
		var s string
		_ = json.Unmarshal(args, &s)
		a = s
	}
	if a == "" {
		a = "{}"
	}
	return FunctionCall{Name: tc.Name, Arguments: a}, true
}

func qwenXMLCalls(s string) []FunctionCall {
	var out []FunctionCall
	for _, f := range reQwenFunc.FindAllStringSubmatch(s, -1) {
		args := map[string]any{}
		for _, p := range reQwenParam.FindAllStringSubmatch(f[2], -1) {
			args[p[1]] = p[2]
		}
		b, _ := json.Marshal(args)
		out = append(out, FunctionCall{Name: f[1], Arguments: string(b)})
	}
	return out
}

// parseTextToolCalls extracts tool calls from reply text, trying formats in
// order. known reports whether a name is a real tool; it guards the loose
// json_block format against ordinary JSON code in an answer.
func parseTextToolCalls(content string, formats []string, known func(string) bool) ([]ToolCall, string) {
	var fcs []FunctionCall
	rest := content
	for _, f := range formats {
		switch f {
		case "hermes":
			for _, m := range reHermes.FindAllStringSubmatch(content, -1) {
				inner := strings.TrimSpace(m[1])
				if strings.HasPrefix(inner, "<function=") {
					fcs = append(fcs, qwenXMLCalls(inner)...)
				} else if fc, ok := jsonCall(inner); ok {
					fcs = append(fcs, fc)
				}
			}
			if len(fcs) > 0 {
				rest = reQwenCallTx.ReplaceAllString(content, "")
			}
		case "qwen_xml":
			fcs = qwenXMLCalls(content)
			if len(fcs) > 0 {
				rest = reQwenCallTx.ReplaceAllString(content, "")
				rest = reQwenFunc.ReplaceAllString(rest, "")
			}
		case "json_block":
			for _, m := range reJSONBlock.FindAllStringSubmatch(content, -1) {
				if fc, ok := jsonCall(m[1]); ok && known(fc.Name) {
					fcs = append(fcs, fc)
					rest = strings.Replace(rest, m[0], "", 1)
				}
			}
			if t := strings.TrimSpace(content); len(fcs) == 0 && strings.HasPrefix(t, "{") {
				if fc, ok := jsonCall(t); ok && known(fc.Name) {
					fcs = append(fcs, fc)
					rest = ""
				}
			}
		}
		if len(fcs) > 0 {
			break
		}
	}
	if len(fcs) == 0 {
		return nil, content
	}
	calls := make([]ToolCall, len(fcs))
	for i, fc := range fcs {
		calls[i] = ToolCall{Type: "function", Function: fc}
	}
	return calls, strings.TrimSpace(rest)
}

// ensureIDs gives every tool call a unique id (text-parsed calls and some
// servers have none).
func ensureIDs(calls []ToolCall, step int) {
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d_%d", step, i)
		}
		calls[i].Type = "function"
	}
}
