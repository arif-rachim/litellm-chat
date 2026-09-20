package main

import (
	"context"
	"strings"
	"testing"
)

// --- Tahap 4: /task + planner

func TestValidatePlanRejectsUncheckableSubtask(t *testing.T) {
	env := &Env{VerifyCmds: []string{"npm test"}}
	bad := []struct {
		sts  []Subtask
		want string
	}{
		{nil, "no subtasks"},
		{make([]Subtask, 9), "at most 8"},
		{[]Subtask{{Title: "x"}}, "has no verify"},
		{[]Subtask{{Title: "x", Verify: "cat README.md"}}, "only prints"},
		{[]Subtask{{Title: "x", Verify: "npm test || true"}}, "cannot fail"},
		{[]Subtask{{Title: "x", Verify: "node index.js"}}, "not a check"},
		{[]Subtask{{Title: strings.Repeat("panjang ", 20), Verify: "npm test"}}, "longer than 100"},
	}
	for _, c := range bad {
		ps := validatePlan(c.sts, env)
		if len(ps) == 0 || !strings.Contains(strings.Join(ps, " | "), c.want) {
			t.Errorf("rencana %+v harus ditolak dengan %q, dapat %v", c.sts, c.want, ps)
		}
	}
	good := []Subtask{
		{Title: "a", Verify: "npm test -- math"},
		{Title: "b", Verify: "test -f docs/api.md"},
		{Title: "c", Verify: "go test ./pkg -run TestX"},
	}
	if ps := validatePlan(good, env); len(ps) != 0 {
		t.Fatalf("rencana yang baik ditolak: %v", ps)
	}
}

func TestTaskSystemPromptSwapsTodoRule(t *testing.T) {
	env := DetectEnv(t.TempDir())
	task, normal := taskSystemPrompt(env), systemPrompt(env)
	if strings.Contains(task, "call todo") || !strings.Contains(task, "subtask_done") {
		t.Fatal("prompt subtask harus tanpa aturan todo dan menyebut subtask_done")
	}
	if !strings.Contains(normal, "call todo") || strings.Contains(normal, "subtask_done") {
		t.Fatal("prompt biasa harus tetap memuat aturan todo")
	}
	if task == normal {
		t.Fatal("strings.Replace tidak mengubah apa pun: planRule tidak lagi cocok dengan basePrompt")
	}
}

func schemaNames(schemas []any) string {
	var names []string
	for _, s := range schemas {
		f, _ := s.(map[string]any)["function"].(map[string]any)
		names = append(names, f["name"].(string))
	}
	return strings.Join(names, ",")
}

func TestTaskToolScopes(t *testing.T) {
	solo, task := schemaNames(toolSchemas(false)), schemaNames(toolSchemas(true))
	if !strings.Contains(solo, "todo") || strings.Contains(solo, "subtask_done") {
		t.Fatalf("giliran biasa: %s", solo)
	}
	if strings.Contains(task, "todo") || !strings.Contains(task, "subtask_done") {
		t.Fatalf("subtask: %s", task)
	}
	// Calling subtask_done outside /task gets a scope correction, not "no such tool".
	a, _, _, _ := newFakeAgent(t, toolReply("subtask_done", `{"summary": "x"}`), textReply("ok"))
	a.RunTurn(context.Background(), "go")
	tm := toolMsgs(a)
	if len(tm) != 1 || !strings.Contains(tm[0].Content, "only exists inside a /task run") || strings.Contains(tm[0].Content, "There is no tool named") {
		t.Fatalf("koreksi scope: %q", tm[0].Content)
	}
}

const twoStepPlan = `{"mode": "plan", "subtasks": [
  {"title": "Write config.yaml", "detail": "create it with a: 1", "files": ["config.yaml"], "verify": "test -f config.yaml"},
  {"title": "Write marker.txt", "files": ["marker.txt"], "verify": "test -f marker.txt"}]}`

func msgsOf(req map[string]any) []map[string]any {
	raw, _ := req["messages"].([]any)
	var out []map[string]any
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		out = append(out, mm)
	}
	return out
}

func content(m map[string]any) string { s, _ := m["content"].(string); return s }

