#!/usr/bin/env python3
"""The race video: a pixel-art racing game at 480x270, scaled 4x
(1080p60), with synthesized chiptune and sound effects.

The general test first, then the plugins, the climax:
- general: the V8 benchmark suite v7, 8 stations, each benchmark an equal
  eighth of the track, the seven cars and moejs's previous release
  (v0.1.0-alpha.11) at one rate, ×0.8, then its score card;
- plugins: an AI gateway intro, the five legs by name, then one relay
  through them in real time: small, agent100k, big8m, multi8x1m and
  agent100k on eight workers; then the moejs/C QuickJS ratio of each.
On screen every word is plain: programs, races and systems by name; the
suite, scenario ids, build flags, target counts and the general relay's
rate are only on the methods page. Car positions come only from the
recorded timelines (data.py); the race clock reads recorded seconds; no
race changes rate, and a frozen race shows a badge; every bar is
proportional to its number; every caption stays up for its language's
reading time (text.py; --check lists the ones that do not).

Usage: video.py <root> <out.mp4> [--lang zh|en] [--stills <sheet.png>] [--still <still.png>]
                [--claims <claims.json>] [--check <step>]

<root> is the recording of measure.py."""

import copy
import json
import math
import os
import subprocess
import time
from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw

import audio
import data
import pix
import text
from pix import (P, W, H, S, FPS, tx, clamp, popsc, smooth, pop_text, shake, sky, mountains, stars,
                 road_strip, car, confetti, dissolve, checker_band, plate, flash, paste, puffs, speed_lines)

LABEL = "moejs"

# The language tables (text.py); use() picks one before build().
LANG, TX, PLUGIN_NAMES, V8_NAMES, OS_BIG, OS_SMALL, OS_GOOS, OLD_LANE = (None,) * 8

# key, label, body, dark, door tag, reference car. The six Go-embeddable
# engines first, moejs on top; C QuickJS last, behind a curb, as a
# marked reference car.
LANES = [
    ("moejs", LABEL, P["orange"], P["red"], "Go", False),
    ("go-quickjs", "go-quickjs", P["cyan"], P["teal"], "Go", False),
    ("goant", "goant", P["lime"], P["green"], "Go", False),
    ("modernc", "modernc", P["blue"], P["navy"], "Go", False),
    ("goja", "goja", P["red"], P["plum"], "Go", False),
    ("sobek", "Sobek", P["yellow"], P["plum"], "Go", False),
    ("qjs", "C QuickJS", P["grey"], P["slate"], "C", True),
]


def use(lang):
    """Selects the text of one language (a fresh copy, as build() fills in
    the recorded counts)."""
    global LANG, TX, PLUGIN_NAMES, V8_NAMES, OS_BIG, OS_SMALL, OS_GOOS, OLD_LANE
    LANG = copy.deepcopy(text.LANGS[lang])
    TX, PLUGIN_NAMES, V8_NAMES = LANG["TX"], LANG["plugin_names"], LANG["v8_names"]
    OS_BIG, OS_SMALL = [n for n, _ in LANG["os_big"]], [n for n, _ in LANG["os_small"]]
    OS_GOOS = dict(LANG["os_big"] + LANG["os_small"])
    # the V8 relay's eighth car: moejs's previous release, marked like C QuickJS
    OLD_LANE = ("moejs-old", LANG["old"], P["orange"], P["red"], "Go", True)


class Scene:
    dur = 1.0

    def __init__(self):
        self.ev = []  # (local time, sound, kwargs)

    def frame(self, t):
        raise NotImplementedError

    def hum(self, t):
        return 0.0, 0.0


