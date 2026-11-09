#!/usr/bin/env python3
"""plugin_chart.py draws the plugin scenario chart, docs/assets/plugin-bench.en.png
and plugin-bench.zh.png, from the matrix's results: one JSON object per line,
the throughput runs (engine, scenario, workers, rep, rps) and the in-use
memory runs at 32 workers (engine, scenario, peak_mib). Every cell is the
median of its runs. --tables prints the requests/s and memory tables of
docs/performance.md and docs/performance.zh_CN.md instead.

    uv run --no-project --with matplotlib==3.11.2 bench/scripts/plugin_chart.py docs/assets/plugin-bench.jsonl
    uv run --no-project --with matplotlib==3.11.2 bench/scripts/plugin_chart.py --tables docs/assets/plugin-bench.jsonl

The chart shows throughput at 8 workers and memory at 32; the tables carry
every cell. chartkit.py holds the look it shares with v8_chart.py. LABEL is
the moejs version and date the chart shows, MACHINE the machine it describes.
"""

import argparse
import json
import os
import statistics
import sys
from collections import defaultdict

LABEL = {"en": "moejs from the 2026-11-07 source, measured 2026-11-08", "zh": "moejs 为 2026-11-07 的源码，2026-11-08 测量"}
MACHINE = {
    "en": "Intel Xeon E5-2699A v4 KVM guest, 8 of 24 vCPUs, Debian 13, Go 1.26.6",
    "zh": "Intel Xeon E5-2699A v4 KVM 虚拟机，24 个 vCPU 中的 8 个，Debian 13，Go 1.26.6",
}

# key, English name, Chinese name: the column heads of the tables
ENGINES = [
    ("moejs-nocopy", "moejs (no copy)", "moejs（不拷贝）"),
    ("moejs", "moejs", "moejs"),
    ("sobek", "Sobek", "Sobek"),
    ("modernc", "QuickJS pure Go (modernc)", "QuickJS 纯 Go（modernc）"),
    ("quickjs-go", "quickjs-go (default)", "quickjs-go 默认"),
    ("quickjs-go-nocheck", "quickjs-go (check off)", "quickjs-go 关检查"),
]
REFS = [
    ("c-bellard", "QuickJS (C)", "QuickJS（C）"),
    ("c-ng-o3", "quickjs-ng (C)", "quickjs-ng（C）"),
]
WORKERS = [1, 8, 32]

# The chart's rows: key, style, then name and detail in English and Chinese.
ROWS = [
    ("moejs-nocopy", "moejs", ("moejs", "reads the caller's string"), ("moejs", "直接读调用方的字符串")),
    ("moejs", "moejs-light", ("moejs", "copies the input"), ("moejs", "先拷贝一份输入")),
    ("sobek", "other", ("Sobek", ""), ("Sobek", "")),
    ("modernc", "other", ("modernc", "QuickJS translated to Go"), ("modernc", "翻译成 Go 的 QuickJS")),
    ("quickjs-go", "other", ("quickjs-go", "default settings"), ("quickjs-go", "默认设置")),
    ("quickjs-go-nocheck", "other", ("quickjs-go", "goroutine check off"), ("quickjs-go", "关闭 goroutine 检查")),
    ("c-bellard", "ref", ("QuickJS (C)", ""), ("QuickJS（C）", "")),
    ("c-ng-o3", "ref", ("quickjs-ng (C)", ""), ("quickjs-ng（C）", "")),
]
# The chart shows throughput at 8 workers, the serving case, and memory with
# one 8 MiB image at 32 workers; the tables carry the rest.
CHART_WORKERS = 8
PANELS = ["small", "agent100k", "big8m", "memory"]

