#!/usr/bin/env python3
"""intl_webapi_chart.py draws the charts of moejs's Intl, Web APIs and node:fs
against Node.js, intl-bench.{en,zh}.png, webapi-bench.{en,zh}.png and
nodefs-bench.{en,zh}.png, from the JSONL files of
bench/scripts/intl_webapi_bench.py (-nodefs is the node:fs chart's file,
measured with -sections nodefs): one bar per workload with moejs's speed
relative to Node.js on a log scale (Node.js is 1×, right of it moejs is
faster, left of it slower), and both absolute values on the row. Every value
is the median of the kept rounds, and a thin line spans the ratios of the
single rounds. --table prints the table of numbers in
Markdown instead (-lang zh for Chinese).

    uv run --with matplotlib bench/scripts/intl_webapi_chart.py docs/assets/intl-webapi-bench.jsonl \
        -nodefs docs/assets/nodefs-bench.jsonl
    python3 bench/scripts/intl_webapi_chart.py --table -lang zh docs/assets/intl-webapi-bench.jsonl

The text uses Nimbus Sans and Noto Sans CJK SC, as plugin_chart.py does.
MACHINE is the machine the subtitle describes. The subtitle ends with the
label "moejs, <date of the measurements>", which -label replaces.
"""

import argparse
import json
import math
import os
import statistics
import sys
from collections import defaultdict

MACHINE = {
    "en": "8 vCPUs of an Intel Xeon E5-2667 v4 (KVM guest), Debian 13",
    "zh": "Intel Xeon E5-2667 v4 的 8 个 vCPU（KVM 虚拟机），Debian 13",
}

# The colours and type of plugin_chart.py.
FONT = ["Nimbus Sans", "Noto Sans CJK SC"]
INK = "#0f172a"
SUB = "#475569"
MUTED = "#64748b"
AXIS = "#94a3b8"
GRID = "#e2e8f0"
BAND = "#eef2f6"
MOEJS = "#c2410c"
MOEJS_LABEL = "#9a3412"
REF = "#748196"
REF_LABEL = "#8a95a5"

