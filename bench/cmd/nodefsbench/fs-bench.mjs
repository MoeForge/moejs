// The node:fs and Buffer benchmark, the same script in Node.js and in
// moejs (nodefsbench): each case runs for about a second after a warm-up,
// and prints its time per call in microseconds as a JSON line.
//
//	nodefsbench -fixture DIR                      # writes the fixture tree
//	nodefsbench -js fs-bench.mjs -dir DIR         # the script in moejs
//	node fs-bench.mjs DIR                         # the script in Node.js
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import { Buffer } from 'node:buffer';

const dir = globalThis.benchDir ?? process.argv[2];
const only = globalThis.benchOnly ?? process.argv[3] ?? '';
const now = globalThis.benchNow ?? (() => performance.now());
const engine = globalThis.benchNow ? 'moejs' : 'node';
const flat = fs.readdirSync(dir + '/flat').map((n) => dir + '/flat/' + n);
const b64 = fs.readFileSync(dir + '/b64.bin').toString('base64');
const bin = fs.readFileSync(dir + '/b64.bin');

async function bench(name, f, batch = 1) {
  if (only && !name.includes(only)) return;
  let sink = 0;
  for (let i = 0; i < 3; i++) sink += await f();
  let n = 0;
  const start = now();
  let t = start;
  while (t - start < 1000) {
    for (let i = 0; i < 16; i++) sink += await f();
    n += 16;
    t = now();
  }
  const us = ((t - start) * 1000) / (n * batch);
  console.log(JSON.stringify({ engine, name, us: Number(us.toFixed(3)), n: n * batch, sink: sink > 0 }));
}

const sync = (f) => () => f();

await bench('readFileSync 4KiB utf8', sync(() => fs.readFileSync(dir + '/small.txt', 'utf8').length));
await bench('readFileSync 4KiB Buffer', sync(() => fs.readFileSync(dir + '/small.txt').length));
await bench('readFileSync 1MiB utf8', sync(() => fs.readFileSync(dir + '/big.txt', 'utf8').length));
await bench('readFileSync 1MiB Buffer', sync(() => fs.readFileSync(dir + '/big.txt').length));
await bench('await readFile 4KiB utf8', async () => (await fsp.readFile(dir + '/small.txt', 'utf8')).length);
await bench('await readFile 4KiB Buffer', async () => (await fsp.readFile(dir + '/small.txt')).length);
await bench('await readFile 1MiB utf8', async () => (await fsp.readFile(dir + '/big.txt', 'utf8')).length);
await bench('await readFile 1MiB Buffer', async () => (await fsp.readFile(dir + '/big.txt')).length);
await bench('readdirSync 1000 entries', sync(() => fs.readdirSync(dir + '/flat').length));
await bench('await readdir 1000 entries', async () => (await fsp.readdir(dir + '/flat')).length);
await bench('readdirSync recursive tree', sync(() => fs.readdirSync(dir + '/tree', { recursive: true }).length));
await bench('statSync x1000', sync(() => { let s = 0; for (const p of flat) s += fs.statSync(p).size; return s; }), 1000);
await bench('await stat x1000', async () => { let s = 0; for (const p of flat) s += (await fsp.stat(p)).size; return s; }, 1000);
await bench('globSync **/*.ts tree', sync(() => fs.globSync('**/*.ts', { cwd: dir + '/tree' }).length));
await bench('Buffer.from base64 64KiB', sync(() => Buffer.from(b64, 'base64').length));
await bench('toString base64 64KiB', sync(() => bin.toString('base64').length));
