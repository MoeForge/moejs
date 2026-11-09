"""Synthesized chiptune and sound effects (numpy): a 4-bar loop in A minor
at 150 bpm, square/triangle voices, noise drums, and short effects. The
music dips while the traffic lights count down and comes back in at GO."""

import wave

import numpy as np

SR = 44100


def mf(m):
    return 440.0 * 2 ** ((m - 69) / 12)


def tone(freq, dur, kind="sq", duty=0.5, vol=0.1, a=0.004, r=0.05):
    n = max(1, int(dur * SR)) if np.isscalar(freq) else len(freq)
    ph = freq * np.arange(n) / SR if np.isscalar(freq) else np.cumsum(freq[:n]) / SR
    ph %= 1.0
    if kind == "sq":
        w = np.where(ph < duty, 1.0, -1.0)
    elif kind == "tri":
        w = 4 * np.abs(ph - 0.5) - 1
    else:
        w = np.sin(2 * np.pi * ph)
    e = np.ones(n)
    na, nr = min(n, int(a * SR)), min(n, int(r * SR))
    if na:
        e[:na] = np.linspace(0, 1, na)
    if nr:
        e[-nr:] *= np.linspace(1, 0, nr)
    return w * e * vol


def noise(dur, vol, seed=0, hp=True):
    x = np.random.default_rng(seed).uniform(-1, 1, int(dur * SR))
    if hp:
        x = np.diff(x, prepend=0) * 0.5
    return x * vol


def decay(x, k):
    return x * np.exp(-np.arange(len(x)) / SR * k)


def add(buf, x, t0):
    i = int(t0 * SR)
    if i >= len(buf) or i + len(x) <= 0:
        return
    j = min(len(buf), i + len(x))
    buf[max(i, 0):j] += x[max(0, -i):j - i]


