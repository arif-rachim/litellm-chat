# Cara membangun ulang panduan PDF

`shot2png.py` merender rekaman terminal (raw ANSI, hasil `script -qfec ... /dev/null`)
menjadi PNG yang tampak seperti tangkapan layar. `mkguide.py` menyusun rekaman-rekaman
itu menjadi satu HTML, lalu HTML-nya dicetak ke PDF:

```bash
python3 mkguide.py
google-chrome --headless=new --no-pdf-header-footer \
  --print-to-pdf=panduan-lchat.pdf "file://$PWD/panduan-lchat.html"
```

Butuh: python3 dengan Pillow, Google Chrome, dan font DejaVu Sans Mono.
Path rekaman di `mkguide.py` masih menunjuk ke folder tempat rekaman dibuat;
sesuaikan konstanta `BASE` sebelum menjalankannya lagi.
