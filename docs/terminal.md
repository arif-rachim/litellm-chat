# Editor baris dan pemilih model

Catatan teknis untuk siapa pun yang menyentuh `input.go` atau `wizard.go`.
Keduanya kelihatan sepele sampai dicoba di terminal sempit atau dengan endpoint
yang punya ratusan model — dua hal yang keduanya pernah jadi bug nyata di sini.

## 1. Kenapa ada editor baris sendiri

Terminal dalam mode *cooked* hanya menyerahkan baris utuh, jadi Tab, panah, dan
konfirmasi satu tombol tidak pernah sampai ke program. `Editor` (`input.go`)
masuk ke *raw mode* hanya selama membaca satu baris (`rawMode`/`restore`),
supaya Ctrl+C tetap menjadi SIGINT di luar itu.

## 2. Menggambar dihitung per baris, bukan per kolom

Versi lama menggambar ulang dengan `\r\x1b[K`. Masalahnya: `\r` kembali ke awal
**baris terminal yang sedang ditempati**, dan `\x1b[K` hanya menghapus baris itu.
Selama teks muat satu baris semuanya benar; begitu teks lebih lebar dari
terminal, kursor sudah berada di baris hasil wrap, sehingga setiap ketukan
tombol menggambar `prompt + seluruh isi baris` lagi mulai dari tengah. Yang
terlihat pengguna: kalimatnya tercetak berkali-kali.

Aturan sekarang (`Editor.render`, `Editor.endLine`):

1. `e.lastRow` menyimpan di baris ke berapa kursor berada di dalam blok yang
   digambar terakhir kali.
2. Naik `lastRow` baris, `\r`, lalu `\x1b[J` — hapus sampai akhir layar. Ini
   sekaligus membersihkan daftar saran di bawahnya, jadi tidak perlu penghapus
   terpisah.
3. Gambar prompt + isi, lalu daftar saran (masing-masing diawali `\n`).
4. Hitung posisi kursor dengan `advance()`, naik ke barisnya, `\r`, geser kanan.

Semua perpindahan bersifat relatif, jadi tetap benar saat layar ikut menggulung
di dasar terminal.

**Jebakan *pending wrap*.** Kalau teks pas memenuhi satu baris, terminal
memarkir kursor di kolom terakhir dan menunda wrap. `render` menormalkannya
dengan mencetak satu `\n` ketika kolom akhir sama dengan lebar layar; tanpa itu
perhitungan baris meleset satu.

Lebar layar dibaca lewat `ioctl(TIOCGWINSZ)` (`Editor.width`), jatuh ke 80 kalau
tidak ada terminal. Field `cols` hanya untuk test.

## 3. Lebar sel, bukan jumlah rune

Terminal memberi dua sel untuk ideograf CJK dan emoji, dan nol sel untuk tanda
gabung. Jadi aritmetika kursor memakai `runeWidth(r)` dan `advance()`:

- 0 sel: kontrol, `unicode.Mn`/`Me` (termasuk selektor variasi), ZWJ, ZWSP.
- 2 sel: rentang East Asian Wide/Fullwidth dan emoji di `wideRanges`.
- Selektor emoji `U+FE0F` menaikkan karakter sebelumnya menjadi 2 sel.

`advance()` juga menirukan wrap terminal: karakter lebar yang tidak muat lagi di
sisa baris pindah ke baris berikutnya dan meninggalkan satu sel kosong.

`wideRanges` **harus tetap urut** — pencarian di `runeWidth` berhenti lebih awal
begitu melewati rentang. `TestRuneWidth` menjaga itu.

Batas yang diketahui: rangkaian emoji ber-ZWJ (👨‍👩‍👧) dan bendera dua *regional
indicator* bisa meleset satu sel. Di situ emulator terminal sendiri saling
berbeda, jadi tidak ada jawaban yang benar untuk semuanya.

## 4. Melengkapi perintah `/`

`slashCmds` (`main.go`) adalah satu-satunya sumber untuk tiga hal sekaligus:
isi `/help`, kandidat Tab, dan daftar saran yang muncul sambil mengetik.
Menambah perintah cukup di satu tempat.

- `Editor.matches` hanya menganggap kata pertama yang diawali `/` sebagai nama
  perintah; begitu ada spasi, barisnya dianggap argumen.
- `Editor.complete` melengkapi ke prefiks terpanjang yang dimiliki semua
  kandidat, dan menambah spasi kalau perintahnya menerima argumen.
- Kalau baris bukan kata perintah, Tab kembali ke tugas lamanya: mengganti mode.
- Baris saran dipotong agar muat di lebar terminal; baris yang ikut ter-wrap
  akan merusak perhitungan di bagian 2.

## 5. Pemilih model: angka memilih, teks memfilter

`chooseModel` (`wizard.go`) dipakai bersama oleh wizard `/config` dan perintah
`/model`. Aturannya adalah keputusan produk, bukan kebetulan implementasi:

> **Hanya angka yang memilih. Teks apa pun hanya mempersempit daftar.**

Endpoint seperti OpenRouter mengembalikan ratusan model. Perilaku lama menerima
teks sebagai nama model, sehingga mengetik `qwen` langsung menyimpan model
bernama "qwen" — pilihan terjadi tanpa pengguna pernah melihat entri yang
terpilih. Konsekuensinya untuk perubahan berikutnya:

- Jangan menambahkan "pilih otomatis kalau hasil filter tinggal satu".
- Jangan menerima teks bebas sebagai id model tanpa penanda eksplisit. Jalan
  keluarnya sudah ada: awalan `=`, misalnya `=lokal/model-baru`.
- Angka yang bisa dipilih dibatasi pada entri yang **terlihat di halaman itu**
  (20 per halaman, `n`/`p` untuk pindah), supaya tidak ada yang memilih entri
  yang tidak tampak di layar.
- Enter kosong berarti batal dan model tidak diubah.

`/model qwen` membuka pemilih yang sudah terfilter, bukan mengganti model. Satu
pengecualian yang disengaja: id yang **persis** ada di daftar diterima langsung
(`/model qwen/qwen3-coder`), begitu juga ketika endpoint sama sekali tidak bisa
dihubungi — di situ nama dipakai apa adanya agar sesi offline tetap bisa jalan.

## 6. Cara mengujinya

`modes_test.go` berisi emulator terminal mini (`tinyTerm`) yang menerapkan
escape sequence ke grid karakter — termasuk lebar sel dan *pending wrap* — lalu
membandingkan **layar akhir**, bukan sekadar deretan byte. Itu satu-satunya cara
menangkap bug "teksnya tercetak berulang" secara otomatis.

Test yang menjaga bagian di atas: `TestLineEditorLongLineDoesNotRepeat`,
`TestLineEditorEditsWrappedLine`, `TestLineEditorWideRunes`,
`TestLineEditorCaretAfterWideRunes`, `TestRuneWidth`, `TestSlashCompletion`,
`TestChooseModelFiltersOnText`, `TestChooseModelPaging`.

Untuk memeriksa dengan mata di terminal sungguhan, jalankan binernya di dalam
pty dengan ukuran layar yang dipaksa kecil (`TIOCSWINSZ`) lalu render ulang
keluarannya — cara itu yang dipakai untuk memastikan wrap 40 kolom benar.
