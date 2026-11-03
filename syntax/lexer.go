package syntax

import (
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MoeForge/moejs/internal/regexpsyntax"
)

// bailout is the panic value used to abort parsing at the first error. The
// public entry points recover it and return the recorded *Error.
type bailout struct{}

// Character-class table for the ASCII fast paths.
const (
	ccIdentStart uint8 = 1 << iota
	ccIdentPart
	ccDigit
)

var charClass [256]uint8

func init() {
	for c := 'a'; c <= 'z'; c++ {
		charClass[c] = ccIdentStart | ccIdentPart
	}
	for c := 'A'; c <= 'Z'; c++ {
		charClass[c] = ccIdentStart | ccIdentPart
	}
	for c := '0'; c <= '9'; c++ {
		charClass[c] = ccIdentPart | ccDigit
	}
	charClass['$'] = ccIdentStart | ccIdentPart
	charClass['_'] = ccIdentStart | ccIdentPart
}

func isDigit(c byte) bool    { return charClass[c]&ccDigit != 0 }
func isHexDigit(c byte) bool { return isDigit(c) || (c|0x20) >= 'a' && (c|0x20) <= 'f' }

func hexVal(c byte) int {
	if isDigit(c) {
		return int(c - '0')
	}
	return int(c|0x20-'a') + 10
}

// isIDStart implements ES ID_Start plus `$` and `_`, with the Unicode version
// of the regexp tables rather than Go's.
func isIDStart(r rune) bool {
	if r < utf8.RuneSelf {
		return charClass[r]&ccIdentStart != 0
	}
	return regexpsyntax.IsIDStart(r)
}

// isIDPart implements ES ID_Continue plus `$`, ZWNJ and ZWJ.
func isIDPart(r rune) bool {
	if r < utf8.RuneSelf {
		return charClass[r]&ccIdentPart != 0
	}
	return r == 0x200C || r == 0x200D || regexpsyntax.IsIDContinue(r)
}

// isUnicodeSpace reports whether a non-ASCII rune is ES WhiteSpace.
func isUnicodeSpace(r rune) bool {
	return r == 0xA0 || r == 0xFEFF || unicode.Is(unicode.Zs, r)
}

func isLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
}

// isLineSeparatorAt reports whether the UTF-8 encoding of U+2028 or U+2029
// starts at src[pos].
func isLineSeparatorAt(src string, pos int) bool {
	return pos+2 < len(src) && src[pos] == 0xE2 && src[pos+1] == 0x80 && (src[pos+2] == 0xA8 || src[pos+2] == 0xA9)
}

// lexer scans one source text. The current token lives in value fields so
// that tokenising allocates nothing; identifier names and unescaped string
// values are substrings of the source.
type lexer struct {
	file *File
	src  string
	pos  int // scan offset just past the current token

	tok      Token
	nlBefore bool // a line terminator separates this token from the previous one
	escaped  bool // identifier spelled with \u escapes
	tail     bool // template chunk ended with ` rather than ${
	// legacy is the kind of the first legacy octal literal, octal escape or
	// \8/\9 escape of the current token (legacyNone: none). Strict code
	// rejects them as they are scanned (noteLegacy); the parser checks the
	// tokens scanned before a "use strict" directive switched the mode.
	legacy uint8
	html   bool // HTML-like comments are recognised: script code
	strict bool // strict mode code: modules, classes and code under a "use strict" directive

	start int    // byte offset of the current token
	end   int    // byte offset just past the current token
	val   string // Identifier name, String/Template cooked value (WTF-8), Regex pattern, BigInt decimal digits
	raw   string // Template raw text; Regex flags
	num   float64

	// badEscape is 1 + the offset of the first invalid escape in the current
	// template chunk (0: none). Only tagged templates accept one; their
	// cooked value is then undefined.
	badEscape int

	err *Error
}

func (l *lexer) init(file *File) {
	l.file = file
	l.src = file.Src
	l.pos = 0
	if strings.HasPrefix(l.src, "#!") {
		l.pos = l.skipLineComment(2)
	}
}

// fail records the first error and unwinds to the entry point.
func (l *lexer) fail(pos int, msg string) {
	if l.err == nil {
		l.err = l.file.errorAt(pos, msg)
	}
	panic(bailout{})
}

func (l *lexer) text() string { return l.src[l.start:l.end] }

