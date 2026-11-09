"""Pixel helpers for the race video: text sprites in the 12px font (48 px
at 1080p, the smallest type in the video), pop-in animation, skies,
mountains, roads, cars, confetti and transitions. Artwork is drawn at
480x270 in the Sweetie 16 palette and scaled 4x with nearest neighbour.

Text uses Fusion Pixel 12px proportional (zh_hans), SIL Open Font License
1.1, which is not in the repository: RACE_FONT names the TTF file, by
default fonts/fusion-pixel-12px-proportional-zh_hans.ttf next to this file
(see README.md)."""

import math
import os
import random
from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw, ImageFont

W, H, S = 480, 270, 4
HORIZON = 88

# Sweetie 16 palette.
P = dict(ink="#1a1c2c", plum="#5d275d", red="#b13e53", orange="#ef7d57", yellow="#ffcd75",
         lime="#a7f070", green="#38b764", teal="#257179", navy="#29366f", blue="#3b5dc9",
         sky="#41a6f6", cyan="#73eff7", white="#f4f4f4", grey="#94b0c2", slate="#566c86",
         dark="#333c57")

FONT_PATH = os.environ.get("RACE_FONT") or str(Path(__file__).resolve().parent / "fonts" /
                                               "fusion-pixel-12px-proportional-zh_hans.ttf")
F12 = ImageFont.truetype(FONT_PATH, 12)
FPS = 60

DAY = [(0, P["navy"]), (22, P["blue"]), (50, P["sky"]), (78, P["cyan"])]
DUSK = [(0, P["navy"]), (24, P["plum"]), (52, P["red"]), (74, P["orange"])]
NIGHT = [(0, P["ink"]), (40, P["navy"]), (76, P["dark"])]
SUNSET = [(0, P["ink"]), (30, P["navy"]), (58, P["plum"]), (80, P["red"]), (92, P["orange"])]

_tc = {}


def tx(s, colour, sc=1, outline=P["ink"], shadow=True):
    """A crisp pixel-font sprite with an outline and a drop shadow. Only the
    12px font exists here, so no caption can be smaller than 48 px."""
    key = (s, colour, sc, outline, shadow)
    if key in _tc:
        return _tc[key]
    l, t, r, b = F12.getbbox(s)
    w, h = r - l + 4, b - t + 5
    im = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    d = ImageDraw.Draw(im)
    d.fontmode = "1"
    ox, oy = 1 - l, 1 - t
    if shadow and outline:
        d.text((ox + 1, oy + 2), s, font=F12, fill=outline)
    if outline:
        for dx in (-1, 0, 1):
            for dy in (-1, 0, 1):
                d.text((ox + dx, oy + dy), s, font=F12, fill=outline)
    d.text((ox, oy), s, font=F12, fill=colour)
    if sc > 1:
        im = im.resize((w * sc, h * sc), Image.NEAREST)
    _tc[key] = im
    return im


def clamp(x, a=0.0, b=1.0):
    return max(a, min(b, x))


def back(x, k=3.0):
    x = clamp(x) - 1
    return 1 + x * x * ((k + 1) * x + k)


def popsc(target, p):
    """An integer scale that pops in with overshoot; 0 = hidden."""
    if p <= 0:
        return 0
    if p >= 1:
        return target
    return max(1, round(target * back(p)))


def smooth(x):
    x = clamp(x)
    return x * x * (3 - 2 * x)


def paste(img, sp, x, y, anchor="lt"):
    """Composites a sprite, cropped to the frame, at an anchor point."""
    if anchor[0] == "m":
        x -= sp.width // 2
    elif anchor[0] == "r":
        x -= sp.width
    if anchor[1] == "m":
        y -= sp.height // 2
    elif anchor[1] == "b":
        y -= sp.height
    x, y = int(x), int(y)
    l, t = max(0, -x), max(0, -y)
    r, b = min(sp.width, img.width - x), min(sp.height, img.height - y)
    if r <= l or b <= t:
        return
    if (l, t, r, b) != (0, 0, sp.width, sp.height):
        sp = sp.crop((l, t, r, b))
    img.alpha_composite(sp, (x + l, y + t))


