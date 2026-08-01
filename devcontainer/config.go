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
	"slices"
	"strconv"
	"strings"

	"github.com/tailscale/hujson"
)

var (
	variablePattern          = regexp.MustCompile(`\$\{([^{}]+)\}`)
	portNumberOrRangePattern = regexp.MustCompile(`^\d+(-\d+)?$`)
	byteRequirementPattern   = regexp.MustCompile(`^\d+([tgmk]b)?$`)
)

// Load reads and resolves one repository configuration or a synthetic default-image configuration.
func Load(options LoadOptions) (*ResolvedConfiguration, error) {
	workspace := options.Workspace
	source := options.Source
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
	allowedPathRoot, err := resolveAllowedPathRoot(options.AllowedPathRoot)
	if err != nil {
		return nil, err
	}
	var content []byte
	var configPath string
	switch {
	case strings.TrimSpace(source.DefaultImage) != "":
		image := strings.TrimSpace(source.DefaultImage)
		if source.Path != "" || source.ContentSHA256 != "" {
			return nil, fmt.Errorf("platform default contains repository fields")
		}
		content, err = json.Marshal(Configuration{Name: "Dev Container", Image: image})
		configPath = filepath.Join(workspace, ".devcontainer-default.json")
	case strings.TrimSpace(source.Path) != "":
		configPath = filepath.FromSlash(strings.TrimSpace(source.Path))
		if !filepath.IsAbs(configPath) {
			configPath = filepath.Join(workspace, configPath)
		}
		resolvedPath, resolveErr := filepath.EvalSymlinks(configPath)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve Dev Container configuration links: %w", resolveErr)
		}
		configPath = resolvedPath
		if allowedPathRoot != "" && !pathIsInsideRoot(allowedPathRoot, configPath) {
			return nil, fmt.Errorf("Dev Container configuration leaves the allowed path root")
		}
		content, err = os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("read Dev Container configuration: %w", err)
		}
	default:
		return nil, fmt.Errorf("Dev Container configuration source is empty")
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	if source.ContentSHA256 != "" && !strings.EqualFold(hex.EncodeToString(digest[:]), strings.TrimSpace(source.ContentSHA256)) {
		return nil, fmt.Errorf("Dev Container configuration digest does not match create request")
	}
	standard, err := hujson.Standardize(content)
	if err != nil {
		return nil, fmt.Errorf("parse Dev Container configuration: %w", err)
	}
	var value any
	valueDecoder := json.NewDecoder(bytes.NewReader(standard))
	valueDecoder.UseNumber()
	if err := valueDecoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse Dev Container configuration: %w", err)
	}
	value, err = substituteVariables(value, substitutionContext{
		localWorkspace: workspace,
		localEnv:       maps.Clone(options.LocalEnv),
		devContainerID: options.ID,
	}.resolve)
	if err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
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
	if err := normalizeDockerfileBuild(&config); err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ResolvedConfiguration{
		Configuration:     config,
		Workspace:         workspace,
		ConfigurationPath: configPath,
		ConfigurationDir:  filepath.Dir(configPath),
		ContentSHA256:     hex.EncodeToString(digest[:]),
		DevContainerID:    options.ID,
		LocalEnvironment:  maps.Clone(options.LocalEnv),
		AllowedPathRoot:   allowedPathRoot,
		Synthetic:         strings.TrimSpace(source.DefaultImage) != "",
	}, nil
}

func normalizeDockerfileBuild(configuration *Configuration) error {
	dockerfile := strings.TrimSpace(configuration.DockerFile)
	if dockerfile == "" {
		return nil
	}
	if configuration.Build == nil {
		configuration.Build = &Build{}
	}
	if configured := strings.TrimSpace(configuration.Build.Dockerfile); configured != "" && configured != dockerfile {
		return fmt.Errorf("Dev Container dockerFile conflicts with build.dockerfile")
	}
	configuration.Build.Dockerfile = dockerfile
	if configuration.Build.Context == "" {
		configuration.Build.Context = configuration.Context
	}
	configuration.DockerFile = ""
	configuration.Context = ""
	return nil
}

