# Harness lchat: bagaimana model kecil dibuat terarah

Dokumen ini untuk orang (atau agen) yang akan mengubah kode ini berikutnya.
Isinya bukan cara memakai lchat — itu ada di `README.md` dan
`docs/panduan-lchat.pdf` — melainkan **kenapa loop agennya berbentuk seperti
sekarang**, supaya perubahan berikutnya tidak diam-diam membatalkan alasannya.

Nomor baris di bawah bisa bergeser; nama fungsinya yang dipakai sebagai patokan.

## 0. Satu prinsip yang mendasari semuanya

> Model kecil tidak bisa disuruh "lebih pintar". Yang bisa diubah adalah
> **bentuk tugas** yang diberikan kepadanya.

Setiap kali harness mendeteksi model tersesat, jawabannya bukan menaikkan
tekanan ("coba lagi dengan lebih teliti") melainkan mengganti pertanyaan menjadi
sesuatu yang lebih sempit dan lebih konkret. Omelan yang sama diulang-ulang
justru dipelajari model kecil sebagai token yang bisa diabaikan.

## 1. Anatomi satu giliran

`Agent.RunTurn` (`agent.go`) menyiapkan giliran lalu mendelegasikan ke
`Agent.loop`, siklus yang sama persis yang dipakai satu subtask `/task` (§3.7):

```
RunTurn(input)            RunTask(goal) → runSubtask → loop(budget)
 └─ loop(st, cfg.MaxSteps): untuk tiap langkah (default 30):
     ├─ fitContext()         pangkas konteks kalau mulai penuh
     ├─ call(think, st)      satu permintaan ke model
     │   └─ pesan sistem dirakit ulang: prompt + modeNote + ledger
     ├─ kalau balasannya teks saja  → finishCheck(): benar-benar selesai?
     └─ kalau ada tool call         → runCalls()
         └─ untuk tiap panggilan:
             ├─ execTool()   validasi, izin, penjaga loop, jalankan
             ├─ noteResult() catat ke ledger + perbarui tanda kegagalan
             └─ escalate()   naikkan tangga kalau tembok yang sama berulang
```

`turnState` (`reason.go`) memegang semua keadaan yang hidup **satu giliran**:
langkah ke berapa, apa yang sudah diubah, sudahkah diverifikasi, catatan
percobaan, dan posisi di tangga eskalasi. Tidak ada yang bertahan lintas giliran
— itu disengaja, lihat bagian 4.

## 2. Penjaga yang sudah ada sebelumnya

