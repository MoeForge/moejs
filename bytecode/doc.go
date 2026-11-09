// Package bytecode: instruction set reference.
//
// # Machine model
//
// moejs runs a register machine. A function executes in a window of
// NumRegs registers R[0..NumRegs-1] carved out of one contiguous per-realm
// value stack. Arguments arrive in R[0..argc-1]; missing parameters are
// undefined; when HasRest is set the surplus arguments are collected into an
// array in R[NumParams] before any other instruction runs, and when
// HasArguments is set an unmapped arguments object holding every argument
// is created in the next register (R[NumParams], or R[NumParams+1] after a
// rest array). `this` is held in the frame, not in a register. Registers
// above the parameters hold locals (un-captured let/const/var/function
// bindings) and expression temporaries; the compiler allocates them
// stack-like so that a call block (callee, this, args) is always the
// highest live group of registers.
//
// Captured bindings never live in registers: they live in closure
// environments (Env). env^0 is the innermost environment; env^d is its d-th
// parent. A function whose CaptureLayout has n > 0 slots creates its own Env
// of n slots at entry (with the parameter registers listed in the layout
// copied in); otherwise it runs directly in the environment captured by its
// closure. Block scopes with captured bindings push and pop nested Envs.
//
// Every instruction is one 32-bit word: op:8 | A:8 | B:8 | C:8, or
// op:8 | A:8 | Bx:16 (unsigned) / sBx:16 (signed). Ops marked +X below are
// followed by one ExtraArg word (a raw uint32); GetEnvChkW and GetImportW
// have two. Jump
// offsets are relative to the pc of the following word. Constants K[i] are
// Function.Consts; F[i] are Function.Children.
//
// Exceptions unwind through Function.Handlers without panic/recover: the
// interpreter takes the first row whose [Start, End) covers the faulting pc,
// pops closure environments down to StackDepth, stores the thrown value as
// described by Kind, and resumes at Handler. Rows without a match propagate
// the error to the caller's frame. Interrupts (Realm.Interrupt) are checked
// at function entry and at every backward jump (Jmp, and the relational
// compare-and-branch ops JmpLt .. JmpNGe and JmpLtI .. JmpNGeI, which close
// rotated loops) and are not catchable.
//
// # Opcodes
//
// Loads and moves
//
//	LoadConst  A Bx        R[A] = K[Bx] (number, string or bigint constant)
//	LoadInt    A sBx       R[A] = sBx (small integer)
//	LoadUndef  A           R[A] = undefined
//	LoadNull   A           R[A] = null
//	LoadTrue   A           R[A] = true
//	LoadFalse  A           R[A] = false
//	LoadHole   A           R[A] = hole (the TDZ marker; never observable)
//	LoadThis   A           R[A] = this (undefined at module top level; lexical in arrows)
//	LoadCallee A           R[A] = the function object being executed (named function expressions)
//	Move       A B         R[A] = R[B]
//	UndefRange A B         R[A .. A+B-1] = undefined (var bindings at entry)
//
// Closure environments
//
//	GetEnv     A B C       R[A] = env^B[C]
//	GetEnv0    A B C       GetEnv with B = 0 (the innermost environment)
//	GetEnv1    A B C       GetEnv with B = 1 (the enclosing environment)
//	GetEnv2    A B C       GetEnv with B = 2 (the one around it)
//	GetEnvChk  A B C +X    R[A] = env^B[C]; ReferenceError "K[X] is not defined" if hole
//	SetEnv     A B C       env^B[C] = R[A]
//	GetEnvW    A B +X      R[A] = env^B[X]              (slot >= 256)
//	GetEnvChkW A B +X +X   R[A] = env^B[X1] with TDZ check; X2 = name constant
//	SetEnvW    A B +X      env^B[X] = R[A]
//	PushEnv    Bx          env = new Env(env, Bx slots initialized to undefined)
//	PopEnv                 env = env.parent
//	CopyEnv                env = copy of env with the same parent (per-iteration let bindings)
//	CheckTDZ   A +X        ReferenceError "Cannot access 'K[X]' before initialization" if R[A] is hole
//
// Module imports (env^B[C] holds a reference to the exporting module's
// binding, filled in when the module graph is instantiated)
//
//	GetImport  A B C +X    R[A] = the binding env^B[C] refers to; ReferenceError if hole; X = name constant
//	GetImportW A B +X +X   same with slot X1 (slot >= 256); X2 = name constant
//
// Dynamic import and import.meta (off the interpreter's jump table, after
// the async ops; the compiler sets Function.ScriptOrModule on the root of
// code that uses them, and of a script or module holding a direct eval)
//
//	ImportCall A B         R[A] = a new promise for import(R[B]): ToString(R[B]), then the host's module for
//	                       it in the running root's script or module, linked and evaluated in the realm's module
//	                       map, fulfils it with the module's namespace; any failure rejects it
//	ImportMeta A           R[A] = import.meta of the running root module: a null-prototype object created, and
//	                       filled by the host, on the first read in the realm
//
// Globals (X = name constant:16 | inline cache:16)
//
//	GetGlobal        A +X  R[A] = globalThis[name]; ReferenceError if absent
//	GetGlobalOrUndef A +X  R[A] = globalThis[name] or undefined (typeof operand)
//	SetGlobal        A +X  globalThis[name] = R[A]; ReferenceError if absent in a strict function, else
//	                       a new global object property and a rejected write ignored (SetGlobalSloppy)
//
// A global name resolves first in the realm's global declarative
// environment (the let, const and class declarations of scripts), then on
// the global object. Scripts reach their own top-level declarations by name
// too: GlobalDeclarationInstantiation (Realm.RunScript) creates them from
// Function.Extra.Globals before the body runs.
//
// Properties
//
//	GetProp    A B +X      R[A] = R[B].name (X = name:16 | ic:16; TypeError on null/undefined base)
//	GetLen     A B +X      R[A] = R[B].length (array and string fast paths; X = ic in the high half)
//	SetProp    A B +X      R[A].name = R[B] (strict PutValue); C = SetPropSloppyFlag: SetPropSloppy's,
//	                       the form the compiler emits for sloppy code
//	GetElem    A B C       R[A] = R[B][R[C]] (dense array and string index fast paths)
//	GetElemRef A B C +X    R[C] = the key R[X], converted by ToPropertyKey when it is an object and
//	                       the base R[B] is neither undefined nor null; then GetElem A B C: the read of a
//	                       compound assignment or update, whose key GetValue converts once, after ToObject
//	                       of the base, and PutValue reuses (X is a register, C or another)
//	GetElemInc A B C       R[C] = ToNumeric(R[C]) + 1, then R[A] = R[B][R[C]]: Inc C C; GetElem A B C, the
//	                       read x[++k] of a register local k
//	SetElem    A B C       R[A][R[B]] = R[C]: PutValue, strict or not as the function is
//	DelProp    A B +X      R[A] = delete R[B].name (strict: TypeError when not configurable)
//	DelElem    A B C       R[A] = delete R[B][R[C]]
//	In         A B C       R[A] = R[B] in R[C]
//	InstanceOf A B C       R[A] = R[B] instanceof R[C]
//
// Literals
//
//	NewObject       A Bx   R[A] = {} (ordinary object, Bx = property count hint)
//	NewArray        A Bx   R[A] = [] (Bx = capacity hint)
//	DefineField     A B +X CreateDataProperty(R[A], K[name], R[B]) (X = name:16 | ic:16; the IC caches the shape transition)
//	DefineElem      A B C  CreateDataProperty(R[A], ToPropertyKey(R[B]), R[C])
//	DefineMethod    A B C  DefineElem for an object-literal method with a computed key: SetFunctionName(R[C], key) first
//	DefineAccessor  A B C +X  DefinePropertyOrThrow(R[A], ToPropertyKey(R[B]), {[[Get]] or [[Set]]: R[C], [[Enumerable]], [[Configurable]]: true}),
//	                       merging with an existing accessor half; X = AccessorSetter | AccessorEnumerable | AccessorNameKey
//	                       (AccessorNameKey: SetFunctionName(R[C], key, "get"/"set") first, for computed keys)
//	SetProto        A B    if R[B] is an object or null, R[A].[[Prototype]] = R[B] (the __proto__: v literal form)
//	CopyDataProps   A B    copy own enumerable properties of R[B] into R[A] (object spread)
//	CopyDataPropsEx A B C  same, skipping the keys listed in array R[C] (object rest pattern)
//	ArrayPush       A B    append R[B] to array R[A]
//	ArrayPushK      A +X   append the constants K[X.lo] .. K[X.lo+X.hi-1] (numbers, strings, null or
//	                       undefined, X.hi >= 1) to array R[A]: a run of an array literal's elements
//	NewArrayK       A +X   R[A] = a new array of the constants K[X.lo] .. K[X.lo+X.hi-1] (as ArrayPushK's),
//	                       an array literal whose elements are all constants
//	NewObjectLit    A B C +X  R[A] = a new ordinary object with the data properties K[X.lo] .. K[X.lo+C-1]
//	                       (property names, C >= 1) of the values R[B] .. R[B+C-1], in that order, as NewObject
//	                       and a DefineField of each would make it; X.hi is the inline cache that keeps the
//	                       object's shape, which no other instruction of the function names
//	ArrayHole       A      append a hole to array R[A] (length += 1)
//	AppendSpread    A B    append every value produced by iterating R[B] to array R[A]
//	Closure         A Bx   R[A] = new function object over F[Bx] closing over env (and this for arrows)
//	NewRegExp       A Bx   R[A] = new RegExp(K[Bx].pattern, K[Bx].flags)
//	RegExpTest      A sBx +X R[A] = /K[X lo]/.test(R[X hi]) and pc += sBx: the call /lit/.test(x) of a literal
//	                       neither global nor sticky with x a local, when x is a string and the test and exec of
//	                       %RegExp.prototype% are the originals, so that no code could see the literal's object and
//	                       the match needs none. Else it falls through to the call, which it jumps over
//	GetTemplate     A +X   R[A] = the template object of K[X lo] (GetTemplateObject: frozen cooked strings with a frozen raw array), created on the site's first evaluation in the realm and cached in IC slot X hi
//
// Arithmetic (number fast paths; BigInt operands take the slow path)
//
//	Add       A B C        R[A] = R[B] + R[C] (numbers, string concatenation, ToPrimitive)
//	Sub       A B C        R[A] = R[B] - R[C]
//	Mul       A B C        R[A] = R[B] * R[C]
//	Div       A B C        R[A] = R[B] / R[C]
//	Mod       A B C        R[A] = R[B] % R[C]
//	Exp       A B C        R[A] = R[B] ** R[C]
//	AddImm    A B C        R[A] = R[B] + int8(C) (Add semantics: strings concatenate)
//	SubImm    A B C        R[A] = R[B] - int8(C)
//	Inc       A B          R[A] = ToNumeric(R[B]) + 1 (++; never concatenates)
//	Dec       A B          R[A] = ToNumeric(R[B]) - 1 (--)
//	Neg       A B          R[A] = -R[B]
//	Plus      A B          R[A] = +R[B] (ToNumber)
//	Not       A B          R[A] = !ToBoolean(R[B])
//	BitNot    A B          R[A] = ~R[B]
//	ToNumeric A B          R[A] = ToNumeric(R[B]) (postfix update result)
//	PostInc   A B          R[A] = ToNumeric(R[B]), then R[B] = R[A] + 1: ToNumeric A B; Inc B A, a postfix ++
//	                       whose value is used
//	PostDec   A B          R[A] = ToNumeric(R[B]), then R[B] = R[A] - 1: ToNumeric A B; Dec B A
//	RequireObjectCoercible A  throw TypeError when R[A] is null or undefined (empty or rest-only destructuring pattern)
//	ToStr     A B          R[A] = ToString(R[B]) (template substitutions)
//	Concat    A B C        R[A] = R[B] + R[C], both already strings (rope above the threshold)
//	Typeof    A B          R[A] = typeof R[B]
//	TypeofIs  A B C        R[A] = typeof R[B] === TypeNames[C]
//	BitAnd    A B C        R[A] = R[B] & R[C]
//	BitOr     A B C        R[A] = R[B] | R[C]
//	BitXor    A B C        R[A] = R[B] ^ R[C]
//	Shl       A B C        R[A] = R[B] << R[C]
//	Shr       A B C        R[A] = R[B] >> R[C]
//	UShr      A B C        R[A] = R[B] >>> R[C]
//
// Comparison
//
//	Eq       A B C         R[A] = R[B] == R[C]
//	Ne       A B C         R[A] = R[B] != R[C]
//	StrictEq A B C         R[A] = R[B] === R[C]
//	StrictNe A B C         R[A] = R[B] !== R[C]
//	Lt       A B C         R[A] = R[B] < R[C]
//	Le       A B C         R[A] = R[B] <= R[C]
//	Gt       A B C         R[A] = R[B] > R[C]
//	Ge       A B C         R[A] = R[B] >= R[C]
//
// Control flow (offsets relative to the next pc)
//
//	Jmp           sBx      pc += sBx; a negative offset is a loop back-edge and checks the interrupt flag
//	                       (so does a negative offset of the relational compare-and-branch ops below,
//	                       jumping or not: a for, while or do-while loop whose condition is a relational
//	                       comparison ends in its jump)
//	JmpT          A sBx    if ToBoolean(R[A]) pc += sBx
//	JmpF          A sBx    if !ToBoolean(R[A]) pc += sBx
//	JmpNullish    A sBx    if R[A] is null or undefined pc += sBx
//	JmpNotNullish A sBx    if R[A] is neither null nor undefined pc += sBx
//	JmpNotUndef   A sBx    if R[A] is not undefined pc += sBx
//	JmpLt         A sBx +X if R[A] < R[X] pc += sBx (offset relative to the X word); JmpLe, JmpGt, JmpGe
//	                       likewise with <=, >, >=. The compiler emits them for a comparison whose value
//	                       only decides a conditional jump; they convert and throw as Lt .. Ge do
//	JmpNLt        A sBx +X if !(R[A] < R[X]) pc += sBx; JmpNLe, JmpNGt, JmpNGe likewise (NaN jumps).
//	                       An X of EnvOperand or more makes the right operand of JmpLt .. JmpNGe
//	                       env^0.slots[X>>8 & 0xff] instead (X's low byte is then A): the compiler emits it for
//	                       a binding closures share, initialized before any read (i < width)
//	JmpStrictEq   A sBx +X if R[A] === R[X] pc += sBx; JmpStrictNe with !==
//	JmpLtI .. JmpStrictNeI A sBx +X  JmpLt .. JmpStrictNe (same order) with the right operand the integer
//	                       int32(X) instead of R[X]: the compiler emits them for a comparison with an integer
//	                       literal that no register holds (n < 2)
//	JmpStrictEqK  A sBx +X JmpStrictEq with the right operand K[X], a string, null or undefined: the
//	                       compiler emits it (and JmpStrictNeK) for a strict comparison with such a literal
//	                       that no register holds and for a switch's case of one
//	JmpTypeofIs   A sBx +X if typeof R[A] === TypeNames[X] pc += sBx; JmpTypeofNot with !==: TypeofIs and the
//	                       jump on its result, for a typeof test that decides a jump
//	JmpCaseK      A sBx +X JmpStrictEqK, then each JmpStrictEqK after it that tests R[A], with no dispatch
//	                       of its own: the compiler emits it for the first of a switch's cases on constants
//	                       that test the discriminant in a row (caseRunMin of them or more)
//
// Calls
//
//	Call       A B C       R[A] = R[A](this = R[A+1], args R[A+2 .. A+1+B]); TypeError if not callable;
//	                       C&CallUndefThis: this is undefined and R[A+1] is not read (a plain call);
//	                       C&CallOverlap: the callee's registers start at R[A+2], its parameters the arguments
//	                       in place, and it writes over the caller's registers from there (the compiler marks
//	                       a call so only when they are dead after it, MarkOverlapCalls)
//	CallSpread A           R[A] = R[A](this = R[A+1], ...array R[A+2])
//	ApplyArguments A B C   R[A] = R[A](this = R[A+1], args R[A+2], R[B]): f.apply(x, arguments) in a function
//	                       with ForwardArguments, whose arguments register R[B] holds the count of the arguments
//	                       the frame keeps above its registers until the object exists. While it does not and
//	                       R[A] is the realm's %Function.prototype.apply%, R[A+1] is called with this R[A+2] and
//	                       those arguments (TypeError if not callable, as apply's). Else the object is created
//	                       into R[B] if it is not yet (C = 1: a mapped one, as with MapArguments B = 1), and
//	                       the call is Call A 2 with R[A+3] = R[B]
//	ArgsLen    A B         R[A] = R[B].length, arguments.length in a function with ForwardArguments: the count R[B]
//	                       holds until the arguments object exists, then its length property
//	ArgsGet    A B C +X    R[A] = R[B][R[C]], arguments[k] in a function with ForwardArguments: while the object
//	                       does not exist, the kept argument R[C] when R[C] is an int32 below the count; else the
//	                       object is created into R[B] first (X = 1: a mapped one, as with ApplyArguments C = 1)
//	New        A B         R[A] = new R[A](R[A+2 .. A+1+B]); TypeError if not a constructor
//	                       C&CallOverlap: the constructor's registers start at R[A+2], as a Call's
//	NewSpread  A           R[A] = new R[A](...array R[A+2])
//	Ret        A           return R[A]
//	RetUndef               return undefined
//	Throw      A           throw R[A]
//	ThrowConstAssign +X    throw TypeError("Assignment to constant variable.") (K[X] names the binding)
//
// Iteration (for-of, spread and array destructuring share GetIterator and
// IteratorStepValue; R[A], R[A+1] hold the Iterator Record: the iterator
// and its next method, or, while the protocol is unobservable, the array or
// string itself and the next index; R[A] is undefined once done or closed)
//
//	IterInit  A B          R[A], R[A+1] = GetIterator(R[B]); TypeError if not iterable
//	IterNext  A sBx        R[A+2] = next value; when exhausted pc += sBx
//	IterValue A B          R[B] = next value or undefined when exhausted (destructuring)
//	IterRest  A B          R[B] = array of the remaining values (rest element)
//	IterClose A sBx        IteratorClose(R[A], normal completion) unless done; pc += sBx
//	IterThrow A B          IteratorClose(R[A], throw completion) unless done; throw R[B]
//	ForInInit A B          R[A] = enumerable string keys of R[B] (prototype chain, deduplicated), R[A+1] = 0, R[A+2] = the object
//	ForInNext A sBx        R[A+3] = next key still present as a property; when exhausted pc += sBx
//
// Classes, super, new.target and private names (off the interpreter's jump
// table: they only run while classes are defined and in class code)
//
//	CtorEntry     A        R[A] = new.target; TypeError "Class constructor X cannot be invoked without 'new'" in a [[Call]] (first op of every class constructor)
//	LoadNewTarget A        R[A] = new.target, undefined in a [[Call]] (first op of a function with NewTarget set)
//	LoadHome      A        R[A] = [[HomeObject]] of the running function
//	SetHome       A B      [[HomeObject]] of function R[A] = R[B]
//	CreateClass   A B C    finish class constructor R[A] (the Closure of a class-constructor template):
//	                       R[A+1] = the new prototype object, R[A].prototype = R[A+1] (read-only),
//	                       R[A+1].constructor = R[A] (non-enumerable), [[HomeObject]] of R[A] = R[A+1];
//	                       C&1: R[B] is the heritage (null, or a constructor whose prototype is an object or null);
//	                       C&2: first name R[A] after property key R[A+1] (an anonymous class field value)
//	DefineClassMethod A B C  R[A][R[B]] := R[C] non-enumerable, [[HomeObject]] of R[C] = R[A];
//	                       an unnamed function is named after the key
//	ToPropertyKey A B      R[A] = ToPropertyKey(R[B]) (computed field keys)
//	GetProtoOf    A B      R[A] = R[B].[[GetPrototypeOf]]() or null: the super base of home object R[B],
//	                       or the super constructor of active function R[B]
//	GetSuper      A B C    R[A] = super property R[C] of base R[B] read with receiver R[B+1];
//	                       TypeError if the base is null (A may equal B)
//	SetSuper      A B C    set super property R[B] of base R[A] to R[C] with receiver R[A+1]; TypeError if refused
//	SuperCall     A B      R[A] = Construct(R[A], args R[A+2 .. A+1+B], new.target R[A+1]); TypeError if not a constructor
//	SuperCallSpread A      same with the argument array R[A+2] (AppendSpread-built, or the default constructor's rest array)
//	CheckSuper    A        ReferenceError "Super constructor may only be called once" unless R[A] is hole
//	DerivedResult A B      R[A] = the value a derived constructor returns for return value R[A] and raw `this` R[B]:
//	                       an object as is; undefined gives `this` (ReferenceError if still hole); otherwise TypeError
//	NewPrivateName A Bx    R[A] = a new private name described by K[Bx] ("#x", or the class name for a brand)
//	SetPrivateMethod A B C +X  make R[B] the method, getter or setter of private name R[A], checked against
//	                       the brand R[C] (or, with PrivateStatic, the class constructor R[C]); X = Private* flags
//	GetPrivate    A B C    R[A] = R[B].#R[C]; TypeError when R[B] lacks it
//	SetPrivate    A B C    R[A].#R[B] = R[C]; TypeError when R[A] lacks it or it is a method
//	DefPrivate    A B C    add private field #R[B] = R[C] to object R[A]; TypeError if already present
//	AddBrand      A B      add class brand R[B] to object R[A]; TypeError if already present
//	InPrivate     A B C    R[A] = #R[B] in R[C]; TypeError if R[C] is not an object
//	ThrowError    A Bx     throw a new TypeError (A=ThrowTypeError) or ReferenceError (A=ThrowReferenceError) with message K[Bx]
//
// Sloppy mode, script globals and with (off the interpreter's jump table,
// after the async ops: only sloppy code, script top levels, with statements
// and strict assignments of an undeclared name from a value that may run
// code use them)
//
//	SetGlobalSloppy A +X   PutValue of an identifier resolved against the global environment in sloppy code:
//	                       an unresolvable name becomes a global object property; a rejected write is
//	                       ignored (X = name:16 | ic:16, the SetGlobal cache; not emitted by the compiler:
//	                       SetGlobal ignores it in a function that is not strict)
//	ResolveGlobal A +X     R[A] = HasBinding(K[X.lo]) of the global environment: ResolveBinding of an
//	                       undeclared name in strict code, before the value assigned to it (X = name:16 | ic:16)
//	SetGlobalRef  A B +X   PutValue of R[A] to the reference ResolveGlobal resolved: ReferenceError unless
//	                       R[B] (its result) is true, else SetMutableBinding(K[X.lo], R[A], strict), whose
//	                       HasProperty throws a ReferenceError for a binding gone since (X = ResolveGlobal's)
//	InitGlobal    A +X     initialize the script's global lexical binding K[X.lo] to R[A] (X.hi = 1: const)
//	SetGlobalVar  A +X     evaluation of a sloppy block function declaration at script level (Annex B.3.2.2):
//	                       globalThis[K[X]] = R[A] as SetGlobalSloppy, skipped when a global lexical binding K[X] exists
//	DelGlobal     A +X     R[A] = delete K[X] for a name resolved against the global environment:
//	                       false for a global lexical binding, else globalThis.[[Delete]](K[X])
//	SetPropSloppy A B +X   R[A].name = R[B], ignoring a rejected assignment (X = name:16 | ic:16)
//	SetElemSloppy A B C    R[A][R[B]] = R[C], ignoring a rejected assignment (not emitted by the
//	                       compiler: SetElem ignores it in a function that is not strict)
//	DelPropSloppy A B +X   R[A] = delete R[B].name; false instead of a TypeError when not configurable
//	DelElemSloppy A B C    R[A] = delete R[B][R[C]]; false instead of a TypeError when not configurable
//	SetSuperSloppy A B C   set super property R[B] of base R[A] to R[C] with receiver R[A+1], ignoring a
//	                       rejected assignment
//	ToObject      A B      R[A] = ToObject(R[B]); TypeError on null or undefined. Also makes the object a
//	                       member initializer or sloppy eval code defines fields on one for the verifier
//	JmpWith       A sBx +X pc += sBx if the object environment of with object R[A] has binding K[X]:
//	                       HasProperty, then a truthy @@unscopables entry blocks it; falls through when R[A]
//	                       is not an object (a function's %evalvars before a direct eval declares a var)
//	WithGet       A B +X   R[A] = GetBindingValue(K[X.lo]) of the object environment of R[B]: undefined,
//	                       or with X.hi = 1 (strict) a ReferenceError, when the property is gone
//	WithSet       A B +X   SetMutableBinding(K[X.lo], R[B]) of the object environment of R[A]
//	                       (X.hi = 1: strict, a ReferenceError when the property is gone and a TypeError when rejected)
//	CoerceThis    A        this = the global object when undefined or null, else ToObject(this); R[A] = this
//	                       (the entry of a sloppy function that uses this, as LoadThis A)
//	MapArguments  A B      make the fresh arguments object R[A] a mapped one: its indices below both the argument
//	                       count and NumParams alias the parameters' slots of the function's own Env (CaptureLayout
//	                       entries below NumParams) and callee is the running function. B = 1 when no code writes
//	                       a parameter (nor can: no direct eval) and parameter i is Env slot i: the object then
//	                       keeps its elements and builds the alias on the first write, define or delete of one
//	CallEval      A B C +X R[A] = Call as with Call (C = 1: CallSpread) of callee R[A], this R[A+1]; when the
//	                       callee is the realm's %eval% it is a direct eval (PerformEval) of the first argument
//	                       in the running Env, compiled against the call site's scope Extra.Evals[X]
package bytecode
