package node

// errnosMore is empty on js/wasm: its syscall package has no ETXTBSY, and
// Node.js's ETXTBSY makes the call panic before it gets here.
var errnosMore []errnoEntry
