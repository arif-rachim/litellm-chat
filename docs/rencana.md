# Rencana harness lchat

Ini **satu-satunya** rencana proyek. Dulu ada tiga yang tumpang tindih — daftar
"sengaja belum dikerjakan" di `harness.md`, sebuah rencana dekomposisi tugas 848
baris dari sesi lain, dan tiga mekanisme yang sebenarnya sudah jadi — dan
ketiganya dilebur ke sini setelah dianalisis. Yang terbukti bagus diambil, yang
dibuang dicatat di bagian akhir beserta alasannya, supaya orang berikutnya tidak
mengusulkannya lagi tanpa tahu kenapa ia pernah ditolak.

Bahasa dan gayanya mengikuti `harness.md`: menjelaskan *kenapa*, memakai nama
fungsi sebagai patokan karena nomor baris bergeser, dan menyebut test mana yang
menjaga tiap perilaku.

## Prinsip

> **"Selesai = exit code" dulu, dekomposisi terakhir — dan ramping.**

Empat tahap berurutan. Tiap tahap bisa dikirim sendiri dan diuji tanpa model
sungguhan (`fakeLLM`, `newFakeAgent` di `agent_test.go`). Urutannya bukan
pilihan: Tahap 4 dibangun di atas gerbang Tahap 1 dan handoff Tahap 3, dan
Tahap 1-2 berguna di **setiap** giliran sementara Tahap 4 hanya di `/task`.

Status: **keempat tahap selesai** (dijelaskan di `harness.md` §3.4-§3.7).
Yang tersisa adalah pemakaian nyata: ambang-ambang di bawah (`SubSteps` 8,
`noProgressSteps` 6, awal osilasi di rentetan 2, `maxSplits` 3) belum pernah
diuji dengan model dan proyek sungguhan, dan itu satu-satunya cara menyetelnya.
Yang sudah jalan sebelum rencana ini: catatan percobaan, tanda kegagalan, dan
tangga eskalasi (`harness.md` §3).

---

## Tahap 1 — Gerbang verifikasi: harness yang menjalankan, bukan mengomel

**Masalah.** Hari ini `verifyReminder` (`reason.go`) hanya *mengingatkan sekali*
saat model menyatakan selesai tanpa memverifikasi. Lebih buruk, `st.verifiedAfter`
disetel oleh `reVerifyCmd` (`tools.go`) yang sangat longgar: `node x.js`,
`python3 y`, `npx z`, dan `make` telanjang semuanya dihitung "verifikasi". Model
kecil bisa memuaskan pengingat itu dengan perintah yang tidak membuktikan apa
pun. Kedua rencana lama berdiri di atas fondasi ini tanpa menyadarinya.

**Pekerjaan.**

1. **Pisahkan "menjalankan sesuatu" dari "memverifikasi sesuatu".** `reVerifyCmd`
   dipersempit ke: awalan salah satu `env.VerifyCmds` (`envinfo.go`), atau
   bentuk cek eksplisit — `go test|build|vet`, `npm test`, `pytest`,
   `cargo test`, `test -f`, `grep -q`, `diff`, `git diff --exit-code`. `node`,
   `python3`, `npx`, `make` telanjang keluar dari daftar. Test tabel mengunci
   mana yang dihitung dan mana yang tidak.
2. **Cabang verifikasi di `finishCheck` (`agent.go`) jadi gerbang keras.** Kalau
   ada mutasi non-prosa yang belum diverifikasi dan `env.VerifyCmds` tidak
   kosong, harness **menjalankan sendiri** `env.VerifyCmds[0]` — lewat gerbang
   izin yang sama dengan panggilan bash model (`a.allowed`, `commandRisk`) —
   dan hasilnya masuk sebagai pesan tool plus `noteResult`, sehingga kegagalan
   masuk ledger dan tangga seperti kegagalan lain. Lulus → jawaban akhir
   diterima. Gagal → giliran berlanjut dengan bukti kegagalan di konteks.
   `verifyReminder` hanya dipakai ketika tidak ada `VerifyCmds` yang dikenal.
3. **Tanda centang `todo` milik harness.** `runTodo` (`tools.go`) melakukan
   `a.todos = todos` — model bisa menulis ulang rencananya agar cocok dengan
   yang terlanjur dikerjakan, dan harness tidak tahu. Perbaikannya bukan
   merebut daftarnya, melainkan centangnya: sebuah item hanya boleh `done`
   kalau ada verifikasi yang lulus sejak item itu `in_progress`. Pelanggaran →
   status diturunkan ke `in_progress` + `correction()`. Kalau daftar menyusut,
   layar menyebut `rencana diubah: N langkah dihapus`. Model tetap memiliki
   daftarnya; harness memiliki centangnya.

