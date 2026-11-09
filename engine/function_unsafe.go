package engine

import "unsafe"

// The native kinds' nativeState and a bound function's boundFunc come right
// after the FunctionData in every object that holds one.
var _ [0]struct{} = [unsafe.Offsetof(nativeFuncObject{}.nat) - unsafe.Offsetof(nativeFuncObject{}.fd) - unsafe.Sizeof(FunctionData{}) +
	unsafe.Offsetof(nativeCtorObject{}.nat) - unsafe.Offsetof(nativeCtorObject{}.fd) - unsafe.Sizeof(FunctionData{}) +
	unsafe.Offsetof(callableProxyObject{}.nat) - unsafe.Offsetof(callableProxyObject{}.fd) - unsafe.Sizeof(FunctionData{}) +
	unsafe.Offsetof(boundFuncObject{}.bound) - unsafe.Offsetof(boundFuncObject{}.fd) - unsafe.Sizeof(FunctionData{})]struct{}{}

// nat returns the state of a function of the native kinds (FuncNative,
// FuncNativeData, FuncProxy): a nativeFuncObject, nativeCtorObject or
// callableProxyObject holds it right after fd. A function of another kind
// has none there: the caller checks the kind.
func (fd *FunctionData) nat() *nativeState {
	return (*nativeState)(unsafe.Add(unsafe.Pointer(fd), unsafe.Sizeof(FunctionData{})))
}

// bound returns the state of a bound function, which its boundFuncObject
// holds right after fd.
func (fd *FunctionData) bound() *boundFunc {
	return (*boundFunc)(unsafe.Add(unsafe.Pointer(fd), unsafe.Sizeof(FunctionData{})))
}
