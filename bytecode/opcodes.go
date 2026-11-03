package bytecode

import "math"

// Op is an opcode. Opcodes are dense so that the interpreter's switch
// compiles to a jump table. See doc.go for the semantics of every op.
type Op uint8

// Instruction formats. Every instruction is one 32-bit word:
//
//	ABC : op:8 | A:8 | B:8 | C:8
//	ABx : op:8 | A:8 | Bx:16        (Bx unsigned)
//	AsBx: op:8 | A:8 | sBx:16       (sBx signed, jump offset relative to the next pc)
//
// Ops marked "+X" in doc.go are followed by one ExtraArg word (a raw uint32)
// whose layout is given per op; a few use two.
const (
	// --- loads and moves ---
	LoadConst  Op = iota // ABx  R[A] = K[Bx]
	LoadInt              // AsBx R[A] = sBx
	LoadUndef            // A    R[A] = undefined
	LoadNull             // A    R[A] = null
	LoadTrue             // A    R[A] = true
	LoadFalse            // A    R[A] = false
	LoadHole             // A    R[A] = hole (TDZ marker)
	LoadThis             // A    R[A] = this
	LoadCallee           // A    R[A] = the running function object
	Move                 // AB   R[A] = R[B]
	UndefRange           // AB   R[A .. A+B-1] = undefined

	// --- closure environments ---
	GetEnv     // ABC  R[A] = env^B[C]
	GetEnvChk  // ABC+X R[A] = env^B[C]; ReferenceError if hole; X = name const
	SetEnv     // ABC  env^B[C] = R[A]
	GetEnvW    // AB+X  R[A] = env^B[X]
	GetEnvChkW // AB+X+X R[A] = env^B[X1]; ReferenceError if hole; X2 = name const
	SetEnvW    // AB+X  env^B[X] = R[A]
	PushEnv    // ABx  env = new Env(parent env, Bx slots)
	PopEnv     // -    env = env.parent
	CopyEnv    // -    env = shallow copy of env (per-iteration bindings)
	CheckTDZ   // A+X  ReferenceError "Cannot access 'K[X]' before initialization" if R[A] is hole

	// --- globals ---
	GetGlobal        // A+X  R[A] = global[name]; ReferenceError if absent; X = name:16 | ic:16
	GetGlobalOrUndef // A+X  R[A] = global[name] or undefined (typeof)
	SetGlobal        // A+X  global[name] = R[A]; ReferenceError if absent

	// --- properties ---
	GetProp    // AB+X  R[A] = R[B].name;          X = name:16 | ic:16
	GetLen     // AB+X  R[A] = R[B].length;        X = ic:16
	SetProp    // AB+X  R[A].name = R[B];          X = name:16 | ic:16
	GetElem    // ABC   R[A] = R[B][R[C]]
	SetElem    // ABC   R[A][R[B]] = R[C]
	DelProp    // AB+X  R[A] = delete R[B].name;   X = name const
	DelElem    // ABC   R[A] = delete R[B][R[C]]
	In         // ABC   R[A] = R[B] in R[C]
	InstanceOf // ABC  R[A] = R[B] instanceof R[C]

	// --- literals ---
	NewObject       // ABx  R[A] = {} with room for Bx properties
	NewArray        // ABx  R[A] = [] with capacity Bx
	DefineField     // AB+X R[A].name := R[B] (CreateDataProperty); X = name const
	DefineElem      // ABC  R[A][R[B]] := R[C] (CreateDataProperty)
	DefineMethod    // ABC  R[A][R[B]] := R[C], naming the function R[C] after the key (computed-key method)
	DefineAccessor  // ABC+X define R[C] as the getter or setter of R[A][R[B]]; X = Accessor* flags
	SetProto        // AB   R[A].[[Prototype]] = R[B] when object or null (__proto__: v)
	CopyDataProps   // AB   copy own enumerable properties of R[B] into R[A]
	CopyDataPropsEx // ABC  same, excluding the keys listed in array R[C]
	ArrayPush       // AB   append R[B] to array R[A]
	ArrayHole       // A    append a hole to array R[A]
	AppendSpread    // AB   append every element of iterable R[B] to array R[A]
	Closure         // ABx  R[A] = new function object from Children[Bx]
	NewRegExp       // ABx  R[A] = new RegExp(K[Bx].pattern, K[Bx].flags)
	GetTemplate     // A+X  R[A] = template object of K[X] (GetTemplateObject); X = const:16 | ic:16

	// --- arithmetic ---
	Add       // ABC  R[A] = R[B] + R[C]
	Sub       // ABC
	Mul       // ABC
	Div       // ABC
	Mod       // ABC
	Exp       // ABC  R[A] = R[B] ** R[C]
	AddImm    // ABC  R[A] = R[B] + int8(C)
	SubImm    // ABC  R[A] = R[B] - int8(C)
	Inc       // AB   R[A] = ToNumeric(R[B]) + 1
	Dec       // AB   R[A] = ToNumeric(R[B]) - 1
	Neg       // AB   R[A] = -R[B]
	Plus      // AB   R[A] = +R[B] (ToNumber)
	Not       // AB   R[A] = !R[B]
	BitNot    // AB   R[A] = ~R[B]
	ToNumeric // AB   R[A] = ToNumeric(R[B])
	// RequireObjectCoercible throws TypeError when R[A] is null or undefined
	// (destructuring through a pattern with no properties to read).
	RequireObjectCoercible // A
	ToStr                  // AB   R[A] = ToString(R[B])
	Concat                 // ABC  R[A] = R[B] concatenated with R[C] (both strings)
	Typeof                 // AB   R[A] = typeof R[B]
	TypeofIs               // ABC  R[A] = typeof R[B] === TypeName(C)
	BitAnd                 // ABC
	BitOr                  // ABC
	BitXor                 // ABC
	Shl                    // ABC
	Shr                    // ABC  signed >>
	UShr                   // ABC  >>>

	// --- comparison ---
	Eq       // ABC  R[A] = R[B] == R[C]
	Ne       // ABC
	StrictEq // ABC
	StrictNe // ABC
	Lt       // ABC
	Le       // ABC
	Gt       // ABC
	Ge       // ABC

	// --- control flow ---
	Jmp           // sBx   pc += sBx  (negative offsets are back-edges: interrupt check)
	JmpT          // AsBx  if ToBoolean(R[A]) pc += sBx
	JmpF          // AsBx  if !ToBoolean(R[A]) pc += sBx
	JmpNullish    // AsBx  if R[A] is null or undefined
	JmpNotNullish // AsBx
	JmpNotUndef   // AsBx

	// --- calls ---
	Call             // ABC  R[A] = R[A].call(this = R[A+1], R[A+2 .. A+1+B]); C = CallUndefThis: this is undefined
	CallSpread       // A    R[A] = R[A].call(this = R[A+1], ...R[A+2])
	New              // AB   R[A] = new R[A](R[A+2 .. A+1+B])
	NewSpread        // A    R[A] = new R[A](...R[A+2])
	Ret              // A    return R[A]
	RetUndef         // -    return undefined
	Throw            // A    throw R[A]
	ThrowConstAssign // +X  throw TypeError("Assignment to constant variable."); X = name const

	// --- iteration ---
	IterInit  // AB    R[A], R[A+1] = iterator state over R[B]
	IterNext  // AsBx  R[A+2] = next value of iterator R[A]; when done pc += sBx
	IterValue // AB    R[B] = next value of iterator R[A], or undefined when done
	IterRest  // AB    R[B] = array of the remaining values of iterator R[A]
	IterClose // AsBx  IteratorClose(R[A], normal); pc += sBx
	IterThrow // AB    IteratorClose(R[A], throw R[B]); throw R[B]
	ForInInit // AB    R[A] = key list of R[B], R[A+1] = 0, R[A+2] = ToObject(R[B]) (or undefined)
	ForInNext // AsBx  R[A+3] = next key still present on R[A+2]; when done pc += sBx

	// --- references (on the jump table, numbered after the ops above so
	// that code without them keeps its encoding) ---
	GetElemRef // ABC+X R[C] = R[X], converted once; R[A] = R[B][R[C]]; X = register

	// --- classes (dispatched off the interpreter's jump table) ---
	CtorEntry         // A     R[A] = new.target; TypeError when a class constructor is called
	LoadNewTarget     // A     R[A] = new.target, or undefined in a [[Call]]
	LoadHome          // A     R[A] = [[HomeObject]] of the running function
	SetHome           // AB    [[HomeObject]] of function R[A] = R[B]
	CreateClass       // ABC   make R[A] a class constructor, R[A+1] = its prototype; C&1: R[B] is the heritage, C&2: named after key R[A+1]
	DefineClassMethod // ABC   R[A][R[B]] := R[C] non-enumerable, [[HomeObject]] R[A]
	ToPropertyKey     // AB    R[A] = ToPropertyKey(R[B])
	GetProtoOf        // AB    R[A] = R[B].[[GetPrototypeOf]]() (super base, super constructor)
	GetSuper          // ABC   R[A] = R[B][R[C]] with receiver R[B+1]
	SetSuper          // ABC   R[A][R[B]] = R[C] with receiver R[A+1]
	SuperCall         // AB    R[A] = Construct(R[A], R[A+2 .. A+1+B], new.target R[A+1])
	SuperCallSpread   // A     R[A] = Construct(R[A], ...R[A+2], new.target R[A+1])
	CheckSuper        // A     ReferenceError unless R[A] (the raw `this` binding) is hole
	DerivedResult     // AB    R[A] = result of a derived constructor returning R[A] with `this` R[B]
	NewPrivateName    // ABx   R[A] = new private name described by K[Bx]
	SetPrivateMethod  // ABC+X private name R[A] names method R[B] owned by brand/class R[C]; X = Private* flags
	GetPrivate        // ABC   R[A] = R[B].#R[C]
	SetPrivate        // ABC   R[A].#R[B] = R[C]
	DefPrivate        // ABC   add private field #R[B] = R[C] to R[A]
	AddBrand          // AB    add class brand R[B] to R[A]
	InPrivate         // ABC   R[A] = #R[B] in R[C]
	ThrowError        // ABx   throw a new error of kind A (Throw* kinds) with message K[Bx]

	// --- generators (dispatched off the jump table after the class ops) ---
	GenFunc  // A     make the fresh closure R[A] a generator function
	GenStart // A     R[A] = new generator with `this` R[A]; suspends it, and the call returns it
	Yield    // ABC   generator R[B] suspends yielding R[A] (C=1: R[A] is a result object, yielded as is);
	//                 resumed with the sent value in R[A] and the mode in R[A+1]: undefined next, true throw, false return
	GenIter  // AB    R[A] = GetIterator(R[B]) (never a fast mode), R[A+1] = its next method
	Delegate // AB    one yield* step on the record R[B..B+1] with the value R[A] and mode R[A+1] (as Yield):
	//                 R[A+1] = undefined: yield the result R[A]; true: done with value R[A]; false: return R[A]

	// --- async functions and iteration (dispatched after the generator ops) ---
	AsyncFunc       // A     make the fresh closure R[A] an async function or async generator function
	AsyncStart      // A     unused: an async function body starts with a LoadThis (Await); the op keeps its number, by which the interpreter's jump table is laid out
	AsyncGenStart   // A     R[A] = new async generator with `this` R[A]; suspends it, and the call returns it
	Await           // AB    the frame awaits R[A] (R[B]: the async function call's this, then its state; or the async generator); resumed with R[A] = the result, R[A+1] = undefined (fulfilled) or true (rejected)
	AsyncReturn     // AB    resolve the async function call's promise (its state is R[B] once it awaited) with R[A]; R[A] = the promise
	AsyncThrow      // AB    reject the async function call's promise (its state is R[B] once it awaited) with R[A]; R[A] = the promise
	AsyncYield      // AB    async generator R[B] settles its head request with {value: R[A], done: false}; resumed as Yield
	AsyncIterInit   // AB    R[A] = GetIterator(R[B], async), R[A+1] = its next method
	AsyncIterNext   // A     R[A+2] = Call(R[A+1], R[A])
	AsyncIterResult // AsBx  TypeError unless R[A+2] is an object; when done pc += sBx, else R[A+2] = its value
	AsyncIterClose  // AsBx  R[A] = undefined; pc += sBx if the iterator has no return method, else R[A+2] = Call(return, iterator)
	AsyncIterClosed // A     TypeError unless the awaited return result R[A+2] is an object
	AsyncDelegate   // ABC   one async yield* step on the record R[B..B+2] with value R[A] and mode R[A+1] (C=0 call, C=1 result)

	// --- modules ---
	GetImport  // ABC+X  R[A] = the binding env^B[C] refers to; ReferenceError if hole; X = name const
	GetImportW // AB+X+X R[A] = the binding env^B[X1] refers to; ReferenceError if hole; X2 = name const
	ImportCall // AB    R[A] = import(R[B]): a promise for the namespace of the module the host loads for it
	ImportMeta // A     R[A] = import.meta of the running script or module's root

	// --- sloppy mode, script globals and with (dispatched after the async ops) ---
	SetGlobalSloppy // A+X   global[name] = R[A] in sloppy mode: creates an unresolvable name, a rejected write is ignored; X = name:16 | ic:16
	ResolveGlobal   // A+X   R[A] = whether the global environment has a binding name (strict, before the value of an assignment); X = name:16 | ic:16
	SetGlobalRef    // AB+X  global binding name = R[A] (strict) when R[B], ResolveGlobal's result, else ReferenceError; X = name:16 | ic:16 (ResolveGlobal's)
	InitGlobal      // A+X   initialize the script's global lexical binding name to R[A]; X = name:16 | const<<16
	SetGlobalVar    // A+X   Annex B.3.2.2: global[name] = R[A] (sloppy) unless a global lexical binding name exists; X = name const
	DelGlobal       // A+X   R[A] = delete of the global binding name (false for a global lexical); X = name const
	SetPropSloppy   // AB+X  R[A].name = R[B], a rejected assignment ignored; X = name:16 | ic:16
	SetElemSloppy   // ABC   R[A][R[B]] = R[C], a rejected assignment ignored
	DelPropSloppy   // AB+X  R[A] = delete R[B].name, false when rejected; X = name const
	DelElemSloppy   // ABC   R[A] = delete R[B][R[C]], false when rejected
	SetSuperSloppy  // ABC   R[A][R[B]] = R[C] with receiver R[A+1], a rejected assignment ignored
	ToObject        // AB    R[A] = ToObject(R[B]) (the with statement's object, DefineField's instance in initializers and eval code)
	JmpWith         // AsBx+X pc += sBx if the with object R[A] has a binding name (HasProperty, not @@unscopables-blocked); X = name const
	WithGet         // AB+X  R[A] = binding name of the with object R[B]; X = name:16 | strict<<16
	WithSet         // AB+X  set binding name of the with object R[A] to R[B]; X = name:16 | strict<<16
	CoerceThis      // -     this = the global object if undefined or null, else ToObject(this) (sloppy function entry)
	MapArguments    // A     make the arguments object R[A] mapped to the parameters in the function's own Env
	CallEval        // ABC+X R[A] = call of callee R[A] with this R[A+1] and args R[A+2..] (B = argc, C = 1: one spread array), a direct eval when the callee is %eval%; X = index into Extra.Evals

	// --- compare and branch (a comparison whose result only feeds a
	// conditional jump; numbered last so that code without them keeps its
	// encoding). The offset is relative to the X word. ---
	JmpLt       // AsBx+X if R[A] < R[X] pc += sBx
	JmpLe       // AsBx+X if R[A] <= R[X] pc += sBx
	JmpGt       // AsBx+X if R[A] > R[X] pc += sBx
	JmpGe       // AsBx+X if R[A] >= R[X] pc += sBx
	JmpNLt      // AsBx+X if !(R[A] < R[X]) pc += sBx
	JmpNLe      // AsBx+X if !(R[A] <= R[X]) pc += sBx
	JmpNGt      // AsBx+X if !(R[A] > R[X]) pc += sBx
	JmpNGe      // AsBx+X if !(R[A] >= R[X]) pc += sBx
	JmpStrictEq // AsBx+X if R[A] === R[X] pc += sBx
	JmpStrictNe // AsBx+X if R[A] !== R[X] pc += sBx

	// --- compare and branch against an integer: JmpLt .. JmpStrictNe with
	// the right operand int32(X), in the same order ---
	JmpLtI       // AsBx+X if R[A] < int32(X) pc += sBx
	JmpLeI       // AsBx+X if R[A] <= int32(X) pc += sBx
	JmpGtI       // AsBx+X if R[A] > int32(X) pc += sBx
	JmpGeI       // AsBx+X if R[A] >= int32(X) pc += sBx
	JmpNLtI      // AsBx+X if !(R[A] < int32(X)) pc += sBx
	JmpNLeI      // AsBx+X if !(R[A] <= int32(X)) pc += sBx
	JmpNGtI      // AsBx+X if !(R[A] > int32(X)) pc += sBx
	JmpNGeI      // AsBx+X if !(R[A] >= int32(X)) pc += sBx
	JmpStrictEqI // AsBx+X if R[A] === int32(X) pc += sBx
	JmpStrictNeI // AsBx+X if R[A] !== int32(X) pc += sBx

	opCount
)

