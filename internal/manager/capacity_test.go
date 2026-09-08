// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import "testing"

func TestSharedCapacityCoordinatorDoesNotDoubleAdvertiseCapacity(t *testing.T) {
	coordinator := NewSharedCapacityCoordinator(2, 2, 1)
	first := coordinator.Reserve(1, CapacityObservation{}, CapacityReservation{Startup: 2, Cleanup: 1})
	if first != (CapacityReservation{Startup: 2, Cleanup: 1}) {
		t.Fatalf("first reservation = %+v", first)
	}
	second := coordinator.Reserve(2, CapacityObservation{}, CapacityReservation{Startup: 2, Cleanup: 1})
	if second != (CapacityReservation{}) {
		t.Fatalf("second reservation = %+v", second)
	}

	coordinator.Release(1, first, CapacityReservation{Startup: 1})
	second = coordinator.Reserve(2, CapacityObservation{}, CapacityReservation{Startup: 2, Cleanup: 1})
	if second != (CapacityReservation{Startup: 1, Cleanup: 1}) {
		t.Fatalf("reservation while first site starts = %+v", second)
	}

	coordinator.Forget(1)
	coordinator.Release(2, second, CapacityReservation{})
	second = coordinator.Reserve(2, CapacityObservation{}, CapacityReservation{Startup: 2, Cleanup: 1})
	if second != (CapacityReservation{Startup: 2, Cleanup: 1}) {
		t.Fatalf("reservation after forgetting stopped site = %+v", second)
	}
}
