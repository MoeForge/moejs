// Package bytecode defines the moejs instruction set and the compiled-function
// template shared between the compiler and the engine's interpreter.
//
// See doc.go for the ISA reference and opcodes.go for the encoding. The
// types in this file are the data model: nothing in a Function is mutated
// after compilation (but for the name index an EvalLevel caches, see
// EvalLevel.Slot), so one *Function is shared by every realm (and every
// goroutine) that instantiates the same module. Per-realm state (inline
// caches, materialized constants) lives in engine side tables keyed by
// *Function.
package bytecode

import (
	"slices"
	"sort"
	"strings"
	"sync/atomic"
)

// Kind classifies a compiled function template.
type Kind uint8

const (
	// KindNormal is an ordinary function declaration or expression.
	KindNormal Kind = iota
	// KindArrow is an arrow function (lexical this, not constructible).
	KindArrow
	// KindMethod is a concise object-literal method (not constructible).
	KindMethod
	// KindModule is the module top-level body.
	KindModule
	// KindScript is a script top-level body (Realm.RunScript): its
	// top-level declarations are global bindings reached by name.
	KindScript
	// KindClassCtor is a base class constructor: [[Construct]] creates
	// `this` from new.target and [[Call]] throws (CtorEntry). The class
	// kinds come last so IsClassCtor is one comparison.
	KindClassCtor
	// KindDerivedCtor is the constructor of a class with an extends clause:
	// `this` stays uninitialized until super() returns.
	KindDerivedCtor
)

// IsClassCtor reports whether k is a class constructor kind.
func (k Kind) IsClassCtor() bool { return k >= KindClassCtor }

// String returns the kind name used by disassembly and error messages.
func (k Kind) String() string {
	switch k {
	case KindNormal:
		return "normal"
	case KindArrow:
		return "arrow"
	case KindMethod:
		return "method"
	case KindModule:
		return "module"
	case KindScript:
		return "script"
	case KindClassCtor:
		return "class constructor"
	case KindDerivedCtor:
		return "derived constructor"
	}
	return "unknown"
}

// ConstKind tags a constant-pool entry.
type ConstKind uint8

const (
	// ConstNumber is a float64 literal (Num).
	ConstNumber ConstKind = iota
	// ConstString is a string literal (Str, IsASCII, Units).
	ConstString
	// ConstBigInt is a BigInt literal stored as text (Str): decimal, or hex
	// after "0x" (see syntax.BigIntLit).
	ConstBigInt
	// ConstRegExp is a regular-expression literal (Str = pattern, Flags).
	ConstRegExp
	// ConstTemplate is a tagged template's strings (Cooked, Raw); untagged
	// templates are lowered to Concat.
	ConstTemplate
	// ConstFunction is an index into Function.Children (Index). Reserved;
	// Closure indexes Children directly.
	ConstFunction
	// ConstNull and ConstUndefined are null and undefined, as the right
	// operand of JmpStrictEqK, JmpStrictNeK and JmpCaseK.
	ConstNull
	ConstUndefined
)

// Const is a constant-pool entry. Only the fields relevant to Kind are set.
type Const struct {
	Kind    ConstKind
	Num     float64  // ConstNumber
	Str     string   // ConstString (WTF-8), ConstBigInt (decimal or 0x hex), ConstRegExp (pattern)
	IsASCII bool     // ConstString: every byte of Str is < 0x80
	Units   []uint16 // ConstString: UTF-16 code units when !IsASCII (nil otherwise)
	Key     bool     // ConstString: used as a property name (the engine interns it)
	Flags   string   // ConstRegExp flags
	Cooked  []string // ConstTemplate cooked strings (WTF-8, or UndefinedCooked)
	Raw     []string // ConstTemplate raw strings (UTF-8)
	Index   int      // ConstFunction: index into Function.Children
}

// UndefinedCooked is the Const.Cooked entry of a tagged template chunk with
// an invalid escape, whose cooked value is undefined. The byte 0xFF never
// occurs in WTF-8.
const UndefinedCooked = "\xff"

