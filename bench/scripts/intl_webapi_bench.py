#!/usr/bin/env python3
"""intl_webapi_bench.py measures the rows of the Intl and Web API chart
(intl_webapi_chart.py) in moejs and in Node.js with the same scripts on both
sides, and appends one JSON object per measurement to a JSONL file:

    python3 bench/scripts/intl_webapi_bench.py -bin DIR -node NODE -rounds 7 -o intl-webapi-bench.jsonl \\
        -canary 'cpu:248000:taskset -c 0-7 ./c-host -scenario small -workers 8 -dur 1'

DIR holds bench.test (go test -c in bench), webapibench, intlfirst and
nodefsbench (go build ./cmd/webapibench ./cmd/intlfirst ./cmd/nodefsbench in
bench). The rows:

  - BenchmarkIntlFormat and BenchmarkIntlSegmenter for moejs, against
    bench/scripts/intl_format_node.mjs and intl_node.mjs, which run the same
    modules of bench/testdata in Node; ns per value, segment or construction;
  - the first toLocaleString in a fresh process, bench/cmd/intlfirst against
    bench/scripts/intl_first_node.mjs, the median of FIRST processes;
  - bench/cmd/webapibench/fetch-bench.js in moejs (webapibench -js) and in
    Node against webapibench -serve on SERVER_CPUS: small JSON requests at 1
    and 8 concurrent, a 256 MiB body read with getReader, and server-sent
    events; the median of the script's own rounds;
  - bench/cmd/nodefsbench/fs-bench.mjs in moejs (nodefsbench -js, package
    node with a read-only file system confined to the fixture) and in Node,
    over the fixture tree of nodefsbench -fixture in FIXTURE: node:fs reads,
    directory listings, stat, glob and Buffer's base64; µs per call.

-sections runs a subset of the sections: format, segmenter, first, fetch
and nodefs, the rows above in that order. The node:fs chart has its own
file: -sections nodefs -o nodefs-bench.jsonl. Each round takes LOCK, waits until
other processes use less than a quarter of a core, runs the canaries, then
every workload with moejs and Node in turn (odd rounds moejs first), then
the canaries again. A round whose canary falls below its floor, or during
which other processes used more than a quarter of a core, goes to the
.dropped file and runs again. Both engines run with
TZ=UTC and LC_ALL=en_US.UTF-8, pinned to CPUS.
"""

import argparse
import datetime
import fcntl
import json
import os
import re
import statistics
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
TREE = os.path.normpath(os.path.join(HERE, "..", ".."))
ENV = dict(os.environ, TZ="UTC", LC_ALL="en_US.UTF-8", LANG="en_US.UTF-8", LANGUAGE="")
FOREIGN_MAX = 0.25  # cores

FORMAT = {
    "yenFormat": "yen_format",
    "yenParts": "yen_parts",
    "decimalFormat": "decimal_format",
    "tokyoFormat": "tokyo_format",
    "tokyoParts": "tokyo_parts",
    "numberToLocale": "number_tolocale",
    "dateToLocale": "date_tolocale",
    "constructNumber": "construct_nf",
    "constructDate": "construct_dtf",
}
SEGMENTER = {
    "graphemes": "seg_graphemes",
    "words": "seg_words",
    "sentences": "seg_sentences",
    "construct": "construct_seg",
}
FETCH = {"small1": "fetch_small1", "small8": "fetch_small8", "stream": "fetch_stream", "sse": "fetch_sse"}
NODEFS = {
    "readFileSync 4KiB utf8": "fs_readsync_4k_utf8",
    "readFileSync 4KiB Buffer": "fs_readsync_4k_buf",
    "readFileSync 1MiB utf8": "fs_readsync_1m_utf8",
    "readFileSync 1MiB Buffer": "fs_readsync_1m_buf",
    "await readFile 4KiB utf8": "fs_read_4k_utf8",
    "await readFile 4KiB Buffer": "fs_read_4k_buf",
    "await readFile 1MiB utf8": "fs_read_1m_utf8",
    "await readFile 1MiB Buffer": "fs_read_1m_buf",
    "readdirSync 1000 entries": "fs_readdirsync",
    "await readdir 1000 entries": "fs_readdir",
    "readdirSync recursive tree": "fs_readdirsync_recursive",
    "statSync x1000": "fs_statsync",
    "await stat x1000": "fs_stat",
    "globSync **/*.ts tree": "fs_globsync",
    "Buffer.from base64 64KiB": "buf_from_base64",
    "toString base64 64KiB": "buf_to_base64",
}
FIRST_SCRIPT = "(1234.5).toLocaleString()"


def run(cmd, cwd=None, env=ENV):
    p = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)
    if p.returncode != 0:
        raise RuntimeError(f"{cmd[:3]} exited {p.returncode}: {p.stderr[-2000:]}")
    return p.stdout