// OpCount is the number of defined opcodes.
const OpCount = int(opCount)

// Format describes how an instruction's operands are laid out.
type Format uint8

const (
	FmtNone Format = iota // no operands
	FmtA                  // A
	FmtAB                 // A B
	FmtABC                // A B C
	FmtABx                // A Bx
	FmtAsBx               // A sBx
	FmtSBx                // sBx
)

// opInfo is the static description of an opcode.
type opInfo struct {
	name  string
	fmt   Format
	extra uint8 // number of ExtraArg words following the instruction
}

var opTable = [...]opInfo{

	LoadConst:  {"LoadConst", FmtABx, 0},
	LoadInt:    {"LoadInt", FmtAsBx, 0},
	LoadUndef:  {"LoadUndef", FmtA, 0},
	LoadNull:   {"LoadNull", FmtA, 0},
	LoadTrue:   {"LoadTrue", FmtA, 0},
	LoadFalse:  {"LoadFalse", FmtA, 0},
	LoadHole:   {"LoadHole", FmtA, 0},
	LoadThis:   {"LoadThis", FmtA, 0},
	LoadCallee: {"LoadCallee", FmtA, 0},
	Move:       {"Move", FmtAB, 0},
	UndefRange: {"UndefRange", FmtAB, 0},

	GetEnv:     {"GetEnv", FmtABC, 0},
	GetEnvChk:  {"GetEnvChk", FmtABC, 1},
	SetEnv:     {"SetEnv", FmtABC, 0},
	GetEnvW:    {"GetEnvW", FmtAB, 1},
	GetEnvChkW: {"GetEnvChkW", FmtAB, 2},
	SetEnvW:    {"SetEnvW", FmtAB, 1},
	PushEnv:    {"PushEnv", FmtABx, 0},
	PopEnv:     {"PopEnv", FmtNone, 0},
	CopyEnv:    {"CopyEnv", FmtNone, 0},
	CheckTDZ:   {"CheckTDZ", FmtA, 1},

	GetGlobal:        {"GetGlobal", FmtA, 1},
	GetGlobalOrUndef: {"GetGlobalOrUndef", FmtA, 1},
	SetGlobal:        {"SetGlobal", FmtA, 1},

	GetProp:    {"GetProp", FmtAB, 1},
	GetLen:     {"GetLen", FmtAB, 1},
	SetProp:    {"SetProp", FmtAB, 1},
	GetElem:    {"GetElem", FmtABC, 0},
	SetElem:    {"SetElem", FmtABC, 0},
	DelProp:    {"DelProp", FmtAB, 1},
	DelElem:    {"DelElem", FmtABC, 0},
	In:         {"In", FmtABC, 0},
	InstanceOf: {"InstanceOf", FmtABC, 0},

	NewObject:       {"NewObject", FmtABx, 0},
	NewArray:        {"NewArray", FmtABx, 0},
	DefineField:     {"DefineField", FmtAB, 1},
	DefineElem:      {"DefineElem", FmtABC, 0},
	DefineMethod:    {"DefineMethod", FmtABC, 0},
	DefineAccessor:  {"DefineAccessor", FmtABC, 1},
	SetProto:        {"SetProto", FmtAB, 0},
	CopyDataProps:   {"CopyDataProps", FmtAB, 0},
	CopyDataPropsEx: {"CopyDataPropsEx", FmtABC, 0},
	ArrayPush:       {"ArrayPush", FmtAB, 0},
	ArrayHole:       {"ArrayHole", FmtA, 0},
	AppendSpread:    {"AppendSpread", FmtAB, 0},
	Closure:         {"Closure", FmtABx, 0},
	NewRegExp:       {"NewRegExp", FmtABx, 0},
	GetTemplate:     {"GetTemplate", FmtA, 1},

	Add:       {"Add", FmtABC, 0},
	Sub:       {"Sub", FmtABC, 0},
	Mul:       {"Mul", FmtABC, 0},
	Div:       {"Div", FmtABC, 0},
	Mod:       {"Mod", FmtABC, 0},
	Exp:       {"Exp", FmtABC, 0},
	AddImm:    {"AddImm", FmtABC, 0},
	SubImm:    {"SubImm", FmtABC, 0},
	Inc:       {"Inc", FmtAB, 0},
	Dec:       {"Dec", FmtAB, 0},
	Neg:       {"Neg", FmtAB, 0},
	Plus:      {"Plus", FmtAB, 0},
	Not:       {"Not", FmtAB, 0},
	BitNot:    {"BitNot", FmtAB, 0},
	ToNumeric: {"ToNumeric", FmtAB, 0},

	RequireObjectCoercible: {"RequireObjectCoercible", FmtA, 0},
	ToStr:                  {"ToStr", FmtAB, 0},
	Concat:                 {"Concat", FmtABC, 0},
	Typeof:                 {"Typeof", FmtAB, 0},
	TypeofIs:               {"TypeofIs", FmtABC, 0},
	BitAnd:                 {"BitAnd", FmtABC, 0},
	BitOr:                  {"BitOr", FmtABC, 0},
	BitXor:                 {"BitXor", FmtABC, 0},
	Shl:                    {"Shl", FmtABC, 0},
	Shr:                    {"Shr", FmtABC, 0},
	UShr:                   {"UShr", FmtABC, 0},

	Eq:       {"Eq", FmtABC, 0},
	Ne:       {"Ne", FmtABC, 0},
	StrictEq: {"StrictEq", FmtABC, 0},
	StrictNe: {"StrictNe", FmtABC, 0},
	Lt:       {"Lt", FmtABC, 0},
	Le:       {"Le", FmtABC, 0},
	Gt:       {"Gt", FmtABC, 0},
	Ge:       {"Ge", FmtABC, 0},

	Jmp:           {"Jmp", FmtSBx, 0},
	JmpT:          {"JmpT", FmtAsBx, 0},
	JmpF:          {"JmpF", FmtAsBx, 0},
	JmpNullish:    {"JmpNullish", FmtAsBx, 0},
	JmpNotNullish: {"JmpNotNullish", FmtAsBx, 0},
	JmpNotUndef:   {"JmpNotUndef", FmtAsBx, 0},

	Call:             {"Call", FmtAB, 0},
	CallSpread:       {"CallSpread", FmtA, 0},
	New:              {"New", FmtAB, 0},
	NewSpread:        {"NewSpread", FmtA, 0},
	Ret:              {"Ret", FmtA, 0},
	RetUndef:         {"RetUndef", FmtNone, 0},
	Throw:            {"Throw", FmtA, 0},
	ThrowConstAssign: {"ThrowConstAssign", FmtNone, 1},

	IterInit:  {"IterInit", FmtAB, 0},
	IterNext:  {"IterNext", FmtAsBx, 0},
	IterValue: {"IterValue", FmtAB, 0},
	IterRest:  {"IterRest", FmtAB, 0},
	IterClose: {"IterClose", FmtAsBx, 0},
	IterThrow: {"IterThrow", FmtAB, 0},
	ForInInit: {"ForInInit", FmtAB, 0},
	ForInNext: {"ForInNext", FmtAsBx, 0},

	GetElemRef: {"GetElemRef", FmtABC, 1},

	CtorEntry:         {"CtorEntry", FmtA, 0},
	LoadNewTarget:     {"LoadNewTarget", FmtA, 0},
	LoadHome:          {"LoadHome", FmtA, 0},
	SetHome:           {"SetHome", FmtAB, 0},
	CreateClass:       {"CreateClass", FmtABC, 0},
	DefineClassMethod: {"DefineClassMethod", FmtABC, 0},
	ToPropertyKey:     {"ToPropertyKey", FmtAB, 0},
	GetProtoOf:        {"GetProtoOf", FmtAB, 0},
	GetSuper:          {"GetSuper", FmtABC, 0},
	SetSuper:          {"SetSuper", FmtABC, 0},
	SuperCall:         {"SuperCall", FmtAB, 0},
	SuperCallSpread:   {"SuperCallSpread", FmtA, 0},
	CheckSuper:        {"CheckSuper", FmtA, 0},
	DerivedResult:     {"DerivedResult", FmtAB, 0},
	NewPrivateName:    {"NewPrivateName", FmtABx, 0},
	SetPrivateMethod:  {"SetPrivateMethod", FmtABC, 1},
	GetPrivate:        {"GetPrivate", FmtABC, 0},
	SetPrivate:        {"SetPrivate", FmtABC, 0},
	DefPrivate:        {"DefPrivate", FmtABC, 0},
	AddBrand:          {"AddBrand", FmtAB, 0},
	InPrivate:         {"InPrivate", FmtABC, 0},
	ThrowError:        {"ThrowError", FmtABx, 0},

	GenFunc:  {"GenFunc", FmtA, 0},
	GenStart: {"GenStart", FmtA, 0},
	Yield:    {"Yield", FmtABC, 0},
	GenIter:  {"GenIter", FmtAB, 0},
	Delegate: {"Delegate", FmtAB, 0},

	AsyncFunc:       {"AsyncFunc", FmtA, 0},
	AsyncStart:      {"AsyncStart", FmtA, 0},
	AsyncGenStart:   {"AsyncGenStart", FmtA, 0},
	Await:           {"Await", FmtAB, 0},
	AsyncReturn:     {"AsyncReturn", FmtAB, 0},
	AsyncThrow:      {"AsyncThrow", FmtAB, 0},
	AsyncYield:      {"AsyncYield", FmtAB, 0},
	AsyncIterInit:   {"AsyncIterInit", FmtAB, 0},
	AsyncIterNext:   {"AsyncIterNext", FmtA, 0},
	AsyncIterResult: {"AsyncIterResult", FmtAsBx, 0},
	AsyncIterClose:  {"AsyncIterClose", FmtAsBx, 0},
	AsyncIterClosed: {"AsyncIterClosed", FmtA, 0},
	AsyncDelegate:   {"AsyncDelegate", FmtABC, 0},

	GetImport:  {"GetImport", FmtABC, 1},
	GetImportW: {"GetImportW", FmtAB, 2},
	ImportCall: {"ImportCall", FmtAB, 0},
	ImportMeta: {"ImportMeta", FmtA, 0},

	SetGlobalSloppy: {"SetGlobalSloppy", FmtA, 1},
	ResolveGlobal:   {"ResolveGlobal", FmtA, 1},
	SetGlobalRef:    {"SetGlobalRef", FmtAB, 1},
	InitGlobal:      {"InitGlobal", FmtA, 1},
	SetGlobalVar:    {"SetGlobalVar", FmtA, 1},
	DelGlobal:       {"DelGlobal", FmtA, 1},
	SetPropSloppy:   {"SetPropSloppy", FmtAB, 1},
	SetElemSloppy:   {"SetElemSloppy", FmtABC, 0},
	DelPropSloppy:   {"DelPropSloppy", FmtAB, 1},
	DelElemSloppy:   {"DelElemSloppy", FmtABC, 0},
	SetSuperSloppy:  {"SetSuperSloppy", FmtABC, 0},
	ToObject:        {"ToObject", FmtAB, 0},
	JmpWith:         {"JmpWith", FmtAsBx, 1},
	WithGet:         {"WithGet", FmtAB, 1},
	WithSet:         {"WithSet", FmtAB, 1},
	CoerceThis:      {"CoerceThis", FmtNone, 0},
	MapArguments:    {"MapArguments", FmtA, 0},
	CallEval:        {"CallEval", FmtABC, 1},

	JmpLt:       {"JmpLt", FmtAsBx, 1},
	JmpLe:       {"JmpLe", FmtAsBx, 1},
	JmpGt:       {"JmpGt", FmtAsBx, 1},
	JmpGe:       {"JmpGe", FmtAsBx, 1},
	JmpNLt:      {"JmpNLt", FmtAsBx, 1},
	JmpNLe:      {"JmpNLe", FmtAsBx, 1},
	JmpNGt:      {"JmpNGt", FmtAsBx, 1},
	JmpNGe:      {"JmpNGe", FmtAsBx, 1},
	JmpStrictEq: {"JmpStrictEq", FmtAsBx, 1},
	JmpStrictNe: {"JmpStrictNe", FmtAsBx, 1},

	JmpLtI:       {"JmpLtI", FmtAsBx, 1},
	JmpLeI:       {"JmpLeI", FmtAsBx, 1},
	JmpGtI:       {"JmpGtI", FmtAsBx, 1},
	JmpGeI:       {"JmpGeI", FmtAsBx, 1},
	JmpNLtI:      {"JmpNLtI", FmtAsBx, 1},
	JmpNLeI:      {"JmpNLeI", FmtAsBx, 1},
	JmpNGtI:      {"JmpNGtI", FmtAsBx, 1},
	JmpNGeI:      {"JmpNGeI", FmtAsBx, 1},
	JmpStrictEqI: {"JmpStrictEqI", FmtAsBx, 1},
	JmpStrictNeI: {"JmpStrictNeI", FmtAsBx, 1},
}

