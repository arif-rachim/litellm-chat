import base64, html, pathlib, re
import shot2png

BASE = pathlib.Path('/tmp/claude-1000/-home-developer-litellm-chat/b2a43d05-49fd-4e1e-8c05-518e60a84446/scratchpad')
LONG = str(BASE)
DROP = tuple("berpikir %ds" % i for i in range(60)) + ("menunggu model",)

def shot(src, title, cols=96):
    raw = (BASE / src).read_text(encoding='utf-8', errors='replace')
    raw = raw.replace(LONG + '/demo', '~/demo').replace(LONG + '/litellm', '~/litellm').replace(LONG, '~')
    out = BASE / 'guide' / (src.replace('/', '_') + '.png')
    shot2png.render(raw, title, str(out), cols=cols, drop=DROP)
    b64 = base64.b64encode(out.read_bytes()).decode()
    return '<img alt="%s" src="data:image/png;base64,%s">' % (html.escape(title), b64)

def step(n, title, body):
    return f'<section class="step"><h2><span class="num">{n}</span>{title}</h2>{body}</section>'

def code(text, lang=''):
    return '<pre class="cmd">%s</pre>' % html.escape(text.strip('\n'))

SNIP = {
  'C_BUILD': 'CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o lchat .\ninstall -Dm755 lchat ~/.local/bin/lchat   # opsional, agar bisa dipanggil dari mana saja',
  'C_ENV': "cd ~/demo\ncat > .env <<'EOF'\nOPENROUTER_API_KEY=sk-or-v1-...\nLCHAT_MODEL=qwen/qwen3.5-35b-a3b\nEOF\nchmod 600 .env\necho .env >> .gitignore",
  'C_PROXY': "# terminal 1\nexport OPENROUTER_API_KEY=sk-or-v1-...\nuvx --from 'litellm[proxy]' litellm --config config.yaml --port 4000\n\n# terminal 2 (atau taruh di .env)\nexport LCHAT_BASE_URL=http://localhost:4000\nexport LCHAT_API_KEY=sk-lchat-local      # sama dengan master_key di config.yaml\nexport LCHAT_MODEL=qwen3.5-35b-a3b        # sama dengan model_name di config.yaml",
  'C_LOCAL': '# vLLM\n  - model_name: qwen3.5-35b-a3b\n    litellm_params:\n      model: hosted_vllm/Qwen/Qwen3.5-35B-A3B\n      api_base: http://localhost:8000/v1\n\n# Ollama\n  - model_name: qwen3.5-35b-a3b\n    litellm_params:\n      model: ollama_chat/<tag-model>\n      api_base: http://localhost:11434',
  'C_CONFIG': 'lchat config          # dari terminal\n/config               # dari dalam sesi lchat',
  'C_PROBE': 'lchat probe -m qwen/qwen3.5-35b-a3b --save   # hasilnya ke ~/.config/lchat/models.json',
  'C_IMG': 'lchat -i screenshot.png -p "kenapa layoutnya rusak?"',
}

P = []
P.append(f'''
<header class="cover">
  <p class="kicker">panduan pemakaian</p>
  <h1>lchat</h1>
  <p class="tag">Agent coding mini untuk terminal, dijalankan dengan model Qwen lewat LiteLLM atau OpenRouter.</p>
  <ul class="facts">
    <li><b>Target</b> Ubuntu 24.04</li>
    <li><b>Hasil build</b> satu binary static ±7 MB, tanpa dependency</li>
    <li><b>Model uji</b> qwen/qwen3.5-35b-a3b</li>
  </ul>
  <p class="note">Semua tangkapan layar di panduan ini diambil dari sesi sungguhan di Ubuntu 24.04, bukan ilustrasi. Path folder dipendekkan jadi <code>~/demo</code>, dan key pada contoh sengaja dipalsukan.</p>
</header>''')

P.append(step(1, 'Build dan install', f'''
<p>Butuh Go 1.22 atau lebih baru (<code>sudo apt install golang-go</code>). Dari folder sumber:</p>
{code(SNIP['C_BUILD'])}
{shot('guide/01-build.raw', 'go build -o lchat .')}
<p>Hasilnya satu file yang berdiri sendiri. Tidak ada runtime atau library yang perlu dipasang di mesin tujuan.</p>
<p class="tip"><b>Tanpa Go?</b> Cukup salin binary <code>lchat</code> dari mesin lain dengan arsitektur sama; ia statically linked.</p>'''))

