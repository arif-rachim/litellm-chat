package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Message is one chat message in OpenAI format. Unexported fields are harness
// metadata and never reach the API.
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`

	Images []Image `json:"-"` // user messages: sent as image parts

	summary string // tool messages: short description shown when the output is elided
	elided  bool
}

// MarshalJSON sends a plain string content, or the OpenAI content-parts array
// when the message carries images.
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Images) == 0 {
		type plain Message
		return json.Marshal(plain(m))
	}
	parts := []any{}
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, im := range m.Images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + im.Mime + ";base64," + base64.StdEncoding.EncodeToString(im.Data)},
		})
	}
	out := map[string]any{"role": m.Role, "content": parts}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	return json.Marshal(out)
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Response is one assistant reply with reasoning already separated from content.
type Response struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        *Usage
	StrayClose   bool // a closing think tag arrived without an opening one
	ReasoningVia string
}

// Handlers receive text while it streams in. Either may be nil.
type Handlers struct {
	Text  func(string)
	Think func(string)
}

// ParseOpts controls how reasoning embedded in content is recognised.
type ParseOpts struct {
	ThinkOpen, ThinkClose string
	ImplicitOpen          bool // template already emitted the opening tag
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{BaseURL: baseURL, APIKey: apiKey, HTTP: &http.Client{}}
}

func (c *Client) endpoint() string {
	u := strings.TrimRight(c.BaseURL, "/")
	if strings.HasSuffix(u, "/v1") {
		return u + "/chat/completions"
	}
	return u + "/v1/chat/completions"
}

// Chat sends one streaming chat completion request and returns the assembled reply.
func (c *Client) Chat(ctx context.Context, body map[string]any, po ParseOpts, h Handlers) (*Response, error) {
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("X-Title", "lchat")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}

	acc := newAccumulator(po, h)
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		// The server ignored stream=true and sent one JSON document.
		if err := acc.whole(resp.Body); err != nil {
			return nil, err
		}
		return acc.result(), nil
	}
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, rerr := br.ReadString('\n')
		line = strings.TrimSpace(line)
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				break
			}
			if err := acc.chunk([]byte(data)); err != nil {
				return nil, err
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, rerr
		}
	}
	return acc.result(), nil
}

// flexString accepts a JSON string or any other JSON value (kept as raw text).
// Some servers send tool arguments as an object instead of a string.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	*f = flexString(b)
	return nil
}

type wireToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string     `json:"name"`
		Arguments flexString `json:"arguments"`
	} `json:"function"`
}

type wireMsg struct {
	Content          *string        `json:"content"`
	ReasoningContent *string        `json:"reasoning_content"`
	Reasoning        *string        `json:"reasoning"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

