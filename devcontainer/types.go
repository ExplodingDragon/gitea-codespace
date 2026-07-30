// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultWorkspaceFolder is the conventional container-side workspace root.
const DefaultWorkspaceFolder = "/workspaces"

// LifecycleStage identifies one command stage in the Dev Container lifecycle.
type LifecycleStage string

const (
	// LifecycleStageInitialize is ready immediately after initializeCommand.
	LifecycleStageInitialize LifecycleStage = "initializeCommand"
	// LifecycleStageOnCreate is ready after onCreateCommand.
	LifecycleStageOnCreate LifecycleStage = "onCreateCommand"
	// LifecycleStageUpdateContent is ready after updateContentCommand.
	LifecycleStageUpdateContent LifecycleStage = "updateContentCommand"
	// LifecycleStagePostCreate is ready after postCreateCommand.
	LifecycleStagePostCreate LifecycleStage = "postCreateCommand"
	// LifecycleStagePostStart is ready after postStartCommand.
	LifecycleStagePostStart LifecycleStage = "postStartCommand"
)

// Configuration contains the Dev Container properties used to create an environment.
type Configuration struct {
	Schema                      string                     `json:"$schema,omitempty"`
	Name                        string                     `json:"name,omitempty"`
	Image                       string                     `json:"image,omitempty"`
	Build                       *Build                     `json:"build,omitempty"`
	DockerFile                  string                     `json:"dockerFile,omitempty"`
	Context                     string                     `json:"context,omitempty"`
	DockerComposeFile           StringList                 `json:"dockerComposeFile,omitempty"`
	Service                     string                     `json:"service,omitempty"`
	RunServices                 []string                   `json:"runServices,omitempty"`
	WorkspaceFolder             string                     `json:"workspaceFolder,omitempty"`
	WorkspaceMount              string                     `json:"workspaceMount,omitempty"`
	Mounts                      []Mount                    `json:"mounts,omitempty"`
	ContainerEnv                map[string]string          `json:"containerEnv,omitempty"`
	RemoteEnv                   RemoteEnvironment          `json:"remoteEnv,omitempty"`
	ContainerUser               string                     `json:"containerUser,omitempty"`
	RemoteUser                  string                     `json:"remoteUser,omitempty"`
	UpdateRemoteUserUID         *bool                      `json:"updateRemoteUserUID,omitempty"`
	UserEnvProbe                string                     `json:"userEnvProbe,omitempty"`
	InitializeCommand           Command                    `json:"initializeCommand,omitempty"`
	OnCreateCommand             Command                    `json:"onCreateCommand,omitempty"`
	UpdateContentCommand        Command                    `json:"updateContentCommand,omitempty"`
	PostCreateCommand           Command                    `json:"postCreateCommand,omitempty"`
	PostStartCommand            Command                    `json:"postStartCommand,omitempty"`
	PostAttachCommand           Command                    `json:"postAttachCommand,omitempty"`
	WaitFor                     LifecycleStage             `json:"waitFor,omitempty"`
	ShutdownAction              string                     `json:"shutdownAction,omitempty"`
	Features                    map[string]json.RawMessage `json:"features,omitempty"`
	OverrideFeatureInstallOrder []string                   `json:"overrideFeatureInstallOrder,omitempty"`
	Customizations              map[string]json.RawMessage `json:"customizations,omitempty"`
	ForwardPorts                []Port                     `json:"forwardPorts,omitempty"`
	PortsAttributes             map[string]PortAttributes  `json:"portsAttributes,omitempty"`
	OtherPortsAttributes        *PortAttributes            `json:"otherPortsAttributes,omitempty"`
	AppPort                     AppPortList                `json:"appPort,omitempty"`
	RunArgs                     []string                   `json:"runArgs,omitempty"`
	Init                        bool                       `json:"init,omitempty"`
	Privileged                  bool                       `json:"privileged,omitempty"`
	CapAdd                      []string                   `json:"capAdd,omitempty"`
	SecurityOpt                 []string                   `json:"securityOpt,omitempty"`
	OverrideCommand             *bool                      `json:"overrideCommand,omitempty"`
	HostRequirements            HostRequirements           `json:"hostRequirements,omitempty"`
	Secrets                     map[string]Secret          `json:"secrets,omitempty"`
}

// RemoteEnvironment preserves explicit null values until inherited process
// variables can be removed from the final remote environment.
type RemoteEnvironment map[string]*string