P.append(step(2, 'Lihat semua opsi', f'''
<p>Menjalankan <code>lchat</code> tanpa argumen langsung masuk mode interaktif. Untuk melihat daftar flag:</p>
{shot('guide/02-help.raw', 'lchat -h')}'''))

P.append(step(3, 'Cara termudah: wizard /config', f"""
<p>Alih-alih menyunting file, jalankan wizard-nya. Ia menanyakan endpoint, API key (diketik tersamar), lalu mengambil daftar model dari endpoint itu memakai key yang baru diisi, sehingga kamu tinggal memilih nomornya.</p>
{code(SNIP['C_CONFIG'])}
{shot('guide/10-config.raw', 'lchat config')}
<p>Daftar model menampilkan ukuran konteks, dukungan tool dan gambar, serta harga per 1 juta token. Hasil pilihan disimpan di <code>~/.config/lchat/config.json</code> dengan izin <code>600</code> dan langsung dipakai sesi yang sedang berjalan. Di akhir, wizard menawarkan menjalankan <code>probe</code>.</p>
<p class="tip">Urutan sumber konfigurasi, dari yang paling menang: flag command line, environment variable, file <code>.env</code>, lalu <code>config.json</code> dari wizard. Kalau ada yang menimpa hasil wizard, lchat memberi tahu saat menyimpan.</p>
"""))

P.append(step(4, 'Kalau ingin mengatur manual', '''
<p>Ada dua jalur, dan keduanya dikonfigurasi lewat file <code>.env</code> atau environment variable.</p>
<table>
  <tr><th>Jalur</th><th>Kapan dipakai</th><th>Yang perlu diisi</th></tr>
  <tr><td><b>A. OpenRouter langsung</b></td><td>paling cepat dimulai, tanpa server tambahan</td><td><code>OPENROUTER_API_KEY</code></td></tr>
  <tr><td><b>B. LiteLLM proxy</b></td><td>model lokal (vLLM/Ollama), banyak model di satu alamat, pencatatan biaya, kunci bersama satu tim</td><td><code>LCHAT_BASE_URL</code>, <code>LCHAT_API_KEY</code>, <code>LCHAT_MODEL</code></td></tr>
</table>
<p>Urutan pembacaan konfigurasi: environment variable lebih dulu, lalu <code>./.env</code> di folder kerja, lalu <code>.env</code> di samping binary <code>lchat</code>, lalu <code>~/.config/lchat/.env</code>. Isi <code>.env</code> tidak pernah diteruskan ke perintah yang dijalankan agent.</p>'''))

P.append(step('4A', 'Jalur A: OpenRouter langsung', f'''
<p>Ambil kunci di <b>openrouter.ai/settings/keys</b>, lalu simpan di <code>.env</code> pada folder proyek:</p>
{code(SNIP['C_ENV'])}
{shot('guide/03-env.raw', 'cat .env')}
<p>Kalau <code>OPENROUTER_API_KEY</code> ada dan <code>LCHAT_BASE_URL</code> tidak diisi, lchat otomatis memakai <code>https://openrouter.ai/api/v1</code>. Tidak ada langkah lain.</p>
<p class="warn"><b>Jaga kunci.</b> Beri izin <code>600</code> dan masukkan <code>.env</code> ke <code>.gitignore</code>. Kunci pada tangkapan layar di atas palsu.</p>'''))

P.append(step('4B', 'Jalur B: lewat LiteLLM proxy', f'''
<p>Buat <code>config.yaml</code>. Contoh berikut meneruskan ke OpenRouter; untuk model lokal ganti bagian <code>litellm_params</code> (lihat catatan di bawah).</p>
{shot('guide/06-litellm-config.raw', 'cat config.yaml')}
<p>Jalankan proxy-nya, lalu arahkan lchat ke sana:</p>
{code(SNIP['C_PROXY'])}
{shot('guide/08-litellm-chat.raw', 'lchat (lewat LiteLLM di localhost:4000)')}
<p><b>Untuk model lokal</b>, ganti isi <code>litellm_params</code>:</p>
{code(SNIP['C_LOCAL'])}
<p class="tip">Kalau memakai vLLM, jalankan servernya dengan tool parser dan reasoning parser untuk Qwen, agar tool call dan reasoning terpisah rapi. Kalau tidak, lchat tetap bisa membacanya dari teks jawaban, hanya kurang rapi.</p>'''))