SHRUNK = set()  # (caption, scale asked, scale drawn) of settled captions that had to drop a scale
# Caption visibility for the reading-time check: while REC holds a scene
# index, every caption drawn records the first and last video time NOW at
# which it was on screen, SEEN[(scene, caption)] = [first, last].
REC, NOW, SEEN = None, 0.0, {}


def seen(s):
    if REC is not None:
        span = SEEN.setdefault((REC, s), [NOW, NOW])
        span[0], span[1] = min(span[0], NOW), max(span[1], NOW)


def pop_text(img, s, colour, sc, p, x, y, anchor="mt", outline=P["ink"], maxw=W - 12):
    """Pops text in. A line too wide drops to a smaller integer scale, never
    below 1; a line too wide even at 1 is an error: cut words, not type."""
    k = popsc(sc, p)
    if not k:
        return
    sp = tx(s, colour, k, outline)
    while sp.width > maxw and k > 1:
        k -= 1
        sp = tx(s, colour, k, outline)
    if k < sc and p >= 1:
        SHRUNK.add((s, sc, k))
    if sp.width > maxw:
        raise ValueError("caption too wide: " + s)
    seen(s)
    paste(img, sp, x, y, anchor)


def shake(img, t, t0, amp=3, dur=0.25):
    if not 0 <= t - t0 < dur:
        return img
    rnd = random.Random(int(t * FPS))
    a = amp * (1 - (t - t0) / dur)
    dx, dy = round(rnd.uniform(-a, a)), round(rnd.uniform(-a, a))
    out = Image.new("RGBA", (W, H), P["ink"])
    out.paste(img, (dx, dy))
    return out


_sky = {}


def sky(img, bands, horizon=HORIZON):
    """Dithered sky bands, laid out for an 88 px horizon and scaled to fit."""
    key = (tuple(bands), horizon)
    if key not in _sky:
        im = Image.new("RGBA", (W, horizon), P["ink"])
        d = ImageDraw.Draw(im)
        bs = [(int(y * horizon / 88), c) for y, c in bands]
        edges = [y for y, _ in bs] + [horizon]
        for i, (y0, c) in enumerate(bs):
            y1 = edges[i + 1]
            nxt = bs[i + 1][1] if i + 1 < len(bs) else c
            for y in range(y0, y1):
                d.line((0, y, W, y), fill=c)
                if y >= y1 - 2 and nxt != c:
                    for x in range(y % 2, W, 2):
                        d.point((x, y), fill=nxt)
        _sky[key] = im
    img.paste(_sky[key], (0, 0))


def mountains(d, cam, k, base, amp, colour, horizon=HORIZON, snow=None, seed=0.0):
    for x in range(W):
        wx = x + cam * k
        h = base + amp * (math.sin(wx * 0.031 + seed) * 0.6 + math.sin(wx * 0.077 + 1.3 + seed) * 0.3
                          + math.sin(wx * 0.19 + 0.5) * 0.1)
        top = int(horizon - h)
        d.line((x, top, x, horizon), fill=colour)
        if snow and h > base + amp * 0.45:
            d.line((x, top, x, top + 2), fill=snow)


def stars(d, t, drift=0.0, n=70, seed=5, ymax=HORIZON):
    rnd = random.Random(seed)
    for i in range(n):
        x, y = rnd.uniform(0, W), rnd.uniform(0, ymax - 4)
        x = (x - t * drift * (0.5 + (i % 3) * 0.5)) % W
        if (int(t * 6) + i) % 9:
            d.point((x, y), fill=P["white"] if i % 2 else P["grey"])


