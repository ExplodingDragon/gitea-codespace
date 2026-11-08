// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/distribution/reference"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const (
	maxInjectedFeatures     = 32
	maxJSONObjectBytes      = 16 * 1024
	maxLifecycleScriptBytes = 64 * 1024
	maxWebIDEArchiveURL     = 4096
	maxWebIDEExtensions     = 256
)

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
	Enabled bool `json:"enabled"`
	// ArchiveURL optionally replaces the platform Web IDE program. The Runtime
	// substitutes {arch} with the architecture shared by its Pod and Dev Container.
	// +kubebuilder:validation:MaxLength=4096
	ArchiveURL string               `json:"archiveURL,omitempty"`
	Settings   apiextensionsv1.JSON `json:"settings"`
	// +kubebuilder:validation:MaxItems=256
	Extensions []string `json:"extensions,omitempty"`
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
	if len(bytes.TrimSpace(c.WebIDE.Settings.Raw)) == 0 {
		return fmt.Errorf("settings for Web IDE are required")
	}
	if err := validateJSONObject("Web IDE settings", c.WebIDE.Settings.Raw); err != nil {
		return err
	}
	archiveURL := strings.TrimSpace(c.WebIDE.ArchiveURL)
	if len(archiveURL) > maxWebIDEArchiveURL {
		return fmt.Errorf("web IDE archive URL must be at most %d characters", maxWebIDEArchiveURL)
	}
	if archiveURL != "" {
		resolved := strings.ReplaceAll(archiveURL, "{arch}", "amd64")
		if strings.ContainsAny(resolved, "{}") {
			return fmt.Errorf("web IDE archive URL contains an unknown placeholder")
		}
		endpoint, err := url.Parse(resolved)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.Fragment != "" {
			return fmt.Errorf("web IDE archive URL must be an absolute HTTP(S) URL without a fragment")
		}
	}
	if len(c.WebIDE.Extensions) > maxWebIDEExtensions {
		return fmt.Errorf("web IDE supports at most %d default extensions", maxWebIDEExtensions)
	}
	extensionIDs := make(map[string]struct{}, len(c.WebIDE.Extensions))
	for _, extension := range c.WebIDE.Extensions {
		extension = strings.TrimSpace(extension)
		if extension == "" || len(extension) > 255 || strings.ContainsAny(extension, " \t\r\n") {
			return fmt.Errorf("web IDE extension %q is empty, too long, or contains whitespace", extension)
		}
		identity := strings.ToLower(strings.SplitN(extension, "@", 2)[0])
		if _, exists := extensionIDs[identity]; exists {
			return fmt.Errorf("web IDE extension %q is declared more than once", identity)
		}
		extensionIDs[identity] = struct{}{}
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
	c.WebIDE.Settings.DeepCopyInto(&out.WebIDE.Settings)
	out.WebIDE.Extensions = slices.Clone(c.WebIDE.Extensions)
	if c.Features != nil {
		out.Features = make([]InjectedFeature, len(c.Features))
		for i := range c.Features {
			out.Features[i] = c.Features[i]
			c.Features[i].Options.DeepCopyInto(&out.Features[i].Options)
		}
	}
}
