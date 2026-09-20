# Catatan pengembangan

Hal-hal yang memakan waktu untuk ditemukan, ditulis supaya tidak ditemukan dua
kali.

## 1. Build: ada **dua** biner, jangan lupa yang kedua

```bash
VERSION=$(git describe --tags --always --dirty 2>/dev/null || date +%Y.%m.%d)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o lchat .
./release.sh          # regenerasi dist/: amd64 + arm64 + .gz + SHA256SUMS
```

- `./lchat` di akar proyek — yang dibangun `make build`.
- `dist/lchat-linux-amd64` — hasil `./release.sh`.

Keduanya pernah dipakai bergantian, dan pernah menghabiskan beberapa putaran
bolak-balik karena hanya satu yang dibangun ulang: perbaikannya sudah benar,
tapi yang dijalankan adalah biner lama di `dist/`. **Kalau ada laporan
"perbaikannya belum masuk", buktikan dulu biner mana yang jalan**, jangan
menebak:

```bash
for d in /proc/[0-9]*; do case "$(readlink $d/exe 2>/dev/null)" in
  *lchat*) echo "$d -> $(readlink $d/exe)";; esac; done
grep -ac "<potongan string versi lama>" lchat dist/lchat-linux-amd64
```

Ingat juga: proses yang sudah berjalan tetap memakai kode lama di memori
meskipun file binernya ditimpa. Sesi REPL yang terbuka sejak sebelum build harus
ditutup dan dijalankan ulang.

## 2. Toolchain

Go **tidak** ada di `PATH` secara default di mesin pengembangan ini, tapi
terpasang di `~/.local/go` (`~/.local/go/bin/go`, `gofmt`). Periksa `~/.local`,
`~/.local/bin`, `~/go`, dan `~/sdk` sebelum menyimpulkan sebuah tool tidak ada.

`make` belum terpasang dan `sudo` meminta password, jadi `make test`/`make build`
tidak bisa dipakai; gunakan perintah `go` langsung seperti di bagian 1.

Proyek ini **tanpa dependensi pihak ketiga** (`go.mod` hanya berisi nama modul
dan versi Go), jadi `go build` berhasil bahkan dengan `GOPROXY=off`.

## 3. Uji

```bash
gofmt -l .        # harus kosong
go vet ./...
go test ./...
```

Seluruh harness dan editor baris bisa diuji tanpa jaringan dan tanpa model: ada
`fakeLLM` untuk endpoint, `tinyTerm` untuk layar terminal, dan server HTTP
palsu untuk daftar model. Lihat `docs/harness.md` bagian 6 dan
`docs/terminal.md` bagian 6.

## 4. Apa yang disimpan ke disk, dan apa yang tidak

Pertanyaan yang sering muncul: "sesinya disimpan relatif terhadap folder kerja,
kan?" **Tidak.** Percakapan tidak pernah dimuat ulang, dan tidak ada yang
disimpan di folder proyek. Yang ada adalah **log sesi** untuk dianalisis
belakangan — bukan untuk dilanjutkan.

Yang ditulis ke disk, semuanya global per pengguna:

| Berkas | Isi |
|---|---|
| `~/.config/lchat/config.json` (mode `0600`) | endpoint, API key, model |
| `~/.config/lchat/models.json` | profil model hasil `lchat probe` |
| `~/.local/state/lchat/sessions/<waktu>.jsonl` (dir `0700`, file `0600`) | log sesi: satu objek JSON per baris — `turn`, `request` (langkah, thinking, rider harness: ledger/handoff/mode), `reply` (teks, reasoning terpotong, tool call), `tool` (hasil, dipotong 4 KB, dengan `by`: model/harness/preflight/recheck), `harness` (koreksi yang dikirim), `screen` (tiap baris Info/Warn/Error/Harness), `plan`/`subtask`/`aux_request`/`aux_reply` untuk `/task`, `turn_end` (hasil, langkah, detik). `LCHAT_LOG=off` atau `--no-log` mematikannya, `LCHAT_LOG_DIR` memindahkannya, `/log` menampilkan path-nya. **Isinya memuat output tool** (isi file, output perintah) — jangan dibagikan mentah. |

