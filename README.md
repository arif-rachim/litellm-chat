# lchat

Agent coding mini untuk terminal, versi kecil dari Claude Code. Satu binary Go static (~7 MB, tanpa dependency). Model dipanggil lewat LiteLLM atau endpoint lain yang OpenAI-compatible. Dirancang untuk model kecil seperti Qwen3.5-35B-A3B.

Harness-nya yang membuat model kecil tetap terarah:

- **Kenal environment.** OS, tool yang terpasang, tipe proyek, `scripts` di `package.json`, dan pohon file dikirim di awal. Model tidak perlu buang langkah untuk eksplorasi.
- **Mengingatkan kalau model salah.**
  - JSON argumen yang rusak diperbaiki otomatis.
  - Tool call yang ditulis sebagai teks dikonversi jadi tool call beneran.
  - Nama tool atau argumen yang salah dibalas dengan koreksi berformat *Problem / Why / Next step / Example*.
  - Kalau `edit_file` tidak match, harness menunjukkan potongan file yang paling mirip.
  - Aksi yang diulang terus dideteksi sebagai loop.
  - Setelah 3 kesalahan yang sama berturut-turut, giliran dihentikan.
- **Reasoning.**
  - Thinking adaptif: nyala saat merencanakan dan setelah error, mati di langkah rutin.
  - Rencana kerja (`todo`) diingatkan di setiap langkah.
  - Setelah gagal, model diminta merefleksikan penyebabnya dulu.
  - Model wajib memverifikasi sebelum menyatakan selesai.
- **Bertanya balik.** Lewat tool `ask_user`, model bisa mengajukan pertanyaan pilihan ganda atau isian bebas saat keputusannya ada di tanganmu, alih-alih menebak.
- **Tiga mode kerja** yang bisa diganti dengan Tab: `plan`, `ask`, dan `auto`.
- **Gambar.** Screenshot bisa dikirim ke model lewat Ctrl+V, `/img`, atau paste path file.
- **Auto-check** setelah menulis atau mengedit file: `node --check`, validasi JSON, dan `py_compile`. `tsc` hanya jalan dengan `--check-ts`.
- **Profil model.** Semua hal yang spesifik ke model (cara switch thinking, sampling, format tool call, ukuran konteks) ada di profil. Kalau ganti model, jalankan `lchat probe`.

## Distribusi

Hasil build adalah **satu file yang berdiri sendiri**: statically linked, tanpa runtime, library, atau file pendamping. Cukup salin filenya ke mesin tujuan.

```bash
./release.sh v0.1.0      # atau: make release
```

Hasilnya di `dist/`:

| File | Ukuran | Untuk |
|---|---|---|
| `lchat-linux-amd64` | 7,2 MB (3,1 MB setelah gzip) | PC/server Intel atau AMD |
| `lchat-linux-arm64` | 6,7 MB (2,8 MB setelah gzip) | Raspberry Pi, server ARM |
| `SHA256SUMS` | | untuk memverifikasi unduhan |

Di mesin tujuan:

```bash
install -Dm755 lchat-linux-amd64 ~/.local/bin/lchat
lchat -version
lchat config        # atur endpoint, key, dan model
```

Yang perlu diingat saat membagikan: jangan ikut menyalin `.env` atau `~/.config/lchat/config.json`, karena keduanya berisi API key. Binary-nya sendiri tidak memuat key apa pun.

## Install

```bash
sudo apt install golang-go      # Go 1.22+ (atau pakai Go yang sudah ada)
make install                    # build lalu copy ke ~/.local/bin/lchat
```

