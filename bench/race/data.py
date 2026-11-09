"""Racer positions from the recorded timelines (raw/<scene>/selected.json:
for each engine the whole median-total round of three, never a synthetic
curve).

A relay track has n equal legs; a position runs from 0 to n.

- V8 relay: the eight benchmarks of one recorded run, each 1/8 of the
  track. Inside a leg the car follows that leg's recorded iteration
  completions and waits while the run does setup or teardown, so its
  clock time is the run's own clock. Raw iteration counts never set a
  position (Richards alone is 533 of 1,215).
- Plugin relay: five legs, each measured as its own run, so an engine's
  leg i starts when its leg i-1 ends (the sum of its own recorded times).
  Legs 1-4 are small, agent100k, big8m and multi8x1m, one request at a
  time: the car reaches 1/count of the leg at each recorded request
  completion and moves at a steady pace between two completions. Leg 5
  is 8 cores (raw/par8): agent100k on eight workers per engine, each with
  its own clock; the leg ends at the n-th request the eight finished
  together, n being a count every engine reached inside the recorded
  window, and the car steps at each of those n completions.
- 8 cores in the window: the requests the eight workers finished by the
  window's end (the tiles' ratio).

Every scene comes from one recording (measure.py)."""

import bisect
import json
from pathlib import Path

PLUGIN_LEGS = ["small", "agent100k", "big8m", "multi8x1m"]
SHOWN = ["moejs", "go-quickjs", "goant", "modernc", "goja", "sobek", "qjs"]


class Racer:
    """legs: [(start, setup_end, end, completions, count)] in seconds on the
    racer's own relay clock."""

    def __init__(self, key, legs):
        self.key = key
        self.legs = legs
        self.n = len(legs)
        self.end = legs[-1][2]
        self.leg_end = [l[2] for l in legs]

    def pos(self, t):
        if t <= 0:
            return 0.0
        for i, (start, setup, end, ms, count) in enumerate(self.legs):
            if t >= end:
                continue
            if t < setup:
                return float(i)
            k = bisect.bisect_right(ms, t)
            if k >= len(ms):
                return float(i + 1)
            prev = ms[k - 1] if k else setup
            return i + (k + (t - prev) / (ms[k] - prev)) / count
        return float(self.n)

    def done(self, t):
        """Work units (requests or iterations) completed by time t."""
        return sum(bisect.bisect_right(l[3], t) for l in self.legs)


class Counter:
    """Requests done by several workers, each on its own clock, counted up
    to `window` seconds."""

    def __init__(self, key, workers, window):
        self.key, self.window = key, window
        self.ms = sorted(x / 1000 for w in workers for x in w["ms"] if x <= window * 1000)
        self.workers = len(workers)

    def done(self, t):
        return bisect.bisect_right(self.ms, min(t, self.window))


def _sel(root, scene):
    return json.loads((Path(root) / "raw" / scene / "selected.json").read_text())


def plugin_relay(root, keys, n, window):
    """The five-leg plugin relay: legs 1-4 one request at a time, leg 5 up
    to the n-th request of the 8-core run, every selected run checked
    against the reference hash of its scenario."""
    ref = refs(root)
    runs = {s: _sel(root, s) for s in PLUGIN_LEGS}
    par = par8_runs(root, keys)
    out = {}
    for k in keys:
        legs, off = [], 0.0
        for s in PLUGIN_LEGS:
            d = runs[s][k]
            assert d["check"] == ref[s], (s, k)
            ms = [off + x / 1000 for x in d["ms"]]
            assert len(ms) == d["requests"]
            legs.append((off, off, ms[-1], ms, len(ms)))
            off = ms[-1]
        # each worker on its own clock, as the window count; the n-th
        # request falls inside every engine's recorded window
        ms = sorted(x for w in par[k]["workers"] for x in w["ms"])[:n]
        assert len(ms) == n and ms[-1] <= window * 1000, k
        ms = [off + x / 1000 for x in ms]
        legs.append((off, off, ms[-1], ms, n))
        out[k] = Racer(k, legs)
    return out


def v8_relay(root, keys):
    runs = _sel(root, "fixed")
    out = {}
    for k in keys:
        d = runs[k]
        legs = []
        for l in d["legs"]:
            ms = [x / 1000 for x in l["ms"]]
            assert len(ms) == l["count"]
            legs.append((l["start_ms"] / 1000, l["setup_end_ms"] / 1000, l["end_ms"] / 1000, ms, l["count"]))
        # a run's total may include up to 2 ms of teardown after its last iteration
        assert len(legs) == 8 and abs(legs[-1][2] - d["total_ms"] / 1000) <= 0.002
        out[k] = Racer(k, legs)
    return out, [l["name"] for l in d["legs"]]


def scores(root):
    return {k: float(v["scores"]["Score"]) for k, v in _sel(root, "score").items()}


def refs(root):
    return json.loads((Path(root) / "refs.json").read_text())


def par8_runs(root, keys):
    """{engine: selected run} of the 8-core race, every worker checked
    against the reference hash of agent100k."""
    ref, runs = refs(root)["agent100k"], _sel(root, "par8")
    for k in keys:
        assert all(w["check"] == ref for w in runs[k]["workers"]), k
    return {k: runs[k] for k in keys}


def lanes(root):
    """{scene: K} from the interference checks: each scene ran on K + 1
    lanes (K busy neighbours)."""
    return {f.stem: json.loads(f.read_text())["K"] for f in sorted((Path(root) / "check").glob("*.json"))}


def par8(root, keys, window):
    counts = {k: Counter(k, w["workers"], window) for k, w in par8_runs(root, keys).items()}
    assert all(c.workers == 8 for c in counts.values())
    return counts
