// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	incus "github.com/lxc/incus/v6/client"
)

const (
	scriptBuiltin                   = "builtin"
	scriptResultDone                = "done"
	scriptResultRecoverableFailed   = "recoverable_failed"
	scriptResultUnrecoverableFailed = "unrecoverable_failed"
	scriptPhasePrepare              = "prepare"
	scriptPhaseActivate             = "activate"
	defaultWorkspaceRoot            = "/workspaces"
)

var sharedEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type scriptResult struct {
	Outcome string `json:"outcome"`
	Stage   string `json:"stage"`
}

// ScriptFailureKind identifies whether a script failure can use the same operation context again.
type ScriptFailureKind string

const (
	// ScriptFailureRecoverable means the current operation can retry the same stage later.
	ScriptFailureRecoverable ScriptFailureKind = "recoverable"
	// ScriptFailureUnrecoverable means the current operation must be finalized as failed.
	ScriptFailureUnrecoverable ScriptFailureKind = "unrecoverable"
)

// ScriptFailureError reports a classified lifecycle script failure.
type ScriptFailureError struct {
	Kind    ScriptFailureKind
	Outcome string
	Stage   string
	Message string
}

func (e *ScriptFailureError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("script outcome %q at stage %s", e.Outcome, e.Stage)
}

// IsRecoverableScriptFailure reports whether err is a recoverable lifecycle script failure.
func IsRecoverableScriptFailure(err error) bool {
	var failure *ScriptFailureError
	return errors.As(err, &failure) && failure.Kind == ScriptFailureRecoverable
}

// LoadScripts reads the configured lifecycle script suite and records content digests.
func LoadScripts(config ScriptConfig) (ScriptSnapshot, error) {
	initPath := strings.TrimSpace(config.Init)
	startPath := strings.TrimSpace(config.Start)
	resumePath := strings.TrimSpace(config.Resume)
	if initPath == "" {
		initPath = scriptBuiltin
	}
	if startPath == "" {
		startPath = scriptBuiltin
	}
	if resumePath == "" {
		resumePath = scriptBuiltin
	}
	if initPath == scriptBuiltin && startPath == scriptBuiltin && resumePath == scriptBuiltin {
		return newScriptSnapshot(builtinInitScript, builtinStartScript, builtinResumeScript), nil
	}

	initScript, err := readScriptFile(initPath)
	if err != nil {
		return ScriptSnapshot{}, err
	}
	startScript, err := readScriptFile(startPath)
	if err != nil {
		return ScriptSnapshot{}, err
	}
	resumeScript, err := readScriptFile(resumePath)
	if err != nil {
		return ScriptSnapshot{}, err
	}
	return newScriptSnapshot(initScript, startScript, resumeScript), nil
}

func newScriptSnapshot(initScript, startScript, resumeScript string) ScriptSnapshot {
	return ScriptSnapshot{
		Init:   newScriptFileSnapshot(initScript),
		Start:  newScriptFileSnapshot(startScript),
		Resume: newScriptFileSnapshot(resumeScript),
	}
}

func newScriptFileSnapshot(content string) ScriptFileSnapshot {
	sum := sha256.Sum256([]byte(content))
	return ScriptFileSnapshot{
		Content: content,
		SHA256:  fmt.Sprintf("%x", sum[:]),
	}
}

func scriptSnapshotComplete(snapshot ScriptSnapshot) bool {
	return snapshot.Init.Content != "" &&
		snapshot.Start.Content != "" &&
		snapshot.Resume.Content != "" &&
		snapshot.Init.SHA256 != "" &&
		snapshot.Start.SHA256 != "" &&
		snapshot.Resume.SHA256 != ""
}

func readScriptFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("script path %q must be absolute", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read script %s: %w", path, err)
	}
	if len(content) == 0 {
		return "", fmt.Errorf("script %s is empty", path)
	}
	return string(content), nil
}