// Mount is one bind, volume, or tmpfs mount accepted by the Dev Container specification.
type Mount struct {
	Type            string `json:"type"`
	Source          string `json:"source"`
	Target          string `json:"target"`
	Consistency     string `json:"consistency"`
	ReadOnly        bool   `json:"readonly"`
	BindPropagation string `json:"bindPropagation,omitempty"`
	VolumeNoCopy    bool   `json:"volumeNoCopy,omitempty"`
}

// UnmarshalJSON accepts the string and object mount forms defined by the specification.
func (m *Mount) UnmarshalJSON(data []byte) error {
	*m = Mount{}
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		for _, item := range strings.Split(value, ",") {
			name, setting, hasSetting := strings.Cut(strings.TrimSpace(item), "=")
			switch strings.TrimSpace(name) {
			case "type", "source", "src", "target", "dst", "destination", "consistency", "bind-propagation":
				if !hasSetting {
					return fmt.Errorf("mount item %q requires a value", item)
				}
				setting = strings.TrimSpace(setting)
				switch strings.TrimSpace(name) {
				case "type":
					m.Type = setting
				case "source", "src":
					m.Source = setting
				case "target", "dst", "destination":
					m.Target = setting
				case "consistency":
					m.Consistency = setting
				case "bind-propagation":
					m.BindPropagation = setting
				}
			case "readonly", "ro":
				m.ReadOnly = !hasSetting || setting == "" || strings.EqualFold(strings.TrimSpace(setting), "true")
			case "volume-nocopy":
				m.VolumeNoCopy = !hasSetting || setting == "" || strings.EqualFold(strings.TrimSpace(setting), "true")
			default:
				return fmt.Errorf("mount setting %q is unsupported", name)
			}
		}
		return m.Validate()
	}
	type plain Mount
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*plain)(m)); err != nil {
		return fmt.Errorf("mount must be a string or object: %w", err)
	}
	return m.Validate()
}

// Validate checks mount type-specific fields before they reach the container engine.
func (m Mount) Validate() error {
	switch m.Consistency {
	case "", "default", "consistent", "cached", "delegated":
	default:
		return fmt.Errorf("mount consistency %q is invalid", m.Consistency)
	}
	switch m.Type {
	case "bind":
		if strings.TrimSpace(m.Source) == "" {
			return fmt.Errorf("bind mount source is required")
		}
		if m.VolumeNoCopy {
			return fmt.Errorf("bind mount does not accept volume-nocopy")
		}
		switch m.BindPropagation {
		case "", "rprivate", "private", "rshared", "shared", "rslave", "slave":
		default:
			return fmt.Errorf("bind propagation %q is invalid", m.BindPropagation)
		}
	case "volume":
		if m.BindPropagation != "" {
			return fmt.Errorf("volume mount does not accept bind-propagation")
		}
	case "tmpfs":
		if m.Source != "" {
			return fmt.Errorf("tmpfs mount does not accept a source")
		}
		if m.BindPropagation != "" || m.VolumeNoCopy {
			return fmt.Errorf("tmpfs mount contains options for another mount type")
		}
	default:
		return fmt.Errorf("mount type %q is invalid", m.Type)
	}
	if !filepath.IsAbs(strings.TrimSpace(m.Target)) {
		return fmt.Errorf("mount target must be absolute")
	}
	return nil
}

// Build describes a Dockerfile build declared by a Dev Container configuration.
type Build struct {
	Dockerfile string            `json:"dockerfile,omitempty"`
	Context    string            `json:"context,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	Target     string            `json:"target,omitempty"`
	CacheFrom  StringList        `json:"cacheFrom,omitempty"`
	Options    []string          `json:"options,omitempty"`
}

// PortAttributes describes how a forwarded port is presented to a caller.
type PortAttributes struct {
	Label            string `json:"label"`
	Protocol         string `json:"protocol"`
	OnAutoForward    string `json:"onAutoForward"`
	RequireLocalPort bool   `json:"requireLocalPort"`
	ElevateIfNeeded  bool   `json:"elevateIfNeeded"`
}

// HostRequirements contains the minimum resources requested by a Dev Container.
type HostRequirements struct {
	CPUs    float64         `json:"cpus"`
	Memory  string          `json:"memory"`
	Storage string          `json:"storage"`
	GPU     json.RawMessage `json:"gpu"`
}

// Secret documents one environment secret expected by a Dev Container.
type Secret struct {
	Description      string `json:"description"`
	DocumentationURL string `json:"documentationUrl"`
}

// Source selects a repository configuration or a caller-provided default image.
type Source struct {
	Path          string `json:"path,omitempty"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	DefaultImage  string `json:"default_image,omitempty"`
}