// next scans the next token. `/` is always scanned as division; the parser
// calls rescanRegex when a regular expression is grammatically expected.
func (l *lexer) next() {
	l.nlBefore = false
	l.escaped = false
	if l.legacy != legacyNone {
		l.legacy = legacyNone
	}
	src := l.src
	pos := l.pos
skip:
	for pos < len(src) {
		c := src[pos]
		switch c {
		case ' ', '\t', '\v', '\f':
			pos++
		case '\n':
			pos++
			l.nlBefore = true
			l.file.addLine(pos)
		case '\r':
			pos++
			if pos < len(src) && src[pos] == '\n' {
				pos++
			}
			l.nlBefore = true
			l.file.addLine(pos)
		case '/':
			if pos+1 < len(src) {
				switch src[pos+1] {
				case '/':
					pos = l.skipLineComment(pos + 2)
					continue
				case '*':
					pos = l.skipBlockComment(pos + 2)
					continue
				}
			}
			break skip
		default:
			if c < utf8.RuneSelf {
				break skip
			}
			r, size := utf8.DecodeRuneInString(src[pos:])
			if r == 0x2028 || r == 0x2029 {
				pos += size
				l.nlBefore = true
				l.file.addLine(pos)
				continue
			}
			if isUnicodeSpace(r) {
				pos += size
				continue
			}
			break skip
		}
	}
	l.start = pos
	if pos >= len(src) {
		l.tok = EOF
		l.end, l.pos = pos, pos
		return
	}
	c := src[pos]
	if charClass[c]&ccIdentStart != 0 || c == '\\' || c >= utf8.RuneSelf {
		l.scanIdentifier(pos)
		return
	}
	if isDigit(c) || c == '.' && pos+1 < len(src) && isDigit(src[pos+1]) {
		l.scanNumber(pos)
		return
	}
	l.scanPunctuator(pos)
}

// skipHTMLComment skips the rest of the line of an HTML-like comment
// (Annex B.1.1) whose opener ends before pos and scans the token after it,
// which keeps a line break seen before the comment.
func (l *lexer) skipHTMLComment(pos int) {
	l.pos = l.skipLineComment(pos)
	nl := l.nlBefore
	l.next()
	l.nlBefore = l.nlBefore || nl
}

// skipLineComment returns the offset of the line terminator ending the
// comment (or len(src)); the terminator itself is handled by next.
func (l *lexer) skipLineComment(pos int) int {
	src := l.src
	for pos < len(src) {
		c := src[pos]
		if c == '\n' || c == '\r' {
			return pos
		}
		if c == 0xE2 && pos+2 < len(src) && src[pos+1] == 0x80 && (src[pos+2] == 0xA8 || src[pos+2] == 0xA9) {
			return pos
		}
		pos++
	}
	return pos
}

func (l *lexer) skipBlockComment(pos int) int {
	src := l.src
	start := pos - 2
	for pos < len(src) {
		c := src[pos]
		switch c {
		case '*':
			if pos+1 < len(src) && src[pos+1] == '/' {
				return pos + 2
			}
			pos++
		case '\n':
			pos++
			l.nlBefore = true
			l.file.addLine(pos)
		case '\r':
			pos++
			if pos < len(src) && src[pos] == '\n' {
				pos++
			}
			l.nlBefore = true
			l.file.addLine(pos)
		case 0xE2:
			if pos+2 < len(src) && src[pos+1] == 0x80 && (src[pos+2] == 0xA8 || src[pos+2] == 0xA9) {
				pos += 3
				l.nlBefore = true
				l.file.addLine(pos)
				continue
			}
			pos++
		default:
			pos++
		}
	}
	l.fail(start, "Unterminated comment")
	return pos
}

func (l *lexer) scanIdentifier(start int) {
	src := l.src
	pos := start
	for pos < len(src) && charClass[src[pos]]&ccIdentPart != 0 {
		pos++
	}
	if pos < len(src) && (src[pos] == '\\' || src[pos] >= utf8.RuneSelf) {
		l.scanIdentifierSlow(start, pos)
		return
	}
	if pos == start {
		l.fail(start, "Invalid or unexpected token")
	}
	l.val = src[start:pos]
	l.tok = lookupKeyword(l.val)
	l.end, l.pos = pos, pos
}

// scanIdentifierSlow handles identifiers containing non-ASCII characters or
// \u escapes. The cooked name is built in a buffer.
func (l *lexer) scanIdentifierSlow(start, pos int) {
	src := l.src
	buf := make([]byte, 0, 16)
	buf = append(buf, src[start:pos]...)
	first := pos == start
scan:
	for pos < len(src) {
		c := src[pos]
		var r rune
		var size int
		switch {
		case c == '\\':
			if pos+1 >= len(src) || src[pos+1] != 'u' {
				l.fail(pos, "Invalid Unicode escape sequence")
			}
			r, size = l.scanUnicodeEscape(pos + 2)
			size += 2
			l.escaped = true
		case c < utf8.RuneSelf:
			if charClass[c]&ccIdentPart == 0 {
				break scan
			}
			r, size = rune(c), 1
		default:
			r, size = utf8.DecodeRuneInString(src[pos:])
			if r == utf8.RuneError && size == 1 {
				l.fail(pos, "Invalid or unexpected token")
			}
		}
		if first {
			if !isIDStart(r) {
				l.fail(pos, "Invalid or unexpected token")
			}
			first = false
		} else if !isIDPart(r) {
			if c == '\\' {
				l.fail(pos, "Invalid Unicode escape sequence")
			}
			break scan
		}
		buf = utf8.AppendRune(buf, r)
		pos += size
	}
	if first {
		l.fail(start, "Invalid or unexpected token")
	}
	l.val = string(buf)
	// An unescaped keyword gets here when a non-ASCII separator such as
	// U+2028 ends it; an escaped one is only an IdentifierName.
	l.tok = lookupKeyword(l.val)
	if l.escaped && l.tok != Identifier {
		l.tok = EscapedWord
	}
	l.end, l.pos = pos, pos
}