Tanpa `make`:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o lchat . && install -Dm755 lchat ~/.local/bin/lchat
```

## Konfigurasi

Cara paling mudah: jalankan wizard-nya.

```bash
lchat config        # atau ketik /config di dalam sesi
```

Wizard menanyakan empat hal, berurutan:

1. **Endpoint**: OpenRouter langsung, atau LiteLLM / endpoint OpenAI-compatible lain (alamatnya diisi sendiri).
2. **API key**, diketik tersamar. Enter berarti pakai yang tersimpan.
3. **Model**: daftarnya diambil otomatis dari endpoint memakai key tadi, lengkap dengan ukuran konteks, dukungan tool dan gambar, serta harga per 1 juta token. Di pemilih ini **angka memilih, teks memfilter**: mengetik `qwen` hanya mempersempit daftar, yang benar-benar memilih adalah nomor yang diketik sesudahnya. `n`/`p` pindah halaman, `=id` memaksa id yang tidak ada di daftar, Enter kosong membatalkan.
4. **Probe**: opsional, untuk mengenali cara model itu menyalakan thinking, lalu menyimpannya sebagai profil.

Hasilnya disimpan di `~/.config/lchat/config.json` dengan izin `600`, dan langsung dipakai sesi yang sedang berjalan.

### Sumber konfigurasi dan urutannya

Dari yang paling menang: flag di command line, environment variable, file `.env`, lalu `config.json` dari wizard. Kalau ada yang menimpa hasil wizard, lchat memberi tahu saat menyimpan.

| Env | Default | Keterangan |
|---|---|---|
| `LCHAT_BASE_URL` | `http://localhost:4000` | LiteLLM proxy (atau endpoint OpenAI-compatible lain) |
| `LCHAT_API_KEY` | | master key LiteLLM / API key |
| `LCHAT_MODEL` | `qwen3.5-35b-a3b` | nama/alias model |
| `LCHAT_CTX` | dari profil | batas konteks (token) |
| `LCHAT_TOOL_MAX` | `8000` | batas byte output tool yang dikirim ke model |
| `LCHAT_TEMPERATURE` | dari profil | override temperature |
| `LCHAT_MODELS` | `~/.config/lchat/models.json` | file profil model |
| `LCHAT_CONFIG` | `~/.config/lchat/config.json` | file hasil `/config` |

**File `.env`.** Semua variabel di tabel ini, ditambah `OPENROUTER_API_KEY`, bisa ditaruh di `.env`. lchat mencarinya berurutan di folder kerja, di samping binary `lchat`, lalu di `~/.config/lchat/.env`. Variabel environment yang sudah di-set tetap menang. Isinya tidak diteruskan ke perintah yang dijalankan agent. Salin `.env.example` sebagai contoh. File `.env` sudah ada di `.gitignore`.

**Langsung ke OpenRouter tanpa LiteLLM.** Kalau `LCHAT_BASE_URL` dan `LCHAT_API_KEY` tidak di-set tapi `OPENROUTER_API_KEY` ada, lchat otomatis memakai `https://openrouter.ai/api/v1` dengan model default `qwen/qwen3.5-35b-a3b`.

Contoh config LiteLLM (vLLM, Ollama, OpenRouter) ada di `litellm.config.example.yaml`.

## Pemakaian

```bash
lchat                                   # mode interaktif
lchat -p "test-nya gagal, perbaiki"     # sekali jalan: jawaban ke stdout, aktivitas ke stderr
lchat --yolo -p "..."                   # tanpa konfirmasi izin (hati-hati)
lchat config                            # atur endpoint, key, dan model (dipandu)
lchat probe -m qwen3.8-27b --save       # deteksi perilaku model baru, simpan profilnya
```

Flag: `-m`, `-p`, `-i <gambar>`, `--yolo`, `--mode plan|ask|auto`, `--max-steps 30`, `--think auto|on|off`, `--quiet-think`, `--check-ts`, `--raw`, `-v`.