class Hook(Scene):
    dur = 5.4
    T_Q, REV, LAUNCH = 1.3, 4.1, 4.5

    def __init__(self):
        super().__init__()
        self.ev += [(0.3, "pop", {}), (self.T_Q, "slam", {}), (self.REV, "rev", {}), (self.LAUNCH, "whoosh", {})]

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        sky(img, pix.SUNSET)
        stars(d, t, n=60, ymax=50)
        cx, cy, r = W // 2, 88, 46
        for y in range(cy - r, cy):
            w = int(math.sqrt(max(0, r * r - (y - cy) ** 2)))
            k = y - (cy - 22)
            if not (k >= 0 and k % 5 in (0, 1)):
                d.line((cx - w, y, cx + w, y), fill=P["yellow"] if y < cy - 24 else P["orange"])
        mountains(d, t * 20, 1, 12, 8, P["plum"], seed=1.0)
        mountains(d, t * 40, 1, 5, 5, P["ink"], seed=3.0)
        d.rectangle((0, 88, W, H), fill=P["ink"])
        ph = (t * 1.3) % 1
        for k in range(10):
            y = 88 + int(2 * 1.55 ** (k + ph))
            if y < H:
                d.line((0, y, W, y), fill=P["plum"])
        for k in range(-12, 13):
            d.line((cx + k * 6, 88, cx + k * 70, H), fill=P["plum"])
        pop_text(img, TX["hook1"], P["white"], 3, (t - 0.3) / 0.3, W // 2, 8)
        if t >= self.T_Q:
            k = popsc(3, (t - self.T_Q) / 0.3)
            # parts (text, colour[, pull]): pull draws a part that many more
            # pixels to the left, for punctuation after a coloured word
            parts = [(p[0], p[1], p[2] if len(p) > 2 else 0) for p in TX["hook2"]]
            sps = [tx(s, P[c], k) for s, c, _ in parts]
            x = (W - sum(sp.width for sp in sps)) // 2 + k * (len(sps) - 1) + k * sum(q for *_, q in parts) // 2
            y = 112 + (3 - k) * 6
            for sp, (_, _, q) in zip(sps, parts):
                x -= k * q
                paste(img, sp, x, y)
                x += sp.width - k
        road_strip(d, 246, t, 0 if t < self.LAUNCH else 300 * (t - self.LAUNCH))
        for i, (body, dark, tag, ref) in enumerate(((P["orange"], P["red"], "Go", False),
                                                    (P["grey"], P["slate"], "C", True))):
            go = max(0.0, t - self.LAUNCH - i * 0.12)
            x = W // 2 - 96 + i * 112 + int(0.5 * 1400 * go * go)
            bob = 1 if int(t * (30 if t > self.REV else 8) + i) % 2 else 0
            sp = car(body, dark, tag, int(t * 30) if t > self.REV else 0, ref=ref, scale=2)
            if t > self.REV:
                puffs(d, x, 236, t + i * 0.2, 1)
            img.alpha_composite(sp, (x, 212 - bob))
        return shake(img, t, self.T_Q)
class PureGo(Scene):
    """A terminal counts the repository's assembly and C files, then 0 assembly
    (moejs's own code), 0 cgo (the engine with its dependencies) and the
    systems the engine library builds for lighting up by name. The rows
    stay until both notes, and the systems until their title and note,
    have had their reading time."""
    T_TYPE, T_OUT, T_ASM, T_CGO, NOTE = 0.25, 1.35, 2.0, 2.7, 0.3

    def __init__(self, goos):
        super().__init__()
        missing = [n for n in OS_BIG + OS_SMALL if OS_GOOS[n] not in goos]
        assert not missing, missing
        self.T_TILES = max(t0 + self.NOTE + reading_s(TX[k])
                           for t0, k in ((self.T_ASM, "asm_note"), (self.T_CGO, "cgo_note"))) + 0.2
        self.rate = (len(TX["cmd"]) - 2) / (self.T_OUT - 0.25 - self.T_TYPE)
        self.ev.append((0.1, "pop", {}))
        for i in range(len(TX["cmd"]) - 2):
            self.ev.append((self.T_TYPE + i / self.rate, "type", {}))
        self.ev += [(self.T_OUT, "blip", {"f": 880}), (self.T_ASM, "slam", {}), (self.T_CGO, "slam", {})]
        # (name, x, y, w, h, scale, light-up time)
        self.tiles = []
        for i, n in enumerate(OS_BIG):
            col, row = i % 3, i // 3
            self.tiles.append((n, 24 + col * 148, 52 + row * 40, 136, 34, 2, self.T_TILES + 0.25 + 0.08 * i))
        t_small = self.tiles[-1][-1] + 0.25
        for i, n in enumerate(OS_SMALL):
            self.tiles.append((n, 24 + i * 73, 136, 68, 22, 1, t_small + 0.06 * i))
        for k, tile in enumerate(self.tiles):
            self.ev.append((tile[-1], "blip", {"f": 440 * 2 ** (k / 12)}))
        self.t_note = self.tiles[-1][-1] + 0.25
        self.dur = max(self.T_TILES + 0.1 + reading_s(TX["os_title"]), self.t_note + reading_s(TX["os_note"])) + 0.2
        self.ev += [(self.T_TILES + 0.1, "pop", {}), (self.t_note, "coin", {})]

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        sky(img, pix.NIGHT)
        d.rectangle((0, 88, W, H), fill=P["ink"])
        stars(d, t, drift=12, n=90, ymax=230)
        road_strip(d, 236, t, 240)
        cx = int(-40 + (W + 40) * t / self.dur)
        puffs(d, cx, 238, t, 1)
        img.alpha_composite(car(P["orange"], P["red"], "Go", int(t * 30)), (cx, 220 - (int(t * 20) % 2)))
        if t < self.T_ASM:
            if popsc(1, t / 0.25):
                x0, y0, x1, y1 = 40, 60, 440, 128
                d.rectangle((x0, y0, x1, y1), fill=P["ink"], outline=P["slate"], width=2)
                d.rectangle((x0, y0, x1, y0 + 10), fill=P["dark"])
                for i, c in enumerate((P["red"], P["yellow"], P["green"])):
                    d.rectangle((x0 + 6 + i * 8, y0 + 3, x0 + 10 + i * 8, y0 + 7), fill=c)
                n = int(clamp((t - self.T_TYPE) * self.rate, 0, len(TX["cmd"])))
                sp = tx(TX["cmd"][:max(n, 2)], P["white"], outline=None, shadow=False)
                paste(img, sp, x0 + 10, y0 + 18)
                if t < self.T_OUT and int(t * 4) % 2:
                    d.rectangle((x0 + 10 + sp.width, y0 + 20, x0 + 15 + sp.width, y0 + 31), fill=P["lime"])
                if t >= self.T_OUT:
                    pop_text(img, "0", P["lime"], 2, (t - self.T_OUT) / 0.2, x0 + 12, y0 + 36, "lt", outline=None)
        elif t < self.T_TILES:  # both rows stay up together, each note beside its word
            for t0, word, note, y in ((self.T_ASM, TX["asm"], TX["asm_note"], 76),
                                      (self.T_CGO, "cgo", TX["cgo_note"], 166)):
                pop_text(img, "0", P["lime"], 5, (t - t0) / 0.3, 40, y, "mm")
                pop_text(img, word, P["white"], 4, (t - t0 - 0.15) / 0.3, 70, y, "lm")
                nx = 70 + tx(word, P["white"], 4).width + 8
                pop_text(img, note, P["yellow"], 2, (t - t0 - self.NOTE) / 0.3, nx, y + 4, "lm", maxw=W - nx - 4)
        else:
            pop_text(img, TX["os_title"], P["white"], 2, (t - self.T_TILES - 0.1) / 0.3, W // 2, 10)
            for name, x, y, w, h, sc, tl in self.tiles:
                if t < tl - 0.25:
                    continue
                on = t >= tl
                jump = 2 if 0 <= t - tl < 0.12 else 0
                y -= jump
                d.rectangle((x, y, x + w, y + h), fill=P["green"] if on else P["dark"],
                            outline=P["lime"] if on else P["slate"], width=2 if sc > 1 else 1)
                col = P["white"] if on else P["slate"]
                sp = tx(name, col, sc, outline=P["ink"] if on else None, shadow=False)
                if OS_GOOS[name] == "js":  # the hint: Go builds it as WebAssembly
                    hint = tx(TX["wasm"], P["yellow"] if on else P["slate"], outline=P["ink"] if on else None,
                              shadow=False)
                    xm = x + (w - sp.width - hint.width - 4) // 2
                    paste(img, sp, xm, y + h // 2 + 1, "lm")
                    paste(img, hint, xm + sp.width + 4, y + h // 2 + 3, "lm")
                    if on:
                        pix.seen(name + TX["wasm"])
                else:
                    paste(img, sp, x + w // 2, y + h // 2 + 1, "mm")
            pop_text(img, TX["os_note"], P["yellow"], 2, (t - self.t_note) / 0.3, W // 2, 172)
        img = shake(img, t, self.T_ASM)
        img = shake(img, t, self.T_CGO)
        return shake(img, t, self.t_note, amp=2)
class Race(Scene):
    """A relay over len(names) equal legs with a camera that follows the
    leader and parks at the finish. segs maps scene time to race time as
    (t0, rate) pieces; rate 0 (the race frozen where the recorded
    timelines put every car) shows the freeze badge, no other rate shows
    anything. From t_times on, a column at the right edge gives every
    car's own result."""
    X0 = 150
    HZ, RT, LANE = 70, 76, 25
    night = False
    COAST = 120
    HEAD_W, HEAD_X = W - 2 * 128, W // 2
    LIGHTS_XY = (W // 2, 52)
    live = {"GO!"}  # effects and running labels, outside the reading-time rule

    def __init__(self, racers, lanes, names, Lw, lights, bands):
        super().__init__()
        self.r, self.lanes, self.names, self.Lw = racers, lanes, names, Lw
        self.plates, self.sched, self.bursts, self.flashes = [], [], [], []  # plates: (t0, t1, lines, cy)
        self.nlegs = len(names)
        self.lights, self.go = lights, lights[-1]
        self.segs = [(self.go, 1.0)]
        self.bands = bands
        self.finish = self.X0 + self.nlegs * Lw
        self.RB = self.RT + self.LANE * len(lanes)
        self.sep = next((i for i, l in enumerate(lanes) if l[5]), None)
        self.t = 0.0
        self.t_times, self.times_on = None, False
        for i, tl in enumerate(lights):
            self.ev.append((tl, "go" if i == len(lights) - 1 else "beep", {}))

    # --- clocks
    def rt(self, t):
        if t < self.go:
            return 0.0
        rt, (pt, rate) = 0.0, self.segs[0]
        for t0, r in self.segs[1:]:
            if t < t0:
                break
            rt += rate * (t0 - pt)
            pt, rate = t0, r
        return rt + rate * (t - pt)

    def rate(self, t):
        rate = 0.0 if t < self.go else 1.0
        for t0, r in self.segs:
            if t >= t0:
                rate = r
        return rate

    def t_of(self, rt):
        acc, (pt, rate) = 0.0, self.segs[0]
        for t0, r in self.segs[1:]:
            span = rate * (t0 - pt)
            if acc + span >= rt:
                break
            acc += span
            pt, rate = t0, r
        return pt + (rt - acc) / rate

    def rank(self):
        """moejs's place among the cars shown (finish time; ties share)."""
        me = self.r["moejs"].end
        return 1 + sum(r.end < me for k, r in self.r.items() if k != "moejs")

    def front(self, k, rt):
        r = self.r[k]
        f = self.X0 + r.pos(rt) * self.Lw
        if rt > r.end:
            f += self.COAST * (1 - math.exp(-(rt - r.end) * 2.0))
        return f

    def cam(self, t):
        rt = self.rt(t)
        lead = max(self.front(k, rt) for k, *_ in self.lanes)
        return clamp(lead - 300, 0, self.finish - 300)

    # --- world
    def ground(self, d, cam):
        RT, RB, HZ = self.RT, self.RB, self.HZ
        d.rectangle((0, HZ, W, RT - 4), fill=P["green"])
        for x in range(-16, W + 16, 16):
            sx = x - int(cam) % 32
            d.rectangle((sx, HZ, sx + 7, RT - 5), fill=P["lime"] if (x // 16) % 2 else P["green"])
        curbs = [(RT - 4, RT - 1), (RB, RB + 3)]
        if self.sep is not None:
            y = RT + self.sep * self.LANE
            curbs.append((y - 1, y + 1))
        d.rectangle((0, RT, W, RB - 1), fill=P["dark"])
        for y0, y1 in curbs:
            for x in range(-16, W + 24, 8):
                sx = x - int(cam) % 16
                c = P["red"] if ((x + int(cam) - int(cam) % 16) // 8) % 2 else P["white"]
                d.rectangle((sx, y0, sx + 7, y1), fill=c)
        for k in range(1, len(self.lanes)):
            if k == self.sep:
                continue
            y = RT + k * self.LANE
            for x in range(-12, W + 12, 12):
                sx = x - int(cam) % 24
                if ((x + 12) // 12) % 2:
                    d.rectangle((sx, y, sx + 5, y), fill=P["slate"])
        d.rectangle((0, RB + 4, W, H), fill=P["green"])

    def gates(self, img, d, cam):
        for b in range(self.nlegs + 1):
            sx = self.X0 + b * self.Lw - int(cam)
            if sx < -70 or sx > W + 70:
                continue
            if b == self.nlegs:
                for y in range(self.RT, self.RB, 3):
                    for c in range(3):
                        col = P["white"] if ((y - self.RT) // 3 + c) % 2 else P["ink"]
                        d.rectangle((sx + c * 3, y, sx + c * 3 + 2, y + 2), fill=col)
                d.line((sx + 4, 42, sx + 4, self.RT - 4), fill=P["grey"], width=2)
                wave = int(self.t * 8) % 2
                for yy in range(4):
                    for xx in range(6):
                        col = P["ink"] if (xx + yy) % 2 else P["white"]
                        oy = wave if xx % 2 else 0
                        d.rectangle((sx + 6 + xx * 3, 42 + yy * 3 + oy, sx + 8 + xx * 3, 44 + yy * 3 + oy), fill=col)
                continue
            d.rectangle((sx, self.RT, sx + 1, self.RB - 1), fill=P["white"])
            d.line((sx, 58, sx, self.RT - 4), fill=P["grey"], width=2)
            sp = tx(self.names[b], P["yellow"], outline=None, shadow=False)
            bw = sp.width + 6
            d.rectangle((sx - bw // 2, 42, sx + bw // 2, 58), fill=P["ink"], outline=P["slate"])
            paste(img, sp, sx, 43, "mt")

    def draw_lights(self, img, d, t):
        if t < self.lights[0] - 0.4 or t > self.go + 0.9:
            return
        bx, by = self.LIGHTS_XY
        d.rectangle((bx - 40, by - 12, bx + 40, by + 12), fill=P["ink"], outline=P["slate"], width=2)
        for i in range(3):
            x = bx - 24 + i * 24
            on = t >= self.lights[i]
            col = P["lime"] if t >= self.go else (P["red"] if on else P["dark"])
            d.ellipse((x - 8, by - 8, x + 8, by + 8), fill=col, outline=P["ink"])
            if on:
                d.point((x - 3, by - 3), fill=P["white"])
        if t >= self.go:
            pop_text(img, "GO!", P["yellow"], 5, (t - self.go) / 0.25, W // 2, by + (98 if by < 100 else 0), "mm")
    def draw_lane(self, img, d, i, lane, t, rt, cam, leader):
        k, name, body, dark, tag, ref = lane
        r, L = self.r[k], self.LANE
        top = self.RT + i * L
        f = self.front(k, rt)
        sx = int(f - cam)
        x0 = int(self.X0 - cam)
        done = rt >= r.end
        moving = t >= self.go and (not done or rt - r.end < 1.0) and self.rate(t) > 0
        trail_end = sx - 36
        if trail_end > max(x0, 0):
            d.rectangle((max(x0, 0), top + L - 3, trail_end, top + L - 2), fill=body)
        lab = tx(name, P["white"] if k == "moejs" else P["grey"], outline=P["ink"], shadow=False)
        if sx < 42:  # behind the camera: an arrow and the name at the left edge
            cyl = top + L // 2
            d.polygon([(2, cyl), (6, cyl - 4), (6, cyl + 4)], fill=body)
            paste(img, lab, 9, cyl, "lm")
            return
        if sx - 40 > W:
            return
        cy = top + L - 19
        bob = 1 if moving and int(t * 20 + i) % 2 else 0
        if k == leader and moving and not done:
            speed_lines(d, sx - 40, cy + 3, t)
        if moving:
            puffs(d, sx - 40, cy + 12, t + i * 0.13, 1)
        img.alpha_composite(car(body, dark, tag, int(t * 30) if moving else 0, ref=ref), (sx - 40, cy - bob))
        if sx - 44 - lab.width >= 2:
            paste(img, lab, sx - 44, cy + 9, "rm")
        elif not done:
            paste(img, lab, sx + 4, cy + 9, "lm")
        if done and not self.times_on:
            tag_t = tx(TX["sec"].format(x=r.end), P["yellow"] if k == "moejs" else P["white"], outline=P["ink"], shadow=False)
            paste(img, tag_t, sx + 3, cy + 9, "lm")

    def minimap(self, img, d, rt):
        mx0, mx1, my = 140, 340, self.RB + 11
        if my + 3 >= H:
            return
        d.rectangle((mx0 - 2, my - 3, mx1 + 2, my + 3), fill=P["ink"])
        for b in range(1, self.nlegs):
            xb = mx0 + (mx1 - mx0) * b // self.nlegs
            d.line((xb, my - 3, xb, my + 3), fill=P["slate"])
        self.markers(d, rt, mx0, mx1, my)

    def markers(self, d, rt, mx0, mx1, my, r=2):
        """A dot per car, moejs on top; a reference car's dot is outlined
        white like its car."""
        for k, _, body, _, _, ref in sorted(self.lanes, key=lambda l: l[0] == "moejs"):
            x = mx0 + (mx1 - mx0) * min(self.r[k].pos(rt), self.nlegs) / self.nlegs
            d.rectangle((x - r, my - r, x + r, my + r), fill=body, outline=P["white"] if ref else P["ink"])

    def badge(self, img, d, t):
        if t < self.go or self.rate(t) != 0:
            return
        sp = tx(TX["freeze"], P["white"], 2)
        x0, y0 = 6, 6
        bw = sp.width + 26
        d.rectangle((x0, y0, x0 + bw, y0 + sp.height + 2), fill=P["blue"], outline=P["ink"], width=2)
        if int(t * 4) % 2:
            for k in range(2):
                xx = x0 + 5 + k * 7
                d.rectangle((xx, y0 + 10, xx + 3, y0 + 22), fill=P["yellow"])
        paste(img, sp, x0 + 24, y0 + 1)

    def frame(self, t):
        self.t = t
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        rt, cam = self.rt(t), int(self.cam(t))
        sky(img, self.bands, self.HZ)
        if self.night:
            stars(d, t, n=50, ymax=self.HZ - 20)
        else:
            pix.clouds(d, cam, t)
        mountains(d, cam, 0.08, 30, 18, P["slate"], self.HZ, snow=P["white"])
        mountains(d, cam, 0.25, 14, 8, P["teal"], self.HZ, seed=2.0)
        mountains(d, cam, 0.5, 5, 4, P["green"], self.HZ, seed=4.0)
        self.ground(d, cam)
        self.gates(img, d, cam)
        fronts = {k: self.front(k, rt) for k, *_ in self.lanes}
        leader = max(fronts, key=fronts.get)
        self.times_on = self.t_times is not None and t >= self.t_times
        for i, lane in enumerate(self.lanes):
            self.draw_lane(img, d, i, lane, t, rt, cam, leader)
        if self.times_on:
            self.times_column(img, d, t)
        self.minimap(img, d, rt)
        paste(img, tx(TX["sec"].format(x=rt), P["yellow"], 2), W - 8, 6, "rt")
        self.badge(img, d, t)
        self.draw_lights(img, d, t)
        self.overlay(img, d, t, rt, cam)
        return img

    col_head = "col_time"

    def result(self, k):
        """The right-edge label of car k once the results show."""
        return TX["sec"].format(x=self.r[k].end)

    def times_column(self, img, d, t):
        p = (t - self.t_times) / 0.25
        pop_text(img, TX[self.col_head], P["white"], 1, p, W - 4, self.RT - 19, "rt")
        for i, (k, *_) in enumerate(self.lanes):
            if not popsc(1, p - 0.4 * i):
                continue
            y = self.RT + i * self.LANE + self.LANE // 2
            sp = tx(self.result(k), P["yellow"] if k == "moejs" else P["white"], outline=P["ink"], shadow=False)
            d.rectangle((W - 8 - sp.width, y - 8, W - 2, y + 8), fill=P["ink"])
            pix.seen(self.result(k))
            paste(img, sp, W - 4, y, "rm")

    def overlay(self, img, d, t, rt, cam):
        busy = False  # a plate over the headline
        for t0, t1, lines, cy in self.plates:
            if t0 <= t < t1:
                plate(img, d, lines, (t - t0) / 0.3, cy=cy)
                busy = busy or cy < self.RT
        if not busy:
            for t0, t1, s in self.sched:
                if t0 <= t < t1:
                    pop_text(img, s, P["white"], 2, (t - t0) / 0.25, self.HEAD_X, 6, maxw=self.HEAD_W)
        for t0, x in self.bursts:
            confetti(d, x - cam, self.RT - 20, t - t0, n=160)
        for t0 in self.flashes:
            flash(img, t, t0)

    def hum(self, t):
        if t < self.go - 0.6 or (t >= self.go and self.rate(t) == 0):
            return 0.0, 0.0
        if t < self.go:
            return 55.0, 0.03
        rt = self.rt(t)
        if not any(rt < self.r[k].end + 0.8 for k, *_ in self.lanes):
            return 0.0, 0.0
        f = 60 + 50 * smooth((t - self.go) / 0.6)
        return f + 3 * math.sin(t * 7), 0.05


class Relay(Race):
    """A relay at one rate from GO: when moejs finishes, the race freezes
    (the freeze badge) with every car where its timeline puts it, the place plate
    holds to the end, and after COL s the right-edge column gives every
    car's own finish time."""
    COL, TAIL = 0.8, 4.6
    LIGHTS = [0.7, 1.2, 1.7, 2.2]

    def __init__(self, racers, lanes, names, Lw, rate, bands=pix.DAY):
        super().__init__(racers, lanes, names, Lw, self.LIGHTS, bands)
        me = racers["moejs"]
        self.segs = [(self.go, rate)]
        self.t_win = self.t_of(me.end)
        self.segs.append((self.t_win, 0.0))
        self.t_times = self.t_win + self.COL
        self.dur = self.t_win + self.TAIL
        self.starts = [self.t_of(l[0]) for l in me.legs]
        place = TX["first"].format(n=self.rank(), nth=text.ordinal(self.rank()))
        self.plates.append((self.t_win, self.dur, [(place, P["yellow"], 3)], 38))
        self.bursts.append((self.t_win, self.finish))
        self.ev += [(self.t_win, "jingle", {}), (self.t_win, "confetti", {}), (self.t_times, "pop", {})]


class V8Relay(Relay):
    """Richards → … → NavierStokes, eight equal legs, the seven cars and
    moejs's previous release (a marked reference car under the curb, like
    C QuickJS), all at RATE from GO to the freeze, with no badge: the
    clock reads recorded seconds and the methods page gives the rate. The
    headline names the program moejs is running and the gates the ones
    ahead; at the freeze a second plate gives moejs's speedup over its
    previous release."""
    RATE, HZ, RT, LANE = 0.8, 65, 71, 23
    TAIL, T_OLD = 5.4, 0.9

    def __init__(self, racers, names):
        self.live = Race.live | set(V8_NAMES.values())
        heads = [V8_NAMES[n] for n in names]
        super().__init__(racers, LANES + [OLD_LANE], heads, 240, self.RATE, bands=pix.DUSK)
        me, old, starts = racers["moejs"], racers["moejs-old"], self.starts
        for i, head in enumerate(heads):
            t1 = starts[i + 1] if i + 1 < len(heads) else self.t_win
            self.sched.append((0.15 if i == 0 else starts[i], t1, head))
            if i:
                self.ev.append((starts[i], "pop", {}))
        self.speedup = old.end / me.end
        t_old = self.t_win + self.T_OLD
        # over the empty middle lanes: at the freeze only moejs is in view
        self.plates.append((t_old, self.dur, [(TX["old_fast"].format(x=self.speedup), P["yellow"], 2)], 178))
        self.ev.append((t_old, "slam", {}))
        self.leg_screen = [self.t_of(l[2]) - self.t_of(l[0]) for l in me.legs]
        self.beats = dict(v8_lights=1.3, v8_go=self.go + 0.6, v8_leg3=self.t_of(me.legs[2][0] + 0.8),
                          v8_leg6=self.t_of(me.legs[5][0] + 0.8), v8_first=self.t_win + 0.6,
                          v8_old=t_old + 1.5, v8_times=self.dur - 0.4)


class PluginRelay(Relay):
    """The plugin relay in real time: five equal legs (data.plugin_relay),
    each car on its own recorded times. The route map under the track
    names the legs and lights each one as the leader enters it. When
    moejs takes the first leg from C QuickJS by less than CLOSE, both
    crossings flash and the headline reads the close-win caption for its
    reading time."""
    RT, LANE, TAIL = 76, 23, 5.0
    CLOSE = 1.10  # the close-win caption while C QuickJS takes less than 10 % longer
    MX0, MX1 = 16, 464

    def __init__(self, racers, names):
        super().__init__(racers, LANES, names, 240, 1.0)
        me, q = racers["moejs"], racers["qjs"]
        # the leader enters leg i when the first car does
        self.enter = [min(r.legs[i][0] for r in racers.values()) for i in range(self.nlegs)]
        self.ev += [(self.t_of(rt), "pop", {}) for rt in self.enter[1:]]
        self.ratio_first = q.legs[0][2] / me.legs[0][2]
        self.close = 1 < self.ratio_first < self.CLOSE
        self.sched = [(0.15, self.t_win, TX["plug_head"])]
        t_close = None
        if self.close:
            t_me, t_q = self.t_of(me.legs[0][2]), self.t_of(q.legs[0][2])
            t_close = t_q + reading_s(TX["close"]) + 0.3
            self.sched = [(0.15, t_q, TX["plug_head"]), (t_q, t_close, TX["close"]),
                          (t_close, self.t_win, TX["plug_head"])]
            self.flashes += [t_me, t_q]
            self.ev += [(t_me, "shutter", {}), (t_q, "shutter", {}), (t_q + 0.1, "slam", {})]
        self.beats = dict(plug_go=self.go + 1.0, plug_leg3=self.t_of(me.legs[2][0] + 1.0),
                          plug_leg5=self.t_of(me.legs[4][0] + 0.6), plug_first=self.t_win + 0.8,
                          plug_times=self.dur - 0.4)
        if self.close:
            self.beats["plug_close"] = self.t_of(q.legs[0][2]) + 0.5

    def minimap(self, img, d, rt):
        """The route: the five legs by name over a bar with a dot per car."""
        y0, n = self.RB + 4, self.nlegs
        d.rectangle((0, y0, W, H), fill=P["ink"])
        x0, x1, yb = self.MX0, self.MX1, y0 + 23
        d.rectangle((x0, yb - 2, x1, yb + 2), fill=P["dark"])
        for i, name in enumerate(self.names):
            xa, xb = x0 + (x1 - x0) * i // n, x0 + (x1 - x0) * (i + 1) // n
            on = self.t >= self.go and rt >= self.enter[i]
            jump = 2 if on and 0 <= rt - self.enter[i] < 0.15 else 0
            sp = tx(name, P["yellow"] if on else P["slate"], outline=P["ink"], shadow=False)
            paste(img, sp, (xa + xb) // 2, y0 + 1 - jump, "mt")
            pix.seen(name)
            if i:
                d.line((xa, yb - 4, xa, yb + 4), fill=P["grey"])
        self.markers(d, rt, x0, x1, yb, r=3)


def card_bg(img, d, t):
    stars(d, t, drift=10, n=80, ymax=H)
    checker_band(d, 0, t)
    checker_band(d, H - 8, t)


class StationTiles(Scene):
    """The five plugin legs against C QuickJS, under the plugin headline:
    per leg the speed ratio rolls up from 1.00×, with two bars proportional
    to speed (moejs full, C QuickJS its fraction): request rate for the
    four serial legs, the requests done in the window for the 8-core leg."""
    dur = 6.4

    def __init__(self, rows, title):
        super().__init__()
        self.rows, self.title = rows, title  # rows: (name, ratio)
        self.starts = [0.6 + 0.45 * i for i in range(len(rows))]
        for s in self.starts:
            for k in range(6):
                self.ev.append((s + k * 0.1, "blip", {"f": 660 - k * 30}))
            self.ev.append((s + 0.75, "coin", {}))
        self.ev += [(0.15, "pop", {})]

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        card_bg(img, d, t)
        pop_text(img, self.title, P["yellow"], 2, (t - 0.15) / 0.25, W // 2, 10)
        pop_text(img, TX["tiles_sub"], P["white"], 1, (t - 0.4) / 0.25, W // 2, 42)
        top = max(v for _, v in self.rows)
        for i, (name, v) in enumerate(self.rows):
            if not popsc(1, (t - self.starts[i] + 0.3) / 0.3):
                continue
            row, col = divmod(i, 3)
            n = 3 if row == 0 else len(self.rows) - 3
            cx = W // 2 + (col - (n - 1) / 2) * 152
            bob = int(2 * math.sin(t * 3 + i))
            x0, y0 = int(cx) - 70, 62 + row * 100 + bob
            hot = v == top
            d.rectangle((x0, y0, x0 + 140, y0 + 94), fill=P["dark"], outline=P["yellow"] if hot else P["slate"],
                        width=2)
            pop_text(img, name, P["yellow"], 1, 1, int(cx), y0 + 5, maxw=136)
            roll = clamp((t - self.starts[i]) / 0.8)
            val = 1.0 + (v - 1.0) * (1 - (1 - roll) ** 3)
            paste(img, tx(f"{val:.2f}×", P["orange"] if hot else P["white"], 3), int(cx), y0 + 22, "mt")
            for j, (tag, colr, frac) in enumerate((("moejs", P["orange"], 1.0), ("C", P["grey"], 1 / val))):
                y = y0 + 64 + j * 14
                paste(img, tx(tag, colr, outline=None, shadow=False), x0 + 6, y - 4)
                d.rectangle((x0 + 42, y + 1, x0 + 42 + max(2, int(88 * frac)), y + 7), fill=colr)
        return img


class V8Score(Scene):
    """The suite's own scores as a drag strip. Every car leaves the start
    line and drives a distance proportional to its score, so the bar from
    the line to its nose is its score; the number rolls up behind it."""
    dur = 7.2
    X0, SPAN = 124, 310

    def __init__(self, scores, title):
        super().__init__()
        self.s, self.title = scores, title
        self.ORDER = sorted(scores, key=lambda k: -scores[k])  # highest first
        self.ratio = scores["moejs"] / scores["qjs"]
        self.ev += [(0.15, "pop", {}), (0.6, "rev", {}), (0.7, "whoosh", {}), (2.3, "coin", {}),
                    (2.8, "slam", {})]

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        card_bg(img, d, t)
        pop_text(img, self.title, P["yellow"], 2, (t - 0.15) / 0.25, W // 2, 6)
        pop_text(img, TX["score_sub"], P["grey"], 1, (t - 0.4) / 0.25, W // 2, 38)
        lanes = {l[0]: l for l in LANES}
        top = max(self.s[k] for k in self.ORDER)
        x0 = self.X0
        y0 = 58
        for y in range(y0 - 2, y0 + 23 * len(self.ORDER), 4):  # the start line
            d.rectangle((x0, y, x0 + 1, y + 1), fill=P["white"] if (y // 4) % 2 else P["slate"])
        for i, k in enumerate(self.ORDER):
            _, name, body, dark, tag, ref = lanes[k]
            y = y0 + i * 23
            col = P["white"] if k == "moejs" else P["grey"]
            nm = "moejs" if k == "moejs" else name
            paste(img, tx(nm, col, outline=P["ink"], shadow=False), x0 - 46, y + 10, "rm")
            p = smooth((t - 0.6 - 0.08 * i) / 1.6)
            val = self.s[k] * p
            nose = x0 + int(self.SPAN * val / top)
            if nose - 1 >= x0 + 2:
                d.rectangle((x0 + 2, y + 13, nose - 1, y + 18), fill=body)
                d.line((x0 + 2, y + 18, nose - 1, y + 18), fill=dark)
            moving = 0 < p < 1
            img.alpha_composite(car(body, dark, tag, int(t * 30) if moving else 0, ref=ref),
                                (nose - 40, y + 1 - (1 if moving and int(t * 20 + i) % 2 else 0)))
            paste(img, tx(f"{val:.0f}", P["yellow"] if k == "moejs" else P["white"], outline=P["ink"],
                          shadow=False), nose + 4, y + 10, "lm")
        pop_text(img, TX["score_vs"].format(x=self.ratio), P["yellow"], 2, (t - 2.8) / 0.3, W // 2, 228)
        return shake(img, t, 2.8, amp=2)


class GenTitle(Scene):
    """The general scene's title: eight classic JS programs, each by its
    plain word."""
    dur = 5.1

    def __init__(self, names):
        super().__init__()
        self.names = names
        self.ev += [(0.1, "whoosh", {}), (0.15, "pop", {})]
        self.ev += [(0.6 + 0.08 * i, "blip", {"f": 520 + 40 * i}) for i in range(len(names))]

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        card_bg(img, d, t)
        pop_text(img, TX["gen"], P["yellow"], 3, (t - 0.15) / 0.3, W // 2, 16)
        pop_text(img, TX["gen_sub"], P["white"], 2, (t - 0.3) / 0.3, W // 2, 70)
        for i, n in enumerate(self.names):
            p = (t - 0.6 - 0.08 * i) / 0.25
            if not popsc(1, p):
                continue
            col, row = i % 4, i // 4
            x, y = 14 + col * 114, 124 + row * 46
            d.rectangle((x, y, x + 108, y + 34), fill=P["dark"], outline=P["slate"])
            pop_text(img, V8_NAMES[n], P["yellow"], 1, p, x + 54, y + 18, "mm", maxw=104)
        return img


class Gateway(Scene):
    """The plugin part's intro: requests flow through an AI gateway, and
    each one passes the JS plugin on its way to the model."""
    dur = 6.0
    BOX = (176, 118, 304, 182)

    def __init__(self):
        super().__init__()
        self.sends = [1.0 + 0.45 * k for k in range(13)]
        self.ev += [(0.1, "whoosh", {}), (0.15, "pop", {}), (0.55, "pop", {}), (0.9, "slam", {})]
        self.ev += [(t0 + 0.55, "blip", {"f": 660 + 30 * k}) for k, t0 in enumerate(self.sends)]

    def envelope(self, d, x, y, done):
        body = P["lime"] if done else P["white"]
        d.rectangle((x, y, x + 13, y + 9), fill=body, outline=P["ink"])
        d.line((x + 1, y + 1, x + 7, y + 5), fill=P["slate"])
        d.line((x + 12, y + 1, x + 7, y + 5), fill=P["slate"])

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        card_bg(img, d, t)
        pop_text(img, TX["gw"], P["yellow"], 3, (t - 0.15) / 0.3, W // 2, 12)
        pop_text(img, TX["gw_sub"], P["white"], 2, (t - 0.55) / 0.3, W // 2, 70)
        if t < 0.8:
            return img
        x0, y0, x1, y1 = self.BOX
        cy = (y0 + y1) // 2
        for x in range(20, W - 20, 6):  # the request path
            d.rectangle((x, cy + 14, x + 2, cy + 15), fill=P["slate"])
        d.rectangle((x0, y0, x1, y1), fill=P["dark"], outline=P["slate"], width=2)
        k = popsc(2, (t - 0.9) / 0.3)
        if k:
            chip = tx(TX["chip"], P["ink"], k, outline=None, shadow=False)
            cw, ch = chip.width + 12, chip.height + 6
            hot = any(0 <= t - (t0 + 0.55) < 0.15 for t0 in self.sends)
            d.rectangle(((x0 + x1 - cw) // 2, cy - ch // 2, (x0 + x1 + cw) // 2, cy + ch // 2),
                        fill=P["yellow"] if hot else P["orange"], outline=P["ink"], width=2)
            paste(img, chip, (x0 + x1) // 2, cy + 1, "mm")
        paste(img, tx(TX["gw_in"], P["grey"], outline=P["ink"], shadow=False), 44, y1 + 10, "mt")
        paste(img, tx(TX["gw_out"], P["grey"], outline=P["ink"], shadow=False), W - 52, y1 + 10, "mt")
        for t0 in self.sends:
            u = t - t0
            if not 0 <= u < 1.4:
                continue
            x = int(20 + (W - 54) * u / 1.4)
            if x0 - 8 < x < x1 - 6:  # inside the box, behind the chip
                continue
            self.envelope(d, x, cy + 9, u > 0.55)
        return img


class PluginTitle(Scene):
    """The plugin part's title: the five legs of the relay in order, each
    by its plain name and what its requests carry."""
    ROW0, ROW_H = 104, 27

    def __init__(self, rows):
        super().__init__()
        self.rows = rows  # (name, description)
        self.at = [0.7 + 0.25 * i for i in range(len(rows))]
        self.ev += [(0.1, "whoosh", {}), (0.15, "pop", {})] + [(t0, "blip", {"f": 560 + 50 * i})
                                                              for i, t0 in enumerate(self.at)]
        self.dur = max([0.15 + reading_s(TX["plug"]), 0.4 + reading_s(TX["plug_sub"])] +
                       [t0 + 0.15 + reading_s(desc) for t0, (_, desc) in zip(self.at, rows)]) + 0.2

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        card_bg(img, d, t)
        pop_text(img, TX["plug"], P["yellow"], 3, (t - 0.15) / 0.3, W // 2, 14)
        pop_text(img, TX["plug_sub"], P["white"], 2, (t - 0.4) / 0.3, W // 2, 66)
        for i, ((name, desc), t0) in enumerate(zip(self.rows, self.at)):
            if not popsc(1, (t - t0) / 0.25):
                continue
            y = self.ROW0 + i * self.ROW_H
            d.rectangle((26, y, 454, y + 22), fill=P["dark"], outline=P["slate"])
            paste(img, tx(str(i + 1), P["orange"], outline=None, shadow=False), 40, y + 11, "mm")
            pop_text(img, name, P["yellow"], 1, (t - t0) / 0.25, 56, y + 3, "lt")
            pop_text(img, desc, P["white"], 1, (t - t0 - 0.15) / 0.25, 166, y + 3, "lt", maxw=284)
        return img


class End(Scene):
    """moejs, the README tagline, and the two headlines: the plugin lead
    first, the general test under it (build() checks the results both
    need)."""
    dur = 6.4

    def __init__(self):
        super().__init__()
        self.lines = [(TX["end_head"], P["yellow"], 3), (TX["end_sub"], P["white"], 2)]
        self.ev += [(0.0, "whoosh", {}), (0.2, "slam", {}), (0.8, "pop", {}), (1.4, "slam", {}),
                    (2.0, "pop", {})]

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        sky(img, pix.DUSK)
        stars(d, t, drift=6, n=40, ymax=40)
        mountains(d, t * 30, 1, 22, 14, P["plum"], seed=1.0)
        mountains(d, t * 60, 1, 8, 8, P["ink"], seed=3.0)
        d.rectangle((0, 88, W, H), fill=P["ink"])
        road_strip(d, 226, t, 320)
        x = int(min(200, -90 + 400 * t))
        speed_lines(d, x, 200, t)
        puffs(d, x, 232, t, 1)
        img.alpha_composite(car(P["orange"], P["red"], "Go", int(t * 30), scale=2), (x, 204 - int(t * 20) % 2))
        confetti(d, W // 2, -20, (t - 1.4) % 3.0, n=90, seed=int((t - 1.4) // 3.0) + 3, spread=260)
        pop_text(img, "moejs", P["orange"], 4, (t - 0.2) / 0.3, W // 2, 4)
        pop_text(img, TX["end_tag"], P["white"], 2, (t - 0.8) / 0.3, W // 2, 76)
        if t >= 1.4:
            plate(img, d, self.lines, (t - 1.4) / 0.3, cy=156)
        pop_text(img, "github.com/MoeForge/moejs", P["white"], 1, (t - 2.0) / 0.3, W // 2, 250)
        return shake(img, t, 1.4, amp=2)


class Method(Scene):
    """The methods page, a line every STEP s, held until the last line has
    had its reading time; it fades to black."""
    STEP = 0.15

    def __init__(self, lines):
        super().__init__()
        self.lines = lines
        self.ev += [(0.15, "pop", {})] + [(0.5 + i * self.STEP, "blip", {"f": 660 + i * 50})
                                          for i in range(len(self.lines))]
        self.dur = max(0.5 + i * self.STEP + reading_s(s) for i, s in enumerate(lines)) + 0.2

    def frame(self, t):
        img = Image.new("RGBA", (W, H), P["ink"])
        d = ImageDraw.Draw(img)
        stars(d, t, drift=8, n=70, ymax=H)
        pop_text(img, TX["method"], P["white"], 2, (t - 0.15) / 0.25, W // 2, 6)
        for i, s in enumerate(self.lines):
            p = (t - 0.5 - i * self.STEP) / 0.25
            if popsc(1, p):
                y = 36 + i * 17
                d.rectangle((14, y + 5, 19, y + 10), fill=P["orange"])
                pop_text(img, s, P["white"], 1, p, 26, y, "lt", maxw=W - 30)
        if t > self.dur - 1.0:
            img = dissolve(img, Image.new("RGBA", (W, H), (0, 0, 0, 255)), (t - (self.dur - 1.0)) / 0.9)
        return img


class Video:
    TR = 0.5

    def __init__(self, scenes):
        self.scenes = scenes
        self.starts = []
        t = 0.0
        for s in scenes:
            self.starts.append(t)
            t += s.dur
        self.dur = t

    def at(self, T):
        for i in range(len(self.scenes) - 1, -1, -1):
            if T >= self.starts[i]:
                return i, T - self.starts[i]
        return 0, T

    def frame(self, T):
        i, t = self.at(T)
        s = self.scenes[i]
        pix.REC, pix.NOW = i, T
        img = s.frame(min(t, s.dur - 1e-6))
        pix.REC = None
        if i and t < self.TR:
            prev = self.scenes[i - 1]
            img = dissolve(prev.frame(prev.dur - 1e-6), img, t / self.TR)
        return img

    def reading(self, step):
        """Captions on screen for less than their reading time (text.py),
        from SEEN after rendering every step-th second; the methods page is
        reference text and is listed apart."""
        short, method = [], []
        for (i, cap), (a, b) in sorted(pix.SEEN.items(), key=lambda kv: kv[1][0]):
            if cap in getattr(self.scenes[i], "live", ()):
                continue
            need = reading_s(cap)
            got = b - a + step
            if got + 1e-6 < need:
                (method if isinstance(self.scenes[i], Method) else short).append(
                    (type(self.scenes[i]).__name__, cap, round(got, 2), round(need, 2)))
        return short, method

    def events(self):
        return [(t0 + t, n, kw) for s, t0 in zip(self.scenes, self.starts) for t, n, kw in s.ev]

    def beats(self):
        """Named moments in video time, for the stills and compare.py."""
        out = {}
        for s, t0 in zip(self.scenes, self.starts):
            for k, t in getattr(s, "beats", {}).items():
                out[k] = t0 + t
        return out


def reading_s(s):
    """The least time a caption stays on screen, by the language's rule
    (text.py)."""
    return LANG["reading"](s)


WINDOW = 10.0  # the 8-core leg's window, as recorded (measure.py)
SCENARIOS = data.PLUGIN_LEGS + ["agent100k"]  # the five legs; the fifth on eight workers
LEAD = 2.0  # the plugin headline: every other car takes at least twice moejs's relay time


def lanes_line(ks):
    """The methods line on lanes: the scenes that ran beside other tests,
    one per core (K busy neighbours > 0); the rest ran alone."""
    words = LANG["scene_words"]
    shared = list(dict.fromkeys(words[s] for s in words if ks.get(s)))
    if not shared:
        return LANG["lanes_alone"]
    line = LANG["lanes_shared"].format(scenes=LANG["lanes_join"].join(shared))
    return line[:1].upper() + line[1:]


def leg5_requests(counts):
    """The fifth leg's length: the most whole thousands of requests every
    engine's eight workers finished inside the recorded window."""
    return min(c.done(WINDOW) for c in counts.values()) // 1000 * 1000


def build(root):
    keys = [l[0] for l in LANES]
    v8, names = data.v8_relay(root, keys + ["moejs-old"])
    sc = {k: v for k, v in data.scores(root).items() if k in keys}
    ks = data.lanes(root)
    par = data.par8(root, keys, WINDOW)
    n = leg5_requests(par)
    TX["subs"][4] = TX["subs"][4].format(n=n)
    port = {}
    for f in ("core.tsv", "purego-core.tsv"):
        rows = [line.split("\t") for line in (Path(root) / "portability" / f).read_text().splitlines()]
        assert all(rc == "0" for _, rc in rows), f
        port[f] = [t for t, _ in rows]
    assert port["core.tsv"] == port["purego-core.tsv"]
    goos = sorted({t.split("/")[0] for t in port["core.tsv"]})
    goarch = sorted({t.split("/")[1] for t in port["core.tsv"]})
    fill = iter([TX["leg5"].format(n=n), lanes_line(ks)])
    TX["method_lines"] = [next(fill) if line is None else line.format(targets=len(port["core.tsv"]))
                          for line in TX["method_lines"]]
    hook, pure = Hook(), PureGo(set(goos))
    hook.beats = dict(hook=3.7)
    pure.beats = dict(zero=pure.T_TILES - 0.6, os=pure.dur - 0.6)
    gen = GenTitle(names)
    gen.beats = dict(gen=gen.dur - 0.6)
    v = V8Relay(v8, names)
    # the general headline holds only if moejs is first of the eight cars
    # and scores at least C QuickJS's score
    assert v.rank() == 1 and sc["moejs"] >= sc["qjs"], "the general headline does not hold"
    score = V8Score(sc, TX["score"])
    score.beats = dict(score=score.dur - 0.8)
    gw = Gateway()
    gw.beats = dict(gateway=gw.dur - 0.6)
    pt = PluginTitle(list(zip(PLUGIN_NAMES, TX["subs"])))
    pt.beats = dict(plugins=pt.dur - 0.6)
    relay = PluginRelay(data.plugin_relay(root, keys, n, WINDOW), PLUGIN_NAMES)
    legs = {k: [l[2] - l[0] for l in r.legs] for k, r in relay.r.items()}
    places = [1 + sum(legs[k][i] < legs["moejs"][i] for k in keys if k != "moejs") for i in range(relay.nlegs)]
    counts = {k: c.done(WINDOW) for k, c in par.items()}
    # the plugin headline holds only if moejs is first in every leg and
    # every other car takes at least LEAD times its relay time
    me = relay.r["moejs"].end
    assert relay.rank() == 1 and places == [1] * relay.nlegs and \
        all(r.end >= LEAD * me for k, r in relay.r.items() if k != "moejs"), "the plugin headline does not hold"
    ratios = [legs["qjs"][i] / legs["moejs"][i] for i in range(4)] + [counts["moejs"] / counts["qjs"]]
    tiles = StationTiles(list(zip(PLUGIN_NAMES, ratios)), TX["tiles_title"])
    tiles.beats = dict(tiles=tiles.dur - 0.6)
    meth = Method(TX["method_lines"])
    meth.beats = dict(method=meth.dur - 1.4)
    end = End()
    end.beats = dict(end=end.dur - 1.2)
    scenes = [hook, pure, gen, v, score, gw, pt, relay, tiles, meth, end]
    return Video(scenes), dict(v8=v, relay=relay, legs=legs, places=places, n=n, counts=counts, ratios=ratios,
                               scores=sc, targets=len(port["core.tsv"]), goos=goos, goarch=goarch,
                               refs=data.refs(root), root=root, keys=keys, lanes=ks)


def claims(v, x):
    """Every number the video shows, with its source, for the report."""
    r8, relay, legs = x["v8"], x["relay"], x["legs"]
    rows = []
    for i, (name, sc) in enumerate(zip(PLUGIN_NAMES, SCENARIOS)):
        row = dict(name=name, scenario=sc, leg_s={k: round(legs[k][i], 3) for k in x["keys"]}, place=x["places"][i])
        if i < 4:
            row.update(finish_s=row["leg_s"], requests=relay.r["moejs"].legs[i][4], check=x["refs"][sc],
                       lanes=x["lanes"][sc] + 1)
        else:
            sel = data.par8_runs(x["root"], x["keys"])
            row.update(workers=8, window_s=WINDOW, done=x["counts"], leg_requests=x["n"],
                       per_worker_requests={k: d["n"] for k, d in sel.items()},
                       start_skew_s={k: round(d["meta"]["skew_s"], 3) for k, d in sel.items()},
                       check=x["refs"]["agent100k"])
        rows.append(row)
    rate = lambda s: s.segs[0][1] if isinstance(s, Race) else None  # noqa: E731
    return dict(
        duration_s=round(v.dur, 3), frames=round(v.dur * FPS),
        builds=dict(targets=x["targets"], goos=x["goos"], goarch=x["goarch"], default_and_purego="all 0"),
        scenes=[dict(name=type(s).__name__, start=round(t, 2), dur=round(s.dur, 2), rate=rate(s))
                for s, t in zip(v.scenes, v.starts)],
        v8=dict(rate=r8.RATE, rate_segments=r8.segs, finish_s={k: round(r.end, 3) for k, r in r8.r.items()},
                leg_screen_s=[round(s, 2) for s in r8.leg_screen], lanes=x["lanes"]["fixed"] + 1,
                previous="v0.1.0-alpha.11", speedup=round(r8.speedup, 3), moejs_place=r8.rank()),
        scores=x["scores"], score_vs_qjs=round(x["scores"]["moejs"] / x["scores"]["qjs"], 3),
        score_lanes=x["lanes"]["score"] + 1, methods=TX["method_lines"],
        plugin_relay=dict(rate=relay.segs[0][1], rate_segments=relay.segs, legs=PLUGIN_NAMES,
                          total_s={k: round(r.end, 3) for k, r in relay.r.items()}, moejs_place=relay.rank(),
                          close_ratio=round(relay.ratio_first, 4), close_caption=relay.close),
        gateway=rows,
        tiles={n: round(r, 3) for n, r in zip(PLUGIN_NAMES, x["ratios"])},
        headlines=dict(general=TX["score"], plugin=TX["tiles_title"], end=[TX["end_head"], TX["end_sub"]]),
        beats={k: round(t, 2) for k, t in v.beats().items()})


def still(img, path):
    """A 1280x720 still of a 1080p frame."""
    img.resize((1280, 720), Image.LANCZOS).quantize(colors=128, method=Image.Quantize.MEDIANCUT,
                                                    dither=Image.Dither.NONE).save(path, optimize=True)


def main():
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("root", help="the recording of measure.py")
    ap.add_argument("out")
    ap.add_argument("--lang", choices=sorted(text.LANGS), default="zh")
    ap.add_argument("--stills")
    ap.add_argument("--at", help="comma-separated video times for --stills; default: the beats")
    ap.add_argument("--still", help="a still of one frame, by default the plugin relay's results")
    ap.add_argument("--claims")
    ap.add_argument("--check", type=float, help="render every CHECK seconds, then list shrunk and short captions")
    a = ap.parse_args()
    use(a.lang)
    v, x = build(a.root)
    if a.claims:
        Path(a.claims).write_text(json.dumps(claims(v, x), indent=1, ensure_ascii=False) + "\n")
    if a.check:
        for i in range(int(v.dur / a.check) + 1):
            v.frame(i * a.check)
        short, method = v.reading(a.check)
        print("duration", round(v.dur, 2), "scenes", [(type(s).__name__, round(t, 1), round(s.dur, 1))
                                                     for s, t in zip(v.scenes, v.starts)])
        print("shrunk", sorted(pix.SHRUNK))
        print("short", short)
        print("method", method, flush=True)
        return
    if a.still:
        t = float(a.at) if a.at else v.beats()["plug_times"]
        still(v.frame(t).convert("RGB").resize((W * S, H * S), Image.NEAREST), a.still)
        print("still", a.still, round(t, 2), os.path.getsize(a.still), "bytes", flush=True)
        return
    if a.stills:
        if a.at:
            ts = [(f"{float(s):.2f}s", float(s)) for s in a.at.split(",")]
        else:
            ts = sorted(v.beats().items(), key=lambda kv: kv[1])
        tw, th, cols = 640, 360, 4
        sheet = Image.new("RGB", (tw * cols, th * ((len(ts) + cols - 1) // cols)), (26, 28, 44))
        for i, (name, t) in enumerate(ts):
            im = v.frame(t).convert("RGB").resize((tw, th), Image.NEAREST)
            sheet.paste(im, ((i % cols) * tw, (i // cols) * th))
        sheet.save(a.stills)
        print("stills", len(ts), "duration", round(v.dur, 2), [(n, round(t, 2)) for n, t in ts], flush=True)
        print("shrunk", sorted(pix.SHRUNK), flush=True)
        return
    import imageio_ffmpeg
    tmpv, tmpa = a.out + ".video.mp4", a.out + ".audio.wav"
    w = imageio_ffmpeg.write_frames(tmpv, (W * S, H * S), fps=FPS, codec="libx264", macro_block_size=1,
                                    pix_fmt_out="yuv420p", quality=None,
                                    output_params=["-preset", "slow", "-crf", "18", "-threads", "6"],
                                    ffmpeg_log_level="error")
    w.send(None)
    hums, nf, t0 = [], round(v.dur * FPS), time.monotonic()
    for i in range(nf):
        T = i / FPS
        w.send(np.asarray(v.frame(T).convert("RGB").resize((W * S, H * S), Image.NEAREST)))
        k, t = v.at(T)
        hums.append((T, *v.scenes[k].hum(t)))
        if i % 600 == 0:
            print(f"frame {i}/{nf} {time.monotonic() - t0:.0f}s", flush=True)
    w.close()
    races = []
    for s, st in zip(v.scenes, v.starts):
        if isinstance(s, Race):
            races.append((st + s.lights[0], st + s.go))
    gains = audio.gains_for(v.dur, v.starts[1], races)
    rms = audio.write_mix(tmpa, v.dur, v.events(), hums, gains)
    ff = imageio_ffmpeg.get_ffmpeg_exe()
    subprocess.run([ff, "-y", "-loglevel", "error", "-i", tmpv, "-i", tmpa, "-c:v", "copy", "-c:a", "aac",
                    "-b:a", "192k", "-ac", "2", "-shortest", "-movflags", "+faststart", a.out], check=True)
    os.remove(tmpv)
    print("done", a.out, "dur", round(v.dur, 2), "frames", nf, "audio rms", round(rms, 3),
          f"{time.monotonic() - t0:.0f}s", flush=True)
    print("shrunk", sorted(pix.SHRUNK), flush=True)


if __name__ == "__main__":
    main()