// scanUnicodeEscape scans the part of a \u escape after "\u": either XXXX or
// {X...}. It returns the code point and the number of bytes consumed.
func (l *lexer) scanUnicodeEscape(pos int) (rune, int) {
	r, size, msg := l.peekUnicodeEscape(pos)
	if msg != "" {
		l.fail(pos-2, msg)
	}
	return r, size
}

// peekUnicodeEscape is scanUnicodeEscape without failing: msg is the error
// an invalid escape reports.
func (l *lexer) peekUnicodeEscape(pos int) (r rune, size int, msg string) {
	src := l.src
	if pos < len(src) && src[pos] == '{' {
		q := pos + 1
		var v rune
		for q < len(src) && isHexDigit(src[q]) {
			v = v<<4 | rune(hexVal(src[q]))
			if v > unicode.MaxRune {
				return 0, 0, "Undefined Unicode code-point"
			}
			q++
		}
		if q == pos+1 || q >= len(src) || src[q] != '}' {
			return 0, 0, "Invalid Unicode escape sequence"
		}
		return v, q + 1 - pos, ""
	}
	if pos+4 > len(src) {
		return 0, 0, "Invalid Unicode escape sequence"
	}
	var v rune
	for i := range 4 {
		c := src[pos+i]
		if !isHexDigit(c) {
			return 0, 0, "Invalid Unicode escape sequence"
		}
		v = v<<4 | rune(hexVal(c))
	}
	return v, 4, ""
}

// maxBigIntBits is the engine's bound on the magnitude of a BigInt
// (SpiderMonkey's limit, 2^20 bits; engine/bigint.go): a larger literal is
// rejected, as in SpiderMonkey. maxBigIntDigits is the number of decimal
// digits of 2^maxBigIntBits.
const (
	maxBigIntBits   = 1 << 20
	maxBigIntDigits = 315653
)

func (l *lexer) scanNumber(start int) {
	src := l.src
	pos := start
	l.tok = Number
	if src[pos] == '0' && pos+1 < len(src) {
		switch src[pos+1] | 0x20 {
		case 'x':
			l.scanRadix(start, pos+2, 16)
			return
		case 'o':
			l.scanRadix(start, pos+2, 8)
			return
		case 'b':
			l.scanRadix(start, pos+2, 2)
			return
		}
		if isDigit(src[pos+1]) {
			l.scanLegacyNumber(start)
			return
		}
		if src[pos+1] == '_' {
			l.fail(pos+1, "Numeric separator can not be used after leading 0.")
		}
	}
	hasSep := false
	isInt := true
	pos = l.scanDigits(pos, &hasSep)
	if pos < len(src) && src[pos] == '.' {
		isInt = false
		pos++
		if pos < len(src) && src[pos] == '_' {
			l.fail(pos, "Numeric separators are not allowed here.")
		}
		pos = l.scanDigits(pos, &hasSep)
	}
	if pos < len(src) && src[pos]|0x20 == 'e' {
		isInt = false
		q := pos + 1
		if q < len(src) && (src[q] == '+' || src[q] == '-') {
			q++
		}
		if q >= len(src) || !isDigit(src[q]) {
			l.fail(start, "Invalid or unexpected token")
		}
		pos = l.scanDigits(q, &hasSep)
	}
	if pos < len(src) && src[pos] == 'n' {
		if !isInt {
			l.fail(start, "Invalid BigInt literal")
		}
		text := src[start:pos]
		if hasSep {
			text = strings.ReplaceAll(text, "_", "")
		}
		l.val = strings.TrimLeft(text, "0")
		if l.val == "" {
			l.val = "0"
		}
		if n := len(l.val); n > maxBigIntDigits || n == maxBigIntDigits && decimalBitLen(l.val) > maxBigIntBits {
			l.fail(start, "Maximum BigInt size exceeded")
		}
		l.tok = BigInt
		pos++
	} else {
		text := src[start:pos]
		if hasSep {
			text = strings.ReplaceAll(text, "_", "")
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			// Only ErrRange is possible for a validated literal; the
			// returned ±Inf or 0 is the correct ES value.
			if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
				l.fail(start, "Invalid or unexpected token")
			}
		}
		l.num = f
	}
	l.checkAfterNumber(pos)
	l.end, l.pos = pos, pos
}