class Bench:
    def __init__(self, a):
        self.a = a
        self.taskset = ["taskset", "-c", a.cpus]

    def go_bench(self, family, names):
        pattern = f"^{family}$/^({'|'.join(names)})$/^moejs$"
        out = run(self.taskset + [os.path.join(self.a.bin, "bench.test"), "-test.run", "^$", "-test.bench", pattern,
                                  "-test.benchtime", "1s"],
                  cwd=os.path.join(TREE, "bench"), env=dict(ENV, BENCH_ENGINES="moejs"))
        rows = {}
        for line in out.splitlines():
            m = re.match(rf"{family}/(\w+)/moejs(?:-\d+)?\s+\d+\s+([\d.]+) ns/op(?:.*?\s([\d.]+) ns/(?:value|segment))?", line)
            if m:
                rows[m[1]] = float(m[3] if m[3] else m[2])
        return rows

    def node_script(self, script, per):
        out = run(self.taskset + [self.a.node, script], cwd=TREE)
        rows = {}
        for line in out.splitlines():
            m = re.match(rf"(\w+)\s+([\d.]+) ns/op(?:\t.*?([\d.]+) ns/{per})?", line)
            if m:
                rows[m[1]] = float(m[3] if m[3] else m[2])
        return rows

    def intl_format(self, engine):
        if engine == "moejs":
            got = self.go_bench("BenchmarkIntlFormat", list(FORMAT))
        else:
            got = self.node_script("bench/scripts/intl_format_node.mjs", "value")
        return [(FORMAT[k], got[k], "ns") for k in FORMAT]

    def segmenter(self, engine):
        if engine == "moejs":
            got = self.go_bench("BenchmarkIntlSegmenter", list(SEGMENTER))
        else:
            got = self.node_script("bench/scripts/intl_node.mjs", "segment")
        return [(SEGMENTER[k], got[k], "ns") for k in SEGMENTER]

    def first_use(self, engines):
        cmds = {
            "moejs": self.taskset + [os.path.join(self.a.bin, "intlfirst"), FIRST_SCRIPT],
            "node": self.taskset + [self.a.node, "bench/scripts/intl_first_node.mjs", FIRST_SCRIPT],
        }
        samples = {e: [] for e in engines}
        for _ in range(self.a.first):
            for e in engines:
                samples[e].append(float(run(cmds[e], cwd=TREE).split()[0]))
        return {e: [("first_tolocale", statistics.median(s), "us", s)] for e, s in samples.items()}

    def fetch(self, engine):
        d = os.path.join(TREE, "bench", "cmd", "webapibench")
        base = f"http://{self.a.server}"
        if engine == "moejs":
            cmd = self.taskset + [os.path.join(self.a.bin, "webapibench"), "-js", "fetch-bench.js", "-base", base,
                                  "-rounds", str(self.a.fetch_rounds)]
            env = ENV
        else:
            cmd = self.taskset + [self.a.node, "fetch-bench.js"]
            env = dict(ENV, BASE=base, ROUNDS=str(self.a.fetch_rounds))
        out = run(cmd, cwd=d, env=env)
        if "ERROR" in out:
            raise RuntimeError(f"fetch-bench.js in {engine}: {out[-2000:]}")
        got = {}
        for line in out.splitlines():
            if line.startswith("{"):
                r = json.loads(line)
                got.setdefault(r["name"], []).append(r["MBps"] if r["name"] == "stream" else r["usPerUnit"])
        rows = []
        for k, row in FETCH.items():
            unit = "MBps" if k == "stream" else "us"
            rows.append((row, statistics.median(got[k]), unit, got[k]))
        return rows

    def nodefs(self, engine):
        d = os.path.join(TREE, "bench", "cmd", "nodefsbench")
        if engine == "moejs":
            cmd = self.taskset + [os.path.join(self.a.bin, "nodefsbench"), "-js", "fs-bench.mjs", "-dir", self.a.fixture]
        else:
            cmd = self.taskset + [self.a.node, "fs-bench.mjs", self.a.fixture]
        got = {}
        for line in run(cmd, cwd=d).splitlines():
            if line.startswith("{"):
                r = json.loads(line)
                got[r["name"]] = r["us"]
        return [(row, got[k], "us") for k, row in NODEFS.items()]


def busy():
    with open("/proc/stat") as f:
        v = f.readline().split()[1:]
    return sum(int(v[i]) for i in (0, 1, 2, 5, 6))


def cpu_of(pid, children):
    with open(f"/proc/{pid}/stat") as f:
        v = f.read().rsplit(")", 1)[1].split()
    # utime stime cutime cstime are fields 14-17, v[11:15]
    return sum(int(x) for x in v[11:15 if children else 13])