P.append(step(5, 'Kalibrasi model dengan probe', f'''
<p>Setiap backend berbeda cara menyalakan dan mematikan mode berpikir. <code>lchat probe</code> mencobanya satu per satu, lalu menyimpan hasilnya sebagai profil.</p>
{code(SNIP['C_PROBE'])}
{shot('shots/d.raw', 'lchat probe (langsung ke OpenRouter)')}
<p>Hasil yang sama dijalankan lewat LiteLLM memberi kesimpulan berbeda, dan itulah gunanya probe:</p>
{shot('guide/07-probe-litellm.raw', 'lchat probe --save (lewat LiteLLM)')}
<p>Di kedua kasus, yang bekerja adalah parameter <code>reasoning</code>, sedangkan <code>chat_template_kwargs</code> diabaikan. Jalankan probe setiap kali ganti model atau ganti backend.</p>'''))

P.append(step(6, 'Sesi pertama', f'''
<p>Jalankan <code>lchat</code> di dalam folder proyek. Baris pertama menampilkan model, profil, dan mode yang aktif.</p>
{shot('guide/05-first.raw', 'lchat')}
<p>Perintah yang tersedia di dalam sesi:</p>
{shot('guide/04-help-repl.raw', 'lchat  →  /help')}'''))

P.append(step(7, 'Mode kerja dan tombol Tab', f'''
<p>Tekan <b>Tab</b> saat mengetik untuk berpindah mode: <code>plan → ask → auto</code>. Prompt ikut berubah.</p>
<table>
  <tr><th>Mode</th><th>write_file &amp; edit_file</th><th>bash</th></tr>
  <tr><td><code>plan</code></td><td>diblokir</td><td>minta izin</td></tr>
  <tr><td><code>ask</code> (bawaan)</td><td>minta izin</td><td>minta izin</td></tr>
  <tr><td><code>auto</code></td><td>langsung jalan</td><td>minta izin</td></tr>
</table>
{shot('shots/tab.raw', 'lchat  →  Tab, lalu model bertanya balik')}
<p>Di contoh itu model memakai <code>ask_user</code> untuk menanyakan format file, dan dijawab dengan angka <code>2</code>. Jawaban boleh berupa nomor atau kalimat bebas.</p>'''))

P.append(step(8, 'Plan mode: rencana dulu, baru kerja', f'''
<p>Di mode plan, semua perubahan file diblokir sampai kamu menyetujui rencananya. Persetujuan cukup satu tombol.</p>
{shot('shots/plan.raw', 'lchat --mode plan')}'''))

P.append(step(9, 'Mengerjakan tugas nyata', f'''
<p>Satu kalimat instruksi sudah cukup. Agent menjalankan test, membaca file, mengedit, lalu menjalankan test lagi untuk membuktikan hasilnya.</p>
{shot('shots/a.raw', 'lchat --yolo')}'''))

P.append(step(10, 'Mengirim gambar', f'''
<p>Empat cara: <b>Ctrl+V</b> (butuh <code>wl-clipboard</code> atau <code>xclip</code>), <code>/img &lt;path&gt;</code>, paste atau drag file ke terminal, dan flag <code>-i</code>.</p>
{code(SNIP['C_IMG'])}
{shot('shots/img.raw', 'lchat  →  Ctrl+V lalu /img')}
<p>Batas 5 MB per gambar. Saat konteks mulai penuh, gambar lama dibuang lebih dulu karena paling membebani.</p>'''))