| Penjaga | Letak | Menangkap |
|---|---|---|
| Perbaikan JSON otomatis | `repairJSON` (`repair.go`) | argumen tool yang nyaris valid |
| Tool call berbentuk teks | `parseTextToolCalls`, `tagCalls` | model yang menulis `<tool_call>` sebagai teks — format `hermes`, `qwen_xml`, `json_block` dari profil, plus `tag_json` (`<read_file> {json}`, `<todo>` berisi daftar bernomor) dan `bare_json` (baris `read_file {json}` telanjang — persis bentuk contoh di prompt sistem, yang ditiru model kecil secara harfiah), keduanya selalu dicoba terakhir apa pun profilnya |
| Nama/argumen tool salah | `execTool` → `invalid()` | dibalas koreksi *Problem / Why / Next step / Example* |
| Panggilan identik 3× | `callHash` + `st.recent` (`execTool`) | perintah yang sama persis diulang **selama belum ada perubahan** |
| Balasan kosong / terpotong | `finishCheck`, `st.lengthNudges` | model berhenti tanpa jawaban |
| Rencana belum tuntas | `todoReminder` | berhenti padahal `todo` masih terbuka |
| Gerbang verifikasi | `runVerifier`, `isVerifyCmd`, `st.verified()` | menyatakan selesai tanpa menjalankan apa pun — harness menjalankan sendiri perintah verifikasi proyek (§3.4); `verifyReminder` hanya kalau tidak ada yang bisa dijalankan |
| Centang `todo` milik harness | `auditTodos` (`tools.go`) | item dicentang `done` padahal perubahan sejak rencana terakhir belum diverifikasi |
| Osilasi isi file | `st.seenContent` (`execTool`) | file kembali ke isi yang pernah ada di giliran ini (A→B→A): langsung ke anak tangga diagnosis (§3.5) |
| Anggaran tanpa-kemajuan | `checkProgress`, `noProgress` | 6 langkah tanpa bukti baru — tidak ada file baru dibaca, perubahan, jenis kegagalan baru, atau verifikasi lulus (§3.5) |
| Baca sebelum edit kedua | `st.unread` (`execTool`) | `edit_file` kedua pada file yang sama tanpa `read_file` atau verifikasi di antaranya (§3.5) |
| Checkpoint konteks | `checkpoint`, `st.newWindow`, `st.handoffBlock` | item `todo` ditutup dengan perubahan terverifikasi → konteks giliran dipadatkan ke permintaan asli, faktanya menyeberang lewat handoff milik harness (§3.6) |
| Done-condition subtask | `subState.noteVerify`, `runSubtaskDone` | `subtask_done` ditolak sampai harness sendiri melihat perintah `verify` subtask itu exit 0 (§3.7) |
| Pra-terbang "merah dulu" | `preflightGreen` | perintah `verify` yang sudah lulus sebelum subtask dimulai → subtask dilewati, bukan diserahkan ke model yang tak bisa tahu kapan berhenti (§3.7) |
| Tool di luar cakupan | `execTool` (`out_of_scope:`) | `todo` di dalam subtask, `subtask_done` di luar `/task` — dibalas koreksi soal cakupan, bukan "tool tidak dikenal" |
| Aksi berisiko | `toolRisk` (`guard.go`) | selalu minta izin per panggilan, `--yolo` tidak menembusnya |
| Balasan identik berulang | `finishCheck` (`st.lastText`) | teks yang sama persis dua kali tanpa tool call → thinking dinyalakan + diminta *tool call saja*; tiga kali → giliran dihentikan dengan terang, bukan diterima sebagai jawaban |
| Gambar sekali saja | `staleImagesAsText` (`call`) | bagian gambar hanya dikirim pada request tepat setelah dilampirkan; sesudahnya jadi catatan teks (`read_file` melampirkan lagi). Dipertahankan karena murah dan hemat token; dugaan bahwa gambar mematikan tool call native **dibantah** log berikutnya (lihat `rencana.md`, log keempat) |
| Narasi tanpa aksi | `looksLikeIntent` (`finishCheck`) | balasan akhir yang hanya mengumumkan langkah ("mari saya perbaiki…", "let me…") tanpa tool call — ditegur sekali per giliran: lakukan sekarang, atau jawab tanpa mengumumkan |
| Berhenti di tengah pekerjaan | `reportUnfinished` (`RunTurn`) | giliran kehabisan `--max-steps` dengan perubahan: file disebut, verifier proyek dijalankan kalau belum ada yang memverifikasi, dan catatannya **dibawa ke giliran berikutnya** (`a.carry`) supaya model mulai dari kenyataan, bukan dari klaim |
| Gambar sampai ke model | `RunTurn` (cue teks), `runRead` (`ToolResult.Image`), `loop` | lampiran selalu disebut namanya di teks pesan; `read_file` pada file gambar **melampirkan** gambarnya (pesan `user` setelah batch tool), bukan menjawab "file biner"; path gambar yang diketik di REPL ikut dilampirkan |

Detail penting: `callHash(name, args, a.mutations)` menyertakan penghitung
mutasi. Artinya penjaga panggilan-identik **sengaja** hanya berlaku selama tidak
ada file yang berubah. Begitu ada perubahan, hitungannya dimulai lagi — kalau
tidak, pola "edit → uji → edit lagi" yang sah akan ikut tertuduh.

Justru karena itu pola paling khas model kecil dulu lolos semua penjaga:
**edit → gagal → edit sedikit lagi → gagal**, lima kali, dengan hash yang selalu
berbeda. Bagian 3 dibuat untuk itu.

## 3. Tiga mekanisme pencarian

Diagnosisnya: model kecil berputar karena tiga sebab yang berbeda, jadi
obatnya juga tiga, bukan satu penjaga yang lebih galak.

### 3.1 Catatan percobaan — melawan "lupa"

*Sebab:* percobaan yang gagal hilang dari konteks. `fitContext` meringkas output
tool lama menjadi `[old output removed: ...]`, dan yang terhapus justru bukti
kenapa percobaan kedua gagal. Model lalu mengajukan percobaan kedua itu lagi
sebagai "ide baru".

*Obat:* `turnState.noteResult` mencatat tiap panggilan dan hasilnya secara
ringkas; `turnState.ledger()` merendernya; `Agent.call` menempelkannya ke
**pesan sistem**:

```
[already attempted this turn - these results are final, ...]
1. bash npm test → FAILED: cannot find module <path>
2. read_file src/auth.js → baris 1-20 dari 20
3. bash node src/app.js → FAILED: cannot find module <path> (same failure as #1)
The same failure has now survived 2 attempts, so the idea behind them is what is
wrong, not the details.
```

Kenapa di pesan sistem: `call()` membuat **klon** `a.msgs` dan menambahkan blok
ini ke `msgs[0]` setiap permintaan. Pesan sistem dirakit ulang tiap kali, jadi
pemangkasan konteks secara struktural tidak bisa memakannya. Ini juga alasan
`modeNote` memakai jalur yang sama.