class Meter:
    """Meter measures the CPU that processes outside this one, its children
    and the server used, in cores."""

    def __init__(self, server):
        self.server = server

    def mark(self):
        own = cpu_of("self", True) + (cpu_of(self.server, False) if self.server else 0)
        return time.time(), busy(), own

    def foreign(self, m0):
        m1 = self.mark()
        hz = os.sysconf("SC_CLK_TCK")
        return ((m1[1] - m0[1]) - (m1[2] - m0[2])) / hz / max(m1[0] - m0[0], 1e-3)


def canary(spec):
    """canary runs a canary, and once more if it falls below its floor (the
    first run after a pause can start cold), and returns the better rate."""
    name, floor, cmd = spec.split(":", 2)
    v = 0.0
    for _ in range(2):
        out = subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout
        m = re.search(r'"rps":\s*([\d.]+)', out)
        v = max(v, float(m[1]) if m else 0.0)
        if v >= float(floor):
            break
    return name, v, v >= float(floor)


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("-bin", required=True, help="directory of bench.test, webapibench and intlfirst")
    p.add_argument("-node", required=True, help="the node binary")
    p.add_argument("-o", required=True, help="the JSONL file to append to")
    p.add_argument("-rounds", type=int, default=7, help="rounds to keep")
    p.add_argument("-cpus", default="0-7", help="CPUs for the timed processes")
    p.add_argument("-server-cpus", default="8-13", help="CPUs for webapibench -serve")
    p.add_argument("-server", default="127.0.0.1:18090")
    p.add_argument("-first", type=int, default=10, help="fresh processes per engine and round for the first use")
    p.add_argument("-fetch-rounds", type=int, default=3, help="rounds of fetch-bench.js per run")
    p.add_argument("-lock", default=os.path.expanduser("~/moejs-bench/lock"))
    p.add_argument("-canary", action="append", default=[], help="NAME:FLOOR:SHELL-COMMAND printing \"rps\":N")
    p.add_argument("-fixture", default=os.path.join(os.path.expanduser("~"), "nodefs-fixture"),
                   help="the fixture tree of the nodefs section, written by nodefsbench -fixture if missing")
    p.add_argument("-sections", default="format,segmenter,first,fetch", help="the sections to run, comma-separated")
    a = p.parse_args()
    sections = a.sections.split(",")

    b = Bench(a)
    if "nodefs" in sections and not os.path.isdir(a.fixture):
        run([os.path.join(a.bin, "nodefsbench"), "-fixture", a.fixture])
    if not os.path.exists(a.o) or os.path.getsize(a.o) == 0:
        gobin = next(os.path.join(a.bin, f) for f in ("bench.test", "nodefsbench", "webapibench")
                     if os.path.exists(os.path.join(a.bin, f)))
        meta = {
            "date": datetime.date.today().isoformat(),
            "node": run([a.node, "-p", "process.version + ' icu ' + process.versions.icu"]).strip(),
            "go": run(["go", "version", gobin]).split(": ", 1)[1].strip(),
            "cpu": next(l.split(":", 1)[1].strip() for l in open("/proc/cpuinfo") if l.startswith("model name")),
            "cpus": a.cpus,
            "canaries": [{"name": c.split(":")[0], "floor": float(c.split(":")[1])} for c in a.canary],
        }
        if "nodefs" in sections:
            meta["tree_files"] = sum(len(f) for _, _, f in os.walk(os.path.join(a.fixture, "tree")))
        with open(a.o, "a") as f:
            f.write(json.dumps({"meta": meta}) + "\n")
    done = 0
    with open(a.o) as f:
        done = len({json.loads(l)["round"] for l in f if '"round"' in l})

    server = None
    if "fetch" in sections:
        server = subprocess.Popen(["taskset", "-c", a.server_cpus, os.path.join(a.bin, "webapibench"), "-serve", a.server],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(1)
    meter = Meter(server.pid if server else None)
    lock = open(a.lock, "w")
    attempt = 0
    try:
        while done < a.rounds and attempt < 3 * a.rounds:
            attempt += 1
            rnd = done + 1
            order = ["moejs", "node"] if rnd % 2 else ["node", "moejs"]
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
            for name, section in (("format", b.intl_format), ("segmenter", b.segmenter), ("first", None), ("fetch", b.fetch),
                                  ("nodefs", b.nodefs)):
                if name not in sections:
                    continue
                if section is None:
                    results = b.first_use(order)
                else:
                    results = {e: section(e) for e in order}
                for e in order:
                    for r in results[e]:
                        row = {"engine": e, "row": r[0], "value": round(r[1], 3), "unit": r[2]}
                        if len(r) > 3:
                            row["samples"] = r[3]
                        rows.append(row)
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
        if server:
            server.kill()
    print(f"done: {done} rounds kept", flush=True)
    sys.exit(0 if done >= a.rounds else 1)


if __name__ == "__main__":
    main()