def sfx(name, kw, seed):
    if name == "pop":
        return tone(np.linspace(500, 1300, int(0.07 * SR)), 0.07, vol=0.09)
    if name == "slam":
        s = tone(np.linspace(150, 38, int(0.32 * SR)), 0.32, kind="sin", vol=0.55, r=0.1)
        n = decay(noise(0.07, 0.35, seed, hp=False), 40)
        s[:len(n)] += n
        return s
    if name == "type":
        return decay(noise(0.02, 0.18, seed), 250)
    if name == "blip":
        return tone(kw.get("f", 880), 0.055, duty=0.25, vol=0.07, r=0.02)
    if name == "coin":
        return np.concatenate([tone(mf(83), 0.07, vol=0.08, r=0.005), decay(tone(mf(88), 0.35, vol=0.08), 8)])
    if name == "beep":
        return tone(mf(69), 0.22, vol=0.11, r=0.02)
    if name == "go":
        return decay(tone(mf(81), 0.7, vol=0.13), 3)
    if name == "jingle":
        parts = [tone(mf(m), 0.09, vol=0.09, r=0.01) for m in (72, 76, 79, 84, 79, 84)]
        chord = sum(decay(tone(mf(m), 0.8, duty=0.5, vol=0.06), 3) for m in (84, 88, 91))
        return np.concatenate(parts + [chord])
    if name == "ding":
        return np.concatenate([tone(mf(84), 0.06, vol=0.06, r=0.005), decay(tone(mf(91), 0.3, vol=0.06), 10)])
    if name == "ff":
        return tone(np.geomspace(200, 1600, int(0.4 * SR)), 0.4, duty=0.25, vol=0.06)
    if name == "slow":
        return tone(np.geomspace(900, 160, int(0.45 * SR)), 0.45, duty=0.25, vol=0.06)
    if name == "shutter":
        click = decay(noise(0.03, 0.35, seed), 160)
        return np.concatenate([click, np.zeros(int(0.05 * SR)), decay(noise(0.05, 0.25, seed + 1), 90)])
    if name == "rev":
        f = np.concatenate([np.linspace(50, 140, int(0.35 * SR)), np.linspace(140, 100, int(0.15 * SR))])
        return tone(f, 0.5, duty=0.25, vol=0.08)
    if name == "whoosh":
        n = int(0.5 * SR)
        x = np.random.default_rng(seed).uniform(-1, 1, n)
        cut = np.concatenate([np.linspace(0.02, 0.35, n // 2), np.linspace(0.35, 0.03, n - n // 2)])
        out, y = np.empty(n), 0.0
        for i in range(n):
            y += cut[i] * (x[i] - y)
            out[i] = y
        return out * np.sin(np.linspace(0, np.pi, n)) ** 2 * 0.5
    if name == "confetti":
        rnd = np.random.default_rng(seed)
        out = np.zeros(int(1.4 * SR))
        for _ in range(60):
            add(out, decay(noise(0.01, rnd.uniform(0.03, 0.09), int(rnd.integers(1 << 30))), 400),
                rnd.uniform(0, 1.35))
        return out
    raise ValueError(name)


def music(N, gains):
    """gains: layer -> [(t, gain)] breakpoints, linearly interpolated."""
    bar, s16 = 1.6, 0.1
    roots = [45, 41, 48, 43]
    chords = [[57, 60, 64, 69], [53, 57, 60, 65], [55, 60, 64, 67], [55, 59, 62, 67]]
    lead = [[76, None, 74, 72, 69, None, 72, 74], [72, None, 69, None, 65, 69, 72, None],
            [76, 79, 76, 72, 74, None, 76, None], [74, None, 71, 67, 71, 74, 79, None]]
    n = int(4 * bar * SR)
    L = {k: np.zeros(n) for k in ("bass", "arp", "lead", "drums")}
    for b in range(4):
        t0 = b * bar
        for k in range(8):
            m = roots[b] + (12 if k % 2 else 0)
            add(L["bass"], tone(mf(m), 0.19, kind="tri", vol=0.22, r=0.03), t0 + k * 0.2)
            if lead[b][k] is not None:
                add(L["lead"], tone(mf(lead[b][k]), 0.17, vol=0.05, r=0.05), t0 + k * 0.2)
        for k in range(16):
            add(L["arp"], tone(mf(chords[b][k % 4] + 12), 0.09, duty=0.125, vol=0.035, r=0.02), t0 + k * s16)
        for beat in range(4):
            tb = t0 + beat * 0.4
            if beat % 2 == 0:
                add(L["drums"], tone(np.geomspace(160, 45, int(0.14 * SR)), 0.14, kind="sin", vol=0.35, r=0.04), tb)
            else:
                add(L["drums"], decay(noise(0.12, 0.22, beat + b * 4), 25), tb)
            add(L["drums"], decay(noise(0.03, 0.08, 100 + beat + b * 4), 120), tb + 0.2)
    tt = np.arange(N) / SR
    out = np.zeros(N)
    for k, x in L.items():
        g = gains[k]
        out += np.tile(x, N // n + 1)[:N] * np.interp(tt, [p[0] for p in g], [p[1] for p in g])
    return out


def hum_track(samples, N):
    ts = np.array([s[0] for s in samples])
    f = np.interp(np.arange(N) / SR, ts, [s[1] for s in samples])
    v = np.interp(np.arange(N) / SR, ts, [s[2] for s in samples])
    ph = (np.cumsum(f) / SR) % 1.0
    w = np.where(ph < 0.25, 1.0, -1.0)
    return np.convolve(w, np.ones(24) / 24, mode="same") * v


def gains_for(dur, drums_in, races):
    """Layer gains: drums enter at drums_in; for each (lights, go) the
    music drops to a quiet bass bed and every layer is back at GO."""
    lay = {"bass": [(0, 0.9)], "arp": [(0, 1.0)], "drums": [(0, 0.0), (drums_in, 0.0), (drums_in + 0.05, 1.0)],
           "lead": [(0, 0.0)]}
    lead_on = False
    for lights, go in races:
        for k, quiet in (("bass", 0.35), ("arp", 0.0), ("drums", 0.0), ("lead", 0.0)):
            cur = lay[k][-1][1] if k != "lead" or lead_on else 0.0
            lay[k] += [(lights - 0.3, cur), (lights, quiet), (go, quiet), (go + 0.05, 1.0)]
        lead_on = True
    for k in lay:
        lay[k].append((dur, lay[k][-1][1]))
    return lay


def write_mix(path, dur, events, hums, gains):
    N = int(dur * SR) + SR
    buf = np.zeros(N)
    for k, (t, name, kw) in enumerate(events):
        add(buf, sfx(name, kw, k + 1), t)
    fade = np.interp(np.arange(N) / SR, [0, dur - 1.6, dur], [1.0, 1.0, 0.0])
    mix = music(N, gains) * 0.8 * fade + buf + hum_track(hums, N)
    mix = np.tanh(mix * 1.1) / np.tanh(1.1)
    mix *= 0.89 / max(1e-9, np.abs(mix).max())
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes((mix * 32767).astype(np.int16).tobytes())
    return float(np.sqrt(np.mean(mix ** 2)))
