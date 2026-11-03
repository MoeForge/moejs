#!/usr/bin/env python3
"""before_after_chart.py draws moejs before and after a change,
before-after.{en,zh}.png, from the JSONL of bench/scripts/before_after_bench.py,
in the look of intl_webapi_chart.py: one bar per workload with the newer
build's speed relative to the older one on a log scale (the older build is
1×), both absolute values on the row, and a thin line over the ratios of
the single rounds. --table prints the table of numbers in Markdown instead.

    uv run --with matplotlib bench/scripts/before_after_chart.py bench/before-after/2026-10-25.jsonl -o DIR
    python3 bench/scripts/before_after_chart.py --table -lang zh bench/before-after/2026-10-25.jsonl
"""

import argparse
import os
import sys

from intl_webapi_chart import MACHINE, draw, load, table, versions

ENGINES = ("after", "before")

GROUPS = [
    (("Global variables, per loop iteration of 3 reads", "全局变量，循环一次读 3 个"), [
        ("global_read_webapi", "ns", "With the Web APIs installed", "装了 Web API"),
        ("global_read_bare", "ns", "In a bare runtime", "空运行时"),
    ]),
    (("Installing the Web APIs", "安装 Web API"), [
        ("install_total", "ns", "New runtime and loop, then webapi.Install", "新建运行时和事件循环，再 webapi.Install"),
        ("install_own", "ns", "webapi.Install's own part of it", "其中 webapi.Install 自身的部分"),
    ]),
    (("Intl.Segmenter over 64 KiB of mixed text, per segment", "Intl.Segmenter 切分 64 KiB 混合文本，每段"), [
        ("seg_graphemes", "ns", "Graphemes", "字素（grapheme）"),
        ("seg_words", "ns", "Words", "词（word）"),
        ("seg_sentences", "ns", "Sentences", "句（sentence）"),
    ]),
    (("Server-sent events, decoded and parsed, per event", "SSE 事件，解码并 JSON.parse，每个事件"), [
        ("sse_memory", "ns", "From an in-memory body", "响应体在内存里"),
        ("sse_loopback", "us", "From a loopback server", "来自本机回环服务端"),
    ]),
]

TEXT = {
    "en": {
        "title": "moejs before and after",
        "subtitle": "{machine}, {go}, moejs of {before} against moejs of {after}.",
        "embed": "After",
        "moejs": "moejs, {after}",
        "ref": "Before",
        "node": "moejs, {before} = 1×",
        "axis": "Speed relative to moejs of {before} (log scale): right of 1× the newer build is faster",
        "values": "{after}  vs  {before}",
        "faster": "faster",
        "slower": "slower",
        "foot": [
            "Both builds run the same benchmarks: BenchmarkGlobalRead, BenchmarkInstall less BenchmarkNewRuntimeAndLoop and BenchmarkSSEEvent of package",
            "webapi, BenchmarkIntlSegmenter of bench, and the SSE case of bench/cmd/webapibench/fetch-bench.js against a local Go server on 6 other vCPUs.",
            "The global-read benchmark is newer than the older build, which runs a copy of it. Each Go benchmark runs for 1 s per round.",
            "Over loopback the Go server's writes set the pace in most rounds, so the in-memory row shows the client's own cost better.",
            "Bars and numbers are the median of {n} rounds, and the thin line spans the single rounds. Each round runs both builds in turn on the same",
            "8 vCPUs, the first build alternating, between canary runs that catch slowdowns from other guests on the host.",
        ],
        "head": ["Group", "Row", "{after}", "{before}", "Speed", "per-round range"],
    },
    "zh": {
        "title": "moejs 前后对比",
        "subtitle": "{machine}，{go}，{before} 的 moejs 对比 {after} 的 moejs。",
        "embed": "之后",
        "moejs": "moejs，{after}",
        "ref": "之前",
        "node": "moejs，{before} = 1×",
        "axis": "相对 {before} 的 moejs 的速度（对数刻度）：1× 右边新版更快",
        "values": "{after}  vs  {before}",
        "faster": "更快",
        "slower": "更慢",
        "foot": [
            "两个版本跑同样的基准：webapi 包的 BenchmarkGlobalRead、BenchmarkInstall 减去 BenchmarkNewRuntimeAndLoop 和 BenchmarkSSEEvent，",
            "bench 的 BenchmarkIntlSegmenter，以及 bench/cmd/webapibench/fetch-bench.js 的 SSE 一项，服务端是另外 6 个 vCPU 上的本机 Go 服务。",
            "读全局变量的基准比旧版新，旧版跑的是它的一份拷贝。每个 Go 基准每轮跑 1 秒。",
            "回环测试里多数轮次的节奏由 Go 服务端的写入决定，所以内存里那一行更能反映客户端自身的开销。",
            "条和数字是 {n} 轮的中位数，细线是各轮的范围。每轮在同样 8 个 vCPU 上轮流跑两个版本，先后顺序每轮交替，",
            "前后各跑一次金丝雀基准，排除宿主机上其他虚拟机造成的变慢。",
        ],
        "head": ["组", "行", "{after}", "{before}", "速度", "各轮范围"],
    },
}


def texts(lang, meta, n):
    t = TEXT[lang]
    _, _, go = versions(meta)
    args = dict(machine=MACHINE[lang], go=go, before=meta["before"], after=meta["after"], n=n)
    tx = {k: (v.format(**args) if isinstance(v, str) else [s.format(**args) for s in v]) for k, v in t.items()}
    tx["nodeline"] = meta["before"]
    return tx


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("results", nargs="+")
    p.add_argument("-o", default=".", help="directory for the PNGs")
    p.add_argument("--table", action="store_true", help="print the table of numbers in Markdown")
    p.add_argument("-lang", default="en", choices=["en", "zh"])
    a = p.parse_args()
    meta, med, vals, n = load(a.results)
    if a.table:
        sys.stdout.write(table(GROUPS, med, vals, a.lang, texts(a.lang, meta, n)["head"], ENGINES))
        return
    for lang in ("en", "zh"):
        draw(GROUPS, texts(lang, meta, n), lang, med, vals, os.path.join(a.o, f"before-after.{lang}.png"), ENGINES)


if __name__ == "__main__":
    main()
