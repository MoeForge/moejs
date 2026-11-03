// Edge cases of Buffer: the same module runs in Node.js (bufedgegen.mjs)
// and in moejs (buffer_edge_test.go). nb is the node:buffer namespace.
export function edgeBuffer(Buffer, nb, opts = {}) {
  const out = {};
  const norm = (m) => String(m).replace(/9007199254740991|2147483647/g, 'MAX');
  const show = (b) => {
    if (Buffer.isBuffer(b)) return 'B:' + b.toString('hex');
    if (b instanceof Uint8Array) return 'U:' + Buffer.from(b).toString('hex');
    if (b instanceof ArrayBuffer) return 'AB:' + Buffer.from(b).toString('hex');
    if (Array.isArray(b)) return b.map(show);
    return b;
  };
  const errInfo = (e) => (e && typeof e === 'object' && 'name' in e) ? { name: e.name, code: e.code, message: norm(e.message) } : { thrown: String(e) };
  const t = (name, f) => { try { out[name] = { ok: show(f()) }; } catch (e) { out[name] = { err: errInfo(e) }; } };
  const key = (a) => Buffer.isBuffer(a) ? 'B:' + a.toString('hex') : a instanceof Uint8Array ? 'U8' : a instanceof Uint16Array ? 'U16:' + a.length : a instanceof Float64Array ? 'F64' : a instanceof DataView ? 'DV' : a instanceof ArrayBuffer ? 'AB' : typeof a === 'symbol' ? 'sym' : (typeof a === 'object' && a !== null) ? JSON.stringify(a) + (a.valueOf !== Object.prototype.valueOf ? 'v' : '') : String(a);
  const keys = (args) => JSON.stringify(args.map(key));

  // --- 1. fixed-size and variable reads/writes on a view at byteOffset 4 of 16 bytes ---
  const mk = () => { const ab = new ArrayBuffer(16); const u = new Uint8Array(ab); for (let i = 0; i < 16; i++) u[i] = 0xf0 + i; return [ab, u, Buffer.from(ab, 4, 8)]; };
  const reads = ['readUInt8', 'readInt8', 'readUInt16LE', 'readUInt16BE', 'readInt16LE', 'readInt16BE', 'readUInt32LE', 'readUInt32BE', 'readInt32LE', 'readInt32BE', 'readFloatLE', 'readFloatBE', 'readDoubleLE', 'readDoubleBE'];
  const offs = [-0, 0, 4, 6, 7, 8, 9, -1, 0.5, 1e21, -1e21, Infinity, -Infinity, NaN, '2', null, true, 2 ** 31, 2 ** 32, 2 ** 53];
  for (const m of reads) for (const o of offs) t(`${m}(${String(o)})`, () => mk()[2][m](o));
  t('readUInt8(obj)', () => mk()[2].readUInt8({ valueOf: () => 0 }));
  t('readUInt8(undefined)', () => mk()[2].readUInt8(undefined));
  t('readUInt8()', () => mk()[2].readUInt8());
  const varOffs = [[0, 1], [7, 1], [7, 2], [2, 6], [3, 6], [-1, 1], [0, 6.5], [undefined, 1], [0, undefined], ['0', 2], [0, '2'], [NaN, 2], [0, NaN], [Infinity, 1], [0, Infinity], [0, 0], [0, 7], [0, -1], [-0, 3], [0.5, 3], [1e21, 1], [undefined, 9], [undefined, undefined]];
  for (const m of ['readUIntLE', 'readUIntBE', 'readIntLE', 'readIntBE', 'readUintLE']) for (const [o, l] of varOffs) t(`${m}(${String(o)},${String(l)})`, () => mk()[2][m](o, l));
  const wcases = [['writeUInt8', 1, 7], ['writeUInt8', 1, 8], ['writeUInt8', 1, -1], ['writeUInt8', 256, 0], ['writeUInt8', -1, 0], ['writeUInt8', 1.5, 0], ['writeUInt8', NaN, 0], ['writeUInt8', 1, 0.5], ['writeUInt8', 1, '0'], ['writeUInt8', 1, NaN], ['writeUInt8', 1, 9],
    ['writeUInt16LE', 1, 7], ['writeUInt16LE', 1, 6], ['writeUInt16LE', 65535.5, 0], ['writeUInt16LE', -0.5, 0], ['writeInt16LE', -0.5, 0], ['writeInt16LE', -32768.5, 0], ['writeInt16BE', 1, '0'],
    ['writeUInt32LE', 1, 5], ['writeUInt32LE', 1, 4], ['writeUInt32LE', 4294967295.5, 0], ['writeUInt32LE', -0.9, 0], ['writeInt32LE', -0.9, 0], ['writeInt32BE', 2147483647.5, 0], ['writeUInt32BE', 1, 1e21],
    ['writeFloatLE', 1, 5], ['writeFloatLE', '1.5', 0], ['writeFloatLE', 1, NaN], ['writeDoubleLE', 1, 1], ['writeDoubleLE', 1, 0], ['writeDoubleBE', Infinity, 0], ['writeDoubleBE', 1, '0'], ['writeDoubleBE', 1, 2 ** 32], ['writeDoubleBE', 1, -0]];
  for (const [m, v, o] of wcases) t(`${m}(${String(v)},${String(o)})`, () => { const [ab, u, b] = mk(); const r = b[m](v, o); return [r, Buffer.from(ab).toString('hex')]; });
  const vwcases = [['writeUIntLE', 1, 7, 1], ['writeUIntLE', 1, 7, 2], ['writeUIntLE', 1, 2, 6], ['writeUIntLE', 1, 3, 6], ['writeUIntLE', 2 ** 48, 0, 6], ['writeUIntLE', 2 ** 48 - 1, 0, 6], ['writeIntLE', -(2 ** 47), 0, 6], ['writeIntLE', -(2 ** 47) - 1, 0, 6], ['writeIntBE', -1, 0, 6], ['writeUIntBE', 0.5, 0, 1], ['writeUIntBE', NaN, 0, 3], ['writeUIntBE', 1, undefined, 3], ['writeUIntBE', 1, 0, undefined], ['writeUIntBE', 1, 0, 7], ['writeUIntBE', 1, 0, 0], ['writeUIntBE', 1, 0.5, 2], ['writeUIntBE', 1, '0', 2], ['writeUIntBE', '7', 0, 2], ['writeUIntBE', 1, 8, 1], ['writeUIntBE', 1, -1, 1], ['writeIntLE', 0x7fff, 0, 2], ['writeIntLE', 0x8000, 0, 2], ['writeUIntLE', 0x100, 0, 1], ['writeUIntLE', 2 ** 32, 0, 4], ['writeUIntBE', 2 ** 40 - 1, 1, 5], ['writeIntBE', -(2 ** 39), 1, 5]];
  for (const [m, v, o, l] of vwcases) t(`${m}(${String(v)},${String(o)},${String(l)})`, () => { const [ab, u, b] = mk(); const r = b[m](v, o, l); return [r, Buffer.from(ab).toString('hex')]; });

  // --- 2. detach / resize during a call ---
  const det = (name, f) => t('detach ' + name, f);
  det('writeUInt32LE value', () => { const b = Buffer.alloc(8); return [b.writeUInt32LE({ valueOf() { b.buffer.transfer(); return 1; } }, 0), b.length]; });
  det('writeUIntLE value', () => { const b = Buffer.alloc(8); return b.writeUIntLE({ valueOf() { b.buffer.transfer(); return 1; } }, 0, 3); });
  det('writeDoubleLE value', () => { const b = Buffer.alloc(8); return b.writeDoubleLE({ valueOf() { b.buffer.transfer(); return 1; } }, 0); });
  det('writeInt8 value', () => { const b = Buffer.alloc(8); return b.writeInt8({ valueOf() { b.buffer.transfer(); return 1; } }, 0); });
  det('shrink writeUInt32LE value', () => { const rab = new ArrayBuffer(16, { maxByteLength: 16 }); const b = Buffer.from(rab); return [b.writeUInt32LE({ valueOf() { rab.resize(2); return 1; } }, 0), b.length]; });
  det('toString start', () => { const b = Buffer.from('hello world'); return b.toString('utf8', { valueOf() { b.buffer.transfer(); return 0; } }, 4); });
  det('toString end', () => { const b = Buffer.from('hello world'); return b.toString('utf8', 2, { valueOf() { b.buffer.transfer(); return 10; } }); });
  det('toString encoding', () => { const b = Buffer.from('hello world'); return b.toString({ toString() { b.buffer.transfer(); return 'hex'; } }, 2, 10); });
  det('shrink toString end', () => { const rab = new ArrayBuffer(16, { maxByteLength: 16 }); const b = Buffer.from(rab); b.fill(0x41); return b.toString('utf8', 2, { valueOf() { rab.resize(4); return 10; } }); });
  det('write encoding', () => { const b = Buffer.alloc(8); return b.write('hello', 0, 5, { toString() { b.buffer.transfer(); return 'utf8'; } }); });
  det('shrink write encoding', () => { const rab = new ArrayBuffer(16, { maxByteLength: 16 }); const b = Buffer.from(rab); return b.write('hello', 4, 5, { toString() { rab.resize(6); return 'utf8'; } }); });
  if (!opts.skipPanics) {
    det('fill value', () => { const b = Buffer.alloc(16); b.fill({ valueOf() { b.buffer.transfer(); return 1; } }, 8); return b.length; });
    det('shrink fill value', () => { const rab = new ArrayBuffer(16, { maxByteLength: 16 }); const b = Buffer.from(rab); b.fill({ valueOf() { rab.resize(4); return 1; } }, 8, 16); return b.length; });
  }
  det('fill value start0', () => { const b = Buffer.alloc(16); b.fill({ valueOf() { b.buffer.transfer(); return 1; } }); return b.length; });
  det('copy targetStart', () => { const s = Buffer.from('abcdefgh'), d = Buffer.alloc(8); return [s.copy(d, { valueOf() { s.buffer.transfer(); return 0; } }), d.toString('hex')]; });
  det('copy sourceEnd detach target', () => { const s = Buffer.from('abcdefgh'), d = Buffer.alloc(8); return [s.copy(d, 0, 0, { valueOf() { d.buffer.transfer(); return 8; } }), d.length]; });
  det('copy sourceEnd shrink source', () => { const rab = new ArrayBuffer(8, { maxByteLength: 8 }); const s = Buffer.from(rab); s.fill(0x61); const d = Buffer.alloc(8); return [s.copy(d, 0, 4, { valueOf() { rab.resize(2); return 8; } }), d.toString('hex')]; });
  det('slice start', () => { const b = Buffer.alloc(8); return b.slice({ valueOf() { b.buffer.transfer(); return 0; } }); });
  det('shrink slice', () => { const rab = new ArrayBuffer(8, { maxByteLength: 8 }); const b = Buffer.from(rab); return b.slice(2, { valueOf() { rab.resize(4); return 8; } }); });
  det('concat length getter', () => { const b = Buffer.from('abcdefgh'); return Buffer.concat([b, { get length() { b.buffer.transfer(); return 0; } }]); });
  det('concat shrink getter', () => { const rab = new ArrayBuffer(8, { maxByteLength: 8 }); const b = Buffer.from(rab); b.fill(0x61); const c = Buffer.from('xy'); Object.defineProperty(c, 'length', { get() { rab.resize(2); return 2; } }); return Buffer.concat([b, c]); });
  det('from ab offset', () => { const rab = new ArrayBuffer(16, { maxByteLength: 16 }); return Buffer.from(rab, { valueOf() { rab.resize(2); return 0; } }, 8); });
  det('from ab length', () => { const rab = new ArrayBuffer(16, { maxByteLength: 16 }); return Buffer.from(rab, 0, { valueOf() { rab.resize(2); return 8; } }); });
  det('from ab transfer length', () => { const ab = new ArrayBuffer(16); return Buffer.from(ab, 0, { valueOf() { ab.transfer(); return 8; } }); });
  det('alloc fill valueOf', () => Buffer.alloc(4, { valueOf: () => 65 }));
  det('from arraylike getter', () => { const b = Buffer.alloc(4); return Buffer.from({ length: 3, get 0() { b.buffer.transfer(); return 1; }, 1: 2, 2: 3 }); });
  det('byteLength encoding', () => Buffer.byteLength('abc', { toString() { return 'hex'; } }));
  det('equals detached', () => { const a = Buffer.alloc(0), b = Buffer.alloc(4); b.buffer.transfer(); return [a.equals(b), b.equals(a), b.length]; });
  det('isUtf8 detached', () => { const b = Buffer.alloc(4); b.buffer.transfer(); return nb.isUtf8(b); });
  det('isAscii detached ab', () => { const ab = new ArrayBuffer(4); ab.transfer(); return nb.isAscii(ab); });
  det('swap16 detached', () => { const b = Buffer.alloc(4); b.buffer.transfer(); return b.swap16().length; });
  det('toJSON detached', () => { const b = Buffer.alloc(4); b.buffer.transfer(); return JSON.stringify(b); });
  det('toString detached', () => { const b = Buffer.alloc(4); b.buffer.transfer(); return b.toString('hex', 0, 4); });
  det('readUInt8 detached', () => { const b = Buffer.alloc(4); b.buffer.transfer(); return b.readUInt8(0); });
  det('write detached', () => { const b = Buffer.alloc(4); b.buffer.transfer(); return b.write('ab'); });
  det('compare detached', () => { const b = Buffer.alloc(4); b.buffer.transfer(); return b.compare(Buffer.from('a'), 0, 1, 0, 0); });
  det('oob view length', () => { const rab = new ArrayBuffer(16, { maxByteLength: 16 }); const b = Buffer.from(rab); rab.resize(4); return [b.length, b.byteLength, b.toString('hex'), (() => { try { return b.readUInt8(0); } catch (e) { return e.code; } })()]; });
  det('grow view length', () => { const rab = new ArrayBuffer(4, { maxByteLength: 16 }); const b = Buffer.from(rab); rab.resize(16); return [b.length, b.toString('hex').length]; });
  det('length tracking this', () => { const rab = new ArrayBuffer(4, { maxByteLength: 16 }); const u = new Uint8Array(rab); rab.resize(8); return [Buffer.prototype.toString.call(u, 'hex').length, Buffer.prototype.readUInt8.call(u, 7)]; });

  // --- 3. sharing ---
  t('share from ab view', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); b.fill(7); return [...new Uint8Array(ab)]; });
  t('share from u8 copies', () => { const u = new Uint8Array([1, 2, 3]); const b = Buffer.from(u); b[0] = 9; return [u[0], b[0]]; });
  t('share from u8 sub copies', () => { const ab = new ArrayBuffer(8); const u = new Uint8Array(ab, 2, 3); const b = Buffer.from(u); b.fill(9); return [...new Uint8Array(ab)]; });
  t('share from buffer copies', () => { const a = Buffer.from('abc'); const b = Buffer.from(a); b[0] = 0x7a; return [a.toString(), b.toString(), a.buffer === b.buffer]; });
  t('share from u8.buffer', () => { const u = new Uint8Array([1, 2, 3, 4]); const b = Buffer.from(u.buffer, u.byteOffset, u.length); b[1] = 9; return [...u]; });
  t('share from u16 copies', () => { const u = new Uint16Array([1, 2]); const b = Buffer.from(u); b[0] = 9; return [u[0], b.length]; });
  t('share from f64 copies', () => { const u = new Float64Array([1.5, -2]); const b = Buffer.from(u); return [b.length, b.toString('hex')]; });
  t('share from clamped', () => Buffer.from(new Uint8ClampedArray([1, 300])));
  t('share from dataview', () => Buffer.from(new DataView(new ArrayBuffer(4))));
  t('share from sab', () => { const sab = new SharedArrayBuffer(4); const b = Buffer.from(sab, 1, 2); b.fill(5); return [...new Uint8Array(sab)]; });
  t('share slice', () => { const a = Buffer.from('abcdef'); const s = a.slice(2, 4); s[0] = 0x58; return [a.toString(), s.byteOffset, s.buffer === a.buffer]; });
  t('share subarray', () => { const a = Buffer.from('abcdef'); const s = a.subarray(2, 4); s[0] = 0x58; return [a.toString(), s.byteOffset, s.buffer === a.buffer]; });
  t('share slice of slice', () => { const a = Buffer.from('abcdefgh'); const s = a.slice(2).slice(1, 3); s.fill(0x58); return [a.toString(), s.byteOffset, s.length]; });
  t('share slice oob', () => { const a = Buffer.from('abcdef'); return [a.slice(4, 100).length, a.slice(-100, 2).length, a.slice(100).length, a.slice(2, 1).length, a.slice(NaN, NaN).length, a.slice(1.9, 3.9).toString(), a.slice('1', '3').toString(), a.slice(null, 2).toString(), a.slice(-Infinity, Infinity).toString(), a.slice(-1.5).toString(), a.slice(2 ** 32).length, a.slice(-(2 ** 32)).length]; });
  t('share concat copies', () => { const a = Buffer.from('ab'); const c = Buffer.concat([a]); c[0] = 0x58; return [a.toString(), c.toString()]; });
  t('share concat single', () => { const a = Buffer.from('ab'); const c = Buffer.concat([a], 2); return c.buffer === a.buffer; });
  t('share map', () => { const a = Buffer.from('ab'); const m = a.map(x => x + 1); m[0] = 0; return [a.toString(), Buffer.isBuffer(m), m.buffer === a.buffer]; });
  t('share filter', () => { const a = Buffer.from('ab'); const m = a.filter(() => true); return [Buffer.isBuffer(m), m.buffer === a.buffer, m.toString()]; });
  t('share u8 slice', () => { const a = Buffer.from('abcd'); const s = Uint8Array.prototype.slice.call(a, 1, 3); s[0] = 0x58; return [a.toString(), Buffer.isBuffer(s), s.toString()]; });
  t('share swap16 view', () => { const ab = new ArrayBuffer(8); const u = new Uint8Array(ab); for (let i = 0; i < 8; i++) u[i] = i; Buffer.from(ab, 2, 4).swap16(); return [...u]; });
  t('share swap32 view', () => { const ab = new ArrayBuffer(8); const u = new Uint8Array(ab); for (let i = 0; i < 8; i++) u[i] = i; Buffer.from(ab, 4, 4).swap32(); return [...u]; });
  t('share swap64 view', () => { const ab = new ArrayBuffer(16); const u = new Uint8Array(ab); for (let i = 0; i < 16; i++) u[i] = i; Buffer.from(ab, 4, 8).swap64(); return [...u]; });
  t('share fill view', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); b.fill('xyz'); return [...new Uint8Array(ab)]; });
  t('share fill view range', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); b.fill(1, 2, 4); return [...new Uint8Array(ab)]; });
  t('share fill view oob', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); return b.fill(1, 2, 5); });
  t('share fill view start oob', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); return b.fill(1, 5); });
  t('share fill view start oob end', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); return b.fill(1, 5, 4); });
  t('share write view', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); return [b.write('abcdefgh', 1), [...new Uint8Array(ab)]]; });
  t('share write view hex', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); return [b.write('aabbccddeeff', 3, 'hex'), [...new Uint8Array(ab)]]; });
  t('share write view ucs2', () => { const ab = new ArrayBuffer(8); const b = Buffer.from(ab, 2, 4); return [b.write('abc', 1, 'ucs2'), [...new Uint8Array(ab)]]; });
  t('share copy view', () => { const ab = new ArrayBuffer(8); const d = Buffer.from(ab, 2, 4); const s = Buffer.from('abcdefgh'); return [s.copy(d, 1, 0, 100), [...new Uint8Array(ab)]]; });
  t('share copy self overlap', () => { const b = Buffer.from('abcdef'); return [b.copy(b, 2, 0, 4), b.toString()]; });
  t('share copy self overlap back', () => { const b = Buffer.from('abcdef'); return [b.copy(b, 0, 2, 6), b.toString()]; });
  t('share fill self', () => { const b = Buffer.from('abcdef'); return b.fill(b.subarray(1, 3)); });
  t('share fill self whole', () => { const b = Buffer.from('abcdef'); return b.fill(b); });
  t('share readUInt32LE view', () => { const ab = new ArrayBuffer(8); new Uint8Array(ab).fill(0xff); const b = Buffer.from(ab, 2, 4); return [b.readUInt32LE(0), (() => { try { return b.readUInt32LE(1); } catch (e) { return e.code; } })(), (() => { try { return b.readUInt8(4); } catch (e) { return e.code; } })()]; });
  t('share indexOf view', () => { const ab = new ArrayBuffer(8); const u = new Uint8Array(ab); u.set([1, 2, 3, 4, 5, 6, 7, 8]); const b = Buffer.from(ab, 2, 4); return [b.indexOf(1), b.indexOf(3), b.indexOf(7), b.lastIndexOf(6), b.indexOf(Buffer.from([6, 7])), b.indexOf(Buffer.from([5, 6]))]; });
  t('share compare view', () => { const ab = new ArrayBuffer(8); const u = new Uint8Array(ab); u.set([1, 2, 3, 4, 5, 6, 7, 8]); const b = Buffer.from(ab, 2, 4); return [b.compare(Buffer.from([3, 4, 5, 6])), b.compare(Buffer.from([3, 4, 5, 6, 7])), b.equals(Buffer.from([3, 4, 5, 6]))]; });
  t('share toString view', () => { const ab = new ArrayBuffer(8); const u = new Uint8Array(ab); u.set([0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68]); const b = Buffer.from(ab, 2, 4); return [b.toString(), b.toString('utf8', 1, 100), b.toString('utf8', -5, 2), b.toString('hex', 3, 4)]; });
  for (const args of [[1.5, 2], [-0.5], ['2'], [null], [2, null], [2, NaN], [2, -1], [2, 1.5], [2, '3'], [1e300], [2 ** 32], [Infinity], [-Infinity], [NaN, 3], [8], [9], [8, 0], [8, 1], [4, 4], [4, 5], [2, 3]]) t(`from ab ${keys(args)}`, () => Buffer.from(new ArrayBuffer(8), ...args));

  // --- 4. encodings ---
  const b64 = ['YQ=', 'YQ===', '=YQ==', 'Y Q = =', '\nYQ==\n', 'YQ-_', 'YQ+/', '-_-_', '+/+/', 'YQ', 'Y', 'YQ=x', 'Z m 9 v', 'Zm9v\u0000', 'ŁŁŁŁ', 'Zm9vYg', 'Zm9vYg=', 'Zm9vYmE', '////', '====', 'AA==AA', 'AAA=A', 'ab', 'abc', 'abcd', 'abcde', 'abcdef', 'abcdefg', 'abcdefgh', 'ab=cd', 'YR', 'YQ==\n', 'Zm9v\tYmFy', 'Zm9v*YmFy', 'Zm9véYmFy', 'Zm9v\u{1F600}YmFy', 'Zm9vYmFy===', 'Zm9vYmF=', 'Zm9vYm==', 'Zm9vY===', 'İİİİ', 'Zm9vYg==Zm9v', '  ', '=', '==', 'A', 'AB', 'ABC', 'ABCD', 'A=', 'A==', 'A===', 'AB=', 'ABC=', 'Zm9vYmFyYmF6cXV4', 'Zm9vYmFyYmF6cXV4\n\n', 'Zm9v\nYmFy\nYmF6\ncXV4\n', 'Zm9vYmFyYmF6cXVÿ'];
  for (const enc of ['base64', 'base64url']) for (const s of b64) { t(`b64 from ${enc} ${JSON.stringify(s)}`, () => Buffer.from(s, enc)); t(`b64 len ${enc} ${JSON.stringify(s)}`, () => Buffer.byteLength(s, enc)); }
  const hx = ['a', 'abc', 'abcx', 'xabc', 'ab x', 'ABcd', 'ŁŁ', 'abİİ', 'ab-c', '0g', 'g0', 'aB', ' ab', 'ab ', '00ff', 'FFFF', 'abcdef0', 'İa', 'aİ'];
  for (const s of hx) { t(`hex from ${JSON.stringify(s)}`, () => Buffer.from(s, 'hex')); t(`hex len ${JSON.stringify(s)}`, () => Buffer.byteLength(s, 'hex')); }
  const u16 = ['a\uD800', '\uD800', '\uDC00\uD800', '😀', 'ab', '', '\u0000', 'Āā', '￿', 'Ł', 'a\uDFFFb'];
  for (const s of u16) for (const enc of ['utf16le', 'ucs2', 'latin1', 'ascii', 'utf8']) { t(`enc from ${enc} ${JSON.stringify(s)}`, () => Buffer.from(s, enc)); t(`enc len ${enc} ${JSON.stringify(s)}`, () => Buffer.byteLength(s, enc)); }
  const bytesets = [[0x41], [0x41, 0], [0x41, 0, 0x42], [0, 0xd8], [0, 0xd8, 0, 0xdc], [0, 0xdc, 0, 0xd8], [0x3d, 0xd8, 0x00, 0xde], [0xff, 0xff], [0xc0, 0x80], [0xe0, 0x80, 0x80], [0xf0, 0x80, 0x80, 0x80], [0xf8, 0x88, 0x80, 0x80, 0x80], [0xfc, 0x84, 0x80, 0x80, 0x80, 0x80], [0xed, 0xa0, 0x80, 0xed, 0xb0, 0x80], [0xf4, 0x90, 0x80, 0x80], [0xe2, 0x82], [0xe2, 0x82, 0xac], [0xe2, 0x41], [0xc3], [0xff, 0xfe], [0x80, 0x80], [0xc2, 0xc2], [0xe1, 0x80, 0xc2], [0xf0, 0x9f, 0x98, 0x80, 0x80], [0xef, 0xbb, 0xbf, 0x41], [0xf0, 0x90], [0xf0, 0x90, 0x80], [0xe0, 0xa0], [0xed, 0x9f, 0xbf], [0xee, 0x80, 0x80], [0x7f, 0x80, 0xfe, 0xff], [0xc1, 0xbf], [0xe0, 0x9f, 0xbf], [0xf0, 0x8f, 0xbf, 0xbf], [0xf4, 0x8f, 0xbf, 0xbf], [0xf5, 0x80, 0x80, 0x80], [0x61, 0xe9, 0x62], [0xc3, 0xa9, 0x41, 0x00]];
  for (const b of bytesets) { const buf = Buffer.from(b); const k = b.map(x => x.toString(16)).join(','); for (const enc of ['utf8', 'utf16le', 'latin1', 'ascii', 'base64', 'base64url', 'hex']) t(`dec ${enc} ${k}`, () => buf.toString(enc)); t(`isUtf8 ${k}`, () => nb.isUtf8(buf)); t(`isAscii ${k}`, () => nb.isAscii(buf)); }
  t('isUtf8 ab', () => nb.isUtf8(new ArrayBuffer(2)));
  t('isUtf8 dataview', () => nb.isUtf8(new DataView(new Uint8Array([0xff]).buffer)));
  t('isUtf8 string', () => nb.isUtf8('abc'));
  t('isUtf8 u16', () => nb.isUtf8(new Uint16Array([0xffff])));
  t('isUtf8 none', () => nb.isUtf8());
  t('isAscii string', () => nb.isAscii('abc'));
  t('isAscii view', () => { const ab = new ArrayBuffer(4); new Uint8Array(ab).fill(0xff); return nb.isAscii(new Uint8Array(ab, 1, 0)); });
  for (const s of ['YQ==', 'YQ', 'Y', 'YQ=', ' Y Q ', 'YQ-_', 'YQ===', 'YQ==YQ==', '', 'Zm9v\nYmFy', 'Zm9vYmF', 'Zm9vYmE=', 'Zm9vYm=', 'Zm9vYmFy=', 'Zm9vYg==', 'Zm9vY===', 'ŁŁŁŁ', 'Zm9v*YmFy', 'YR', 'Zm9v YmFy', 'Zm9v\fYmFy']) t(`atob ${JSON.stringify(s)}`, () => nb.atob(s));
  for (const s of ['', 'a', 'ab', 'abc', 'ÿ', 'Ā', 'a\uD800', '\u0000']) t(`btoa ${JSON.stringify(s)}`, () => nb.btoa(s));
  t('atob no arg', () => nb.atob()); t('btoa no arg', () => nb.btoa()); t('atob number', () => nb.atob(1)); t('btoa number', () => nb.btoa(123)); t('atob object', () => nb.atob({ toString: () => 'YQ==' }));
  for (const e of ['HEX', 'Hex', 'Base64', 'BASE64URL', 'base64URL', 'UTF-16le', 'utf-16LE', 'UCS2', 'ucs-2', 'UCS-2', 'BINARY', 'Latin1', 'LATIN1', 'ASCII', 'utf-16', 'utf16', 'utf-8 ', ' utf8', 'utf8\u0000', 'raw', 'raws', 'buffer', undefined, null, 5, {}, Symbol.iterator]) t(`isEncoding ${key(e)}`, () => Buffer.isEncoding(e));
  for (const e of ['HEX', 'Base64', 'BASE64URL', 'UCS2', 'ucs-2', 'BINARY', 'LATIN1', 'ASCII', 'UTF-16LE', 'utf-16', 'buffer', 'raw', '', null, 5, true]) { t(`from enc ${String(e)}`, () => Buffer.from('6162', e)); t(`toString enc ${String(e)}`, () => Buffer.from('ab').toString(e)); t(`byteLength enc ${String(e)}`, () => Buffer.byteLength('6162', e)); t(`write enc ${String(e)}`, () => { const b = Buffer.alloc(4); return [b.write('6162', e), b.toString('hex')]; }); t(`fill enc ${String(e)}`, () => Buffer.alloc(4).fill('6162', e)); }
  const wr = [[3, '€'], [2, '€'], [5, '😀😀'], [3, 'a😀'], [4, 'a😀'], [5, 'a😀'], [3, 'ab', 'ucs2'], [3, 'ab', 1, 'ucs2'], [4, 'abc', 1, 'ucs2'], [4, 'abc', 'hex'], [4, 'zz', 'hex'], [4, 'abzz', 'hex'], [4, 'YQ==YQ==', 'base64'], [4, 'Ā', 'latin1'], [4, 'Ā', 'ascii'], [2, 'a', 2], [2, 'a', 3], [2, '', 0], [2, '', 2], [4, 'abc', 1, 1], [4, 'abc', 1, 0], [4, 'abc', 1, 10], [4, 'abc', 1, -1], [4, 'abc', 1, 1.5], [4, 'abc', 1, '2'], [4, 'abc', 4, 0], [4, 'abc', 0, 4, 'latin1'], [4, 'a\uD800b', 0], [4, '\uDC00', 0], [1, '😀'], [4, '€', 2], [4, '€', 1], [4, 'ab', 'ucs2'], [3, 'a', 2, 'ucs2'], [3, 'ab', 2, 1, 'ucs2'], [8, 'Zm9vYmFy', 'base64url'], [3, 'Zm9vYmFy', 'base64'], [4, 'ab', NaN], [4, 'ab', Infinity], [4, 'ab', -0], [4, 'ab', null], [4, 'ab', 0, null], [4, 'ab', 0, undefined, null], [4, 'ab', 0, 2, 5], [4, 'ab', 0, 2, ''], [4, 'ab', 5, 0], [4, 'ab', 4], [4, 'ab', 2, 2 ** 32], [4, 'ab', '1', 2], [4, 'ab', 'ucs2', 2]];
  for (const [n, ...args] of wr) t(`write ${n} ${keys(args)}`, () => { const b = Buffer.alloc(n); const r = b.write(...args); return [r, b.toString('hex')]; });
  t('write object offset', () => Buffer.alloc(4).write('a', { valueOf: () => 1 }));
  t('write no args', () => Buffer.alloc(4).write());
  t('write object string', () => Buffer.alloc(4).write({ toString: () => 'ab' }));
  const h = Buffer.from('hello world');
  const ranges = [[0, NaN], [0, -1], [0, 2 ** 32 + 2], [-Infinity, Infinity], [1.9, 3.1], [0, '2'], [{ valueOf: () => 2 }, 4], [null, null], [true, 3], [0, true], [2 ** 31, 3], [3, 2 ** 31], [-1, -1], [NaN, NaN], [undefined, undefined], [0, 0], [11, 11], [10, 11], [0, Infinity], [-0, 5], [2, 2.5], ['a', 'b'], [[], [3]], [0, {}], [2, -Infinity], [-5, 3], [3, -5], [0.9, 1.1], [10.5, 11]];
  for (const [s, e] of ranges) t(`toString range ${keys([s, e])}`, () => h.toString('utf8', s, e));
  for (const enc of ['hex', 'base64', 'utf16le', 'latin1', 'ascii', 'base64url']) t(`toString range ${enc}`, () => [h.toString(enc, 1, 4), h.toString(enc, 1), h.toString(enc, 1, 100), h.toString(enc, 1, 0), h.toString(enc, 3, 4)]);
  t('toString utf16le odd', () => Buffer.from('abc').toString('utf16le', 1));
  t('toString utf16le odd2', () => Buffer.from('abcde').toString('utf16le', 0, 3));
  t('toString undefined x3', () => h.toString(undefined, undefined, undefined));
  t('toString many', () => h.toString('hex', 1, 3, 'extra'));
  t('toLocaleString range', () => h.toLocaleString('hex', 1, 3));
  for (const [s, e] of [['YQ==', 'base64'], ['YQ=', 'base64'], ['YQ', 'base64'], ['Y', 'base64'], ['==', 'base64'], ['=', 'base64'], ['YQ==\n', 'base64'], [' YQ== ', 'base64'], ['YQ==', 'base64url'], ['abc', 'hex'], ['a', 'hex'], ['a', 'utf16le'], ['Ā', 'latin1'], ['\uD800', 'utf8'], ['𐀀', 'utf8'], ['abc', 'nope'], ['abc', 5], ['abc', null], ['abc', ''], ['abc', 'UCS2'], ['', 'base64'], ['abc', true], ['abc', {}], ['a\uD800b', 'utf8'], ['\uDC00\uD800', 'utf8']]) t(`byteLength ${JSON.stringify(s)} ${key(e)}`, () => Buffer.byteLength(s, e));
  t('byteLength u16', () => Buffer.byteLength(new Uint16Array(3)));
  t('byteLength dataview', () => Buffer.byteLength(new DataView(new ArrayBuffer(5), 1)));
  t('byteLength sab', () => Buffer.byteLength(new SharedArrayBuffer(6)));
  t('byteLength view', () => Buffer.byteLength(new Uint8Array(new ArrayBuffer(8), 2, 3)));
  t('byteLength null', () => Buffer.byteLength(null)); t('byteLength undefined', () => Buffer.byteLength()); t('byteLength obj', () => Buffer.byteLength({ length: 3 })); t('byteLength array', () => Buffer.byteLength([1, 2]));

  // --- 5. ranges of copy / compare / indexOf / fill / alloc / concat ---
  const cp = [[4, 0, 0, 10], [4, 2, 2, 4], [4, 3, 0, 4], [4, 4, 0, 4], [4, 5], [4, -1], [4, 0, -1], [4, 0, 5], [4, 0, 4], [4, 0, 0, -1], [4, 0, 0, 0], [4, 0, 2, 1], [4, 1.9, 1.9, 3.9], [4, NaN, NaN, NaN], [4, '1', '1', '3'], [4, null, null, null], [4, 0, 0, Infinity], [4, Infinity], [4, 0, Infinity], [4, 0, 0, 2 ** 53], [4, 0, 0, 2 ** 53 + 2], [4, 2 ** 53, 0], [4, -(2 ** 53) - 2], [4, {}, {}, {}], [4, [], [], [1]], [0, 0, 0, 4], [4, -0.5], [4, 0, -0.5], [4, 0, 0, -0.5]];
  for (const [n, ...args] of cp) t(`copy ${n} ${keys(args)}`, () => { const d = Buffer.alloc(n); const r = Buffer.from('wxyz').copy(d, ...args); return [r, d.toString('hex')]; });
  t('copy to u16', () => { const d = new Uint16Array(2); return [Buffer.from('ab').copy(d), [...d]]; });
  t('copy from u16', () => { const d = Buffer.alloc(4); return [Buffer.prototype.copy.call(new Uint16Array([1, 2]), d), d.toString('hex')]; });
  t('copy to dataview', () => Buffer.from('ab').copy(new DataView(new ArrayBuffer(2))));
  t('copy to arraybuffer', () => Buffer.from('ab').copy(new ArrayBuffer(2)));
  t('copy to u8', () => { const d = new Uint8Array(4); return [Buffer.from('ab').copy(d, 1), [...d]]; });
  t('copy no target', () => Buffer.from('ab').copy());
  const cmp = [[0, 2, 0, 2], [0, 10], [10], [2, 1], [0, 2, 3, 2], [0, 2, 7], [0, 2, 0, 7], [-1], [0, -1], [1.5], [0, 1.5], ['1'], [0, '1'], [NaN], [undefined, undefined, undefined, undefined], [0, 0, 0, 0], [6, 6, 6, 6], [0, 6, 6, 6], [6, 6, 0, 6], [7, 7, 0, 6], [2 ** 32], [0, 1, 2 ** 32], [undefined], [null], [0, null]];
  for (const args of cmp) t(`compare ${keys(args)}`, () => Buffer.from('abcdef').compare(Buffer.from('abcdef'), ...args));
  t('compare u16 target', () => Buffer.from('ab').compare(new Uint16Array(1)));
  t('compare static u16', () => Buffer.compare(new Uint16Array(1), Buffer.from('ab')));
  t('compare static u8', () => Buffer.compare(new Uint8Array([97]), Buffer.from('a')));
  t('compare static clamped', () => Buffer.compare(new Uint8ClampedArray([97]), Buffer.from('a')));
  t('equals clamped', () => Buffer.from('a').equals(new Uint8ClampedArray([97])));
  const b6 = Buffer.from('abcabc');
  const io = [['c', -100], ['', 100], ['', -100], ['c', 2 ** 40], ['c', -(2 ** 40)], ['c', 2.7], ['c', -0.5], ['c', null], ['c', true], ['c', {}], ['c', []], ['c', [3]], ['c', '3'], ['c', 'latin1'], ['c', 'nope'], ['c', 0, 'nope'], [Buffer.alloc(0)], [Buffer.alloc(0), 3], [Buffer.alloc(0), 100], [Buffer.alloc(0), -100], [0x100], [-1], [NaN], [Infinity], [1e10], [2.9], [-0.5], [0x63, 2 ** 31 - 1], [0x63, -(2 ** 31)], ['abcabc'], ['abcabcd'], ['abcabc', 1], ['bcab', 1], ['bcab', 2], [Buffer.from('ca')], [Buffer.from('ca'), 3], [Buffer.from('ca'), -4], [new Uint8Array([0x62]), 2], [new Uint16Array([0x62])], ['c', 0, 'ucs2'], ['c\u0000', 0, 'ucs2'], ['63', 0, 'hex'], ['6', 0, 'hex'], ['zz', 0, 'hex'], ['zz', 3, 'hex'], ['Yw==', 0, 'base64'], ['Yw', 0, 'base64url'], ['c', undefined, 'latin1'], ['c', undefined, undefined], ['é'], ['é', 0, 'latin1'], [{}], [null], [undefined], [Symbol.iterator], [true], [new Uint8ClampedArray([0x62])]];
  for (const args of io) { const k = keys(args); t(`indexOf ${k}`, () => b6.indexOf(...args)); t(`lastIndexOf ${k}`, () => b6.lastIndexOf(...args)); t(`includes ${k}`, () => b6.includes(...args)); }
  t('indexOf empty haystack', () => [Buffer.alloc(0).indexOf('a'), Buffer.alloc(0).indexOf(''), Buffer.alloc(0).lastIndexOf(''), Buffer.alloc(0).indexOf(0), Buffer.alloc(0).lastIndexOf(0), Buffer.alloc(0).indexOf('', 5), Buffer.alloc(0).lastIndexOf('', -5)]);
  t('lastIndexOf variants', () => ['ab', 'abc', 'c', 'abcabc', 'bca', 'cab'].flatMap(s => [-7, -6, -1, 0, 2, 3, 5, 6, 7, 100].map(o => Buffer.from('abcabc').lastIndexOf(s, o))));
  t('indexOf variants', () => ['ab', 'abc', 'c', 'abcabc', 'bca', 'cab'].flatMap(s => [-7, -6, -1, 0, 2, 3, 5, 6, 7, 100].map(o => Buffer.from('abcabc').indexOf(s, o))));
  t('indexOf byte variants', () => [0x61, 0x63].flatMap(s => [-7, -6, -1, 0, 2, 3, 5, 6, 7, 100].flatMap(o => [Buffer.from('abcabc').indexOf(s, o), Buffer.from('abcabc').lastIndexOf(s, o)])));
  const fl = [[6, 0x41, 7], [6, 0x41, 6], [6, 0x41, 6, 6], [6, 0x41, 7, 6], [6, 0x41, 0, 7], [6, 0x41, -1], [6, 0x41, 0, -1], [6, 0x41, 1.5], [6, 0x41, 0, 1.5], [6, 0x41, '1'], [6, 0x41, 0, '1'], [6, 0x41, NaN], [6, 0x41, 0, NaN], [6, 0x41, Infinity], [6, 0x41, 0, Infinity], [6, 'ab', 'nope'], [6, 'ab', 0, 'nope'], [6, 'ab', 0, 6, 'nope'], [6, 'ab', null], [6, 'ab', 0, null], [6, 'ab', 0, 6, null], [6, 'ab', 0, 6, 5], [6, 'ab', 0, 6, undefined], [6, 'ab', 0, 6, 'ucs2'], [6, 'ab', 1, 6, 'ucs2'], [6, 'abc', 0, 6, 'ucs2'], [6, '€', 0, 6], [6, '€€€', 0, 4], [6, 'Ā', 0, 6, 'latin1'], [6, 'a', 0, 6, 'latin1'], [6, '€', 0, 6, 'latin1'], [6, 'zz', 0, 6, 'hex'], [6, 'z', 0, 6, 'hex'], [6, 'abc', 0, 6, 'hex'], [6, '=', 0, 6, 'base64'], [6, 'YQ', 0, 6, 'base64'], [6, Buffer.alloc(0)], [6, new Uint16Array([0x4142])], [6, new Uint16Array([])], [6, new Float64Array([1])], [6, new DataView(new ArrayBuffer(1))], [6, true], [6, null], [6, undefined], [6, {}], [6, { valueOf: () => 66 }], [6, []], [6, [1]], [6, -1], [6, 1.9], [6, 256], [6, 2 ** 32 + 1], [6, NaN], [6, Infinity], [6, -Infinity], [6, '', 2], [6, '', 2, 4, 'hex'], [0, 'a'], [0, 'a', 0], [0, 'a', 1], [0, 1, 0, 0], [6, 'a', 7, 6], [6, 'a', 7], [6, 'a', 2, 1], [6], [6, 'abc', 4], [6, 'é', 5], [6, 'ab', 'ucs2'], [6, 'ab', 1, 'ucs2'], [6, 'ab', 1, 'ucs2', 'x']];
  for (const [n, ...args] of fl) t(`fill ${n} ${keys(args)}`, () => Buffer.alloc(n).fill(...args));
  const al = [[4, ''], [4, 'a', 'nope'], [4, 'ab', 7], [4, 'ab', null], [4, 'ab', undefined], [4, 'ab', ''], [0, 'x'], [3, 0], [3, -1], [3, 1.5], [3, NaN], [3, Infinity], [3, '€'], [4, '€'], [5, Buffer.alloc(0)], [5, new Uint16Array([1])], [5, new Float64Array([1])], [5, new DataView(new ArrayBuffer(2))], [5, true], [5, false], [5, null], [3, { valueOf: () => 65 }], [3, {}], [3, []], [3, [66]], [3, 'zz', 'hex'], [3, 'z', 'hex'], [3, '=', 'base64'], [3, 'ab', 'ucs2'], [3, 'a', 'ucs2'], [3, 'Ā', 'latin1'], [3, 'ab', 'UCS2'], [3, 'ab', 'Hex'], [NaN], [Infinity], [-0], [1.5], ['3'], [null], [undefined], [-1], [2 ** 53], [true], [[]], [{}], [3, Symbol.iterator], [3, 5n], [3, 'ab', 0], [3, 'ab', 'utf8', 'x']];
  for (const args of al) t(`alloc ${keys(args)}`, () => Buffer.alloc(...args));
  for (const a of [-1, NaN, 1.5, '3', 2 ** 53, undefined, null, 0, -0, Infinity, [], {}]) { t(`allocUnsafe ${key(a)}`, () => Buffer.allocUnsafe(a).length); t(`allocUnsafeSlow ${key(a)}`, () => Buffer.allocUnsafeSlow(a).length); t(`new Buffer ${key(a)}`, () => new Buffer(a).length); }
  const cc = [[[Buffer.from('ab'), Buffer.from('cd')], 3], [[Buffer.from('ab'), Buffer.from('cd')], 0], [[Buffer.from('ab')], -1], [[Buffer.from('ab')], 1.5], [[Buffer.from('ab')], '2'], [[Buffer.from('ab')], NaN], [[Buffer.from('ab')], null], [[Buffer.from('ab')], Infinity], [[Buffer.from('ab'), 'cd']], [[Buffer.from('ab'), 'cd'], 2], [[Buffer.from('ab'), 'cd'], 1], [[{ length: 3 }]], [[{ length: 3 }], 0], [[{ length: -3 }]], [[{ length: NaN }]], [[{ length: '3' }]], [[{}]], [[null]], [[undefined]], [[1]], [[new Uint16Array([1, 2])]], [[new Uint8Array([1, 2]), Buffer.from('a')]], [[new Uint8ClampedArray([1, 2])]], [[Buffer.from('ab').subarray(1)], 5], [[]], [[], 3], [[], -1], [[Buffer.alloc(0)]], [[Buffer.alloc(0)], 2], [new Array(3)], [[, Buffer.from('a')]], ['ab'], [{ length: 1, 0: Buffer.from('a') }], [[Buffer.from('ab')], 2 ** 53]];
  for (const args of cc) t(`concat ${args.length} ${JSON.stringify(args[0] && Array.isArray(args[0]) ? Array.from(args[0], key) : key(args[0]))} ${String(args[1])}`, () => Buffer.concat(...args).length);
  t('concat length getter long', () => { const b = Buffer.from('ab'); Object.defineProperty(b, 'length', { value: 10 }); return Buffer.concat([b]); });
  t('concat length getter short', () => { const b = Buffer.from('abcd'); Object.defineProperty(b, 'length', { value: 2 }); return Buffer.concat([b]); });
  t('concat length getter short 2', () => { const b = Buffer.from('abcd'); Object.defineProperty(b, 'length', { value: 2 }); return Buffer.concat([b, Buffer.from('xy')]); });
  if (!opts.skipPanics) {
    t('concat length Infinity', () => Buffer.concat([{ length: Infinity }]));
    t('concat length 1e300', () => Buffer.concat([{ length: 1e300 }]));
    t('concat length overflow', () => Buffer.concat([{ length: 2 ** 62 }, { length: 2 ** 62 }]));
  }

  // --- 6. receivers, API surface ---
  t('swap16 empty', () => Buffer.alloc(0).swap16()); t('swap32 6', () => Buffer.alloc(6).swap32()); t('swap64 7', () => Buffer.alloc(7).swap64()); t('swap64 0', () => Buffer.alloc(0).swap64());
  t('swap16 u16 this', () => Buffer.prototype.swap16.call(new Uint16Array([0x0102]))); t('swap16 string this', () => Buffer.prototype.swap16.call('ab'));
  t('toString u16 this', () => Buffer.prototype.toString.call(new Uint16Array([0x6261]), 'utf8'));
  t('readUInt16LE f64 this', () => Buffer.prototype.readUInt16LE.call(new Float64Array([1]), 0));
  t('write f64 this', () => Buffer.prototype.write.call(new Float64Array(1), 'ab'));
  t('fill u16 this', () => Buffer.prototype.fill.call(new Uint16Array(2), 1));
  t('copy to f64 this', () => Buffer.from('abcd').copy(new Float64Array(1)));
  t('toString null this', () => Buffer.prototype.toString.call(null)); t('toString obj this', () => Buffer.prototype.toString.call({})); t('toString proto this', () => Buffer.prototype.toString.call(Object.create(Buffer.prototype)));
  t('readUInt8 proto this', () => Buffer.prototype.readUInt8.call(Object.create(Buffer.prototype), 0));
  t('equals string this', () => Buffer.prototype.equals.call('a', Buffer.from('a')));
  t('toJSON f64', () => Buffer.prototype.toJSON.call(new Float64Array([1])).data.length);
  t('toJSON obj', () => Buffer.prototype.toJSON.call({}));
  t('isBuffer subclass', () => { class X extends Buffer {} return [Buffer.isBuffer(Reflect.construct(Uint8Array, [2], X)), Buffer.isBuffer(Object.create(Buffer.prototype)), Reflect.construct(Uint8Array, [2], X) instanceof X]; });
  t('subclass species', () => { class X extends Buffer {} const x = Reflect.construct(Uint8Array, [4], X); return [x.subarray(1).constructor.name, x.slice(1).constructor.name, x.map(v => v).constructor.name, Buffer.isBuffer(x.slice(1))]; });
  t('api', () => { const o = {}; for (const n of ['readBigUInt64LE', 'readBigUInt64BE', 'readBigInt64LE', 'readBigInt64BE', 'writeBigUInt64LE', 'writeBigUInt64BE', 'writeBigInt64LE', 'writeBigInt64BE', 'readBigUint64LE', 'writeBigUint64LE', 'hexSlice', 'utf8Slice', 'latin1Slice', 'asciiSlice', 'base64Slice', 'base64urlSlice', 'ucs2Slice', 'hexWrite', 'utf8Write', 'latin1Write', 'asciiWrite', 'base64Write', 'base64urlWrite', 'ucs2Write', 'toLocaleString', 'parent', 'offset', 'inspect', 'readUintLE', 'writeUintBE']) o[n] = typeof Buffer.prototype[n]; for (const n of ['copyBytesFrom', 'from', 'alloc', 'allocUnsafe', 'allocUnsafeSlow', 'isBuffer', 'isEncoding', 'byteLength', 'compare', 'concat', 'poolSize', 'of']) o['Buffer.' + n] = typeof Buffer[n]; for (const n of ['Blob', 'File', 'SlowBuffer', 'transcode', 'resolveObjectURL', 'isUtf8', 'isAscii', 'atob', 'btoa', 'kMaxLength', 'kStringMaxLength', 'constants', 'INSPECT_MAX_BYTES', 'Buffer', 'default']) o['nb.' + n] = typeof nb[n]; return o; });
  t('constants', () => [nb.kMaxLength === nb.constants.MAX_LENGTH, nb.kStringMaxLength === nb.constants.MAX_STRING_LENGTH, nb.INSPECT_MAX_BYTES, nb.kStringMaxLength]);
  t('offset parent', () => { const b = Buffer.from(new ArrayBuffer(8), 2, 2); return [b.offset, b.parent instanceof ArrayBuffer, b.parent === b.buffer]; });
  t('from bigint', () => Buffer.from(5n)); t('from symbol', () => Buffer.from(Symbol.iterator)); t('from bool', () => Buffer.from(true)); t('from undefined', () => Buffer.from()); t('from fn', () => Buffer.from(() => 1));
  t('from string object enc', () => Buffer.from(new String('6162'), 'hex'));
  t('from array of strings', () => Buffer.from(['1', 'a', '0x10', '', null, undefined, true, [], {}, NaN, Infinity, -Infinity, 2 ** 40 + 3, -1.5]));
  t('from arraylike huge length', () => Buffer.from({ length: 2 ** 53 }).length);
  t('from arraylike negative length', () => Buffer.from({ length: -5 }).length);
  t('from arraylike string length', () => Buffer.from({ length: '2', 0: 1, 1: 2 }));
  t('from arraylike fractional length', () => Buffer.from({ length: 2.7, 0: 1, 1: 2, 2: 3 }));
  t('from json bad data', () => Buffer.from({ type: 'Buffer', data: 'abc' })); t('from json no data', () => Buffer.from({ type: 'Buffer' })); t('from json obj data', () => Buffer.from({ type: 'Buffer', data: { length: 1, 0: 7 } }));
  t('from valueOf number', () => Buffer.from({ valueOf: () => 3 })); t('from valueOf array', () => Buffer.from({ valueOf: () => [1, 2] })); t('from valueOf ab', () => Buffer.from({ valueOf: () => new ArrayBuffer(2) }, 1)); t('from valueOf self', () => Buffer.from({ length: 1, 0: 9, valueOf() { return this; } })); t('from valueOf null', () => Buffer.from({ length: 1, 0: 9, valueOf: () => null })); t('from valueOf throws', () => Buffer.from({ valueOf() { throw new RangeError('boom'); } }));
  t('from toPrimitive number', () => Buffer.from({ [Symbol.toPrimitive]: () => 5 })); t('from toPrimitive throws', () => Buffer.from({ [Symbol.toPrimitive]() { throw new RangeError('boom'); } }));
  t('of none', () => Buffer.of()); t('of strings', () => Buffer.of('1', 'x', {}));
  t('inspect', () => Buffer.from('abc')[Symbol.for('nodejs.util.inspect.custom')]()); t('inspect 51', () => Buffer.alloc(51, 1)[Symbol.for('nodejs.util.inspect.custom')]()); t('inspect empty', () => Buffer.alloc(0)[Symbol.for('nodejs.util.inspect.custom')]());
  t('inspect subclass', () => { class Foo extends Buffer {} return Reflect.construct(Uint8Array, [2], Foo)[Symbol.for('nodejs.util.inspect.custom')](); });
  t('Buffer name length', () => [Buffer.name, Buffer.length, Buffer.from.length, Buffer.alloc.length, Buffer.prototype.toString.length, Buffer.prototype.write.length, Buffer.prototype.fill.length, Buffer.prototype.copy.length, Buffer.prototype.indexOf.length, Buffer.prototype.compare.length, Buffer.prototype.readUIntLE.length, Buffer.prototype.writeUIntLE.length, Buffer.prototype.readUInt8.length, Buffer.prototype.writeUInt8.length]);
  t('props', () => Object.getOwnPropertyNames(Buffer.prototype).sort().join());
  t('static props', () => Object.getOwnPropertyNames(Buffer).sort().join());
  t('toString tag', () => [Object.prototype.toString.call(Buffer.from('a')), Buffer.from('a')[Symbol.toStringTag]]);
  t('poolSize writable', () => { const o = Buffer.poolSize; Buffer.poolSize = 1; const r = Buffer.poolSize; Buffer.poolSize = o; return r; });
  return out;
}
