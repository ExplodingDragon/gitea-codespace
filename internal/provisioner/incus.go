// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

const defaultCodespaceRoot = "/codespace"

// IncusConfig configures one Incus-backed provisioner.
type IncusConfig struct {
	Project       string
	Remote        string
	UnixSocket    string
	CodespaceRoot string
	Bootstrap     BootstrapConfig
}

// IncusProvisioner provisions codespace as Incus instances.
type IncusProvisioner struct {
	client        incus.InstanceServer
	codespaceRoot string
	bootstrap     BootstrapConfig
}

// NewIncus creates one Incus-backed provisioner.
func NewIncus(config IncusConfig) (*IncusProvisioner, error) {
	client, err := connectIncus(config)
	if err != nil {
		return nil, fmt.Errorf("connect incus: %w", err)
	}

	codespaceRoot := config.CodespaceRoot
	if codespaceRoot == "" {
		codespaceRoot = defaultCodespaceRoot
	}

	return &IncusProvisioner{
		client:        client,
		codespaceRoot: codespaceRoot,
		bootstrap:     config.Bootstrap,
	}, nil
}

// CreateOrStart creates or starts one instance.
func (p *IncusProvisioner) CreateOrStart(spec InstanceSpec) (*Instance, error) {
	instanceName := spec.Name
	if instanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}

	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		if !isNotFoundError(err) {
			return nil, fmt.Errorf("get instance %s: %w", instanceName, err)
		}
		if err := p.createInstance(spec); err != nil {
			return nil, fmt.Errorf("create instance %s: %w", instanceName, err)
		}
		instance, _, err = p.client.GetInstance(instanceName)
		if err != nil {
			return nil, fmt.Errorf("reload instance %s: %w", instanceName, err)
		}
	}

	return p.startExistingInstance(spec, instance.Name)
}

// StartExisting starts one existing instance.
func (p *IncusProvisioner) StartExisting(spec InstanceSpec) (*Instance, error) {
	instanceName := spec.Name
	if instanceName == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return nil, fmt.Errorf("instance %s does not exist", instanceName)
		}
		return nil, fmt.Errorf("get instance %s: %w", instanceName, err)
	}
	return p.startExistingInstance(spec, instance.Name)
}

// Bootstrap clones the repo, configures git auth, and runs the init script.
func (p *IncusProvisioner) Bootstrap(instanceName string, request BootstrapRequest) error {
	if instanceName == "" {
		return fmt.Errorf("instance name is empty")
	}
	if request.Workdir == "" {
		return fmt.Errorf("workdir is empty")
	}
	if request.RepoURL == "" {
		return fmt.Errorf("repo url is empty")
	}

	authPrefix, httpsPrefix, err := buildGitURLPrefixes(request.RepoURL, request.GitUsername, request.GitToken)
	if err != nil {
		return fmt.Errorf("build git url prefixes: %w", err)
	}

	environment := map[string]string{
		"HOME":              p.bootstrap.HomeDir,
		"CODESPACE_ID":      request.CodespaceID,
		"CODESPACE_TOKEN":   request.RuntimeToken,
		"CODESPACE_API_URL": request.RuntimeAPIURL,
		"CODESPACE_ROOT":    request.Workdir,
		"CODESPACE_DIR":     request.Workdir,
		"CODESPACE_PARENT":  filepath.Dir(request.Workdir),
		"REPO_URL":          request.RepoURL,
		"REPO_FULL_NAME":    request.RepoFullName,
		"START_REF":         request.StartRef,
		"START_SHA":         request.StartSHA,
		"INIT_SCRIPT":       request.InitScript,
		"GIT_AUTH_PREFIX":   authPrefix,
		"GIT_HTTPS_PREFIX":  httpsPrefix,
	}

	script := strings.TrimSpace(`
set -eu
mkdir -p "$HOME" "$CODESPACE_PARENT"
if [ ! -d "$CODESPACE_DIR/.git" ]; then
  git clone "$REPO_URL" "$CODESPACE_DIR"
fi
git -C "$CODESPACE_DIR" remote set-url origin "$REPO_URL"
if [ -n "$GIT_AUTH_PREFIX" ] && [ -n "$GIT_HTTPS_PREFIX" ]; then
  git config --global credential.helper store
  git config --global url."$GIT_AUTH_PREFIX".insteadOf "$GIT_HTTPS_PREFIX"
fi
if [ -n "$START_REF" ]; then
  git -C "$CODESPACE_DIR" fetch origin "$START_REF" --tags --prune
else
  git -C "$CODESPACE_DIR" fetch --all --tags --prune
fi
if [ -n "$START_SHA" ]; then
  git -C "$CODESPACE_DIR" checkout --detach "$START_SHA"
elif [ -n "$START_REF" ]; then
  git -C "$CODESPACE_DIR" checkout --detach FETCH_HEAD
fi
if [ -n "$INIT_SCRIPT" ]; then
  cd "$CODESPACE_DIR"
  ` + p.bootstrap.Shell + ` -lc "$INIT_SCRIPT"
fi
`)

	if err := p.execScript(instanceName, script, environment, request.Workdir); err != nil {
		return fmt.Errorf("bootstrap instance %s: %w", instanceName, err)
	}
	return nil
}

