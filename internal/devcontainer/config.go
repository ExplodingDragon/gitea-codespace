// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tailscale/hujson"
)

const DefaultWorkspaceFolder = "/workspaces"

var variablePattern = regexp.MustCompile(`\$\{([^{}]+)\}`)

// Configuration contains the Dev Container properties used to create an environment.
type Configuration struct {
	Schema                      string                     `json:"$schema"`
	Name                        string                     `json:"name"`
	Image                       string                     `json:"image"`
	Build                       *Build                     `json:"build"`
	DockerFile                  string                     `json:"dockerFile"`
	Context                     string                     `json:"context"`
	DockerComposeFile           StringList                 `json:"dockerComposeFile"`
	Service                     string                     `json:"service"`
	RunServices                 []string                   `json:"runServices"`
	WorkspaceFolder             string                     `json:"workspaceFolder"`
	WorkspaceMount              string                     `json:"workspaceMount"`
	Mounts                      []Mount                    `json:"mounts"`
	ContainerEnv                map[string]string          `json:"containerEnv"`
	RemoteEnv                   map[string]string          `json:"remoteEnv"`
	ContainerUser               string                     `json:"containerUser"`
	RemoteUser                  string                     `json:"remoteUser"`
	UpdateRemoteUserUID         *bool                      `json:"updateRemoteUserUID"`
	UserEnvProbe                string                     `json:"userEnvProbe"`
	InitializeCommand           Command                    `json:"initializeCommand"`
	OnCreateCommand             Command                    `json:"onCreateCommand"`
	UpdateContentCommand        Command                    `json:"updateContentCommand"`
	PostCreateCommand           Command                    `json:"postCreateCommand"`
	PostStartCommand            Command                    `json:"postStartCommand"`
	PostAttachCommand           Command                    `json:"postAttachCommand"`
	WaitFor                     string                     `json:"waitFor"`
	ShutdownAction              string                     `json:"shutdownAction"`
	Features                    map[string]json.RawMessage `json:"features"`
	OverrideFeatureInstallOrder []string                   `json:"overrideFeatureInstallOrder"`
	Customizations              map[string]json.RawMessage `json:"customizations"`
	ForwardPorts                []Port                     `json:"forwardPorts"`
	PortsAttributes             map[string]PortAttributes  `json:"portsAttributes"`
	OtherPortsAttributes        PortAttributes             `json:"otherPortsAttributes"`
	AppPort                     json.RawMessage            `json:"appPort"`
	RunArgs                     []string                   `json:"runArgs"`
	Init                        bool                       `json:"init"`
	Privileged                  bool                       `json:"privileged"`
	CapAdd                      []string                   `json:"capAdd"`
	SecurityOpt                 []string                   `json:"securityOpt"`
	OverrideCommand             *bool                      `json:"overrideCommand"`
	HostRequirements            HostRequirements           `json:"hostRequirements"`
	Secrets                     map[string]Secret          `json:"secrets"`
}

// Mount is one bind, volume, or tmpfs mount accepted by the Dev Container specification.
type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	Consistency string `json:"consistency"`
}

