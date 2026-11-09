#!/usr/bin/env python3
"""v8_chart.py draws the V8 benchmark suite v7 chart, docs/assets/v8-bench.en.png
and v8-bench.zh.png, from docs/assets/v8-bench.json: each engine's total
score and the scores of the eight programs, from the run with the median
total of its three. It checks that every total is the geometric mean of its
programs first. --table prints the per-program tables of docs/performance.md
and docs/performance.zh_CN.md instead.

    uv run --no-project --with matplotlib==3.11.2 bench/scripts/v8_chart.py
    uv run --no-project --with matplotlib==3.11.2 bench/scripts/v8_chart.py --table

chartkit.py holds the look it shares with plugin_chart.py.
"""

import argparse
import json
import math
import os
import sys

TEXT = {
    "en": {
        "title": "V8 benchmark suite v7",
        "subtitle": "Eight classic JavaScript programs; the total is their geometric mean, higher is better. "
                    "Xeon E5-2699A v4 KVM guest, 2026-11-07.",
        "embed": "Engines a Go program can embed",
        "ref": "Reference: called from C, no Go",
        "total": "Total score",
        "ratio": "{x:.2f}× QuickJS (C)",
        "programs": "Score per program",
        "scale": "one scale for all eight",
        "foot": [
            "Each engine runs single-threaded on its own pinned vCPU, median of 3 runs. Go 1.27.2; moejs from the "
            "2026-11-07 source, built with -tags purego.",
            "QuickJS (C) is release 2026-06-04, run by its qjs shell. Versions and every score: docs/performance.md.",
        ],
        "head": ["Program"],
        "row_total": "Total",
    },
    "zh": {
        "title": "V8 benchmark suite v7",
        "subtitle": "V8 的经典测试集：8 个 JavaScript 程序，总分是它们得分的几何平均，越高越好。"
                    "Xeon E5-2699A v4 KVM 虚拟机，2026-11-07。",
        "embed": "Go 程序能嵌入的引擎",
        "ref": "参考：C 直接调用，不经过 Go",
        "total": "总分",
        "ratio": "QuickJS（C）的 {x:.2f} 倍",
        "programs": "各程序得分",
        "scale": "8 张小图用同一比例",
        "foot": [
            "每个引擎单线程跑在自己绑定的 vCPU 上，3 轮取中位数。Go 1.27.2，moejs 为 2026-11-07 的源码，"
            "用 -tags purego 编译。",
            "QuickJS（C）是 2026-06-04 版本，用它自带的 qjs 运行。各引擎版本和每项得分见 docs/performance.zh_CN.md。",
        ],
        "head": ["程序"],
        "row_total": "总分",
    },
}


def name(e, lang):
    return e["name"].replace(" (C)", "（C）") if lang == "zh" else e["name"]


def load(path):
    """load reads the scores and checks every total against the geometric
    mean of its programs; the suite prints scores above 100 as integers."""
    with open(path) as f:
        data = json.load(f)
    for e in data["engines"]:
        progs = [e["programs"][p] for p in data["programs"]]
        geo = math.exp(sum(math.log(v) for v in progs) / len(progs))
        if abs(geo - e["score"]) > 1:
            raise SystemExit(f"{e['name']}: total {e['score']} is not the geometric mean {geo:.1f} of its programs")
    return data


def fmt(v):
    return f"{v:,.0f}" if v >= 100 else f"{v:.3g}"


def table(data):
    out = []
    engines = data["engines"]
    for lang in ("en", "zh"):
        tx = TEXT[lang]
        out.append("| " + " | ".join(tx["head"] + [name(e, lang) for e in engines]) + " |")
        out.append("|---|" + "--:|" * len(engines))
        for p in data["programs"]:
            out.append(f"| {p} | " + " | ".join(fmt(e["programs"][p]) for e in engines) + " |")
        out.append(f"| **{tx['row_total']}** | " + " | ".join(f"**{fmt(e['score'])}**" for e in engines) + " |")
        out.append("")
    return "\n".join(out)