TEXT = {
    "en": {
        "title": "moejs on new-api's plugin workloads",
        "subtitle": "Requests per second at 8 workers, higher is better, and memory in use at 32 workers, lower is better. Median of 3 runs.",
        "setup": "{machine}. {label}.",
        "embed": "Engines a Go program can embed",
        "ref": "Reference: called from C, no Go",
        "panels": [
            ("Small requests", "requests/s"),
            ("Agent, 100k tokens", "requests/s"),
            ("One 8 MiB image", "requests/s"),
            ("Memory in use", "MiB, lower is better"),
        ],
        "foot": [
            "Small: 269 calls to 10 task plugins, about 700 B each. Agent: coding sessions, 350 KB in and out. Image: 25 MB in, 17 MB out.",
            "quickjs-go calls QuickJS through cgo. modernc has no in-use figure. docs/performance.md adds 1 and 32 workers and eight 1 MiB images.",
        ],
    },
    "zh": {
        "title": "moejs 跑 new-api 的插件负载",
        "subtitle": "8 并发下每秒处理的请求数，越高越好；32 并发时真正占用的内存，越低越好。3 轮取中位数。",
        "setup": "{machine}。{label}。",
        "embed": "Go 程序能嵌入的引擎",
        "ref": "参考：C 直接调用，不经过 Go",
        "panels": [
            ("小请求", "次/秒"),
            ("10 万 token agent", "次/秒"),
            ("单张 8 MiB 图", "次/秒"),
            ("占用的内存", "MiB，越低越好"),
        ],
        "foot": [
            "小请求：10 个任务插件的 269 次调用，每次约 700 B。agent：编码会话，进出各约 350 KB。图片：25 MB 进，17 MB 出。",
            "quickjs-go 通过 cgo 调用 QuickJS。modernc 统计不到占用的内存。1 并发、32 并发和 8 张 1 MiB 图的数字见 docs/performance.zh_CN.md。",
        ],
    },
}

TABLE = {
    "en": {
        "rps_head": ["Scenario", "Workers"],
        "mem_head": ["Scenario"],
        "scenarios": {
            "small": "Small requests",
            "agent100k": "Agent requests, 100k tokens",
            "big8m": "One 8 MiB image",
            "multi8x1m": "Eight 1 MiB images",
        },
    },
    "zh": {
        "rps_head": ["场景", "并发"],
        "mem_head": ["场景"],
        "scenarios": {
            "small": "小请求",
            "agent100k": "10 万 token agent 请求",
            "big8m": "单张 8 MiB 图",
            "multi8x1m": "8 张 1 MiB 图",
        },
    },
}


def load(paths):
    """load returns the median requests/s per (engine, scenario, workers) and
    the in-use MiB per (engine, scenario)."""
    runs = defaultdict(list)
    mem = defaultdict(list)
    for path in paths:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                r = json.loads(line)
                if r.get("failed"):
                    continue
                if "rps" in r:
                    runs[r["engine"], r["scenario"], r["workers"]].append(r["rps"])
                elif "peak_mib" in r:
                    mem[r["engine"], r["scenario"]].append(r["peak_mib"])
    rps = {k: statistics.median(v) for k, v in runs.items()}
    inuse = {k: statistics.median(v) for k, v in mem.items()}
    return rps, inuse


def cell(v, decimals):
    if v is None:
        return "—"
    if decimals and v < 100:
        return f"{v:.1f}"
    return f"{v:,.0f}"


def tables(rps, inuse):
    out = []
    for lang in ("en", "zh"):
        t = TABLE[lang]
        names = [e[1] if lang == "en" else e[2] for e in ENGINES + REFS]
        head = t["rps_head"] + names
        out.append("| " + " | ".join(head) + " |")
        out.append("|---|--:|" + "--:|" * len(names))
        for s in ("small", "agent100k", "big8m", "multi8x1m"):
            for w in WORKERS:
                vals = [cell(rps.get((e[0], s, w)), s in ("big8m", "multi8x1m")) for e in ENGINES + REFS]
                out.append(f"| {t['scenarios'][s]} | {w} | " + " | ".join(vals) + " |")
        out.append("")
        out.append("| " + " | ".join(t["mem_head"] + names) + " |")
        out.append("|---|" + "--:|" * len(names))
        for s in ("big8m", "multi8x1m"):
            vals = [cell(inuse.get((e[0], s)), False) for e in ENGINES + REFS]
            out.append(f"| {t['scenarios'][s]} | " + " | ".join(vals) + " |")
        out.append("")
    return "\n".join(out)


def value(panel, v, lang):
    """value formats a bar's number the way the panel's end labels show it."""
    if v is None:
        return "—"
    if panel == "small":
        if lang == "zh":
            return f"{v / 1e4:.1f}万" if v >= 1e4 else f"{v:,.0f}"
        return f"{v / 1e3:.0f}k" if v >= 1e4 else f"{v / 1e3:.1f}k"
    if panel == "big8m" and v < 100:
        return f"{v:.1f}"
    return f"{v:,.0f}"