// LoadOptions controls configuration loading and the caller's permitted path boundary.
type LoadOptions struct {
	Workspace       string
	Source          Source
	LocalEnv        map[string]string
	ID              string
	AllowedPathRoot string
}

// ResolvedConfiguration contains immutable paths and values used to create an environment.
type ResolvedConfiguration struct {
	Configuration
	Workspace                 string              `json:"workspace"`
	ConfigurationPath         string              `json:"configuration_path"`
	ConfigurationDir          string              `json:"configuration_dir"`
	ContentSHA256             string              `json:"content_sha256"`
	DevContainerID            string              `json:"dev_container_id"`
	LocalEnvironment          map[string]string   `json:"-"`
	AllowedPathRoot           string              `json:"-"`
	Synthetic                 bool                `json:"-"`
	FrozenLockfile            bool                `json:"-"`
	InjectedFeatureReferences map[string]struct{} `json:"-"`
	FeatureEntrypoints        []string            `json:"-"`
}

// InjectedFeature is a caller-owned Feature merged with repository configuration for one create.
type InjectedFeature struct {
	Reference string                     `json:"reference"`
	Origin    string                     `json:"origin"`
	Options   map[string]json.RawMessage `json:"options,omitempty"`
}

// StringList accepts either one string or an array of strings.
type StringList []string

// UnmarshalJSON accepts one string or an array of strings.
func (s *StringList) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("must be a string or string array")
	}
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		if strings.TrimSpace(one) == "" {
			return fmt.Errorf("must not contain an empty string")
		}
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("must be a string or string array")
	}
	for _, value := range many {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("must not contain an empty string")
		}
	}
	*s = many
	return nil
}

// AppPortList accepts the scalar and array forms of appPort.
type AppPortList []Port

// UnmarshalJSON decodes one application port or an array of application ports.
func (p *AppPortList) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("appPort must be a port or array of ports")
	}
	var values []Port
	if err := json.Unmarshal(data, &values); err == nil {
		*p = values
		return nil
	}
	var value Port
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("appPort must be a port or array of ports: %w", err)
	}
	*p = []Port{value}
	return nil
}

// Port accepts a numeric port or a service/port string.
type Port struct {
	Number  uint16
	Address string
}

// UnmarshalJSON accepts a numeric port or a host-and-port string.
func (p *Port) UnmarshalJSON(data []byte) error {
	*p = Port{}
	var number uint16
	if err := json.Unmarshal(data, &number); err == nil && number > 0 {
		p.Number = number
		return nil
	}
	var address string
	if err := json.Unmarshal(data, &address); err != nil || strings.TrimSpace(address) == "" {
		return fmt.Errorf("port must be a positive number or non-empty string")
	}
	p.Address = strings.TrimSpace(address)
	return nil
}

// MarshalJSON preserves the numeric or host-and-port representation.
func (p Port) MarshalJSON() ([]byte, error) {
	if p.Number != 0 {
		return json.Marshal(p.Number)
	}
	return json.Marshal(p.Address)
}

// ContainerPort returns the container-side port from a numeric value or a
// Docker-style address mapping.
func (p Port) ContainerPort() (uint16, error) {
	if p.Number != 0 {
		return p.Number, nil
	}
	parts := strings.Split(strings.TrimSpace(p.Address), ":")
	raw := strings.TrimSpace(parts[len(parts)-1])
	value, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("port %q is invalid", p.Address)
	}
	return uint16(value), nil
}

// Command stores one lifecycle command in string, argv, or named-command form.
type Command struct {
	Value json.RawMessage
}

// UnmarshalJSON validates and stores a lifecycle command in any supported form.
func (c *Command) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		c.Value = nil
		return nil
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch value := value.(type) {
	case string:
	case []any:
		for _, item := range value {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("command array must contain strings")
			}
		}
	case map[string]any:
		for _, item := range value {
			switch item := item.(type) {
			case string:
			case []any:
				for _, argument := range item {
					if _, ok := argument.(string); !ok {
						return fmt.Errorf("parallel command array must contain strings")
					}
				}
			default:
				return fmt.Errorf("parallel command must be a string or string array")
			}
		}
	default:
		return fmt.Errorf("command must be a string, string array, or object")
	}
	c.Value = append(c.Value[:0], data...)
	return nil
}

// MarshalJSON returns the original lifecycle command representation.
func (c Command) MarshalJSON() ([]byte, error) {
	if len(c.Value) == 0 {
		return []byte("null"), nil
	}
	return c.Value, nil
}
