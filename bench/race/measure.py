#!/usr/bin/env python3
"""Records every number of the race video in one run on one machine.

Engines: moejs (the current tree, built by build.sh), go-quickjs, goant,
modernc, goja, Sobek and C QuickJS, and in the V8 relay also moejs-old,
moejs's previous release (v0.1.0-alpha.11). With pluginrace-ctl and
suite-ctl in RACE_BIN, an off-screen control, moejs-ctl (any other moejs
build), is measured beside them in every scene and never shown.

Scenes: small, agent100k, big8m and multi8x1m (one request at a time,
every engine checking the hash of its hook results against the C host's
before its clock), fixed (the V8 v7 relay with fixed iteration counts)
and score (the suite's own run.js), each measured in parallel lanes;
then par8: agent100k on eight workers on PAR_CPUS with no other process
on the recording's CPUs.

Lanes. Every process is pinned to its own CPU with GOMAXPROCS=1. Before
a scene is measured, an interference check times moejs and C QuickJS
alone on one CPU and while K = 3, 5, 7 other lanes run the same scene's
workload, the four interleaved (check/<scene>.json); the scene uses the
largest K at which, and below which, both lose at most 3 % against alone
and their ratio moves at most 2 %. The scene's jobs (every engine three
rounds) then run on K + 1 lanes spread over the CPUs: a free lane takes
the next job, longest first, preferring an engine it has not run yet,
and while any job still runs, an idle lane runs filler copies of the
scene (the scene's engines in turn, results dropped). So every job runs
beside K busy neighbours until the last one ends, whether it is fast or
slow. The timing lock is held once around each batch; before every
batch and every par8 try the CPU canaries (PAR_CPUS, nothing else
running) must reach 95 % and 93 % of the recording's own base.

The recording uses the CPUs in RACE_CPUS only (lanes, canaries, par8,
this script on ORCH_CPU); pin it to idle cores. A try counts only if no
foreign process showed up on those CPUs while it ran: none on its own
CPU (its CPU's busy time beyond its own), and on the idle CPUs of the
set no second busier than 30 % and at most 0.1 busy cores on average;
others are kept and retried, and so is a configuration of the
interference check. The gate before a batch or a par8 try waits for the
CPUs to be quiet. Three rounds per engine, a round more than 5 % from
both others rerun (up to three passes, beside the same filler load), the
median round's own run selected (raw/<scene>/selected.json).

Settings (environment):
  RACE_OUT        the recording (default: race-out)
  RACE_BIN        the runners of build.sh, plus qjs and racehost (default: race-bin)
  RACE_SCENARIOS  plugins.js, plugins.mjs and scn/ of bench/scenarios/gen.py
                  (default: bench/testdata/scenarios)
  MOEJS_V8_DIR    the V8 suite of bench/v8suite/fetch.sh (default: ~/.cache/moejs/v8-v7)
  RACE_LOCK       a lock file shared with other timing jobs (default: $RACE_OUT/lock)
  RACE_CPUS       the CPUs of the recording (default: 0-19)
  RACE_SPREAD     lane CPUs, the most spread first (default: 2,14,8,17,5,11,16,0)
  RACE_PAR_CPUS   the eight CPUs of par8 and the canaries (default: 0-7)
  RACE_ORCH_CPU   this script's CPU, never a lane (default: the last of RACE_CPUS)

Usage: measure.py [scene ...]   (default: all, in order)"""
import fcntl, hashlib, itertools, json, math, os, queue, signal, statistics, subprocess, sys, threading, time
from pathlib import Path

HERE = Path(__file__).resolve().parent


def cpus(s):
    """'0-7,10' -> [0, 1, ..., 7, 10]"""
    out = []
    for part in s.split(','):
        a, _, b = part.partition('-')
        out += list(range(int(a), int(b or a) + 1))
    return out