Batas: 10 entri (`maxLedger`), tiap baris dipangkas — sekitar 150-200 token.

### 3.2 Tanda kegagalan — melawan "ide sama, baju baru"

*Sebab:* penjaga lama membandingkan **panggilan**. Lima perintah berbeda yang
menabrak asumsi salah yang sama terlihat seperti lima ide berbeda.

*Obat:* `failSignature(out)` (`reason.go`) meringkas output gagal menjadi tanda:

1. ambil baris pertama yang benar-benar berisi error (lewati `exit_code:` dan
   baris `[harness]`), jatuh ke baris pertama kalau tidak ada kata kunci error;
2. ganti token yang mengandung `/` atau berbentuk nama file menjadi `<path>` —
   **token demi token**, sehingga nama dalam kutip tetap utuh, karena
   `cannot find module 'auth'` justru berbeda makna dari `'db'`;
3. angka dan heksadesimal menjadi `<n>`; sisanya dikecilkan dan dipangkas 80
   karakter.

Hasilnya: `cat a.txt` dan `cat ./b.txt` yang sama-sama *no such file* terhitung
**satu tembok**, sedangkan error yang benar-benar lain tetap terpisah.

Dua keputusan yang mudah terlewat kalau kode ini diubah:

- **`loop:<tool>` dilipat ke tanda yang sedang berlaku.** Penjaga panggilan
  identik menghasilkan `ErrKey = "loop:bash"`. Kalau itu diperlakukan sebagai
  tanda baru, rentetan ter-reset dan tangga eskalasi turun lagi ke anak tangga
  pertama — model terjebak di rung 2 selamanya. Mengulang persis adalah tembok
  yang sama diteruskan, jadi ia **menaikkan** tangga.
- **Hanya verifikasi yang lulus menghapus rentetan.** `read_file` yang berhasil
  di antara dua kegagalan identik bukan kemajuan. Yang mengosongkan rentetan
  adalah `res.Verify` (perintah bash yang dikenali sebagai verifikasi, lihat
  `reVerifyCmd` di `tools.go`) yang tidak gagal.

Kegagalan tanpa pesan sama sekali diberi tanda `unclear failure in <tool>`,
supaya dua kegagalan sunyi dari tool berbeda tidak dianggap tembok yang sama.

### 3.3 Tangga eskalasi — melawan "koreksi yang membosankan"

*Sebab:* dulu setiap pengulangan dibalas kalimat sejenis, lalu giliran dimatikan
di hitungan ketiga. Bagi model kecil itu dua-duanya buruk: koreksinya tidak
mengubah distribusi keluaran, dan berhentinya terlalu cepat untuk sebuah
pencarian yang sah.

*Obat:* `turnState.escalate(canAsk)` mengembalikan koreksi yang **berbeda
bentuknya** di tiap anak tangga, dan hanya sekali per tingkat (`st.rungDone`):

| Rentetan | Yang diminta |
|---|---|
| 2× | Tulis tiga baris: yang diharapkan, yang terjadi, asumsi mana yang terbukti salah. Lalu **satu** hipotesis baru. Thinking dinyalakan (`st.needThink`). |
| 3× | Wajib ganti jenis langkah (`switchAdvice`): habis mengedit → baca file dan titik errornya; habis menjalankan perintah → buka file yang disebut error; habis membaca → reproduksi dengan bash. |
| 4× | Wajib `ask_user` dengan 2-4 opsi konkret. Dilewati kalau tidak ada cara bertanya (`a.askLine == nil`, mis. mode `-p`). |
| ≥5× | Giliran dihentikan; model diminta merangkum apa yang dicoba dan apa yang dibutuhkan agar bisa lanjut. |

**Kegagalan protokol tidak masuk tangga ini.** `bad_json:`, `bad_args:`,
`unknown_tool:`, `plan_mode` (lihat `isProtocolFailure`) adalah model gagal
*bicara*, bukan gagal *berpikir*. Menyuruhnya "mendiagnosis idenya" tidak
menolong, dan tanda tangan tool beserta contohnya sudah dikirim pada koreksi
pertama. Jadi jalurnya tetap seperti dulu: satu kesempatan lagi, lalu berhenti
di hitungan ketiga.

Saat sebuah anak tangga dikirim, sisa panggilan dalam batch yang sama
dibatalkan ("Skipped: the harness interrupted this batch") supaya model
benar-benar bereaksi terhadap koreksinya, bukan menyelesaikan rencana lama.

### 3.4 Gerbang verifikasi — selesai ditentukan exit code, bukan klaim

