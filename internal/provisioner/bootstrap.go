// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	incus "github.com/lxc/incus/v6/client"
)

const (
	bootstrapResultDone                = "done"
	bootstrapResultRecoverableFailed   = "recoverable_failed"
	bootstrapResultUnrecoverableFailed = "unrecoverable_failed"
	defaultWorkspaceRoot               = "/workspaces"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type bootstrapResult struct {
	Outcome string `json:"outcome"`
	Stage   string `json:"stage"`
}

// RuntimeFailureKind identifies whether a runtime failure can use the same operation context again.
type RuntimeFailureKind string

const (
	// RuntimeFailureRecoverable means the current operation can retry the same stage later.
	RuntimeFailureRecoverable RuntimeFailureKind = "recoverable"
	// RuntimeFailureUnrecoverable means the current operation must be finalized as failed.
	RuntimeFailureUnrecoverable RuntimeFailureKind = "unrecoverable"
)

// RuntimeFailureError reports a classified native runtime failure.
type RuntimeFailureError struct {
	Kind    RuntimeFailureKind
	Outcome string
	Stage   string
	Message string
}

func (e *RuntimeFailureError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("runtime outcome %q at stage %s", e.Outcome, e.Stage)
}

// IsRecoverableRuntimeFailure reports whether err is a recoverable runtime failure.
func IsRecoverableRuntimeFailure(err error) bool {
	var failure *RuntimeFailureError
	return errors.As(err, &failure) && failure.Kind == RuntimeFailureRecoverable
}

func (p *IncusProvisioner) runBootstrap(ctx context.Context, instanceName string, request LifecycleRequest) (map[string]string, error) {
	const stage = "initialize-system"
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.CodespaceUUID) == "" {
		return nil, fmt.Errorf("codespace uuid is empty")
	}
	if err := p.ensureBootstrapStateFiles(ctx, instanceName); err != nil {
		return nil, err
	}
	dockerConfigurationChanged, err := p.configureDockerDaemon(ctx, instanceName)
	if err != nil {
		return nil, err
	}
	scriptPath := filepath.Join(bootstrapScriptDir, "bootstrap.sh")
	if err := p.writeRuntimeFile(ctx, instanceName, scriptPath, builtinBootstrapScript, 0o700, "file"); err != nil {
		return nil, err
	}
	if err := p.writeBootstrapOutputFile(ctx, instanceName, ""); err != nil {
		return nil, err
	}
	resultPath := filepath.Join(bootstrapResultDir, resultFileName(request.Operation, stage))
	if err := p.writeRuntimeFile(ctx, instanceName, resultPath, "", 0o600, "file"); err != nil {
		return nil, err
	}

	environment := p.bootstrapEnvironment(request, resultPath)
	if dockerConfigurationChanged {
		environment["CODESPACE_DOCKER_DAEMON_CONFIGURED"] = "1"
	}
	p.writeLifecycleLog(ctx, request.LogSink, fmt.Sprintf("%s started", stage))
	execErr := p.execCommandWithLogSink(ctx, instanceName, []string{p.bootstrap.Shell, scriptPath}, environment, "/", request.LogSink)
	resultContent, _, err := p.readRuntimeFile(ctx, instanceName, resultPath)
	if err != nil {
		if execErr != nil {
			return nil, newRecoverableRuntimeFailure(stage, fmt.Sprintf("bootstrap command failed and result could not be read: %v; %v", execErr, err))
		}
		return nil, newRecoverableRuntimeFailure(stage, fmt.Sprintf("bootstrap result could not be read: %v", err))
	}
	if err := validateBootstrapResult(resultContent, stage); err != nil {
		if execErr != nil {
			return nil, fmt.Errorf("bootstrap command failed: %v; %w", execErr, err)
		}
		return nil, err
	}
	currentEnvFile, _, err := p.readRuntimeFile(ctx, instanceName, runtimeBootstrapOutputFile)
	if err != nil {
		return nil, newRecoverableRuntimeFailure(stage, fmt.Sprintf("bootstrap output could not be read: %v", err))
	}
	currentEnv, err := parseEnvironmentFile(currentEnvFile, bootstrapInputNames())
	if err != nil {
		return nil, newRecoverableRuntimeFailure(stage, fmt.Sprintf("bootstrap output is invalid: %v", err))
	}
	if err := p.writeBootstrapOutputFile(ctx, instanceName, encodeEnvironmentFile(currentEnv)); err != nil {
		return nil, err
	}
	p.writeLifecycleLog(ctx, request.LogSink, fmt.Sprintf("%s completed", stage))
	return currentEnv, nil
}

