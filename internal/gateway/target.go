// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"fmt"

	"github.com/google/uuid"
)

// ValidateRuntimeUUID checks the canonical identity used in routes and state keys.
func ValidateRuntimeUUID(codespaceUUID string) error {
	parsed, err := uuid.Parse(codespaceUUID)
	if err != nil {
		return err
	}
	if parsed.Version() != 4 || parsed.String() != codespaceUUID {
		return fmt.Errorf("codespace uuid must be canonical lower-case UUID v4")
	}
	return nil
}