R = Path(os.environ.get('RACE_OUT', 'race-out')).resolve()
BIN = Path(os.environ.get('RACE_BIN', 'race-bin')).resolve()
QB = Path(os.environ.get('RACE_SCENARIOS', HERE.parent / 'testdata/scenarios')).resolve()
V8 = Path(os.environ.get('MOEJS_V8_DIR', Path.home() / '.cache/moejs/v8-v7'))
SHOWN = ['moejs', 'go-quickjs', 'goant', 'modernc', 'goja', 'sobek', 'qjs']
CTL, OLD = 'moejs-ctl', 'moejs-old'
HAVE_CTL = (BIN / 'pluginrace-ctl').exists() and (BIN / 'suite-ctl').exists()
PLUGIN = dict(small=100000, agent100k=1600, big8m=160, multi8x1m=160)
SCENES = ['small', 'agent100k', 'big8m', 'multi8x1m', 'fixed', 'score', 'par8']
# The V8 relay's iterations per benchmark, in suite order: one count for
# every engine, about 13 s of moejs work (1,215 iterations).
FIXED_COUNTS = dict(Richards=533, DeltaBlue=213, Crypto=16, RayTrace=33, EarleyBoyer=13, RegExp=23, Splay=356,
                    NavierStokes=28)
# Each scene's engines, the longest run first.
ORDER = dict(
    small=['sobek', 'goja', 'goant', 'modernc', 'go-quickjs', 'qjs', CTL, 'moejs'],
    agent100k=['modernc', 'sobek', 'goja', 'goant', 'go-quickjs', 'qjs', CTL, 'moejs'],
    big8m=['modernc', 'sobek', 'goja', 'qjs', 'go-quickjs', 'goant', CTL, 'moejs'],
    multi8x1m=['modernc', 'sobek', 'goja', 'qjs', 'go-quickjs', 'goant', CTL, 'moejs'],
    fixed=['goja', 'sobek', 'goant', 'modernc', OLD, 'qjs', 'go-quickjs', CTL, 'moejs'],
    score=['goja', 'sobek', 'goant', 'modernc', 'qjs', 'go-quickjs', CTL, 'moejs'])
ORDER = {s: [e for e in v if e != CTL or HAVE_CTL] for s, v in ORDER.items()}
MINE = cpus(os.environ.get('RACE_CPUS', '0-19'))
SPREAD = [int(c) for c in os.environ.get('RACE_SPREAD', '2,14,8,17,5,11,16,0').split(',')]
KS = (3, 5, 7)
PROBES = ('moejs', 'qjs')
ORCH_CPU = int(os.environ.get('RACE_ORCH_CPU', MINE[-1]))  # this script; never a lane
PAR_CPUS = cpus(os.environ.get('RACE_PAR_CPUS', '0-7'))
assert len(PAR_CPUS) == 8 and set(SPREAD + PAR_CPUS + [ORCH_CPU]) <= set(MINE) and ORCH_CPU not in SPREAD + PAR_CPUS
WINDOW = 10.0
HZ = os.sysconf('SC_CLK_TCK')
LOCK = Path(os.environ.get('RACE_LOCK', R / 'lock'))
ENV = dict(os.environ, GOMAXPROCS='1', GOTMPDIR=str(R / 'tmp'), TMPDIR=str(R / 'tmp'))
REFS, COUNTS = None, None


def log(s):
    line = time.strftime('%H:%M:%S ') + s
    print(line, flush=True)
    with open(R / 'log', 'a') as f:
        f.write(line + '\n')


def save(p, x):
    p.write_text(json.dumps(x, separators=(',', ':')) + '\n')


def ps():
    out = subprocess.run(['ps', '-eo', 'psr,pcpu,args', '--sort=-pcpu'], capture_output=True, text=True).stdout
    return ' | '.join(line.strip()[:90] for line in out.splitlines()[1:6])


def cpu_times():
    """Busy and steal ticks per CPU, and for the whole machine (key -1)."""
    out = {}
    for line in Path('/proc/stat').read_text().splitlines():
        if not line.startswith('cpu'):
            break
        f = line.split()
        v = [float(x) for x in f[1:]]
        out[-1 if f[0] == 'cpu' else int(f[0][3:])] = (v[0] + v[1] + v[2] + v[5] + v[6], v[7])
    return out


