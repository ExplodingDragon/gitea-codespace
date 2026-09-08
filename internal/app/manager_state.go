// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
)

// ManagerStateStore persists the site's inventory sequence under its leader term.
type ManagerStateStore struct {
	records *executionStateStore
}

func (s *ManagerStateStore) SaveInventoryGeneration(generation int64) error {
	previous, revision, err := loadInventoryGeneration(s.records)
	if err != nil {
		return err
	}
	if generation < previous || generation < 0 {
		return fmt.Errorf("inventory generation must not decrease")
	}
	return s.records.save("inventory", []byte(strconv.FormatInt(generation, 10)), revision)
}

func loadInventoryGeneration(records *executionStateStore) (generation, revision int64, err error) {
	content, revision, err := records.load("inventory")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	generation, err = strconv.ParseInt(string(content), 10, 64)
	if err != nil || generation < 0 {
		return 0, 0, fmt.Errorf("invalid stored inventory generation")
	}
	return generation, revision, nil
}
