"""Render a captured terminal session (raw ANSI) to a PNG that looks like a screenshot."""
import re, sys
from PIL import Image, ImageDraw, ImageFont

ESC = re.compile(r'\x1b\[([0-9;?]*)([A-Za-z])')
FG = {31: '#e8686d', 32: '#5cc274', 33: '#d9b65c', 34: '#6ea8f6', 35: '#c58af0', 36: '#4ec7d4', 37: '#d5dde2'}
BG, FGDEF, BAR, BARFG = '#0a0e11', '#d5dde2', '#11171b', '#8fa0a8'
SPINNER = tuple('⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏')
F = '/usr/share/fonts/truetype/dejavu/DejaVuSansMono%s.ttf'

def blend(hex_a, hex_b, t):
    a = tuple(int(hex_a[i:i+2], 16) for i in (1, 3, 5))
    b = tuple(int(hex_b[i:i+2], 16) for i in (1, 3, 5))
    return tuple(round(x + (y - x) * t) for x, y in zip(a, b))

def parse(raw, drop=()):
    """-> list of lines; each line is a list of (text, bold, dim, italic, color)."""
    lines, held = [], None
    for physical in raw.split('\n'):
        physical = physical.split('\r')[-1]
        if physical.startswith('\x1b[K'):   # a keystroke redraw: keep only the last state
            held = physical
            continue
        if held is not None:
            lines.append(held); held = None
        lines.append(physical)
    if held is not None:
        lines.append(held)

    out = []
    for line in lines:
        line = line.expandtabs(4)
        plain = ESC.sub('', line)
        if plain[:1] in SPINNER or any(d in plain for d in drop):
            continue
        if not plain.strip() and (not out or not out[-1]):
            continue
        bold = dim = italic = False
        color = None
        runs, pos = [], 0
        for m in ESC.finditer(line):
            chunk = line[pos:m.start()]
            pos = m.end()
            if chunk:
                runs.append((chunk, bold, dim, italic, color))
            if m.group(2) != 'm':
                continue
            for code in (m.group(1) or '0').split(';'):
                n = int(code or 0)
                if n == 0: bold = dim = italic = False; color = None
                elif n == 1: bold = True
                elif n == 2: dim = True
                elif n == 3: italic = True
                elif n in FG: color = FG[n]
        if line[pos:]:
            runs.append((line[pos:], bold, dim, italic, color))
        out.append(runs)
    while out and not any(t.strip() for t, *_ in out[-1]):
        out.pop()
    return out

def wrap(lines, cols):
    out = []
    for runs in lines:
        cur, width = [], 0
        for text, b, d, i, c in runs:
            while text:
                room = cols - width
                if room <= 0:
                    out.append(cur); cur, width, room = [], 0, cols
                piece, text = text[:room], text[room:]
                cur.append((piece, b, d, i, c))
                width += len(piece)
        out.append(cur)
    return out

def render(raw, title, path, cols=96, scale=2, drop=()):
    fonts = {(0,0): ImageFont.truetype(F % '', 15*scale), (1,0): ImageFont.truetype(F % '-Bold', 15*scale),
             (0,1): ImageFont.truetype(F % '-Oblique', 15*scale), (1,1): ImageFont.truetype(F % '-BoldOblique', 15*scale)}
    cw = fonts[(0,0)].getlength('M')
    lh = round(19 * scale)
    lines = wrap(parse(raw, drop), cols)
    pad, bar = 12 * scale, 30 * scale
    w = round(cw * cols) + pad * 2
    h = bar + pad * 2 + lh * max(len(lines), 1)
    img = Image.new('RGB', (w, h), BG)
    d = ImageDraw.Draw(img)
    d.rectangle([0, 0, w, bar], fill=BAR)
    d.line([0, bar, w, bar], fill='#1e272c')
    tf = fonts[(0,0)]
    d.text((pad, bar/2 - 8*scale), '$', font=tf, fill='#4ec7d4')
    d.text((pad + cw*2, bar/2 - 8*scale), title, font=tf, fill=BARFG)
    y = bar + pad
    for runs in lines:
        x = pad
        for text, b, dm, it, c in runs:
            f = fonts[(1 if b else 0, 1 if it else 0)]
            col = c or FGDEF
            fill = blend(BG, col, 0.62) if dm else col
            d.text((x, y), text, font=f, fill=fill)
            x += cw * len(text)
        y += lh
    img.save(path)
    return img.size

if __name__ == '__main__':
    raw = open(sys.argv[1], encoding='utf-8', errors='replace').read()
    print(render(raw, sys.argv[3], sys.argv[2]))