// Legacy literal kinds (lexer.legacy), each an early error in strict code.
const (
	legacyNone    = iota
	legacyOctal   // 017
	legacyDecimal // 08, 019.5
	legacyEscape  // "\01"
	legacyEscape89
)

var legacyMessages = [...]string{
	legacyOctal:    "Octal literals are not allowed in strict mode.",
	legacyDecimal:  "Decimals with leading zeros are not allowed in strict mode.",
	legacyEscape:   "Octal escape sequences are not allowed in strict mode.",
	legacyEscape89: "\\8 and \\9 are not allowed in strict mode.",
}

// noteLegacy records the first legacy construct of the current token,
// which strict code rejects.
func (l *lexer) noteLegacy(kind uint8, pos int) {
	if l.legacy != legacyNone {
		return
	}
	l.legacy = kind
	if l.strict {
		l.fail(pos, legacyMessages[kind])
	}
}

// legacyError returns the position and message of the current token's
// legacy construct: the literal itself, or the first legacy escape of a
// string literal, found again rather than kept for this rare error.
func (l *lexer) legacyError() (int, string) {
	pos := l.start
	if l.tok == String {
		src := l.src[:l.end]
		for pos++; pos+1 < len(src); pos++ {
			if src[pos] != '\\' {
				continue
			}
			// The escaped character cannot start another escape.
			if c := src[pos+1]; c >= '1' && c <= '9' || c == '0' && pos+2 < len(src) && isDigit(src[pos+2]) {
				break
			}
			pos++
		}
	}
	return pos, legacyMessages[l.legacy]
}

// scanLegacyNumber scans a literal whose leading 0 is followed by a digit:
// a LegacyOctalIntegerLiteral (017), or a NonOctalDecimalIntegerLiteral
// (08, 019), which may go on as a decimal literal with a fraction or an
// exponent. Neither takes a separator in its integer part or a BigInt
// suffix.
func (l *lexer) scanLegacyNumber(start int) {
	src := l.src
	pos := start + 1
	octal := true
	for pos < len(src) && isDigit(src[pos]) {
		if src[pos] >= '8' {
			octal = false
		}
		pos++
	}
	if pos < len(src) && src[pos] == '_' {
		l.fail(pos, "Numeric separator can not be used after leading 0.")
	}
	if octal {
		l.noteLegacy(legacyOctal, start)
		digits := src[start+1 : pos]
		if n, err := strconv.ParseUint(digits, 8, 64); err == nil && n < 1<<53 {
			l.num = float64(n)
		} else {
			v, _ := new(big.Int).SetString(digits, 8)
			l.num, _ = new(big.Float).SetInt(v).Float64()
		}
	} else {
		l.noteLegacy(legacyDecimal, start)
		hasSep := false
		if pos < len(src) && src[pos] == '.' {
			pos++
			if pos < len(src) && src[pos] == '_' {
				l.fail(pos, "Numeric separators are not allowed here.")
			}
			pos = l.scanDigits(pos, &hasSep)
		}
		if pos < len(src) && src[pos]|0x20 == 'e' {
			q := pos + 1
			if q < len(src) && (src[q] == '+' || src[q] == '-') {
				q++
			}
			if q >= len(src) || !isDigit(src[q]) {
				l.fail(start, "Invalid or unexpected token")
			}
			pos = l.scanDigits(q, &hasSep)
		}
		text := src[start:pos]
		if hasSep {
			text = strings.ReplaceAll(text, "_", "")
		}
		l.num, _ = strconv.ParseFloat(text, 64) // only ErrRange, with the right ±Inf
	}
	if pos < len(src) && src[pos] == 'n' {
		l.fail(start, "Invalid BigInt literal")
	}
	l.tok = Number
	l.checkAfterNumber(pos)
	l.end, l.pos = pos, pos
}

// decimalBitLen returns the bit length of the value of the decimal digits s.
func decimalBitLen(s string) int {
	v, _ := new(big.Int).SetString(s, 10)
	return v.BitLen()
}

// scanDigits scans decimal digits with optional single `_` separators
// between digits, starting at pos (which follows a digit or a `.`).
func (l *lexer) scanDigits(pos int, hasSep *bool) int {
	src := l.src
	for pos < len(src) {
		c := src[pos]
		if isDigit(c) {
			pos++
			continue
		}
		if c != '_' {
			break
		}
		if !isDigit(src[pos-1]) {
			l.fail(pos, "Numeric separators are not allowed here.")
		}
		if pos+1 < len(src) && src[pos+1] == '_' {
			l.fail(pos, "Only one underscore is allowed as numeric separator")
		}
		if pos+1 >= len(src) || !isDigit(src[pos+1]) {
			l.fail(pos, "Numeric separators are not allowed at the end of numeric literals")
		}
		*hasSep = true
		pos++
	}
	return pos
}