// TestSubtaskRunEndToEnd covers: planner without tools, subtask_done refused
// until the harness saw the verifier pass, a fresh context for subtask 2 that
// carries only the handoff, and one clean synthetic turn at the end.
func TestSubtaskRunEndToEnd(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply(twoStepPlan), // 0 planner
		// subtask 1
		toolReply("subtask_done", `{"summary": "done"}`),                      // 1 ditolak: belum diverifikasi
		toolReply("write_file", `{"path": "config.yaml", "content": "a: 1"}`), // 2
		toolReply("bash", `{"command": "echo hi"}`),                           // 3 lulus, tapi bukan verifier
		toolReply("subtask_done", `{"summary": "done"}`),                      // 4 tetap ditolak
		toolReply("bash", `{"command": "test -f config.yaml"}`),               // 5 verifier lulus
		toolReply("subtask_done", `{"summary": "config written"}`),            // 6 diterima
		// subtask 2
		toolReply("write_file", `{"path": "marker.txt", "content": "m"}`), // 7
		toolReply("bash", `{"command": "test -f marker.txt"}`),            // 8
		toolReply("subtask_done", `{"summary": "marker written"}`),        // 9
		textReply("Dua subtask selesai dan terverifikasi."),               // 10 laporan
	)
	a.cfg.SubSteps = 8
	if err := a.RunTask(context.Background(), "buat config dan marker"); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 11 {
		t.Fatalf("permintaan = %d, mau 11", len(f.reqs))
	}
	if _, ok := f.reqs[0]["tools"]; ok {
		t.Fatal("planner tidak boleh mendapat tools")
	}
	if f.reqs[0]["model"] != a.model || f.reqs[1]["model"] != a.model {
		t.Fatal("tanpa LCHAT_PLANNER_MODEL semua permintaan memakai model utama")
	}
	tm := toolMsgs(a) // hanya subtask terakhir yang tersisa di a.msgs? tidak: a.msgs sudah ditulis ulang
	if len(tm) != 0 {
		t.Fatalf("setelah /task, a.msgs harus satu giliran sintetis bersih, tapi ada %d pesan tool", len(tm))
	}
	// Penolakan subtask_done terlihat di permintaan berikutnya (pesan tool).
	m2 := msgsOf(f.reqs[2])
	if last := content(m2[len(m2)-1]); !strings.Contains(last, "has not passed") {
		t.Fatalf("subtask_done sebelum verifikasi harus ditolak: %q", last)
	}
	// Penolakan kedua juga menaiki tangga (diagnosis), jadi koreksi tangga
	// menyusul di belakang pesan tool: lihat dua pesan terakhir.
	m5 := msgsOf(f.reqs[5])
	tail := content(m5[len(m5)-2]) + content(m5[len(m5)-1])
	if !strings.Contains(tail, "has not passed") {
		t.Fatalf("perintah lain yang lulus tidak boleh membuka kunci: %q", tail)
	}
	if !strings.Contains(tail, "proven wrong") {
		t.Fatalf("penolakan berulang harus menaiki tangga: %q", tail)
	}
	// Begitu verifier lulus, output tool-nya sendiri menyuruh menutup.
	m6 := msgsOf(f.reqs[6])
	if last := content(m6[len(m6)-1]); !strings.Contains(last, "Call subtask_done now") {
		t.Fatalf("verifier yang lulus harus langsung menyuruh menutup: %q", last)
	}
	// Subtask 2 mulai dari konteks bersih: hanya sistem + subtask, dengan handoff.
	m7 := msgsOf(f.reqs[7])
	if len(m7) != 2 {
		t.Fatalf("subtask 2 harus mulai dengan 2 pesan, dapat %d", len(m7))
	}
	sys, user := content(m7[0]), content(m7[1])
	for _, want := range []string{"<task>", "buat config dan marker", "Write config.yaml", "passed: test -f config.yaml", "changed: config.yaml", "note: config written", "[subtask 2 of 2]"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("pesan sistem subtask 2 kehilangan %q:\n%s", want, sys)
		}
	}
	if !strings.Contains(user, "<subtask 2 of 2>") || !strings.Contains(user, "test -f marker.txt") || strings.Contains(user, "echo hi") {
		t.Fatalf("pesan subtask 2: %q", user)
	}
	// Akhir: satu giliran sintetis bersih, rencana dibuang.
	if len(a.msgs) != 3 || a.msgs[1].Content != "buat config dan marker" || a.msgs[2].Role != "assistant" || !strings.Contains(a.msgs[2].Content, "Dua subtask") {
		t.Fatalf("a.msgs setelah task: %d pesan", len(a.msgs))
	}
	if a.plan != nil || a.sub != nil {
		t.Fatal("plan/sub harus nil setelah task")
	}
	if !strings.Contains(screen.String(), "verifikasi ulang terakhir lulus") {
		t.Fatalf("verifikasi ulang terakhir tidak dilaporkan:\n%s", screen)
	}
}