type wireChoice struct {
	Delta        wireMsg `json:"delta"`
	Message      wireMsg `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

type wireChunk struct {
	Choices []wireChoice `json:"choices"`
	Usage   *Usage       `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type toolAcc struct {
	id, name string
	args     strings.Builder
}

// accumulator assembles streamed deltas into a Response.
type accumulator struct {
	h            Handlers
	sp           *splitter
	text, think  strings.Builder
	calls        []*toolAcc
	byIndex      map[int]*toolAcc
	finish       string
	usage        *Usage
	implicit     bool
	sawContent   bool
	reasoningVia string
}

func newAccumulator(po ParseOpts, h Handlers) *accumulator {
	open, close := po.ThinkOpen, po.ThinkClose
	if open == "" || close == "" {
		open, close = "<think>", "</think>"
	}
	a := &accumulator{h: h, byIndex: map[int]*toolAcc{}, implicit: po.ImplicitOpen}
	a.sp = newSplitter([]tagPair{
		{open, close, kindThink},
		{"<tool_call>", "</tool_call>", kindHidden},
		{"<function=", "</function>", kindHidden},
	}, a.emit)
	a.sp.onStray = func() {
		// Everything before a stray closing tag was reasoning, not answer text.
		a.think.WriteString(a.text.String())
		a.text.Reset()
	}
	if po.ImplicitOpen {
		a.sp.cur = 0
	}
	return a
}

func (a *accumulator) emit(kind int, s string) {
	switch kind {
	case kindText:
		a.text.WriteString(s)
		if a.h.Text != nil {
			a.h.Text(s)
		}
	case kindThink:
		if a.reasoningVia == "" {
			a.reasoningVia = "tag"
		}
		a.think.WriteString(s)
		if a.h.Think != nil {
			a.h.Think(s)
		}
	case kindHidden:
		a.text.WriteString(s)
	}
}

func (a *accumulator) reasoning(s string) {
	if s == "" {
		return
	}
	if a.implicit && !a.sawContent {
		// The server separates reasoning itself, so content will be plain text.
		a.sp.cur = -1
		a.implicit = false
	}
	a.reasoningVia = "field"
	a.think.WriteString(s)
	if a.h.Think != nil {
		a.h.Think(s)
	}
}

func (a *accumulator) content(s string) {
	if s == "" {
		return
	}
	a.sawContent = true
	a.sp.write(s)
}

func (a *accumulator) toolDeltas(tcs []wireToolCall) {
	for _, tc := range tcs {
		var t *toolAcc
		if tc.Index != nil {
			t = a.byIndex[*tc.Index]
			if t == nil || (tc.ID != "" && t.id != "" && t.id != tc.ID) {
				t = &toolAcc{}
				a.calls = append(a.calls, t)
				a.byIndex[*tc.Index] = t
			}
		} else {
			if len(a.calls) == 0 || (tc.ID != "" && a.calls[len(a.calls)-1].id != "" && a.calls[len(a.calls)-1].id != tc.ID) {
				a.calls = append(a.calls, &toolAcc{})
			}
			t = a.calls[len(a.calls)-1]
		}
		if tc.ID != "" {
			t.id = tc.ID
		}
		if n := tc.Function.Name; n != "" {
			if t.name == "" || t.name == n {
				t.name = n
			} else {
				t.name += n
			}
		}
		t.args.WriteString(string(tc.Function.Arguments))
	}
}

func pick(a, b *string) string {
	if a != nil && *a != "" {
		return *a
	}
	if b != nil {
		return *b
	}
	return ""
}

func (a *accumulator) chunk(data []byte) error {
	var ch wireChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		return nil // ignore keep-alives and comments we don't understand
	}
	if ch.Error != nil {
		return fmt.Errorf("server: %s", ch.Error.Message)
	}
	if ch.Usage != nil {
		a.usage = ch.Usage
	}
	for _, c := range ch.Choices {
		d := c.Delta
		a.reasoning(pick(d.ReasoningContent, d.Reasoning))
		if d.Content != nil {
			a.content(*d.Content)
		}
		a.toolDeltas(d.ToolCalls)
		if c.FinishReason != nil && *c.FinishReason != "" {
			a.finish = *c.FinishReason
		}
	}
	return nil
}

func (a *accumulator) whole(r io.Reader) error {
	var ch wireChunk
	if err := json.NewDecoder(r).Decode(&ch); err != nil {
		return err
	}
	if ch.Error != nil {
		return fmt.Errorf("server: %s", ch.Error.Message)
	}
	a.usage = ch.Usage
	for _, c := range ch.Choices {
		m := c.Message
		a.reasoning(pick(m.ReasoningContent, m.Reasoning))
		if m.Content != nil {
			a.content(*m.Content)
		}
		for i := range m.ToolCalls {
			idx := i
			m.ToolCalls[i].Index = &idx
		}
		a.toolDeltas(m.ToolCalls)
		if c.FinishReason != nil {
			a.finish = *c.FinishReason
		}
	}
	return nil
}

func (a *accumulator) result() *Response {
	a.sp.flush()
	text, think := a.text.String(), a.think.String()
	if a.implicit && !a.sp.sawClose && text == "" && a.reasoningVia != "field" {
		// Implicit think block never closed: it was the answer after all.
		text, think = think, ""
		if a.h.Text != nil {
			a.h.Text(text)
		}
	}
	r := &Response{
		Content:      strings.TrimSpace(text),
		Reasoning:    strings.TrimSpace(think),
		FinishReason: a.finish,
		Usage:        a.usage,
		StrayClose:   a.sp.stray,
		ReasoningVia: a.reasoningVia,
	}
	for _, t := range a.calls {
		if t.name == "" {
			continue
		}
		r.ToolCalls = append(r.ToolCalls, ToolCall{
			ID: t.id, Type: "function",
			Function: FunctionCall{Name: t.name, Arguments: t.args.String()},
		})
	}
	return r
}