# The rows of each chart by group: key, kind of value, English and Chinese
# label. A kind of "ns" or "us" is a time, lower is better, and "MBps" is a rate.
CHARTS = {
    "intl": [
        (("Formatting, per call", "格式化，每次调用"), [
            ("yen_format", "ns", "NumberFormat ja-JP yen: format", "NumberFormat ja-JP 日元：format"),
            ("yen_parts", "ns", "NumberFormat ja-JP yen: formatToParts", "NumberFormat ja-JP 日元：formatToParts"),
            ("decimal_format", "ns", "NumberFormat de-DE decimal: format", "NumberFormat de-DE 小数：format"),
            ("tokyo_format", "ns", "DateTimeFormat Asia/Tokyo: format", "DateTimeFormat Asia/Tokyo：format"),
            ("tokyo_parts", "ns", "DateTimeFormat Asia/Tokyo: formatToParts", "DateTimeFormat Asia/Tokyo：formatToParts"),
            ("number_tolocale", "ns", "(n).toLocaleString()", "(n).toLocaleString()"),
            ("date_tolocale", "ns", "date.toLocaleString()", "date.toLocaleString()"),
        ]),
        (("Creating a formatter or segmenter", "创建格式化器或分段器"), [
            ("construct_nf", "ns", "new Intl.NumberFormat (ja-JP yen)", "new Intl.NumberFormat（ja-JP 日元）"),
            ("construct_dtf", "ns", "new Intl.DateTimeFormat (Asia/Tokyo)", "new Intl.DateTimeFormat（Asia/Tokyo）"),
            ("construct_seg", "ns", "new Intl.Segmenter (zh-Hans graphemes)", "new Intl.Segmenter（zh-Hans 字素）"),
        ]),
        (("Intl.Segmenter over 64 KiB of mixed text, per segment", "Intl.Segmenter 切分 64 KiB 混合文本，每段"), [
            ("seg_graphemes", "ns", "Graphemes", "字素（grapheme）"),
            ("seg_words", "ns", "Words", "词（word）"),
            ("seg_sentences", "ns", "Sentences", "句（sentence）"),
        ]),
        (("First use in a fresh process", "新进程里的第一次调用"), [
            ("first_tolocale", "us", "First (1234.5).toLocaleString()", "第一次 (1234.5).toLocaleString()"),
        ]),
    ],
    "webapi": [
        (("fetch of a small JSON request, per request", "fetch 小 JSON 请求，每个请求"), [
            ("fetch_small1", "us", "1 at a time", "1 个并发"),
            ("fetch_small8", "us", "8 concurrent", "8 个并发"),
        ]),
        (("Streaming response bodies", "流式响应体"), [
            ("fetch_stream", "MBps", "256 MiB body read with getReader()", "getReader() 读 256 MiB 响应体"),
            ("fetch_sse", "us", "Server-sent events, decoded and parsed, per event", "SSE 事件，解码并 JSON.parse，每个事件"),
        ]),
    ],
    "nodefs": [
        (("Reading a file, per call", "读文件，每次调用"), [
            ("fs_readsync_4k_utf8", "us", "readFileSync 4 KiB, utf8", "readFileSync 4 KiB，utf8"),
            ("fs_readsync_4k_buf", "us", "readFileSync 4 KiB, Buffer", "readFileSync 4 KiB，Buffer"),
            ("fs_readsync_1m_utf8", "us", "readFileSync 1 MiB, utf8", "readFileSync 1 MiB，utf8"),
            ("fs_readsync_1m_buf", "us", "readFileSync 1 MiB, Buffer", "readFileSync 1 MiB，Buffer"),
            ("fs_read_4k_utf8", "us", "await readFile 4 KiB, utf8", "await readFile 4 KiB，utf8"),
            ("fs_read_4k_buf", "us", "await readFile 4 KiB, Buffer", "await readFile 4 KiB，Buffer"),
            ("fs_read_1m_utf8", "us", "await readFile 1 MiB, utf8", "await readFile 1 MiB，utf8"),
            ("fs_read_1m_buf", "us", "await readFile 1 MiB, Buffer", "await readFile 1 MiB，Buffer"),
        ]),
        (("Listing directories, per call", "列目录，每次调用"), [
            ("fs_readdirsync", "us", "readdirSync, 1,000 entries", "readdirSync，1,000 项"),
            ("fs_readdir", "us", "await readdir, 1,000 entries", "await readdir，1,000 项"),
            ("fs_readdirsync_recursive", "us", "readdirSync recursive, the tree", "readdirSync 递归列出整棵树"),
        ]),
        (("stat, per file", "stat，每个文件"), [
            ("fs_statsync", "us", "statSync", "statSync"),
            ("fs_stat", "us", "await stat", "await stat"),
        ]),
        (("glob, per call", "glob，每次调用"), [
            ("fs_globsync", "us", "globSync('**/*.ts'), the tree", "globSync('**/*.ts')，整棵树"),
        ]),
        (("Buffer, 64 KiB of binary data", "Buffer，64 KiB 二进制数据"), [
            ("buf_from_base64", "us", "Buffer.from(s, 'base64')", "Buffer.from(s, 'base64')"),
            ("buf_to_base64", "us", "buf.toString('base64')", "buf.toString('base64')"),
        ]),
    ],
}