**Berkas:** `reason.go` (regex, `verified()`), `agent.go` (`finishCheck`, helper
`runVerifier`), `tools.go` (`runTodo`), `agent_test.go`.

**Test yang harus ada:** tabel `reVerifyCmd`; gerbang yang menjalankan
`VerifyCmds[0]` lalu menerima jawaban saat lulus dan menolak saat gagal; `todo`
yang `done` tanpa verifikasi diturunkan.

**Dibangun** — `isVerifyCmd` + `reCheckForm` menggantikan `reVerifyCmd`;
`runVerifier`/`pickVerifier` di `agent.go`; `auditTodos` di `tools.go`;
test `TestIsVerifyCmd`, `TestHarnessRunsVerifier`,
`TestTodoCheckmarksAreHarnessOwned`. Satu penyimpangan kecil yang disengaja:
`pickVerifier` memilih perintah yang mengandung `test` lebih dulu, bukan
`VerifyCmds[0]` mentah — untuk proyek Go, `[0]` adalah `go build ./...` yang
lulusnya membuktikan lebih sedikit daripada `go test ./...`.

---

## Tahap 2 — Sinyal kemajuan

Tiga item yang dulu ada di `harness.md` §5, tidak berubah.

1. **Osilasi isi file.** Simpan sha isi file tiap mutasi (`st.noteMutation`).
   Isi yang kembali ke hash yang pernah ada berarti model membatalkan
   pekerjaannya sendiri (A→B→A) — bukti keras, langsung ke anak tangga
   diagnosis dengan `failSig` khusus `oscillation:<path>`.
2. **Anggaran tanpa-kemajuan.** Sinyal kemajuan = file baru dibaca
   (`a.readFiles`: hari ini ditulis di `tools.go` tapi tidak pernah dibaca),
   `failSig` baru, mutasi, atau verifikasi lulus. Enam langkah tanpa satu pun
   sinyal → tangga naik satu anak tangga, bukan giliran dimatikan.
3. **Wajib baca sebelum edit kedua.** Dua `edit_file` ke path yang sama tanpa
   `read_file` atau verifikasi di antaranya ditolak dengan `correction()`.
   Datanya sudah ada di `st.mutated` dan `st.verifiedAfter`.

**Berkas:** `reason.go`, `agent.go` (`execTool`), `agent_test.go`.

**Dibangun** — `st.seenContent`/`contentHash`, `st.noteRead`/`noteEdited`/
`noteVerified`, `checkProgress` + `noProgress`, penolakan `edit_unread`; test
`TestOscillationIsCaught`, `TestNoProgressBudget`, `TestSecondEditNeedsRead`.
Tiga keputusan yang menyimpang dari teks rencana, semuanya disengaja:

- "Baca sebelum edit kedua" berlaku setelah edit yang **berhasil** saja. Edit
  yang gagal (`edit_nomatch`) sudah mengembalikan potongan file terdekat, jadi
  percobaan ulang dengan `old_string` yang dikoreksi adalah langkah sah.
- Anggaran tanpa-kemajuan punya tangga mini sendiri (sebutkan yang kurang →
  `ask_user` → berhenti), tidak menumpang tangga kegagalan: menumpang akan
  me-reset rentetan kegagalan yang sedang berjalan.
- "File baru dibaca" dihitung per giliran (`st.readPaths`), bukan lewat
  `a.readFiles` yang bertahan lintas giliran dan `/clear`; `a.readFiles` tetap
  disimpan untuk handoff Tahap 3.

---

## Tahap 3 — Checkpoint konteks: dekomposisi tanpa planner

Inti manfaat dekomposisi adalah **konteks yang tidak tumbuh melewati kapasitas
model**. Itu bisa didapat tanpa planner: begitu sebuah item `todo` lulus gerbang
Tahap 1 (verifikasi exit 0 setelah mutasi), harness **memadatkan** `a.msgs`
menjadi `{system, permintaan asli user, baris handoff}` dan mereset ledger.

- **Handoff dicatat harness, bukan model:** judul item, file yang berubah,
  perintah verifikasi beserta exit code-nya, satu baris ringkasan dari model
  (`clip(…, 160)`), dan path yang sudah dibaca — path saja, bukan isinya.
- Handoff ikut di klon pesan sistem seperti ledger, jadi invarian #1
  (`harness.md` §4) utuh. `fitContext` tidak berubah; jangkar
  `msgs[0]`/`msgs[1]`-nya justru cocok dengan tata letak ini.
