// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package runtimeevent defines the public in-instance event limits.
package runtimeevent

import (
	"fmt"
	"unicode/utf8"
)

const (
	MaxTypeBytes    = 48
	MaxMessageBytes = 16 * 1024
)

func Validate(eventType, message string) error {
	if len(eventType) == 0 || len(eventType) > MaxTypeBytes {
		return fmt.Errorf("event type must contain 1 to %d ASCII bytes", MaxTypeBytes)
	}
	for _, c := range eventType {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return fmt.Errorf("event type may contain only letters, digits, dots, underscores and hyphens")
		}
	}
	if len(message) == 0 || len(message) > MaxMessageBytes || !utf8.ValidString(message) {
		return fmt.Errorf("event message must contain 1 to %d bytes of UTF-8 text", MaxMessageBytes)
	}
	return nil
}