TEXT = {
    "en": {
        "title": {"intl": "moejs Intl against Node.js", "webapi": "moejs Web APIs against Node.js",
                  "nodefs": "moejs node:fs against Node.js"},
        "subtitle": "{machine}, {go}, Node.js {node}, {label}.",
        "embed": "Engine a Go program can embed",
        "ref": "Reference: native, no Go",
        "moejs": "moejs",
        "node": "Node.js {node} (V8, ICU {icu}) = 1×",
        "node_nodefs": "Node.js {node} (V8, libuv) = 1×",
        "axis": "Speed relative to Node.js (log scale): right of 1× moejs is faster, left of it slower",
        "faster": "faster",
        "slower": "slower",
        "values": "moejs  vs  Node.js",
        "nodeline": "Node.js",
        "foot": {
            "intl": [
                "The same modules of bench/testdata run in both engines, with TZ=UTC and the en-US locale. Formatting rows format 100 values per call of the "
                "benchmark. Each figure is per value.",
                "Each engine runs a workload for at least 1 s per round. First use: the median of {first} fresh processes per round, timed from the call, after "
                "the runtime exists.",
            ],
            "webapi": [
                "The same script, bench/cmd/webapibench/fetch-bench.js, runs in moejs (package webapi over net/http) and in Node.js (its built-in fetch), "
                "against a local loopback server",
                "(Go net/http on 6 other vCPUs). A small request POSTs 80 B of JSON and parses a 280 B JSON reply: 2,000 at 1 concurrent, 4,000 at 8.",
                "Events: 20,000 of about 110 B, each flushed on its own, read with getReader, decoded and parsed. Each run's figure is the median of its {fr} rounds.",
                "Node.js's SSE time swings between about {nlo:.1f} and {nhi:.1f} µs per event from round to round, with how many events each read returns. "
                "moejs stays between {mlo:.1f} and {mhi:.1f} µs.",
            ],
            "nodefs": [
                "The same script, bench/cmd/nodefsbench/fs-bench.mjs, runs in moejs (package node) and in Node.js over the same tree on disk, in the page cache:",
                "a 4 KiB and a 1 MiB text file, a directory of 1,000 files, a tree of {tree:,} files in nested directories and 64 KiB of binary data.",
                "moejs runs it in a sandbox: every path resolves inside the tree's root through openat2 with RESOLVE_BENEATH (os.Root elsewhere), and the file",
                "system is read-only, the default. Each case runs about 1 s after a warm-up. moejs is slower on statSync, on readFileSync of 4 KiB and on base64.",
            ],
        },
        "method": ["Bars and numbers are the median of {n} rounds. The thin line spans the single rounds. Each round runs moejs and Node.js in turn on the",
                   "same 8 vCPUs, the first engine alternating, between canary runs that catch slowdowns from other guests on the host."],
    },
    "zh": {
        "title": {"intl": "moejs 的 Intl 对比 Node.js", "webapi": "moejs 的 Web API 对比 Node.js",
                  "nodefs": "moejs 的 node:fs 对比 Node.js"},
        "subtitle": "{machine}，{go}，Node.js {node}，{label}。",
        "embed": "Go 程序里能嵌入的引擎",
        "ref": "参考：原生，不经过 Go",
        "moejs": "moejs",
        "node": "Node.js {node}（V8，ICU {icu}）= 1×",
        "node_nodefs": "Node.js {node}（V8，libuv）= 1×",
        "axis": "相对 Node.js 的速度（对数刻度）：1× 右边 moejs 更快，左边更慢",
        "faster": "更快",
        "slower": "更慢",
        "values": "moejs  vs  Node.js",
        "nodeline": "Node.js",
        "foot": {
            "intl": [
                "两个引擎跑 bench/testdata 里的同一批模块，TZ=UTC，en-US 区域。格式化各行每次基准调用格式化 100 个值，图中数字是每个值的耗时。",
                "每轮里每个引擎每项至少跑 1 秒。第一次调用：每轮 {first} 个新进程取中位数，从调用开始计时，不含创建运行时。",
            ],
            "webapi": [
                "moejs（webapi 包，基于 net/http）和 Node.js（内置 fetch）跑同一个脚本 bench/cmd/webapibench/fetch-bench.js，"
                "服务端是本机回环地址上的 Go net/http，",
                "跑在另外 6 个 vCPU 上。小请求 POST 80 B 的 JSON 并解析 280 B 的 JSON 响应：1 并发 2,000 个，8 并发 4,000 个。",
                "SSE：20,000 个约 110 B 的事件，每个单独 flush，用 getReader 读、解码并 JSON.parse。每次运行取脚本内 {fr} 轮的中位数。",
                "Node.js 的 SSE 每个事件的耗时在各轮之间在约 {nlo:.1f} µs 和 {nhi:.1f} µs 之间跳动，取决于每次 read 拿到多少个事件。moejs 稳定在 {mlo:.1f} 到 {mhi:.1f} µs。",
            ],
            "nodefs": [
                "moejs（node 包）和 Node.js 跑同一个脚本 bench/cmd/nodefsbench/fs-bench.mjs，读磁盘上同一棵目录树，文件都在页缓存里：",
                "一个 4 KiB 和一个 1 MiB 的文本文件、一个有 1,000 个文件的目录、一棵 {tree:,} 个文件的多层目录树，以及 64 KiB 二进制数据。",
                "moejs 在沙箱里跑：每个路径都经 openat2 的 RESOLVE_BENEATH（其他系统上是 os.Root）限制在树根之内，文件系统是默认的只读。",
                "每项预热后跑约 1 秒。statSync、4 KiB 的 readFileSync 和 base64 是 moejs 更慢的几行。",
            ],
        },
        "method": ["条和数字是 {n} 轮的中位数，细线是各轮的范围。每轮在同样 8 个 vCPU 上轮流跑 moejs 和 Node.js，先后顺序每轮交替，",
                   "前后各跑一次金丝雀基准，排除宿主机上其他虚拟机造成的变慢。"],
    },
}