// HandlerKind distinguishes catch and finally rows of the handler table.
type HandlerKind uint8

const (
	// HandlerCatch receives the thrown value in R[Reg].
	HandlerCatch HandlerKind = iota
	// HandlerFinally receives the completion kind (1 = throw) in R[Reg] and
	// the thrown value in R[Reg+1].
	HandlerFinally
)

// Handler is one exception-table row: [Start, End) is the protected pc range,
// Handler is the pc to resume at and StackDepth the closure-environment
// depth (number of PushEnv levels relative to function entry) to unwind to.
// Rows are ordered innermost first; the interpreter takes the first row whose
// range covers the faulting pc.
type Handler struct {
	Start      uint32
	End        uint32
	Handler    uint32
	StackDepth uint16
	Kind       HandlerKind
	Reg        uint16 // register receiving the exception (see HandlerKind)
}

// LineEntry maps the first pc of a run of instructions to a source position.
type LineEntry struct {
	PC   uint32
	Line int32
	Col  int32
}

// SourceInfo locates a function template inside its source file.
// Function.prototype.toString slices Src[Start:End].
type SourceInfo struct {
	Name  string // file name used in stack traces
	Src   string // full module/script source text
	Start int    // byte offset of the function's first character
	End   int    // byte offset one past the function's last character
}

// NoRegister marks a CaptureLayout slot that is not initialized from a
// parameter register.
const NoRegister uint16 = 0xFFFF

// CaptureLayout describes the function's own closure environment: its
// length is the number of Env slots created at function entry (zero means
// the function creates no Env and runs in its closure's environment), and
// entry i is the parameter register whose value is boxed into slot i at
// entry, or NoRegister for slots initialized to undefined.
type CaptureLayout []uint16

// Function is an immutable compiled-function template. A single *Function is
// shared by every realm that instantiates the same module, so nothing in it
// may be mutated after compilation; per-realm state (inline caches) lives in
// engine.Realm side tables keyed by *Function.
type Function struct {
	Name      string // "" for anonymous functions
	Length    int    // formal parameter count exposed as .length
	Strict    bool   // strict mode code (modules, classes, "use strict"); else sloppy
	Kind      Kind
	NumRegs   uint16 // register window size
	NumParams uint16 // declared parameters (excluding rest)
	HasRest   bool   // the rest array is collected into R[NumParams] at entry
	// HasArguments makes the entry create an unmapped arguments object in
	// the register after the parameters (and the rest array); the prologue
	// of a sloppy function with simple parameters maps it (MapArguments).
	HasArguments bool
	// ForwardArguments, with HasArguments, marks a function that is no
	// generator nor async and whose code reads the arguments register only
	// in ApplyArguments, ArgsLen and ArgsGet: the entry creates no object
	// but copies the arguments above the register window, where the frame
	// keeps them, and puts their count in the register. ApplyArguments
	// and ArgsGet create the object when a call or a key needs it.
	ForwardArguments bool
	// NewTarget is set when the function reads new.target (LoadNewTarget):
	// [[Construct]] then passes it through the realm. Class constructors
	// always receive it.
	NewTarget bool
	// Generator marks a generator function (of kind KindMethod: not
	// constructible); its body starts with GenStart.
	Generator bool
	// Async marks an async function, async generator function or module
	// with top-level await (see compiler/async.go).
	Async bool
	// ScriptOrModule is set on the root template of a script or module
	// whose code uses import() or import.meta: they need the script or
	// module record the host gave for it at run time
	// (GetActiveScriptOrModule), which a realm keeps only for such roots.
	// It is set too on the root of a script or module holding a direct
	// eval call: the code a realm compiles from a string shares the record
	// of the code that evaluates it.
	ScriptOrModule bool
	// ICCount is the number of inline-cache slots this function needs. Their
	// indices are assigned per realm on the function's first call there
	// (engine.Realm.AllocIC); the template is never mutated.
	ICCount uint32

	Code     []uint32
	Consts   []Const
	Children []*Function
	Handlers []Handler

	CaptureLayout CaptureLayout
	LineTable     []LineEntry
	Source        *SourceInfo

	// Module is the module record of a KindModule template (nil for any
	// other kind).
	Module *Module

	// Extra holds the rarely needed data of scripts, eval code and
	// functions containing a direct eval (nil for everything else, so
	// ordinary functions pay one pointer word).
	Extra *Extra

	// overlapSum is overlapSum's of the function when MarkOverlapCalls
	// last proved its CallOverlap marks, for VerifyRegisters to take them
	// without proving them again while nothing they rest on has changed;
	// 0 if it has not.
	overlapSum uint64
}

