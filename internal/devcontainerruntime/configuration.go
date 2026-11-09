// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const (
	maxInjectedFeatures     = 32
	maxJSONObjectBytes      = 16 * 1024
	maxLifecycleScriptBytes = 64 * 1024
	maxWebIDEImageLength    = 4096
)

var webIDEProviderPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,30}[a-z0-9])?$`)

// Configuration defines administrator-owned additions to a repository's Dev
// Container configuration. It is copied into the Codespace runtime snapshot.
type Configuration struct {
	WebIDE WebIDEConfiguration `json:"webIDE"`
	// GiteaCLI adds the CLI bundled with the platform image to the Dev Container.
	GiteaCLI bool              `json:"giteaCLI"`
	Features []InjectedFeature `json:"features,omitempty"`
	// +kubebuilder:validation:MaxLength=65536
	LifecycleScript string `json:"lifecycleScript,omitempty"`
}

// WebIDEConfiguration defines the platform-managed Web IDE runtime and initial
// user configuration.
type WebIDEConfiguration struct {
	// Image is resolved by the Manager before this configuration is persisted.
	// +kubebuilder:validation:MaxLength=4096
	Image string `json:"image,omitempty"`
	// Provider is derived from the image metadata rather than selected separately.
	Provider WebIDEProvider `json:"provider,omitempty"`
	// Options are interpreted by the selected provider adapter.
	// +kubebuilder:pruning:PreserveUnknownFields
	Options apiextensionsv1.JSON `json:"options,omitempty"`
}

type WebIDEProvider string

func (c WebIDEConfiguration) Enabled() bool {
	return strings.TrimSpace(c.Image) != ""
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
	seen := make(map[string]string, len(c.Features))
	if err := c.WebIDE.validate(); err != nil {
		return err
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
		if err := validateJSONObject(fmt.Sprintf("feature %q options", feature.Reference), feature.Options.Raw); err != nil {
			return err
		}
	}
	return nil
}

func (c WebIDEConfiguration) validate() error {
	image := strings.TrimSpace(c.Image)
	provider := strings.TrimSpace(string(c.Provider))
	if image == "" {
		if provider != "" || len(bytes.TrimSpace(c.Options.Raw)) != 0 {
			return fmt.Errorf("disabled Web IDE configuration must be empty")
		}
		return nil
	}
	if err := ValidateWebIDEProvider(provider); err != nil {
		return err
	}
	if len(image) > maxWebIDEImageLength {
		return fmt.Errorf("web IDE image must be at most %d characters", maxWebIDEImageLength)
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("invalid Web IDE image: %w", err)
	}
	digested, ok := named.(reference.Digested)
	if !ok || digested.Digest().Algorithm() != digest.SHA256 {
		return fmt.Errorf("web IDE image must be pinned by sha256 digest")
	}
	return validateJSONObject("Web IDE options", c.Options.Raw)
}

// ValidateWebIDEProvider checks the stable provider identity shared by Manager
// configuration, Runtime snapshots, and provider bundle manifests.
func ValidateWebIDEProvider(provider string) error {
	if !webIDEProviderPattern.MatchString(provider) {
		return fmt.Errorf("web IDE provider must be a lowercase identifier of at most 32 characters")
	}
	return nil
}

func validateJSONObject(name string, raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > maxJSONObjectBytes || !json.Valid(raw) || raw[0] != '{' {
		return fmt.Errorf("%s must be a JSON object no larger than 16 KiB", name)
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
