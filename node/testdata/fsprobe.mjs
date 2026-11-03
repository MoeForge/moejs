// The cases of the differential test of node:fs against Node.js: the
// same module runs in Node.js (fsgen.mjs) and in moejs (fs_diff_test.go),
// over the same tree (fstree.json), with the working directory at the
// tree's root. Paths under the root come out as <root>.

export async function probeRead(fs, fsp, Buffer, root) {
  const out = {};
  const norm = (v) => (typeof v === 'string' ? v.split(root).join('<root>') : v);
  const errInfo = (e) => ({
    name: e.name, code: e.code, errno: e.errno, syscall: e.syscall,
    path: norm(e.path), dest: norm(e.dest), message: norm(e.message), keys: Object.keys(e),
  });
  const val = (v) => {
    if (v === undefined) return { undefined: true };
    if (typeof v === 'string') return norm(v);
    if (Buffer.isBuffer(v)) return { buffer: norm(v.toString('latin1')) };
    if (Array.isArray(v)) return v.map(val);
    if (v instanceof fs.Stats) {
      const o = { keys: Object.keys(v), ctor: v.constructor.name };
      for (const k of Object.keys(v)) o[k] = v[k];
      for (const m of ['isFile', 'isDirectory', 'isSymbolicLink', 'isFIFO', 'isSocket', 'isBlockDevice', 'isCharacterDevice']) o[m] = v[m]();
      o.mtime = v.mtime.getTime();
      o.mtimeType = Object.prototype.toString.call(v.mtime);
      return o;
    }
    if (v && typeof v === 'object' && 'parentPath' in v) {
      return { dirent: val(v.name), parentPath: norm(v.parentPath), keys: Object.keys(v),
        file: v.isFile(), dir: v.isDirectory(), link: v.isSymbolicLink() };
    }
    return v;
  };
  const forms = async (name, fn, args, only) => {
    if (!only || only.includes('sync')) {
      try { out[name + ' sync'] = { ok: val(fs[fn + 'Sync'](...args)) }; } catch (e) { out[name + ' sync'] = { err: errInfo(e) }; }
    }
    if (!only || only.includes('promise')) {
      try { out[name + ' promise'] = { ok: val(await fsp[fn](...args)) }; } catch (e) { out[name + ' promise'] = { err: errInfo(e) }; }
    }
    if (!only || only.includes('callback')) {
      out[name + ' callback'] = await new Promise((res) => {
        try {
          fs[fn](...args, (e, v) => res(e ? { err: errInfo(e) } : { ok: val(v) }));
        } catch (e) { res({ threw: errInfo(e) }); }
      });
    }
  };

  // readFile
  await forms('readFile file', 'readFile', ['file.txt']);
  await forms('readFile utf8', 'readFile', ['file.txt', 'utf8']);
  await forms('readFile utf8 multibyte', 'readFile', ['utf8.txt', 'utf8']);
  await forms('readFile bad utf8', 'readFile', ['bad.bin', 'utf8']);
  await forms('readFile latin1', 'readFile', ['utf8.txt', 'latin1']);
  await forms('readFile base64', 'readFile', ['utf8.txt', { encoding: 'base64' }]);
  await forms('readFile hex', 'readFile', ['bad.bin', { encoding: 'hex', flag: 'r' }]);
  await forms('readFile ucs2', 'readFile', ['utf8.txt', 'ucs2']);
  await forms('readFile ENOENT', 'readFile', ['nope.txt']);
  await forms('readFile EISDIR', 'readFile', ['dir']);
  await forms('readFile EISDIR utf8', 'readFile', ['dir', 'utf8']);
  await forms('readFile ENOTDIR', 'readFile', ['file.txt/x']);
  await forms('readFile trailing slash', 'readFile', ['file.txt/']);
  await forms('readFile link', 'readFile', ['link.txt', 'utf8']);
  await forms('readFile through linkdir', 'readFile', ['linkdir/a.txt', 'utf8']);
  await forms('readFile dangling', 'readFile', ['dangling']);
  await forms('readFile loop', 'readFile', ['loop1']);
  await forms('readFile dotdot', 'readFile', ['dir/../file.txt', 'utf8']);
  await forms('readFile through link up', 'readFile', ['src/up/a.txt', 'utf8']);
  await forms('readFile dot slash', 'readFile', ['./dir/sub/b.ts', 'utf8']);
  await forms('readFile absolute', 'readFile', [root + '/file.txt', 'utf8']);
  await forms('readFile buffer path', 'readFile', [Buffer.from('file.txt'), 'utf8']);
  await forms('readFile bad encoding', 'readFile', ['file.txt', 'nope']);
  await forms('readFile bad options', 'readFile', ['file.txt', 12]);
  await forms('readFile bad path', 'readFile', [{}]);
  await forms('readFile NUL', 'readFile', ['a\0b']);
  await forms('readFile empty path', 'readFile', ['']);
  await forms('readFile bad flag', 'readFile', ['file.txt', { flag: 'q' }]);

  // readdir
  await forms('readdir root', 'readdir', ['.']);
  await forms('readdir dir', 'readdir', ['dir']);
  await forms('readdir dir slash', 'readdir', ['dir/']);
  await forms('readdir types', 'readdir', ['.', { withFileTypes: true }]);
  await forms('readdir recursive', 'readdir', ['src', { recursive: true }]);
  await forms('readdir recursive root', 'readdir', ['.', { recursive: true }]);
  await forms('readdir recursive types', 'readdir', ['src', { recursive: true, withFileTypes: true }]);
  await forms('readdir buffer', 'readdir', ['dir', 'buffer']);
  await forms('readdir hex', 'readdir', ['dir', { encoding: 'hex' }]);
  await forms('readdir ENOENT', 'readdir', ['nope']);
  await forms('readdir ENOTDIR', 'readdir', ['file.txt']);
  await forms('readdir link', 'readdir', ['linkdir']);
  await forms('readdir empty', 'readdir', ['empty']);
  await forms('readdir bad recursive', 'readdir', ['.', { recursive: 'yes' }]);

  // stat, lstat
  for (const p of ['file.txt', 'dir', 'link.txt', 'linkdir', 'dangling', 'nope', 'file.txt/x', 'loop1', 'ro.txt', 'empty']) {
    await forms('stat ' + p, 'stat', [p]);
    await forms('lstat ' + p, 'lstat', [p]);
  }
  await forms('stat throwIfNoEntry', 'stat', ['nope', { throwIfNoEntry: false }], ['sync']);
  await forms('stat throwIfNoEntry ENOTDIR', 'stat', ['file.txt/x', { throwIfNoEntry: false }], ['sync']);
  await forms('stat bad throwIfNoEntry', 'stat', ['nope', { throwIfNoEntry: 1 }], ['sync']);

  // access, exists
  await forms('access', 'access', ['file.txt']);
  await forms('access R_OK', 'access', ['file.txt', fs.constants.R_OK]);
  await forms('access W_OK ro', 'access', ['ro.txt', fs.constants.W_OK]);
  await forms('access X_OK', 'access', ['file.txt', fs.constants.X_OK]);
  await forms('access ENOENT', 'access', ['nope']);
  await forms('access bad mode', 'access', ['file.txt', 8]);
  await forms('access bad mode type', 'access', ['file.txt', 'x']);
  out['existsSync'] = ['file.txt', 'nope', 'dir', 'dangling', 'linkdir', '', 'file.txt/'].map((p) => fs.existsSync(p));
  out['existsSync bad'] = [fs.existsSync(), fs.existsSync(123)];
  out['exists'] = await new Promise((res) => fs.exists('file.txt', (a) => fs.exists('nope', (b) => res([a, b]))));

  // realpath, readlink
  for (const p of ['link.txt', 'linkdir/sub', '.', 'dir/../dir2', 'src/up', 'src/up/sub/b.ts']) {
    await forms('realpath ' + p, 'realpath', [p]);
  }
  await forms('realpath ENOENT', 'realpath', ['nope/x']);
  await forms('realpath dangling', 'realpath', ['dangling']);
  await forms('realpath buffer', 'realpath', ['link.txt', 'buffer']);
  for (const p of ['link.txt', 'linkdir', 'src/up', 'file.txt', 'nope']) {
    await forms('readlink ' + p, 'readlink', [p]);
  }

  // glob: the matches as a set; moejs sorts them, Node.js does not.
  const globs = ['**/*.ts', '**', '*', '**/*', 'dir/**', 'dir/**/*', '**/a.txt', 'linkdir/*', 'linkdir/**',
    '.hidden/*', '**/.dot.ts', '*/', '**/', 'dir/*/b.ts', '{dir,dir2}/*', 'dir/{a,b}.txt', 'dir/[ab].txt',
    'dir/?.txt', 'nope/*', 'file.txt', 'file.txt/', './dir/*', 'dir/../dir/a.txt', 'DIR/*', '**/sub', '**/sub/',
    'dir/sub/**', '.*', '**/.*', '+(dir|dir2)/*', '!(dir)', '[d]ir/a.txt', 'dir/[!a].txt', 'src/**/*.{ts,tsx}',
    'src/*/*.ts', 'src/**/?(*.)ts', 'src/a/*(o|n|e).ts', '*/a.txt', 'src/**/[[:lower:]]*.ts', 'src/up/*',
    'src/{a,c}/**/*.ts', '**/*.md', 'src/a/b/*'];
  for (const g of globs) {
    try { out['glob ' + g] = { ok: fs.globSync(g).map(norm).sort() }; } catch (e) { out['glob ' + g] = { err: errInfo(e) }; }
  }
  out['glob exclude'] = fs.globSync('**/*', { exclude: ['dir/**', 'src/a/**'] }).sort();
  out['glob exclude fn'] = fs.globSync('src/**/*.ts', { exclude: (p) => p.includes('b') }).sort();
  out['glob cwd'] = fs.globSync('*.ts', { cwd: 'src/a' }).sort();
  out['glob array'] = fs.globSync(['dir/*', 'dir/a.txt', 'src/c/*']).sort();
  out['glob types'] = fs.globSync('dir/*', { withFileTypes: true }).map((d) => [d.name, d.parentPath, d.isFile(), d.isDirectory()]).sort();
  out['glob follow'] = fs.globSync('**/*.ts', { followSymlinks: true }).sort();
  out['glob promise'] = (await Array.fromAsync(fsp.glob('src/**/*.ts'))).sort();
  out['glob callback'] = await new Promise((res) => fs.glob('src/**/*.ts', (e, m) => res(e ? errInfo(e) : m.sort())));
  out['glob bad'] = (() => { try { fs.globSync(1); return 'no error'; } catch (e) { return errInfo(e); } })();
  return out;
}