// Validate checks the supported Dev Container configuration without adding
// defaults that could hide whether a source declared a property.
func (c *Configuration) Validate() error {
	sources := 0
	if strings.TrimSpace(c.Image) != "" {
		sources++
	}
	if c.Build != nil {
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
	if len(c.DockerComposeFile) > 0 && strings.TrimSpace(c.WorkspaceFolder) == "" {
		return fmt.Errorf("Docker Compose Dev Container workspaceFolder is required")
	}
	if len(c.DockerComposeFile) > 0 && strings.TrimSpace(c.WorkspaceMount) != "" {
		return fmt.Errorf("Docker Compose Dev Container uses service volumes instead of workspaceMount")
	}
	if c.Build != nil && strings.TrimSpace(c.Build.Dockerfile) == "" {
		return fmt.Errorf("Dev Container build.dockerfile is required")
	}
	if c.HostRequirements.CPUs < 0 {
		return fmt.Errorf("Dev Container hostRequirements.cpus must be a positive number")
	}
	for name, value := range map[string]string{"memory": c.HostRequirements.Memory, "storage": c.HostRequirements.Storage} {
		if value != "" && !byteRequirementPattern.MatchString(value) {
			return fmt.Errorf("Dev Container hostRequirements.%s %q is invalid", name, value)
		}
	}
	if err := validateGPURequirement(c.HostRequirements.GPU); err != nil {
		return err
	}
	for key, attributes := range c.PortsAttributes {
		if err := validatePortAttributeKey(key); err != nil {
			return err
		}
		if err := validatePortAttributes(attributes); err != nil {
			return fmt.Errorf("Dev Container portsAttributes %q: %w", key, err)
		}
	}
	if c.OtherPortsAttributes != nil {
		if err := validatePortAttributes(*c.OtherPortsAttributes); err != nil {
			return fmt.Errorf("Dev Container otherPortsAttributes: %w", err)
		}
	}
	for _, port := range c.AppPort {
		if _, err := port.ContainerPort(); err != nil {
			return fmt.Errorf("Dev Container appPort: %w", err)
		}
	}
	switch c.WaitFor {
	case "", LifecycleStageInitialize, LifecycleStageOnCreate, LifecycleStageUpdateContent, LifecycleStagePostCreate, LifecycleStagePostStart:
	default:
		return fmt.Errorf("Dev Container waitFor value %q is invalid", c.WaitFor)
	}
	switch c.UserEnvProbe {
	case "", "none", "loginShell", "loginInteractiveShell", "interactiveShell":
	default:
		return fmt.Errorf("Dev Container userEnvProbe value %q is invalid", c.UserEnvProbe)
	}
	if len(c.DockerComposeFile) > 0 {
		switch c.ShutdownAction {
		case "", "none", "stopCompose":
		default:
			return fmt.Errorf("Docker Compose shutdownAction value %q is invalid", c.ShutdownAction)
		}
	} else {
		switch c.ShutdownAction {
		case "", "none", "stopContainer":
		default:
			return fmt.Errorf("Dev Container shutdownAction value %q is invalid", c.ShutdownAction)
		}
	}
	return nil
}

// Finalize applies specification defaults after all metadata sources have been
// merged and validates the effective configuration.
func (c *Configuration) Finalize() error {
	if c.WaitFor == "" {
		c.WaitFor = LifecycleStageUpdateContent
	}
	if c.UserEnvProbe == "" {
		c.UserEnvProbe = "loginInteractiveShell"
	}
	if c.ShutdownAction == "" {
		if len(c.DockerComposeFile) > 0 {
			c.ShutdownAction = "stopCompose"
		} else {
			c.ShutdownAction = "stopContainer"
		}
	}
	return c.Validate()
}

func validatePortAttributeKey(value string) error {
	value = strings.TrimSpace(value)
	if !portNumberOrRangePattern.MatchString(value) {
		if _, err := regexp.Compile(value); err != nil {
			return fmt.Errorf("Dev Container portsAttributes key %q is neither a port, range, nor valid process regular expression: %w", value, err)
		}
		return nil
	}
	parts := strings.Split(value, "-")
	ports := make([]uint64, len(parts))
	for i, part := range parts {
		port, err := strconv.ParseUint(part, 10, 16)
		if err != nil || port == 0 {
			return fmt.Errorf("Dev Container portsAttributes key %q contains an invalid port", value)
		}
		ports[i] = port
	}
	if len(ports) == 2 && ports[0] > ports[1] {
		return fmt.Errorf("Dev Container portsAttributes range %q is reversed", value)
	}
	return nil
}

func validateGPURequirement(value json.RawMessage) error {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) || bytes.Equal(value, []byte("true")) || bytes.Equal(value, []byte("false")) || bytes.Equal(value, []byte(`"optional"`)) {
		return nil
	}
	var requirement struct {
		Cores  *float64 `json:"cores"`
		Memory string   `json:"memory"`
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&requirement); err != nil {
		return fmt.Errorf("Dev Container hostRequirements.gpu is invalid")
	}
	if requirement.Cores != nil && *requirement.Cores < 1 {
		return fmt.Errorf("Dev Container hostRequirements.gpu is invalid")
	}
	if requirement.Memory != "" && !byteRequirementPattern.MatchString(requirement.Memory) {
		return fmt.Errorf("Dev Container hostRequirements.gpu is invalid")
	}
	return nil
}

