package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// dotenv holds lchat settings read from .env files. They are kept out of the
// process environment so commands run by the agent never see the API key.
var dotenv = map[string]string{}

// getenv returns a setting from the real environment, falling back to .env.
func getenv(k string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return dotenv[k]
}

func dotenvKey(k string) bool { return strings.HasPrefix(k, "LCHAT_") || k == "OPENROUTER_API_KEY" }

// loadDotEnv reads KEY=VALUE lines from each file; earlier files win. Only
// lchat's own keys are read, so a project's other secrets are ignored.
func loadDotEnv(paths ...string) (loaded []string) {
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		loaded = append(loaded, p)
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
			k, v, ok := strings.Cut(line, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !ok || strings.HasPrefix(k, "#") || !dotenvKey(k) {
				continue
			}
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			if _, set := dotenv[k]; !set {
				dotenv[k] = v
			}
		}
	}
	return loaded
}

// dotEnvPaths: the working directory, next to the lchat binary, then the
// user config directory.
func dotEnvPaths() []string {
	paths := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			paths = append(paths, filepath.Join(filepath.Dir(exe), ".env"))
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "lchat", ".env"))
	}
	return paths
}

func envOr(k, def string) string {
	if v := getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(getenv(k)); err == nil {
		return n
	}
	return def
}

func defaultModelsPath() string {
	if p := getenv("LCHAT_MODELS"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "lchat", "models.json")
}

// configFile holds what /config saved; it is the lowest precedence source.
var configFile = &ConfigFile{}

func baseConfig() *Config {
	loadDotEnv(dotEnvPaths()...)
	configFile, _ = LoadConfigFile(configPath())
	cfg := &Config{
		BaseURL:      envOr("LCHAT_BASE_URL", firstNonEmpty(configFile.BaseURL, "http://localhost:4000")),
		APIKey:       firstNonEmpty(getenv("LCHAT_API_KEY"), configFile.APIKey),
		Model:        envOr("LCHAT_MODEL", firstNonEmpty(configFile.Model, "qwen3.5-35b-a3b")),
		Ctx:          envInt("LCHAT_CTX", 0),
		ToolMax:      envInt("LCHAT_TOOL_MAX", 8000),
		MaxSteps:     30,
		Think:        "auto",
		Mode:         envOr("LCHAT_MODE", "ask"),
		PlannerModel: envOr("LCHAT_PLANNER_MODEL", configFile.PlannerModel),
		SubSteps:     envInt("LCHAT_SUBTASK_STEPS", 8),
		LogDir:       logDir(),
		ModelsPath:   defaultModelsPath(),
	}
	// No LiteLLM configured but an OpenRouter key exists: talk to OpenRouter directly.
	if or := getenv("OPENROUTER_API_KEY"); or != "" {
		if getenv("LCHAT_BASE_URL") == "" && configFile.BaseURL == "" && cfg.APIKey == "" {
			cfg.BaseURL = "https://openrouter.ai/api/v1"
			cfg.Model = envOr("LCHAT_MODEL", "qwen/qwen3.5-35b-a3b")
		}
		if strings.Contains(cfg.BaseURL, "openrouter.ai") && cfg.APIKey == "" {
			cfg.APIKey = or
		}
	}
	if t, err := strconv.ParseFloat(getenv("LCHAT_TEMPERATURE"), 64); err == nil {
		cfg.Temperature = &t
	}
	return cfg
}

// version is stamped at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `lchat: agent coding mini untuk terminal (LiteLLM / OpenAI-compatible)

Pakai:
  lchat [flag]                 mode interaktif
  lchat [flag] -p "tugas"      sekali jalan (jawaban ke stdout, aktivitas ke stderr)
  lchat --task -p "tugas"      pecah tugas besar jadi subtask terverifikasi, lalu kerjakan satu per satu
  lchat -i shot.png -p "apa ini?"   kirim gambar
  lchat config                 atur endpoint, API key, dan model (dipandu)
  lchat probe [-m model] [--save]   deteksi cara thinking & tool call sebuah model