func (p *IncusProvisioner) writeLifecycleLog(ctx context.Context, sink LifecycleLogSink, message string) {
	if sink == nil || message == "" {
		return
	}
	_ = sink.WriteLifecycleLog(ctx, message)
}

func (p *IncusProvisioner) writeBootstrapOutputFile(ctx context.Context, instanceName, content string) error {
	return p.execCommand(ctx, instanceName, []string{p.bootstrap.Shell, "-c", `
set -eu
tmp="${CODESPACE_BOOTSTRAP_OUTPUT}.tmp.$$"
umask 177
printf '%s' "$CODESPACE_BOOTSTRAP_OUTPUT_CONTENT" > "$tmp"
chmod 600 "$tmp"
mv "$tmp" "$CODESPACE_BOOTSTRAP_OUTPUT"
`}, map[string]string{
		"CODESPACE_BOOTSTRAP_OUTPUT":         runtimeBootstrapOutputFile,
		"CODESPACE_BOOTSTRAP_OUTPUT_CONTENT": content,
	}, "/")
}

func (p *IncusProvisioner) ensureBootstrapStateFiles(ctx context.Context, instanceName string) error {
	for _, directory := range []struct {
		path string
		mode int
	}{
		{path: runtimeCredentialDir, mode: runtimeRootDirMode},
		{path: runtimeManifestDir, mode: runtimeRootDirMode},
		{path: runtimeStateDir, mode: runtimePrivateDirMode},
		{path: bootstrapResultDir, mode: runtimePrivateDirMode},
		{path: runtimeExecutableDir, mode: runtimeRootDirMode},
		{path: bootstrapScriptDir, mode: runtimeRootDirMode},
	} {
		if err := p.writeRuntimeFile(ctx, instanceName, directory.path, "", directory.mode, "directory"); err != nil {
			return err
		}
	}
	return nil
}

func (p *IncusProvisioner) writeRuntimeFile(ctx context.Context, instanceName, path, content string, mode int, kind string) error {
	return p.writeRuntimeContent(ctx, instanceName, path, strings.NewReader(content), mode, kind)
}

