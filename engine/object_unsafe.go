package engine

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// elemStore is a []Value in 16 bytes: the dense elements of an array or
// arguments object, right after its header, and those of any other object
// in its objectExt. Capacity past 1<<32-1 is dropped, which only makes a
// later append reallocate.
type elemStore struct {
	p    *Value
	n, c uint32
}

// sliceHeader is the layout of a []Value.
type sliceHeader struct {
	p    *Value
	n, c int
}

func (e *elemStore) slice() []Value {
	h := sliceHeader{e.p, int(e.n), int(e.c)}
	return *(*[]Value)(unsafe.Pointer(&h))
}

// at returns the address of element i, which the caller has checked is
// below e.n. The run loop's element fast paths index through it: a slice
// built from the elemStore (slice) goes through a local whose stores, in a
// condition, keep the compiler from turning a && chain into branches.
func (e *elemStore) at(i int) *Value {
	return (*Value)(unsafe.Add(unsafe.Pointer(e.p), uintptr(i)*unsafe.Sizeof(Value{})))
}

func (e *elemStore) set(s []Value) {
	e.p, e.n, e.c = unsafe.SliceData(s), uint32(len(s)), uint32(min(uint(cap(s)), 1<<32-1))
}

// inlineObject is the start of every object with bitInlineElems: the
// header and its elements.
type inlineObject struct {
	obj Object
	el  elemStore
}

var _ [0]struct{} = [unsafe.Offsetof(inlineObject{}.el) - unsafe.Sizeof(Object{})]struct{}{}

// inlineElems returns the element storage of an object with bitInlineElems,
// which was allocated as an inlineObject or a type starting with one.
func (o *Object) inlineElems() *elemStore { return &(*inlineObject)(unsafe.Pointer(o)).el }

// initInline marks o, the header of x, an object with inline elements.
func (x *inlineObject) initInline() *Object {
	x.obj.bits |= bitInlineElems
	return &x.obj
}

// payloadKind is the type of an object's payload: an index into
// payloadTypes, which holds the type word of the interface the payload
// was set as. A kind below firstExtKind (a kindOf) is co-allocated right
// after the header; any other (an extKindOf, or a kind a host's type got)
// is a pointer in ext, or in the objectExt when the object has one. That
// pointer is the interface's data word, so the collector sees it as it saw
// the interface: the address of the payload, or of the boxed copy of a
// value that is not a pointer.
type payloadKind uint8

// kindOf is a payloadKind whose payload is a T right after the header.
type kindOf[T any] payloadKind

// extKindOf is a payloadKind whose payload is a *T in ext.
type extKindOf[T any] payloadKind