*Sebab:* dulu "sudah diverifikasi" berarti dua hal yang sama-sama lemah: sebuah
pengingat yang hanya dikirim sekali, dan regex `reVerifyCmd` yang menganggap
`node x.js`, `python3 y`, `npx z`, bahkan `make` telanjang sebagai verifikasi.
Model kecil memuaskan pengingat itu dengan perintah yang tidak membuktikan apa
pun, lalu menyatakan selesai.

*Obat:* tiga bagian yang saling mengunci.

1. **`isVerifyCmd` (`reason.go`)** memutuskan apa yang dihitung: segmen perintah
   (dipecah pada `&&`, `||`, `;`, `|`) yang berawalan salah satu
   `env.VerifyCmds`, atau berbentuk cek eksplisit (`reCheckForm`: `go test`,
   `npm test`, `pytest`, `cargo test`, `make test`, `test -f`, `grep -q`,
   `diff`, `git diff --exit-code`, …). Menjalankan program bukan verifikasi.
   `TestIsVerifyCmd` adalah tabel yang mengunci kedua sisinya.
2. **`runVerifier` (`agent.go`)**: saat model menyatakan selesai dengan mutasi
   non-prosa yang belum diverifikasi, harness **menjalankan sendiri** perintah
   verifikasi proyek — `pickVerifier` memilih yang mengandung `test` lebih dulu,
   karena `go build` yang lulus membuktikan lebih sedikit daripada `go test`.
   Perintah itu lewat gerbang izin yang sama dengan bash dari model
   (`a.allowed`, `commandRisk`), jadi model izin tidak berubah. Exit 0 →
   jawaban diterima. Gagal → outputnya kembali ke model sebagai bukti lewat
   `correction()`, masuk `noteResult` sehingga ledger dan tangga bekerja seperti
   pada kegagalan lain; kalau terus gagal, tangga yang menghentikan giliran.
   Kalau user menolak izin atau tidak ada `VerifyCmds`, jalur lama
   (`verifyReminder`) tetap ada.
3. **`auditTodos` (`tools.go`)**: model tetap memiliki daftar `todo`, harness
   memiliki centangnya. Item hanya boleh berpindah ke `done` kalau tidak ada
   mutasi sejak rencana terakhir diperbarui (langkah membaca) atau mutasinya
   sudah diverifikasi (`st.verified()`); selain itu diturunkan ke `in_progress`
   dengan koreksi. Daftar yang menyusut diumumkan di layar — rencana yang
   ditulis ulang agar cocok dengan yang terlanjur dikerjakan adalah persis
   kegagalan yang ingin ditangkap. Tool membaca giliran lewat `a.turn`, yang
   diisi `RunTurn` selama giliran berjalan.

Kenapa hasil verifikasi harness dikirim sebagai pesan `user`, bukan `tool`:
tidak ada tool call model yang mendahuluinya, dan endpoint OpenAI-compatible
menolak pesan `tool` yatim. Kalau tangga eskalasi ikut menyala, pesannya
digabung ke satu pesan yang sama, bukan dua pesan `user` berturut-turut.

### 3.5 Sinyal kemajuan — membedakan mencari dari berputar

Tangga eskalasi (§3.3) hanya melihat **kegagalan**. Model kecil juga bisa
berputar tanpa satu pun kegagalan: membaca file yang sama, menjalankan perintah
yang tidak membuktikan apa pun, mengedit lalu membatalkan editannya sendiri.
Tiga penjaga ini melihat *ketiadaan kemajuan*, bukan kehadiran error. Semua
keadaannya ada di `turnState` dan diisi dari `execTool`.

**Bukti** (`st.evidence`) adalah salah satu dari empat hal: file dibaca untuk
pertama kali di giliran ini (`st.noteRead`, per giliran — bukan `a.readFiles`
yang bertahan lintas giliran dan `/clear`), sebuah mutasi, jenis kegagalan baru
(`noteResult` saat tanda berubah), atau verifikasi yang lulus
(`st.noteVerified`). Perintah yang sukses tapi bukan verifikasi — `ls`, `echo`,
`node x.js` — sengaja **bukan** bukti.

1. **Osilasi isi file.** Sebelum `edit_file`/`write_file`, `st.seenContent`
   mencatat sha isi file saat itu; sesudahnya ia mengecek isi barunya. Kalau isi
   baru sama dengan isi yang pernah ada di giliran ini, model telah membatalkan
   pekerjaannya sendiri. Hasil edit ditandai gagal dengan tanda
   `oscillation: <path> …`, dan `noteResult` memulainya langsung di rentetan 2:
   anak tangga diagnosis menyala **seketika**, karena A→B→A sudah bukti bahwa
   ide di balik kedua versi yang salah, bukan sekadar satu percobaan lagi.
   Pengulangan pada file yang sama menaiki tangga seperti biasa.