func (p *IncusProvisioner) runLifecycleScript(
	ctx context.Context,
	instanceName string,
	scriptName string,
	script string,
	stage string,
	request BootstrapRequest,
) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.CodespaceUUID) == "" {
		return nil, fmt.Errorf("codespace uuid is empty")
	}
	if err := p.ensureScriptStateFiles(ctx, instanceName); err != nil {
		return nil, err
	}
	scriptPath := filepath.Join(runtimeScriptDir, scriptName)
	if err := p.writeRuntimeFile(ctx, instanceName, scriptPath, script, 0o700, "file"); err != nil {
		return nil, err
	}
	previousEnvFile, _, err := p.readRuntimeFile(ctx, instanceName, runtimeSharedEnvFilePath)
	if err != nil {
		return nil, err
	}
	rawPreviousEnvFile := previousEnvFile
	previousEnvFile = trimSharedEnvNULPadding(rawPreviousEnvFile)
	sharedEnv, err := parseSharedEnv(previousEnvFile, nil)
	if err != nil {
		return nil, fmt.Errorf("parse existing shared env: %w", err)
	}
	if previousEnvFile != rawPreviousEnvFile {
		if err := p.writeRuntimeSharedEnvFile(ctx, instanceName, previousEnvFile); err != nil {
			return nil, err
		}
	}
	resultPath := filepath.Join(runtimeScriptResultDir, resultFileName(request.Operation, stage))
	if err := p.writeRuntimeFile(ctx, instanceName, resultPath, "", 0o600, "file"); err != nil {
		return nil, err
	}

	environment := make(map[string]string, len(sharedEnv)+32)
	for name, value := range sharedEnv {
		environment[name] = value
	}
	for name, value := range p.predefinedScriptEnv(request, stage, resultPath) {
		environment[name] = value
	}
	if _, ok := sharedEnv["CODESPACE_WORKSPACE_DIR"]; !ok && request.Workdir != "" {
		environment["CODESPACE_WORKSPACE_DIR"] = request.Workdir
	}
	execErr := p.execCommand(ctx, instanceName, []string{p.bootstrap.Shell, scriptPath}, environment, "/")
	resultContent, _, err := p.readRuntimeFile(ctx, instanceName, resultPath)
	if err != nil {
		_ = p.writeRuntimeSharedEnvFile(ctx, instanceName, previousEnvFile)
		if execErr != nil {
			return nil, newRecoverableScriptFailure(stage, fmt.Sprintf("script command failed and result could not be read: %v; %v", execErr, err))
		}
		return nil, newRecoverableScriptFailure(stage, fmt.Sprintf("script result could not be read: %v", err))
	}
	if err := validateScriptResult(resultContent, stage); err != nil {
		_ = p.writeRuntimeSharedEnvFile(ctx, instanceName, previousEnvFile)
		if execErr != nil {
			return nil, fmt.Errorf("script command failed: %v; %w", execErr, err)
		}
		return nil, err
	}
	currentEnvFile, _, err := p.readRuntimeFile(ctx, instanceName, runtimeSharedEnvFilePath)
	if err != nil {
		_ = p.writeRuntimeSharedEnvFile(ctx, instanceName, previousEnvFile)
		return nil, newRecoverableScriptFailure(stage, fmt.Sprintf("shared env could not be read: %v", err))
	}
	currentEnv, err := parseSharedEnv(currentEnvFile, predefinedScriptEnvNames())
	if err != nil {
		_ = p.writeRuntimeSharedEnvFile(ctx, instanceName, previousEnvFile)
		return nil, newRecoverableScriptFailure(stage, fmt.Sprintf("shared env is invalid: %v", err))
	}
	if err := p.writeRuntimeSharedEnvFile(ctx, instanceName, encodeSharedEnv(currentEnv)); err != nil {
		return nil, err
	}
	return currentEnv, nil
}

func (p *IncusProvisioner) writeRuntimeSharedEnvFile(ctx context.Context, instanceName, content string) error {
	return p.execCommand(ctx, instanceName, []string{p.bootstrap.Shell, "-c", `
set -eu
tmp="${CODESPACE_ENV}.tmp.$$"
umask 177
printf '%s' "$CODESPACE_ENV_CONTENT" > "$tmp"
chmod 600 "$tmp"
mv "$tmp" "$CODESPACE_ENV"
`}, map[string]string{
		"CODESPACE_ENV":         runtimeSharedEnvFilePath,
		"CODESPACE_ENV_CONTENT": content,
	}, "/")
}

func (p *IncusProvisioner) ensureScriptStateFiles(ctx context.Context, instanceName string) error {
	for _, path := range []string{runtimeCredentialDir, runtimeGitCredentialDir, runtimeManifestDir, runtimeScriptResultDir, runtimeScriptParentDir, runtimeScriptDir} {
		if err := p.writeRuntimeFile(ctx, instanceName, path, "", runtimeCredentialDirMode, "directory"); err != nil {
			return err
		}
	}
	return nil
}