def road_strip(d, y0, t, speed):
    d.rectangle((0, y0, W, y0 + 14), fill=P["dark"])
    off = int(t * speed) % 16
    for x in range(-16, W + 16, 16):
        d.rectangle((x - off + 4, y0 + 7, x - off + 9, y0 + 7), fill=P["slate"])
    for x in range(-16, W + 16, 8):
        c = P["red"] if ((x + 16) // 8) % 2 else P["white"]
        d.rectangle((x - off, y0 + 15, x - off + 7, y0 + 17), fill=c)


def rgb(h):
    h = h.lstrip("#")
    return tuple(int(h[i:i + 2], 16) for i in (0, 2, 4))

GLYPH = {
    "G": [".##", "#..", "#.#", "#.#", ".##"],
    "o": ["...", "...", "###", "#.#", "###"],
    "C": [".##", "#..", "#..", "#..", ".##"],
}

def _car40(body, dark, tag, phase=0):
    """A 40x18 side-view car facing right with its language on the door."""
    im = Image.new("RGBA", (40, 18), (0, 0, 0, 0))
    d = ImageDraw.Draw(im)
    d.polygon([(10, 6), (14, 1), (27, 1), (32, 6)], fill=body)         # cabin
    d.polygon([(15, 2), (26, 2), (29, 5), (13, 5)], fill=P["cyan"])     # window
    d.line((21, 2, 21, 5), fill=dark)
    d.rectangle((0, 1, 3, 3), fill=dark)                                 # wing
    d.line((2, 3, 2, 6), fill=dark)
    d.rectangle((1, 6, 37, 13), fill=body)                               # body
    d.rectangle((37, 8, 39, 13), fill=body)                              # nose
    d.line((1, 13, 39, 13), fill=dark)
    d.line((2, 7, 36, 7), fill=P["white"] if body != P["white"] else P["grey"])
    d.rectangle((38, 9, 39, 10), fill=P["yellow"])                       # light
    d.rectangle((1, 8, 1, 10), fill=P["red"])
    d.rectangle((16, 6, 26, 13), fill=P["white"])                        # door roundel
    d.rectangle((16, 6, 16, 6), fill=body)
    d.rectangle((26, 6, 26, 6), fill=body)
    gl = [GLYPH[c] for c in tag]
    gw = 4 * len(gl) - 1
    gx = 16 + (11 - gw + 1) // 2
    for g in gl:
        for y, row in enumerate(g):
            for x, c in enumerate(row):
                if c == "#":
                    d.point((gx + x, 7 + y), fill=P["ink"])
        gx += 4
    for wx in (5, 27):                                                   # wheels
        d.ellipse((wx, 11, wx + 7, 17), fill=P["ink"])
        hx, hy = [(wx + 3, 13), (wx + 4, 14), (wx + 3, 15), (wx + 2, 14)][phase % 4]
        d.point((hx, hy), fill=P["grey"])
        d.point((wx + 3, 14), fill=P["slate"])
    return im

def clouds(d, cam, t):
    rnd = random.Random(7)
    for _ in range(9):
        wx, y, s = rnd.uniform(0, 1400), rnd.uniform(8, 60), rnd.choice((1, 2, 3))
        x = (wx - cam * 0.05 - t * 3) % 700 - 100
        for dx, dy, r in ((0, 0, 4), (5, -2, 5), (11, 0, 4), (3, 2, 4), (8, 2, 4)):
            r = r * s // 2 + 1
            d.ellipse((x + dx * s - r, y + dy - r, x + dx * s + r, y + dy + r), fill=P["white"])

def puffs(d, x, y, t, speed):
    if speed <= 0:
        return
    for k in range(4):
        a = (t * 6 + k / 4) % 1
        px, py = x - 3 - a * 14, y - a * 5
        if a < 0.8:
            d.rectangle((px, py, px + 1 + (a > 0.4), py + 1), fill=P["grey"] if a < 0.5 else P["slate"])

def speed_lines(d, x, y0, t):
    rnd = random.Random(3)
    for k in range(6):
        ly = y0 + rnd.randint(2, 12)
        off = (t * 220 + rnd.uniform(0, 80)) % 80
        lx = x - 10 - off
        d.line((lx, ly, lx + rnd.randint(5, 10), ly), fill=P["white"])


_cars = {}


def car(body, dark, tag, phase=0, ref=False, scale=1):
    """The 40x18 style A car. A reference car (C QuickJS) gets a white
    outline and hatched flanks, so it never reads as one of the Go cars."""
    key = (body, dark, tag, phase % 4, ref, scale)
    if key in _cars:
        return _cars[key]
    im = _car40(body, dark, tag, phase)
    if ref:
        d = ImageDraw.Draw(im)
        d.line([(1, 6), (10, 6), (14, 1), (27, 1), (32, 6), (37, 6), (39, 8), (39, 13)], fill=P["white"])
        for x in (3, 8, 29, 34):
            d.line((x, 9, x + 3, 12), fill=P["slate"])
    if scale > 1:
        im = im.resize((40 * scale, 18 * scale), Image.NEAREST)
    _cars[key] = im
    return im


def confetti(d, fx, fy, dt, n=220, seed=11, spread=190):
    if dt < 0 or dt > 3.5:
        return
    rnd = random.Random(seed)
    cols = [P["yellow"], P["orange"], P["white"], P["cyan"], P["lime"], P["red"], P["sky"]]
    for _ in range(n):
        side = rnd.choice((-1, 1))
        vx = side * rnd.uniform(10, spread) + rnd.uniform(-30, 30)
        vy = -rnd.uniform(60, 200)
        x = fx + vx * dt + 4 * math.sin(dt * 9 + rnd.uniform(0, 6))
        y = fy + vy * dt + 0.5 * 150 * dt * dt
        c = rnd.choice(cols)
        s = rnd.choice((1, 2, 2))
        if int(dt * 12 + rnd.random() * 4) % 3 == 0:
            d.point((x, y), fill=c)
        else:
            d.rectangle((x, y, x + s, y + 2), fill=c)


B4 = (np.array([[0, 8, 2, 10], [12, 4, 14, 6], [3, 11, 1, 9], [15, 7, 13, 5]]) + 0.5) / 16
BAY = np.tile(B4, (H // 4 + 1, W // 4 + 1))[:H, :W]


def dissolve(a, b, p):
    """An ordered-dither dissolve from a to b."""
    x = np.asarray(a.convert("RGB"))
    y = np.asarray(b.convert("RGB"))
    return Image.fromarray(np.where((BAY < p)[..., None], y, x)).convert("RGBA")


def checker_band(d, y0, t, speed=20):
    """A scrolling chequered band, the race-flag border of the cards."""
    off = int(t * speed) % 8
    for x in range(-8, W + 8, 4):
        for yy in range(2):
            col = P["white"] if ((x // 4) + yy) % 2 else P["dark"]
            d.rectangle((x - off, y0 + yy * 4, x - off + 3, y0 + yy * 4 + 3), fill=col)


def plate(img, d, lines, p, cx=W // 2, cy=58):
    """The red result plate: lines of (text, colour, scale)."""
    sps = [tx(s, c, popsc(sc, p)) for s, c, sc in lines if popsc(sc, p)]
    if not sps:
        return
    for s, _, _ in lines:
        seen(s)
    w = max(s.width for s in sps) + 18
    h = sum(s.height for s in sps) + 12
    x0, y0 = cx - w // 2, cy - h // 2
    d.rectangle((x0, y0, x0 + w, y0 + h), fill=P["red"], outline=P["ink"], width=2)
    d.rectangle((x0 + 2, y0 + 2, x0 + w - 2, y0 + 4), fill=P["orange"])
    y = y0 + 7
    for s in sps:
        paste(img, s, cx, y, "mt")
        y += s.height


def flash(img, t, t0, dur=0.15):
    """A camera flash: a white frame fading out."""
    if not 0 <= t - t0 < dur:
        return img
    a = int(150 * (1 - (t - t0) / dur))
    white = Image.new("RGBA", (W, H), (244, 244, 244, a))
    img.alpha_composite(white)
    return img