Env: LCHAT_BASE_URL, LCHAT_API_KEY, LCHAT_MODEL, LCHAT_CTX, LCHAT_TOOL_MAX,
     LCHAT_TEMPERATURE, LCHAT_MODELS (file profil, default ~/.config/lchat/models.json)
     LCHAT_PLANNER_MODEL (model penyusun rencana /task; kosong = model utama), LCHAT_SUBTASK_STEPS
     LCHAT_LOG=off mematikan log sesi; LCHAT_LOG_DIR memindahkannya (default ~/.local/state/lchat/sessions)
     Tanpa LCHAT_BASE_URL/LCHAT_API_KEY tapi ada OPENROUTER_API_KEY: langsung ke OpenRouter.
     Semua variabel ini juga dibaca dari ./.env, .env di samping binary lchat,
     dan ~/.config/lchat/.env.

Flag:
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		os.Exit(probeMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "config" {
		os.Exit(configMain())
	}
	cfg := baseConfig()
	fs := flag.NewFlagSet("lchat", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); fs.PrintDefaults() }
	fs.StringVar(&cfg.Model, "m", cfg.Model, "nama model (alias di LiteLLM)")
	prompt := fs.String("p", "", "jalankan satu tugas lalu keluar")
	var images []string
	fs.Func("i", "lampirkan gambar (boleh diulang)", func(v string) error { images = append(images, v); return nil })
	fs.BoolVar(&cfg.Yolo, "yolo", false, "jalankan tool tanpa minta izin")
	fs.IntVar(&cfg.MaxSteps, "max-steps", cfg.MaxSteps, "batas langkah per giliran")
	fs.StringVar(&cfg.PlannerModel, "planner-model", cfg.PlannerModel, "model penyusun rencana untuk /task (kosong = model utama)")
	fs.IntVar(&cfg.SubSteps, "subtask-steps", cfg.SubSteps, "anggaran langkah dasar per subtask di /task")
	fs.BoolVar(&cfg.Task, "task", false, "pecah tugas -p jadi subtask terverifikasi")
	noLog := fs.Bool("no-log", false, "jangan tulis log sesi (JSONL) ke ~/.local/state/lchat/sessions")
	fs.StringVar(&cfg.Think, "think", cfg.Think, "mode thinking: auto | on | off")
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "mode kerja: plan | ask | auto (Tab untuk ganti)")
	fs.BoolVar(&cfg.QuietThink, "quiet-think", false, "sembunyikan isi reasoning")
	fs.BoolVar(&cfg.CheckTS, "check-ts", false, "auto-check file .ts dengan tsc --noEmit (lambat)")
	fs.BoolVar(&cfg.Raw, "raw", false, "tampilkan markdown mentah")
	fs.BoolVar(&cfg.Verbose, "v", false, "tampilkan output tool lengkap")
	showVersion := fs.Bool("version", false, "tampilkan versi lalu keluar")
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Printf("lchat %s (%s/%s, go %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}
	if *prompt == "" && fs.NArg() > 0 {
		*prompt = strings.Join(fs.Args(), " ")
	}
	switch cfg.Think {
	case "auto", "on", "off":
	default:
		fmt.Fprintln(os.Stderr, "--think harus auto, on, atau off")
		os.Exit(2)
	}
	switch cfg.Mode {
	case "plan", "ask", "auto":
	default:
		fmt.Fprintln(os.Stderr, "--mode harus plan, ask, atau auto")
		os.Exit(2)
	}

	oneShot := *prompt != ""
	noColor := os.Getenv("NO_COLOR") != ""
	mdColor := !oneShot && !cfg.Raw && !noColor && isTTY(os.Stdout)
	logColor := !noColor && isTTY(os.Stderr)
	logW := os.Stdout
	if oneShot {
		logW = os.Stderr
	}
	ui := NewUI(os.Stdout, logW, mdColor, logColor && (oneShot || isTTY(os.Stdout)), isTTY(logW))

	user, err := LoadUserProfiles(cfg.ModelsPath)
	if err != nil {
		ui.Warn("profil diabaikan: " + err.Error())
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	in := newInput(ui, cfg)
	cwd, err := os.Getwd()
	if err != nil {
		ui.Error(err.Error())
		os.Exit(1)
	}
	agent := NewAgent(cfg, ui, in.confirm, in.line, user, cwd)
	if *noLog {
		cfg.LogDir = ""
	}
	if cfg.LogDir != "" {
		lg, err := OpenSessionLog(cfg.LogDir)
		if err != nil {
			ui.Warn("log sesi tidak bisa dibuat: " + err.Error())
		} else {
			agent.log, ui.sink = lg, lg.screen
			defer lg.Close()
		}
	}

	if cfg.APIKey == "" && !strings.Contains(cfg.BaseURL, "localhost") {
		ui.Warn("belum ada API key. Jalankan `lchat config` atau ketik /config untuk mengaturnya.")
	}
	if oneShot {
		for _, p := range images {
			img, err := loadImage(p)
			if err != nil {
				ui.Error("gambar: " + err.Error())
				os.Exit(1)
			}
			agent.Attach(img)
		}
		run := agent.RunTurn
		if cfg.Task {
			run = agent.RunTask
		}
		err := runWith(agent, *prompt, sigs, run)
		if !ui.outNL {
			fmt.Println()
		}
		if err != nil {
			if isCanceled(err) {
				os.Exit(130)
			}
			ui.Error(err.Error())
			os.Exit(1)
		}
		return
	}
	repl(agent, in, sigs)
}

// runTurn runs one turn; Ctrl+C cancels the turn, not the program.
func runTurn(a *Agent, input string, sigs chan os.Signal) error {
	return runWith(a, input, sigs, a.RunTurn)
}

// runWith runs one request under Ctrl+C handling: a turn, or a decomposed
// task, which treats the interrupt as cancelling the whole task.
func runWith(a *Agent, input string, sigs chan os.Signal, run func(context.Context, string) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-done:
		}
	}()
	err := run(ctx, input)
	close(done)
	cancel()
	switch {
	case err != nil && isCanceled(err):
		a.ui.EndResponse()
		a.ui.Warn("dibatalkan")
	case err != nil && strings.Contains(err.Error(), "connection refused"):
		a.ui.Info("tidak ada server di " + a.cfg.BaseURL + ". Jalankan LiteLLM di alamat itu, atau atur endpoint lain dengan `lchat config` / /config.")
	case err != nil && strings.Contains(err.Error(), "HTTP 401"):
		a.ui.Info("key ditolak endpoint. Perbarui dengan `lchat config` / /config.")
	}
	return err
}

