// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import "sync"

// CapacityObservation is one site's current use of process-wide runtime and worker capacity.
type CapacityObservation struct {
	RuntimeOccupied int32
	StartupActive   int32
	CleanupActive   int32
}

// CapacityReservation is capacity advertised by one FetchOperations request.
type CapacityReservation struct {
	Startup int32
	Cleanup int32
}

// CapacityCoordinator prevents site agents in one process from advertising the same capacity.
type CapacityCoordinator interface {
	Reserve(siteID int64, observation CapacityObservation, requested CapacityReservation) CapacityReservation
	Release(siteID int64, reserved, started CapacityReservation)
	Forget(siteID int64)
}

type sharedCapacitySite struct {
	observation CapacityObservation
	held        CapacityReservation
}

type sharedCapacityCoordinator struct {
	mu             sync.Mutex
	capacityTotal  int32
	startupWorkers int32
	cleanupWorkers int32
	sites          map[int64]sharedCapacitySite
}

// NewSharedCapacityCoordinator creates one admission coordinator for all site agents in a process.
func NewSharedCapacityCoordinator(capacityTotal, startupWorkers, cleanupWorkers int32) CapacityCoordinator {
	return &sharedCapacityCoordinator{
		capacityTotal:  capacityTotal,
		startupWorkers: startupWorkers,
		cleanupWorkers: cleanupWorkers,
		sites:          make(map[int64]sharedCapacitySite),
	}
}

func (c *sharedCapacityCoordinator) Reserve(siteID int64, observation CapacityObservation, requested CapacityReservation) CapacityReservation {
	c.mu.Lock()
	defer c.mu.Unlock()

	// The new observation includes work accepted by this site's previous fetch, so its old hold can be replaced.
	c.sites[siteID] = sharedCapacitySite{observation: observation}
	var runtimeOccupied, startupActive, cleanupActive, startupHeld, cleanupHeld int32
	for _, site := range c.sites {
		runtimeOccupied += site.observation.RuntimeOccupied
		startupActive += site.observation.StartupActive
		cleanupActive += site.observation.CleanupActive
		startupHeld += site.held.Startup
		cleanupHeld += site.held.Cleanup
	}
	reserved := CapacityReservation{
		Startup: min(requested.Startup, max(0, c.capacityTotal-runtimeOccupied-startupHeld), max(0, c.startupWorkers-startupActive-startupHeld)),
		Cleanup: min(requested.Cleanup, max(0, c.cleanupWorkers-cleanupActive-cleanupHeld)),
	}
	c.sites[siteID] = sharedCapacitySite{observation: observation, held: reserved}
	return reserved
}

func (c *sharedCapacityCoordinator) Release(siteID int64, reserved, started CapacityReservation) {
	c.mu.Lock()
	defer c.mu.Unlock()

	site, ok := c.sites[siteID]
	if !ok {
		return
	}
	site.held = CapacityReservation{
		Startup: min(started.Startup, reserved.Startup),
		Cleanup: min(started.Cleanup, reserved.Cleanup),
	}
	c.sites[siteID] = site
}

func (c *sharedCapacityCoordinator) Forget(siteID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sites, siteID)
}