// The payload kinds the engine sets. The type switch of payloadKindOf and
// enginePayloadTypes list the same ones; any other type gets a kind on first
// use (registerPayload).
const (
	kindFunction         kindOf[FunctionData]     = iota + 1
	kindProxy            kindOf[proxyData]        = iota + 1
	kindNamespace        kindOf[moduleNamespace]  = iota + 1
	kindDate             kindOf[DateData]         = iota + 1
	kindPromise          kindOf[promiseData]      = iota + 1
	kindIterator         kindOf[IteratorData]     = iota + 1
	kindCollection       kindOf[collection]       = iota + 1
	kindCollIter         kindOf[collIterData]     = iota + 1
	kindDataView         kindOf[dataView]         = iota + 1
	kindWeakColl         kindOf[weakColl]         = iota + 1
	kindWeakRef          kindOf[weakRefData]      = iota + 1
	kindTypedArray       kindOf[typedArray]       = iota + 1
	kindTextDecoder      kindOf[textDecoder]      = iota + 1
	kindGenerator        kindOf[genFrame]         = iota + 1
	kindError            kindOf[ErrorData]        = iota + 1
	kindRegExp           kindOf[RegExpData]       = iota + 1
	kindRegExpStringIter kindOf[regexpStringIter] = iota + 1
	// A shared buffer's payload is another object's; an async generator and
	// an async-from-sync iterator are their payload; the others are set
	// after the object is made, or allocated apart from it.
	kindArrayBuffer        extKindOf[arrayBuffer]         = iota + 1
	kindAsyncGenerator     extKindOf[asyncGenerator]      = iota + 1
	kindAsyncFromSync      extKindOf[asyncFromSyncIter]   = iota + 1
	kindArgumentsMap       extKindOf[argumentsMap]        = iota + 1
	kindArgumentsEnv       extKindOf[argumentsEnv]        = iota + 1
	kindPrimitive          extKindOf[primitiveWrapper]    = iota + 1
	kindString             extKindOf[String]              = iota + 1
	kindSymbol             extKindOf[Symbol]              = iota + 1
	kindBigInt             extKindOf[BigInt]              = iota + 1
	kindLazyProps          extKindOf[lazyProps]           = iota + 1
	kindLazyKeys           extKindOf[lazyKeys]            = iota + 1
	kindPendingCompile     extKindOf[pendingCompile]      = iota + 1
	kindRealm              extKindOf[Realm]               = iota + 1
	kindIntlLocale         extKindOf[intlLocale]          = iota + 1
	kindIntlSegmenter      extKindOf[intlSegmenter]       = iota + 1
	kindIntlSegments       extKindOf[intlSegments]        = iota + 1
	kindIntlNumberFormat   extKindOf[intlNumberFormat]    = iota + 1
	kindIntlDateTimeFormat extKindOf[intlDateTimeFormat]  = iota + 1
	kindIntlPluralRules    extKindOf[intlPluralRulesData] = iota + 1
	kindPendingDescs       extKindOf[pendingDescs]        = iota + 1
	kindLast                                              = iota + 1

	firstExtKind = payloadKind(kindArrayBuffer)
)

// The kinds of the payloads that are not pointers: a host map node's Go map
// and a host object node's JSON text.
const (
	kindHostMap payloadKind = kindLast + iota
	kindHostStrings
	kindHostLists
	kindRawText
	staticKinds
	// kindBoxed is the kind of a payload whose type found no kind of its
	// own, or of a co-allocated kind set apart from its object: the payload
	// is an *any holding it, which payload returns and hostPayload unwraps.
	kindBoxed payloadKind = 255
)

// payloadTypes holds the type word of each payload kind below
// payloadCount: the engine's from the start (an initializer, so that a
// package variable holding a realm finds them), others as registerPayload
// adds them, publishing each type word before the count that covers it.
var (
	payloadTypes = enginePayloadTypes()
	payloadCount atomic.Int32
	payloadMu    sync.Mutex
)

func setKind[T any](t *[256]unsafe.Pointer, k kindOf[T]) { t[k] = typeWord((*T)(nil)) }

func setExtKind[T any](t *[256]unsafe.Pointer, k extKindOf[T]) { t[k] = typeWord((*T)(nil)) }

func enginePayloadTypes() (t [256]unsafe.Pointer) {
	setKind(&t, kindFunction)
	setKind(&t, kindProxy)
	setKind(&t, kindNamespace)
	setKind(&t, kindDate)
	setKind(&t, kindPromise)
	setKind(&t, kindIterator)
	setKind(&t, kindCollection)
	setKind(&t, kindCollIter)
	setExtKind(&t, kindArrayBuffer)
	setKind(&t, kindDataView)
	setKind(&t, kindWeakColl)
	setKind(&t, kindWeakRef)
	setKind(&t, kindTypedArray)
	setKind(&t, kindTextDecoder)
	setKind(&t, kindGenerator)
	setKind(&t, kindError)
	setKind(&t, kindRegExp)
	setKind(&t, kindRegExpStringIter)
	setExtKind(&t, kindAsyncGenerator)
	setExtKind(&t, kindAsyncFromSync)
	setExtKind(&t, kindArgumentsMap)
	setExtKind(&t, kindArgumentsEnv)
	setExtKind(&t, kindPrimitive)
	setExtKind(&t, kindString)
	setExtKind(&t, kindSymbol)
	setExtKind(&t, kindBigInt)
	setExtKind(&t, kindLazyProps)
	setExtKind(&t, kindLazyKeys)
	setExtKind(&t, kindPendingCompile)
	setExtKind(&t, kindRealm)
	setExtKind(&t, kindIntlLocale)
	setExtKind(&t, kindIntlSegmenter)
	setExtKind(&t, kindIntlSegments)
	setExtKind(&t, kindIntlNumberFormat)
	setExtKind(&t, kindIntlDateTimeFormat)
	setExtKind(&t, kindIntlPluralRules)
	setExtKind(&t, kindPendingDescs)
	t[kindHostMap] = typeWord(map[string]any(nil))
	t[kindHostStrings] = typeWord(map[string]string(nil))
	t[kindHostLists] = typeWord(map[string][]string(nil))
	t[kindRawText] = typeWord(rawText(""))
	t[kindBoxed] = typeWord((*any)(nil))
	return t
}

