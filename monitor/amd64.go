// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package monitor

import (
	"github.com/usbarmory/tamago/amd64"
	"github.com/usbarmory/tamago/amd64/lapic"
)

// aliased constants
const (
	lapicID        = amd64.LAPIC_ID
	lapicEOI       = amd64.LAPIC_EOI
	lapicICRL      = amd64.LAPIC_BASE + lapic.LAPIC_ICRL
	lapicICRH      = amd64.LAPIC_BASE + lapic.LAPIC_ICRH
	lapicTimer     = amd64.LAPIC_TIMER
	lapicTimerInit = amd64.LAPIC_TIMER_INIT
	lapicTimerDiv  = amd64.LAPIC_TIMER_DIV
)

// defined in amd64.s
func alarm(count uint32, poll uint64, gap uint64) bool
func xgetbv(index uint32) uint64