// Stop stops one instance if it exists.
func (p *IncusProvisioner) Stop(instanceName string) error {
	if instanceName == "" {
		return nil
	}

	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("get instance %s: %w", instanceName, err)
	}
	if strings.EqualFold(instance.Status, "Stopped") {
		return nil
	}

	operation, err := p.client.UpdateInstanceState(instanceName, api.InstanceStatePut{
		Action:  "stop",
		Force:   true,
		Timeout: -1,
	}, "")
	if err != nil {
		return fmt.Errorf("stop instance %s: %w", instanceName, err)
	}
	if err := operation.Wait(); err != nil {
		return fmt.Errorf("wait stop instance %s: %w", instanceName, err)
	}
	return nil
}

// Delete deletes one instance if it exists.
func (p *IncusProvisioner) Delete(instanceName string) error {
	if instanceName == "" {
		return nil
	}

	if err := p.Stop(instanceName); err != nil {
		return err
	}

	operation, err := p.client.DeleteInstance(instanceName)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("delete instance %s: %w", instanceName, err)
	}
	if err := operation.Wait(); err != nil {
		return fmt.Errorf("wait delete instance %s: %w", instanceName, err)
	}
	return nil
}

func (p *IncusProvisioner) createInstance(spec InstanceSpec) error {
	instanceType := spec.Type
	if instanceType == "" {
		instanceType = "container"
	}

	imageAlias := spec.Image
	if imageAlias == "" {
		imageAlias = "images:debian/12"
	}

	request := api.InstancesPost{
		Name: spec.Name,
		Type: api.InstanceType(instanceType),
		Source: api.InstanceSource{
			Type:     "image",
			Alias:    trimRemoteAlias(imageAlias),
			Server:   imageServerForAlias(imageAlias),
			Protocol: "simplestreams",
		},
	}

	operation, err := p.client.CreateInstance(request)
	if err != nil {
		return fmt.Errorf("create instance request: %w", err)
	}
	if err := operation.Wait(); err != nil {
		return fmt.Errorf("wait create instance: %w", err)
	}
	return nil
}

func (p *IncusProvisioner) startInstance(instanceName string) error {
	instance, _, err := p.client.GetInstance(instanceName)
	if err != nil {
		return fmt.Errorf("get instance %s: %w", instanceName, err)
	}
	if strings.EqualFold(instance.Status, "Running") {
		return nil
	}

	operation, err := p.client.UpdateInstanceState(instanceName, api.InstanceStatePut{
		Action:  "start",
		Timeout: -1,
	}, "")
	if err != nil {
		return fmt.Errorf("start instance request: %w", err)
	}
	if err := operation.Wait(); err != nil {
		return fmt.Errorf("wait start instance: %w", err)
	}
	return nil
}

func (p *IncusProvisioner) startExistingInstance(spec InstanceSpec, instanceName string) (*Instance, error) {
	if err := p.startInstance(instanceName); err != nil {
		return nil, fmt.Errorf("start instance %s: %w", instanceName, err)
	}
	return &Instance{
		Name:         instanceName,
		State:        codespacev1.CodespaceStatus_CODESPACE_STATUS_RUNNING,
		Workdir:      filepath.Join(p.codespaceRoot, repoDirName(spec.RepoFullName)),
		Image:        spec.Image,
		Type:         spec.Type,
		RepoFullName: spec.RepoFullName,
	}, nil
}

func connectIncus(config IncusConfig) (incus.InstanceServer, error) {
	if config.Remote != "" {
		client, err := incus.ConnectIncus(config.Remote, nil)
		if err != nil {
			return nil, fmt.Errorf("connect remote %s: %w", config.Remote, err)
		}
		return withProject(client, config.Project), nil
	}

	client, err := incus.ConnectIncusUnix(config.UnixSocket, nil)
	if err != nil {
		return nil, fmt.Errorf("connect unix socket %q: %w", config.UnixSocket, err)
	}
	return withProject(client, config.Project), nil
}

func withProject(client incus.InstanceServer, project string) incus.InstanceServer {
	if project == "" {
		return client
	}
	return client.UseProject(project)
}

func trimRemoteAlias(value string) string {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return value
}

func imageServerForAlias(value string) string {
	if strings.HasPrefix(value, "images:") {
		return "https://images.linuxcontainers.org"
	}
	return ""
}

func buildGitURLPrefixes(repoURL string, username string, token string) (string, string, error) {
	if username == "" || token == "" {
		return "", "", nil
	}

	parsedURL, err := url.Parse(repoURL)
	if err != nil {
		return "", "", fmt.Errorf("parse repo url %q: %w", repoURL, err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return "", "", nil
	}

	baseURL := &url.URL{
		Scheme: parsedURL.Scheme,
		Host:   parsedURL.Host,
		Path:   "/",
	}
	authURL := *baseURL
	authURL.User = url.UserPassword(username, token)
	return authURL.String(), baseURL.String(), nil
}

func (p *IncusProvisioner) execScript(
	instanceName string,
	script string,
	environment map[string]string,
	workdir string,
) error {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	operation, err := p.client.ExecInstance(instanceName, api.InstanceExecPost{
		Command:     []string{p.bootstrap.Shell, "-lc", script},
		Environment: environment,
		Cwd:         workdir,
		User:        p.bootstrap.User,
		Group:       p.bootstrap.Group,
	}, &incus.InstanceExecArgs{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("exec bootstrap command: %w", err)
	}
	if err := operation.Wait(); err != nil {
		return fmt.Errorf(
			"wait bootstrap command: %w (stdout=%q stderr=%q)",
			err,
			stdout.String(),
			stderr.String(),
		)
	}
	return nil
}

func isNotFoundError(err error) bool {
	var apiStatus api.StatusError
	return errors.As(err, &apiStatus) && apiStatus.Status() == 404
}
