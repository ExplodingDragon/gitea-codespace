// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import "testing"

func TestInventoryGenerationPersistsAndIsolatesSites(t *testing.T) {
	state := newTestCodespaceStateStore(t, t.TempDir())
	store := &ManagerStateStore{records: state.records}
	if err := store.SaveInventoryGeneration(7); err != nil {
		t.Fatal(err)
	}
	generation, _, err := loadInventoryGeneration(state.records)
	if err != nil || generation != 7 {
		t.Fatalf("generation = %d: %v", generation, err)
	}
	if err := store.SaveInventoryGeneration(6); err == nil {
		t.Fatal("generation decreased")
	}
	other := *state.records
	other.siteID = 2
	generation, _, err = loadInventoryGeneration(&other)
	if err != nil || generation != 0 {
		t.Fatalf("other site generation = %d: %v", generation, err)
	}
}
