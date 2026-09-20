package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Prompter is what the wizard needs from the terminal.
type Prompter interface {
	Line(ctx context.Context, prompt string) (string, error)   // free text
	Secret(ctx context.Context, prompt string) (string, error) // hidden text (API keys)
	Confirm(ctx context.Context, prompt string) (string, error)
}

// RunConfigWizard walks the user through provider, key, model, and saves the
// result. It applies the new settings to the running session too.
func RunConfigWizard(ctx context.Context, a *Agent, p Prompter, path string) error {
	ui := a.ui
	saved, _ := LoadConfigFile(path)

	ui.Info("konfigurasi lchat · Enter = pakai nilai sekarang · Ctrl+C batal")
	ui.Options([]string{
		"OpenRouter (langsung, butuh API key OpenRouter)",
		"LiteLLM atau endpoint OpenAI-compatible lain (alamat + key)",
	})
	choice, err := p.Line(ctx, fmt.Sprintf("  pilih [%s] › ", defaultChoice(a.cfg.BaseURL)))
	if err != nil {
		return err
	}
	base, key := a.cfg.BaseURL, a.cfg.APIKey
	switch strings.TrimSpace(choice) {
	case "1":
		base = "https://openrouter.ai/api/v1"
	case "2":
		base = ""
	case "":
		if strings.Contains(a.cfg.BaseURL, "openrouter.ai") {
			base = "https://openrouter.ai/api/v1"
		} else {
			base = ""
		}
	default:
		return fmt.Errorf("pilihan tidak dikenal: %s", choice)
	}

	if base == "" { // LiteLLM / other endpoint
		def := a.cfg.BaseURL
		if def == "" || strings.Contains(def, "openrouter.ai") {
			def = "http://localhost:4000"
		}
		in, err := p.Line(ctx, fmt.Sprintf("  alamat LiteLLM [%s] › ", def))
		if err != nil {
			return err
		}
		base = strings.TrimSpace(in)
		if base == "" {
			base = def
		}
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			base = "http://" + base
		}
	}

	label := "API key OpenRouter"
	if !strings.Contains(base, "openrouter.ai") {
		label = "API key / master key LiteLLM (boleh kosong)"
	}
	hint := "kosong"
	if key != "" {
		hint = maskKey(key)
	}
	in, err := p.Secret(ctx, fmt.Sprintf("  %s [%s] › ", label, hint))
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(in); s != "" {
		key = s
	}

	// Ask the endpoint which models this key can use.
	client := NewClient(base, key)
	ui.Info("  mengambil daftar model…")
	models, err := FetchModels(ctx, client)
	if err != nil {
		ui.Warn("gagal mengambil daftar model: " + err.Error())
		id, ierr := p.Line(ctx, fmt.Sprintf("  ketik nama model manual [%s] › ", a.cfg.Model))
		if ierr != nil {
			return ierr
		}
		return finishConfig(ctx, a, p, path, saved, base, key, firstNonEmpty(strings.TrimSpace(id), a.cfg.Model))
	}
	ui.Info(fmt.Sprintf("  %d model tersedia untuk key ini", len(models)))

	model, err := chooseModel(ctx, p, ui, models, a.cfg.Model, "")
	if err != nil {
		return err
	}
	if model == "" { // nothing picked: keep what the session already uses
		model = a.cfg.Model
	}
	return finishConfig(ctx, a, p, path, saved, base, key, model)
}

