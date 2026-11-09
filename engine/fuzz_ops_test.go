package engine

import (
	"testing"

	"github.com/MoeForge/moejs/bytecode"
)

// fuzzOps is the op numbering of FuzzBytecode's edits: an edit's value byte
// v names the op fuzzOps[v], not bytecode.Op(v) (fuzzEdit), so that the
// corpus in testdata/fuzz/FuzzBytecode keeps meaning the programs it was
// found as when the ops are renumbered. It is the op order of 7f62662,
// which the corpus was found under. Append-only: a new op goes at its end
// (which changes only what a byte past the old end names), and no entry
// moves or goes.
var fuzzOps = [...]bytecode.Op{
	bytecode.LoadConst, bytecode.LoadInt, bytecode.LoadUndef, bytecode.LoadNull,
	bytecode.LoadTrue, bytecode.LoadFalse, bytecode.LoadHole, bytecode.LoadThis,
	bytecode.LoadCallee, bytecode.Move, bytecode.UndefRange, bytecode.GetEnv,
	bytecode.GetEnvChk, bytecode.SetEnv, bytecode.GetEnvW, bytecode.GetEnvChkW,
	bytecode.SetEnvW, bytecode.PushEnv, bytecode.PopEnv, bytecode.CopyEnv,
	bytecode.CheckTDZ, bytecode.GetGlobal, bytecode.GetGlobalOrUndef,
	bytecode.SetGlobal, bytecode.GetProp, bytecode.GetLen, bytecode.SetProp,
	bytecode.GetElem, bytecode.SetElem, bytecode.DelProp, bytecode.DelElem,
	bytecode.In, bytecode.InstanceOf, bytecode.NewObject, bytecode.NewArray,
	bytecode.DefineField, bytecode.DefineElem, bytecode.DefineMethod,
	bytecode.DefineAccessor, bytecode.SetProto, bytecode.CopyDataProps,
	bytecode.CopyDataPropsEx, bytecode.ArrayPush, bytecode.ArrayHole,
	bytecode.AppendSpread, bytecode.Closure, bytecode.NewRegExp,
	bytecode.GetTemplate, bytecode.Add, bytecode.Sub, bytecode.Mul,
	bytecode.Div, bytecode.Mod, bytecode.Exp, bytecode.AddImm, bytecode.SubImm,
	bytecode.Inc, bytecode.Dec, bytecode.Neg, bytecode.Plus, bytecode.Not,
	bytecode.BitNot, bytecode.ToNumeric, bytecode.RequireObjectCoercible,
	bytecode.ToStr, bytecode.Concat, bytecode.Typeof, bytecode.TypeofIs,
	bytecode.BitAnd, bytecode.BitOr, bytecode.BitXor, bytecode.Shl,
	bytecode.Shr, bytecode.UShr, bytecode.Eq, bytecode.Ne, bytecode.StrictEq,
	bytecode.StrictNe, bytecode.Lt, bytecode.Le, bytecode.Gt, bytecode.Ge,
	bytecode.Jmp, bytecode.JmpT, bytecode.JmpF, bytecode.JmpNullish,
	bytecode.JmpNotNullish, bytecode.JmpNotUndef, bytecode.Call,
	bytecode.CallSpread, bytecode.New, bytecode.NewSpread, bytecode.Ret,
	bytecode.RetUndef, bytecode.Throw, bytecode.ThrowConstAssign,
	bytecode.IterInit, bytecode.IterNext, bytecode.IterValue, bytecode.IterRest,
	bytecode.IterClose, bytecode.IterThrow, bytecode.ForInInit,
	bytecode.ForInNext, bytecode.GetElemRef, bytecode.CtorEntry,
	bytecode.LoadNewTarget, bytecode.LoadHome, bytecode.SetHome,
	bytecode.CreateClass, bytecode.DefineClassMethod, bytecode.ToPropertyKey,
	bytecode.GetProtoOf, bytecode.GetSuper, bytecode.SetSuper,
	bytecode.SuperCall, bytecode.SuperCallSpread, bytecode.CheckSuper,
	bytecode.DerivedResult, bytecode.NewPrivateName, bytecode.SetPrivateMethod,
	bytecode.GetPrivate, bytecode.SetPrivate, bytecode.DefPrivate,
	bytecode.AddBrand, bytecode.InPrivate, bytecode.ThrowError,
	bytecode.GenFunc, bytecode.GenStart, bytecode.Yield, bytecode.GenIter,
	bytecode.Delegate, bytecode.AsyncFunc, bytecode.AsyncStart,
	bytecode.AsyncGenStart, bytecode.Await, bytecode.AsyncReturn,
	bytecode.AsyncThrow, bytecode.AsyncYield, bytecode.AsyncIterInit,
	bytecode.AsyncIterNext, bytecode.AsyncIterResult, bytecode.AsyncIterClose,
	bytecode.AsyncIterClosed, bytecode.AsyncDelegate, bytecode.GetImport,
	bytecode.GetImportW, bytecode.ImportCall, bytecode.ImportMeta,
	bytecode.SetGlobalSloppy, bytecode.ResolveGlobal, bytecode.SetGlobalRef,
	bytecode.InitGlobal, bytecode.SetGlobalVar, bytecode.DelGlobal,
	bytecode.SetPropSloppy, bytecode.SetElemSloppy, bytecode.DelPropSloppy,
	bytecode.DelElemSloppy, bytecode.SetSuperSloppy, bytecode.ToObject,
	bytecode.JmpWith, bytecode.WithGet, bytecode.WithSet, bytecode.CoerceThis,
	bytecode.MapArguments, bytecode.CallEval, bytecode.JmpLt, bytecode.JmpLe,
	bytecode.JmpGt, bytecode.JmpGe, bytecode.JmpNLt, bytecode.JmpNLe,
	bytecode.JmpNGt, bytecode.JmpNGe, bytecode.JmpStrictEq,
	bytecode.JmpStrictNe, bytecode.JmpLtI, bytecode.JmpLeI, bytecode.JmpGtI,
	bytecode.JmpGeI, bytecode.JmpNLtI, bytecode.JmpNLeI, bytecode.JmpNGtI,
	bytecode.JmpNGeI, bytecode.JmpStrictEqI, bytecode.JmpStrictNeI,
	bytecode.JmpStrictEqK, bytecode.JmpStrictNeK, bytecode.JmpTypeofIs,
	bytecode.JmpTypeofNot, bytecode.JmpCaseK, bytecode.RegExpTest,
	bytecode.ApplyArguments, bytecode.ArrayPushK, bytecode.ArgsLen,
	bytecode.ArgsGet, bytecode.GetElemInc, bytecode.PostInc, bytecode.PostDec,
	bytecode.NewArrayK, bytecode.GetEnv1, bytecode.GetEnv2, bytecode.GetEnv0,
	bytecode.NewObjectLit,
}

// fuzzOpIndex is fuzzOps inverted, the index of an op in it, and the
// identity past OpCount, so that a byte maps either way one to one.
var fuzzOpIndex = func() (ix [256]byte) {
	for i := range ix {
		ix[i] = byte(i)
	}
	for i, op := range fuzzOps {
		ix[op] = byte(i)
	}
	return ix
}()

// fuzzOp is the op of fuzzOps at index i, and Op(i) past it.
func fuzzOp(i byte) bytecode.Op {
	if int(i) < len(fuzzOps) {
		return fuzzOps[i]
	}
	return bytecode.Op(i)
}

func TestFuzzOpsHasEveryOpOnce(t *testing.T) {
	if len(fuzzOps) != bytecode.OpCount {
		t.Fatalf("fuzzOps has %d ops, bytecode %d: append a new op at fuzzOps' end", len(fuzzOps), bytecode.OpCount)
	}
	seen := make([]bool, bytecode.OpCount)
	for i, op := range fuzzOps {
		if int(op) >= bytecode.OpCount || seen[op] {
			t.Fatalf("fuzzOps[%d] = %v: not an op, or twice", i, op)
		}
		seen[op] = true
	}
}
