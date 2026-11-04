#!/usr/bin/env python3
"""Day 29: turn the text a demo printed into a terminal-style GIF.

    make converge-demo > /tmp/out.txt        (any output; colour codes are stripped)
    scripts/render-demo-gif.py /tmp/out.txt docs/day-29-demo.gif

This is a REPLAY of real output, line by line, with pauses at each section. It is not a screen
recording: the timing between lines is chosen here, not taken from the run. Needs Pillow.
"""
import re, sys
from PIL import Image, ImageDraw, ImageFont

FONTS = ["/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf", "/usr/share/fonts/dejavu/DejaVuSansMono.ttf"]
BOLD = [f.replace("Mono.ttf", "Mono-Bold.ttf") for f in FONTS]
W, H, PAD, SIZE, LINE = 1000, 600, 18, 15, 21
BG, FG, DIM = (22, 24, 30), (214, 218, 228), (128, 134, 150)
GREEN, RED, CYAN, AMBER = (110, 220, 140), (255, 120, 120), (120, 200, 255), (250, 200, 100)

def font(paths):
    for p in paths:
        try:
            return ImageFont.truetype(p, SIZE)
        except OSError:
            pass
    return ImageFont.load_default()

REG, BLD = font(FONTS), font(BOLD)

def style(line):
    if line.startswith("== "):
        c = RED if "OFF" in line else GREEN if "WiFi ON" in line else CYAN
        return c, BLD
    if line.startswith("ok:"):
        return GREEN, REG
    if line.startswith("--- "):
        return AMBER, REG
    if "IDENTICAL" in line or line.startswith("Both replicas"):
        return GREEN, BLD
    if "differ" in line or "PARTITIONED" in line:
        return RED, REG
    if line.startswith("(") or line.startswith("state hash"):
        return DIM, REG
    return FG, REG

def frame(lines, title):
    im = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(im)
    d.rectangle([0, 0, W, 34], fill=(40, 43, 54))
    for i, c in enumerate([(255, 95, 86), (255, 189, 46), (39, 201, 63)]):
        d.ellipse([14 + i * 22, 11, 26 + i * 22, 23], fill=c)
    d.text((100, 9), title, font=REG, fill=DIM)
    rows = (H - 34 - PAD) // LINE
    y = 34 + PAD // 2
    for ln in lines[-rows:]:
        col, f = style(ln)
        d.text((PAD, y), ln[:112], font=f, fill=col)
        y += LINE
    return im

def main(src, dst):
    ansi = re.compile(r"\x1b\[[0-9;]*m")
    raw = [ansi.sub("", l.rstrip("\n")) for l in open(src)]
    # collapse runs of blank lines to one, and drop a leading blank
    lines = []
    for l in raw:
        if l == "" and (not lines or lines[-1] == ""):
            continue
        lines.append(l)
    title = "offgrid  make converge-demo   (replay of real output; simulated partition on localhost)"
    frames, durs, shown = [], [], []
    for i, l in enumerate(lines):
        shown.append(l)
        pause = 350
        if l.startswith("== "):
            pause = 1400
        elif l.startswith("--- "):
            pause = 900
        elif l.startswith("state hash"):
            pause = 600
        if i == len(lines) - 1:
            pause = 6000
        frames.append(frame(shown, title))
        durs.append(pause)
    pal = [f.convert("P", palette=Image.ADAPTIVE, colors=64) for f in frames]
    pal[0].save(dst, save_all=True, append_images=pal[1:], duration=durs, loop=0, optimize=True, disposal=1)
    print(f"{dst}: {len(frames)} frames, {sum(durs)/1000:.0f}s")

if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