// scanRadix scans a 0x/0o/0b literal body starting at pos.
func (l *lexer) scanRadix(start, pos int, base int) {
	src := l.src
	digitsStart := pos
	var acc uint64
	over := false // acc overflowed; the value is read from the digits
	for pos < len(src) {
		c := src[pos]
		if c == '_' {
			if pos == digitsStart || pos+1 >= len(src) || !isRadixDigit(src[pos+1], base) {
				l.fail(pos, "Numeric separators are not allowed here.")
			}
			pos++
			continue
		}
		if !isRadixDigit(c, base) {
			break
		}
		d := uint64(hexVal(c))
		if acc > (^uint64(0)-d)/uint64(base) {
			over = true
		} else {
			acc = acc*uint64(base) + d
		}
		pos++
	}
	if pos == digitsStart || pos < len(src) && isDigit(src[pos]) {
		l.fail(start, "Invalid or unexpected token")
	}
	isBigInt := pos < len(src) && src[pos] == 'n'
	var v *big.Int
	if over {
		// The digits are packed into words, in linear time. A Number of
		// more than 1024 bits is at least 2^1024, which rounds to Infinity.
		digits := strings.TrimLeft(src[digitsStart:pos], "0_")
		shift := uint(bits.TrailingZeros(uint(base)))
		n := len(digits) - strings.Count(digits, "_")
		nbits := (n-1)*int(shift) + bits.Len(uint(hexVal(digits[0])))
		switch {
		case isBigInt && nbits > maxBigIntBits:
			l.fail(start, "Maximum BigInt size exceeded")
		case !isBigInt && nbits > 1024:
			l.num = math.Inf(1)
		default:
			v = packRadixDigits(digits, shift, nbits)
		}
	}
	if isBigInt {
		l.tok = BigInt
		if v == nil {
			l.val = strconv.FormatUint(acc, 10)
		} else {
			// Hex, which converts in linear time where decimal does not.
			l.val = "0x" + v.Text(16)
		}
		pos++
	} else if v != nil {
		l.num, _ = new(big.Float).SetInt(v).Float64()
	} else if !over {
		l.num = float64(acc)
	}
	l.checkAfterNumber(pos)
	l.end, l.pos = pos, pos
}

// packRadixDigits returns the value of the digits of a power-of-two base
// with shift bits per digit, skipping separators; nbits is its bit length.
func packRadixDigits(digits string, shift uint, nbits int) *big.Int {
	words := make([]big.Word, (nbits+bits.UintSize-1)/bits.UintSize)
	p := uint(0)
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c == '_' {
			continue
		}
		d := big.Word(hexVal(c))
		w, o := p/bits.UintSize, p%bits.UintSize
		words[w] |= d << o
		if o+shift > bits.UintSize && int(w)+1 < len(words) {
			words[w+1] |= d >> (bits.UintSize - o)
		}
		p += shift
	}
	return new(big.Int).SetBits(words)
}

func isRadixDigit(c byte, base int) bool {
	switch base {
	case 16:
		return isHexDigit(c)
	case 8:
		return c >= '0' && c <= '7'
	default:
		return c == '0' || c == '1'
	}
}

// checkAfterNumber rejects an identifier start glued to a numeric literal
// (`3in`, `0x1g`).
func (l *lexer) checkAfterNumber(pos int) {
	if pos >= len(l.src) {
		return
	}
	c := l.src[pos]
	if c < utf8.RuneSelf {
		if charClass[c]&ccIdentStart != 0 || c == '\\' {
			l.fail(pos, "Identifier directly after number")
		}
		return
	}
	if r, _ := utf8.DecodeRuneInString(l.src[pos:]); isIDStart(r) {
		l.fail(pos, "Identifier directly after number")
	}
}