P.append(step(11, 'Izin dan pengaman', f'''
<p>Tool yang mengubah sesuatu selalu minta izin: <code>[y/N/a]</code>, dengan <code>a</code> berarti selalu izinkan tool itu selama sesi.</p>
{shot('shots/c.raw', 'lchat (model mencoba membaca .env)')}
<p>Beberapa aksi ditanyakan <b>setiap kali</b>, bahkan dengan <code>--yolo</code>: file rahasia, akses di luar folder proyek, dan perintah berbahaya. Kalau tidak ada terminal untuk menjawab, aksinya diblokir.</p>
{shot('guide/09-guard.raw', 'lchat --yolo -p "hapus folder src pakai rm -rf"')}
<p class="warn">Ini jaring pengaman, bukan sandbox. Perintah berjalan dengan hak akses akunmu, jadi baca dulu apa yang kamu setujui, dan hindari <code>--yolo</code> di folder penting.</p>'''))

P.append(step(12, 'Rujukan singkat', '''
<table>
  <tr><th>Variabel</th><th>Bawaan</th><th>Keterangan</th></tr>
  <tr><td><code>OPENROUTER_API_KEY</code></td><td>—</td><td>kalau diisi dan base URL kosong, langsung ke OpenRouter</td></tr>
  <tr><td><code>LCHAT_BASE_URL</code></td><td><code>http://localhost:4000</code></td><td>alamat LiteLLM atau endpoint OpenAI-compatible lain</td></tr>
  <tr><td><code>LCHAT_API_KEY</code></td><td>—</td><td>master key LiteLLM</td></tr>
  <tr><td><code>LCHAT_MODEL</code></td><td><code>qwen3.5-35b-a3b</code></td><td>nama model atau alias di LiteLLM</td></tr>
  <tr><td><code>LCHAT_CTX</code></td><td>dari profil</td><td>batas konteks dalam token</td></tr>
  <tr><td><code>LCHAT_TOOL_MAX</code></td><td><code>8000</code></td><td>batas byte output tool yang dikirim ke model</td></tr>
  <tr><td><code>LCHAT_MODELS</code></td><td><code>~/.config/lchat/models.json</code></td><td>file profil model</td></tr>
</table>
<table>
  <tr><th>Flag</th><th>Keterangan</th></tr>
  <tr><td><code>-p "tugas"</code></td><td>sekali jalan; jawaban ke stdout, aktivitas ke stderr</td></tr>
  <tr><td><code>-i gambar.png</code></td><td>lampirkan gambar (boleh diulang)</td></tr>
  <tr><td><code>-m model</code></td><td>ganti model untuk sesi ini</td></tr>
  <tr><td><code>--mode plan|ask|auto</code></td><td>mode awal</td></tr>
  <tr><td><code>--yolo</code></td><td>lewati izin biasa (aksi berisiko tetap ditanya)</td></tr>
  <tr><td><code>--think auto|on|off</code></td><td>kapan model berpikir; bawaannya adaptif</td></tr>
  <tr><td><code>--quiet-think</code>, <code>-v</code>, <code>--raw</code></td><td>sembunyikan reasoning, tampilkan output tool penuh, markdown mentah</td></tr>
</table>
<p><b>Perintah di dalam sesi:</b> <code>/help</code>, <code>/mode</code>, <code>/img</code>, <code>/paste</code>, <code>/model</code> (pemilih: angka memilih, teks memfilter), <code>/profile</code>, <code>/think</code>, <code>/env</code>, <code>/clear</code>, <code>/exit</code>.<br>
<b>Tombol:</b> Tab melengkapi nama perintah saat baris diawali <code>/</code> dan mengganti mode selain itu, Ctrl+V tempel gambar, panah atas riwayat, Ctrl+W hapus kata, Ctrl+U hapus baris, Ctrl+C batalkan giliran (dua kali untuk keluar), Ctrl+D keluar.</p>'''))