2. **Anggaran tanpa-kemajuan.** `checkProgress` (`RunTurn`, setelah `runCalls`)
   mengukur jendela dalam langkah model: `noProgressSteps` (6) langkah tanpa
   bukti → `noProgress` mengirim koreksi yang mengubah bentuk tugas
   (1× sebutkan apa yang kurang dan ambil dalam satu panggilan; 2× wajib
   `ask_user`; 3× giliran dihentikan). Ini tangga mini tersendiri, **bukan**
   menumpang tangga kegagalan — menumpang berarti me-reset rentetan kegagalan
   yang sedang berjalan, dan itu justru mematikan §3.3.
3. **Baca sebelum edit kedua.** Setelah `edit_file` **berhasil**, path-nya masuk
   `st.unread`; `edit_file` berikutnya pada path yang sama ditolak
   (`edit_unread:<path>`) sampai ada `read_file` path itu atau verifikasi lulus.
   Alasannya: setelah satu edit, salinan file di konteks model sudah basi, dan
   mengedit salinan basi adalah sumber `old_string` tidak cocok dan kode ganda.
   Aturan ini sengaja **tidak** berlaku setelah edit yang *gagal*: kegagalan
   `edit_nomatch` sudah mengembalikan potongan file yang paling mirip, jadi
   percobaan ulang dengan `old_string` yang dikoreksi adalah langkah yang sah dan
   memaksa `read_file` di situ hanya membuang satu langkah.

### 3.6 Checkpoint konteks — dekomposisi tanpa planner

*Sebab:* model kecil punya kapasitas konteks yang kecil, dan satu giliran
panjang mengisinya dengan transkrip: isi file yang dibaca, output test, diff.
`fitContext` memangkas dari tengah begitu penuh — dan yang terpangkas acak.
Ledger (§3.1) menambal gejalanya; sebabnya adalah konteks yang tumbuh tanpa
batas selama tugasnya belum selesai.

*Obat:* memotong giliran menjadi beberapa **jendela konteks** pada batas yang
bisa dipercaya — bukan batas yang dipilih model, melainkan **milestone yang
diverifikasi harness**.

1. **Pemicu.** `auditTodos` (§3.4) sudah tahu kapan sebuah item `todo` berpindah
   ke `done` dengan mutasi yang lulus verifikasi; item-item itu dicatat ke
   `st.milestone`. Langkah membaca yang ditutup tanpa mutasi **bukan**
   milestone: memadatkan di situ justru membuang apa yang baru saja dibaca.
2. **Pemadatan** (`Agent.checkpoint`, dipanggil `RunTurn` setelah batch tool
   selesai — tidak di dalam tool, supaya pesan tool terakhir tidak jadi yatim).
   `a.msgs` dipotong ke `a.msgs[:turnStart+1]`: pesan sistem, giliran-giliran
   sebelumnya utuh, dan permintaan asli giliran ini. Transkrip jendela lama
   hilang seluruhnya.
3. **Handoff** (`handoff`, `st.handoffBlock`) adalah satu-satunya yang
   menyeberang, dan **dicatat harness, bukan model**: item yang ditutup, file
   yang berubah (`st.mutated`), perintah verifikasi yang lulus
   (`st.verifyCmd`), path yang pernah dibaca giliran ini (`st.readSeen`,
   path saja — isinya sengaja tidak ikut), plus satu kalimat terakhir model
   sebelum menutup (`clip(…, 160)`), yang jelas ditandai sebagai catatan model.
   Rider ini menumpang klon pesan sistem seperti ledger dan `modeNote`, ikut
   memuat rencana `todo` terkini, dan berakhir dengan perintah melanjutkan dari
   langkah terbuka berikutnya.
4. **Jendela baru** (`st.newWindow`) mengosongkan semua keadaan yang mengindeks
   bukti di konteks lama — ledger, rentetan, hash isi file, set baca, `unread`
   — dan menyalakan thinking. Yang bertahan hanya `step` (anggaran
   `--max-steps` tetap per giliran), `handoffs`, `readSeen`, dan pengingat
   tingkat giliran (`planAsked`, `lengthNudges`, `emptyNudged`).

Ini mengambil manfaat inti dekomposisi tugas — konteks yang tidak pernah
melewati satu milestone — **tanpa** planner, `/task`, tool baru, atau loop
kedua. Rencananya tetap milik model (`todo`), tapi batas jendelanya milik
harness, karena satu-satunya batas yang boleh dipercaya adalah yang dibuktikan
exit code.

