#!/usr/bin/env python3
"""before_after_bench.py measures moejs before and after a change: the same
benchmarks built from two trees, run in turn on the same CPUs, and appends
one JSON object per measurement to a JSONL file that
before_after_chart.py draws:

    python3 bench/scripts/before_after_bench.py -before DIR1 -after DIR2 -rounds 7 -o round.jsonl \\
        -before-date 2026-10-24 -after-date 2026-10-25 \\
        -canary 'cpu:248000:taskset -c 0-7 ./c-host -scenario small -workers 8 -dur 1'

Each DIR holds bench.test (go test -c in bench), webapi.test (go test -c
./webapi) and webapibench (go build ./cmd/webapibench in bench), built from
its tree. A tree that lacks webapi/global_bench_test.go gets a copy of it
before the build (before_after_bench.sh does both). The rows:

  - BenchmarkGlobalRead/shared/{webapi,bare} of package webapi: a loop
    iteration that reads the globals JSON, Math and Object, in a runtime
    with the Web APIs installed and in a bare one;
  - BenchmarkInstall/shared less BenchmarkNewRuntimeAndLoop/shared: what
    webapi.Install itself costs on a new runtime;
  - BenchmarkIntlSegmenter's graphemes, words and sentences, ns per segment;
  - BenchmarkSSEEvent: one server-sent event from an in-memory body,
    decoded and parsed;
  - the sse case of bench/cmd/webapibench/fetch-bench.js against the
    after tree's webapibench -serve on SERVER_CPUS, µs per event, the median
    of the script's own rounds.

Rounds work as in intl_webapi_bench.py: LOCK, a quiet machine, the
canaries before and after, odd rounds the before build first. A round
whose canary falls below its floor, or during which other processes used
more than a quarter of a core, goes to the .dropped file and runs again.
"""

import argparse
import fcntl
import json
import os
import re
import statistics
import subprocess
import sys
import time

from intl_webapi_bench import ENV, FOREIGN_MAX, TREE, Meter, canary, run

# The Go benchmarks: row, binary, -test.bench pattern, metric.
GO_ROWS = [
    ("global_read_webapi", "webapi.test", "^BenchmarkGlobalRead$/^shared$/^webapi$", "ns/op"),
    ("global_read_bare", "webapi.test", "^BenchmarkGlobalRead$/^shared$/^bare$", "ns/op"),
    ("install_total", "webapi.test", "^BenchmarkInstall$/^shared$", "ns/op"),
    ("runtime_loop", "webapi.test", "^BenchmarkNewRuntimeAndLoop$/^shared$", "ns/op"),
    ("seg_graphemes", "bench.test", "^BenchmarkIntlSegmenter$/^graphemes$/^moejs$", "ns/segment"),
    ("seg_words", "bench.test", "^BenchmarkIntlSegmenter$/^words$/^moejs$", "ns/segment"),
    ("seg_sentences", "bench.test", "^BenchmarkIntlSegmenter$/^sentences$/^moejs$", "ns/segment"),
    ("sse_memory", "webapi.test", "^BenchmarkSSEEvent$", "ns/op"),
]


def go_bench(a, build, binary, pattern, metric):
    cwd = os.path.join(TREE, "bench") if binary == "bench.test" else os.path.join(TREE, "webapi")
    out = run(["taskset", "-c", a.cpus, os.path.join(getattr(a, build), binary), "-test.run", "^$",
               "-test.bench", pattern, "-test.benchtime", a.benchtime],
              cwd=cwd, env=dict(ENV, BENCH_ENGINES="moejs"))
    for line in out.splitlines():
        m = re.match(r"Benchmark\S+\s+\d+\s+([\d.]+) ns/op(.*)", line)
        if m:
            if metric == "ns/op":
                return float(m[1])
            m2 = re.search(r"([\d.]+) " + re.escape(metric), m[2])
            return float(m2[1])
    raise RuntimeError(f"{binary} {pattern}: no result in {out[-1000:]}")