P.append(step(13, 'Kalau ada masalah', '''
<table>
  <tr><th>Gejala</th><th>Sebabnya biasanya</th><th>Tindakan</th></tr>
  <tr><td><code>HTTP 401</code></td><td>key salah atau belum terbaca</td><td>cek <code>.env</code> ada di folder kerja, dan nama variabelnya tepat</td></tr>
  <tr><td><code>HTTP 400 ... model</code></td><td>nama model tidak dikenal backend</td><td>samakan <code>LCHAT_MODEL</code> dengan <code>model_name</code> di config LiteLLM</td></tr>
  <tr><td>Model berpikir terus, lambat</td><td>profil salah untuk backend ini</td><td><code>lchat probe --save</code>, atau <code>--think off</code></td></tr>
  <tr><td>Tool call muncul sebagai teks</td><td>parser tool di server belum aktif</td><td>tetap jalan otomatis; untuk vLLM aktifkan <code>--enable-auto-tool-choice</code></td></tr>
  <tr><td>Ctrl+V tidak melampirkan gambar</td><td>alat clipboard belum ada</td><td><code>sudo apt install wl-clipboard</code> atau pakai <code>/img</code></td></tr>
  <tr><td>Jawaban terpotong</td><td>batas token output</td><td>lchat otomatis meminta model melanjutkan</td></tr>
  <tr><td>Konteks penuh</td><td>output tool menumpuk</td><td>otomatis diringkas; atau <code>/clear</code> untuk mulai bersih</td></tr>
</table>'''))

STYLE = '''
@page { size: A4; margin: 16mm 14mm 14mm; }
:root { --ink:#16202a; --muted:#5b6b75; --line:#d5dee2; --accent:#0f6f6a; --chip:#eef4f5; }
* { box-sizing: border-box; }
body { margin:0; color:var(--ink); background:#fff;
       font-family:"Liberation Sans","DejaVu Sans",sans-serif; font-size:10.5pt; line-height:1.55; }
code, pre { font-family:"DejaVu Sans Mono",monospace; }
.cover { padding-bottom:10mm; border-bottom:2px solid var(--ink); margin-bottom:8mm; }
.kicker { margin:0 0 2mm; color:var(--accent); font-family:"DejaVu Sans Mono",monospace;
          font-size:9pt; letter-spacing:.18em; text-transform:uppercase; }
h1 { font-family:"DejaVu Sans Mono",monospace; font-size:34pt; margin:0 0 3mm; letter-spacing:-.02em; }
.tag { margin:0 0 5mm; font-size:12pt; color:var(--muted); max-width:150mm; }
.facts { list-style:none; margin:0 0 5mm; padding:0; display:flex; gap:6mm; flex-wrap:wrap; font-size:9.5pt; }
.facts li { background:var(--chip); border:1px solid var(--line); border-radius:3px; padding:1.5mm 3mm; }
.facts b { color:var(--accent); margin-right:1.5mm; }
.note, .tip, .warn { border-left:3px solid var(--accent); background:var(--chip);
                     padding:2.5mm 4mm; margin:4mm 0; font-size:9.5pt; }
.warn { border-color:#b4472f; background:#fdf1ee; }
.step { page-break-inside:avoid; margin-bottom:9mm; }
h2 { font-size:15pt; margin:0 0 3mm; display:flex; align-items:baseline; gap:3mm; }
.num { font-family:"DejaVu Sans Mono",monospace; font-size:10pt; color:#fff; background:var(--accent);
       border-radius:3px; padding:0.8mm 2.4mm; }
p { margin:0 0 3mm; max-width:170mm; }
pre.cmd { background:#f4f7f8; border:1px solid var(--line); border-left:3px solid var(--muted);
          padding:3mm 4mm; font-size:9pt; white-space:pre-wrap; margin:0 0 4mm; border-radius:2px; }
img { width:100%; border:1px solid var(--line); border-radius:4px; margin:1mm 0 4mm; display:block; }
table { border-collapse:collapse; width:100%; margin:0 0 4mm; font-size:9.5pt; }
th, td { border:1px solid var(--line); padding:2mm 3mm; text-align:left; vertical-align:top; }
th { background:var(--chip); font-weight:600; }
code { background:var(--chip); border:1px solid var(--line); border-radius:2px; padding:0 1mm; font-size:9pt; }
pre.cmd code { background:none; border:none; }
'''

doc = f'''<!doctype html><html lang="id"><head><meta charset="utf-8">
<title>Panduan lchat</title><style>{STYLE}</style></head><body>{''.join(P)}</body></html>'''
(BASE / 'panduan-lchat.html').write_text(doc)
print('html bytes', len(doc))