- **Amandemen invarian #3:** `turnState` berumur satu *jendela konteks*, bukan
  satu giliran. Ledger adalah indeks ke bukti yang ada di konteks; kalau
  konteksnya diganti tapi ledgernya ikut, model membaca ringkasan percobaan
  yang buktinya sudah tidak bisa dibuka — terlihat seperti pengetahuan, padahal
  bukan. Yang boleh menyeberang hanya **fakta terverifikasi** di handoff.
- Ctrl+C tetap membatalkan giliran. Tidak ada semantik baru.

Yang **tidak** dibangun di tahap ini: `/task`, planner, `subtask_done`,
`split_subtask`, `Tool.Scope`, loop kedua.

**Berkas:** `agent.go` (`checkpoint()`, dipanggil dari `finishCheck`/`runCalls`
saat item terverifikasi), `reason.go` (tipe `handoff`), `agent_test.go`
(`TestCheckpointLeavesCleanContext`).

**Dibangun** — `handoff`, `st.newWindow`, `st.handoffBlock`, `Agent.checkpoint`,
pemicu `st.milestone` dari `auditTodos`, `st.verifyCmd`; test
`TestCheckpointLeavesCleanContext`. Penyimpangan yang disengaja dari teks
rencana:

- `checkpoint` dipanggil dari `RunTurn` setelah `runCalls`, bukan dari dalam
  `finishCheck`/`runCalls`: pemadatan harus terjadi setelah pesan tool terakhir
  masuk `a.msgs`, kalau tidak pesan itu menjadi yatim di jendela baru.
- Pemicunya hanya item `todo` yang ditutup dengan **mutasi** terverifikasi.
  Langkah membaca yang ditutup tanpa mutasi tidak memadatkan, karena isi yang
  baru dibaca justru yang dibutuhkan langkah berikutnya.
- "Path yang sudah dibaca" memakai `st.readSeen` (gabungan semua jendela di
  giliran ini), bukan `a.readFiles` yang bertahan lintas giliran dan `/clear`.
  `a.readFiles` kini tidak punya pemakai; boleh dihapus di pembersihan nanti.
- Ringkasan model diambil dari kalimat terakhir asisten sebelum menutup item,
  tanpa menambah parameter ke tool `todo`.

---

## Tahap 4 — `/task` + planner, versi ramping

Dikerjakan setelah Tahap 3. Memakai gerbang Tahap 1 sebagai `subtask_done` dan
handoff Tahap 3 sebagai konteks antar subtask. Bentuknya adalah rencana
dekomposisi lama **dikurangi** semua bagian yang memindahkan perencanaan ke
model kecil atau menambah tangga.

**Diambil dari rencana lama:**

- Pemicu deterministik: `/task <permintaan>` di REPL, `--task` untuk `-p`.
  Bukan heuristik pada pesan user.
- Planner boleh menjawab `{"mode":"direct"}` sehingga salah-picu hanya
  memakan satu panggilan. `LCHAT_PLANNER_MODEL`; kosong = model utama, jadi
  default tidak menambah biaya. Planner memakai `SelectProfile` sendiri, tanpa
  key `tools`, thinking dipaksa nyala, parsing lewat `repairJSON` yang sudah
  bertest; satu percobaan ulang dengan `correction()`, lalu jatuh ke `RunTurn`.
- `validatePlan` + `acceptableVerifier` + `reVerifierCheat` (menolak `|| true`,
  `cat`, `ls`, verifier yang tidak bisa gagal).
- Konteks bersih per subtask: `a.msgs = {system + taskBlock, subtaskBlock}`.
- Batas keras: `maxSubtasks = 8`, `maxSplits = 3`, plafon langkah total
  `MaxSteps*3`. **Dua subtask beruntun kehabisan anggaran → rencana dibuang**,
  jatuh ke satu `RunTurn` biasa dengan catatan apa yang sudah dikerjakan.
- `finishTask` menulis ulang `a.msgs` menjadi satu giliran sintetis bersih,
  agar pesan lanjutan user mendarat di konteks yang tahu permintaan aslinya.
- Jawaban `ask_user` di dalam subtask ikut ke handoff — keputusan user adalah
  fakta tentang dunia.

**Ditambahkan (wajib):**

- **Pra-terbang "merah dulu".** Sebelum subtask dimulai, harness menjalankan
  perintah `verify`-nya sekali. Sudah exit 0 → done-condition kosong → subtask
  dilewati atau planner diminta ulang. Ini menutup mode gagal terbesar rancangan
  ini: planner kecil memilih verifier yang sudah hijau. Kira-kira 20 baris.
