#!/usr/bin/env python3
"""A side-by-side sheet of two cuts at matching beats, 640x360 per frame
(phone size), the beat name above each pair: for example the zh cut
against the en cut, or a render against a published one. A time of
"-" leaves that side empty (a beat the cut lacks).

Usage: compare.py [--labels <left>,<right>] <left.mp4> <right.mp4> <out.png> <name>=<t_left>,<t_right> ..."""

import subprocess
import sys

import imageio_ffmpeg
import numpy as np
from PIL import Image, ImageDraw

import pix
from pix import P

TW, TH, BAR = 640, 360, 40
FF = imageio_ffmpeg.get_ffmpeg_exe()


def grab(path, t):
    raw = subprocess.run([FF, "-v", "error", "-ss", f"{t:.3f}", "-i", path, "-frames:v", "1", "-vf",
                          f"scale={TW}:{TH}:flags=area", "-f", "rawvideo", "-pix_fmt", "rgb24", "-"],
                         capture_output=True, check=True).stdout
    return Image.fromarray(np.frombuffer(raw, np.uint8).reshape(TH, TW, 3))


def label(img, s, x, y, colour):
    sp = pix.tx(s, colour, 2)
    img.paste(sp, (x - sp.width // 2, y), sp)


def main():
    args = sys.argv[1:]
    names = ["left", "right"]
    if args[0] == "--labels":
        names, args = args[1].split(","), args[2:]
    left, right, out = args[:3]
    rows = [(n, *[None if v == "-" else float(v) for v in ts.split(",")])
            for n, ts in (a.split("=") for a in args[3:])]
    sheet = Image.new("RGB", (2 * TW, BAR + len(rows) * (BAR + TH)), pix.rgb(P["ink"]))
    label(sheet, names[0], TW // 2, 6, P["grey"])
    label(sheet, names[1], TW + TW // 2, 6, P["orange"])
    for i, (name, tl, tr) in enumerate(rows):
        y = BAR + i * (BAR + TH)
        ImageDraw.Draw(sheet).rectangle((0, y, 2 * TW, y + BAR - 1), fill=P["dark"])
        label(sheet, name, TW, y + 6, P["yellow"])
        for k, (path, t) in enumerate(((left, tl), (right, tr))):
            if t is None:
                label(sheet, "—", k * TW + TW // 2, y + BAR + TH // 2 - 16, P["slate"])
            else:
                sheet.paste(grab(path, t), (k * TW, y + BAR))
    sheet.save(out)
    print("rows", len(rows), sheet.size)


if __name__ == "__main__":
    main()