func (l *lexer) scanPunctuator(pos int) {
	src := l.src
	c := src[pos]
	at := func(i int) byte {
		if pos+i < len(src) {
			return src[pos+i]
		}
		return 0
	}
	tok, n := Illegal, 1
	switch c {
	case '{':
		tok = LBrace
	case '}':
		tok = RBrace
	case '(':
		tok = LParen
	case ')':
		tok = RParen
	case '[':
		tok = LBrack
	case ']':
		tok = RBrack
	case ';':
		tok = Semicolon
	case ',':
		tok = Comma
	case ':':
		tok = Colon
	case '~':
		tok = BitNot
	case '.':
		tok = Dot
		if at(1) == '.' && at(2) == '.' {
			tok, n = Ellipsis, 3
		}
	case '?':
		tok = Question
		switch at(1) {
		case '?':
			tok, n = Nullish, 2
			if at(2) == '=' {
				tok, n = NullishAssign, 3
			}
		case '.':
			// `?.` followed by a digit is a conditional with a number: a?.5:b
			if !isDigit(at(2)) {
				tok, n = QuestionDot, 2
			}
		}
	case '<':
		if l.html && at(1) == '!' && at(2) == '-' && at(3) == '-' {
			l.skipHTMLComment(pos + 4)
			return
		}
		tok = Lt
		switch at(1) {
		case '=':
			tok, n = LtEq, 2
		case '<':
			tok, n = Shl, 2
			if at(2) == '=' {
				tok, n = ShlAssign, 3
			}
		}
	case '>':
		tok = Gt
		switch at(1) {
		case '=':
			tok, n = GtEq, 2
		case '>':
			tok, n = Shr, 2
			switch at(2) {
			case '=':
				tok, n = ShrAssign, 3
			case '>':
				tok, n = UShr, 3
				if at(3) == '=' {
					tok, n = UShrAssign, 4
				}
			}
		}
	case '=':
		tok = Assign
		switch at(1) {
		case '=':
			tok, n = Eq, 2
			if at(2) == '=' {
				tok, n = StrictEq, 3
			}
		case '>':
			tok, n = Arrow, 2
		}
	case '!':
		tok = Not
		if at(1) == '=' {
			tok, n = NotEq, 2
			if at(2) == '=' {
				tok, n = StrictNeq, 3
			}
		}
	case '+':
		tok = Plus
		switch at(1) {
		case '+':
			tok, n = Inc, 2
		case '=':
			tok, n = AddAssign, 2
		}
	case '-':
		tok = Minus
		switch at(1) {
		case '-':
			// `-->` first on a line (after white space and comments) or
			// first in the source is an HTML close comment.
			if l.html && at(2) == '>' && (l.nlBefore || l.end == 0) {
				l.skipHTMLComment(pos + 3)
				return
			}
			tok, n = Dec, 2
		case '=':
			tok, n = SubAssign, 2
		}
	case '*':
		tok = Mul
		switch at(1) {
		case '*':
			tok, n = Exp, 2
			if at(2) == '=' {
				tok, n = ExpAssign, 3
			}
		case '=':
			tok, n = MulAssign, 2
		}
	case '/':
		tok = Div
		if at(1) == '=' {
			tok, n = DivAssign, 2
		}
	case '%':
		tok = Rem
		if at(1) == '=' {
			tok, n = RemAssign, 2
		}
	case '&':
		tok = BitAnd
		switch at(1) {
		case '&':
			tok, n = LogAnd, 2
			if at(2) == '=' {
				tok, n = LogAndAssign, 3
			}
		case '=':
			tok, n = AndAssign, 2
		}
	case '|':
		tok = BitOr
		switch at(1) {
		case '|':
			tok, n = LogOr, 2
			if at(2) == '=' {
				tok, n = LogOrAssign, 3
			}
		case '=':
			tok, n = OrAssign, 2
		}
	case '^':
		tok = BitXor
		if at(1) == '=' {
			tok, n = XorAssign, 2
		}
	case '"', '\'':
		l.scanString(pos, c)
		return
	case '`':
		l.scanTemplate(pos, pos+1)
		return
	case '#':
		if q := pos + 1; q < len(src) && (charClass[src[q]]&ccIdentStart != 0 || src[q] == '\\' || src[q] >= utf8.RuneSelf) {
			l.scanIdentifier(q)
			l.tok = PrivateIdent
			l.start = pos
			return
		}
		l.fail(pos, "Invalid or unexpected token")
	default:
		l.fail(pos, "Invalid or unexpected token")
	}
	l.tok = tok
	l.end, l.pos = pos+n, pos+n
}

func (l *lexer) scanString(start int, quote byte) {
	src := l.src
	pos := start + 1
	for pos < len(src) {
		c := src[pos]
		if c == quote {
			l.val = src[start+1 : pos]
			l.tok = String
			l.end, l.pos = pos+1, pos+1
			return
		}
		if c == '\\' || c == '\n' || c == '\r' {
			break
		}
		if c == 0xE2 && isLineSeparatorAt(src, pos) {
			l.file.addLine(pos + 3)
		}
		pos++
	}
	// Slow path: escapes or an error.
	buf := make([]byte, 0, pos-start+16)
	buf = append(buf, src[start+1:pos]...)
	for pos < len(src) {
		c := src[pos]
		switch c {
		case quote:
			l.val = string(buf)
			l.tok = String
			l.end, l.pos = pos+1, pos+1
			return
		case '\\':
			pos, buf = l.scanEscape(pos+1, buf)
		case '\n', '\r':
			l.fail(start, "Unterminated string constant")
		default:
			if c == 0xE2 && isLineSeparatorAt(src, pos) {
				l.file.addLine(pos + 3)
			}
			buf = append(buf, c)
			pos++
		}
	}
	l.fail(start, "Unterminated string constant")
}