func (m *Mount) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		for _, item := range strings.Split(value, ",") {
			name, setting, ok := strings.Cut(strings.TrimSpace(item), "=")
			if !ok {
				return fmt.Errorf("mount item %q must contain '='", item)
			}
			switch strings.TrimSpace(name) {
			case "type":
				m.Type = strings.TrimSpace(setting)
			case "source", "src":
				m.Source = strings.TrimSpace(setting)
			case "target", "dst", "destination":
				m.Target = strings.TrimSpace(setting)
			case "consistency":
				m.Consistency = strings.TrimSpace(setting)
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

func (m Mount) Validate() error {
	switch m.Type {
	case "bind":
		if strings.TrimSpace(m.Source) == "" {
			return fmt.Errorf("bind mount source is required")
		}
	case "volume":
	case "tmpfs":
		if m.Source != "" {
			return fmt.Errorf("tmpfs mount does not accept a source")
		}
	default:
		return fmt.Errorf("mount type %q is invalid", m.Type)
	}
	if !filepath.IsAbs(strings.TrimSpace(m.Target)) {
		return fmt.Errorf("mount target must be absolute")
	}
	return nil
}

type Build struct {
	Dockerfile string            `json:"dockerfile"`
	Context    string            `json:"context"`
	Args       map[string]string `json:"args"`
	Target     string            `json:"target"`
	CacheFrom  StringList        `json:"cacheFrom"`
	Options    []string          `json:"options"`
}

type PortAttributes struct {
	Label            string `json:"label"`
	Protocol         string `json:"protocol"`
	OnAutoForward    string `json:"onAutoForward"`
	RequireLocalPort bool   `json:"requireLocalPort"`
	ElevateIfNeeded  bool   `json:"elevateIfNeeded"`
}

type HostRequirements struct {
	CPUs    float64         `json:"cpus"`
	Memory  string          `json:"memory"`
	Storage string          `json:"storage"`
	GPU     json.RawMessage `json:"gpu"`
}

type Secret struct {
	Description      string `json:"description"`
	DocumentationURL string `json:"documentationUrl"`
}

// Selection identifies the configuration fixed by Gitea for one Codespace.
type Selection struct {
	Source        string `json:"source"`
	Path          string `json:"path,omitempty"`
	CommitSHA     string `json:"commit_sha,omitempty"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	DefaultImage  string `json:"default_image,omitempty"`
}

// ResolvedConfiguration is the immutable input used by the runtime engine.
type ResolvedConfiguration struct {
	Configuration
	Workspace         string `json:"workspace"`
	ConfigurationPath string `json:"configuration_path"`
	ConfigurationDir  string `json:"configuration_dir"`
	ContentSHA256     string `json:"content_sha256"`
	DevContainerID    string `json:"dev_container_id"`
}

func Load(workspace string, selection Selection, localEnv map[string]string, devContainerID string) (*ResolvedConfiguration, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("workspace is empty")
	}
	workspace, err := filepath.Abs(strings.TrimSpace(workspace))
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace links: %w", err)
	}
	var content []byte
	var configPath string
	switch selection.Source {
	case "platform_default":
		image := strings.TrimSpace(selection.DefaultImage)
		if image == "" {
			return nil, fmt.Errorf("platform default image is empty")
		}
		if selection.Path != "" || selection.CommitSHA != "" || selection.ContentSHA256 != "" {
			return nil, fmt.Errorf("platform default contains repository fields")
		}
		content, err = json.Marshal(Configuration{Name: "Gitea Codespace", Image: image})
		configPath = filepath.Join(workspace, ".gitea-codespace-default.json")
	case "repository":
		if strings.TrimSpace(selection.DefaultImage) != "" {
			return nil, fmt.Errorf("repository configuration contains a default image")
		}
		if err := validateRepositoryPath(selection.Path); err != nil {
			return nil, err
		}
		configPath = filepath.Join(workspace, filepath.FromSlash(selection.Path))
		if !pathWithin(workspace, configPath) {
			return nil, fmt.Errorf("Dev Container configuration leaves workspace")
		}
		resolvedPath, resolveErr := filepath.EvalSymlinks(configPath)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve Dev Container configuration links: %w", resolveErr)
		}
		if !pathWithin(workspace, resolvedPath) {
			return nil, fmt.Errorf("Dev Container configuration link leaves workspace")
		}
		configPath = resolvedPath
		content, err = os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("read Dev Container configuration: %w", err)
		}
		digest := sha256.Sum256(content)
		if !strings.EqualFold(hex.EncodeToString(digest[:]), strings.TrimSpace(selection.ContentSHA256)) {
			return nil, fmt.Errorf("Dev Container configuration digest does not match create request")
		}
	default:
		return nil, fmt.Errorf("Dev Container configuration source is invalid")
	}
	if err != nil {
		return nil, err
	}
	standard, err := hujson.Standardize(content)
	if err != nil {
		return nil, fmt.Errorf("parse Dev Container configuration: %w", err)
	}
	var value any
	if err := json.Unmarshal(standard, &value); err != nil {
		return nil, fmt.Errorf("parse Dev Container configuration: %w", err)
	}
	value, err = substituteValue(value, substitutionContext{
		localWorkspace: workspace,
		localEnv:       maps.Clone(localEnv),
		devContainerID: devContainerID,
	})
	if err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		if remote, ok := object["remoteEnv"].(map[string]any); ok {
			for name, item := range remote {
				if item == nil {
					delete(remote, name)
				}
			}
		}
		if err := validateExtensionProperties(object); err != nil {
			return nil, err
		}
	}
	standard, err = json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(standard))
	var config Configuration
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode Dev Container configuration: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	return &ResolvedConfiguration{
		Configuration:     config,
		Workspace:         workspace,
		ConfigurationPath: configPath,
		ConfigurationDir:  filepath.Dir(configPath),
		ContentSHA256:     hex.EncodeToString(digest[:]),
		DevContainerID:    devContainerID,
	}, nil
}

func (c *Configuration) Validate() error {
	sources := 0
	if strings.TrimSpace(c.Image) != "" {
		sources++
	}
	if c.Build != nil || strings.TrimSpace(c.DockerFile) != "" {
		sources++
	}
	if len(c.DockerComposeFile) > 0 {
		sources++
	}
	if sources != 1 {
		return fmt.Errorf("Dev Container configuration must select exactly one image, build, or Docker Compose source")
	}
	if len(c.DockerComposeFile) > 0 && strings.TrimSpace(c.Service) == "" {
		return fmt.Errorf("Docker Compose Dev Container service is required")
	}
	if c.Build != nil && strings.TrimSpace(c.Build.Dockerfile) == "" {
		if strings.TrimSpace(c.DockerFile) == "" {
			return fmt.Errorf("Dev Container build.dockerfile is required")
		}
		c.Build.Dockerfile = c.DockerFile
		if c.Build.Context == "" {
			c.Build.Context = c.Context
		}
	}
	if c.Build != nil && len(c.Build.Options) > 0 {
		return fmt.Errorf("Dev Container build.options is not available in the native Docker API; use typed build properties")
	}
	if len(c.RunArgs) > 0 {
		return fmt.Errorf("Dev Container runArgs is not available in the native Docker API; use typed container properties")
	}
	if c.WaitFor == "" {
		c.WaitFor = "updateContentCommand"
	}
	switch c.WaitFor {
	case "initializeCommand", "onCreateCommand", "updateContentCommand", "postCreateCommand", "postStartCommand":
	default:
		return fmt.Errorf("Dev Container waitFor value %q is invalid", c.WaitFor)
	}
	if c.UserEnvProbe == "" {
		c.UserEnvProbe = "loginInteractiveShell"
	}
	switch c.UserEnvProbe {
	case "none", "loginShell", "loginInteractiveShell", "interactiveShell":
	default:
		return fmt.Errorf("Dev Container userEnvProbe value %q is invalid", c.UserEnvProbe)
	}
	return nil
}

func validateExtensionProperties(configuration map[string]any) error {
	known := map[string]struct{}{
		"$schema": {}, "name": {}, "image": {}, "build": {}, "dockerFile": {}, "context": {},
		"dockerComposeFile": {}, "service": {}, "runServices": {}, "workspaceFolder": {}, "workspaceMount": {},
		"mounts": {}, "containerEnv": {}, "remoteEnv": {}, "containerUser": {}, "remoteUser": {},
		"updateRemoteUserUID": {}, "userEnvProbe": {}, "initializeCommand": {}, "onCreateCommand": {},
		"updateContentCommand": {}, "postCreateCommand": {}, "postStartCommand": {}, "postAttachCommand": {},
		"waitFor": {}, "shutdownAction": {}, "features": {}, "overrideFeatureInstallOrder": {}, "customizations": {},
		"forwardPorts": {}, "portsAttributes": {}, "otherPortsAttributes": {}, "appPort": {}, "runArgs": {},
		"init": {}, "privileged": {}, "capAdd": {}, "securityOpt": {}, "overrideCommand": {},
		"hostRequirements": {}, "secrets": {},
	}
	for name, value := range configuration {
		if _, ok := known[name]; ok {
			continue
		}
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("Dev Container extension property %q must be an object", name)
		}
	}
	return nil
}

type substitutionContext struct {
	localWorkspace string
	localEnv       map[string]string
	devContainerID string
}

func substituteValue(value any, context substitutionContext) (any, error) {
	switch value := value.(type) {
	case string:
		var substitutionErr error
		result := variablePattern.ReplaceAllStringFunc(value, func(match string) string {
			name := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
			switch name {
			case "localWorkspaceFolder":
				return context.localWorkspace
			case "localWorkspaceFolderBasename":
				return filepath.Base(context.localWorkspace)
			case "devcontainerId":
				return context.devContainerID
			}
			if strings.HasPrefix(name, "localEnv:") {
				parts := strings.SplitN(strings.TrimPrefix(name, "localEnv:"), ":", 2)
				if resolved, ok := context.localEnv[parts[0]]; ok {
					return resolved
				}
				if len(parts) == 2 {
					return parts[1]
				}
				return ""
			}
			if strings.HasPrefix(name, "containerEnv:") || strings.HasPrefix(name, "containerWorkspaceFolder") {
				return match
			}
			substitutionErr = fmt.Errorf("unsupported Dev Container variable %q", match)
			return match
		})
		return result, substitutionErr
	case []any:
		for i := range value {
			resolved, err := substituteValue(value[i], context)
			if err != nil {
				return nil, err
			}
			value[i] = resolved
		}
	case map[string]any:
		for key, item := range value {
			resolved, err := substituteValue(item, context)
			if err != nil {
				return nil, err
			}
			value[key] = resolved
		}
	}
	return value, nil
}

func validateRepositoryPath(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) || filepath.Clean(value) != filepath.FromSlash(value) || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("Dev Container repository path is invalid")
	}
	return nil
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// StringList accepts either one string or an array of strings.
type StringList []string

func (s *StringList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("must be a string or string array")
	}
	*s = many
	return nil
}

// Port accepts a numeric port or a service/port string.
type Port struct {
	Number  uint16
	Address string
}

func (p *Port) UnmarshalJSON(data []byte) error {
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

func (p Port) MarshalJSON() ([]byte, error) {
	if p.Number != 0 {
		return json.Marshal(p.Number)
	}
	return json.Marshal(p.Address)
}

// Command keeps all three lifecycle command forms without converting shell semantics.
type Command struct {
	Value json.RawMessage
}

func (c *Command) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
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

func (c Command) MarshalJSON() ([]byte, error) {
	if len(c.Value) == 0 {
		return []byte("null"), nil
	}
	return c.Value, nil
}
