package main

/*
#include <malloc/malloc.h>
static void malloc_stats(size_t *in_use, size_t *max_in_use) {
	malloc_statistics_t s;
	malloc_zone_statistics(NULL, &s);
	*in_use = s.size_in_use;
	*max_in_use = s.max_size_in_use;
}
*/
import "C"

// mallocStats returns the bytes C malloc has handed out and not freed, and
// the most it ever had.
func mallocStats() (inUse, maxInUse uint64) {
	var a, b C.size_t
	C.malloc_stats(&a, &b)
	return uint64(a), uint64(b)
}