func (p *IncusProvisioner) writeRuntimeContent(ctx context.Context, instanceName, path string, content io.ReadSeeker, mode int, kind string) error {
	args := incus.InstanceFileArgs{
		Content:   content,
		UID:       0,
		GID:       0,
		Mode:      mode,
		Type:      kind,
		WriteMode: runtimeCredentialWriteMode,
	}
	if kind == "directory" {
		args.Content = nil
	} else {
		if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
			return err
		}
		if err := p.client.DeleteInstanceFile(instanceName, path); err != nil && !isNotFoundError(err) {
			return fmt.Errorf("delete previous %s: %w", path, err)
		}
	}
	if err := p.createInstanceFile(ctx, instanceName, path, args); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func (p *IncusProvisioner) readRuntimeFile(ctx context.Context, instanceName, path string) (string, bool, error) {
	if err := p.waitInstanceFileAPI(ctx, instanceName); err != nil {
		return "", false, err
	}
	content, _, err := p.client.GetInstanceFile(instanceName, path)
	if err != nil {
		if isNotFoundError(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	defer content.Close()
	data, err := io.ReadAll(io.LimitReader(content, 1024*1024))
	if err != nil {
		return "", false, fmt.Errorf("read %s content: %w", path, err)
	}
	return string(data), true, nil
}

func (p *IncusProvisioner) bootstrapEnvironment(request LifecycleRequest, resultPath string) map[string]string {
	runtimeUserName := strings.TrimSpace(request.RuntimeUserName)
	if runtimeUserName == "" {
		runtimeUserName = p.bootstrap.UserName
	}
	repoName := repoDirName(request.RepoFullName)
	values := map[string]string{
		"HOME":                            p.bootstrap.HomeDir,
		"CODESPACE_UUID":                  request.CodespaceUUID,
		"CODESPACE_NAME":                  request.CodespaceName,
		"CODESPACE_OPERATION":             string(request.Operation),
		"CODESPACE_USER":                  runtimeUserName,
		"CODESPACE_RESULT":                resultPath,
		"CODESPACE_BOOTSTRAP_OUTPUT":      runtimeBootstrapOutputFile,
		"CODESPACE_WORKSPACES_DIR":        defaultWorkspaceRoot,
		"CODESPACE_RUNTIME_DIR":           runtimeCredentialDir,
		"CODESPACE_RUNTIME_SEED_DIR":      runtimeCredentialSeedDir,
		"CODESPACE_GITEA_TOKEN_FILE":      runtimeGiteaTokenFilePath,
		"CODESPACE_GIT_SSH_PRIVATE_KEY":   runtimeGitSSHPrivateKey,
		"CODESPACE_GIT_SSH_PUBLIC_KEY":    runtimeGitSSHPublicKey,
		"CODESPACE_GIT_SSH_KNOWN_HOSTS":   runtimeGitSSHKnownHosts,
		"CODESPACE_SECRETS_FILE":          runtimeSecretFile,
		"GITEA_USER_NAME":                 request.UserName,
		"GITEA_GIT_USER_NAME":             request.UserName,
		"GITEA_GIT_USER_EMAIL":            request.GitUserEmail,
		"GITEA_SERVER_URL":                request.ServerURL,
		"GITEA_TOKEN":                     request.GiteaToken,
		"GITEA_REPO_CLONE_HTTP_URL":       request.RepoCloneHTTPURL,
		"GITEA_REPO_CLONE_SSH_URL":        request.RepoCloneSSHURL,
		"GITEA_GIT_PROTOCOL":              request.GitProtocol,
		"GITEA_REPO_FULL_NAME":            request.RepoFullName,
		"GITEA_REPO_NAME":                 repoName,
		"GITEA_START_REF":                 request.StartRef,
		"GITEA_COMMIT_SHA":                request.CommitSHA,
		"GITEA_CODESPACE_ENVIRONMENT_TAG": request.EnvironmentTag,
		"GITEA_DEVCONTAINER_SOURCE":       request.DevContainer.Source,
		"GITEA_DEVCONTAINER_PATH":         request.DevContainer.Path,
		"GITEA_DEVCONTAINER_COMMIT_SHA":   request.DevContainer.CommitSHA,
		"CODESPACE_REPO_NAME":             repoName,
	}
	return values
}

func (p *IncusProvisioner) configureDockerDaemon(ctx context.Context, instanceName string) (bool, error) {
	const daemonConfigPath = "/etc/docker/daemon.json"
	content, exists, err := p.readRuntimeFile(ctx, instanceName, daemonConfigPath)
	if err != nil {
		return false, err
	}
	encoded, changed, err := p.dockerDaemonConfiguration(content)
	if err != nil || !changed {
		return false, err
	}
	if err := p.writeRuntimeFile(ctx, instanceName, "/etc/docker", "", 0o755, "directory"); err != nil {
		return false, err
	}
	if err := p.writeRuntimeFile(ctx, instanceName, daemonConfigPath, encoded, 0o600, "file"); err != nil {
		return false, err
	}
	return !exists || strings.TrimSpace(content) != encoded, nil
}

func (p *IncusProvisioner) dockerDaemonConfiguration(content string) (string, bool, error) {
	var registryMirrors []string
	if mirror := strings.TrimSpace(p.registryMirrors["docker.io"]); mirror != "" {
		parsed, err := url.Parse(mirror)
		if err == nil && strings.Trim(parsed.Path, "/") == "" {
			registryMirrors = append(registryMirrors, mirror)
		}
	}
	insecureRegistries := make([]string, 0, len(p.registryMirrors)+1)
	values := []string{p.buildCacheRegistry}
	for _, value := range p.registryMirrors {
		values = append(values, value)
	}
	for _, value := range values {
		parsed, err := url.Parse(strings.TrimSpace(value))
		if err == nil && parsed.Scheme == "http" && parsed.Host != "" {
			insecureRegistries = append(insecureRegistries, parsed.Host)
		}
	}
	if len(registryMirrors) == 0 && len(insecureRegistries) == 0 {
		return "", false, nil
	}

	configuration := map[string]json.RawMessage{}
	if strings.TrimSpace(content) != "" {
		if err := json.Unmarshal([]byte(content), &configuration); err != nil {
			return "", false, fmt.Errorf("decode existing Docker daemon configuration: %w", err)
		}
		if configuration == nil {
			return "", false, fmt.Errorf("existing Docker daemon configuration must be a JSON object")
		}
	}
	mergeList := func(name string, additions []string) error {
		if len(additions) == 0 {
			return nil
		}
		values := []string{}
		if raw := configuration[name]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &values); err != nil {
				return fmt.Errorf("decode Docker daemon %s: %w", name, err)
			}
		}
		seen := make(map[string]struct{}, len(values)+len(additions))
		for _, value := range values {
			seen[value] = struct{}{}
		}
		for _, value := range additions {
			if _, ok := seen[value]; !ok {
				values = append(values, value)
				seen[value] = struct{}{}
			}
		}
		sort.Strings(values)
		encoded, err := json.Marshal(values)
		if err != nil {
			return err
		}
		configuration[name] = encoded
		return nil
	}
	if err := mergeList("registry-mirrors", registryMirrors); err != nil {
		return "", false, err
	}
	if err := mergeList("insecure-registries", insecureRegistries); err != nil {
		return "", false, err
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return "", false, fmt.Errorf("encode Docker daemon configuration: %w", err)
	}
	return string(encoded), strings.TrimSpace(content) != string(encoded), nil
}

