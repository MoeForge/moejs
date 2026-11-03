#!/usr/bin/env python3
"""gen.py writes the inputs of the plugin scenarios that bench/cmd/pluginbench
runs into bench/testdata/scenarios (git-ignored), and checks each file against
SHA256SUMS:

    bench/testdata/plugins/fetch.sh
    python3 bench/scenarios/gen.py

It needs Python 3.9 or later and, for the agent requests, the source tree of
Go 1.26.6, which it locates with `GOTOOLCHAIN=go1.26.6 go env GOROOT` (the go
command downloads that toolchain when it is not installed) or takes from
--goroot.

No input of a scenario is kept as data. gen.py makes them from:

- plugins.js and plugins.mjs: the new-api task plugins that fetch.sh
  downloads, each wrapped in a function that returns its exports, after
  utils.js (the stand-in for new-api's utils global) and followed by agent.js
  (the plugin of the agent requests);
- hooks.json: the hooks the fixtures call, in the order they first appear,
  then the agent plugin's; a call names its hook by its index in this list;
- scn/small.bin: the 269 calls of the fixtures in bench/testdata/fixtures;
- scn/big8m.bin and scn/multi8x1m.bin: two fixture calls of the alibaba
  plugin, with the URL of their image replaced by data URLs of images that
  random.Random(7) makes;
- scn/agent100k.bin: the sessions of agent-sessions.json, with the results of
  their read_file and grep calls computed from the Go source.

A scenario file holds the number of requests, then for each request its
number of calls, then for each call a line "<hook> <n>" followed by n bytes
of JSON arguments and a newline.
"""

import argparse
import base64
import copy
import hashlib
import json
import os
import random
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
BENCH = os.path.dirname(HERE)
GO = "go1.26.6"


def dumps(v):
    """The JSON of a call's arguments: sorted keys, no spaces, UTF-8."""
    return json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def read_text(path):
    with open(path, encoding="utf-8", newline="") as f:
        return f.read()


def scenario(reqs):
    out = [b"%d\n" % len(reqs)]
    for calls in reqs:
        out.append(b"%d\n" % len(calls))
        for hook, args in calls:
            out += [b"%d %d\n" % (hook, len(args)), args, b"\n"]
    return b"".join(out)


# --- the plugins -----------------------------------------------------------

TRAILER = """];
__H.forEach(function (f, i) { if (typeof f !== 'function') throw new Error('hook ' + i + ' is not a function'); });

var __U = { __u: true };
// Every engine calls the same function: the hook's errors and undefined come back as plain values.
function __call(i, a) {
  try {
    var r = __H[i].apply(undefined, a);
    return r === undefined ? __U : r;
  } catch (e) {
    return { __err: String(e && e.message) };
  }
}
// For hosts without a JSON API (modernc.org/quickjs): JSON text in, JSON text out.
function __callJSON(i, t) { return JSON.stringify(__call(i, JSON.parse(t))); }
"""


def wrap(key, text):
    """The module text of a plugin as a function that returns its exports,
    stored in __P[key]: every engine runs the plugins as one script."""
    names = []

    def unexport(m):
        names.append(m.group(2))
        return m.group(1) + " " + m.group(2)

    body = re.sub(r"^export (const|function) ([A-Za-z_$][\w$]*)", unexport, text, flags=re.M)
    if re.search(r"^\s*export\b", body, flags=re.M):
        sys.exit("gen.py: %s has an export other than export const and export function" % key)
    ret = ", ".join('"%s": %s' % (n, n) for n in names)
    return '__P["%s"] = (function () {\n"use strict";\n%s\nreturn { %s };\n})();\n' % (key, body, ret)


def bundle(plugins, hooks):
    """plugins.js, and plugins.mjs, which exports __call."""
    parts = ['"use strict";\n\n', read_text(os.path.join(HERE, "utils.js")), "\nvar __P = {};\n"]
    parts += [wrap(key, text) for key, text in plugins]
    parts.append("var __H = [\n")
    parts.append(",\n".join('__P["%s"]["%s"]' % (k, h) + "".join('["%s"]' % p for p in path) for k, h, path in hooks))
    parts.append("\n" + TRAILER)
    js = "".join(parts)
    return js.encode(), (js + "\nexport { __call };\n").encode()


def hook_path(case):
    """FixtureCase.HookPath in bench/fixtures.go."""
    return [case["member"]] if case.get("member") else case.get("path", [])


