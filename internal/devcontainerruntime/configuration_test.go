// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestConfigurationDeepCopy(t *testing.T) {
	source := Configuration{
		WebIDE:   WebIDEConfiguration{Provider: "dsh", Options: apiextensionsv1.JSON{Raw: []byte(`{"plugins":["example.plugin"]}`)}},
		Features: []InjectedFeature{{Reference: "ghcr.io/example/features/tool:1", Options: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}}},
	}
	clone := Configuration{}
	source.DeepCopyInto(&clone)
	clone.WebIDE.Options.Raw[2] = 'x'
	clone.Features[0].Options.Raw[2] = 'x'
	require.True(t, json.Valid(source.WebIDE.Options.Raw))
	require.True(t, json.Valid(source.Features[0].Options.Raw))
}

func TestConfigurationValidation(t *testing.T) {
	tests := []struct {
		name          string
		configuration Configuration
		wantError     string
	}{
		{name: "disabled", configuration: Configuration{}},
		{
			name: "enabled provider", configuration: Configuration{WebIDE: WebIDEConfiguration{
				Provider: "vscode",
				Image:    "ghcr.io/example/web-ide-vscode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Options:  apiextensionsv1.JSON{Raw: []byte(`{"extensions":["example.tool"]}`)},
			}},
		},
		{
			name: "disabled configuration has no provider", configuration: Configuration{WebIDE: WebIDEConfiguration{Provider: "vscode"}},
			wantError: "must be empty",
		},
		{
			name: "provider identity", configuration: Configuration{WebIDE: WebIDEConfiguration{
				Provider: "Invalid Provider", Image: "ghcr.io/example/web-ide@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
			wantError: "lowercase identifier",
		},
		{
			name: "tagged image", configuration: Configuration{WebIDE: WebIDEConfiguration{Provider: "vscode", Image: "ghcr.io/example/web-ide:latest"}},
		},
		{
			name: "repository image", configuration: Configuration{WebIDE: WebIDEConfiguration{Provider: "vscode", Image: "ghcr.io/example/web-ide"}},
		},
		{
			name: "options are object", configuration: Configuration{WebIDE: WebIDEConfiguration{
				Provider: "dsh", Image: "ghcr.io/example/web-ide@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Options: apiextensionsv1.JSON{Raw: []byte(`[]`)},
			}},
			wantError: "must be a JSON object",
		},
		{
			name: "duplicate feature identity",
			configuration: Configuration{Features: []InjectedFeature{
				{Reference: "ghcr.io/example/features/tool:1"}, {Reference: "ghcr.io/example/features/tool:2"},
			}},
			wantError: "same identity",
		},
		{
			name: "feature options are an object",
			configuration: Configuration{Features: []InjectedFeature{{
				Reference: "ghcr.io/example/features/tool:1", Options: apiextensionsv1.JSON{Raw: []byte(`true`)},
			}}},
			wantError: "must be a JSON object",
		},
		{
			name:          "lifecycle script is bounded",
			configuration: Configuration{LifecycleScript: strings.Repeat("x", maxLifecycleScriptBytes+1)},
			wantError:     "lifecycle script",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.configuration.Validate()
			if test.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.wantError)
			}
		})
	}
}
