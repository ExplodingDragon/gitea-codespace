// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gitea.dev/codespace/internal/devcontainer"
)

var endpointIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,28}[a-z0-9])?$`)

func runEndpoint(args []string) error {
	if len(args) < 2 || (args[0] != "set" && args[0] != "delete") {
		return fmt.Errorf("usage: endpoint set <id> <label> <http|https> <port> [--public] | endpoint delete <id>")
	}
	id := args[1]
	if id == devcontainer.WorkspaceEndpointID || !endpointIDPattern.MatchString(id) {
		return fmt.Errorf("endpoint id %q is invalid", id)
	}
	manifest := devcontainer.EndpointManifest{Version: devcontainer.EndpointManifestVersion}
	if err := readJSON(devcontainer.EndpointManifestPath, &manifest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read endpoint manifest: %w", err)
	}
	if manifest.Version != devcontainer.EndpointManifestVersion {
		return fmt.Errorf("endpoint manifest version %d is invalid", manifest.Version)
	}
	manifest.Endpoints = slices.DeleteFunc(manifest.Endpoints, func(endpoint devcontainer.Endpoint) bool {
		return endpoint.EndpointID == id
	})
	if args[0] == "set" {
		if len(args) != 5 && len(args) != 6 {
			return fmt.Errorf("endpoint set requires id, label, scheme, and port")
		}
		if args[3] != "http" && args[3] != "https" {
			return fmt.Errorf("endpoint scheme must be http or https")
		}
		label := strings.TrimSpace(args[2])
		if label == "" || !utf8.ValidString(label) || utf8.RuneCountInString(label) > 64 || strings.ContainsFunc(label, func(r rune) bool { return unicode.IsControl(r) || r == '<' || r == '>' }) {
			return fmt.Errorf("endpoint label is invalid")
		}
		port, err := strconv.ParseUint(args[4], 10, 16)
		if err != nil || port == 0 {
			return fmt.Errorf("endpoint port is invalid")
		}
		public := false
		if len(args) == 6 {
			if args[5] != "--public" {
				return fmt.Errorf("endpoint option %q is invalid", args[5])
			}
			public = true
		}
		manifest.Endpoints = append(manifest.Endpoints, devcontainer.Endpoint{
			EndpointID: id, Label: label, UpstreamScheme: args[3], UpstreamPort: int(port), Public: public,
		})
	}
	if len(manifest.Endpoints) > devcontainer.MaxDeclaredEndpointCount {
		return fmt.Errorf("endpoint manifest exceeds limit %d", devcontainer.MaxDeclaredEndpointCount)
	}
	slices.SortFunc(manifest.Endpoints, func(a, b devcontainer.Endpoint) int {
		if a.EndpointID < b.EndpointID {
			return -1
		}
		if a.EndpointID > b.EndpointID {
			return 1
		}
		return 0
	})
	return writeJSONAtomic(devcontainer.EndpointManifestPath, manifest)
}