def command(scene, engine, cpu, check, n):
    if scene in PLUGIN or scene == 'par8':
        sc = 'agent100k' if scene == 'par8' else scene
        if engine == 'qjs':
            c = [str(BIN / 'racehost'), '-engine', 'qjs', '-check', str(check)]
        else:
            b = {'moejs': 'pluginrace-moejs', CTL: 'pluginrace-ctl'}.get(engine, 'pluginrace')
            c = [str(BIN / b), '-engine', 'moejs' if engine == CTL else engine, '-ref', REFS[sc]]
        c += ['-dir', str(QB), '-scenario', sc, '-n', str(n)]
    else:
        js, cli = R / 'suite' / f'{scene}.js', R / 'suite' / f'{scene}-cli.js'
        if engine == 'qjs':
            c = [str(BIN / 'qjs'), str(cli)]
        else:
            b = {'moejs': 'suite-moejs', CTL: 'suite-ctl', OLD: 'suite-old'}.get(engine, 'suite')
            c = [str(BIN / b), '-engine', 'moejs' if engine == CTL else engine, str(js)]
    return ['taskset', '-c', str(cpu)] + c


def parse(scene, engine, rc, out, err, check, n):
    rows = [json.loads(s) for s in out.splitlines() if s.startswith('{')]
    if rc or not rows:
        raise ValueError(f'exit {rc}: {err[-300:]}')
    d = rows[-1]
    if scene in PLUGIN or scene == 'par8':
        sc = 'agent100k' if scene == 'par8' else scene
        if engine == 'qjs':
            d['check'] = hashlib.sha256(Path(check).read_bytes()).hexdigest()
        if d['check'] != REFS[sc]:
            raise ValueError(f'hash {d["check"]} != {REFS[sc]}')
        ms = d['ms']
        if not (len(ms) == n == d['requests'] and all(x <= y for x, y in zip(ms, ms[1:]))):
            raise ValueError('bad timeline')
    else:
        if d.get('errors'):
            raise ValueError(str(d['errors']))
        if scene == 'score':
            if not (len(d['scores']) == 9 and float(d['scores']['Score']) > 0):
                raise ValueError('bad scores')
        elif not (len(d['legs']) == 8 and len(d['ms']) == sum(d['counts']) and d['counts'] == COUNTS
                  and all(x <= y for x, y in zip(d['ms'], d['ms'][1:]))):
            raise ValueError('bad relay')
    d['engine'] = engine
    return d


def metric(scene, d):
    if scene == 'score':
        return float(d['scores']['Score'])
    if scene == 'fixed':
        return d['total_ms'] / 1000
    if scene == 'par8':
        return float(sum(sum(1 for x in w['ms'] if x <= WINDOW * 1000) for w in d['workers']))
    return d['ms'][-1] / 1000


def slower(scene, a, b):
    """How much slower result a is than result b."""
    return b / a - 1 if scene in ('score', 'par8') else a / b - 1