func validatePortAttributes(attributes PortAttributes) error {
	switch attributes.Protocol {
	case "", "http", "https":
	default:
		return fmt.Errorf("protocol %q is invalid", attributes.Protocol)
	}
	switch attributes.OnAutoForward {
	case "", "notify", "openBrowser", "openBrowserOnce", "openPreview", "silent", "ignore":
	default:
		return fmt.Errorf("onAutoForward %q is invalid", attributes.OnAutoForward)
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

func (c substitutionContext) resolve(name, match string) (string, error) {
	switch name {
	case "localWorkspaceFolder":
		return c.localWorkspace, nil
	case "localWorkspaceFolderBasename":
		return filepath.Base(c.localWorkspace), nil
	case "devcontainerId":
		return c.devContainerID, nil
	}
	if strings.HasPrefix(name, "localEnv:") || strings.HasPrefix(name, "env:") {
		_, arguments, _ := strings.Cut(name, ":")
		parts := strings.SplitN(arguments, ":", 2)
		if parts[0] == "" {
			return match, fmt.Errorf("Dev Container variable %q has no environment variable name", match)
		}
		if resolved, ok := c.localEnv[parts[0]]; ok {
			return resolved, nil
		}
		if len(parts) == 2 {
			return parts[1], nil
		}
		return "", nil
	}
	if strings.HasPrefix(name, "containerEnv:") || strings.HasPrefix(name, "containerWorkspaceFolder") {
		return match, nil
	}
	return match, nil
}

func substituteVariables(value any, resolve func(name, match string) (string, error)) (any, error) {
	switch value := value.(type) {
	case string:
		var substitutionErr error
		result := variablePattern.ReplaceAllStringFunc(value, func(match string) string {
			name := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
			resolved, err := resolve(name, match)
			if err != nil && substitutionErr == nil {
				substitutionErr = err
			}
			return resolved
		})
		return result, substitutionErr
	case []any:
		for i := range value {
			resolved, err := substituteVariables(value[i], resolve)
			if err != nil {
				return nil, err
			}
			value[i] = resolved
		}
	case map[string]any:
		for key, item := range value {
			resolved, err := substituteVariables(item, resolve)
			if err != nil {
				return nil, err
			}
			value[key] = resolved
		}
	}
	return value, nil
}

func pathIsInsideRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func resolveAllowedPathRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	root, err := filepath.Abs(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("resolve allowed path root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve allowed path root links: %w", err)
	}
	return root, nil
}

// ResolveLocalVariables resolves host-side variables in image or Feature metadata
// before it is merged with the repository configuration.
func ResolveLocalVariables(configuration Configuration, workspace string, environment map[string]string, id string) (Configuration, error) {
	return resolveConfigurationVariables(configuration, func(value any) (any, error) {
		return substituteVariables(value, substitutionContext{
			localWorkspace: workspace,
			localEnv:       maps.Clone(environment),
			devContainerID: id,
		}.resolve)
	})
}

// ResolveContainerVariables resolves variables that become available after the image and Feature metadata are merged.
func ResolveContainerVariables(configuration Configuration, workspaceFolder string, environment map[string]string) (Configuration, error) {
	return resolveConfigurationVariables(configuration, func(value any) (any, error) {
		return substituteVariables(value, func(name, match string) (string, error) {
			switch name {
			case "containerWorkspaceFolder":
				return workspaceFolder, nil
			case "containerWorkspaceFolderBasename":
				return filepath.Base(workspaceFolder), nil
			}
			if strings.HasPrefix(name, "containerEnv:") {
				parts := strings.SplitN(strings.TrimPrefix(name, "containerEnv:"), ":", 2)
				if parts[0] == "" {
					return match, fmt.Errorf("Dev Container variable %q has no environment variable name", match)
				}
				if resolved, ok := environment[parts[0]]; ok {
					return resolved, nil
				}
				if len(parts) == 2 {
					return parts[1], nil
				}
				return "", nil
			}
			return match, nil
		})
	})
}

func resolveConfigurationVariables(configuration Configuration, substitute func(any) (any, error)) (Configuration, error) {
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return Configuration{}, err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Configuration{}, err
	}
	value, err = substitute(value)
	if err != nil {
		return Configuration{}, err
	}
	encoded, err = json.Marshal(value)
	if err != nil {
		return Configuration{}, err
	}
	var resolved Configuration
	if err := json.Unmarshal(encoded, &resolved); err != nil {
		return Configuration{}, err
	}
	return resolved, nil
}

