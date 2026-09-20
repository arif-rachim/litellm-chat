package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c := &ConfigFile{BaseURL: "https://openrouter.ai/api/v1", APIKey: "sk-or-v1-abc", Model: "qwen/qwen3.5-35b-a3b"}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config holds a key, permissions = %v", st.Mode().Perm())
	}
	got, err := LoadConfigFile(path)
	if err != nil || *got != *c {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	if empty, err := LoadConfigFile(filepath.Join(t.TempDir(), "none.json")); err != nil || empty.Model != "" {
		t.Fatal("a missing config file is not an error")
	}
	if maskKey("sk-or-v1-abcdefghijklmnop") != "sk-or-v1-a••••••mnop" || maskKey("") != "(kosong)" {
		t.Fatalf("mask: %q", maskKey("sk-or-v1-abcdefghijklmnop"))
	}
}

// modelServer serves an OpenRouter-shaped model list.
func modelServer(t *testing.T) (*httptest.Server, *[]string) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if !strings.HasSuffix(r.URL.Path, "/v1/models") {
			http.Error(w, "not found: "+r.URL.Path, 404)
			return
		}
		io.WriteString(w, `{"data": [
		  {"id": "qwen/qwen3.5-35b-a3b", "context_length": 262144, "architecture": {"input_modalities": ["text","image"]},
		   "supported_parameters": ["tools","reasoning"], "pricing": {"prompt": "0.00000009", "completion": "0.00000045"}},
		  {"id": "qwen/qwen3-coder-next", "context_length": 262144, "architecture": {"input_modalities": ["text"]},
		   "supported_parameters": ["tools"], "pricing": {"prompt": "0.0000003", "completion": "0.0000012"}},
		  {"id": "some/text-only", "context_length": 8192, "architecture": {"input_modalities": ["text"]},
		   "supported_parameters": ["temperature"], "pricing": {"prompt": "0", "completion": "0"}}
		]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

func TestFetchAndFilterModels(t *testing.T) {
	srv, auth := modelServer(t)
	models, err := FetchModels(context.Background(), NewClient(srv.URL, "sk-test"))
	if err != nil || len(models) != 3 {
		t.Fatalf("fetch: %d %v", len(models), err)
	}
	if (*auth)[0] != "Bearer sk-test" {
		t.Fatalf("key not sent: %q", (*auth)[0])
	}
	m := models[0] // sorted by id
	if m.ID != "qwen/qwen3-coder-next" {
		t.Fatalf("sort: %s", m.ID)
	}
	q := filterModels(models, "qwen3.5")
	if len(q) != 1 || q[0].ID != "qwen/qwen3.5-35b-a3b" {
		t.Fatalf("filter: %v", q)
	}
	if b := q[0].badges(); !strings.Contains(b, "256k") || !strings.Contains(b, "tools") || !strings.Contains(b, "gambar") || !strings.Contains(b, "$0.09/$0.45") {
		t.Fatalf("badges: %q", b)
	}
	// Models that cannot call tools are listed last.
	all := filterModels(models, "")
	if all[len(all)-1].ID != "some/text-only" {
		t.Fatalf("tool-less model should sink: %v", all)
	}
	if _, err := FetchModels(context.Background(), NewClient("http://127.0.0.1:1", "")); err == nil {
		t.Fatal("unreachable endpoint should error")
	}
}

// scriptPrompter answers the wizard from a fixed list.
type scriptPrompter struct {
	answers []string
	asked   []string
}

func (s *scriptPrompter) next(p string) (string, error) {
	s.asked = append(s.asked, p)
	if len(s.answers) == 0 {
		return "", errors.New("no more answers")
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, nil
}
func (s *scriptPrompter) Line(_ context.Context, p string) (string, error)    { return s.next(p) }
func (s *scriptPrompter) Secret(_ context.Context, p string) (string, error)  { return s.next(p) }
func (s *scriptPrompter) Confirm(_ context.Context, p string) (string, error) { return s.next(p) }

func TestConfigWizard(t *testing.T) {
	srv, _ := modelServer(t)
	a, _, _, _ := newFakeAgent(t)
	path := filepath.Join(t.TempDir(), "config.json")

	// Endpoint "2" (LiteLLM-style), key, filter, pick #1, skip probe.
	p := &scriptPrompter{answers: []string{"2", srv.URL, "sk-master", "qwen3.5", "1", "n"}}
	if err := RunConfigWizard(context.Background(), a, p, path); err != nil {
		t.Fatal(err)
	}
	saved, _ := LoadConfigFile(path)
	if saved.BaseURL != srv.URL || saved.APIKey != "sk-master" || saved.Model != "qwen/qwen3.5-35b-a3b" {
		t.Fatalf("saved: %+v", saved)
	}
	if a.model != saved.Model || a.cfg.APIKey != "sk-master" || a.client.BaseURL != srv.URL {
		t.Fatalf("running session not reconfigured: %s %s", a.model, a.client.BaseURL)
	}
	if len(a.msgs) != 1 {
		t.Fatal("conversation should be reset after reconfiguring")
	}
	// The key prompt must show the old key masked, never in full.
	for _, q := range p.asked {
		if strings.Contains(q, "sk-master") {
			t.Fatalf("key echoed in a prompt: %q", q)
		}
	}

	// Enter keeps the saved values; "=id" forces an id that is not in the list.
	p = &scriptPrompter{answers: []string{"", "", "", "=tidak-ada-di-daftar", "n"}}
	if err := RunConfigWizard(context.Background(), a, p, path); err != nil {
		t.Fatal(err)
	}
	saved, _ = LoadConfigFile(path)
	if saved.APIKey != "sk-master" || saved.Model != "tidak-ada-di-daftar" {
		t.Fatalf("keep/typed: %+v", saved)
	}

	// Ctrl+C in the middle leaves the file untouched.
	before, _ := os.ReadFile(path)
	p = &scriptPrompter{answers: []string{"1"}}
	if err := RunConfigWizard(context.Background(), a, p, path); err == nil {
		t.Fatal("aborting should return an error")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("config changed despite aborting")
	}
}

func TestConfigWizardUnreachableEndpoint(t *testing.T) {
	a, _, _, _ := newFakeAgent(t)
	path := filepath.Join(t.TempDir(), "config.json")
	p := &scriptPrompter{answers: []string{"2", "http://127.0.0.1:1", "", "model-manual", "n"}}
	if err := RunConfigWizard(context.Background(), a, p, path); err != nil {
		t.Fatal(err)
	}
	saved, _ := LoadConfigFile(path)
	if saved.Model != "model-manual" {
		t.Fatalf("manual fallback: %+v", saved)
	}
}

func TestReadSecretMasks(t *testing.T) {
	var out bytes.Buffer
	e := &Editor{in: bufio.NewReader(strings.NewReader("sk-abc\x7f\r")), out: &out}
	got, err := e.ReadSecret("key › ")
	if got != "sk-ab" || err != nil {
		t.Fatalf("secret: %q %v", got, err)
	}
	if strings.Contains(out.String(), "sk-ab") {
		t.Fatalf("secret echoed to the screen: %q", out.String())
	}
	if !strings.Contains(out.String(), "•••••") {
		t.Fatalf("no masking dots: %q", out.String())
	}
}

func TestConfigFilePrecedence(t *testing.T) {
	// Env beats config file; config file beats the built-in default.
	dir := t.TempDir()
	cf := &ConfigFile{BaseURL: "http://from-config:4000", APIKey: "key-config", Model: "model-config"}
	cf.Save(filepath.Join(dir, "config.json"))
	t.Setenv("LCHAT_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("LCHAT_MODELS", filepath.Join(dir, "models.json"))
	t.Setenv("LCHAT_MODEL", "model-env")
	dotenv = map[string]string{}
	cfg := baseConfig()
	if cfg.Model != "model-env" || cfg.BaseURL != "http://from-config:4000" || cfg.APIKey != "key-config" {
		b, _ := json.Marshal(cfg)
		t.Fatalf("precedence: %s", b)
	}
}

// testUI is a UI that writes everything to one buffer, without colors.
func testUI() (*UI, *bytes.Buffer) {
	var buf bytes.Buffer
	return NewUI(&buf, &buf, false, false, false), &buf
}

func TestChooseModelFiltersOnText(t *testing.T) {
	srv, _ := modelServer(t)
	models, err := FetchModels(context.Background(), NewClient(srv.URL, ""))
	if err != nil {
		t.Fatal(err)
	}
	ui, out := testUI()

	// Text narrows the list and picks nothing by itself; the number decides.
	p := &scriptPrompter{answers: []string{"qwen", "2"}}
	got, err := chooseModel(context.Background(), p, ui, models, "", "")
	if err != nil || got != "qwen/qwen3.5-35b-a3b" {
		t.Fatalf("filter then pick: %q %v", got, err)
	}
	if !strings.Contains(out.String(), "2 model cocok dengan \"qwen\"") {
		t.Fatalf("filter not reported: %q", out.String())
	}

	// A name that looks like a model is still only a filter.
	p = &scriptPrompter{answers: []string{"some/text-only", "1"}}
	if got, err := chooseModel(context.Background(), p, ui, models, "", ""); err != nil || got != "some/text-only" {
		t.Fatalf("exact name typed as filter: %q %v", got, err)
	}

	// An out-of-range number asks again instead of picking something.
	p = &scriptPrompter{answers: []string{"99", "1"}}
	if got, _ := chooseModel(context.Background(), p, ui, models, "", ""); got != "qwen/qwen3-coder-next" {
		t.Fatalf("out of range: %q", got)
	}

	// Empty means "leave the model alone"; "=id" forces an unlisted id.
	p = &scriptPrompter{answers: []string{""}}
	if got, err := chooseModel(context.Background(), p, ui, models, "cur", ""); got != "" || err != nil {
		t.Fatalf("cancel: %q %v", got, err)
	}
	p = &scriptPrompter{answers: []string{"=lokal/model-baru"}}
	if got, _ := chooseModel(context.Background(), p, ui, models, "", ""); got != "lokal/model-baru" {
		t.Fatalf("literal id: %q", got)
	}

	// A filter that matches nothing keeps the list as it was.
	p = &scriptPrompter{answers: []string{"zzz", "1"}}
	out.Reset()
	if got, _ := chooseModel(context.Background(), p, ui, models, "", ""); got != "qwen/qwen3-coder-next" {
		t.Fatalf("no match should not change the list: %q", got)
	}
	if !strings.Contains(out.String(), "tidak ada yang cocok") {
		t.Fatalf("missing warning: %q", out.String())
	}
}

func TestChooseModelPaging(t *testing.T) {
	var models []ModelInfo
	for i := range 25 {
		models = append(models, ModelInfo{ID: fmt.Sprintf("vendor/m%02d", i+1)})
	}
	ui, out := testUI()

	// A number belonging to another page is refused, not silently applied.
	p := &scriptPrompter{answers: []string{"25", "n", "25"}}
	got, err := chooseModel(context.Background(), p, ui, models, "", "")
	if err != nil || got != "vendor/m25" {
		t.Fatalf("paging: %q %v", got, err)
	}
	if !strings.Contains(out.String(), "nomor di luar halaman ini (1-20)") {
		t.Fatalf("off-page number should be refused: %q", out.String())
	}
	if !strings.Contains(out.String(), "halaman 2/2") {
		t.Fatalf("second page not shown: %q", out.String())
	}
}
