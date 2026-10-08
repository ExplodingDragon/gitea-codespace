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
		{name: "default", configuration: DefaultConfiguration()},
		{
			name: "duplicate feature identity",
			configuration: Configuration{Features: []InjectedFeature{
				{Reference: "ghcr.io/example/features/tool:1"},
				{Reference: "ghcr.io/example/features/tool:2"},
			}},
			wantError: "same identity",
		},
		{
			name:          "disabled web IDE has configuration",
			configuration: Configuration{WebIDE: WebIDEConfiguration{Version: "4.121.0"}},
			wantError:     "must be empty",
		},
		{
			name: "feature options are an object",
			configuration: Configuration{Features: []InjectedFeature{{
				Reference: "ghcr.io/example/features/tool:1",
				Options:   apiextensionsv1.JSON{Raw: []byte(`true`)},
			}}},
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