// input reads from the terminal. With a real terminal it uses the raw-mode
// editor (Tab, history, single-key confirmations); otherwise plain lines.
type input struct {
	ui      *UI
	cfg     *Config
	ed      *Editor
	lines   *lineReader
	prompt  func() string
	pending []Image // images waiting to go with the next message
}

// attach queues an image and tells the user, without disturbing the line
// being typed.
func (in *input) attach(img Image) {
	in.pending = append(in.pending, img)
	fmt.Fprintf(os.Stdout, "\n%s\n", in.ui.c(sgrCyan, "  [gambar] "+img.label()+" dilampirkan ke pesan berikutnya"))
}

func (in *input) note(msg string) {
	fmt.Fprintf(os.Stdout, "\n%s\n", in.ui.c(sgrYellow, "  "+msg))
}

// takeImages hands over the queued images and clears the queue.
func (in *input) takeImages() []Image {
	imgs := in.pending
	in.pending = nil
	return imgs
}

// fromClipboard is Ctrl+V: an image is attached, text is typed into the line.
func (in *input) fromClipboard() string {
	img, text, hint := readClipboard()
	switch {
	case img != nil:
		in.attach(*img)
		return ""
	case hint != "":
		in.note(hint)
		return ""
	}
	if paths := imagePathsIn(text); len(paths) > 0 {
		in.addPaths(paths)
		return ""
	}
	return strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", " ")
}

// fromPaste handles a bracketed paste: a pasted or dragged image path is
// attached instead of typed.
func (in *input) fromPaste(text string) string {
	paths := imagePathsIn(text)
	if len(paths) == 0 {
		return text
	}
	in.addPaths(paths)
	for _, p := range paths {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "'"+p+"'", ""), p, "")
	}
	return strings.TrimSpace(text)
}

func (in *input) addPaths(paths []string) {
	for _, p := range paths {
		img, err := loadImage(p)
		if err != nil {
			in.note("gagal memuat " + p + ": " + err.Error())
			continue
		}
		in.attach(img)
	}
}

var modeOrder = []string{"plan", "ask", "auto"}

