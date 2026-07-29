// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/pkg/archive"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"gitea.dev/codespace/internal/devcontainer"
)

const (
	devContainerFeatureMediaType = "application/vnd.devcontainers"
	codeServerFeature            = "ghcr.io/coder/devcontainer-features/code-server:2.0.0"
)

type resolvedFeature struct {
	Reference string
	Digest    string
	Directory string
	Options   map[string]string
	Metadata  featureMetadata
}

type featureMetadata struct {
	ID                   string                        `json:"id"`
	Version              string                        `json:"version"`
	Options              map[string]featureOption      `json:"options"`
	DependsOn            map[string]json.RawMessage    `json:"dependsOn"`
	InstallsAfter        []string                      `json:"installsAfter"`
	ContainerEnv         map[string]string             `json:"containerEnv"`
	RemoteEnv            map[string]string             `json:"remoteEnv"`
	ContainerUser        string                        `json:"containerUser"`
	RemoteUser           string                        `json:"remoteUser"`
	Mounts               []devcontainer.Mount          `json:"mounts"`
	Init                 bool                          `json:"init"`
	Privileged           bool                          `json:"privileged"`
	CapAdd               []string                      `json:"capAdd"`
	SecurityOpt          []string                      `json:"securityOpt"`
	OnCreateCommand      devcontainer.Command          `json:"onCreateCommand"`
	UpdateContentCommand devcontainer.Command          `json:"updateContentCommand"`
	PostCreateCommand    devcontainer.Command          `json:"postCreateCommand"`
	PostStartCommand     devcontainer.Command          `json:"postStartCommand"`
	PostAttachCommand    devcontainer.Command          `json:"postAttachCommand"`
	Customizations       map[string]json.RawMessage    `json:"customizations"`
	HostRequirements     devcontainer.HostRequirements `json:"hostRequirements"`
}

type featureOption struct {
	Type      string            `json:"type"`
	Default   json.RawMessage   `json:"default"`
	Proposals []json.RawMessage `json:"proposals"`
}

func (e *Engine) applyFeatures(ctx context.Context, baseImage string, resolved *devcontainer.ResolvedConfiguration) (string, map[string]string, error) {
	requested := make(map[string]json.RawMessage, len(resolved.Features)+1)
	for reference, options := range resolved.Features {
		requested[reference] = options
	}
	codeServerOptions, _ := json.Marshal(map[string]any{
		"version":            "4.121.0",
		"auth":               "none",
		"host":               "0.0.0.0",
		"port":               strconv.Itoa(webIDEPort),
		"disableTelemetry":   true,
		"disableUpdateCheck": true,
	})
	requested[codeServerFeature] = codeServerOptions

	temporary, err := os.MkdirTemp("", "gitea-devcontainer-features-*")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(temporary)
	features := map[string]*resolvedFeature{}
	var fetch func(string, json.RawMessage) error
	fetch = func(reference string, rawOptions json.RawMessage) error {
		if _, ok := features[reference]; ok {
			return nil
		}
		directory := filepath.Join(temporary, strconv.Itoa(len(features)))
		feature, err := fetchFeature(ctx, reference, rawOptions, directory)
		if err != nil {
			return err
		}
		features[reference] = feature
		for dependency, options := range feature.Metadata.DependsOn {
			if err := fetch(dependency, options); err != nil {
				return fmt.Errorf("resolve dependency of Feature %s: %w", reference, err)
			}
		}
		return nil
	}
	for reference, options := range requested {
		if err := fetch(reference, options); err != nil {
			return "", nil, err
		}
	}
	ordered, err := orderFeatures(features, resolved.OverrideFeatureInstallOrder)
	if err != nil {
		return "", nil, err
	}
	for _, feature := range ordered {
		mergeFeatureMetadata(&resolved.Configuration, feature.Metadata)
	}
	containerUser := strings.TrimSpace(resolved.ContainerUser)
	if containerUser == "" {
		inspect, err := e.client.ImageInspect(ctx, baseImage)
		if err != nil {
			return "", nil, fmt.Errorf("inspect Dev Container base image: %w", err)
		}
		if inspect.Config != nil {
			containerUser = strings.TrimSpace(inspect.Config.User)
		}
	}
	if containerUser == "" {
		containerUser = "root"
	}
	resolved.ContainerUser = containerUser
	if strings.TrimSpace(resolved.RemoteUser) == "" {
		resolved.RemoteUser = containerUser
	}
	if err := writeFeatureBuildContext(temporary, baseImage, ordered, resolved.RemoteUser); err != nil {
		return "", nil, err
	}
	imageName := "gitea-devcontainer-feature:" + resolved.DevContainerID
	reader, err := archive.TarWithOptions(temporary, &archive.TarOptions{})
	if err != nil {
		return "", nil, err
	}
	defer reader.Close()
	response, err := e.client.ImageBuild(ctx, reader, build.ImageBuildOptions{Dockerfile: "Dockerfile.features", Tags: []string{imageName}, Remove: true})
	if err != nil {
		return "", nil, fmt.Errorf("build Dev Container Features: %w", err)
	}
	defer response.Body.Close()
	if err := streamDockerProgress(response.Body, e.stderr); err != nil {
		return "", nil, fmt.Errorf("build Dev Container Features: %w", err)
	}
	digests := make(map[string]string, len(ordered))
	for _, feature := range ordered {
		digests[feature.Reference] = feature.Digest
	}
	return imageName, digests, nil
}

