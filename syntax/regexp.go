package syntax

import (
	"strings"
	"sync/atomic"

	"github.com/MoeForge/moejs/internal/regexpsyntax"
)

// regexpChecker validates the regexp literals of one parse, reusing its
// buffers from one literal to the next.
type regexpChecker struct {
	c     regexpsyntax.Checker
	units []uint16
}

// regexpCache holds the checker of the last parse that met a regexp
// literal, so checking the literals of a compile allocates nothing once its
// buffers have grown. Concurrent parses take turns with it (the others
// allocate their own). Unlike a sync.Pool it survives garbage collections,
// which a compile triggers every few parses.
var regexpCache atomic.Pointer[regexpChecker]

// regexpCachedUnits is the longest pattern buffer the cached checker keeps.
const regexpCachedUnits = 4096

// releaseRegexp hands the parse's checker back to the cache.
func (p *parser) releaseRegexp() {
	if re := p.re; re != nil && cap(re.units) <= regexpCachedUnits {
		regexpCache.Store(re)
	}
	p.re = nil
}

// checkRegex reports an invalid pattern in the regexp literal just scanned
// (val holds the pattern, raw the flags) as an early error, with the message
// the RegExp constructor would give. The check builds no tree; the engine
// parses the pattern again when the literal is evaluated.
func (p *parser) checkRegex(start int) {
	var f regexpsyntax.Flags
	for i := range len(p.raw) {
		switch p.raw[i] {
		case 'i':
			f.IgnoreCase = true
		case 'm':
			f.Multiline = true
		case 's':
			f.DotAll = true
		case 'u':
			f.Unicode = true
		case 'v': // v implies the u flag's code-point semantics
			f.Unicode, f.UnicodeSets = true, true
		}
	}
	if p.re == nil {
		if p.re = regexpCache.Swap(nil); p.re == nil {
			p.re = new(regexpChecker)
		}
	}
	// Source text compiled from a String keeps its lone surrogates as WTF-8
	// (engine sourceText); other invalid UTF-8 reads as U+FFFD, an ordinary
	// character.
	units := appendWTF8Units(p.re.units[:0], p.val)
	p.re.units = units
	if err := p.re.c.Check(units, f); err != nil {
		var flags strings.Builder
		for _, c := range "dgimsuvy" {
			if strings.ContainsRune(p.raw, c) {
				flags.WriteRune(c)
			}
		}
		p.fail(start, "Invalid regular expression: /"+p.val+"/"+flags.String()+": "+err.Error())
	}
}