- Tandai di layar subtask yang mengubah file yang disebut di perintah
  `verify`-nya sendiri.
- Anggaran langkah subtask = `8 + 2 × jumlah files`, bukan 8 datar.
- Mode `-p --task`: rencana dicetak lalu disetujui otomatis, karena tidak ada
  yang bisa ditanya.

**Dibuang:**

- `split_subtask` sebagai tool model. Pemecahan hanya oleh harness, lewat
  planner dengan ledger subtask yang gagal sebagai bukti.
- `Tool.Scope`. Cukup `toolSchemas(task)` yang menyembunyikan `todo` di dalam
  subtask; di luar `/task`, `todo` tetap ada dengan centang milik harness.
- Tiga tangga. Di dalam subtask hanya satu tangga pendek: 2× diagnosis, 3×
  kembalikan ke orkestrator. Orkestrator yang memutuskan pecah → tanya →
  hentikan.

**Fase pengerjaan:** plumbing inert → `plan.go` murni (`validatePlan`,
`acceptableVerifier`, renderer) → `planner.go` → refaktor netral `RunTurn` →
`loop` dengan gerbang *semua test lama lulus tanpa diedit* → jalur bahagia →
pemecahan oleh harness → poles dan dokumen.

**Test yang dipertahankan dari rencana lama:**
`TestValidatePlanRejectsUncheckableSubtask`, `TestPlannerUsesItsOwnModel`,
`TestDirectVerdictSkipsDecomposition`, `TestPlannerRetriesOnceThenFallsBack`,
`TestSubtaskContextIsFresh`, `TestSubtaskDoneNeedsTheVerifier`,
`TestTaskLeavesOneCleanTurn`; ditambah `TestVerifierMustBeRedFirst`.

**Berkas baru:** `plan.go`, `planner.go`, `subtask.go`, `task_test.go`.
**Diubah:** `agent.go`, `reason.go`, `tools.go`, `prompt.go`, `main.go`,
`config.go`, `harness.md` (§1, §2, §3.7, §6).

**Dibangun** — semuanya di atas, dengan penyimpangan yang disengaja dari teks
rencana:

- Test murni digabung ke `task_test.go`, tidak ada `plan_test.go` terpisah.
- `TestSubtaskContextIsFresh`, `TestSubtaskDoneNeedsTheVerifier`, dan
  `TestTaskLeavesOneCleanTurn` dilebur menjadi satu alur
  `TestSubtaskRunEndToEnd`: ketiganya adalah tiga asersi pada satu run yang
  sama, dan satu skenario terskrip lebih mudah dijaga daripada tiga.
- Model tidak punya tool untuk mengembalikan subtask; ia menjawab
  `BLOCKED: …` sebagai jawaban akhir setelah satu pengingat. Deterministik dan
  tidak menambah skema tool.
- Jawaban akhir saat verifier sudah lulus tapi `subtask_done` lupa dipanggil
  **ditutup oleh harness** (teksnya jadi ringkasan) — model kecil sering lupa
  langkah penutup, dan buktinya sudah ada di tangan harness.
- Tidak ada seam `makePlan` untuk test: `fakeLLM` sudah cukup karena planner
  menjawab JSON biasa.
- `finishTask` tidak pernah bergantung pada model: kalau panggilan laporan
  gagal, harness menyusun laporan dari handoff.

---

## Uji nyata pertama — `/task` di salinan proyek ini

Dijalankan sekali dengan `qwen/qwen3.5-35b-a3b` lewat OpenRouter, `--yolo -p`,
pada salinan proyek (repo ini belum punya commit, jadi tidak diuji langsung di
folder proyek). Tugas: buat `tools/check.sh` (gofmt, vet, test; exit non-zero)
dan dokumentasikan di `pengembangan.md`. Hasil: 71 detik, selesai, skripnya ada
dan jalan, dokumentasinya masuk. Tapi jalannya tidak mulus, dan justru itu
gunanya: empat bug harness yang tidak mungkin ketahuan dari `fakeLLM`.

1. **`runsVerifier` tidak pernah bisa mencocokkan verifier majemuk.** Planner
   menulis `test -f … && grep -q … && grep -q …`; model menjalankannya persis
   (plus `&& echo ok`), tapi pencocokan membandingkan *tiap segmen* perintah
   dengan *seluruh* string verifier. `subtask_done` ditolak dua kali, anggaran
   10 langkah habis untuk tugas yang sebenarnya sudah selesai di langkah 4,
   subtask dipecah, dan planner — salah membaca bukti — memindahkan skrip ke
   `docs/tools/`. Perbaikan: segmen verifier harus muncul **berurutan dan
   bersambung** di antara segmen perintah (`TestRunsVerifierMatchesCompound`).