func TestPlannerUsesItsOwnModel(t *testing.T) {
	a, f, _, _ := newFakeAgent(t,
		textReply(`{"mode": "plan", "subtasks": [{"title": "Write marker.txt", "verify": "test -f marker.txt"}]}`),
		toolReply("write_file", `{"path": "marker.txt", "content": "m"}`),
		toolReply("bash", `{"command": "test -f marker.txt"}`),
		toolReply("subtask_done", `{"summary": "ok"}`),
		textReply("Selesai."),
	)
	a.cfg.SubSteps, a.cfg.PlannerModel = 8, "big-planner"
	if err := a.RunTask(context.Background(), "buat marker"); err != nil {
		t.Fatal(err)
	}
	if f.reqs[0]["model"] != "big-planner" || f.reqs[1]["model"] != a.model {
		t.Fatalf("planner=%v eksekutor=%v", f.reqs[0]["model"], f.reqs[1]["model"])
	}
}

func TestDirectVerdictSkipsDecomposition(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply(`{"mode": "direct", "why": "one edit in one file"}`),
		textReply("Sudah."),
	)
	a.cfg.SubSteps = 8
	if err := a.RunTask(context.Background(), "ganti nama variabel x"); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("direct harus satu panggilan planner lalu giliran biasa: %d", len(f.reqs))
	}
	if sys := content(msgsOf(f.reqs[1])[0]); strings.Contains(sys, "<subtask") || strings.Contains(sys, "<task>") {
		t.Fatal("giliran biasa tidak boleh membawa blok subtask")
	}
	if !strings.Contains(screen.String(), "dikerjakan langsung") {
		t.Fatalf("user tidak diberi tahu:\n%s", screen)
	}
}

func TestPlannerRetriesOnceThenFallsBack(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply("Tentu, saya akan membantu memecah tugas ini."),                   // prosa, bukan JSON
		textReply(`{"mode": "plan", "subtasks": [{"title": "x", "verify": "ls"}]}`), // verifier tak berguna
		textReply("Dikerjakan langsung."),
	)
	a.cfg.SubSteps = 8
	if err := a.RunTask(context.Background(), "tugas"); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 3 {
		t.Fatalf("mau 2 panggilan planner + 1 giliran biasa, dapat %d", len(f.reqs))
	}
	m1 := msgsOf(f.reqs[1])
	if last := content(m1[len(m1)-1]); !strings.Contains(last, "[harness] Problem:") || !strings.Contains(last, "not a JSON object") {
		t.Fatalf("percobaan ulang harus membawa koreksi: %q", last)
	}
	if !strings.Contains(screen.String(), "dikerjakan seperti biasa") {
		t.Fatalf("planner buruk harus jatuh ke giliran biasa, bukan mematikan tool:\n%s", screen)
	}
}

func TestVerifierMustBeRedFirst(t *testing.T) {
	// a.txt sudah ada di folder kerja palsu: verifier-nya hijau sebelum mulai.
	a, f, _, screen := newFakeAgent(t,
		textReply(`{"mode": "plan", "subtasks": [{"title": "Create a.txt", "verify": "test -f a.txt"}]}`),
		textReply("Tidak ada yang perlu dikerjakan."),
	)
	a.cfg.SubSteps = 8
	if err := a.RunTask(context.Background(), "buat a.txt"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen.String(), "sudah hijau") {
		t.Fatalf("verifier yang sudah lulus harus melewati subtask:\n%s", screen)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("tanpa eksekutor: planner + laporan saja, dapat %d", len(f.reqs))
	}
}

