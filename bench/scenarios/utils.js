var utils = (function () {
  "use strict";
  var K = [0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
    0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
    0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
    0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
    0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
    0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
    0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
    0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2];
  function utf8(s) {
    s = String(s);
    var out = [];
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      if (c < 0x80) out.push(c);
      else if (c < 0x800) out.push(0xc0 | c >> 6, 0x80 | c & 63);
      else if (c >= 0xd800 && c < 0xdc00 && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00) {
        var cp = 0x10000 + ((c - 0xd800) << 10) + (s.charCodeAt(++i) - 0xdc00);
        out.push(0xf0 | cp >> 18, 0x80 | cp >> 12 & 63, 0x80 | cp >> 6 & 63, 0x80 | cp & 63);
      } else if (c >= 0xd800 && c < 0xe000) out.push(0xef, 0xbf, 0xbd);
      else out.push(0xe0 | c >> 12, 0x80 | c >> 6 & 63, 0x80 | c & 63);
    }
    return out;
  }
  function fromUTF8(b) {
    var s = "";
    for (var i = 0; i < b.length;) {
      var c = b[i], n = c < 0x80 ? 0 : c >= 0xf0 && c < 0xf8 ? 3 : c >= 0xe0 ? 2 : c >= 0xc0 ? 1 : -1;
      if (n < 0 || (n > 0 && i + n >= b.length)) { s += "�"; i++; continue; }
      var cp = n === 0 ? c : c & (0x3f >> n), ok = true;
      for (var j = 1; j <= n; j++) { if ((b[i + j] & 0xc0) !== 0x80) { ok = false; break; } cp = cp << 6 | b[i + j] & 63; }
      if (!ok) { s += "�"; i++; continue; }
      s += String.fromCodePoint(cp); i += n + 1;
    }
    return s;
  }
  function sha256(msg) {
    var l = msg.length, n = ((l + 9 + 63) >>> 6) << 6, b = new Uint8Array(n), w = new Int32Array(64);
    b.set(msg); b[l] = 0x80;
    var bits = l * 8, hi = Math.floor(bits / 0x100000000);
    b[n - 8] = hi >>> 24 & 255; b[n - 7] = hi >>> 16 & 255; b[n - 6] = hi >>> 8 & 255; b[n - 5] = hi & 255;
    b[n - 4] = bits >>> 24 & 255; b[n - 3] = bits >>> 16 & 255; b[n - 2] = bits >>> 8 & 255; b[n - 1] = bits & 255;
    var H = [0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19];
    for (var off = 0; off < n; off += 64) {
      for (var i = 0; i < 16; i++) w[i] = b[off + 4 * i] << 24 | b[off + 4 * i + 1] << 16 | b[off + 4 * i + 2] << 8 | b[off + 4 * i + 3];
      for (i = 16; i < 64; i++) {
        var x = w[i - 15], y = w[i - 2];
        var s0 = (x >>> 7 | x << 25) ^ (x >>> 18 | x << 14) ^ (x >>> 3);
        var s1 = (y >>> 17 | y << 15) ^ (y >>> 19 | y << 13) ^ (y >>> 10);
        w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
      }
      var a = H[0], bb = H[1], c = H[2], d = H[3], e = H[4], f = H[5], g = H[6], h = H[7];
      for (i = 0; i < 64; i++) {
        var S1 = (e >>> 6 | e << 26) ^ (e >>> 11 | e << 21) ^ (e >>> 25 | e << 7);
        var t1 = (h + S1 + ((e & f) ^ (~e & g)) + K[i] + w[i]) | 0;
        var S0 = (a >>> 2 | a << 30) ^ (a >>> 13 | a << 19) ^ (a >>> 22 | a << 10);
        var t2 = (S0 + ((a & bb) ^ (a & c) ^ (bb & c))) | 0;
        h = g; g = f; f = e; e = (d + t1) | 0; d = c; c = bb; bb = a; a = (t1 + t2) | 0;
      }
      H[0] = (H[0] + a) | 0; H[1] = (H[1] + bb) | 0; H[2] = (H[2] + c) | 0; H[3] = (H[3] + d) | 0;
      H[4] = (H[4] + e) | 0; H[5] = (H[5] + f) | 0; H[6] = (H[6] + g) | 0; H[7] = (H[7] + h) | 0;
    }
    var out = [];
    for (i = 0; i < 8; i++) out.push(H[i] >>> 24 & 255, H[i] >>> 16 & 255, H[i] >>> 8 & 255, H[i] & 255);
    return out;
  }
  function hmac(key, msg) {
    if (key.length > 64) key = sha256(key);
    var ip = [], op = [];
    for (var i = 0; i < 64; i++) { var k = key[i] || 0; ip.push(k ^ 0x36); op.push(k ^ 0x5c); }
    return sha256(op.concat(sha256(ip.concat(msg))));
  }
  var HEX = "0123456789abcdef";
  function hex(b) { var s = ""; for (var i = 0; i < b.length; i++) s += HEX[b[i] >> 4] + HEX[b[i] & 15]; return s; }
  var B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  function b64url(b) {
    var s = "", i = 0;
    for (; i + 2 < b.length; i += 3) { var v = b[i] << 16 | b[i + 1] << 8 | b[i + 2]; s += B64[v >> 18] + B64[v >> 12 & 63] + B64[v >> 6 & 63] + B64[v & 63]; }
    if (b.length - i === 1) { v = b[i] << 16; s += B64[v >> 18] + B64[v >> 12 & 63]; }
    else if (b.length - i === 2) { v = b[i] << 16 | b[i + 1] << 8; s += B64[v >> 18] + B64[v >> 12 & 63] + B64[v >> 6 & 63]; }
    return s;
  }
  function b64urlDecode(s) {
    s = String(s);
    if (s.length % 4 === 1) throw new TypeError("illegal base64 data at input byte " + (s.length - 1));
    var out = [], acc = 0, nb = 0;
    for (var i = 0; i < s.length; i++) {
      var v = B64.indexOf(s[i]);
      if (v < 0) throw new TypeError("illegal base64 data at input byte " + i);
      acc = acc << 6 | v; nb += 6;
      if (nb >= 8) { nb -= 8; out.push(acc >> nb & 255); }
    }
    return out;
  }
  function sortKeys(v) {
    if (Array.isArray(v)) return v.map(sortKeys);
    if (v !== null && typeof v === "object") {
      var o = {}, ks = Object.keys(v).sort();
      for (var i = 0; i < ks.length; i++) o[ks[i]] = sortKeys(v[ks[i]]);
      return o;
    }
    return v;
  }
  // encoding/json.Marshal: sorted keys, HTML-safe escapes.
  function goJSON(v) {
    return JSON.stringify(sortKeys(JSON.parse(JSON.stringify(v)))).replace(/[<>&\u2028\u2029]/g, function (c) {
      return "\\u" + ("000" + c.charCodeAt(0).toString(16)).slice(-4);
    });
  }
  return {
    hasCapability: function (name) { return name === "json-clone@1" || name === "submit-sse-delta@1"; },
    unixNow: function () { return 1700000000; },
    uuid: function () { return "00000000-0000-4000-8000-000000000000"; },
    hmacSHA256: function (message, secret) { return hex(hmac(utf8(secret), utf8(message))); },
    base64URL: function (value) { return b64url(utf8(value)); },
    base64URLDecode: function (value) { return fromUTF8(b64urlDecode(value)); },
    jwtSignHS256: function (claims, secret) {
      var input = b64url(utf8('{"alg":"HS256","typ":"JWT"}')) + "." + b64url(utf8(goJSON(claims)));
      return input + "." + b64url(hmac(utf8(secret), utf8(input)));
    },
    volcSignV4: function () {
      return {
        "Authorization": "HMAC-SHA256 Credential=AKSTUB/20231114/cn-north-1/cv/request, SignedHeaders=content-type;host;x-content-sha256;x-date, Signature=stub",
        "X-Content-Sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
        "X-Date": "20231114T221320Z",
      };
    },
    json: {
      clone: function (v) {
        if (v === undefined) throw new TypeError("json.clone requires a JSON value");
        return sortKeys(JSON.parse(JSON.stringify(v)));
      },
    },
  };
})();