def load(paths):
    """load returns the meta line, the median value per (engine, row), the
    per-round values and the number of rounds. The engine is a record's
    "engine", or its "build" in the files of before_after_bench.py."""
    meta, vals = {}, defaultdict(dict)
    rounds = set()
    for path in paths:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                r = json.loads(line)
                if "meta" in r:
                    meta = r["meta"]
                elif "engine" in r or "build" in r:
                    vals[r.get("engine") or r["build"], r["row"]][r["round"]] = r["value"]
                    rounds.add(r["round"])
    med = {k: statistics.median(v.values()) for k, v in vals.items()}
    return meta, med, vals, len(rounds)


def ratio(kind, moejs, node):
    """ratio is how many times faster moejs is than Node.js."""
    return moejs / node if kind == "MBps" else node / moejs


def fmt_value(kind, v):
    if kind == "MBps":
        return f"{v / 1e3:.2f} GB/s" if v >= 1e3 else f"{v:,.0f} MB/s"
    ns = v * 1e3 if kind == "us" else v
    if ns >= 1e6:
        return f"{ns / 1e6:.1f} ms"
    if ns >= 1e5:
        return f"{ns / 1e3:,.0f} µs"
    if ns >= 1e3:
        return f"{ns / 1e3:.2f} µs" if ns < 1e4 else f"{ns / 1e3:.1f} µs"
    return f"{ns:,.0f} ns"


def fmt_ratio(r):
    if r >= 10:
        return f"{r:.0f}×"
    if r >= 0.995:
        return f"{r:.2f}×" if r < 1.1 else f"{r:.1f}×"
    return f"{r:.2f}×"


def label(meta, lang):
    """label is the subtitle's last item: -label, or moejs and the date of
    the measurements."""
    s = meta.get("label") or f"moejs, {meta.get('date', '')}"
    return s.replace(", ", "，") if lang == "zh" else s


def versions(meta):
    node, _, icu = meta.get("node", "").partition(" icu ")
    return node.lstrip("v"), icu, meta.get("go", "").replace("go1", "Go 1", 1)


def table(groups, med, vals, lang, head, engines=("moejs", "node")):
    """table returns the Markdown table of the rows of groups: the group, the
    row, both engines' medians, the speed and the per-round range."""
    a, b = engines
    out = ["| " + " | ".join(head) + " |", "|---|---|--:|--:|--:|--:|"]
    for title, rows in groups:
        for key, kind, en, zh in rows:
            m, d = med.get((a, key)), med.get((b, key))
            if m is None or d is None:
                continue
            per = per_round(kind, key, vals, engines)
            out.append(f"| {title[0 if lang == 'en' else 1]} | {en if lang == 'en' else zh} | {fmt_value(kind, m)} | {fmt_value(kind, d)} | "
                       f"{fmt_ratio(ratio(kind, m, d))} | {fmt_ratio(min(per))}–{fmt_ratio(max(per))} |")
    return "\n".join(out) + "\n"


TABLE_HEAD = {
    "en": ["Group", "Row", "moejs", "Node.js", "moejs speed", "per-round range"],
    "zh": ["组", "行", "moejs", "Node.js", "moejs 速度", "各轮范围"],
}

W, DPI = 3060, 150
ROW, GROUP_H = 84, 76  # pixels per row and per group header
TOP = 470  # top of the first group header
X_LABEL, X_AX0, X_AX1, X_VAL = 153, 1180, 2400, 2470


def per_round(kind, key, vals, engines=("moejs", "node")):
    """per_round returns the ratio of each round both engines ran."""
    m, d = vals[engines[0], key], vals[engines[1], key]
    return [ratio(kind, m[r], d[r]) for r in m if r in d]


def texts(chart, lang, meta, vals, n, first, fetch_rounds):
    """texts returns the words of a chart against Node.js for draw."""
    tx = TEXT[lang]
    node, icu, go = versions(meta)
    sse = {e: vals[e, "fetch_sse"].values() or [0] for e in ("moejs", "node")}
    args = dict(first=first, fr=fetch_rounds, n=n, nlo=min(sse["node"]), nhi=max(sse["node"]), mlo=min(sse["moejs"]),
                mhi=max(sse["moejs"]), tree=meta.get("tree_files", 0))
    return {
        "title": tx["title"][chart],
        "subtitle": tx["subtitle"].format(machine=MACHINE[lang], label=label(meta, lang), node=node, go=go),
        "embed": tx["embed"],
        "moejs": tx["moejs"],
        "ref": tx["ref"],
        "node": tx.get("node_" + chart, tx["node"]).format(node=node, icu=icu),
        "axis": tx["axis"],
        "values": tx["values"],
        "nodeline": tx["nodeline"],
        "faster": tx["faster"],
        "slower": tx["slower"],
        "foot": [line.format(**args) for line in tx["foot"][chart] + tx["method"]],
    }