// CompareJump returns the op that jumps when the comparison op (Lt, Le, Gt,
// Ge, StrictEq or StrictNe) of R[A] and R[X] is jumpIf, and false for any
// other op.
func CompareJump(op Op, jumpIf bool) (Op, bool) {
	switch op {
	case Lt, Le, Gt, Ge:
		if jumpIf {
			return JmpLt + (op - Lt), true
		}
		return JmpNLt + (op - Lt), true
	case StrictEq, StrictNe:
		if (op == StrictEq) == jumpIf {
			return JmpStrictEq, true
		}
		return JmpStrictNe, true
	}
	return 0, false
}

// Immediate returns the form of the compare-and-branch op (JmpLt ..
// JmpStrictNe) whose right operand is the integer in the X word instead of
// a register.
func (op Op) Immediate() Op { return op - JmpLt + JmpLtI }

// ImmediateOperand reports whether n can be the X word of an immediate
// compare-and-branch op: an int32 other than -0.
func ImmediateOperand(n float64) (int32, bool) {
	i := int32(n)
	return i, float64(i) == n && !(n == 0 && math.Signbit(n))
}

// String returns the mnemonic.
func (op Op) String() string {
	if int(op) < len(opTable) && opTable[op].name != "" {
		return opTable[op].name
	}
	return "Op(" + itoa(int(op)) + ")"
}

