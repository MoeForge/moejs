package main

/*
#include <malloc.h>
static size_t malloc_in_use(void) {
	struct mallinfo2 m = mallinfo2();
	return m.uordblks + m.hblkhd;
}
*/
import "C"

// mallocStats returns the bytes glibc malloc has handed out and not freed,
// summed over all arenas plus mmapped chunks. glibc keeps no maximum.
func mallocStats() (inUse, maxInUse uint64) {
	return uint64(C.malloc_in_use()), 0
}