func bootstrapInputNames() map[string]struct{} {
	names := map[string]struct{}{}
	for name := range (&IncusProvisioner{}).bootstrapEnvironment(LifecycleRequest{}, "") {
		names[name] = struct{}{}
	}
	return names
}

func resultFileName(operation LifecycleOperation, stage string) string {
	return string(operation) + "-" + stage + ".json"
}

func validateBootstrapResult(content, stage string) error {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var result bootstrapResult
	if err := decoder.Decode(&result); err != nil {
		return newRecoverableRuntimeFailure(stage, fmt.Sprintf("decode bootstrap result: %v", err))
	}
	if strings.TrimSpace(result.Stage) != stage {
		return newRecoverableRuntimeFailure(stage, fmt.Sprintf("bootstrap stage %q does not match %q", result.Stage, stage))
	}
	switch strings.TrimSpace(result.Outcome) {
	case bootstrapResultDone:
		return nil
	case bootstrapResultRecoverableFailed:
		return &RuntimeFailureError{Kind: RuntimeFailureRecoverable, Outcome: result.Outcome, Stage: stage}
	case bootstrapResultUnrecoverableFailed:
		return &RuntimeFailureError{Kind: RuntimeFailureUnrecoverable, Outcome: result.Outcome, Stage: stage}
	default:
		return newRecoverableRuntimeFailure(stage, fmt.Sprintf("bootstrap outcome %q at stage %s", result.Outcome, stage))
	}
}

func newRecoverableRuntimeFailure(stage, message string) error {
	return &RuntimeFailureError{
		Kind:    RuntimeFailureRecoverable,
		Stage:   stage,
		Message: message,
	}
}

func parseEnvironmentFile(content string, ignored map[string]struct{}) (map[string]string, error) {
	content = trimEnvironmentFilePadding(content)
	if strings.ContainsRune(content, '\x00') {
		return nil, fmt.Errorf("environment file contains NUL")
	}
	values := map[string]string{}
	for lineNumber, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d must use NAME=value", lineNumber+1)
		}
		if !environmentNamePattern.MatchString(name) {
			return nil, fmt.Errorf("line %d has invalid name %q", lineNumber+1, name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("line %d has invalid value", lineNumber+1)
		}
		if _, skip := ignored[name]; skip {
			continue
		}
		values[name] = value
	}
	return values, nil
}

func trimEnvironmentFilePadding(content string) string {
	return strings.TrimRight(content, "\x00")
}

func encodeEnvironmentFile(values map[string]string) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var buffer bytes.Buffer
	for _, name := range names {
		buffer.WriteString(name)
		buffer.WriteByte('=')
		buffer.WriteString(values[name])
		buffer.WriteByte('\n')
	}
	return buffer.String()
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return map[string]string{}
	}
	copied := make(map[string]string, len(values))
	for name, value := range values {
		copied[name] = value
	}
	return copied
}

func parseUint32Env(values map[string]string, name string) (uint32, error) {
	value, err := parseIntEnv(values, name)
	if err != nil {
		return 0, err
	}
	return uint32(value), nil
}

func parseIntEnv(values map[string]string, name string) (int, error) {
	raw := strings.TrimSpace(values[name])
	if raw == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must be non-negative", name)
	}
	return value, nil
}
