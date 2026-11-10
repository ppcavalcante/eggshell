// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package microvm

import (
	"sync/atomic"
)

// Start a Go scheduler M on each processor, by running goroutines on all of
// them at once, as starting one (amd64.CPU.Task) writes low memory which
// process page tables do not map.
func preallocateMs(n int) {
	running := atomic.Int32{}
	spin := func() {
		for running.Add(1); running.Load() < int32(n); {
		}
	}

	for range n - 1 {
		go spin()
	}

	spin()
}