// payloadKindOf returns the kind of v's dynamic type, giving the type one
// if it has none yet.
func payloadKindOf(v any) payloadKind {
	switch v.(type) {
	case nil:
		return 0
	case *FunctionData:
		return payloadKind(kindFunction)
	case *lazyProps:
		return payloadKind(kindLazyProps)
	case *String:
		return payloadKind(kindString)
	case *primitiveWrapper:
		return payloadKind(kindPrimitive)
	case *Symbol:
		return payloadKind(kindSymbol)
	case *BigInt:
		return payloadKind(kindBigInt)
	case map[string]any:
		return kindHostMap
	case map[string]string:
		return kindHostStrings
	case map[string][]string:
		return kindHostLists
	case rawText:
		return kindRawText
	}
	t := typeWord(v)
	if k := findPayloadKind(t, payloadKinds()); k != 0 {
		return k
	}
	return registerPayload(t)
}

// findPayloadKind returns the kind below n whose type word is t, or 0. The
// kinds a host registered come last, so they are found first.
func findPayloadKind(t unsafe.Pointer, n int) payloadKind {
	for k := n - 1; k > 0; k-- {
		if atomic.LoadPointer(&payloadTypes[k]) == t {
			return payloadKind(k)
		}
	}
	return 0
}

// registerPayload gives the type t a payload kind.
func registerPayload(t unsafe.Pointer) payloadKind {
	payloadMu.Lock()
	defer payloadMu.Unlock()
	n := payloadKinds()
	if k := findPayloadKind(t, n); k != 0 {
		return k
	}
	if n >= int(kindBoxed) {
		return kindBoxed
	}
	atomic.StorePointer(&payloadTypes[n], t)
	payloadCount.Store(int32(n + 1 - int(staticKinds)))
	return payloadKind(n)
}

// payloadKinds returns the number of payload kinds in use.
func payloadKinds() int { return int(staticKinds) + int(payloadCount.Load()) }

// payloadAddr returns the address of o's payload, of kind k.
func (o *Object) payloadAddr(k payloadKind) unsafe.Pointer {
	if k < firstExtKind {
		return unsafe.Add(unsafe.Pointer(o), unsafe.Sizeof(Object{}))
	}
	return o.extPayload()
}

// extPayload returns the payload of a kind that is not co-allocated.
func (o *Object) extPayload() unsafe.Pointer {
	if o.bits&bitHasExt != 0 {
		return (*objectExt)(o.ext).payload
	}
	return o.ext
}

// setExtPayload sets the payload to p, of kind k, which is not
// co-allocated (or 0 with p nil): in the objectExt if o has one, else in
// ext itself.
func (o *Object) setExtPayload(k payloadKind, p unsafe.Pointer) {
	if o.bits&bitHasExt != 0 {
		(*objectExt)(o.ext).payload = p
	} else {
		o.ext = p
	}
	o.pkind = k
}

// payload returns the class-specific payload.
func (o *Object) payload() (v any) {
	if o.pkind == 0 {
		return nil
	}
	e := (*[2]unsafe.Pointer)(unsafe.Pointer(&v))
	e[0], e[1] = atomic.LoadPointer(&payloadTypes[o.pkind]), o.payloadAddr(o.pkind)
	return v
}