2. **Peringatan "ceknya dilemahkan" salah sasaran** untuk verifier `grep -q` /
   `test -f`: file itu memang harus diubah subtasknya. Sekarang dilewati untuk
   verifier berbentuk cek file (`reFileCheck`, `TestVerifierTouchFlagSkipsFileChecks`).
3. **`bash docs/tools/check.sh` ditolak sebagai verifier.** Untuk subtask yang
   deliverable-nya skrip, menjalankan skrip itu adalah cek yang paling jujur,
   dan pra-terbang menjaminnya merah dulu (skrip belum ada → gagal). Diterima di
   `verifierProblem` lewat `reScriptRun`, hanya untuk rencana `/task`; gerbang
   umum `isVerifyCmd` tetap ketat (`TestValidatePlanAcceptsScriptRun`).
4. **Path absolut membuat layar, ledger, dan handoff tidak terbaca**
   (`write_file /tmp/claude-1000/-home-…`). Ringkasan panggilan kini relatif
   terhadap proyek (`TestSummaryUsesRelativePaths`).

**Run kedua** (biner yang sudah diperbaiki, tugas sama): hasilnya benar —
`tools/check.sh` di tempat yang diminta, executable, `gofmt -l .` yang benar,
dokumentasi masuk, laporan akhir jujur — tapi jalannya membuka dua lubang lagi:

5. **Verifier yang tak bisa gagal lolos validasi.** Planner menulis
   `test -f … && bash … ; echo $?`: `echo $?` di belakang `;` membuat exit code
   selalu 0. Pra-terbang melihat "hijau" padahal filenya belum ada, subtask 1
   dilewati, subtask 2 membuang dua langkah mencari file yang tak pernah dibuat.
   Dua perbaikan: `reVerifierCheat` menolak `; echo`, `|| echo`, dan `$?`
   (`TestValidatePlanRejectsExitMask`); dan pra-terbang **tidak** melewati
   subtask yang hijau kalau file di `files`-nya belum ada — verifier dicurigai,
   subtask tetap dijalankan (`TestPreflightDoesNotSkipWhenFilesMissing`).
6. **Verifier lulus, model tidak menutup.** `test -x tools/check.sh` lulus di
   langkah 7 (harness melihatnya), tapi model mengabaikan rider "call
   subtask_done now" dan lanjut mengerjakan subtask *berikutnya* sampai anggaran
   habis → dipecah jadi tiga anak yang semuanya langsung dilewati pra-terbang.
   Dua perbaikan: subtask yang `verified` **ditutup oleh harness** saat anggaran
   habis atau dikembalikan — fakta di tangan harness mengalahkan diamnya model
   (`TestVerifiedSubtaskClosesOnBudget`); dan tepat saat verifier lulus, output
   tool-nya sendiri ditempeli `[harness] The done-condition has passed. Call
   subtask_done now…` — rider di pesan sistem saja tidak cukup untuk model
   kecil ini.

**Run ketiga** (kedua perbaikan di atas masuk, tugas sama): **32 detik, 2
subtask, nol intervensi harness.** Planner langsung menulis verifier majemuk
yang jujur (`test -x … && grep -q … && …`), subtask 1 tertutup pada percobaan
pertama begitu verifier dijalankan, tidak ada pemecahan, tidak ada hijau palsu.
Dari 71 detik dengan satu pemecahan dan tiga subtask dilewati, menjadi 32
detik lurus — perbedaannya seluruhnya di harness, model dan tugasnya sama.

Satu temuan yang bertahan di ketiga run, dan ini batas yang jujur: skrip yang
ditulis model selalu memakai `gofmt -l ./...` (gofmt menerima path, bukan pola).
Di run 3 skripnya lebih ketat sehingga gagal jujur (exit 1) pada tree yang
bersih — kesalahan yang **bisa** ditangkap verifier "jalankan skripnya"
(merah sebelum, dan tetap merah karena skripnya salah), tapi **tidak** oleh
verifier `grep` yang dipilih planner. Aturan 3 prompt planner kini
*mengutamakan* menjalankan deliverable yang berupa skrip alih-alih meng-grep
teksnya. "Selesai = exit code" menjamin kontraknya dipenuhi; memilih kontrak
yang benar tetap tugas planner, dan di situlah prompt harus terus disetel
dengan data seperti ini.

