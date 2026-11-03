// The cases of the differential test of Buffer against Node.js: the same
// module runs in Node.js (bufgen.mjs) and in moejs (buffer_diff_test.go).

export function probeBuffer(Buffer) {
  const out = {};
  const hex = (b) => (Buffer.isBuffer(b) ? 'B:' + b.toString('hex') : b);
  const errInfo = (e) => ({ name: e.name, code: e.code, message: e.message });
  const t = (name, f) => {
    try { out[name] = { ok: hex(f()) }; } catch (e) { out[name] = { err: errInfo(e) }; }
  };
  const encodings = ['utf8', 'utf-8', 'UTF8', 'hex', 'base64', 'base64url', 'latin1', 'binary', 'ascii', 'ucs2', 'utf16le', 'UTF-16LE'];
  const strings = ['', 'hello', 'héllo wörld', '✓ 😀 \u{10FFFF}', 'a\uD800b', '\uDC00x', '\0\x7f\x80\xff',
    'aGVsbG8=', 'aGVs bG8', 'a-_b', 'ab+/', 'abc', 'zz12', 'deadBEEF', '=', 'YQ', 'YQ==YQ==', 'Āā'];
  for (const enc of encodings) {
    for (const s of strings) {
      t(`from ${enc} ${JSON.stringify(s)}`, () => Buffer.from(s, enc));
      t(`byteLength ${enc} ${JSON.stringify(s)}`, () => Buffer.byteLength(s, enc));
    }
  }
  const bytes = [[], [0], [0x68, 0x65, 0x6c, 0x6c, 0x6f], [0xff, 0xfe, 0xfd], [0xe2, 0x82], [0xe2, 0x82, 0x41],
    [0xc0, 0x80], [0xed, 0xa0, 0x80], [0xf0, 0x9f, 0x98, 0x80], [0xf0, 0x9f, 0x98], [0xf4, 0x90, 0x80, 0x80],
    [0x80, 0x41, 0xbf], [0, 1, 2, 3, 250, 251, 252, 253, 254, 255], [0x41, 0x00, 0x42]];
  for (const enc of encodings) {
    for (const b of bytes) {
      t(`toString ${enc} ${b.join(',')}`, () => Buffer.from(b).toString(enc));
    }
  }
  // Round trips of every byte value.
  const all = Buffer.alloc(256);
  for (let i = 0; i < 256; i++) all[i] = i;
  for (const enc of ['hex', 'base64', 'base64url', 'latin1', 'utf16le']) {
    t(`roundtrip ${enc}`, () => Buffer.from(all.toString(enc), enc).equals(all));
  }
  t('roundtrip utf8', () => Buffer.from(all.toString('utf8'), 'utf8'));
  t('roundtrip ascii', () => Buffer.from(all.toString('ascii'), 'ascii'));
  const big = Buffer.alloc(65536);
  for (let i = 0; i < big.length; i++) big[i] = (i * 31 + 7) & 255;
  t('big base64', () => Buffer.from(big.toString('base64'), 'base64').equals(big));
  t('big base64 head', () => big.toString('base64').slice(0, 40));

  // toString ranges
  const h = Buffer.from('hello world');
  for (const [s, e] of [[0, 5], [6], [-1, 3], [3, 100], [5, 2], [NaN, 4], ['2', '4'], [1.7, 4.2], [undefined, 3], [11], [100]]) {
    t(`toString utf8 ${s} ${e}`, () => h.toString('utf8', s, e));
  }
  t('toString no args', () => h.toString());
  t('toString undefined', () => h.toString(undefined));
  t('toString bad encoding', () => h.toString('nope'));
  t('toString null encoding', () => h.toString(null));
  t('toString number encoding', () => h.toString(5));

  // from: other sources
  t('from array', () => Buffer.from([1, 256, -1, 3.7, '5', null, 'x', 1e10]));
  t('from arraybuffer shares', () => { const ab = new ArrayBuffer(4); const b = Buffer.from(ab, 1, 2); b[0] = 9; return [...new Uint8Array(ab)].join(','); });
  t('from arraybuffer offset', () => Buffer.from(new ArrayBuffer(4), 5));
  t('from arraybuffer length', () => Buffer.from(new ArrayBuffer(4), 1, 9));
  t('from buffer copies', () => { const a = Buffer.from('ab'); const b = Buffer.from(a); b[0] = 0x7a; return a.toString() + b.toString(); });
  t('from uint16array', () => Buffer.from(new Uint16Array([1, 258, 65535])));
  t('from json', () => Buffer.from({ type: 'Buffer', data: [1, 2, 3] }));
  t('from array-like', () => Buffer.from({ length: 3, 0: 7, 1: 8 }));
  t('from object', () => Buffer.from({}));
  t('from number', () => Buffer.from(5));
  t('from null', () => Buffer.from(null));
  t('from valueOf', () => Buffer.from(new String('abc')));
  t('from toPrimitive', () => Buffer.from({ [Symbol.toPrimitive]: () => 'prim' }));
  t('from empty encoding', () => Buffer.from('abc', ''));
  t('from bad encoding', () => Buffer.from('abc', 'nope'));
  t('of', () => Buffer.of(1, 2, 300));

  // alloc
  t('alloc', () => Buffer.alloc(4));
  t('alloc fill number', () => Buffer.alloc(4, 257));
  t('alloc fill string', () => Buffer.alloc(5, 'ab'));
  t('alloc fill hex', () => Buffer.alloc(5, 'abcd', 'hex'));
  t('alloc fill bad hex', () => Buffer.alloc(5, 'zz', 'hex'));
  t('alloc fill buffer', () => Buffer.alloc(5, Buffer.from([1, 2])));
  t('alloc negative', () => Buffer.alloc(-1));
  t('alloc string', () => Buffer.alloc('4'));
  t('allocUnsafe', () => Buffer.allocUnsafe(3).length);
  t('allocUnsafeSlow', () => Buffer.allocUnsafeSlow(3).length);

  // statics
  t('concat', () => Buffer.concat([Buffer.from('ab'), new Uint8Array([99]), Buffer.from('d')]));
  t('concat length', () => Buffer.concat([Buffer.from('abc'), Buffer.from('def')], 4));
  t('concat longer', () => Buffer.concat([Buffer.from('ab')], 4));
  t('concat empty', () => Buffer.concat([]));
  t('concat bad', () => Buffer.concat([Buffer.from('a'), 'b']));
  t('concat not array', () => Buffer.concat('ab'));
  t('isBuffer', () => [Buffer.isBuffer(Buffer.alloc(1)), Buffer.isBuffer(new Uint8Array(1)), Buffer.isBuffer('x'), Buffer.isBuffer(null)]);
  t('isEncoding', () => ['utf8', 'UTF-8', 'hex', 'nope', '', 'buffer', 'binary', 'ucs-2'].map((e) => Buffer.isEncoding(e)));
  t('byteLength buffer', () => Buffer.byteLength(Buffer.alloc(7)));
  t('byteLength arraybuffer', () => Buffer.byteLength(new ArrayBuffer(9)));
  t('byteLength bad', () => Buffer.byteLength(5));
  t('compare', () => [Buffer.compare(Buffer.from('a'), Buffer.from('b')), Buffer.compare(Buffer.from('b'), Buffer.from('a')), Buffer.compare(Buffer.from('ab'), Buffer.from('ab')), Buffer.compare(Buffer.from('a'), Buffer.from('ab'))]);
  t('compare bad', () => Buffer.compare('a', Buffer.from('a')));
  t('poolSize', () => Buffer.poolSize);

  // instance methods
  const b = Buffer.from('abcdef');
  t('slice', () => b.slice(1, 3));
  t('slice negative', () => b.slice(-2));
  t('slice shares', () => { const c = Buffer.from('xyz'); c.slice(1)[0] = 0x41; return c.toString(); });
  t('subarray', () => b.subarray(2, 4));
  t('subarray isBuffer', () => Buffer.isBuffer(b.subarray(1)));
  t('subarray shares', () => { const c = Buffer.from('xyz'); c.subarray(1)[0] = 0x41; return c.toString(); });
  t('map isBuffer', () => Buffer.isBuffer(b.map((x) => x + 1)));
  t('equals', () => [b.equals(Buffer.from('abcdef')), b.equals(Buffer.from('abc')), b.equals(new Uint8Array([97, 98, 99, 100, 101, 102]))]);
  t('equals bad', () => b.equals('abcdef'));
  t('compare method', () => [b.compare(Buffer.from('abc')), b.compare(Buffer.from('b')), b.compare(Buffer.from('cd'), 0, 2, 2, 4), b.compare(Buffer.from('xx'), 0, 2, 3, 3)]);
  t('compare range', () => b.compare(Buffer.from('a'), 0, 5));
  for (const [v, o, e] of [['c'], ['cd'], ['x'], [99], [99, 3], ['b', -5], ['', 2], ['', 100], [Buffer.from('ef')], ['ef', 'latin1'], [0x163], ['a', 1], ['f', -1], ['abcdefg'], ['6566', 0, 'hex']]) {
    t(`indexOf ${JSON.stringify([v, o, e])}`, () => b.indexOf(v, o, e));
    t(`lastIndexOf ${JSON.stringify([v, o, e])}`, () => Buffer.from('abcabc').lastIndexOf(v, o, e));
    t(`includes ${JSON.stringify([v, o, e])}`, () => b.includes(v, o, e));
  }
  t('indexOf bad', () => b.indexOf({}));
  t('indexOf bad encoding', () => b.indexOf('a', 0, 'nope'));
  t('toJSON', () => JSON.stringify(Buffer.from([1, 2, 3])));
  t('toJSON empty', () => JSON.stringify(Buffer.alloc(0)));
  t('JSON round trip', () => Buffer.from(JSON.parse(JSON.stringify(Buffer.from('hi')))));
  for (const args of [[0x41], ['xy'], ['xy', 1], ['xy', 1, 4], ['6162', 'hex'], ['6162', 0, 3, 'hex'], [0x41, 2, 4], [Buffer.from([1, 2])], ['', 1], [257], [0x41, 5, 2], [0x41, 7]]) {
    t(`fill ${JSON.stringify(args)}`, () => Buffer.alloc(6).fill(...args));
  }
  t('fill bad encoding', () => Buffer.alloc(3).fill('a', 'nope'));
  for (const args of [['hi'], ['hi', 2], ['hi', 2, 1], ['hello', 4], ['6869', 'hex'], ['6869', 1, 'hex'], ['héllo', 0], ['€€€', 0, 5], ['€€', 5], ['hi', 7], ['hi', -1], ['hi', 1.5], ['ab', 'ucs2'], ['abc', 0, 3, 'ucs2'], ['aGk=', 'base64']]) {
    t(`write ${JSON.stringify(args)}`, () => { const c = Buffer.alloc(6); const n = c.write(...args); return [n, c.toString('hex')]; });
  }
  t('write bad', () => Buffer.alloc(2).write(5));
  t('write bad encoding', () => Buffer.alloc(2).write('a', 'nope'));
  for (const args of [[Buffer.alloc(4)], [Buffer.alloc(4), 1], [Buffer.alloc(4), 1, 2], [Buffer.alloc(4), 0, 1, 3], [Buffer.alloc(2), 0, 0, 10], [Buffer.alloc(4), 10], [Buffer.alloc(4), -1], [Buffer.alloc(4), 0, 7], [Buffer.alloc(4), '1', '2']]) {
    t(`copy ${args.slice(1).join(',')}`, () => { const src = Buffer.from('wxyz'); const n = src.copy(...args); return [n, args[0].toString('hex')]; });
  }
  t('copy bad', () => Buffer.from('a').copy('b'));
  t('swap16', () => Buffer.from([1, 2, 3, 4]).swap16());
  t('swap32', () => Buffer.from([1, 2, 3, 4]).swap32());
  t('swap64', () => Buffer.from([1, 2, 3, 4, 5, 6, 7, 8]).swap64());
  t('swap16 bad', () => Buffer.from([1, 2, 3]).swap16());
  t('inspect', () => Buffer.from('hello')[Symbol.for('nodejs.util.inspect.custom')]());
  t('inspect long', () => Buffer.alloc(60)[Symbol.for('nodejs.util.inspect.custom')]());
  t('proto chain', () => [Object.getPrototypeOf(Buffer.prototype) === Uint8Array.prototype, Object.getPrototypeOf(Buffer) === Uint8Array, Buffer.from('a') instanceof Uint8Array, Buffer.name, Buffer.length]);
  t('length', () => [Buffer.from('héllo').length, Buffer.alloc(3).byteLength]);
  t('toLocaleString', () => Buffer.from('x').toLocaleString());
  t('new Buffer', () => new Buffer('abc'));
  t('new Buffer size', () => new Buffer(3));
  t('Buffer call', () => Buffer('ab'));

  // numbers
  const n = Buffer.from([0x01, 0x02, 0x03, 0x04, 0xff, 0xfe, 0xfd, 0xfc, 0x80, 0x00, 0x00, 0x00]);
  const reads = ['readUInt8', 'readUint8', 'readInt8', 'readUInt16LE', 'readUInt16BE', 'readInt16LE', 'readInt16BE',
    'readUInt32LE', 'readUInt32BE', 'readInt32LE', 'readInt32BE', 'readUint16LE', 'readUint32BE', 'readFloatLE', 'readFloatBE',
    'readDoubleLE', 'readDoubleBE'];
  for (const m of reads) {
    for (const o of [undefined, 0, 3, 4, 8, 11, 12, -1, 1.5, '1', NaN]) {
      t(`${m} ${o}`, () => n[m](o));
    }
  }
  for (const m of ['readUIntLE', 'readUIntBE', 'readIntLE', 'readIntBE']) {
    for (const [o, l] of [[0, 1], [0, 3], [2, 6], [6, 6], [0, 7], [0, 0], [undefined, 2], [1, 2.5]]) {
      t(`${m} ${o} ${l}`, () => n[m](o, l));
    }
  }
  const writes = [['writeUInt8', [255, 0], [256, 0], [-1, 0], [1, 6], [1.9, 0], ['7', 0]],
    ['writeInt8', [-128, 0], [127, 1], [128, 0], [-129, 0]],
    ['writeUInt16LE', [65535, 0], [65536, 0], [258, 4], [1, 5]], ['writeUInt16BE', [258, 0]],
    ['writeInt16LE', [-2, 0], [32768, 0]], ['writeInt16BE', [-2, 0]],
    ['writeUInt32LE', [0xdeadbeef, 0], [2 ** 32, 0], [-1, 0], [1, 3]], ['writeUInt32BE', [0xdeadbeef, 2]],
    ['writeInt32LE', [-1, 0], [2 ** 31, 0], [-(2 ** 31), 2]], ['writeInt32BE', [-2, 0]],
    ['writeFloatLE', [1.5, 0], [NaN, 0], [1e40, 2]], ['writeFloatBE', [-0.25, 0]],
    ['writeDoubleLE', [Math.PI, 0], [1, -1]], ['writeDoubleBE', [1e300, 0]],
    ['writeUint8', [9, 0]], ['writeUint16LE', [9, 0]], ['writeUint32BE', [9, 0]]];
  for (const [m, ...cases] of writes) {
    for (const args of cases) {
      t(`${m} ${JSON.stringify(args)}`, () => { const c = Buffer.alloc(8); const r = c[m](...args); return [r, c.toString('hex')]; });
    }
  }
  for (const [m, args] of [['writeUIntLE', [0x123456, 0, 3]], ['writeUIntBE', [0x123456, 1, 3]], ['writeIntLE', [-2, 0, 6]],
    ['writeIntBE', [-2, 0, 2]], ['writeUIntLE', [2 ** 24, 0, 3]], ['writeIntLE', [0, 0, 7]], ['writeUIntBE', [1, 6, 3]]]) {
    t(`${m} ${JSON.stringify(args)}`, () => { const c = Buffer.alloc(8); const r = c[m](...args); return [r, c.toString('hex')]; });
  }
  return out;
}
