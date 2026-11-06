// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/distribution/reference"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const (
	maxInjectedFeatures     = 32
	maxFeatureOptionsBytes  = 16 * 1024
	maxLifecycleScriptBytes = 64 * 1024
	webIDEFeatureReference  = "ghcr.io/coder/devcontainer-features/code-server:2"
)

// Configuration defines administrator-owned additions to a repository's Dev
// Container configuration. It is copied into the Codespace runtime snapshot.
type Configuration struct {
	WebIDE   WebIDEConfiguration `json:"webIDE"`
	Features []InjectedFeature   `json:"features,omitempty"`
	// +kubebuilder:validation:MaxLength=65536
	LifecycleScript string `json:"lifecycleScript,omitempty"`
}

// WebIDEConfiguration defines the platform-managed code-server installation.
type WebIDEConfiguration struct {
	Enabled bool `json:"enabled"`
	// Options are passed to the code-server Feature. Codespace overwrites the
	// options that control its internal listener, authentication, and lifecycle.
	Options apiextensionsv1.JSON `json:"options"`
}

// InjectedFeature adds one standard Dev Container Feature to an environment.
type InjectedFeature struct {
	Reference string               `json:"reference"`
	Options   apiextensionsv1.JSON `json:"options,omitempty"`
}

// Validate checks that the configuration can be merged deterministically.
func (c Configuration) Validate() error {
	if len(c.LifecycleScript) > maxLifecycleScriptBytes || strings.ContainsRune(c.LifecycleScript, '\x00') {
		return fmt.Errorf("lifecycle script must be no larger than 64 KiB and cannot contain NUL characters")
	}
	seen := make(map[string]string, len(c.Features)+1)
	if len(bytes.TrimSpace(c.WebIDE.Options.Raw)) == 0 {
		return fmt.Errorf("options for Web IDE are required")
	}
	if err := validateFeatureOptions("Web IDE", c.WebIDE.Options.Raw); err != nil {
		return err
	}
	if c.WebIDE.Enabled {
		id, err := validateFeatureReference(webIDEFeatureReference)
		if err != nil {
			return fmt.Errorf("platform Web IDE Feature: %w", err)
		}
		seen[id] = webIDEFeatureReference
	}
	if len(c.Features) > maxInjectedFeatures {
		return fmt.Errorf("environment supports at most %d injected Features", maxInjectedFeatures)
	}
	for _, feature := range c.Features {
		id, err := validateFeatureReference(feature.Reference)
		if err != nil {
			return err
		}
		if existing, exists := seen[id]; exists {
			return fmt.Errorf("features %q and %q have the same identity", existing, feature.Reference)
		}
		seen[id] = feature.Reference
		if err := validateFeatureOptions(fmt.Sprintf("feature %q", feature.Reference), feature.Options.Raw); err != nil {
			return err
		}
	}
	return nil
}

func validateFeatureOptions(name string, raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > maxFeatureOptionsBytes || !json.Valid(raw) || raw[0] != '{' {
		return fmt.Errorf("options for %s must be a JSON object no larger than 16 KiB", name)
	}
	return nil
}

func validateFeatureReference(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 {
		return "", fmt.Errorf("invalid Dev Container Feature reference: value is required and must be at most 512 characters")
	}
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return "", fmt.Errorf("invalid Dev Container Feature reference %q: %w", value, err)
	}
	if _, tagged := named.(reference.NamedTagged); !tagged {
		if _, digested := named.(reference.Digested); !digested {
			return "", fmt.Errorf("invalid Dev Container Feature reference %q: tag or digest is required", value)
		}
	}
	return reference.TrimNamed(named).String(), nil
}

// DeepCopyInto supports use of Configuration in Kubernetes API objects.
func (c *Configuration) DeepCopyInto(out *Configuration) {
	*out = *c
	c.WebIDE.Options.DeepCopyInto(&out.WebIDE.Options)
	if c.Features != nil {
		out.Features = make([]InjectedFeature, len(c.Features))
		for i := range c.Features {
			out.Features[i] = c.Features[i]
			c.Features[i].Options.DeepCopyInto(&out.Features[i].Options)
		}
	}
}