Yang bekerja seperti dirancang: vonis planner dikoreksi sekali lalu jalan;
pemecahan oleh harness membawa ledger; dua anak hasil pemecahan yang sudah
terpenuhi **dilewati oleh pra-terbang** alih-alih dikerjakan ulang; verifikasi
ulang terakhir; laporan akhir; `a.msgs` bersih.

Temuan soal model, bukan harness: skrip yang ditulisnya memuat
`gofmt -l ./...` (gofmt menerima path, bukan pola) yang gagal diam-diam ke
stderr sehingga ceknya kosong dan lolos. Verifier `grep` tidak bisa menangkap
itu, dan verifier "jalankan skripnya" pun tidak (exit tetap 0). Ini batas
yang jujur dari "selesai = exit code": ia menjamin *kontrak* dipenuhi, bukan
bahwa kontraknya benar. Laporan akhir model juga menyebut verifikasi yang
sebenarnya dilewati (`test -x tools/check.sh`) — fakta harness ada di handoff,
teks laporan adalah kata-kata model.

## Temuan dari sesi user: gambar yang tidak sampai

Transkrip sesi user (memperbaiki game Flappy Bird dengan screenshot) menunjukkan
model membuang ±12 langkah: mencoba `read_file` pada PNG (prompt izin, lalu
"file biner"), lalu **delapan** skrip PIL satu-baris terpisah untuk "melihat"
gambar lewat statistik piksel. Bukan model yang salah — tiga celah harness
membuatnya tidak pernah benar-benar mendapat gambarnya:

1. Path gambar yang **diketik** (bukan di-paste) di mode editor tidak pernah
   dilampirkan; model hanya menerima teks path. Sekarang dilampirkan
   (`repl`, kecuali baris perintah `/…`).
2. Lampiran dikirim sebagai `[text, image_url]` tanpa satu kata pun di teks
   bahwa ada gambar; model kecil tidak sadar dan lari ke tool. Sekarang teks
   pesan menyebut `[attached image: …]` (`TestAttachedImageIsNamedInText`).
3. `read_file` pada gambar menjawab "file biner" — jalan buntu, padahal naluri
   model itu benar. Sekarang gambarnya **dilampirkan** ke pesan `user` yang
   dikirim tepat setelah batch tool, agar hasil tool tetap bersambung di
   belakang pesan asisten (`TestReadFileOnImageAttachesIt`).

Ditambah dua aturan prompt: jangan analisis gambar dengan skrip (gambarnya ada
di percakapan), dan beberapa perintah shell untuk satu pertanyaan dijalankan
dalam **satu** panggilan bash. Belum diuji dengan model sungguhan; transkrip
berikutnya dari user adalah ujinya.

Transkrip lanjutan sesi yang sama menunjukkan mode gagal kedua: giliran
kehabisan **30 langkah (408 detik) di tengah refactor** — `const [tilt, setTilt]`
baru dideklarasikan, `const tilt = …` lama sudah dihapus, tapi `setTilt(...)`
belum sempat ditulis. Build tetap hijau; burungnya berhenti miring. Regresi
diam-diam, dan harness hanya bilang "berhenti" plus saran `/task` yang menggema
pesan user terpotong 60 karakter. Sekarang `reportUnfinished`: menyebut file
yang berubah, menjalankan verifier proyek kalau belum ada yang memverifikasi,
melaporkan lulus/gagal, dan **membawa catatannya ke giliran berikutnya** di
pesan user yang sama (`a.carry`) — "periksa perubahan setengah jadi: variabel
yang dideklarasikan tapi tak dipakai, kode yang dihapus tanpa pengganti".
Saran `/task` tidak lagi menggema pesan. Sejak sesi ini pula ada **log sesi
JSONL** (`harness.md` §5b), supaya analisis berikutnya tidak lagi bergantung
pada tempelan layar yang terpotong.

**Log pertama yang dianalisis** (`sessions/a.jsonl`) langsung membayar
dirinya: dua giliran berturut-turut berakhir dalam 3,5 dan 4,5 detik dengan
**0 langkah** — `reply` bertuliskan `finish: "stop"`, `calls: null`, isinya
"Mari saya perbaiki logika collision detection." Model *mengumumkan* langkah
tanpa melakukannya, dan `finishCheck` menerimanya sebagai jawaban akhir. Kolom
`reasoning` menunjukkan niatnya benar ("I need to read App.jsx") — yang hilang
hanya eksekusinya. Ini pola paling umum model kecil, dan tanpa log ia terlihat
seperti "model males". Sekarang `looksLikeIntent`: balasan akhir yang
berbentuk niat orang-pertama (ID/EN) ditegur sekali per giliran — *lakukan
sekarang, atau kalau memang selesai jawab tanpa mengumumkan langkah*
(`TestIntentWithoutActionIsNudged`). Heuristik kata kunci, sadar itu; biayanya
kalau salah tebak satu permintaan pendek, dan hanya sekali. Log yang sama juga
menunjukkan gambar kini sampai ke model, dan bahwa `tokens_est` ±4× terlalu
besar untuk gambar (11,5k vs 2.963 nyata) — estimasi dikalibrasi ke angka
terukur (39 KB PNG ≈ 900 token).