def draw(groups, tx, lang, med, vals, out, engines=("moejs", "node")):
    """draw draws one chart of groups, with the words of tx, into out:
    engines[0]'s speed relative to engines[1]'s."""
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    from matplotlib.lines import Line2D
    from matplotlib.patches import Rectangle
    from matplotlib.ticker import FixedLocator, NullLocator

    plt.rcParams.update({"font.family": FONT, "axes.unicode_minus": False})
    a, b = engines

    # The rows' y, in pixels from the top, and the plot's extent.
    layout, y = [], TOP
    for title, rows in groups:
        layout.append(("group", y, title, None))
        y += GROUP_H
        for row in rows:
            layout.append(("row", y + ROW / 2, None, row))
            y += ROW
        y += 14
    plot_bottom = y + 10
    foot_y = plot_bottom + 120
    H = foot_y + len(tx["foot"]) * 33 + 40

    fig = plt.figure(figsize=(W / DPI, H / DPI), dpi=DPI)
    fig.patch.set_facecolor("white")

    def at(x, y):
        return x / W, 1 - y / H

    def text(x, y, s, **kw):
        kw.setdefault("va", "baseline")
        return fig.text(*at(x, y), s, **kw)

    text(153, 89, tx["title"], fontsize=29.5, fontweight="bold", color=INK)
    text(153, 145, tx["subtitle"], fontsize=14, color=SUB)

    # The legend: moejs, then the reference in its own group.
    renderer = fig.canvas.get_renderer()
    text(162, 243, tx["embed"], fontsize=14, fontweight="bold", color=INK)
    fig.patches.append(Rectangle(at(162, 284), 42 / W, 24 / H, transform=fig.transFigure, facecolor=MOEJS, edgecolor="none"))
    text(162 + 64, 282, tx["moejs"], fontsize=15, color="black")
    rx = 1100
    text(rx, 243, tx["ref"], fontsize=14, fontweight="bold", color=MUTED)
    fig.lines.append(Line2D([at(rx + 21, 0)[0]] * 2, [at(0, 258)[1], at(0, 290)[1]], transform=fig.transFigure,
                            color=REF, linewidth=2.2))
    text(rx + 64, 282, tx["node"], fontsize=15, color=MUTED)

    # The axis caption and the value column's header.
    text(X_AX0, 380, tx["axis"], fontsize=12.75, color=MUTED)
    text(X_VAL, 380, tx["values"], fontsize=12.75, color=MUTED)

    rs = [r for kind, _, _, row in layout if kind == "row" for r in per_round(row[1], row[0], vals, engines)]
    lo = min(0.5, min(rs) / 1.6)
    hi = max(2.0, max(rs) * 2.2)
    ax = fig.add_axes([X_AX0 / W, 1 - plot_bottom / H, (X_AX1 - X_AX0) / W, (plot_bottom - TOP) / H])
    ax.set_xscale("log")
    ax.set_xlim(lo, hi)
    ax.set_ylim(plot_bottom, TOP)
    ticks = [t for t in (0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 50, 100, 200) if lo <= t <= hi]
    ax.xaxis.set_major_locator(FixedLocator(ticks))
    ax.xaxis.set_minor_locator(NullLocator())
    ax.set_xticklabels([f"{t:g}×" for t in ticks])
    ax.tick_params(axis="x", labelsize=12, labelcolor=MUTED, color=AXIS, length=5, width=1)
    ax.set_yticks([])
    ax.grid(axis="x", color=GRID, linewidth=1, zorder=0.5)
    ax.set_axisbelow(True)
    for side in ("top", "right", "left"):
        ax.spines[side].set_visible(False)
    ax.spines["bottom"].set_color(AXIS)
    ax.set_facecolor("none")
    # The reference is the line at 1×, and the slower side carries a light band.
    ax.axvspan(lo, 1, color=BAND, zorder=0, linewidth=0)
    ax.axvline(1, color=REF, linewidth=2.2, zorder=3)
    ax.text(1, TOP + 6, tx["nodeline"], ha="center", va="bottom", fontsize=11.5, color=MUTED, zorder=4,
            transform=ax.transData)
    ax.text(lo * 1.04, TOP + 30, "← " + tx["slower"], ha="left", va="center", fontsize=11, color=MUTED)
    ax.text(hi / 1.04, TOP + 30, tx["faster"] + " →", ha="right", va="center", fontsize=11, color=MUTED)

    bar_h = ROW * 0.5
    for kind, y, title, row in layout:
        if kind == "group":
            text(X_LABEL + 3, y + 48, title[0 if lang == "en" else 1], fontsize=17, fontweight="bold", color=INK)
            continue
        key, unit, en, zh = row
        m, d = med[a, key], med[b, key]
        r = ratio(unit, m, d)
        text(X_LABEL + 3, y + 8, en if lang == "en" else zh, fontsize=14.5, color="black")
        ax.add_patch(Rectangle((min(1, r), y - bar_h / 2), max(r, 1) - min(r, 1), bar_h,
                               facecolor=MOEJS, edgecolor="none", zorder=2))
        rr = per_round(unit, key, vals, engines)
        cap = bar_h * 0.36
        ax.plot([min(rr), max(rr)], [y, y], color=INK, linewidth=1.3, zorder=3, solid_capstyle="butt")
        for x in (min(rr), max(rr)):
            ax.plot([x, x], [y - cap, y + cap], color=INK, linewidth=1.3, zorder=3)
        if r >= 1:
            ax.text(max(r, max(rr)) * 1.035, y, fmt_ratio(r), ha="left", va="center", fontsize=13.5, fontweight="bold",
                    color=MOEJS_LABEL)
        else:
            ax.text(min(r, min(rr)) / 1.035, y, fmt_ratio(r), ha="right", va="center", fontsize=13.5, fontweight="bold",
                    color=MOEJS_LABEL)
        t = text(X_VAL, y + 8, fmt_value(unit, m), fontsize=14, fontweight="bold", color=MOEJS_LABEL)
        x = X_VAL + t.get_window_extent(renderer).width + 14
        t = text(x, y + 8, "vs", fontsize=12.5, color=MUTED)
        x += t.get_window_extent(renderer).width + 14
        text(x, y + 8, fmt_value(unit, d), fontsize=14, color=SUB)

    for k, line in enumerate(tx["foot"]):
        text(153, foot_y + 33 * k, line, fontsize=12.6, color=MUTED)

    fig.savefig(out, dpi=DPI, facecolor="white")
    plt.close(fig)


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    here = os.path.dirname(os.path.abspath(__file__))
    assets = os.path.join(here, "..", "..", "docs", "assets")
    p.add_argument("results", nargs="*", default=[os.path.join(assets, "intl-webapi-bench.jsonl")])
    p.add_argument("-nodefs", default=os.path.join(assets, "nodefs-bench.jsonl"),
                   help="the JSONL of the node:fs chart, no chart if it doesn't exist")
    p.add_argument("-o", default=assets, help="directory for the PNGs")
    p.add_argument("-label", help='the subtitle\'s last item instead of "moejs, <date>", e.g. "moejs v0.1.0, 2026-10-24"')
    p.add_argument("--table", action="store_true", help="print the table of numbers in Markdown")
    p.add_argument("-lang", default="en", choices=["en", "zh"])
    p.add_argument("-first", type=int, default=10, help="fresh processes per round of the first-use row")
    p.add_argument("-fetch-rounds", type=int, default=3, help="rounds of fetch-bench.js per run")
    a = p.parse_args()
    sets = [(["intl", "webapi"], load(a.results))]
    if os.path.exists(a.nodefs):
        sets.append((["nodefs"], load([a.nodefs])))
    for charts, (meta, med, vals, n) in sets:
        if a.label:
            meta["label"] = a.label
        for chart in charts:
            if a.table:
                sys.stdout.write(table(CHARTS[chart], med, vals, a.lang, TABLE_HEAD[a.lang]))
                continue
            for lang in ("en", "zh"):
                tx = texts(chart, lang, meta, vals, n, a.first, a.fetch_rounds)
                draw(CHARTS[chart], tx, lang, med, vals, os.path.join(a.o, f"{chart}-bench.{lang}.png"))


if __name__ == "__main__":
    main()