// Extra is the rarely needed part of a Function.
type Extra struct {
	// Globals lists the names a script or global eval code declares at top
	// level (nil when it declares none) for GlobalDeclarationInstantiation
	// or EvalDeclarationInstantiation, which check and create them before
	// the body runs.
	Globals *GlobalNames
	// Evals describes the scope of each direct eval call site (CallEval's
	// X operand indexes it).
	Evals []*EvalScope
}

// Module is what a module template records for the host and for linking
// (ECMA-262 §16.2.1.7 Source Text Module Records). The module Env is the
// top-level function's own Env, so every local binding, and every exported
// one, is a CaptureLayout slot.
type Module struct {
	// Exports are the local exports sorted by name.
	Exports []Export
	// BodyPC is the pc of the body after the prologue that writes the TDZ
	// markers and creates the hoisted functions: linking a graph runs the
	// prologue of every module before any body runs (InitializeEnvironment).
	BodyPC int
	// Links is nil when the module requests no other module and uses
	// neither import() nor import.meta, which then evaluates without a
	// module graph (a direct eval in its code alone sets ScriptOrModule
	// and leaves Links nil); Requests is empty when it only uses those.
	Links *Links
}

// Export is a local export: a name and the Env slot of its binding.
type Export struct {
	Name string
	Slot int
}

// Links are the module requests of a module and the entries that name
// them, each in source order. Line and Col (1-based, the column in code
// points) locate the specifier or the entry for the errors of linking.
type Links struct {
	// Requests are the specifiers, each once ([[RequestedModules]]).
	Requests []Request
	// Imports are the import bindings: each Env slot holds a reference to
	// the exporting binding (GetImport), or the namespace object when
	// Namespace is set (`import * as ns`, read with GetEnv).
	Imports []Import
	// Reexports are the indirect exports: `export {x as y} from`, `export *
	// as y from` (All), and `export {x as y}` of an import binding x.
	Reexports []Reexport
	// Stars are the requests of the `export * from` entries.
	Stars []Star
}

// Request is a module specifier.
type Request struct {
	Specifier string
	Line, Col int32
}

// Import is an import binding.
type Import struct {
	Request   int    // index into Links.Requests
	Name      string // the imported name (unset for a namespace import)
	Namespace bool
	Slot      int
	Line, Col int32
}

// Reexport is an indirect export.
type Reexport struct {
	Name      string // the exported name
	Request   int
	Import    string // the imported name (unset when All)
	All       bool
	Line, Col int32
}

// Star is an `export * from` entry.
type Star struct {
	Request   int
	Line, Col int32
}

// Export returns the Env slot of the local export name, or false.
func (m *Module) Export(name string) (int, bool) {
	i, ok := slices.BinarySearchFunc(m.Exports, name, func(e Export, name string) int { return strings.Compare(e.Name, name) })
	if !ok {
		return 0, false
	}
	return m.Exports[i].Slot, true
}

// GlobalNames are the top-level declarations of a script, each list in
// source order.
type GlobalNames struct {
	Lexical  []string // let, const and class names
	Function []string // function declaration names, each once
	Var      []string // var names that are not also function names
	// AnnexB lists the names only block-level function declarations of
	// sloppy code declare at top level (Annex B.3.2.2): each becomes a var
	// unless a global lexical binding or a non-extensible global object
	// prevents it.
	AnnexB []string
	// Eval marks the declarations of sloppy eval code, whose global
	// bindings are created configurable (EvalDeclarationInstantiation) and
	// which never declares global lexical bindings.
	Eval bool
}