**Log kedua** (`2026-09-20T15-12-19.jsonl`), setelah teguran niat-tanpa-aksi
masuk: model tidak lagi hanya berjanji — ia menulis tool call sebagai **tag
teks** yang tidak dikenal parser: `<todo>\n1. …\n</todo>` lalu
`<read_file> {"path": "game/src"}`, dengan `tools=7` ikut dikirim dan
`finish: "stop"`. Teguran menyala sekali, model mengulang tag yang sama,
giliran selesai lagi tanpa satu pun tool. Sekarang format `tag_json`
(`tagCalls`) selalu dicoba terakhir apa pun profilnya: tag harus nama tool
yang dikenal, isinya objek JSON dengan pencocokan kurung yang benar, dan
`<todo>` berisi daftar bernomor menjadi item todo
(`TestTagJSONToolCalls`, `TestTagTextToolCallsAreExecuted`). Satu pola yang
patut diwaspadai dari tiga log ini: setiap giliran yang **membawa gambar**
tidak pernah menghasilkan tool call native dari `qwen/qwen3.5-35b-a3b` lewat
OpenRouter, sedangkan giliran tanpa gambar menghasilkannya. Belum bisa
dipastikan penyebabnya (penyedia yang dipilih OpenRouter untuk permintaan
multimodal, atau modelnya sendiri); parser tag adalah jaring pengamannya, dan
`LCHAT_PLANNER_MODEL`/`/model` ke model vision yang tool-calling-nya kuat
adalah jalan keluarnya kalau pola ini bertahan.