func newInput(ui *UI, cfg *Config) *input {
	in := &input{ui: ui, cfg: cfg}
	in.prompt = func() string {
		switch cfg.Mode {
		case "plan":
			return ui.c(sgrYellow, "plan › ")
		case "auto":
			return ui.c(sgrGreen, "auto › ")
		}
		return ui.c(sgrCyan, "› ")
	}
	if isTTY(os.Stdin) && isTTY(os.Stdout) {
		in.ed = NewEditor(os.Stdin, os.Stdout)
		in.ed.cmds, in.ed.color = slashCmds, ui.color
		in.ed.onTab = func() {
			i := slices.Index(modeOrder, cfg.Mode)
			cfg.Mode = modeOrder[(i+1)%len(modeOrder)]
		}
		in.ed.onClip = in.fromClipboard
		in.ed.onPaste = in.fromPaste
		return in
	}
	in.lines = newLineReader()
	return in
}

// readLine reads a line at the main prompt.
func (in *input) readLine(sigs chan os.Signal) (string, error) {
	if in.ed != nil {
		s, err := in.ed.ReadLine(in.prompt)
		in.ui.MarkNewline()
		return s, err
	}
	in.ui.Prompt(strings.TrimSuffix(in.prompt(), " "))
	return readOrInterrupt(in.lines, sigs)
}

// line asks a free-text question (used by ask_user).
func (in *input) line(ctx context.Context, p string) (string, error) {
	if in.ed != nil {
		s, err := in.ed.ReadLine(func() string { return in.ui.c(sgrYellow, p) })
		in.ui.MarkNewline()
		return s, err
	}
	in.ui.Prompt(p)
	return in.lines.read(ctx)
}

// Secret reads an API key without echoing it.
func (in *input) Secret(ctx context.Context, p string) (string, error) {
	if in.ed != nil {
		s, err := in.ed.ReadSecret(in.ui.c(sgrYellow, p))
		in.ui.MarkNewline()
		return s, err
	}
	in.ui.Prompt(p)
	return in.lines.read(ctx)
}

// Line and Confirm also satisfy Prompter.
func (in *input) Line(ctx context.Context, p string) (string, error)    { return in.line(ctx, p) }
func (in *input) Confirm(ctx context.Context, p string) (string, error) { return in.confirm(ctx, p) }

// confirm asks a yes/no style question; on a terminal one keypress is enough.
func (in *input) confirm(ctx context.Context, p string) (string, error) {
	keys := "yna"
	if strings.Contains(p, "[y=") {
		keys = "yt"
	}
	if strings.Contains(p, "[y/N]") {
		keys = "yn"
	}
	if in.ed != nil {
		s, err := in.ed.ReadChoice(in.ui.c(sgrYellow, p), keys)
		in.ui.MarkNewline()
		return s, err
	}
	in.ui.Prompt(p)
	return in.lines.read(ctx)
}

func modeLabel(mode string) string {
	switch mode {
	case "plan":
		return "plan · hanya baca, tulis diblokir, rencana diminta persetujuan"
	case "auto":
		return "auto-edit · write/edit langsung jalan, bash tetap tanya"
	}
	return "ask · setiap write/edit/bash minta izin"
}

func banner(a *Agent) {
	think := a.cfg.Think
	if !a.profile.CanSwitch() {
		think += ", model " + a.profile.Thinking.Control
	}
	a.ui.Info(fmt.Sprintf("lchat %s · %s · profil %s · thinking %s\n%s\nmode: %s · Tab ganti mode · Ctrl+V tempel gambar · /help · Ctrl+D keluar",
		version, a.model, a.profile.Describe(), think, a.cwd, modeLabel(a.cfg.Mode)))
	if a.log != nil {
		a.ui.Info("log sesi: " + a.log.Path + " · /log")
	}
}

// slashCmds drives three things at once: /help, Tab completion, and the
// suggestion list the editor shows while a command is being typed.
var slashCmds = []Completion{
	{"/clear", "", "mulai percakapan baru"},
	{"/config", "", "atur endpoint, API key, dan model (dipandu)"},
	{"/model", "[filter]", "lihat / ganti model lewat pemilih (angka memilih, teks memfilter)"},
	{"/task", "<permintaan>", "pecah tugas besar jadi subtask terverifikasi, kerjakan satu per satu"},
	{"/mode", "[nama]", "plan | ask | auto (atau tekan Tab saat mengetik)"},
	{"/think", "[mode]", "auto | on | off"},
	{"/img", "<path>", "lampirkan gambar ke pesan berikutnya"},
	{"/paste", "", "ambil gambar dari clipboard (sama dengan Ctrl+V)"},
	{"/profile", "", "tampilkan profil model aktif (JSON)"},
	{"/env", "", "tampilkan konteks environment yang dikirim ke model"},
	{"/log", "", "path log sesi ini (JSONL), untuk dianalisis nanti"},
	{"/help", "", "daftar perintah ini"},
	{"/exit", "", "keluar"},
}

