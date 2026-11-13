// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logbuffer

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var (
	authorizationPattern  = regexp.MustCompile(`(?i)\b((?:bearer|basic)\s+)[A-Za-z0-9._~+/=-]+`)
	urlCredentialsPattern = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
)

// Redactor removes known credentials and common authorization syntax before
// output reaches either administrative logs or the operation log buffer.
type Redactor struct{ replacements *strings.Replacer }

func NewRedactor(credentials []string) *Redactor {
	values := make(map[string]struct{})
	for _, credential := range credentials {
		parts := strings.FieldsFunc(credential, func(r rune) bool { return r == '\r' || r == '\n' })
		for _, value := range append(parts, credential) {
			if value == "" {
				continue
			}
			values[value] = struct{}{}
			values[url.QueryEscape(value)] = struct{}{}
			values[base64.StdEncoding.EncodeToString([]byte(value))] = struct{}{}
			// Kubernetes and Docker diagnostics encode status messages as JSON.
			encoded, _ := json.Marshal(value)
			values[string(encoded[1:len(encoded)-1])] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(values))
	for value := range values {
		ordered = append(ordered, value)
	}
	slices.SortFunc(ordered, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(ordered))
	for _, value := range ordered {
		pairs = append(pairs, value, "[redacted]")
	}
	return &Redactor{replacements: strings.NewReplacer(pairs...)}
}

func (r *Redactor) Redact(message string) string {
	message = strings.ReplaceAll(message, "\r", "")
	if r != nil {
		message = r.replacements.Replace(message)
	}
	message = authorizationPattern.ReplaceAllString(message, "${1}[redacted]")
	message = urlCredentialsPattern.ReplaceAllString(message, "${1}[redacted]@")
	message = strings.ToValidUTF8(message, "\ufffd")
	if len(message) > MaxLineBytes {
		return "[WARN] [logging] Output line omitted: redacted line exceeds 32 KiB"
	}
	return message
}
