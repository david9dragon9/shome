//go:build darwin && cgo

package darwin

/*
#cgo CFLAGS: -x objective-c -fmodules -fobjc-arc
#cgo LDFLAGS: -framework Metal -framework Foundation
#import <Metal/Metal.h>
#include <stdlib.h>

// shome_metal_info fills in the fields shome schedules on.
// Returns 1 on success, 0 if no Metal device is present (headless VM, or a
// context where the GPU is not reachable).
static int shome_metal_info(char *name, int name_len,
                            unsigned long long *recommended_working_set,
                            unsigned long long *max_buffer,
                            int *has_unified) {
    @autoreleasepool {
        id<MTLDevice> dev = MTLCreateSystemDefaultDevice();
        if (dev == nil) return 0;
        const char *n = [[dev name] UTF8String];
        if (n != NULL) { strncpy(name, n, name_len - 1); name[name_len - 1] = 0; }
        *recommended_working_set = [dev recommendedMaxWorkingSetSize];
        *max_buffer = [dev maxBufferLength];
        *has_unified = [dev hasUnifiedMemory] ? 1 : 0;
        return 1;
    }
}
*/
import "C"

// MetalInfo describes the GPU as shome schedules it.
type MetalInfo struct {
	Name string
	// RecommendedWorkingSet is the GPU-usable memory budget. On Apple Silicon
	// this is well below installed RAM -- measured 12124 MiB on a 16 GB M5,
	// about 74%. Scheduling against installed RAM would over-commit the GPU
	// and the job would fail at allocation time rather than in the queue.
	RecommendedWorkingSet int64
	MaxBufferLength       int64
	UnifiedMemory         bool
}

// DetectMetal reports the system's Metal device, if any.
//
// Returns ok=false rather than an error when no device exists: a Mac with no
// reachable GPU is a valid CPU-only node, not a broken one.
func DetectMetal() (MetalInfo, bool) {
	var (
		name       [256]C.char
		workingSet C.ulonglong
		maxBuffer  C.ulonglong
		unified    C.int
	)
	if C.shome_metal_info(&name[0], C.int(len(name)), &workingSet, &maxBuffer, &unified) == 0 {
		return MetalInfo{}, false
	}
	return MetalInfo{
		Name:                  C.GoString(&name[0]),
		RecommendedWorkingSet: int64(workingSet),
		MaxBufferLength:       int64(maxBuffer),
		UnifiedMemory:         unified != 0,
	}, true
}