func replHelp() string {
	var b strings.Builder
	b.WriteString("Perintah (Tab melengkapi nama perintah):")
	for _, c := range slashCmds {
		name := c.Name
		if c.Args != "" {
			name += " " + c.Args
		}
		fmt.Fprintf(&b, "\n  %-17s %s", name, c.Help)
	}
	b.WriteString("\nAkhiri baris dengan \\ untuk input multi-baris.")
	return b.String()
}

func repl(a *Agent, in *input, sigs chan os.Signal) {
	banner(a)
	var lastInt time.Time
	for {
		var text string
		for {
			line, err := in.readLine(sigs)
			if errors.Is(err, errInterrupt) {
				if time.Since(lastInt) < 2*time.Second {
					return
				}
				lastInt = time.Now()
				a.ui.Info("(Ctrl+C lagi untuk keluar)")
				continue
			}
			if err != nil { // EOF
				return
			}
			if strings.HasSuffix(line, `\`) {
				text += strings.TrimSuffix(line, `\`) + "\n"
				continue
			}
			text += line
			break
		}
		input := strings.TrimSpace(text)
		if paths := imagePathsIn(input); len(paths) > 0 && !strings.HasPrefix(input, "/") {
			// Pasted paths were attached at paste time; whatever is still in
			// the text was typed or recalled from history, and the model must
			// not be left with a bare path it cannot open.
			in.addPaths(paths)
			for _, p := range paths {
				input = strings.TrimSpace(strings.ReplaceAll(input, p, ""))
			}
		}
		if input == "" && len(in.pending) == 0 {
			continue
		}
		if strings.HasPrefix(input, "/") {
			if quit := command(a, in, input, sigs); quit {
				return
			}
			continue
		}
		a.Attach(in.takeImages()...)
		start := time.Now()
		if err := runTurn(a, input, sigs); err != nil && !isCanceled(err) {
			a.ui.Error(err.Error())
			continue
		}
		stats := fmt.Sprintf("%.1fs", time.Since(start).Seconds())
		if u := a.lastUsage; u != nil {
			stats += fmt.Sprintf(" · konteks %.1fk/%dk tok", float64(u.PromptTokens+u.CompletionTokens)/1000, a.ctxLimit()/1024)
		}
		a.ui.Info(stats)
	}
}

func command(a *Agent, in *input, input string, sigs chan os.Signal) (quit bool) {
	f := strings.Fields(input)
	switch f[0] {
	case "/exit", "/quit", "/q":
		return true
	case "/help", "/?":
		a.ui.Info(replHelp())
	case "/clear":
		a.Reset()
		in.pending = nil
		a.ui.Info("percakapan direset")
	case "/img":
		if len(f) < 2 {
			a.ui.Warn("pakai: /img <path gambar>")
			break
		}
		in.addPaths(f[1:])
	case "/paste":
		in.fromClipboard()
	case "/task":
		goal := strings.TrimSpace(strings.TrimPrefix(input, "/task"))
		if goal == "" {
			a.ui.Warn("pakai: /task <permintaan>")
			break
		}
		a.Attach(in.takeImages()...)
		if err := runWith(a, goal, sigs, a.RunTask); err != nil && !isCanceled(err) {
			a.ui.Error(err.Error())
		}
	case "/log":
		if a.log == nil {
			a.ui.Info("log sesi mati (LCHAT_LOG=off atau --no-log)")
		} else {
			a.ui.Info(a.log.Path)
		}
	case "/model":
		changeModel(a, in, strings.Join(f[1:], " "))
		a.ui.Info(fmt.Sprintf("model %s · profil %s", a.model, a.profile.Describe()))
	case "/profile":
		a.ui.Info(MarshalJSONText(a.profile))
	case "/mode":
		if len(f) > 1 && slices.Contains(modeOrder, f[1]) {
			a.cfg.Mode = f[1]
		}
		a.ui.Info("mode: " + modeLabel(a.cfg.Mode))
	case "/config":
		ctx, cancel := context.WithCancel(context.Background())
		if err := RunConfigWizard(ctx, a, in, configPath()); err != nil {
			if errors.Is(err, errInterrupt) {
				a.ui.Info("konfigurasi dibatalkan")
			} else {
				a.ui.Error(err.Error())
			}
		}
		cancel()
	case "/think":
		if len(f) > 1 && (f[1] == "auto" || f[1] == "on" || f[1] == "off") {
			a.cfg.Think = f[1]
		}
		a.ui.Info("thinking: " + a.cfg.Think + " (control model: " + a.profile.Thinking.Control + ")")
	case "/env":
		a.ui.Info(a.env.Block())
	default:
		a.ui.Warn("perintah tidak dikenal; /help")
	}
	return false
}

// changeModel is /model. A name is never taken as a choice on its own: it is a
// filter for the picker, so "/model qwen" lists the qwen models instead of
// silently switching to something called "qwen". An id that exists verbatim is
// the one exception, and so is an endpoint that will not list its models.
func changeModel(a *Agent, in *input, query string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.ui.Info("mengambil daftar model…")
	models, err := FetchModels(ctx, a.client)
	if err != nil {
		a.ui.Warn("gagal mengambil daftar model: " + err.Error())
		if query != "" {
			a.ui.Info("memakai nama apa adanya: " + query)
			a.SetModel(query)
		}
		return
	}
	for _, m := range models {
		if m.ID == query { // an exact id needs no picker
			a.SetModel(query)
			return
		}
	}
	id, err := chooseModel(ctx, in, a.ui, models, a.model, query)
	if err != nil || id == "" || id == a.model {
		a.ui.Info("model tidak diubah")
		return
	}
	a.SetModel(id)
}

// ---------------------------------------------------------------------------
// Line input shared by the REPL and permission prompts.

type lineReader struct{ ch chan string }

func newLineReader() *lineReader {
	l := &lineReader{ch: make(chan string)}
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			l.ch <- sc.Text()
		}
		close(l.ch)
	}()
	return l
}

func (l *lineReader) read(ctx context.Context) (string, error) {
	select {
	case s, ok := <-l.ch:
		if !ok {
			return "", errors.New("EOF")
		}
		return s, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

var errInterrupt = errors.New("interrupt")

func readOrInterrupt(l *lineReader, sigs chan os.Signal) (string, error) {
	select {
	case s, ok := <-l.ch:
		if !ok {
			return "", errors.New("EOF")
		}
		return s, nil
	case <-sigs:
		return "", errInterrupt
	}
}

// ---------------------------------------------------------------------------

// configMain runs the wizard outside the REPL: `lchat config`.
func configMain() int {
	cfg := baseConfig()
	ui := NewUI(os.Stdout, os.Stdout, false, isTTY(os.Stdout), false)
	in := newInput(ui, cfg)
	cwd, _ := os.Getwd()
	user, _ := LoadUserProfiles(cfg.ModelsPath)
	agent := NewAgent(cfg, ui, in.confirm, in.line, user, cwd)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := RunConfigWizard(ctx, agent, in, configPath()); err != nil {
		if errors.Is(err, errInterrupt) {
			ui.Info("dibatalkan")
			return 130
		}
		ui.Error(err.Error())
		return 1
	}
	return 0
}

func probeMain(args []string) int {
	cfg := baseConfig()
	fs := flag.NewFlagSet("lchat probe", flag.ExitOnError)
	fs.StringVar(&cfg.Model, "m", cfg.Model, "nama model")
	save := fs.Bool("save", false, "simpan profil hasil probe ke "+cfg.ModelsPath)
	_ = fs.Parse(args)
	user, err := LoadUserProfiles(cfg.ModelsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "profil diabaikan:", err)
	}
	base := SelectProfile(cfg.Model, cfg.BaseURL, user)
	fmt.Printf("Profil saat ini: %s\n", base.Describe())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	p := RunProbe(ctx, NewClient(cfg.BaseURL, cfg.APIKey), cfg.Model, base, os.Stdout)
	fmt.Printf("\nSaran profil:\n%s\n", MarshalJSONText(p.compact()))
	if *save {
		if err := SaveProfile(cfg.ModelsPath, p); err != nil {
			fmt.Fprintln(os.Stderr, "gagal menyimpan:", err)
			return 1
		}
		fmt.Println("Disimpan ke", cfg.ModelsPath)
	}
	return 0
}
