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

const maxInjectedFeatures = 32

// Configuration defines administrator-owned additions to a repository's Dev
// Container configuration. It is copied into the Codespace runtime snapshot.
type Configuration struct {
	WebIDE   WebIDEConfiguration `json:"webIDE"`
	Features []InjectedFeature   `json:"features,omitempty"`
}

// WebIDEConfiguration defines the platform-managed code-server installation.
type WebIDEConfiguration struct {
	Enabled    bool     `json:"enabled"`
	Feature    string   `json:"feature,omitempty"`
	Version    string   `json:"version,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
}

// InjectedFeature adds one standard Dev Container Feature to an environment.
type InjectedFeature struct {
	Reference string               `json:"reference"`
	Options   apiextensionsv1.JSON `json:"options,omitempty"`
}

// Validate checks that the configuration can be merged deterministically.
func (c Configuration) Validate() error {
	seen := make(map[string]string, len(c.Features)+1)
	if c.WebIDE.Enabled {
		if strings.TrimSpace(c.WebIDE.Version) == "" || len(c.WebIDE.Version) > 128 {
			return fmt.Errorf("web IDE requires a version of at most 128 characters")
		}
		id, err := validateFeatureReference(c.WebIDE.Feature)
		if err != nil {
			return fmt.Errorf("web IDE Feature: %w", err)
		}
		seen[id] = c.WebIDE.Feature
	} else if c.WebIDE.Feature != "" || c.WebIDE.Version != "" || len(c.WebIDE.Extensions) != 0 {
		return fmt.Errorf("disabled web IDE configuration must be empty")
	}
	if len(c.WebIDE.Extensions) > 128 {
		return fmt.Errorf("web IDE supports at most 128 extensions")
	}
	extensions := make(map[string]struct{}, len(c.WebIDE.Extensions))
	for _, value := range c.WebIDE.Extensions {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 255 || strings.ContainsAny(value, " \t\r\n") {
			return fmt.Errorf("web IDE extension identifiers must be non-empty and contain no whitespace")
		}
		if _, exists := extensions[value]; exists {
			return fmt.Errorf("web IDE extension %q is duplicated", value)
		}
		extensions[value] = struct{}{}
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
		if len(feature.Options.Raw) == 0 {
			continue
		}
		if len(feature.Options.Raw) > 16*1024 || !json.Valid(feature.Options.Raw) || !bytes.HasPrefix(bytes.TrimSpace(feature.Options.Raw), []byte("{")) {
			return fmt.Errorf("feature %q options must be a JSON object no larger than 16 KiB", feature.Reference)
		}
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
	out.WebIDE.Extensions = append([]string(nil), c.WebIDE.Extensions...)
	if c.Features != nil {
		out.Features = make([]InjectedFeature, len(c.Features))
		for i := range c.Features {
			out.Features[i] = c.Features[i]
			c.Features[i].Options.DeepCopyInto(&out.Features[i].Options)
		}
	}
}
