//go:build !purego

package node

// moego's base64 runs its AVX2 kernels from one 24-byte encode block and
// one 32-character decode block on (its encodeMin and decodeMin); below
// them it is encoding/base64, which Buffer's tables in encoding.go beat.
const moegoEncodeMin, moegoDecodeMin = 24, 32