func fetchFeature(ctx context.Context, reference string, rawOptions json.RawMessage, directory string) (*resolvedFeature, error) {
	parsed, err := registry.ParseReference(reference)
	if err != nil || parsed.Reference == "" {
		return nil, fmt.Errorf("parse Dev Container Feature %q: %w", reference, err)
	}
	repository, err := remote.NewRepository(parsed.Registry + "/" + parsed.Repository)
	if err != nil {
		return nil, err
	}
	credentialStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("open Docker credential store: %w", err)
	}
	repository.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache(), Credential: credentials.Credential(credentialStore)}
	descriptor, err := repository.Resolve(ctx, parsed.Reference)
	if err != nil {
		return nil, fmt.Errorf("resolve Dev Container Feature %s: %w", reference, err)
	}
	manifestReader, err := repository.Fetch(ctx, descriptor)
	if err != nil {
		return nil, fmt.Errorf("fetch Dev Container Feature manifest %s: %w", reference, err)
	}
	var manifest ocispec.Manifest
	decodeErr := json.NewDecoder(manifestReader).Decode(&manifest)
	closeErr := manifestReader.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return nil, fmt.Errorf("decode Dev Container Feature manifest %s: %w", reference, err)
	}
	if !strings.HasPrefix(manifest.Config.MediaType, devContainerFeatureMediaType) {
		return nil, fmt.Errorf("OCI artifact %s is not a Dev Container Feature", reference)
	}
	if len(manifest.Layers) != 1 {
		return nil, fmt.Errorf("Dev Container Feature %s must contain one layer", reference)
	}
	layer, err := repository.Fetch(ctx, manifest.Layers[0])
	if err != nil {
		return nil, fmt.Errorf("fetch Dev Container Feature layer %s: %w", reference, err)
	}
	if err := extractFeatureLayer(layer, directory); err != nil {
		_ = layer.Close()
		return nil, fmt.Errorf("extract Dev Container Feature %s: %w", reference, err)
	}
	if err := layer.Close(); err != nil {
		return nil, err
	}
	metadataFile, err := os.Open(filepath.Join(directory, "devcontainer-feature.json"))
	if err != nil {
		return nil, fmt.Errorf("open Feature metadata: %w", err)
	}
	decoder := json.NewDecoder(metadataFile)
	var metadata featureMetadata
	decodeErr = decoder.Decode(&metadata)
	if err := errors.Join(decodeErr, metadataFile.Close()); err != nil {
		return nil, fmt.Errorf("decode Feature metadata: %w", err)
	}
	if strings.TrimSpace(metadata.ID) == "" {
		return nil, fmt.Errorf("Dev Container Feature %s id is empty", reference)
	}
	if _, err := os.Stat(filepath.Join(directory, "install.sh")); err != nil {
		return nil, fmt.Errorf("Dev Container Feature %s install.sh is missing", reference)
	}
	options, err := resolveFeatureOptions(metadata.Options, rawOptions)
	if err != nil {
		return nil, fmt.Errorf("Dev Container Feature %s: %w", reference, err)
	}
	return &resolvedFeature{Reference: reference, Digest: descriptor.Digest.String(), Directory: directory, Options: options, Metadata: metadata}, nil
}

