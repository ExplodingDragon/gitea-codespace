// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
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
				Options: apiextensionsv1.JSON{Raw: []byte(`{}`)},
			}},
		},
		{name: "options required", configuration: Configuration{}, wantError: "options are required"},
		{
			name: "duplicate feature identity",
			configuration: Configuration{
				WebIDE: WebIDEConfiguration{Options: apiextensionsv1.JSON{Raw: []byte(`{}`)}},
				Features: []InjectedFeature{
					{Reference: "ghcr.io/example/features/tool:1"},
					{Reference: "ghcr.io/example/features/tool:2"},
				},
			},
			wantError: "same identity",
		},
		{
			name: "web IDE options are an object",
			configuration: Configuration{WebIDE: WebIDEConfiguration{
				Enabled: true, Options: apiextensionsv1.JSON{Raw: []byte(`[]`)},
			}},
			wantError: "options must be a JSON object",
		},
		{
			name: "web IDE accepts upstream options",
			configuration: Configuration{WebIDE: WebIDEConfiguration{
				Enabled: true, Options: apiextensionsv1.JSON{Raw: []byte(`{"version":"4.121.0","extensions":"example.tool"}`)},
			}},
		},
		{
			name: "feature options are an object",
			configuration: Configuration{
				WebIDE: WebIDEConfiguration{Options: apiextensionsv1.JSON{Raw: []byte(`{}`)}},
				Features: []InjectedFeature{{
					Reference: "ghcr.io/example/features/tool:1",
					Options:   apiextensionsv1.JSON{Raw: []byte(`true`)},
				}},
			},
			wantError: "must be a JSON object",
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
