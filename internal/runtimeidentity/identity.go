// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimeidentity

import (
	"strconv"
	"strings"
)

// Key scopes a Gitea numeric ID to its configured site.
func Key(siteUID string, codespaceID int64) string {
	return siteUID + "/" + strconv.FormatInt(codespaceID, 10)
}

func Parse(key string) (siteUID string, codespaceID int64, valid bool) {
	siteUID, raw, found := strings.Cut(key, "/")
	codespaceID, err := strconv.ParseInt(raw, 10, 64)
	return siteUID, codespaceID, found && siteUID != "" && err == nil && codespaceID > 0 && Key(siteUID, codespaceID) == key
}
