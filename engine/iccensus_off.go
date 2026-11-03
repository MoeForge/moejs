//go:build !iccensus

package engine

import "github.com/MoeForge/moejs/bytecode"

// icCensus is false in normal builds: the census hooks in the interpreter
// loop are dead code the compiler drops (iccensus.go).
const icCensus = false

func censusHit(*bytecode.Function, bytecode.Op, uint32, int, *Shape)            {}
func censusMiss(*bytecode.Function, bytecode.Op, uint32, int, *ICEntry, *Shape) {}