// scanEscape decodes the escape whose backslash precedes pos and appends the
// cooked bytes to buf. Templates check templateEscapeError first, so the
// legacy escapes recorded here are the string-literal ones.
func (l *lexer) scanEscape(pos int, buf []byte) (int, []byte) {
	src := l.src
	if pos >= len(src) {
		l.fail(pos-1, "Unterminated string constant")
	}
	c := src[pos]
	switch c {
	case 'n':
		return pos + 1, append(buf, '\n')
	case 't':
		return pos + 1, append(buf, '\t')
	case 'r':
		return pos + 1, append(buf, '\r')
	case 'b':
		return pos + 1, append(buf, '\b')
	case 'f':
		return pos + 1, append(buf, '\f')
	case 'v':
		return pos + 1, append(buf, '\v')
	case '0', '1', '2', '3', '4', '5', '6', '7':
		if c == '0' && (pos+1 >= len(src) || !isDigit(src[pos+1])) {
			return pos + 1, append(buf, 0)
		}
		// A LegacyOctalEscapeSequence: up to three digits from \0-\3,
		// two from \4-\7 (\08 is \0 followed by 8).
		l.noteLegacy(legacyEscape, pos-1)
		v := rune(c - '0')
		pos++
		if pos < len(src) && src[pos] >= '0' && src[pos] <= '7' {
			v = v*8 + rune(src[pos]-'0')
			pos++
			if c <= '3' && pos < len(src) && src[pos] >= '0' && src[pos] <= '7' {
				v = v*8 + rune(src[pos]-'0')
				pos++
			}
		}
		return pos, appendWTF8(buf, v)
	case '8', '9':
		l.noteLegacy(legacyEscape89, pos-1)
		return pos + 1, append(buf, c)
	case 'x':
		if pos+2 >= len(src) || !isHexDigit(src[pos+1]) || !isHexDigit(src[pos+2]) {
			l.fail(pos-1, "Invalid hexadecimal escape sequence")
		}
		return pos + 3, appendWTF8(buf, rune(hexVal(src[pos+1])<<4|hexVal(src[pos+2])))
	case 'u':
		r, size := l.scanUnicodeEscape(pos + 1)
		pos += 1 + size
		if r >= 0xD800 && r <= 0xDBFF && pos+1 < len(src) && src[pos] == '\\' && src[pos+1] == 'u' {
			// Combine a surrogate pair spelled as two escapes; an invalid
			// second escape is reported when it is scanned on its own.
			lo, size2, msg := l.peekUnicodeEscape(pos + 2)
			if msg == "" && lo >= 0xDC00 && lo <= 0xDFFF {
				r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
				pos += 2 + size2
			}
		}
		return pos, appendWTF8(buf, r)
	case '\r':
		pos++
		if pos < len(src) && src[pos] == '\n' {
			pos++
		}
		l.file.addLine(pos)
		return pos, buf
	case '\n':
		l.file.addLine(pos + 1)
		return pos + 1, buf
	}
	if c < utf8.RuneSelf {
		return pos + 1, append(buf, c)
	}
	r, size := utf8.DecodeRuneInString(src[pos:])
	if r == 0x2028 || r == 0x2029 {
		l.file.addLine(pos + size)
		return pos + size, buf
	}
	return pos + size, append(buf, src[pos:pos+size]...)
}

// templateEscapeError returns the error of the escape whose backslash
// precedes pos when it is not a valid template escape (a NotEscapeSequence),
// or "" when it is valid.
func (l *lexer) templateEscapeError(pos int) string {
	src := l.src
	if pos >= len(src) {
		return ""
	}
	switch c := src[pos]; c {
	case '0', '1', '2', '3', '4', '5', '6', '7':
		if c != '0' || pos+1 < len(src) && isDigit(src[pos+1]) {
			return "Octal escape sequences are not allowed in template strings."
		}
	case '8', '9':
		return "\\8 and \\9 are not allowed in template strings."
	case 'x':
		if pos+2 >= len(src) || !isHexDigit(src[pos+1]) || !isHexDigit(src[pos+2]) {
			return "Invalid hexadecimal escape sequence"
		}
	case 'u':
		_, _, msg := l.peekUnicodeEscape(pos + 1)
		return msg
	}
	return ""
}

