#!/usr/bin/env python3
"""Checks the encoded file, not the renderer: 1920x1080 at 60 fps, a video
and an audio stream that both decode to the end without errors, and the
frame count and duration the renderer planned (claims.json). With the
recording (measure.py's root) it also re-derives every measured number of
claims.json from the raw selected runs: the V8 relay finishes (with the
previous release and its speedup), the scores, the five plugin legs and
their reference hashes, the relay totals and the tiles, each selected run
being the median of its engine's three accepted rounds.

Usage: verify.py <video.mp4> <claims.json> [<root>]"""

import json
import re
import subprocess
import sys

import imageio_ffmpeg

path, claims = sys.argv[1], json.load(open(sys.argv[2]))
ff = imageio_ffmpeg.get_ffmpeg_exe()
info = subprocess.run([ff, "-hide_banner", "-i", path], capture_output=True, text=True).stderr
video = re.search(r"Stream #0:\d+.*Video: (\w+).*?, (\d+)x(\d+).*?, ([\d.]+) fps", info)
audio = re.search(r"Stream #0:\d+.*Audio: (\w+).*?, (\d+) Hz, (\w+)", info)
assert video and audio, info
codec, w, h, fps = video.group(1), int(video.group(2)), int(video.group(3)), float(video.group(4))
assert (w, h) == (1920, 1080) and abs(fps - 60) < 1e-6, (w, h, fps)
# Mapping both streams fails if either is missing; a full decode reads every packet.
r = subprocess.run([ff, "-hide_banner", "-loglevel", "error", "-i", path, "-map", "0:v:0", "-map", "0:a:0",
                    "-progress", "pipe:1", "-nostats", "-f", "null", "-"], capture_output=True, text=True)
assert r.returncode == 0 and not r.stderr.strip(), r.stderr
frames = int(re.findall(r"^frame=(\d+)$", r.stdout, re.M)[-1])
out_us = int(re.findall(r"^out_time_us=(\d+)$", r.stdout, re.M)[-1])
assert frames == claims["frames"], (frames, claims["frames"])
assert abs(out_us / 1e6 - claims["duration_s"]) < 0.1, (out_us, claims["duration_s"])
gw = {}
if len(sys.argv) > 3:
    from pathlib import Path
    root = Path(sys.argv[3])
    refs = json.loads((root / "refs.json").read_text())

    def sel(scene):
        return json.loads((root / "raw" / scene / "selected.json").read_text())

    def rounds(scene, k):
        return [json.loads((root / "raw" / scene / f"{k}.{r}.json").read_text()) for r in (1, 2, 3)]

    def median_of(scene, k, d, metric):
        rs = rounds(scene, k)
        assert d in rs and metric(d) == sorted(map(metric, rs))[1], (scene, k)

    rows = {r["scenario"] + ("-8" if r.get("workers") else ""): r for r in claims["gateway"]}
    for sc in ("small", "agent100k", "big8m", "multi8x1m"):
        runs = sel(sc)
        for k, want in rows[sc]["finish_s"].items():
            d = runs[k]
            assert d["check"] == refs[sc] == rows[sc]["check"], (sc, k)
            assert len(d["ms"]) == d["requests"] == rows[sc]["requests"], (sc, k)
            assert round(d["ms"][-1] / 1000, 3) == want, (sc, k)
            median_of(sc, k, d, lambda x: x["ms"][-1])
    par, window = sel("par8"), rows["agent100k-8"]["window_s"]
    for k, want in rows["agent100k-8"]["done"].items():
        ws = par[k]["workers"]
        assert len(ws) == 8 and all(x["check"] == refs["agent100k"] for x in ws), k
        n = sum(sum(1 for t in x["ms"] if t <= window * 1000) for x in ws)
        assert n == want, (k, n)
        assert min(x["ms"][-1] for x in ws) / 1000 > window + par[k]["meta"]["skew_s"], k
        median_of("par8", k, par[k], lambda x: sum(sum(1 for t in w["ms"] if t <= window * 1000) for w in x["workers"]))
    # the fifth leg: up to the n-th request the eight workers finished
    # together, n the most whole thousands every engine reached in the window
    p8 = rows["agent100k-8"]
    n = p8["leg_requests"]
    assert n == min(p8["done"].values()) // 1000 * 1000, n
    for k, want in p8["leg_s"].items():
        ms = sorted(t for w in par[k]["workers"] for t in w["ms"])
        assert ms[n - 1] <= window * 1000 and round(ms[n - 1] / 1000, 3) == want, (k, ms[n - 1], want)
    pr = claims["plugin_relay"]
    for k, total in pr["total_s"].items():
        assert abs(sum(r["leg_s"][k] for r in claims["gateway"]) - total) <= 0.003, (k, total)
    tiles = list(claims["tiles"].values())
    for sc, want in zip(("small", "agent100k", "big8m", "multi8x1m"), tiles):
        runs = sel(sc)
        assert round(runs["qjs"]["ms"][-1] / runs["moejs"]["ms"][-1], 3) == want, (sc, want)
    assert round(p8["done"]["moejs"] / p8["done"]["qjs"], 3) == tiles[4]
    fixed = sel("fixed")
    for k, want in claims["v8"]["finish_s"].items():
        assert abs(fixed[k]["total_ms"] / 1000 - want) <= 0.002, (k, fixed[k]["total_ms"], want)
        median_of("fixed", k, fixed[k], lambda x: x["total_ms"])
    assert "moejs-old" in claims["v8"]["finish_s"]
    assert round(fixed["moejs-old"]["total_ms"] / fixed["moejs"]["total_ms"], 3) == claims["v8"]["speedup"]
    score = sel("score")
    for k, want in claims["scores"].items():
        assert float(score[k]["scores"]["Score"]) == want, k
        median_of("score", k, score[k], lambda x: float(x["scores"]["Score"]))
    gw = dict(numbers="V8 relay (8 cars, speedup), scores, the five plugin legs, relay totals and tiles "
                      "match the selected median runs and hashes")
res = dict(video=codec, width=w, height=h, fps=fps, frames=frames, decoded_s=round(out_us / 1e6, 3),
           audio=f"{audio.group(1)} {audio.group(2)} Hz {audio.group(3)}", full_decode="ok", **gw)
print(json.dumps(res))