// Format returns the operand layout of op.
func (op Op) Format() Format { return opTable[op].fmt }

// ExtraWords returns how many ExtraArg words follow an instruction of op.
func (op Op) ExtraWords() int { return int(opTable[op].extra) }

// Jumps reports whether an instruction of op may continue at the pc its
// sBx operand names (JumpTarget) instead of the next instruction: Jmp, the
// conditional and compare-and-branch jumps, and the iteration and with ops
// that branch (IterNext, IterClose, ForInNext, AsyncIterResult,
// AsyncIterClose, JmpWith). LoadInt's sBx is a value.
func (op Op) Jumps() bool {
	f := op.Format()
	return f == FmtSBx || f == FmtAsBx && op != LoadInt
}

// JumpTarget returns the pc that the jump w at pc names: its sBx counts
// from the word after the instruction's first, the X word of a
// compare-and-branch or JmpWith included.
func JumpTarget(pc int, w uint32) int { return pc + 1 + int(DecodeSBx(w)) }

// SavesPC reports whether the engine may run a frame again from the
// instruction after one of op: a suspension saves that pc to resume at
// (GenStart, Yield, AsyncGenStart, Await, AsyncYield), and CoerceThis,
// MapArguments and CallEval may rerun the frame from it. The engine stores
// it with the frame's environment depth above PCBits.
func (op Op) SavesPC() bool {
	switch op {
	case GenStart, Yield, AsyncGenStart, Await, AsyncYield, CoerceThis, MapArguments, CallEval:
		return true
	}
	return false
}

