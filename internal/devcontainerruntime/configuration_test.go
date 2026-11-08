// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestConfigurationValidation(t *testing.T) {
	tests := []struct {
		name          string
		configuration Configuration
		wantError     string
	}{
		{
			name: "disabled",
			configuration: Configuration{WebIDE: WebIDEConfiguration{
				Settings: apiextensionsv1.JSON{Raw: []byte(`{}`)},
			}},
		},
		{name: "settings required", configuration: Configuration{}, wantError: "settings for Web IDE are required"},
		{
			name: "duplicate feature identity",
			configuration: Configuration{
				WebIDE: WebIDEConfiguration{Settings: apiextensionsv1.JSON{Raw: []byte(`{}`)}},
				Features: []InjectedFeature{
					{Reference: "ghcr.io/example/features/tool:1"},
					{Reference: "ghcr.io/example/features/tool:2"},
				},
			},
			wantError: "same identity",
		},
		{
			name: "Web IDE settings are an object",
			configuration: Configuration{WebIDE: WebIDEConfiguration{
				Enabled: true, Settings: apiextensionsv1.JSON{Raw: []byte(`[]`)},
			}},
			wantError: "Web IDE settings must be a JSON object",
		},
		{
			name: "Web IDE accepts custom archive and extensions",
			configuration: Configuration{
				WebIDE: WebIDEConfiguration{
					Enabled: true, ArchiveURL: "https://editor.example/code-server-linux-{arch}.tar.gz",
					Settings: apiextensionsv1.JSON{Raw: []byte(`{"editor.tabSize":2}`)}, Extensions: []string{"example.tool"},
				},
			},
		},
		{
			name: "Web IDE rejects unknown archive placeholder",
			configuration: Configuration{WebIDE: WebIDEConfiguration{
				Settings: apiextensionsv1.JSON{Raw: []byte(`{}`)}, ArchiveURL: "https://editor.example/{version}/{arch}.tar.gz",
			}},
			wantError: "unknown placeholder",
		},
		{
			name: "Web IDE rejects duplicate extension identity",
			configuration: Configuration{WebIDE: WebIDEConfiguration{
				Settings: apiextensionsv1.JSON{Raw: []byte(`{}`)}, Extensions: []string{"example.tool@1", "EXAMPLE.TOOL@2"},
			}},
			wantError: "declared more than once",
		},
		{
			name: "feature options are an object",
			configuration: Configuration{
				WebIDE: WebIDEConfiguration{Settings: apiextensionsv1.JSON{Raw: []byte(`{}`)}},
				Features: []InjectedFeature{{
					Reference: "ghcr.io/example/features/tool:1",
					Options:   apiextensionsv1.JSON{Raw: []byte(`true`)},
				}},
			},
			wantError: "must be a JSON object",
		},
		{
			name: "lifecycle script is bounded",
			configuration: Configuration{
				WebIDE:          WebIDEConfiguration{Settings: apiextensionsv1.JSON{Raw: []byte(`{}`)}},
				LifecycleScript: strings.Repeat("x", maxLifecycleScriptBytes+1),
			},
			wantError: "lifecycle script",
		},
		{
			name: "lifecycle script rejects NUL",
			configuration: Configuration{
				WebIDE:          WebIDEConfiguration{Settings: apiextensionsv1.JSON{Raw: []byte(`{}`)}},
				LifecycleScript: "echo before\x00after",
			},
			wantError: "lifecycle script",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.configuration.Validate()
			if test.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantError)
		})
	}
}