// scanTemplate scans a template chunk starting at pos (just after ` or }).
// start is the offset of the delimiter so the token spans it.
func (l *lexer) scanTemplate(start, pos int) {
	src := l.src
	rawStart := pos
	l.badEscape = 0
	needCopy := false
	// Fast path: find the end without escapes or CR.
	q := pos
	for q < len(src) {
		c := src[q]
		if c == '`' || c == '$' && q+1 < len(src) && src[q+1] == '{' {
			break
		}
		if c == '\\' || c == '\r' {
			needCopy = true
			break
		}
		if c == '\n' {
			l.file.addLine(q + 1)
		} else if c == 0xE2 && isLineSeparatorAt(src, q) {
			l.file.addLine(q + 3)
		}
		q++
	}
	if !needCopy {
		if q >= len(src) {
			l.fail(start, "Unterminated template literal")
		}
		l.val = src[rawStart:q]
		l.raw = l.val
		l.finishTemplate(start, q)
		return
	}
	cooked := make([]byte, 0, q-rawStart+16)
	cooked = append(cooked, src[rawStart:q]...)
	raw := make([]byte, 0, q-rawStart+16)
	raw = append(raw, src[rawStart:q]...)
	pos = q
	for pos < len(src) {
		c := src[pos]
		switch {
		case c == '`' || c == '$' && pos+1 < len(src) && src[pos+1] == '{':
			l.val = string(cooked)
			if l.badEscape != 0 {
				l.val = ""
			}
			l.raw = string(raw)
			l.finishTemplate(start, pos)
			return
		case c == '\\':
			if l.templateEscapeError(pos+1) != "" {
				// NotEscapeSequence: the raw text keeps it and the cooked
				// value is undefined. It starts with an ASCII digit, x or u;
				// the rest of it (digits, hex digits, {) is copied like
				// ordinary text.
				if l.badEscape == 0 {
					l.badEscape = pos + 1
				}
				raw = append(raw, '\\', src[pos+1])
				pos += 2
				continue
			}
			escStart := pos
			pos, cooked = l.scanEscape(pos+1, cooked)
			raw = appendTemplateRaw(raw, src[escStart:pos])
		case c == '\r':
			pos++
			if pos < len(src) && src[pos] == '\n' {
				pos++
			}
			l.file.addLine(pos)
			cooked = append(cooked, '\n')
			raw = append(raw, '\n')
		default:
			if c == '\n' {
				l.file.addLine(pos + 1)
			} else if c == 0xE2 && isLineSeparatorAt(src, pos) {
				l.file.addLine(pos + 3)
			}
			cooked = append(cooked, c)
			raw = append(raw, c)
			pos++
		}
	}
	l.fail(start, "Unterminated template literal")
}

// appendTemplateRaw appends escape source text to the raw value, normalising
// CR and CRLF inside line continuations to LF as the spec's TRV does.
func appendTemplateRaw(raw []byte, text string) []byte {
	for i := 0; i < len(text); i++ {
		if text[i] == '\r' {
			raw = append(raw, '\n')
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			continue
		}
		raw = append(raw, text[i])
	}
	return raw
}

func (l *lexer) finishTemplate(start, pos int) {
	l.tok = Template
	l.start = start
	if l.src[pos] == '`' {
		l.tail = true
		l.end, l.pos = pos+1, pos+1
		return
	}
	l.tail = false
	l.end, l.pos = pos+2, pos+2
}

// rescanTemplateContinuation re-reads the current `}` token as the template
// chunk that follows a substitution.
func (l *lexer) rescanTemplateContinuation() {
	l.scanTemplate(l.start, l.start+1)
}

// rescanRegex re-reads the current `/` or `/=` token as a regular expression
// literal. val receives the pattern and raw the flags.
func (l *lexer) rescanRegex() {
	src := l.src
	start := l.start
	pos := start + 1
	inClass := false
	for {
		if pos >= len(src) {
			l.fail(start, "Invalid regular expression: missing /")
		}
		c := src[pos]
		switch {
		case c == '\\':
			pos++
			if pos >= len(src) {
				l.fail(start, "Invalid regular expression: missing /")
			}
			c = src[pos]
			if c == '\n' || c == '\r' {
				l.fail(start, "Invalid regular expression: missing /")
			}
		case c == '\n' || c == '\r':
			l.fail(start, "Invalid regular expression: missing /")
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			l.val = src[start+1 : pos]
			pos++
			l.scanRegexFlags(pos)
			return
		}
		if c < utf8.RuneSelf {
			pos++
			continue
		}
		r, size := utf8.DecodeRuneInString(src[pos:])
		if r == 0x2028 || r == 0x2029 {
			l.fail(start, "Invalid regular expression: missing /")
		}
		pos += size
	}
}

func (l *lexer) scanRegexFlags(pos int) {
	src := l.src
	flagsStart := pos
	var seen uint32
	for pos < len(src) {
		c := src[pos]
		if c >= utf8.RuneSelf {
			r, _ := utf8.DecodeRuneInString(src[pos:])
			if !isIDPart(r) {
				break
			}
			l.fail(flagsStart, "Invalid regular expression flags")
		}
		if charClass[c]&ccIdentPart == 0 && c != '\\' {
			break
		}
		i := strings.IndexByte("dgimsuvy", c)
		if i < 0 || seen&(1<<i) != 0 {
			l.fail(flagsStart, "Invalid regular expression flags")
		}
		seen |= 1 << i
		pos++
	}
	if seen&(1<<5|1<<6) == 1<<5|1<<6 { // u and v are exclusive
		l.fail(flagsStart, "Invalid regular expression flags")
	}
	l.raw = src[flagsStart:pos]
	l.tok = Regex
	l.end, l.pos = pos, pos
}