def sse_loopback(a, build):
    out = run(["taskset", "-c", a.cpus, os.path.join(getattr(a, build), "webapibench"), "-js", "fetch-bench.js",
               "-base", f"http://{a.server}", "-rounds", str(a.fetch_rounds), "-only", "sse"],
              cwd=os.path.join(TREE, "bench", "cmd", "webapibench"))
    got = [json.loads(l)["usPerUnit"] for l in out.splitlines() if l.startswith("{") and '"sse"' in l]
    if not got:
        raise RuntimeError(f"fetch-bench.js sse: {out[-1000:]}")
    return statistics.median(got), got


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("-before", required=True, help="directory of the before build's binaries")
    p.add_argument("-after", required=True, help="directory of the after build's binaries")
    p.add_argument("-before-date", required=True, help="the date the chart names the before build by")
    p.add_argument("-after-date", required=True, help="the date the chart names the after build by")
    p.add_argument("-o", required=True, help="the JSONL file to append to")
    p.add_argument("-rounds", type=int, default=7, help="rounds to keep")
    p.add_argument("-cpus", default="0-7", help="CPUs for the timed processes")
    p.add_argument("-server-cpus", default="8-13", help="CPUs for webapibench -serve")
    p.add_argument("-server", default="127.0.0.1:18091")
    p.add_argument("-benchtime", default="1s", help="-test.benchtime of the Go benchmarks")
    p.add_argument("-fetch-rounds", type=int, default=3, help="rounds of fetch-bench.js per run")
    p.add_argument("-lock", default=os.path.expanduser("~/moejs-bench/lock"))
    p.add_argument("-canary", action="append", default=[], help="NAME:FLOOR:SHELL-COMMAND printing \"rps\":N")
    a = p.parse_args()

    if not os.path.exists(a.o) or os.path.getsize(a.o) == 0:
        meta = {
            "before": a.before_date,
            "after": a.after_date,
            "go": run(["go", "version", os.path.join(a.after, "bench.test")]).split(": ", 1)[1].strip(),
            "cpu": next(l.split(":", 1)[1].strip() for l in open("/proc/cpuinfo") if l.startswith("model name")),
            "cpus": a.cpus,
            "canaries": [{"name": c.split(":")[0], "floor": float(c.split(":")[1])} for c in a.canary],
        }
        with open(a.o, "a") as f:
            f.write(json.dumps({"meta": meta}) + "\n")
    with open(a.o) as f:
        done = len({json.loads(l)["round"] for l in f if '"round"' in l})

    server = subprocess.Popen(["taskset", "-c", a.server_cpus, os.path.join(a.after, "webapibench"), "-serve", a.server],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1)
    meter = Meter(server.pid)
    lock = open(a.lock, "w")
    attempt = 0
    try:
        while done < a.rounds and attempt < 3 * a.rounds:
            attempt += 1
            rnd = done + 1
            order = ["before", "after"] if rnd % 2 else ["after", "before"]
            fcntl.flock(lock, fcntl.LOCK_EX)
            while True:
                m0 = meter.mark()
                time.sleep(5)
                f = meter.foreign(m0)
                if f < FOREIGN_MAX:
                    break
                print(f"{time.strftime('%T')} waiting: foreign {f:.2f} cores", flush=True)
                time.sleep(25)
            start = time.strftime("%T")
            m0 = meter.mark()
            rows, ok, why = [], True, []
            for spec in a.canary:
                name, v, good = canary(spec)
                rows.append({"canary": name, "when": "before", "value": v})
                if not good:
                    ok = False
                    why.append(f"{name} before {v:.0f}")
            got = {b: {} for b in order}
            for row, binary, pattern, metric in GO_ROWS:
                for b in order:
                    got[b][row] = go_bench(a, b, binary, pattern, metric)
            for b in order:
                got[b]["sse_loopback"], samples = sse_loopback(a, b)
                got[b]["sse_loopback_samples"] = samples
            for b in order:
                g = got[b]
                g["install_own"] = g["install_total"] - g["runtime_loop"]
                for row, v in g.items():
                    if row.endswith("_samples"):
                        continue
                    r = {"build": b, "row": row, "value": round(v, 3), "unit": "us" if row == "sse_loopback" else "ns"}
                    if row == "sse_loopback":
                        r["samples"] = g["sse_loopback_samples"]
                    rows.append(r)
            for spec in a.canary:
                name, v, good = canary(spec)
                rows.append({"canary": name, "when": "after", "value": v})
                if not good:
                    ok = False
                    why.append(f"{name} after {v:.0f}")
            f = meter.foreign(m0)
            fcntl.flock(lock, fcntl.LOCK_UN)
            if f > FOREIGN_MAX:
                ok = False
                why.append(f"foreign {f:.2f} cores")
            status = {"status": "kept" if ok else "dropped", "foreign": round(f, 3), "start": start,
                      "end": time.strftime("%T"), "why": why}
            out = a.o if ok else a.o + ".dropped"
            with open(out, "a") as fh:
                for r in rows + [status]:
                    fh.write(json.dumps(dict(round=rnd, **r)) + "\n")
            print(f"{start}-{status['end']} round {rnd} {status['status']} foreign {f:.2f} {' '.join(why)}", flush=True)
            if ok:
                done += 1
            else:
                time.sleep(30)
    finally:
        server.kill()
    print(f"done: {done} rounds kept", flush=True)
    sys.exit(0 if done >= a.rounds else 1)


if __name__ == "__main__":
    main()
