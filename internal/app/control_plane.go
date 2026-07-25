// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import "strings"

const managerServicePath = "/api/codespace"

func managerServiceBaseURL(giteaURL string) string {
	return strings.TrimRight(strings.TrimSpace(giteaURL), "/") + managerServicePath
}
