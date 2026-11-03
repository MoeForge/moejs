// Runs the differential cases (fsprobe.mjs) in Node.js and prints their
// results as JSON: the expectations of fs_diff_test.go.
//
//	node fsgen.mjs > fs_node.json           # a tree of its own
//	node fsgen.mjs read <dir>               # the read cases over <dir>
//
// It builds its trees with the umask 022 that fs_diff_test.go sets too.
import * as fs from 'node:fs';
import * as path from 'node:path';
import * as os from 'node:os';
import { probeRead, probeWrite } from './fsprobe.mjs';

const spec = JSON.parse(fs.readFileSync(new URL('./fstree.json', import.meta.url), 'utf8'));

function unescape(s) {
  return Buffer.from(s.replace(/\\x([0-9a-f]{2})/g, (_, h) => String.fromCharCode(parseInt(h, 16))), 'latin1');
}

export function makeTree(dir) {
  for (const d of spec.dirs) fs.mkdirSync(path.join(dir, d), { recursive: true });
  for (const [f, content] of Object.entries(spec.files)) {
    const data = content.includes('\\x') ? unescape(content) : Buffer.from(content);
    fs.writeFileSync(path.join(dir, f), data);
  }
  for (const [l, target] of Object.entries(spec.links)) fs.symlinkSync(target, path.join(dir, l));
  for (const [f, mode] of Object.entries(spec.modes)) fs.chmodSync(path.join(dir, f), mode);
}

async function run(probe, dir) {
  const cwd = process.cwd();
  process.chdir(dir);
  try {
    return await probe(fs, fs.promises, Buffer, fs.realpathSync(dir));
  } finally {
    process.chdir(cwd);
  }
}

const [mode, dirArg] = process.argv.slice(2);
if (mode === 'read') {
  process.stdout.write(JSON.stringify(await run(probeRead, dirArg)));
} else {
  process.umask(0o022);
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'fsgen-'));
  const r = path.join(base, 'r');
  const w = path.join(base, 'w');
  fs.mkdirSync(r);
  fs.mkdirSync(w);
  makeTree(r);
  makeTree(w);
  const out = { read: await run(probeRead, r), write: await run(probeWrite, w) };
  fs.rmSync(base, { recursive: true, force: true });
  process.stdout.write(JSON.stringify(out, null, 1) + '\n');
}