Yang sengaja **tidak** dilakukan: memadatkan saat verifikasi lulus tanpa `todo`
(tidak ada batas semantik yang bisa dipercaya di situ), dan membawa ringkasan
percobaan gagal ke jendela baru (lihat invarian #3).

### 3.7 `/task` — dekomposisi dengan planner, versi ramping

*Sebab:* checkpoint (§3.6) memotong konteks pada milestone yang **model** pilih
lewat `todo`. Untuk tugas yang benar-benar besar, batas itu masih terlalu
longgar: satu item `todo` bisa sebesar apa pun, dan rencananya bisa ditulis
ulang. `/task` memindahkan dua hal ke harness: **bentuk** tugasnya (dipecah
oleh planner menjadi subtask yang muat di kepala model kecil) dan **batas**
tiap subtask (satu perintah `verify` yang exit code-nya menentukan selesai).

*Bentuknya* (`plan.go`, `planner.go`, `subtask.go`):

1. **Pemicu deterministik.** `/task <permintaan>` di REPL, `--task` untuk `-p`.
   Tidak ada heuristik pada pesan biasa; pesan biasa tidak pernah membayar
   pajak planner. Saat giliran biasa kehabisan `--max-steps`, `RunTurn` hanya
   *menawarkan* `/task` — harness reaktif baru saja membuktikan tugasnya
   terlalu besar, dan itu satu-satunya pemicu yang jujur.
2. **Planner** (`callPlanner`): satu panggilan tanpa key `tools`, thinking
   dipaksa nyala, profil dipilih sendiri untuk `LCHAT_PLANNER_MODEL` (kosong =
   model utama, jadi default tidak menambah biaya). Balasannya JSON
   `{mode, subtasks}`, diparsing `repairJSON` yang sudah bertest. `mode:
   "direct"` berarti permintaan ini satu langkah → `RunTurn` biasa, sehingga
   salah-picu hanya memakan satu panggilan. `validatePlan` menolak bentuk
   terburuk sebelum memakan biaya: tanpa `verify`, verifier yang hanya mencetak
   (`cat`, `ls`), yang tak bisa gagal (`|| true`), atau yang bukan cek menurut
   `isVerifyCmd` (§3.4) — dengan satu tambahan khusus rencana: menjalankan
   skrip yang dihasilkan rencana itu sendiri (`bash tools/check.sh`,
   `reScriptRun`) diterima, karena exit code-nya adalah kontrak skrip itu dan
   pra-terbang menjaminnya merah dulu. Satu koreksi, lalu jatuh ke `RunTurn` —
   planner buruk tidak pernah mematikan tool.
3. **Rencana milik harness** (`TaskPlan`). Model tidak bisa mengganti daftar;
   ia hanya bisa menutup subtask yang sedang berjalan. `todo` disembunyikan
   lewat `toolSchemas(task)`, dan di dalam subtask kata "todo" tidak muncul di
   mana pun — dua rencana memberi model kecil dua tempat untuk mengklaim
   kemajuan, dan yang miliknya adalah yang bisa disunting.
4. **Konteks bersih per subtask** (`runSubtask`): `a.msgs = {system +
   taskBlock, subtaskBlock}`, `turnState` baru, anggaran `SubSteps + 2 ×
   jumlah files`. `taskBlock` memuat permintaan asli kata per kata, subtask
   yang selesai (**dicatat harness**: judul, file yang berubah, perintah yang
   lulus, satu baris catatan model), dan jawaban `ask_user` — keputusan user
   adalah fakta tentang dunia, jadi ia menyeberang. Rider `subState.note`
   mengulang di tiap request satu fakta yang selalu dilupakan model kecil:
   apakah done-condition sudah lulus.
5. **Selesai = exit code yang dilihat harness.** Model menjalankan `verify`
   lewat `bash` seperti perintah lain (gerbang izin tidak berubah);
   `subState.noteVerify` hanya *mengamati*. Pencocokannya (`runsVerifier`):
   segmen-segmen verifier harus muncul berurutan dan bersambung di antara
   segmen perintah yang dijalankan — `&& echo ok` di belakang atau `cd x;` di
   depan tetap dihitung, sebagian segmen atau path yang berbeda tidak. Uji nyata
   pertama menunjukkan kenapa aturan ini tidak boleh lebih sempit: verifier
   majemuk yang tak pernah cocok membakar seluruh anggaran subtask. `subtask_done` ditolak selama
   belum lulus (`ErrKey: subtask_unverified` — menolak berulang menaiki
   tangga). Perintah lain yang kebetulan lulus tidak membuka kunci. Jawaban
   akhir tanpa menutup: kalau sudah lulus, harness menutupkannya (teksnya jadi
   ringkasan); kalau belum, satu pengingat, lalu teksnya menjadi alasan
   pengembalian (`BLOCKED: …`). Kalau verifier sudah lulus tapi model kehabisan
   anggaran atau dikembalikan tanpa menutup, harness menutupnya sendiri: fakta
   yang dilihat harness mengalahkan diamnya model. Dan tepat saat verifier lulus,
   output tool itu sendiri menyuruh menutup — rider sistem saja terbukti tidak
   cukup.
6. **Pra-terbang "merah dulu"** (`preflightGreen`): sebelum subtask mulai,
   harness menjalankan `verify`-nya. Sudah exit 0 → subtask dilewati — kecuali
   file yang disebut di `files` belum ada: hijau tanpa hasil berarti verifiernya
   yang bohong, dan subtask tetap dijalankan. Ini
   menutup mode gagal terbesar rancangan ini: planner kecil memilih verifier
   yang sudah hijau, lalu eksekutor tidak pernah bisa tahu kapan berhenti.
7. **Tangga di dalam subtask pendek**: 2× diagnosis (tangga §3.3), 3×
   dikembalikan ke orkestrator (`subBlocked`). Tidak ada `split_subtask`
   sebagai tool: meminta model kecil merencanakan adalah persis yang ia tidak
   bisa. Pemecahan hanya oleh harness lewat planner (`escalateSubtask`),
   dengan ledger subtask yang gagal sebagai bukti di konteks planner yang
   baru — pemakaian ulang ledger yang tidak melanggar invarian #3, karena
   konteks itu memang tugasnya membaca bukti kegagalan.
8. **Batas keras, tidak pernah senyap**: `maxSubtasks` 8, `maxSplits` 3,
   kedalaman split 1, plafon langkah `MaxSteps × 3`. **Dua subtask beruntun
   yang tidak muat → rencana dibuang** (`abandonPlan`) dan permintaan
   dikerjakan sebagai satu giliran biasa dengan catatan apa yang sudah
   selesai. Menggilas rencana yang salah adalah hasil terburuk yang tersedia;
   ia dibuat tidak terjangkau.
9. **Akhir yang bersih** (`finishTask`): verifikasi ulang subtask terakhir
   (menangkap subtask yang lulus dengan merusak subtask sebelumnya), laporan
   untuk user dari konteks `{system, goal, handoff}` tanpa tool, lalu `a.msgs`
   ditulis ulang menjadi satu giliran sintetis `{system, goal, jawaban}` —
   tanpa ini, pesan lanjutan user mendarat di transkrip subtask terakhir yang
   tidak tahu apa permintaan aslinya. Kalau panggilan laporan gagal, harness
   menyusun laporannya sendiri: ia memang tahu faktanya.

Yang **tidak** dibangun, dan alasannya: `split_subtask` sebagai tool (lihat 7),
`Tool.Scope` sebagai mekanisme (cukup filter di `toolSchemas`), tiga tangga
terpisah, mode kerja keempat, dan heuristik pemicu. Semuanya tercatat di
`rencana.md`.

## 4. Invarian yang jangan dilanggar

1. **Keadaan dinamis tidak ditulis ke `a.msgs`.** Ledger dan `modeNote` hidup di
   klon pesan sistem di dalam `call()`. Kalau dimasukkan ke riwayat, ia akan
   menumpuk tiap langkah dan sekaligus bisa dipangkas `fitContext`.
2. **`fitContext` tidak boleh menyentuh `msgs[0]` dan `msgs[1]`.** Prompt sistem
   dan pesan pertama user adalah jangkar giliran.
3. **`turnState` berumur satu jendela konteks.** Satu giliran biasa punya satu;
   setelah checkpoint (§3.6) jendela berikutnya mulai dari kosong lewat
   `st.newWindow`. Alasannya: ledger adalah **indeks ke bukti yang ada di
   konteks**. Kalau konteksnya diganti tapi ledgernya ikut, model membaca
   ringkasan percobaan yang buktinya sudah tidak bisa dibuka — terlihat seperti
   pengetahuan, padahal bukan, dan itu lebih buruk daripada tidak ada catatan.
   Yang boleh menyeberang batas jendela hanya **fakta terverifikasi** di
   `handoff`: apa yang berubah, verifikasi mana yang lulus, path apa yang
   pernah dibaca — semuanya dicatat harness — plus satu kalimat model yang
   ditandai sebagai catatan.
4. **Penjaga panggilan-identik harus tetap sadar mutasi.** Membuang `a.mutations`
   dari `callHash` akan menuduh siklus edit-uji yang sah sebagai loop.
5. **Anak tangga tidak boleh dikirim dua kali untuk rentetan yang sama**
   (`st.rungDone`), karena tangga ini hanya bekerja selama koreksinya terasa
   baru.
6. **Jangan menambah anak tangga yang isinya "coba lagi".** Setiap tingkat harus
   meminta *jenis langkah* yang berbeda, bukan usaha yang lebih keras.

## 5. Yang belum dikerjakan

Ada di satu tempat saja: [`rencana.md`](rencana.md). Dokumen ini menjelaskan
yang **sudah** ada dan kenapa; rencana empat tahap berikutnya — gerbang
verifikasi yang dijalankan harness, sinyal kemajuan, checkpoint konteks, lalu
`/task` + planner versi ramping — beserta ide-ide yang ditolak dan alasannya,
semuanya hidup di sana. Jangan menambah daftar rencana di sini lagi; dua
rencana yang tumpang tindih adalah masalah yang `rencana.md` dibuat untuk
menyelesaikannya.

## 5b. Melihat ke belakang: log sesi

Semua bagian di atas dirancang dari transkrip sesi nyata, dan transkrip yang
ditempel dari layar selalu terpotong. `SessionLog` (`log.go`) menulis satu
JSONL per sesi berisi keputusan harness — rider yang ditambahkan ke tiap
request, balasan, hasil tool beserta siapa yang menjalankannya, koreksi,
baris layar, rencana dan subtask `/task`. Bukan konteks penuh (itu terlalu
besar dan bisa direkonstruksi), bukan API key (ada di header, tidak dicatat).
Untuk menganalisis sebuah sesi: cari `harness` dan `tool` dengan
`failed:true`, hitung `request` per `turn`, dan baca `extra` untuk melihat apa
yang model diberi tahu sebelum ia mengulang.

## 6. Mengujinya tanpa model sungguhan

Seluruh harness bisa diuji tanpa endpoint LLM:

- `fakeLLM` (`agent_test.go`) memutar balasan SSE yang sudah diskenariokan dan
  merekam setiap request body, jadi isi pesan sistem (termasuk ledger) bisa
  diperiksa langsung.
- `newFakeAgent(t, replies...)` menyiapkan Agent lengkap dengan folder kerja
  sementara.
- Bagian murni (`failSignature`, `noteResult`, `ledger`, `escalate`) diuji
  sebagai fungsi biasa, tanpa jaringan.

Test yang menjaga bagian 3 — jangan dihapus tanpa mengganti penjagaannya:
`TestFailSignature`, `TestEscalationLadder`, `TestLedgerRemembersAttempts`,
`TestLedgerTravelsInEveryRequest`, `TestLadderChangesShapeOfTask` (yang terakhir
juga memastikan tiap koreksi berbeda kalimatnya), dan
`TestInvalidJSONStopsAfterThreeTries` untuk jalur protokol.

Test yang menjaga gerbang verifikasi (§3.4): `TestIsVerifyCmd`,
`TestHarnessRunsVerifier` (lulus → diterima; gagal → bukti + tangga; tanpa
`VerifyCmds` → pengingat lama), `TestTodoCheckmarksAreHarnessOwned`, ditambah
`TestVerifyReminder` dan `TestTodoReminderAndPlanLine` yang lama dan lulus tanpa
diubah.

Test yang menjaga sinyal kemajuan (§3.5): `TestOscillationIsCaught`,
`TestNoProgressBudget` (delapan langkah kosong → tepat satu peringatan; satu
bukti di tengah → tidak ada), `TestSecondEditNeedsRead`.

Test yang menjaga `/task` (§3.7, di `task_test.go`), termasuk empat yang lahir
dari uji nyata pertama (`TestRunsVerifierMatchesCompound`,
`TestValidatePlanAcceptsScriptRun`, `TestVerifierTouchFlagSkipsFileChecks`,
`TestSummaryUsesRelativePaths`):
`TestValidatePlanRejectsUncheckableSubtask`, `TestTaskSystemPromptSwapsTodoRule`
(gagal keras kalau `strings.Replace` diam-diam jadi no-op), `TestTaskToolScopes`,
`TestSubtaskRunEndToEnd` (planner tanpa tools; `subtask_done` ditolak sampai
verifier lulus dan perintah lain yang lulus tidak membuka kunci; subtask 2
mulai dari dua pesan yang hanya memuat handoff; akhirnya satu giliran sintetis
bersih), `TestPlannerUsesItsOwnModel`, `TestDirectVerdictSkipsDecomposition`,
`TestPlannerRetriesOnceThenFallsBack`, `TestVerifierMustBeRedFirst`,
`TestSubtaskBudgetTriggersSplit` (permintaan pemecahan membawa ledger),
`TestTaskDropsPlanAfterTwoBudgetFailures`. Refaktor `RunTurn` → `loop` dijaga
oleh seluruh test lama yang lulus tanpa diedit.

Test yang menjaga checkpoint (§3.6): `TestCheckpointLeavesCleanContext` —
setelah milestone terverifikasi, permintaan berikutnya hanya memuat pesan
sistem, giliran-giliran lama, dan permintaan asli; handoff di pesan sistem
memuat item, file, perintah verifikasi, catatan model, path yang pernah
dibaca, dan rencana terkini; ledger kosong; langkah baca yang ditutup tanpa
mutasi tidak memicu checkpoint kedua.