def draw(lang, data, out):
    from chartkit import BASELINE, INK, MUTED, PAD, SECONDARY, WIDTH, Canvas

    tx = TEXT[lang]
    engines = data["engines"]
    go = [e for e in engines if not e["reference"]]
    refs = [e for e in engines if e["reference"]]
    order = go + refs
    style = ["moejs" if e["name"] == "moejs" else "ref" if e["reference"] else "other" for e in order]
    pitch, thick = 24, 14
    mpitch, mthick, mgap = 14, 8, 6

    # Vertical layout, in CSS pixels.
    y_title = PAD + 17
    y_sub = y_title + 22
    y_head = y_sub + 34
    y_top = y_head + 12
    y_go = y_top + 16
    y_rows = y_go + 4
    y_split = y_rows + pitch * len(go) + 6
    y_ref = y_split + 16
    y_refrows = y_ref + 4
    y_end = y_refrows + pitch * len(refs)
    y_phead = y_end + 36
    y_ptop = y_phead + 12
    grid_h = 20 + mpitch * len(go) + mgap + mpitch * len(refs)
    y_grid = [y_ptop + 6, y_ptop + 6 + grid_h + 14]
    y_foot = y_grid[1] + grid_h + 30
    height = y_foot + 16 * (len(tx["foot"]) - 1) + PAD - 4
    c = Canvas(height)
    full = WIDTH - 2 * PAD

    c.text(PAD, y_title, tx["title"], size=19, weight="bold", fit=full)
    c.text(PAD, y_sub, tx["subtitle"], size=12, color=SECONDARY, fit=full)

    label_w = max(c.width(name(e, lang), 12.5, "bold" if e["name"] == "moejs" else "normal") for e in order)
    x_panels = PAD + label_w + 14

    # The total scores.
    def centre(i):
        if i < len(go):
            return y_rows + pitch * i + pitch / 2
        return y_refrows + pitch * (i - len(go)) + pitch / 2

    c.text(x_panels, y_head, tx["total"], size=12.5, weight="bold")
    c.rule(PAD, y_top, WIDTH - PAD, y_top)
    c.text(PAD, y_go, tx["embed"], size=11, color=SECONDARY, weight="bold")
    c.rule(PAD, y_split, WIDTH - PAD, y_split)
    c.text(PAD, y_ref, tx["ref"], size=11, color=SECONDARY, weight="bold")
    for y0, y1 in ((y_rows, y_split - 6), (y_refrows, y_end)):
        c.rule(x_panels, y0 + 6, x_panels, y1 - 6, color=BASELINE)
    moejs = order[0]
    qjs = next(e for e in refs if e["name"] == "QuickJS (C)")
    ratio = tx["ratio"].format(x=moejs["score"] / qjs["score"])
    room = c.width(fmt(moejs["score"]), 12, "bold") + c.width(ratio, 11) + 16
    scale = (WIDTH - PAD - x_panels - room) / max(e["score"] for e in order)
    for i, e in enumerate(order):
        y = centre(i)
        bold = style[i] == "moejs"
        c.text(PAD, y + 4.5, name(e, lang), size=12.5, weight="bold" if bold else "normal", color=INK)
        c.bar(x_panels, y, e["score"] * scale, thick, style[i])
        end = x_panels + e["score"] * scale + 5
        c.text(end, y, fmt(e["score"]), size=12, va="center_baseline",
                   weight="bold" if bold else "normal", color=INK if bold else SECONDARY)
        if bold:
            c.text(end + c.width(fmt(e["score"]), 12, "bold") + 8, y, ratio, size=11, va="center_baseline", color=MUTED)

    # The eight programs, four to a row, on one scale.
    c.text(x_panels, y_phead, tx["programs"], size=12.5, weight="bold")
    c.text(x_panels + c.width(tx["programs"], 12.5, "bold") + 10, y_phead, tx["scale"], size=11, color=MUTED)
    c.rule(PAD, y_ptop, WIDTH - PAD, y_ptop)
    cols, gap = 4, 18
    panel_w = (WIDTH - PAD - x_panels - gap * (cols - 1)) / cols
    top = max(e["programs"][p] for e in order for p in data["programs"])
    mroom = c.width(fmt(top), 10.5, "bold") + 5
    mscale = (panel_w - mroom) / top

    def mcentre(g, i):
        y = y_grid[g] + 20 + mpitch * i + mpitch / 2
        return y + (mgap if i >= len(go) else 0)

    for g in range(2):
        for i, e in enumerate(order):
            bold = style[i] == "moejs"
            c.text(PAD, mcentre(g, i), name(e, lang), size=11, va="center_baseline",
                   weight="bold" if bold else "normal", color=INK if bold else SECONDARY)
    for k, p in enumerate(data["programs"]):
        g, col = divmod(k, cols)
        x0 = x_panels + col * (panel_w + gap)
        c.text(x0, y_grid[g] + 11, p, size=11.5, weight="bold", color=INK, fit=panel_w)
        last = len(go) - 1
        c.rule(x0, mcentre(g, 0) - mpitch / 2 + 2, x0, mcentre(g, last) + mpitch / 2 - 2, color=BASELINE)
        c.rule(x0, mcentre(g, last + 1) - mpitch / 2 + 2, x0, mcentre(g, len(order) - 1) + mpitch / 2 - 2,
               color=BASELINE)
        for i, e in enumerate(order):
            v, y = e["programs"][p], mcentre(g, i)
            c.bar(x0, y, v * mscale, mthick, style[i])
            # Only moejs and the C reference carry their numbers here; the
            # table in docs/performance.md has every score.
            if style[i] in ("moejs", "ref"):
                bold = style[i] == "moejs"
                c.text(x0 + v * mscale + 4, y, fmt(v), size=10.5, va="center_baseline",
                       weight="bold" if bold else "normal", color=INK if bold else SECONDARY)

    for k, line in enumerate(tx["foot"]):
        c.text(PAD, y_foot + 16 * k, line, size=11, color=MUTED, fit=full)
    c.save(out)


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    assets = os.path.join(here, "..", "..", "docs", "assets")
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("data", nargs="?", default=os.path.join(assets, "v8-bench.json"))
    p.add_argument("-o", default=assets, help="directory for v8-bench.en.png and v8-bench.zh.png")
    p.add_argument("--table", action="store_true", help="print the per-program tables of docs/performance.md")
    a = p.parse_args()
    data = load(a.data)
    if a.table:
        sys.stdout.write(table(data))
        return
    for lang in ("en", "zh"):
        draw(lang, data, os.path.join(a.o, f"v8-bench.{lang}.png"))


if __name__ == "__main__":
    main()