# --- the large requests ----------------------------------------------------

PANDA = "https://img.example.com/reference/panda.png"


def images():
    """The images of the large requests: one stream of random.Random(7), two
    of 6 MiB (8 MiB in base64) and then sixteen of 768 KiB (1 MiB)."""
    r = random.Random(7)
    big = [r.randbytes(6 << 20) for _ in range(2)]
    multi = [r.randbytes(768 << 10) for _ in range(16)]
    return big, multi


def image_requests(cases, hooks, imgs, per):
    """Requests of two calls of the alibaba plugin, decoding a request to edit
    images and building the upstream request, with per images each."""
    decode = next(c for c in cases if c["name"] == "responses decode image edit wan2.7-image")
    submit = next(c for c in cases if c["name"] == "build submit wan2.7 async image edit")
    hd = hooks.index(["alibaba", decode["hook"], hook_path(decode)])
    hs = hooks.index(["alibaba", submit["hook"], hook_path(submit)])
    reqs = []
    for i in range(0, len(imgs), per):
        urls = ["data:image/png;base64," + base64.b64encode(b).decode() for b in imgs[i:i + per]]
        d = copy.deepcopy(decode["args"])
        content = d[0]["body"]["value"]["input"][1]["content"]
        assert [c.get("image_url") for c in content[1:]] == [{"url": PANDA}]
        content[1:] = [{"image_url": {"url": u}, "type": "input_image"} for u in urls]
        s = copy.deepcopy(submit["args"])
        assert s[0]["body"]["value"]["images"] == [PANDA] and s[0]["requestBody"]["images"] == [PANDA]
        s[0]["body"]["value"]["images"] = list(urls)
        s[0]["requestBody"]["images"] = list(urls)
        reqs.append([(hd, dumps(d)), (hs, dumps(s))])
    return reqs


# --- the agent requests ----------------------------------------------------


class GoSource:
    """The source tree of Go that the sessions' tools read."""

    def __init__(self, src):
        self.src = src
        self.cache = {}

    def lines(self, path):
        if path not in self.cache:
            with open(os.path.join(self.src, path), encoding="utf-8") as f:
                self.cache[path] = f.read().split("\n")
        return self.cache[path]

    def read_file(self, path, offset, limit):
        """The result of read_file: limit lines from line offset, numbered."""
        lines = self.lines(path)[offset - 1:offset - 1 + limit]
        return "\n".join("%6d\t%s" % (offset + i, l) for i, l in enumerate(lines))

    def files(self, roots):
        """The .go files in the directory trees roots, testdata directories and
        hidden names left out."""
        out = set()
        for root in roots:
            for dirpath, dirs, names in os.walk(os.path.join(self.src, root)):
                dirs[:] = [d for d in dirs if d != "testdata" and not d.startswith(".")]
                for n in names:
                    if n.endswith(".go") and not n.startswith("."):
                        out.add(os.path.relpath(os.path.join(dirpath, n), self.src).replace(os.sep, "/"))
        return out

    def grep(self, pattern, files):
        """The result of grep for the pattern \\bword\\b: every line of files
        that contains word, as path:line:text with the text trimmed, the first
        60 and then a count of the others. A line matches when it contains
        word anywhere, as the sessions were recorded."""
        word = pattern[2:-2]
        hits = []
        for p in files:
            for i, l in enumerate(self.lines(p)):
                if word in l:
                    hits.append("%s:%d:%s" % (p, i + 1, l.strip()))
        if len(hits) > 60:
            hits[60:] = ["... %d more matches" % (len(hits) - 60)]
        return "\n".join(hits)