export async function probeWrite(fs, fsp, Buffer, root) {
  const out = {};
  const norm = (v) => (typeof v === 'string' ? v.split(root).join('<root>') : v);
  const errInfo = (e) => ({
    name: e.name, code: e.code, errno: e.errno, syscall: e.syscall,
    path: norm(e.path), dest: norm(e.dest), message: norm(e.message), keys: Object.keys(e),
  });
  const t = async (name, f) => {
    try { const v = await f(); out[name] = { ok: v === undefined ? 'undefined' : v }; } catch (e) { out[name] = { err: errInfo(e) }; }
  };
  await t('writeFile', () => fs.writeFileSync('new.txt', 'data'));
  await t('writeFile buffer', () => fs.writeFileSync('new.bin', Buffer.from([1, 2, 3])));
  await t('writeFile encoding', () => fs.writeFileSync('new.hex', '616263', 'hex'));
  await t('writeFile Uint8Array', () => fs.writeFileSync('new.u8', new Uint8Array([65, 66])));
  await t('writeFile EISDIR', () => fs.writeFileSync('dir', 'x'));
  await t('writeFile ENOENT', () => fs.writeFileSync('nope/x', 'x'));
  await t('writeFile wx EEXIST', () => fs.writeFileSync('file.txt', 'x', { flag: 'wx' }));
  await t('writeFile bad data', () => fs.writeFileSync('x.txt', 12));
  await t('writeFile through link', () => fs.writeFileSync('link.txt', 'via link'));
  await t('appendFile', () => fs.appendFileSync('new.txt', '+more'));
  await t('appendFile new', () => fs.appendFileSync('app.txt', 'a'));
  await t('appendFile EISDIR', () => fs.appendFileSync('dir', 'a'));
  await t('mkdir', () => fs.mkdirSync('m1'));
  await t('mkdir EEXIST', () => fs.mkdirSync('dir'));
  await t('mkdir ENOENT', () => fs.mkdirSync('nope/x'));
  await t('mkdir recursive', () => fs.mkdirSync('m2/a/b', { recursive: true }));
  await t('mkdir recursive exists', () => fs.mkdirSync('dir/sub', { recursive: true }));
  await t('mkdir recursive partial', () => fs.mkdirSync('dir/n1/n2', { recursive: true }));
  await t('mkdir recursive ENOTDIR', () => fs.mkdirSync('file.txt/a/b', { recursive: true }));
  await t('mkdir mode', () => fs.mkdirSync('m3', 0o700));
  await t('mkdir mode string', () => fs.mkdirSync('m5', '750'));
  await t('rmdir ENOTEMPTY', () => fs.rmdirSync('dir'));
  await t('rmdir ENOENT', () => fs.rmdirSync('nope'));
  await t('rmdir ENOTDIR', () => fs.rmdirSync('file.txt'));
  await t('rmdir', () => fs.rmdirSync('m1'));
  await t('unlink ENOENT', () => fs.unlinkSync('nope'));
  await t('unlink EISDIR', () => fs.unlinkSync('dir2'));
  await t('unlink', () => fs.unlinkSync('new.u8'));
  await t('unlink link', () => fs.unlinkSync('dangling'));
  await t('rm ENOENT', () => fs.rmSync('nope'));
  await t('rm force', () => fs.rmSync('nope', { force: true }));
  await t('rm dir', () => fs.rmSync('dir2'));
  await t('rm recursive', () => fs.rmSync('dir2', { recursive: true }));
  await t('rm file', () => fs.rmSync('new.hex'));
  await t('rm bad force', () => fs.rmSync('nope', { force: 1 }));
  await t('rename', () => fs.renameSync('new.bin', 'renamed.bin'));
  await t('rename ENOENT', () => fs.renameSync('nope', 'x'));
  await t('rename ENOTEMPTY', () => fs.renameSync('m2', 'dir'));
  await t('rename dir', () => fs.renameSync('m3', 'm4'));
  await t('copyFile', () => fs.copyFileSync('file.txt', 'copy.txt'));
  await t('copyFile ENOENT', () => fs.copyFileSync('nope', 'x'));
  await t('copyFile EISDIR', () => fs.copyFileSync('dir', 'x'));
  await t('copyFile EXCL', () => fs.copyFileSync('file.txt', 'copy.txt', fs.constants.COPYFILE_EXCL));
  await t('copyFile bad mode', () => fs.copyFileSync('file.txt', 'copy2.txt', 9));
  await t('cp file', () => fs.cpSync('file.txt', 'cp.txt'));
  await t('cp ENOENT', () => fs.cpSync('nope', 'x'));
  await t('cp dir no recursive', () => fs.cpSync('src', 'srccopy'));
  await t('cp dir', () => fs.cpSync('src/a', 'acopy', { recursive: true }));
  await t('cp no force', () => fs.cpSync('ro.txt', 'cp.txt', { force: false }));
  await t('cp errorOnExist', () => fs.cpSync('ro.txt', 'cp.txt', { force: false, errorOnExist: true }));
  await t('cp self', () => fs.cpSync('src', 'src/inner', { recursive: true }));
  await t('promises writeFile', () => fsp.writeFile('p.txt', 'p'));
  await t('promises appendFile', () => fsp.appendFile('p.txt', 'q'));
  await t('promises mkdir', () => fsp.mkdir('pm/a', { recursive: true }));
  await t('promises rm', () => fsp.rm('pm', { recursive: true }));
  await t('promises rm ENOENT', () => fsp.rm('pm'));
  await t('promises rename', () => fsp.rename('p.txt', 'p2.txt'));
  await t('promises copyFile', () => fsp.copyFile('p2.txt', 'p3.txt'));
  await t('promises unlink', () => fsp.unlink('p3.txt'));
  await t('promises rmdir', () => fsp.rmdir('m4'));
  await t('promises cp', () => fsp.cp('acopy', 'acopy2', { recursive: true }));
  await t('callback writeFile', () => new Promise((res, rej) => fs.writeFile('cb.txt', 'cb', (e) => (e ? rej(e) : res()))));
  await t('callback mkdir', () => new Promise((res, rej) => fs.mkdir('cbd/x', { recursive: true }, (e, v) => (e ? rej(e) : res(v)))));
  await t('callback rm', () => new Promise((res, rej) => fs.rm('nope', (e) => (e ? rej(e) : res()))));
  // The tree afterwards: every path, and the content of the files.
  const tree = {};
  for (const p of fs.readdirSync('.', { recursive: true }).sort()) {
    const st = fs.lstatSync(p);
    tree[p] = st.isFile() ? fs.readFileSync(p, 'hex') : st.isDirectory() ? 'dir' : 'link';
  }
  out.tree = tree;
  out.modes = { m5: (fs.statSync('m5').mode & 0o777).toString(8), copy: (fs.statSync('copy.txt').mode & 0o777).toString(8),
    ro: (fs.statSync('cp.txt').mode & 0o777).toString(8) };
  return out;
}