func extractFeatureLayer(reader io.Reader, directory string) error {
	buffered := bufio.NewReader(reader)
	var tarReader *tar.Reader
	if header, _ := buffered.Peek(2); len(header) == 2 && header[0] == 0x1f && header[1] == 0x8b {
		compressed, err := gzip.NewReader(buffered)
		if err != nil {
			return err
		}
		defer compressed.Close()
		tarReader = tar.NewReader(compressed)
	} else {
		tarReader = tar.NewReader(buffered)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("Feature archive path %q is invalid", header.Name)
		}
		target := filepath.Join(directory, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, io.LimitReader(tarReader, header.Size))
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("Feature archive entry %q has unsupported type", header.Name)
		}
	}
}

func resolveFeatureOptions(declared map[string]featureOption, raw json.RawMessage) (map[string]string, error) {
	provided := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "true" && string(raw) != "null" {
		if err := json.Unmarshal(raw, &provided); err != nil {
			return nil, fmt.Errorf("options must be true or an object")
		}
	}
	for name := range provided {
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("option %q is not declared", name)
		}
	}
	result := make(map[string]string, len(declared))
	for name, option := range declared {
		value := provided[name]
		if len(value) == 0 {
			value = option.Default
		}
		if len(value) == 0 {
			continue
		}
		var scalar any
		if err := json.Unmarshal(value, &scalar); err != nil {
			return nil, fmt.Errorf("option %q is invalid", name)
		}
		switch scalar := scalar.(type) {
		case string:
			result[strings.ToUpper(name)] = scalar
		case bool:
			result[strings.ToUpper(name)] = strconv.FormatBool(scalar)
		default:
			return nil, fmt.Errorf("option %q must be a string or boolean", name)
		}
	}
	return result, nil
}

func orderFeatures(features map[string]*resolvedFeature, override []string) ([]*resolvedFeature, error) {
	ordered := make([]*resolvedFeature, 0, len(features))
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(reference string) error {
		feature, ok := features[reference]
		if !ok {
			return fmt.Errorf("Feature %s is not resolved", reference)
		}
		if state[reference] == 2 {
			return nil
		}
		if state[reference] == 1 {
			return fmt.Errorf("Dev Container Feature dependency cycle contains %s", reference)
		}
		state[reference] = 1
		dependencies := make([]string, 0, len(feature.Metadata.DependsOn))
		for dependency := range feature.Metadata.DependsOn {
			dependencies = append(dependencies, dependency)
		}
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[reference] = 2
		ordered = append(ordered, feature)
		return nil
	}
	for _, requested := range override {
		reference := requested
		if _, ok := features[reference]; !ok {
			requestedID, err := featureReferenceID(requested)
			if err != nil {
				return nil, fmt.Errorf("overrideFeatureInstallOrder: %w", err)
			}
			reference = ""
			for candidate := range features {
				candidateID, err := featureReferenceID(candidate)
				if err == nil && candidateID == requestedID {
					if reference != "" {
						return nil, fmt.Errorf("overrideFeatureInstallOrder: Feature %s is ambiguous", requested)
					}
					reference = candidate
				}
			}
			if reference == "" {
				return nil, fmt.Errorf("overrideFeatureInstallOrder: Feature %s is not resolved", requested)
			}
		}
		if err := visit(reference); err != nil {
			return nil, fmt.Errorf("overrideFeatureInstallOrder: %w", err)
		}
	}
	references := make([]string, 0, len(features))
	for reference := range features {
		references = append(references, reference)
	}
	sort.Strings(references)
	for _, reference := range references {
		if err := visit(reference); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func featureReferenceID(reference string) (string, error) {
	parsed, err := registry.ParseReference(reference)
	if err != nil {
		return "", fmt.Errorf("parse Feature %q: %w", reference, err)
	}
	return parsed.Registry + "/" + parsed.Repository, nil
}

func writeFeatureBuildContext(root, baseImage string, features []*resolvedFeature, remoteUser string) error {
	var dockerfile strings.Builder
	fmt.Fprintf(&dockerfile, "FROM %s\nUSER root\n", baseImage)
	remoteUserHome := "/root"
	if remoteUser != "root" && remoteUser != "0" && !strings.Contains(remoteUser, "/") {
		remoteUserHome = "/home/" + remoteUser
	}
	for index, feature := range features {
		target := filepath.Join(root, "feature", strconv.Itoa(index))
		if err := copyDirectory(feature.Directory, target); err != nil {
			return err
		}
		fmt.Fprintf(&dockerfile, "COPY feature/%d /tmp/dev-container-feature/%d\n", index, index)
		keys := make([]string, 0, len(feature.Options))
		for key := range feature.Options {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		dockerfile.WriteString("RUN ")
		for _, key := range keys {
			fmt.Fprintf(&dockerfile, "%s=%s ", key, shellQuote(feature.Options[key]))
		}
		fmt.Fprintf(&dockerfile, "_REMOTE_USER=%s _CONTAINER_USER=%s _REMOTE_USER_HOME=%s _CONTAINER_USER_HOME=%s sh /tmp/dev-container-feature/%d/install.sh && rm -rf /tmp/dev-container-feature/%d\n", shellQuote(remoteUser), shellQuote(remoteUser), shellQuote(remoteUserHome), shellQuote(remoteUserHome), index, index)
	}
	if remoteUser != "" {
		fmt.Fprintf(&dockerfile, "USER %s\n", remoteUser)
	}
	return os.WriteFile(filepath.Join(root, "Dockerfile.features"), []byte(dockerfile.String()), 0o600)
}

func copyDirectory(source, target string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			_ = input.Close()
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, input.Close(), output.Close())
	})
}

