//go:build !purego

package node

// moego's base64 runs its NEON kernels from 48 bytes to encode and 64
// characters to decode on (its encodeMin and decodeMin); below them it is
// encoding/base64, which Buffer's tables in encoding.go beat.
const moegoEncodeMin, moegoDecodeMin = 48, 64