// EvalScope describes the environment chain visible at a direct eval call
// site so that the compiler can resolve the eval code against it at run
// time. It is opaque to the engine, which hands it to the compile hook
// together with the source text. The call sites of a compilation share
// their EvalScopes and EvalLevels where they can: sites in one scope have
// the same EvalScope, and the chains of sites in nested scopes share their
// outer levels.
type EvalScope struct {
	// Env is the level of the running Env of the call site, nil when there
	// is none; its Outer chain are the Envs outwards: level i is the Env i
	// hops up the chain from the running Env. The script's global bindings
	// are not levels; they are resolved through the global object and the
	// global lexical environment.
	Env *EvalLevel
	// Var is the level holding the variable environment of the calling
	// function (its %evalvars object), or -1 for the global one.
	Var int
	// This is the level of the Env of the function supplying this,
	// new.target and super (the nearest non-arrow function), or -1 when
	// the call site is not inside a function.
	This int
	// Fields is the level holding the %fields binding of the class of a
	// derived constructor supplying this (super() in the eval code runs its
	// field initializers), or -1.
	Fields int
	// FuncKind is the syntax.FuncKind of that function and Derived marks a
	// derived class constructor.
	FuncKind uint8
	Derived  bool
	// Strict is the strictness of the calling code; InParams marks a call
	// in the parameter list of a function whose body has its own variable
	// environment; Module marks a call in module code.
	Strict, InParams, Module bool
}

// EvalLevel is one Env of an EvalScope: its binding names in slot order
// ("" for a slot no binding owns), with the syntax.BindKind and whether
// the binding has a temporal dead zone for each.
type EvalLevel struct {
	Kind  uint8 // syntax.ScopeKind of the scope owning the Env
	Names []string
	Kinds []uint8
	TDZ   []bool
	// Outer is the next Env up the chain, nil for the outermost.
	Outer *EvalLevel

	index atomic.Pointer[map[string]int32] // Slot's, built on first use
}

// Slot returns the slot of the binding named name, or -1 when the Env has
// none. A large level indexes its names on first use, so that eval code
// using a few of them does not pay for all: the index is a cache every
// goroutine would build the same, stored atomically, and the one thing
// written to a Function after compilation.
func (l *EvalLevel) Slot(name string) int {
	if name == "" {
		return -1
	}
	if len(l.Names) <= 8 {
		return slices.Index(l.Names, name)
	}
	idx := l.index.Load()
	if idx == nil {
		m := make(map[string]int32, len(l.Names))
		for i, n := range l.Names {
			if _, ok := m[n]; !ok && n != "" {
				m[n] = int32(i)
			}
		}
		idx = &m
		l.index.Store(idx)
	}
	if i, ok := (*idx)[name]; ok {
		return int(i)
	}
	return -1
}

// Position returns the source line and column of the instruction at pc, or
// (0, 0) when the function has no line table.
func (f *Function) Position(pc uint32) (line, col int) {
	lt := f.LineTable
	if len(lt) == 0 {
		return 0, 0
	}
	i := sort.Search(len(lt), func(i int) bool { return lt[i].PC > pc })
	if i == 0 {
		return int(lt[0].Line), int(lt[0].Col)
	}
	return int(lt[i-1].Line), int(lt[i-1].Col)
}

// DescribeCallee, when non-nil, returns the source form of the callee of the
// Call, CallSpread, New or NewSpread instruction at pc of f ("obj.foo",
// "f(...)"), or "" when it cannot tell. Package compiler installs it; the
// engine calls it only to word the TypeError of a call or construction of a
// value that is not callable, so no call site carries any extra data.
var DescribeCallee func(f *Function, pc uint32) string

// SourceText returns the function's source slice for Function.prototype.toString,
// or "" when unknown.
func (f *Function) SourceText() string {
	s := f.Source
	if s == nil || s.Start < 0 || s.End > len(s.Src) || s.Start > s.End {
		return ""
	}
	return s.Src[s.Start:s.End]
}