Perintah di REPL: `/config`, `/clear`, `/mode [nama]`, `/img <path>`, `/paste`, `/model [filter]`, `/profile`, `/think [auto|on|off]`, `/env`, `/help`, `/exit`. Ketik `/` lalu Tab untuk melengkapi nama perintah; daftar kandidatnya muncul sendiri sambil mengetik. `/model` membuka pemilih yang sama dengan wizard, dan `/model qwen` hanya memfilternya. Akhiri baris dengan `\` untuk input multi-baris.

### Mode kerja

Tekan **Tab** saat mengetik untuk berpindah `plan → ask → auto`; prompt ikut berubah warna dan label.

| Mode | write_file & edit_file | bash | Untuk apa |
|---|---|---|---|
| `plan` | diblokir | minta izin | model menyelidiki dulu, lalu menyodorkan rencana. Setujui dengan satu tombol `y`, dan mode otomatis pindah ke `auto` |
| `ask` (default) | minta izin | minta izin | kerja sehari-hari |
| `auto` | langsung jalan | minta izin | kalau kamu sudah percaya arah kerjanya |

Aksi berisiko (lihat bagian Keamanan) tetap ditanyakan di semua mode.

### Gambar

Empat cara melampirkan gambar ke pesan berikutnya:

```bash
lchat -i screenshot.png -p "kenapa layoutnya rusak?"   # sekali jalan
```

- **Ctrl+V** di dalam REPL: lchat membaca clipboard sendiri. Butuh `wl-clipboard` (Wayland) atau `xclip` (X11) terpasang: `sudo apt install wl-clipboard`. Kalau clipboard berisi teks, teksnya diketik ke baris; kalau berisi gambar, gambarnya dilampirkan.
- **`/img <path>`** — bisa beberapa path sekaligus.
- **Paste atau drag file** ke terminal: path gambar yang ada di baris otomatis dilampirkan dan dibuang dari teks pesan.

Yang perlu diingat: modelnya harus mendukung gambar (semua Qwen3.5 dan Qwen3-VL mendukung; kalau tidak, lchat memberi tahu saat server menolak). Batas per gambar 5 MB. Gambar paling membebani konteks, jadi saat konteks mulai penuh gambar lama dibuang lebih dulu dan diganti catatan.

### Tombol

Di terminal sungguhan, input memakai raw mode sendiri: panah kiri/kanan, panah atas/bawah untuk riwayat, Ctrl+A/E, Ctrl+U, Ctrl+W, Ctrl+V untuk gambar, dan Tab untuk ganti mode. Prompt izin cukup satu tombol `y`, `n`, atau `a` tanpa Enter. Ctrl+C membatalkan giliran yang sedang jalan; dua kali berturut-turut keluar; Ctrl+D juga keluar. Kalau input bukan terminal, semuanya otomatis turun ke mode baris biasa.

## Keamanan

lchat tidak memakai sandbox; perintah berjalan dengan hak akses akun kamu. Ada tiga lapis pengaman:

1. **Mode kerja.** Di `plan`, tool yang mengubah file diblokir sampai kamu menyetujui rencananya.
2. **Izin per tool.** `bash`, `write_file`, dan `edit_file` selalu minta izin. Pilihan `a` berarti selalu izinkan tool itu selama sesi. Dengan `--yolo`, izin ini dilewati.
3. **Konfirmasi wajib untuk aksi berisiko.** Aksi di bawah ini ditanyakan setiap kali, walaupun memakai `--yolo` atau sudah memilih `a`:
   - membaca, menulis, atau me-list di luar folder proyek, termasuk lewat symlink;
   - menyentuh file rahasia: `.env`, `*.pem`, `*.key`, `id_rsa`, `~/.ssh`, `~/.aws`, `.npmrc`, dan sejenisnya;
   - perintah seperti `rm -r`/`rm -f`, `sudo`, `git push`, `git reset --hard`, `curl … | sh`, `chmod -R`, dan `npm publish`.

   Kalau tidak ada terminal untuk bertanya, misalnya di mode `-p` yang di-pipe, aksi itu langsung diblokir.
4. **Kalau izin ditolak, giliran berhenti.** Model tidak mencoba cara lain sampai kamu memberi instruksi baru.

Key dari `.env` tidak diteruskan ke perintah yang dijalankan agent. Daftar di atas adalah jaring pengaman, bukan jaminan, karena perintah bisa disamarkan. Jadi baca dulu apa yang kamu setujui, dan jangan pakai `--yolo` di folder penting.

## Ganti model

Profil bawaan yang tersedia:

| Profil | Cocok untuk | Cara switch thinking |
|---|---|---|
| `qwen3*` | Qwen3, 3.5, 3.8, … | `chat_template_kwargs.enable_thinking` (vLLM/SGLang) |
| `qwen3*` + URL OpenRouter | Qwen lewat OpenRouter | `reasoning.enabled` |
| `*qwen3*instruct*` | varian instruct | tanpa thinking |
| `*thinking*` | varian thinking | selalu thinking |
| `*` | model lain | tanpa switch |

Untuk model atau backend lain, jalankan:

```bash
lchat probe -m <model> --save
```

Probe mencoba setiap cara switch thinking, mengecek apakah reasoning keluar sebagai field `reasoning_content` atau tag `<think>`, lalu mengetes tool call native. Hasilnya disimpan sebagai profil di `~/.config/lchat/models.json`. File itu bisa diedit manual (komentar `//` diperbolehkan):

```jsonc
{"profiles": [{
  "match": "qwen3.8*",                 // glob nama model
  "match_url": "*localhost:4000*",     // opsional: hanya untuk endpoint ini
  "thinking": {"control": "chat_template_kwargs", "kwarg": "enable_thinking", "history": "drop"},
  "sampling": {"think": {"temperature": 0.6, "top_p": 0.95}, "no_think": {"temperature": 0.7, "top_p": 0.8}},
  "tool_format": ["native", "hermes", "qwen_xml", "json_block"],
  "ctx": 65536
}]}
```

Nilai `control` yang tersedia: `body` (`on_body`/`off_body` digabung ke request), `chat_template_kwargs`, `reasoning_effort`, `prompt_switch` (`/think`, `/no_think`), `always`, dan `none`.

## Development

```bash
make test     # go vet + unit test (termasuk fake LLM server & probe)
make build
```