func defaultChoice(base string) string {
	if base == "" || strings.Contains(base, "openrouter.ai") {
		return "1"
	}
	return "2"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func finishConfig(ctx context.Context, a *Agent, p Prompter, path string, saved *ConfigFile, base, key, model string) error {
	if model == "" {
		return fmt.Errorf("model belum dipilih")
	}
	cf := &ConfigFile{BaseURL: base, APIKey: key, Model: model}
	if err := cf.Save(path); err != nil {
		return err
	}
	a.Reconfigure(base, key, model)
	a.ui.Info(fmt.Sprintf("tersimpan di %s\n  endpoint: %s\n  key: %s\n  model: %s · profil %s",
		path, base, maskKey(key), model, a.profile.Describe()))

	// Warn when a shell or .env value will still win at the next start.
	for _, v := range []struct{ name, saved string }{
		{"LCHAT_BASE_URL", base}, {"LCHAT_API_KEY", key}, {"LCHAT_MODEL", model},
	} {
		if cur := getenv(v.name); cur != "" && cur != v.saved {
			a.ui.Warn(fmt.Sprintf("%s di environment/.env (%s) tetap menang atas config ini saat lchat dijalankan lagi", v.name, clip(cur, 30)))
		}
	}
	if getenv("OPENROUTER_API_KEY") != "" && !strings.Contains(base, "openrouter.ai") {
		a.ui.Warn("OPENROUTER_API_KEY masih ada di environment/.env, tapi endpoint yang dipilih bukan OpenRouter")
	}

	ans, err := p.Confirm(ctx, "  jalankan probe untuk mengenali model ini sekarang? [y/N] ")
	if err == nil && ans == "y" {
		prof := RunProbe(ctx, a.client, model, a.profile, a.ui.log)
		if err := SaveProfile(a.cfg.ModelsPath, prof); err != nil {
			a.ui.Warn("gagal menyimpan profil: " + err.Error())
		} else {
			a.user, _ = LoadUserProfiles(a.cfg.ModelsPath)
			a.SetModel(model)
			a.ui.Info("profil disimpan di " + a.cfg.ModelsPath + " · " + a.profile.Describe())
		}
	}
	a.ui.Info("percakapan direset agar memakai konfigurasi baru")
	a.Reset()
	return nil
}

// modelPage is how many models the picker shows at once.
const modelPage = 20

// chooseModel is the model picker, shared by the wizard and /model. The rule it
// follows: a number picks, anything else filters. Typing "qwen" therefore never
// selects a model by itself -- it narrows the list, and only the number the user
// then types decides. "=name" is the escape hatch for an id that is not in the
// list, and an empty line leaves the model alone.
func chooseModel(ctx context.Context, p Prompter, ui *UI, models []ModelInfo, current, query string) (string, error) {
	list := filterModels(models, query)
	if len(list) == 0 {
		ui.Warn(fmt.Sprintf("tidak ada model yang cocok dengan %q, menampilkan semua", query))
		query, list = "", filterModels(models, "")
	}
	page := 0
	for {
		pages := (len(list) + modelPage - 1) / modelPage
		if page >= pages {
			page = 0
		}
		start, end := page*modelPage, min((page+1)*modelPage, len(list))
		head := fmt.Sprintf("  %d model", len(list))
		if query != "" {
			head += fmt.Sprintf(" cocok dengan %q", query)
		}
		if pages > 1 {
			head += fmt.Sprintf(" · halaman %d/%d", page+1, pages)
		}
		ui.Info(head)
		for i := start; i < end; i++ {
			m := list[i]
			mark := " "
			if m.ID == current {
				mark = ui.c(sgrGreen, "•")
			}
			fmt.Fprintf(ui.log, " %s%s %-38s %s\n", mark, ui.c(sgrCyan, fmt.Sprintf("%3d)", i+1)), m.ID, ui.c(sgrDim, m.badges()))
		}
		hint := "  angka = pilih"
		if pages > 1 {
			hint += " · n/p = halaman berikut/sebelumnya"
		}
		hint += " · teks = filter · =id = pakai id apa adanya · kosong = batal"
		ui.Info(hint)
		in, err := p.Line(ctx, "  › ")
		if err != nil {
			return "", err
		}
		in = strings.TrimSpace(in)
		switch {
		case in == "":
			return "", nil
		case in == "n" && pages > 1:
			page = (page + 1) % pages
		case in == "p" && pages > 1:
			page = (page - 1 + pages) % pages
		case strings.HasPrefix(in, "="):
			if id := strings.TrimSpace(in[1:]); id != "" {
				return id, nil
			}
		default:
			n, err := strconv.Atoi(in)
			if err != nil { // characters: filter the list, never pick from it
				next := filterModels(models, in)
				if len(next) == 0 {
					ui.Warn(fmt.Sprintf("tidak ada yang cocok dengan %q; daftar dibiarkan", in))
					continue
				}
				query, list, page = in, next, 0
				continue
			}
			if n < start+1 || n > end { // only what is on screen can be picked
				ui.Warn(fmt.Sprintf("nomor di luar halaman ini (%d-%d)", start+1, end))
				continue
			}
			m := list[n-1]
			if !m.tools() {
				ui.Warn("model ini tidak mendukung tool calling, jadi agent tidak bisa membaca atau mengubah file")
			}
			return m.ID, nil
		}
	}
}
