package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// The config file written by /config. It sits below environment variables and
// .env in precedence, so a shell or project setting still wins.
type ConfigFile struct {
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`

	PlannerModel string `json:"planner_model,omitempty"` // /task: kosong = model utama
}

func configPath() string {
	if p := getenv("LCHAT_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "lchat", "config.json")
}

func LoadConfigFile(path string) (*ConfigFile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ConfigFile{}, nil
	}
	if err != nil {
		return &ConfigFile{}, err
	}
	var c ConfigFile
	if err := json.Unmarshal(b, &c); err != nil {
		return &ConfigFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config with owner-only permissions: it holds an API key.
func (c *ConfigFile) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func maskKey(k string) string {
	switch {
	case k == "":
		return "(kosong)"
	case len(k) <= 12:
		return strings.Repeat("•", len(k))
	}
	return k[:10] + strings.Repeat("•", 6) + k[len(k)-4:]
}

// ---------------------------------------------------------------------------
// Model list, fetched with the key the user just gave.

type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Architecture  struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
	Pricing             struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

func (m ModelInfo) tools() bool {
	// LiteLLM and plain OpenAI servers do not report this; treat as unknown.
	return len(m.SupportedParameters) == 0 || slices.Contains(m.SupportedParameters, "tools")
}

func (m ModelInfo) vision() bool {
	return slices.Contains(m.Architecture.InputModalities, "image")
}

// badges describes a model in one short line for the picker.
func (m ModelInfo) badges() string {
	var b []string
	if m.ContextLength > 0 {
		b = append(b, fmt.Sprintf("%dk", m.ContextLength/1024))
	}
	if len(m.SupportedParameters) > 0 {
		if m.tools() {
			b = append(b, "tools")
		} else {
			b = append(b, "tanpa-tools")
		}
	}
	if m.vision() {
		b = append(b, "gambar")
	}
	if p := priceLabel(m.Pricing.Prompt, m.Pricing.Completion); p != "" {
		b = append(b, p)
	}
	return strings.Join(b, " · ")
}

func priceLabel(in, out string) string {
	f := func(s string) (float64, bool) {
		var v float64
		if s == "" {
			return 0, false
		}
		if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
			return 0, false
		}
		return v * 1e6, true // price per token -> per 1M tokens
	}
	pi, ok1 := f(in)
	po, ok2 := f(out)
	if !ok1 || !ok2 || (pi == 0 && po == 0) {
		return ""
	}
	return fmt.Sprintf("$%.2f/$%.2f per 1M", pi, po)
}

// FetchModels asks the endpoint which models the key can use.
func FetchModels(ctx context.Context, c *Client) ([]ModelInfo, error) {
	u := strings.TrimRight(c.BaseURL, "/")
	if !strings.HasSuffix(u, "/v1") {
		u += "/v1"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	cl := c.HTTP
	if cl == nil {
		cl = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := cl.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d saat mengambil daftar model", resp.StatusCode)
	}
	var out struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].ID < out.Data[j].ID })
	return out.Data, nil
}

// filterModels keeps the models matching every word of the query, models that
// can call tools first (an agent needs them).
func filterModels(all []ModelInfo, query string) []ModelInfo {
	words := strings.Fields(strings.ToLower(query))
	var out []ModelInfo
	for _, m := range all {
		hay := strings.ToLower(m.ID + " " + m.Name)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].tools() && !out[j].tools() })
	return out
}