def draw(lang, rps, inuse, out):
    from chartkit import BASELINE, INK, MUTED, PAD, RULE, SECONDARY, WIDTH, Canvas

    tx = TEXT[lang]
    side = 1 if lang == "zh" else 0
    pitch, thick = 30, 14
    go_rows = [r for r in ROWS if r[1] != "ref"]
    ref_rows = [r for r in ROWS if r[1] == "ref"]

    # Vertical layout, in CSS pixels.
    y_title = PAD + 17
    y_sub = y_title + 22
    y_setup = y_sub + 17
    y_head = y_setup + 34
    y_unit = y_head + 15
    y_top = y_unit + 12
    y_go = y_top + 16
    y_rows = y_go + 4
    y_split = y_rows + pitch * len(go_rows) + 6
    y_ref = y_split + 16
    y_refrows = y_ref + 4
    y_end = y_refrows + pitch * len(ref_rows)
    y_foot = y_end + 26
    height = y_foot + 16 * (len(tx["foot"]) - 1) + PAD - 4
    c = Canvas(height)
    full = WIDTH - 2 * PAD

    c.text(PAD, y_title, tx["title"], size=19, weight="bold", fit=full)
    c.text(PAD, y_sub, tx["subtitle"], size=12, color=SECONDARY, fit=full)
    c.text(PAD, y_setup, tx["setup"].format(machine=MACHINE[lang], label=LABEL[lang]), size=11.5, color=MUTED, fit=full)

    # The label column is as wide as its widest name or detail.
    names = [r[2 + side] for r in ROWS]
    label_w = max(max(c.width(n, 12.5, "bold" if n == "moejs" else "normal"), c.width(d, 11)) for n, d in names)
    x_panels = PAD + label_w + 14
    gap = 20
    panel_w = (WIDTH - PAD - x_panels - gap * (len(PANELS) - 1)) / len(PANELS)

    def centre(i):
        if i < len(go_rows):
            return y_rows + pitch * i + pitch / 2
        return y_refrows + pitch * (i - len(go_rows)) + pitch / 2

    c.rule(PAD, y_top, WIDTH - PAD, y_top)
    c.text(PAD, y_go, tx["embed"], size=11, color=SECONDARY, weight="bold")
    c.rule(PAD, y_split, WIDTH - PAD, y_split)
    c.text(PAD, y_ref, tx["ref"], size=11, color=SECONDARY, weight="bold")
    for i, (key, style, en, zh) in enumerate(ROWS):
        name, detail = (en, zh)[side]
        y = centre(i)
        moejs = key.startswith("moejs")
        if detail:
            c.text(PAD, y - 1.5, name, size=12.5, weight="bold" if moejs else "normal", color=INK)
            c.text(PAD, y + 11, detail, size=11, color=MUTED)
        else:
            c.text(PAD, y + 4.5, name, size=12.5, color=INK)

    for p, panel in enumerate(PANELS):
        x0 = x_panels + p * (panel_w + gap)
        head, unit = tx["panels"][p]
        c.text(x0, y_head, head, size=12.5, weight="bold", fit=panel_w)
        c.text(x0, y_unit, unit, size=11, color=MUTED, fit=panel_w)
        if panel == "memory":
            vals = [inuse.get((r[0], "big8m")) for r in ROWS]
        else:
            vals = [rps.get((r[0], panel, CHART_WORKERS)) for r in ROWS]
        labels = [value(panel, v, lang) for v in vals]
        room = max(c.width(s, 11.5, "bold") for s in labels) + 6
        scale = (panel_w - room) / max(v for v in vals if v is not None)
        for y0, y1 in ((y_rows, y_split - 6), (y_refrows, y_end)):
            c.rule(x0, y0 + 6, x0, y1 - 6, color=BASELINE)
        for i, (key, style, _, _) in enumerate(ROWS):
            v, y = vals[i], centre(i)
            moejs = key.startswith("moejs")
            if v is not None:
                c.bar(x0, y, v * scale, thick, style)
            end = x0 + (v or 0) * scale + 5
            c.text(end, y, labels[i], size=11.5, va="center_baseline",
                   weight="bold" if moejs else "normal", color=INK if moejs else SECONDARY)

    for k, line in enumerate(tx["foot"]):
        c.text(PAD, y_foot + 16 * k, line, size=11, color=MUTED, fit=full)
    c.save(out)

def main():
    here = os.path.dirname(os.path.abspath(__file__))
    assets = os.path.join(here, "..", "..", "docs", "assets")
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("results", nargs="*", default=[os.path.join(assets, "plugin-bench.jsonl")])
    p.add_argument("-o", default=assets, help="directory for plugin-bench.en.png and plugin-bench.zh.png")
    p.add_argument("--tables", action="store_true", help="print the tables of docs/performance.md")
    a = p.parse_args()
    rps, inuse = load(a.results)
    if a.tables:
        sys.stdout.write(tables(rps, inuse))
        return
    for lang in ("en", "zh"):
        draw(lang, rps, inuse, os.path.join(a.o, f"plugin-bench.{lang}.png"))


if __name__ == "__main__":
    main()