func TestSubtaskBudgetTriggersSplit(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply(`{"mode": "plan", "subtasks": [{"title": "Big step", "verify": "test -f config.yaml"}]}`), // 0
		toolReply("bash", `{"command": "echo a"}`),                                                          // 1
		toolReply("bash", `{"command": "echo b"}`),                                                          // 2 -> anggaran habis
		textReply(`{"mode": "plan", "subtasks": [
		  {"title": "Part A", "files": ["part.txt"], "verify": "test -f part.txt"},
		  {"title": "Part B", "files": ["config.yaml"], "verify": "test -f config.yaml"}]}`), // 3 pemecahan
		toolReply("write_file", `{"path": "part.txt", "content": "p"}`),    // 4
		toolReply("bash", `{"command": "test -f part.txt"}`),               // 5
		toolReply("subtask_done", `{"summary": "part written"}`),           // 6
		toolReply("write_file", `{"path": "config.yaml", "content": "a"}`), // 7
		toolReply("bash", `{"command": "test -f config.yaml"}`),            // 8
		toolReply("subtask_done", `{"summary": "config written"}`),         // 9
		textReply("Selesai lewat dua bagian."),                             // 10
	)
	a.cfg.SubSteps = 2
	if err := a.RunTask(context.Background(), "buat config"); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 11 {
		t.Fatalf("permintaan = %d, mau 11", len(f.reqs))
	}
	split := content(msgsOf(f.reqs[3])[1])
	if !strings.Contains(split, "did not fit") || !strings.Contains(split, "bash echo a") {
		t.Fatalf("permintaan pemecahan harus membawa ledger sebagai bukti:\n%s", split)
	}
	if !strings.Contains(screen.String(), "dipecah jadi 2") {
		t.Fatalf("kehabisan langkah adalah sinyal untuk memecah, bukan error:\n%s", screen)
	}
	if len(a.msgs) != 3 || !strings.Contains(a.msgs[2].Content, "dua bagian") {
		t.Fatalf("akhir task: %d pesan", len(a.msgs))
	}
}

func TestTaskDropsPlanAfterTwoBudgetFailures(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply(`{"mode": "plan", "subtasks": [{"title": "Big step", "verify": "test -f config.yaml"}]}`), // 0
		toolReply("bash", `{"command": "echo a"}`),                                                          // 1 -> anggaran (1) habis
		textReply(`{"mode": "plan", "subtasks": [
		  {"title": "P1", "verify": "test -f p1.txt"},
		  {"title": "P2", "verify": "test -f config.yaml"}]}`), // 2 pemecahan
		toolReply("bash", `{"command": "echo c"}`), // 3 -> anak juga habis: rencana dibuang
		textReply("Langsung saja: selesai."),       // 4 giliran biasa
	)
	a.cfg.SubSteps = 1
	if err := a.RunTask(context.Background(), "buat config"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen.String(), "rencana dibuang") {
		t.Fatalf("dua kegagalan beruntun harus membuang rencana:\n%s", screen)
	}
	m4 := msgsOf(f.reqs[len(f.reqs)-1])
	if req := content(m4[len(m4)-1]); !strings.Contains(req, "buat config") || !strings.Contains(req, "[harness] An earlier attempt") {
		t.Fatalf("giliran biasa harus membawa permintaan asli + catatan:\n%s", req)
	}
	if a.plan != nil || a.sub != nil {
		t.Fatal("plan harus nil setelah dibuang")
	}
}

// --- Temuan uji nyata pertama (lihat rencana.md): pencocokan verifier,
// verifier skrip, peringatan sentuh-verifier, path relatif di ringkasan.

func TestRunsVerifierMatchesCompound(t *testing.T) {
	v := "test -f tools/check.sh && grep -q 'gofmt -l' tools/check.sh"
	cases := []struct {
		cmd  string
		want bool
	}{
		{v, true},
		{v + " && echo ok", true},
		{"cd . ; " + v, true},
		{"grep -q 'gofmt -l' tools/check.sh", false},                                     // hanya sebagian
		{"test -f /abs/tools/check.sh && grep -q 'gofmt -l' /abs/tools/check.sh", false}, // path lain
		{"echo hi", false},
		{"", false},
	}
	for _, c := range cases {
		if got := runsVerifier(c.cmd, v); got != c.want {
			t.Errorf("runsVerifier(%q) = %v, mau %v", c.cmd, got, c.want)
		}
	}
}

func TestValidatePlanAcceptsScriptRun(t *testing.T) {
	env := &Env{}
	for _, ok := range []string{"bash tools/check.sh", "sh scripts/lint.sh", "./tools/check.sh"} {
		if ps := validatePlan([]Subtask{{Title: "x", Verify: ok}}, env); len(ps) != 0 {
			t.Errorf("menjalankan skrip hasil rencana harus diterima: %q -> %v", ok, ps)
		}
	}
	for _, bad := range []string{"bash", "bash -c 'echo hi'", "node index.js"} {
		if ps := validatePlan([]Subtask{{Title: "x", Verify: bad}}, env); len(ps) == 0 {
			t.Errorf("%q bukan done-condition", bad)
		}
	}
}