Percakapan (`a.msgs`), catatan percobaan, `todo`, daftar file yang sudah dibaca,
dan izin "selalu izinkan" hidup **hanya di memori** dan hilang saat proses
keluar.

Yang relatif terhadap folder kerja adalah **cakupan kerjanya**, bukan
penyimpanan: `.env` dibaca dari folder kerja, blok `<env>` dideteksi dari sana,
dan guard menolak sentuhan file di luar folder itu (`pathRisk` di `guard.go`).

Kalau suatu saat sesi ingin bisa dilanjutkan per proyek (mis. `.lchat/session.json`
plus `/resume`), ada satu syarat yang tidak boleh dilewat: transcript bisa
memuat isi file dan output perintah, jadi berkasnya harus `0600` dan sebaiknya
otomatis masuk `.gitignore`.

## 5. Menjalankan di lingkungan terputus (airgap)

Bisa, dan memang sudah diarahkan ke sana: tidak ada telemetri, tidak ada cek
update, dan hanya ada dua panggilan jaringan — `POST /v1/chat/completions`
(`llm.go`) dan `GET /v1/models` (`config.go`) — keduanya ke endpoint yang
dikonfigurasi sendiri. Bawaannya sudah `http://localhost:4000`.

Yang perlu diperhatikan:

- Butuh server OpenAI-compatible lokal (LiteLLM, vLLM, Ollama, llama.cpp) dengan
  model yang **mendukung tool calling**; tanpa itu agennya tidak bisa membaca
  atau mengubah file.
- Jangan menyetel `OPENROUTER_API_KEY` di environment/.env: kalau LiteLLM belum
  dikonfigurasi, base URL otomatis dialihkan ke `openrouter.ai` (`baseConfig` di
  `main.go`) dan hasilnya hanya timeout yang membingungkan.
- Tidak ada opsi CA kustom atau `--insecure`; pakai `http://` di jaringan
  tertutup, atau pasang CA internal ke trust store OS (`SSL_CERT_FILE` /
  `SSL_CERT_DIR` dihormati Go).
- Permintaan chat belum punya timeout (`FetchModels` punya 30 detik), jadi
  alamat yang salah menggantung sampai Ctrl+C.
- Tool opsional (`git`, `node`, `tsc`, `wl-paste`/`xclip`) semuanya dijaga
  `exec.LookPath`; pasang sebelum masuk airgap kalau fiturnya diperlukan.

## 6. Peta berkas

| Berkas | Isi |
|---|---|
| `main.go` | flag, REPL, perintah `/`, konfigurasi berlapis |
| `agent.go` | siklus giliran, eksekusi tool, anggaran konteks |
| `reason.go` | `turnState`, catatan percobaan, tanda kegagalan, tangga eskalasi, sinyal kemajuan, handoff |
| `plan.go` | `/task`: tipe rencana, validasi verifier, renderer blok task/subtask |
| `planner.go` | `/task`: panggilan planner, parsing JSON, permintaan pemecahan |
| `subtask.go` | `/task`: orkestrator `RunTask`, eksekutor `runSubtask`, tool `subtask_done` |
| `tools.go` | definisi dan implementasi tool |
| `guard.go` | aksi berisiko dan file rahasia |
| `llm.go` | klien chat completions + streaming |
| `profile.go` | profil model, `lchat probe` |
| `repair.go` | perbaikan JSON dan tool call berbentuk teks |
| `input.go` | editor baris raw-mode |
| `wizard.go` | wizard `/config` dan pemilih model |
| `render.go` | UI terminal, markdown, spinner |
| `envinfo.go` | deteksi environment untuk blok `<env>` |