func mergeFeatureMetadata(configuration *devcontainer.Configuration, metadata featureMetadata) {
	if configuration.ContainerUser == "" {
		configuration.ContainerUser = metadata.ContainerUser
	}
	if configuration.RemoteUser == "" {
		configuration.RemoteUser = metadata.RemoteUser
	}
	configuration.ContainerEnv = mergeStringMap(metadata.ContainerEnv, configuration.ContainerEnv)
	configuration.RemoteEnv = mergeStringMap(metadata.RemoteEnv, configuration.RemoteEnv)
	configuration.Mounts = append(metadata.Mounts, configuration.Mounts...)
	configuration.Init = configuration.Init || metadata.Init
	configuration.Privileged = configuration.Privileged || metadata.Privileged
	configuration.CapAdd = appendUnique(metadata.CapAdd, configuration.CapAdd)
	configuration.SecurityOpt = appendUnique(metadata.SecurityOpt, configuration.SecurityOpt)
	configuration.OnCreateCommand = mergeCommands(metadata.ID, metadata.OnCreateCommand, configuration.OnCreateCommand)
	configuration.UpdateContentCommand = mergeCommands(metadata.ID, metadata.UpdateContentCommand, configuration.UpdateContentCommand)
	configuration.PostCreateCommand = mergeCommands(metadata.ID, metadata.PostCreateCommand, configuration.PostCreateCommand)
	configuration.PostStartCommand = mergeCommands(metadata.ID, metadata.PostStartCommand, configuration.PostStartCommand)
	configuration.PostAttachCommand = mergeCommands(metadata.ID, metadata.PostAttachCommand, configuration.PostAttachCommand)
	if configuration.Customizations == nil {
		configuration.Customizations = map[string]json.RawMessage{}
	}
	for name, raw := range metadata.Customizations {
		if _, exists := configuration.Customizations[name]; !exists {
			configuration.Customizations[name] = raw
		}
	}
}

func mergeCommands(name string, feature, configuration devcontainer.Command) devcontainer.Command {
	if len(feature.Value) == 0 {
		return configuration
	}
	if len(configuration.Value) == 0 {
		return feature
	}
	value := map[string]json.RawMessage{}
	mergeCommandEntries := func(prefix string, command devcontainer.Command, preserveNames bool) {
		var entries map[string]json.RawMessage
		if json.Unmarshal(command.Value, &entries) == nil && entries != nil {
			for entryName, entry := range entries {
				if preserveNames {
					value[entryName] = entry
				} else {
					value[prefix+"-"+entryName] = entry
				}
			}
			return
		}
		value[prefix] = command.Value
	}
	mergeCommandEntries("feature-"+name, feature, false)
	mergeCommandEntries("configuration", configuration, true)
	encoded, _ := json.Marshal(value)
	return devcontainer.Command{Value: encoded}
}

func mergeStringMap(base, override map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(override))
	for name, value := range base {
		result[name] = value
	}
	for name, value := range override {
		result[name] = value
	}
	return result
}

func appendUnique(values ...[]string) []string {
	seen := map[string]struct{}{}
	var result []string
	for _, list := range values {
		for _, value := range list {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
