// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"fmt"
	"uuid"
)

// Runtime UUIDs must be canonical because routes and state keys use them as
// stable identities rather than display values.
func validateRuntimeUUID(codespaceUUID string) error {
	parsed, err := uuid.Parse(codespaceUUID)
	if err != nil {
		return err
	}
	if parsed[6]>>4 != 4 || parsed.String() != codespaceUUID {
		return fmt.Errorf("codespace uuid must be canonical lower-case UUID v4")
	}
	return nil
}