class Run:
    """One pinned process. A thread reaps it (wait4: its own CPU time and
    peak RSS) and posts it to `done`; a watchdog kills it after 25 min."""

    def __init__(self, scene, engine, cpu, tag, done, n=None, filler=False):
        self.scene, self.engine, self.cpu, self.tag, self.filler = scene, engine, cpu, tag, filler
        self.n = n or PLUGIN.get(scene)
        self.check = R / 'tmp' / f'{tag}.check'
        self.outp, self.errp = R / 'tmp' / f'{tag}.out', R / 'tmp' / f'{tag}.err'
        self.a, self.t0 = cpu_times(), time.monotonic()
        with open(self.outp, 'w') as o, open(self.errp, 'w') as e:
            self.p = subprocess.Popen(command(scene, engine, cpu, self.check, self.n), env=ENV, stdout=o, stderr=e)
        self.dog = threading.Timer(1500, self.kill)
        self.dog.start()
        threading.Thread(target=self._reap, args=(done,), daemon=True).start()

    def kill(self):
        # never Popen.kill: it polls, and could reap the process before wait4
        try:
            os.kill(self.p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass

    def _reap(self, done):
        _, st, ru = os.wait4(self.p.pid, 0)
        self.dog.cancel()
        self.p.returncode = os.waitstatus_to_exitcode(st)
        self.b, self.wall = cpu_times(), time.monotonic() - self.t0
        self.own, self.rss = ru.ru_utime + ru.ru_stime, ru.ru_maxrss
        done.put(self)

    def cleanup(self):
        for f in (self.outp, self.errp, self.check):
            f.unlink(missing_ok=True)

    def result(self):
        """The parsed run (raises on a failed run or a wrong hash)."""
        try:
            return parse(self.scene, self.engine, self.p.returncode, self.outp.read_text(), self.errp.read_text(),
                         self.check, self.n)
        finally:
            self.cleanup()

    def load(self, lanes):
        """Foreign work on its CPU beyond its own CPU time, steal on its CPU,
        and on the recording's other CPUs (this script's excepted) the mean
        busy cores and the busiest second of any one of them."""
        mine = (self.b[self.cpu][0] - self.a[self.cpu][0]) / HZ
        steal = (self.b[self.cpu][1] - self.a[self.cpu][1]) / HZ / self.wall
        rest = [c for c in MINE if c not in lanes and c != ORCH_CPU]
        others = sum(self.b[c][0] - self.a[c][0] for c in rest) / HZ / self.wall
        return dict(foreign_mine=max(0.0, mine - self.own) / self.wall, steal_mine=steal, others=others,
                    others_peak=SAMPLER.peak(self.t0, self.t0 + self.wall, rest))


def clean(ld):
    """No foreign process on any of the recording's CPUs during the try."""
    return ld['foreign_mine'] <= .03 and ld['steal_mine'] <= .05 and ld['others'] <= .1 and ld['others_peak'] <= .3


class Sampler:
    """Busy fraction of every CPU in each second, for the whole recording."""

    def __init__(self):
        self.samples = []
        threading.Thread(target=self._loop, daemon=True).start()

    def _loop(self):
        a, ta = cpu_times(), time.monotonic()
        while True:
            time.sleep(1)
            b, tb = cpu_times(), time.monotonic()
            self.samples.append((ta, tb, {c: (b[c][0] - a[c][0]) / HZ / (tb - ta) for c in b if c >= 0}))
            a, ta = b, tb

    def peak(self, t0, t1, cpus):
        """The busiest second of any of cpus among the samples wholly inside
        [t0, t1] (a sample reaching outside may hold the recording's own
        neighbouring process)."""
        return max((x[c] for s0, s1, x in self.samples if s0 >= t0 and s1 <= t1 for c in cpus), default=0.0)


class Locked:
    def __enter__(self):
        self.f = open(LOCK, 'w')
        fcntl.flock(self.f, fcntl.LOCK_EX)
        return self

    def __exit__(self, *exc):
        fcntl.flock(self.f, fcntl.LOCK_UN)
        self.f.close()


def canary():
    """Two throughput canaries on PAR_CPUS, one pluginrace-moejs process per
    CPU: small (CPU-bound) and big8m (memory bandwidth). The rate is the sum
    of the eight processes' request rates."""
    vals = []
    for sc, n in (('small', 30000), ('big8m', 64)):
        ps_ = [subprocess.Popen(['taskset', '-c', str(c), str(BIN / 'pluginrace-moejs'), '-engine', 'moejs', '-dir',
                                 str(QB), '-scenario', sc, '-n', str(n)], env=ENV, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, text=True) for c in PAR_CPUS]
        rate = 0.0
        for p in ps_:
            try:
                out = p.communicate(timeout=60)[0]
            except subprocess.TimeoutExpired:
                p.kill()
                out = p.communicate()[0]
            rows = [json.loads(s) for s in out.splitlines() if s.startswith('{')]
            rate += n / (rows[-1]['ms'][-1] / 1000) if p.returncode == 0 and rows else 0.0
        vals.append(rate)
    return vals


def base():
    """The recording's canary rates: the median of three readings at the
    first gate. A machine's speed can move from day to day (other guests of
    a host, thermals), so every try is held to the recording's own rate."""
    f = R / 'canary-base.json'
    if f.exists():
        return json.loads(f.read_text())['base']
    vals = [canary() for _ in range(3)]
    b = [sorted(v[i] for v in vals)[1] for i in (0, 1)]
    save(f, dict(base=b, readings=vals, at=time.strftime('%Y-%m-%d %H:%M:%S')))
    log(f'canary base {b}')
    return b


def gate(what):
    """Holding the lock, nothing of ours running: quiet CPUs for 3 s, then
    both canaries at 95 % and 93 % of the recording's base."""
    b = base()
    while True:
        a = cpu_times(); time.sleep(3); c = cpu_times()
        vm = sum(c[k][0] - a[k][0] for k in MINE if k != ORCH_CPU) / HZ / 3
        steal = (c[-1][1] - a[-1][1]) / HZ / 3
        if vm <= 0.3 and steal <= 0.25:
            cs = canary()
            if cs[0] >= 0.95 * b[0] and cs[1] >= 0.93 * b[1]:
                return cs
            log(f'canary low {cs} (base {b}); waiting before {what}')
        else:
            log(f'CPUs busy {vm:.2f}, steal {steal:.2f}; waiting before {what}: {ps()}')
        time.sleep(20)


def stop(busy, done):
    for r in busy.values():
        r.kill()
    while busy:
        r = done.get()
        del busy[r.cpu]
        r.cleanup()


def check(scene):
    """The interference check (see the module doc)."""
    f = R / 'check' / f'{scene}.json'
    if f.exists():
        return json.loads(f.read_text())
    f.parent.mkdir(exist_ok=True)
    # solo and every K interleaved, the order rotating, so the host's drift
    # falls on all of them alike
    reps = 3 if scene in ('fixed', 'score') else 5
    pc = dict(zip(PROBES, SPREAD))
    runs, done = [], queue.Queue()

    def once(config, probes, nfill):
        """One configuration, repeated until no foreign process showed up."""
        while True:
            got = attempt(config, probes, nfill)
            if all(clean(x) for x in got):
                runs.extend(got)
                return
            log(f'check {scene} {config}: foreign {[{k: round(v, 3) if isinstance(v, float) else v for k, v in x.items()} for x in got if not clean(x)]}; retry: {ps()}')
            with open(R / 'raw/runs.jsonl', 'a') as f:
                f.write(json.dumps(dict(scene=scene, check=config, retried=got)) + '\n')
            time.sleep(10)

    def attempt(config, probes, nfill):
        busy, fill, got = {}, itertools.cycle(ORDER[scene]), []
        lanes = SPREAD[:2 + nfill] if nfill else [pc[probes[0]]]
        for c in SPREAD[2:2 + nfill]:
            busy[c] = Run(scene, next(fill), c, f'chkfill.{time.time_ns()}', done, filler=True)
        if nfill:
            time.sleep(1.0)
        for e in probes:
            busy[pc[e]] = Run(scene, e, pc[e], f'chk.{e}.{time.time_ns()}', done)
        left = set(probes)
        while left:
            r = done.get()
            del busy[r.cpu]
            if not r.filler:
                ld = r.load(lanes)
                d = r.result()
                got.append(dict(config=config, engine=r.engine, metric=metric(scene, d), cpu=r.cpu,
                                wall_s=round(r.wall, 3), maxrss_kb=r.rss, **ld))
                left.discard(r.engine)
            else:
                r.cleanup()
            if left and nfill:  # keep K neighbours busy until the last probe ends
                busy[r.cpu] = Run(scene, next(fill), r.cpu, f'chkfill.{time.time_ns()}', done, filler=True)
        stop(busy, done)
        return got

    configs = [0] + list(KS)
    with Locked():
        cs = gate(f'{scene} check')
        for i in range(reps):
            for K in configs[i % 4:] + configs[:i % 4]:
                if K:
                    once(f'K{K}', list(PROBES), K - 1)
                else:
                    for e in PROBES:
                        once('solo', [e], 0)
            log(f'check {scene}: repetition {i + 1} of {reps}')

    def med(config, e):
        return statistics.median(x['metric'] for x in runs if x['config'] == config and x['engine'] == e)

    def ratio(m):
        return m['moejs'] / m['qjs'] if scene == 'score' else m['qjs'] / m['moejs']

    solo = {e: med('solo', e) for e in PROBES}
    res = dict(scene=scene, reps=reps, probe_cpus=pc, spread=SPREAD, canaries=cs, solo=solo, solo_ratio=ratio(solo),
               ks={}, runs=runs)
    chosen = 0  # the largest K at which, and at every smaller K, the check holds
    for K in KS:
        m = {e: med(f'K{K}', e) for e in PROBES}
        loss = {e: slower(scene, m[e], solo[e]) for e in PROBES}
        move = ratio(m) / ratio(solo) - 1
        ok = all(v <= .03 for v in loss.values()) and abs(move) <= .02
        res['ks'][str(K)] = dict(median=m, loss=loss, ratio=ratio(m), ratio_move=move, ok=ok)
        if ok and chosen == ([0] + list(KS))[KS.index(K)]:
            chosen = K
    res['K'] = chosen
    save(f, res)
    log(f'check {scene}: solo {solo}; ' + '; '.join(
        f'K{k}: loss {v["loss"]["moejs"]:+.3f}/{v["loss"]["qjs"]:+.3f} ratio {v["ratio_move"]:+.3f} {"ok" if v["ok"] else "NO"}'
        for k, v in res['ks'].items()) + f' -> K={chosen}')
    return res


def batch(scene, jobs, lanes, used, sweep):
    """Runs jobs [(engine, round)] on the lanes; returns once every job has
    an accepted run."""
    dest = R / 'raw' / scene
    pending = [j for j in jobs if not (dest / f'{j[0]}.{j[1]}.json').exists()]
    if not pending:
        return
    done, busy, fails = queue.Queue(), {}, {}
    fill = itertools.cycle(ORDER[scene])
    with Locked():
        cs = gate(f'{scene} batch')
        log(f'{scene} sweep {sweep}: {len(pending)} jobs on lanes {lanes}; ps: {ps()}')
        while pending or any(not r.filler for r in busy.values()):
            for cpu in lanes:
                if cpu in busy:
                    continue
                if pending:
                    # the first job (round, then longest first) whose engine is not
                    # running, on a CPU it has not used if possible
                    running = {r.engine for r in busy.values() if not r.filler}
                    free = [i for i, (e, _) in enumerate(pending) if e not in running]
                    k = next((i for i in free if cpu not in used.setdefault(pending[i][0], set())),
                             free[0] if free else 0)
                    e, rep = pending.pop(k)
                    r = busy[cpu] = Run(scene, e, cpu, f'{e}.{rep}.{time.time_ns()}', done)
                    r.rep = rep
                elif any(not r.filler for r in busy.values()):
                    e = next(fill)
                    busy[cpu] = Run(scene, e, cpu, f'fill.{e}.{time.time_ns()}', done, filler=True)
            r = done.get()
            del busy[r.cpu]
            if r.filler:
                r.cleanup()
                continue
            ld = r.load(lanes)
            meta = dict(scene=scene, engine=r.engine, rep=r.rep, token=r.tag, cpu=r.cpu, lanes=lanes, sweep=sweep,
                        wall_s=r.wall, cpu_s=r.own, maxrss_kb=r.rss, neighbours_end=len(busy), canaries=cs,
                        clean=clean(ld), **ld)
            try:
                d, meta['error'] = r.result(), None
            except Exception as exc:
                d, meta['error'] = None, str(exc)
            if d is not None:
                d['lane'] = dict(cpu=r.cpu, lanes=lanes, sweep=sweep)
                meta['metric'] = metric(scene, d)
                save(dest / f'{r.tag}.json', d)
            with open(R / 'raw/runs.jsonl', 'a') as f:
                f.write(json.dumps(meta) + '\n')
            key = (r.engine, r.rep)
            if d is not None and clean(ld):
                save(dest / f'{r.engine}.{r.rep}.json', d)
                used.setdefault(r.engine, set()).add(r.cpu)
                log(f'OK {scene} {r.engine} round {r.rep} CPU {r.cpu}: {meta["metric"]:.3f} '
                    f'(neighbours {len(busy)}, rss {r.rss // 1024} MiB)')
                continue
            pending.insert(0, key)
            if d is None:
                fails[key] = fails.get(key, 0) + 1
                log(f'FAILED {scene} {r.engine} round {r.rep}: {meta["error"]}')
                if fails[key] >= 3:
                    stop(busy, done)
                    raise RuntimeError(meta['error'])
            else:
                log(f'foreign {scene} {r.engine} round {r.rep}: {ld}; retry: {ps()}')
        stop(busy, done)


def outliers(scene, engines):
    dest, flagged = R / 'raw' / scene, []
    for e in engines:
        vals = [metric(scene, json.loads((dest / f'{e}.{r}.json').read_text())) for r in (1, 2, 3)]
        for i, v in enumerate(vals):
            if all(abs(v / u - 1) > .05 for k, u in enumerate(vals) if k != i):
                flagged.append((e, i + 1))
    return flagged


def select(scene, engines):
    dest, sel = R / 'raw' / scene, {}
    for e in engines:
        runs = [json.loads((dest / f'{e}.{r}.json').read_text()) for r in (1, 2, 3)]
        sel[e] = sorted(runs, key=lambda d: metric(scene, d))[1]
    save(dest / 'selected.json', sel)
    log(f'{scene} selected ' + json.dumps({e: round(metric(scene, d), 3) for e, d in sel.items()}))


def measure(scene, lanes):
    dest = R / 'raw' / scene
    dest.mkdir(parents=True, exist_ok=True)
    engines, used = ORDER[scene], {}
    for e in engines:
        for r in (1, 2, 3):
            f = dest / f'{e}.{r}.json'
            if f.exists():
                used.setdefault(e, set()).add(json.loads(f.read_text())['lane']['cpu'])
    batch(scene, [(e, r) for r in (1, 2, 3) for e in engines], lanes, used, 0)
    for sweep in (1, 2, 3):
        flagged = outliers(scene, engines)
        if not flagged:
            break
        for e, rep in flagged:
            (dest / f'{e}.{rep}.json').rename(dest / f'{e}.{rep}.off{sweep}.json')
            log(f'rerun outlier pass {sweep}: {scene} {e} round {rep}')
        batch(scene, flagged, lanes, used, sweep)
    select(scene, engines)


def par8():
    """agent100k on eight workers, one per CPU of PAR_CPUS, nothing else
    running; per worker 16 s of requests at the engine's serial rate in
    this recording (raw/agent100k/selected.json)."""
    ag = json.loads((R / 'raw/agent100k/selected.json').read_text())
    nreq = {e: int(math.ceil(1600 / (d['ms'][-1] / 1000) * 16 / 100) * 100) for e, d in ag.items()}
    engines, dest = SHOWN + ([CTL] if HAVE_CTL else []), R / 'raw/par8'
    dest.mkdir(parents=True, exist_ok=True)

    def one(e, rep):
        target = dest / f'{e}.{rep}.json'
        if target.exists():
            return
        attempt = 0
        while True:
            attempt += 1
            cs = gate(f'par8 {e}')
            done, T0, token = queue.Queue(), time.monotonic(), f'{e}.{rep}.{time.time_ns()}'
            log(f'run par8 {e} round {rep} try {attempt}')
            for c in PAR_CPUS:
                Run('par8', e, c, f'{token}.{c}', done, n=nreq[e])
            ws, error, loads = [None] * len(PAR_CPUS), None, []
            for _ in PAR_CPUS:
                r = done.get()
                loads.append(r.load(PAR_CPUS))
                try:
                    d = r.result()
                    d['start_s'] = r.t0 - T0 + r.wall - d['ms'][-1] / 1000
                    ws[PAR_CPUS.index(r.cpu)] = d
                except Exception as exc:
                    error = f'worker {r.cpu}: {exc}'
            meta = dict(race='par8', engine=e, rep=rep, attempt=attempt, token=token, canaries=cs, error=error,
                        foreign_mine=max(x['foreign_mine'] for x in loads), steal=max(x['steal_mine'] for x in loads),
                        others=max(x['others'] for x in loads), others_peak=max(x['others_peak'] for x in loads),
                        clean=all(clean(x) for x in loads))
            if not error:
                starts = [w['start_s'] for w in ws]
                meta.update(skew_s=max(starts) - min(starts), shortest_s=min(w['ms'][-1] for w in ws) / 1000)
                if meta['shortest_s'] < WINDOW + meta['skew_s'] + 0.5:
                    error = meta['error'] = f'a worker stopped at {meta["shortest_s"]:.2f} s, before the window ended'
            with open(R / 'raw/runs.jsonl', 'a') as f:
                f.write(json.dumps(meta) + '\n')
            if error:
                log(f'FAILED par8 {e}: {error}')
                if attempt >= 3:
                    raise RuntimeError(error)
                continue
            d = dict(race='par8', engine=e, scenario='agent100k', n=nreq[e], cpus=PAR_CPUS, workers=ws, meta=meta)
            save(dest / f'{token}.json', d)
            if not all(clean(x) for x in loads):
                log(f'foreign par8 {e}: {meta}; retry: {ps()}')
                time.sleep(10)
                continue
            save(target, d)
            log(f'OK par8 {e} round {rep}: {metric("par8", d):.0f}')
            return

    with Locked():
        save(R / 'par8-n.json', nreq)
        for rep in (1, 2, 3):
            shift = (rep - 1) * max(1, len(engines) // 3)
            for e in engines[shift:] + engines[:shift]:
                one(e, rep)
        for sweep in (1, 2, 3):
            flagged = outliers('par8', engines)
            if not flagged:
                break
            for e, rep in flagged:
                (dest / f'{e}.{rep}.json').rename(dest / f'{e}.{rep}.off{sweep}.json')
                log(f'rerun outlier pass {sweep}: par8 {e} round {rep}')
                one(e, rep)
    select('par8', engines)


def sha256(p):
    h = hashlib.sha256()
    with open(p, 'rb') as f:
        for b in iter(lambda: f.read(1 << 20), b''):
            h.update(b)
    return h.hexdigest()


def prepare():
    """The recording's inputs, made once: the scenario files checked against
    bench/scenarios/SHA256SUMS, the V8 relay and score programs
    (make_suite.py on the suite pinned in bench/v8suite/REVISION; suite/,
    suite.sha256), the relay's counts (fixed-counts.json) and each plugin
    scenario's reference hash, the hash of the C host's check output
    (refs.json)."""
    global REFS, COUNTS
    for line in (HERE.parent / 'scenarios/SHA256SUMS').read_text().splitlines():
        want, name = line.split()
        assert sha256(QB / name) == want, f'{QB / name}: not the file of bench/scenarios/gen.py'
    rev = (HERE.parent / 'v8suite/REVISION').read_text().split()[0]
    assert (V8 / 'REVISION').read_text().strip() == rev, f'{V8}: run bench/v8suite/fetch.sh'
    if not (R / 'suite/fixed.js').exists():
        subprocess.run([sys.executable, str(HERE / 'make_suite.py'), str(V8), str(R / 'suite'), '--counts',
                        ','.join(str(n) for n in FIXED_COUNTS.values())], check=True)
        (R / 'suite.sha256').write_text(''.join(f'{sha256(f)}  {f.name}\n' for f in sorted((R / 'suite').iterdir())))
    f = R / 'fixed-counts.json'
    if not f.exists():
        f.write_text(json.dumps(FIXED_COUNTS) + '\n')
    f = R / 'refs.json'
    if not f.exists():
        refs = {}
        for sc in PLUGIN:
            chk = R / 'tmp' / f'ref.{sc}.check'
            subprocess.run([str(BIN / 'racehost'), '-engine', 'qjs', '-check', str(chk), '-dir', str(QB), '-scenario',
                            sc, '-n', '1'], env=ENV, stdout=subprocess.DEVNULL, check=True)
            refs[sc] = sha256(chk)
            chk.unlink()
        f.write_text(json.dumps(refs) + '\n')
    REFS = json.loads((R / 'refs.json').read_text())
    COUNTS = list(json.loads((R / 'fixed-counts.json').read_text()).values())


SAMPLER = None


def main():
    global SAMPLER
    os.sched_setaffinity(0, {ORCH_CPU})
    SAMPLER = Sampler()
    (R / 'tmp').mkdir(parents=True, exist_ok=True)
    (R / 'raw').mkdir(exist_ok=True)
    prepare()
    for scene in sys.argv[1:] or SCENES:
        if scene == 'par8':
            par8()
        else:
            K = check(scene)['K']
            lanes = SPREAD[:K + 1]
            log(f'{scene}: K={K}, lanes {lanes}')
            measure(scene, lanes)
        (R / f'DONE-{scene}').write_text(time.strftime('%Y-%m-%d %H:%M:%S') + '\n')


if __name__ == '__main__':
    main()
