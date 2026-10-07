// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

type CachePolicy struct {
	Public bool `json:"public"`
	Build  bool `json:"build"`
}