func TestVerifierTouchFlagSkipsFileChecks(t *testing.T) {
	a, _, _, screen := newFakeAgent(t)
	a.flagVerifierTouched(&Subtask{Verify: "grep -q usage docs/x.md"}, []string{"docs/x.md"})
	a.flagVerifierTouched(&Subtask{Verify: "test -f tools/check.sh && grep -q vet tools/check.sh"}, []string{"tools/check.sh"})
	if strings.Contains(screen.String(), "dilemahkan") {
		t.Fatalf("cek isi/keberadaan file memang menyebut filenya:\n%s", screen)
	}
	a.flagVerifierTouched(&Subtask{Verify: "python3 -m pytest tests/test_x.py"}, []string{"tests/test_x.py"})
	if !strings.Contains(screen.String(), "dilemahkan") {
		t.Fatal("mengubah file test yang dijalankan verifier harus ditandai")
	}
}

func TestSummaryUsesRelativePaths(t *testing.T) {
	a, f, dir, screen := newFakeAgent(t)
	f.replies = []string{toolReply("write_file", `{"path": "`+dir+`/abs.txt", "content": "x"}`), textReply("ok")}
	a.RunTurn(context.Background(), "tulis")
	if !strings.Contains(screen.String(), "write_file  abs.txt") {
		t.Fatalf("ringkasan harus relatif terhadap proyek:\n%s", screen)
	}
}

// --- Temuan uji nyata kedua: verifier yang tak bisa gagal, pra-terbang yang
// terlalu percaya, dan subtask yang lulus tapi tidak ditutup.

func TestValidatePlanRejectsExitMask(t *testing.T) {
	env := &Env{}
	for _, bad := range []string{
		"test -f tools/check.sh && bash tools/check.sh > /dev/null 2>&1; echo $?",
		"test -f x || echo missing",
		"go test ./...; echo done",
	} {
		ps := validatePlan([]Subtask{{Title: "x", Verify: bad}}, env)
		if len(ps) == 0 || !strings.Contains(strings.Join(ps, " "), "cannot fail") {
			t.Errorf("%q selalu exit 0, harus ditolak: %v", bad, ps)
		}
	}
	if ps := validatePlan([]Subtask{{Title: "x", Verify: "test -f x && grep -q y x"}}, env); len(ps) != 0 {
		t.Fatalf("rantai && yang jujur harus diterima: %v", ps)
	}
}

func TestPreflightDoesNotSkipWhenFilesMissing(t *testing.T) {
	a, _, _, screen := newFakeAgent(t)
	// "echo ok" selalu hijau; dengan files yang belum ada, itu bukti verifier bohong.
	if a.preflightGreen(context.Background(), &Subtask{Title: "x", Files: []string{"belum-ada.txt"}, Verify: "echo ok"}) {
		t.Fatal("hijau dengan file yang belum ada tidak boleh melewati subtask")
	}
	if !strings.Contains(screen.String(), "dicurigai") {
		t.Fatalf("user harus diberi tahu verifiernya dicurigai:\n%s", screen)
	}
	if !a.preflightGreen(context.Background(), &Subtask{Title: "y", Files: []string{"a.txt"}, Verify: "test -f a.txt"}) {
		t.Fatal("hijau dengan file yang memang ada boleh dilewati")
	}
}

func TestVerifiedSubtaskClosesOnBudget(t *testing.T) {
	a, _, _, screen := newFakeAgent(t,
		textReply(`{"mode": "plan", "subtasks": [{"title": "Write m.txt", "verify": "test -f m.txt"}]}`),
		toolReply("write_file", `{"path": "m.txt", "content": "m"}`),
		toolReply("bash", `{"command": "test -f m.txt && echo ok"}`), // verifier lulus; anggaran (2) habis di sini
		textReply("Selesai."),
	)
	a.cfg.SubSteps = 2
	if err := a.RunTask(context.Background(), "buat m.txt"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen.String(), "ditutup oleh harness") {
		t.Fatalf("verifier lulus + anggaran habis harus ditutup harness, bukan dipecah:\n%s", screen)
	}
	if strings.Contains(screen.String(), "dipecah") {
		t.Fatal("tidak boleh ada pemecahan untuk subtask yang sudah lulus")
	}
	if len(a.msgs) != 3 {
		t.Fatalf("akhir task: %d pesan", len(a.msgs))
	}
}