def agent_requests(go, hooks):
    """The sessions of agent-sessions.json. A tool message without content gets
    the result of its call; one with "lines" keeps that many lines of it, where
    the session's token budget ran out. grep searches the files under the
    session's roots: first those of its order, in that order, then the others,
    which match none of its greps where the order would matter."""
    with open(os.path.join(HERE, "agent-sessions.json"), encoding="utf-8") as f:
        a = json.load(f)
    hook = hooks.index(["agent", "buildUpstreamRequest", []])
    reqs = []
    for n, s in enumerate(a["sessions"]):
        found = go.files(s["grep"]["roots"])
        order = s["grep"]["order"]
        missing = [p for p in order if p not in found]
        if missing:
            sys.exit("gen.py: session %d: %s is not in the Go source" % (n, missing[0]))
        files = order + sorted(found - set(order))
        args = s["args"]
        body = args[0]["requestBody"]
        body["tools"] = a["tools"]
        calls = {}
        for m in body["messages"]:
            for c in m.get("tool_calls", []):
                calls[c["id"]] = (c["function"]["name"], json.loads(c["function"]["arguments"]))
            if m["role"] != "tool" or "content" in m:
                continue
            name, arg = calls[m["tool_call_id"]]
            if name == "read_file":
                out = go.read_file(arg["path"], arg["offset"], arg["limit"])
            elif name == "grep":
                out = go.grep(arg["pattern"], files)
            else:
                sys.exit("gen.py: session %d: no result for %s" % (n, name))
            if "lines" in m:
                out = "\n".join(out.split("\n")[:m.pop("lines")])
            m["content"] = out
        reqs.append([(hook, dumps(args))])
    return reqs


def go_source(goroot):
    if not goroot:
        env = dict(os.environ, GOTOOLCHAIN=GO)
        try:
            goroot = subprocess.run(["go", "env", "GOROOT"], env=env, cwd=HERE, check=True,
                                    capture_output=True, text=True).stdout.strip()
        except (OSError, subprocess.CalledProcessError) as e:
            sys.exit("gen.py: GOTOOLCHAIN=%s go env GOROOT failed (%s); pass --goroot" % (GO, e))
    with open(os.path.join(goroot, "VERSION"), encoding="utf-8") as f:
        version = f.readline().strip()
    if version != GO:
        sys.exit("gen.py: %s is %s, the sessions read %s" % (goroot, version, GO))
    return GoSource(os.path.join(goroot, "src"))


def main():
    ap = argparse.ArgumentParser(description="Write the inputs of the plugin scenarios.")
    ap.add_argument("--out", default=os.path.join(BENCH, "testdata", "scenarios"), help="output directory")
    ap.add_argument("--goroot", help="GOROOT of %s" % GO)
    args = ap.parse_args()

    fixtures = []
    fdir = os.path.join(BENCH, "testdata", "fixtures")
    for name in sorted(os.listdir(fdir)):
        if name.endswith(".json"):
            with open(os.path.join(fdir, name), encoding="utf-8") as f:
                fixtures.append((name[:-5], json.load(f)["cases"]))
    plugins = []
    for key, _ in fixtures:
        p = os.path.join(BENCH, "testdata", "plugins", key, "plugin.js")
        if not os.path.exists(p):
            sys.exit("gen.py: %s is missing; run bench/testdata/plugins/fetch.sh" % p)
        plugins.append((key, read_text(p)))
    plugins.append(("agent", read_text(os.path.join(HERE, "agent.js"))))

    hooks = []
    for key, cases in fixtures:
        for c in cases:
            if [key, c["hook"], hook_path(c)] not in hooks:
                hooks.append([key, c["hook"], hook_path(c)])
    hooks.append(["agent", "buildUpstreamRequest", []])

    js, mjs = bundle(plugins, hooks)
    alibaba = dict(fixtures)["alibaba"]
    big, multi = images()
    out = {
        "hooks.json": json.dumps(hooks).encode(),
        "plugins.js": js,
        "plugins.mjs": mjs,
        "scn/small.bin": scenario([[(hooks.index([k, c["hook"], hook_path(c)]), dumps(c["args"]))]
                                   for k, cases in fixtures for c in cases]),
        "scn/big8m.bin": scenario(image_requests(alibaba, hooks, big, 1)),
        "scn/multi8x1m.bin": scenario(image_requests(alibaba, hooks, multi, 8)),
        "scn/agent100k.bin": scenario(agent_requests(go_source(args.goroot), hooks)),
    }

    want = {}
    with open(os.path.join(HERE, "SHA256SUMS"), encoding="utf-8") as f:
        for line in f:
            if line.strip():
                h, name = line.split()
                want[name] = h
    bad = []
    for name, data in out.items():
        path = os.path.join(args.out, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(data)
        h = hashlib.sha256(data).hexdigest()
        if want.get(name) != h:
            bad.append(name)
        print("%s  %s" % (h, name))
    if bad:
        sys.exit("gen.py: %s differ from SHA256SUMS" % ", ".join(bad))
    print("all files match SHA256SUMS, in %s" % args.out)


if __name__ == "__main__":
    main()
