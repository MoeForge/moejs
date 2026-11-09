// The base64 benchmark, the same script in moejs (base64bench -js) and in
// Node.js (node base64-bench.js). Each case encodes or decodes the base64
// of n random bytes (a fixed seed) and prints one line of JSON with its
// median ns/op over five samples, after a warm-up. Cases an engine does
// not have (Uint8Array.fromBase64 and toBase64 before V8 14) print
// "skip".
const now = typeof benchNow === "function" ? benchNow : () => performance.now();
const only = typeof benchOnly === "string" ? benchOnly : (globalThis.process?.env?.ONLY ?? "");
const engine = typeof benchEngine === "string" ? benchEngine : "node";
const SIZES = [16, 256, 4096, 65536, 1048576];

// mulberry32
function rng(seed) {
  return () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return (t ^ (t >>> 14)) >>> 0;
  };
}

function randomBytes(n, seed) {
  const r = rng(seed);
  const b = new Uint8Array(n);
  for (let i = 0; i < n; i++) b[i] = r() & 255;
  return b;
}

// The setup's own encoder, so that no implementation under test makes
// its inputs.
const STD = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
const URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
function encode(b, url) {
  const abc = url ? URL : STD;
  const parts = [];
  let i = 0;
  for (; i + 3 <= b.length; i += 3) {
    const v = (b[i] << 16) | (b[i + 1] << 8) | b[i + 2];
    parts.push(abc[v >> 18] + abc[(v >> 12) & 63] + abc[(v >> 6) & 63] + abc[v & 63]);
  }
  const rest = b.length - i;
  if (rest > 0) {
    const v = (b[i] << 16) | (rest > 1 ? b[i + 1] << 8 : 0);
    let s = abc[v >> 18] + abc[(v >> 12) & 63];
    s += rest > 1 ? abc[(v >> 6) & 63] : url ? "" : "=";
    s += url ? "" : "=";
    parts.push(s);
  }
  return parts.join("");
}

function latin1(b) {
  const parts = [];
  for (let i = 0; i < b.length; i += 4096) parts.push(String.fromCharCode.apply(null, b.subarray(i, i + 4096)));
  return parts.join("");
}

function sameBytes(a, b) {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

const hasU8 = typeof Uint8Array.fromBase64 === "function" && typeof Uint8Array.prototype.toBase64 === "function";

// op name -> setup(bytes) returning [fn, check]
const OPS = {
  "atob": (b) => {
    const s = encode(b, false), want = latin1(b);
    return [() => atob(s), (r) => r === want];
  },
  "btoa": (b) => {
    const s = latin1(b), want = encode(b, false);
    return [() => btoa(s), (r) => r === want];
  },
  "Uint8Array.prototype.toBase64": hasU8 && ((b) => {
    const want = encode(b, false);
    return [() => b.toBase64(), (r) => r === want];
  }),
  "Uint8Array.fromBase64": hasU8 && ((b) => {
    const s = encode(b, false);
    return [() => Uint8Array.fromBase64(s), (r) => sameBytes(r, b)];
  }),
  "Buffer.from(s, \"base64\")": (b) => {
    const s = encode(b, false);
    return [() => Buffer.from(s, "base64"), (r) => sameBytes(r, b)];
  },
  "buf.toString(\"base64\")": (b) => {
    const buf = Buffer.from(b), want = encode(b, false);
    return [() => buf.toString("base64"), (r) => r === want];
  },
  "Buffer.from(s, \"base64url\")": (b) => {
    const s = encode(b, true);
    return [() => Buffer.from(s, "base64url"), (r) => sameBytes(r, b)];
  },
  "buf.toString(\"base64url\")": (b) => {
    const buf = Buffer.from(b), want = encode(b, true);
    return [() => buf.toString("base64url"), (r) => r === want];
  },
};

let sink = 0;

// timeN runs fn n times and returns the ns per call.
function timeN(fn, n) {
  const t0 = now();
  for (let i = 0; i < n; i++) sink += fn().length;
  return ((now() - t0) * 1e6) / n;
}

function median(xs) {
  const s = xs.slice().sort((a, b) => a - b);
  return s[s.length >> 1];
}

for (const [op, setup] of Object.entries(OPS)) {
  for (const size of SIZES) {
    const name = `${op} ${size}`;
    if (only && !name.includes(only)) continue;
    if (!setup) {
      console.log(JSON.stringify({ engine, op, size, skip: true }));
      continue;
    }
    const [fn, check] = setup(randomBytes(size, size));
    if (!check(fn())) throw new Error(`${name}: wrong result`);
    // Calibrate to about 30 ms a sample, then warm up for about 100 ms.
    let n = 1;
    for (;;) {
      const t0 = now();
      for (let i = 0; i < n; i++) sink += fn().length;
      if (now() - t0 > 10) break;
      n *= 2;
    }
    n = Math.max(1, Math.round((n * 30) / Math.max(1e-3, timeN(fn, n) * n / 1e6)));
    timeN(fn, n); timeN(fn, n); timeN(fn, n);
    const samples = [];
    for (let k = 0; k < 5; k++) samples.push(timeN(fn, n));
    console.log(JSON.stringify({ engine, op, size, n, ns: median(samples), min: Math.min(...samples), max: Math.max(...samples) }));
  }
}
if (sink < 0) console.log(sink);
