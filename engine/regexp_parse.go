package engine

import "github.com/MoeForge/moejs/internal/regexpsyntax"

// Pattern parsing lives in internal/regexpsyntax, which the syntax package
// shares to reject invalid regexp literals at parse time. The aliases below
// give the engine's regexp files short local names.

type (
	reNode             = regexpsyntax.Node
	reAST              = regexpsyntax.AST
	runeSet            = regexpsyntax.Set
	caseMap            = regexpsyntax.CaseMap
	regexpInvalidError = regexpsyntax.Error
)

const (
	reOpEmpty    = regexpsyntax.OpEmpty
	reOpChar     = regexpsyntax.OpChar
	reOpSeq      = regexpsyntax.OpSeq
	reOpAlt      = regexpsyntax.OpAlt
	reOpCapture  = regexpsyntax.OpCapture
	reOpRepeat   = regexpsyntax.OpRepeat
	reOpLook     = regexpsyntax.OpLook
	reOpBackref  = regexpsyntax.OpBackref
	reOpBegin    = regexpsyntax.OpBegin
	reOpEnd      = regexpsyntax.OpEnd
	reOpWordB    = regexpsyntax.OpWordB
	reOpNotWordB = regexpsyntax.OpNotWordB
	maxCodePoint = regexpsyntax.MaxCodePoint
)

// The normalization tables (unicode_norm_tables.go) come from the same UCD
// version as the regexp tables and use their range encoding.

// ucdTable is one generated range table in decodeRanges format.
type ucdTable struct {
	name string
	data string
}

const unicodeVersion = regexpsyntax.UnicodeVersion

func decodeRanges(s string) runeSet { return regexpsyntax.DecodeRanges(s) }

// parseRegExp parses a pattern (UTF-16 code units). Errors are
// *regexpInvalidError.
func parseRegExp(pattern []uint16, flags regexpFlags) (*reAST, error) {
	return regexpsyntax.Parse(pattern, regexpsyntax.Flags{IgnoreCase: flags.ignoreCase, Multiline: flags.multiline,
		DotAll: flags.dotAll, Unicode: flags.unicode, UnicodeSets: flags.unicodeSets})
}

// canonicalizer returns the Canonicalize map for the flags (u or v select
// simple case folding).
func canonicalizer(unicodeMode bool) *caseMap { return regexpsyntax.Canonicalizer(unicodeMode) }

func digitValueUnit(c uint16) int {
	if c >= 0x80 {
		return -1
	}
	return digitValue(byte(c))
}
