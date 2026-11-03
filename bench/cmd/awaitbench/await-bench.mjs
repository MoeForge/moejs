// The await and promise benchmark, the same script in Node.js and in moejs
// (awaitbench): each case runs n units, doubling n until a run takes
// 200 ms, then takes the best of five runs, and prints its time per unit
// in nanoseconds as a JSON line.
//
//	awaitbench -js await-bench.mjs     # the script in moejs
//	node await-bench.mjs               # the script in Node.js
const only = globalThis.benchOnly ?? process.argv[2] ?? '';
const now = globalThis.benchNow ?? (() => performance.now());
const engine = globalThis.benchNow ? 'moejs' : 'node';

async function bench(name, run) {
  if (only && !name.includes(only)) return;
  let sink = await run(1000);
  let n = 1000;
  for (;;) {
    const start = now();
    sink += await run(n);
    if (now() - start >= 200) break;
    n *= 2;
  }
  let best = Infinity;
  for (let k = 0; k < 5; k++) {
    const start = now();
    sink += await run(n);
    best = Math.min(best, now() - start);
  }
  const ns = (best * 1e6) / n;
  console.log(JSON.stringify({ engine, name, ns: Number(ns.toFixed(1)), n, sink: sink > 0 }));
}

const resolved = Promise.resolve(1);
async function ret(x) { return x; }
const inc = (x) => x + 1;
async function* gen(n) { for (let i = 0; i < n; i++) yield i; }
function iter(n) {
  let i = 0;
  return {
    [Symbol.asyncIterator]() { return this; },
    next() { return Promise.resolve(i < n ? { value: i++, done: false } : { value: undefined, done: true }); },
  };
}

await bench('await value', async (n) => { let s = 0; for (let i = 0; i < n; i++) s += await i; return s; });
await bench('await resolved promise', async (n) => { let s = 0; for (let i = 0; i < n; i++) s += await resolved; return s; });
await bench('async call', async (n) => { let p; for (let i = 0; i < n; i++) p = ret(i); return p; });
await bench('await async call', async (n) => { let s = 0; for (let i = 0; i < n; i++) s += await ret(i); return s; });
await bench('then chain link', (n) => { let p = Promise.resolve(0); for (let i = 0; i < n; i++) p = p.then(inc); return p; });
await bench('new Promise + await', async (n) => { let s = 0; for (let i = 0; i < n; i++) s += await new Promise((res) => res(i)); return s; });
await bench('for await async generator', async (n) => { let s = 0; for await (const x of gen(n)) s += x; return s; });
await bench('for await async iterator', async (n) => { let s = 0; for await (const x of iter(n)) s += x; return s; });
await bench('for await array', async (n) => { const a = Array.from({ length: Math.min(n, 4096) }, (_, i) => i); let s = 0; for (let k = 0; k < n; k += a.length) for await (const x of a) s += x; return s; });
await bench('await setImmediate', async (n) => { let s = 0; for (let i = 0; i < n; i++) s += await new Promise((res) => setImmediate(res, 1)); return s; });
