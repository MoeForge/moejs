//go:build (!amd64 && !arm64) || purego

package node

// Here moego's base64 is encoding/base64 at every length, which Buffer's
// tables in encoding.go beat, so Buffer never uses it.
const moegoEncodeMin, moegoDecodeMin = maxInt, maxInt

const maxInt = int(^uint(0) >> 1)