// TypeName indices used by TypeofIs (operand C).
const (
	TypeUndefined uint8 = iota
	TypeObject
	TypeBoolean
	TypeNumber
	TypeString
	TypeSymbol
	TypeBigInt
	TypeFunction
	typeNameCount
)

// TypeNames spells the TypeofIs operand.
var TypeNames = [typeNameCount]string{"undefined", "object", "boolean", "number", "string", "symbol", "bigint", "function"}

// TypeNameIndex returns the TypeofIs operand for a typeof result string.
func TypeNameIndex(name string) (uint8, bool) {
	for i, n := range TypeNames {
		if n == name {
			return uint8(i), true
		}
	}
	return 0, false
}

// DefineAccessor flags (its ExtraArg word). Object literals set
// AccessorEnumerable; class accessors do not.
const (
	AccessorSetter     uint32 = 1 << iota // define the setter half (otherwise the getter)
	AccessorEnumerable                    // the property is enumerable
	AccessorNameKey                       // name the function "get <key>"/"set <key>" at run time (computed keys)
)

// SetPrivateMethod flags (its ExtraArg word).
const (
	PrivateGetter uint32 = 1 << iota // the function is the getter of an accessor
	PrivateSetter                    // the function is the setter of an accessor
	PrivateStatic                    // checked against the class constructor instead of a brand
)