**Log ketiga** (`15-19-23.jsonl`) menutup rantainya. `<todo>` tag berhasil
dikonversi (parser bekerja), lalu langkah 1-3 — thinking **mati** — model
mengeluarkan kalimat yang sama persis tiga kali ("Mari saya lihat struktur
folder game…", `calls: null`). Teguran niat menyala sekali, pengingat todo
sekali, lalu semua teguran satu-kali habis dan kalimat ketiga **diterima
sebagai jawaban**. Tiga perbaikan: teguran niat kini menyalakan thinking (model
ini tanpa reasoning hanya mengulang); balasan teks identik dua kali → thinking
+ "balas dengan tool call saja", tiga kali → giliran dihentikan terang-terangan
(`TestIdenticalRepliesStallTheTurn`); dan **gambar hanya dikirim pada request
tepat setelah dilampirkan** (`staleImagesAsText`,
`TestImageSentOnlyOnRequestAfterAttach`) — empat sesi berturut-turut tidak
pernah menghasilkan tool call native selama bagian gambar ada di request, dan
setiap giliran tanpa gambar menghasilkannya. Model sudah menulis apa yang
dilihatnya di reasoning langkah pertama; `read_file` melampirkan lagi kalau ia
perlu melihat ulang. Ini keputusan berbasis bukti dari log, bukan teori — dan
kalau sesi berikutnya menunjukkan tool call native muncul di langkah 1 setelah
gambar diganti teks, hipotesisnya terkonfirmasi.

**Log keempat** (`15-24-56.jsonl`) **membantahnya**: langkah 1 tanpa gambar
(`tokens_est` 3656 → 2886), thinking nyala, tetap `calls: null`. Bentuknya
yang memberi jawaban: `read_file {"path": "game/src/game.js"}` — tiga baris,
tanpa tag — **persis format contoh di prompt sistem** ("-> bash {…}" di
*Example of a good sequence*). Model kecil meniru contohnya secara harfiah;
selama harness mengajarkan bentuk itu, ia wajib membacanya. Format `bare_json`
(`bareCalls`): satu panggilan per baris, nama harus tool yang dikenal, JSON
harus tertutup di baris itu (`TestBareJSONToolCalls`,
`TestBareTextToolCallsAreExecuted`). Pengiriman-gambar-sekali tetap
dipertahankan (murah, dan mengurangi token), tapi klaim kausalnya dicabut:
bukti empat log berikutnya belum cukup untuk menyalahkan gambar. Yang pasti
dari lima log: model ini sering **tidak memakai tool call native** dan menulis
panggilannya sebagai teks dalam tiga bentuk berbeda — parser sekarang mengenal
ketiganya, dan itu lebih tahan banting daripada berusaha memaksa bentuk native.

## Analisis rencana dekomposisi: apa yang diambil, apa yang diperbaiki

Rencana 848 baris itu dianalisis dengan klaim-klaimnya diverifikasi ke kode.

**Kelebihan — nyata, diambil:**

1. Diagnosis tiga penghambat struktural tepat: rencana milik model (`runTodo`
   memang melakukan `a.todos = todos`), tidak ada isolasi konteks, anggaran
   langkah datar.
2. **Gerbang done-condition** — `subtask_done` ditolak sampai harness melihat
   exit 0 — adalah ide terkuat dari semua rencana. Ia mengubah "pengingat
   verifikasi" (omelan) menjadi gerbang keras.
3. Handoff dicatat harness, sehingga klaim model tidak bisa diselundupkan ke
   konteks berikutnya.
4. Katup pengaman berlapis: `direct`, `validatePlan`, batas keras, buang rencana
   setelah dua kegagalan anggaran.
5. Memakai ulang seam yang ada — `repairJSON`, `correction`, klon pesan sistem,
   jangkar `fitContext` — dan bagian risikonya ditulis jujur.

**Kekurangan — nyata, diperbaiki di rencana ini:**

1. **Ukuran vs. etos proyek.** Tiga file baru, enam diubah, dua tool baru,
   protokol planner, loop kedua, scope tool, logika split, rollup handoff,
   `finishTask`, semantik Ctrl+C, refaktor `runWith`, 14 test — kira-kira
   menggandakan harness pada proyek 6,8k baris tanpa dependensi. Separuh dari
   11 risiko di dokumennya sendiri lahir dari mesin orkestrasinya. → Tahap 4
   dirampingkan; yang berguna untuk setiap giliran dikerjakan lebih dulu.
2. **Planner = model kecil yang sama**, disuruh melakukan hal paling sulit
   (memecah repo asing menjadi unit yang bisa dicek). `validatePlan` memeriksa
   *bentuk* verifier lewat regex, bukan *kebenarannya*: tidak ada yang mengecek
   verifier **merah sebelum** subtask, padahal verifier yang sudah hijau tidak
   bermakna dan itulah keluaran paling mungkin dari planner kecil. → pra-terbang
   "merah dulu".
3. **`split_subtask`** meminta model kecil merencanakan — persis yang ia tidak
   bisa. → pemecahan oleh harness saja.
4. **Tiga tangga eskalasi** sulit dinalar. → satu tangga pendek di dalam subtask.
5. **Anggaran 8 langkah dengan konteks bersih sempit**: membaca ulang 2-3 file
   memakan hampir semuanya. → skala dengan jumlah file.
6. **Verifikasi teater** hanya ditutup di sisi `|| true`; kecurangan realistis
   adalah mengedit test atau memilih verifier yang kebetulan lulus. → "merah
   dulu" + tandai subtask yang mengubah file yang disebut verifier-nya.
7. **Fondasi longgar yang tidak disadari kedua rencana**: `reVerifyCmd` menerima
   `node x.js` sebagai verifikasi. → Tahap 1 butir 1, sebelum apa pun.
8. Jalur `-p --task` tanpa persetujuan tidak dispesifikasikan; nasib `todo`
   digantung. → keduanya diputuskan di Tahap 4.

---

## Yang ditolak, dan alasannya

- **Dekomposisi dulu, reaktif belakangan** (urutan rencana lama). Membalik
  rasio biaya/manfaat: tiga item Tahap 2 ±100 baris dan membantu setiap
  giliran; dekomposisi ±1500 baris dan hanya membantu `/task`. Dekomposisi juga
  *membutuhkan* gerbang verifikasi, jadi Tahap 1 harus ada duluan apa pun
  keputusannya.
- **`split_subtask` sebagai tool** — memindahkan perencanaan ke model kecil.
- **Menaikkan temperatur otomatis setelah gagal.** Menambah keragaman pada
  model yang sedang tidak paham, bukan menambah informasi — kebalikan dari
  prinsip `harness.md` §0.
- **Heuristik panjang/kata kunci pesan untuk memicu dekomposisi** — tebakan,
  bertentangan dengan prinsip §0. Pemicunya harus deterministik (`/task`).
- **Mode keempat `build`** — bertabrakan dengan `Mode` yang semantiknya soal
  izin; `/task` ortogonal dan bisa dikombinasikan dengan mode mana pun.