// PortAttributesFor returns the most specific configured attributes for a port.
func PortAttributesFor(configuration Configuration, port uint16) PortAttributes {
	return PortAttributesForProcess(configuration, port, "")
}

// PortAttributesForProcess returns attributes selected by port, range, or the
// command line of the process listening on the port.
func PortAttributesForProcess(configuration Configuration, port uint16, commandLine string) PortAttributes {
	if attributes, ok := configuration.PortsAttributes[strconv.Itoa(int(port))]; ok {
		return attributes
	}
	var selected PortAttributes
	var selectedKey string
	var selectedSpan uint64
	for key, attributes := range configuration.PortsAttributes {
		first, last, ok := strings.Cut(strings.TrimSpace(key), "-")
		if !ok {
			continue
		}
		lower, lowerErr := strconv.ParseUint(strings.TrimSpace(first), 10, 16)
		upper, upperErr := strconv.ParseUint(strings.TrimSpace(last), 10, 16)
		if lowerErr == nil && upperErr == nil && lower <= uint64(port) && uint64(port) <= upper && lower <= upper {
			span := upper - lower + 1
			if selectedSpan == 0 || span < selectedSpan || (span == selectedSpan && key < selectedKey) {
				selected = attributes
				selectedKey = key
				selectedSpan = span
			}
		}
	}
	if selectedSpan != 0 {
		return selected
	}
	if commandLine != "" {
		keys := make([]string, 0, len(configuration.PortsAttributes))
		for key := range configuration.PortsAttributes {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			if portNumberOrRangePattern.MatchString(key) {
				continue
			}
			if expression, err := regexp.Compile(key); err == nil && expression.MatchString(commandLine) {
				return configuration.PortsAttributes[key]
			}
		}
	}
	if configuration.OtherPortsAttributes != nil {
		return *configuration.OtherPortsAttributes
	}
	return PortAttributes{}
}