func (p *IncusProvisioner) writeRuntimeFile(ctx context.Context, instanceName, path, content string, mode int, kind string) error {
	args := incus.InstanceFileArgs{
		Content:   strings.NewReader(content),
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

func (p *IncusProvisioner) predefinedScriptEnv(request BootstrapRequest, stage, resultPath string) map[string]string {
	phase := ""
	if stage == "prepare-workspace" {
		phase = scriptPhasePrepare
	} else if stage == "start-environment" {
		phase = scriptPhaseActivate
	}
	repoURL := request.RepoCloneHTTPURL
	if request.GitProtocol == "ssh" && request.RepoCloneSSHURL != "" {
		repoURL = request.RepoCloneSSHURL
	}
	authPrefix, httpsPrefix, _ := buildGitURLPrefixes(request.RepoCloneHTTPURL, "codespace", request.GiteaToken)
	values := map[string]string{
		"HOME":                      p.bootstrap.HomeDir,
		"CODESPACE_UUID":            request.CodespaceUUID,
		"CODESPACE_NAME":            request.CodespaceName,
		"CODESPACE_OWNER_NAME":      request.CodespaceOwnerName,
		"CODESPACE_OPERATION":       string(request.Operation),
		"CODESPACE_SCRIPT_PHASE":    phase,
		"CODESPACE_RESULT":          resultPath,
		"CODESPACE_ENV":             runtimeSharedEnvFilePath,
		"CODESPACE_WORKSPACES_DIR":  defaultWorkspaceRoot,
		"GITEA_SERVER_URL":          request.ServerURL,
		"GITEA_TOKEN":               request.GiteaToken,
		"GITEA_REPO_CLONE_HTTP_URL": request.RepoCloneHTTPURL,
		"GITEA_REPO_CLONE_SSH_URL":  request.RepoCloneSSHURL,
		"GITEA_GIT_PROTOCOL":        request.GitProtocol,
		"GITEA_REPO_WEB_URL":        request.RepoWebURL,
		"GITEA_REPO_ID":             strconv.FormatInt(request.RepoID, 10),
		"GITEA_REPO_FULL_NAME":      request.RepoFullName,
		"GITEA_REPO_NAME":           request.RepoName,
		"GITEA_OWNER_ID":            strconv.FormatInt(request.OwnerID, 10),
		"GITEA_OWNER_NAME":          request.OwnerName,
		"GITEA_OWNER_TYPE":          request.OwnerType,
		"GITEA_OWNER_DISPLAY_NAME":  request.OwnerDisplayName,
		"GITEA_REF_TYPE":            request.RefType,
		"GITEA_REF_NAME":            request.RefName,
		"GITEA_START_REF":           request.StartRef,
		"GITEA_COMMIT_SHA":          request.CommitSHA,
		"CODESPACE_REPO_NAME":       request.RepoName,
		"GITEA_LEGACY_REPO_URL":     repoURL,
		"GITEA_LEGACY_AUTH_PREFIX":  authPrefix,
		"GITEA_LEGACY_HTTPS_PREFIX": httpsPrefix,
	}
	return values
}

func predefinedScriptEnvNames() map[string]struct{} {
	names := map[string]struct{}{}
	for name := range (&IncusProvisioner{}).predefinedScriptEnv(BootstrapRequest{}, "", "") {
		names[name] = struct{}{}
	}
	return names
}

func resultFileName(operation ScriptOperation, stage string) string {
	return string(operation) + "-" + stage + ".json"
}

func validateScriptResult(content, stage string) error {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var result scriptResult
	if err := decoder.Decode(&result); err != nil {
		return newRecoverableScriptFailure(stage, fmt.Sprintf("decode script result: %v", err))
	}
	if strings.TrimSpace(result.Stage) != stage {
		return newRecoverableScriptFailure(stage, fmt.Sprintf("script stage %q does not match %q", result.Stage, stage))
	}
	switch strings.TrimSpace(result.Outcome) {
	case scriptResultDone:
		return nil
	case scriptResultRecoverableFailed:
		return &ScriptFailureError{Kind: ScriptFailureRecoverable, Outcome: result.Outcome, Stage: stage}
	case scriptResultUnrecoverableFailed:
		return &ScriptFailureError{Kind: ScriptFailureUnrecoverable, Outcome: result.Outcome, Stage: stage}
	default:
		return newRecoverableScriptFailure(stage, fmt.Sprintf("script outcome %q at stage %s", result.Outcome, stage))
	}
}

func newRecoverableScriptFailure(stage, message string) error {
	return &ScriptFailureError{
		Kind:    ScriptFailureRecoverable,
		Stage:   stage,
		Message: message,
	}
}

func parseSharedEnv(content string, ignored map[string]struct{}) (map[string]string, error) {
	content = trimSharedEnvNULPadding(content)
	if strings.ContainsRune(content, '\x00') {
		return nil, fmt.Errorf("shared env contains NUL")
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
		if !sharedEnvNamePattern.MatchString(name) {
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

func trimSharedEnvNULPadding(content string) string {
	return strings.TrimRight(content, "\x00")
}

func encodeSharedEnv(values map[string]string) string {
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