// CallUndefThis is the Call operand C of a call whose this is undefined (a
// plain call f(x)): the callee gets undefined and R[A+1] is not read, so no
// LoadUndef fills it.
const CallUndefThis = 1

// ThrowError kinds (operand A).
const (
	ThrowTypeError uint8 = iota
	ThrowReferenceError
)

// --- encoding --------------------------------------------------------------------

// Instruction word layout constants.
const (
	MaxRegister = 255    // registers are 8-bit operands
	MaxBx       = 0xFFFF // unsigned 16-bit operand
	MaxSBx      = 0x7FFF
	MinSBx      = -0x8000
)

// EncodeABC packs an ABC instruction.
func EncodeABC(op Op, a, b, c uint8) uint32 {
	return uint32(op) | uint32(a)<<8 | uint32(b)<<16 | uint32(c)<<24
}

// EncodeABx packs an ABx instruction.
func EncodeABx(op Op, a uint8, bx uint16) uint32 {
	return uint32(op) | uint32(a)<<8 | uint32(bx)<<16
}

// EncodeAsBx packs an AsBx instruction.
func EncodeAsBx(op Op, a uint8, sbx int16) uint32 {
	return uint32(op) | uint32(a)<<8 | uint32(uint16(sbx))<<16
}

// DecodeOp extracts the opcode.
func DecodeOp(w uint32) Op { return Op(w) }

// DecodeA extracts operand A.
func DecodeA(w uint32) uint8 { return uint8(w >> 8) }

// DecodeB extracts operand B.
func DecodeB(w uint32) uint8 { return uint8(w >> 16) }

// DecodeC extracts operand C.
func DecodeC(w uint32) uint8 { return uint8(w >> 24) }

// DecodeBx extracts the unsigned 16-bit operand.
func DecodeBx(w uint32) uint16 { return uint16(w >> 16) }

// DecodeSBx extracts the signed 16-bit operand.
func DecodeSBx(w uint32) int16 { return int16(w >> 16) }

// ExtraArg packs two 16-bit fields (used for name:16 | ic:16).
func ExtraArg(lo, hi uint16) uint32 { return uint32(lo) | uint32(hi)<<16 }

// ExtraLo extracts the low half of an ExtraArg word.
func ExtraLo(x uint32) uint16 { return uint16(x) }

// ExtraHi extracts the high half of an ExtraArg word.
func ExtraHi(x uint32) uint16 { return uint16(x >> 16) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