// setPayload replaces the class-specific payload. A function's stays its
// *FunctionData: funcData reads it unchecked. A value of a co-allocated
// kind other than o's own is boxed, so that the engine never reads it as
// o's.
func (o *Object) setPayload(v any) {
	k := payloadKindOf(v)
	p := (*[2]unsafe.Pointer)(unsafe.Pointer(&v))[1]
	own := uintptr(p) == uintptr(unsafe.Pointer(o))+unsafe.Sizeof(Object{}) // compared as integers: o may end there
	if o.class == ClassFunction {
		if k != payloadKind(kindFunction) || !own {
			panic("engine: the payload of a function must be its *FunctionData")
		}
		return
	}
	if k != 0 && k < firstExtKind {
		if k == o.pkind && own {
			return
		}
		k = kindBoxed
	}
	if k == kindBoxed {
		p = boxPayload(v)
	}
	o.setExtPayload(k, p)
}

// hostPayload returns the payload as SetInternal set it.
func (o *Object) hostPayload() (v any) {
	k, p := o.pkind, o.ext
	if k == 0 {
		return nil
	}
	if o.bits&bitHasExt != 0 {
		p = (*objectExt)(p).payload
	}
	if k < firstExtKind {
		p = unsafe.Add(unsafe.Pointer(o), unsafe.Sizeof(Object{}))
	} else if k == kindBoxed {
		return *(*any)(p)
	}
	e := (*[2]unsafe.Pointer)(unsafe.Pointer(&v))
	e[0], e[1] = atomic.LoadPointer(&payloadTypes[k]), p
	return v
}

// boxPayload returns an *any holding v, for kindBoxed.
func boxPayload(v any) unsafe.Pointer {
	p := new(any)
	*p = v
	return unsafe.Pointer(p)
}

// clearPayload drops the payload.
func (o *Object) clearPayload() {
	if o.pkind >= firstExtKind {
		o.setExtPayload(0, nil)
	}
	o.pkind = 0
}

// setPayloadKind sets the payload to v, whose kind, not co-allocated, the
// caller knows is k.
func (o *Object) setPayloadKind(k payloadKind, v any) {
	o.setExtPayload(k, (*[2]unsafe.Pointer)(unsafe.Pointer(&v))[1])
}

// setData sets the payload of o to p, of kind k, which must be right
// after o's header.
func setData[T any](o *Object, k kindOf[T], p *T) {
	if uintptr(unsafe.Pointer(p)) != uintptr(unsafe.Pointer(o))+unsafe.Sizeof(Object{}) {
		panic("engine: a co-allocated payload set apart from its object")
	}
	o.pkind = payloadKind(k)
}

// setExtData sets the payload of o to p, of kind k.
func setExtData[T any](o *Object, k extKindOf[T], p *T) {
	o.setExtPayload(payloadKind(k), unsafe.Pointer(p))
}

// dataOf returns the payload of o if it is of kind k, or nil.
func dataOf[T any](o *Object, k kindOf[T]) *T {
	if o.pkind != payloadKind(k) {
		return nil
	}
	return (*T)(unsafe.Add(unsafe.Pointer(o), unsafe.Sizeof(Object{})))
}

// dataOk is dataOf in the form of a type assertion with ok.
func dataOk[T any](o *Object, k kindOf[T]) (*T, bool) {
	if o.pkind != payloadKind(k) {
		return nil, false
	}
	return (*T)(unsafe.Add(unsafe.Pointer(o), unsafe.Sizeof(Object{}))), true
}

// extDataOf returns the payload of o if it is of kind k, or nil.
func extDataOf[T any](o *Object, k extKindOf[T]) *T {
	if o.pkind != payloadKind(k) {
		return nil
	}
	return (*T)(o.extPayload())
}

// extDataOk is extDataOf in the form of a type assertion with ok.
func extDataOk[T any](o *Object, k extKindOf[T]) (*T, bool) {
	if o.pkind != payloadKind(k) {
		return nil, false
	}
	return (*T)(o.extPayload()), true
}
