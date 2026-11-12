// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package devcontainerruntime

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"

	"gitea.dev/codespace/devcontainer"
	containerdocker "gitea.dev/codespace/devcontainer/docker"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"gitea.dev/codespace/internal/webide"
)

//go:embed builtin/configure-git.sh
var configureGitScript string

//go:embed builtin/install-ca.sh
var installCAScript string

// Outcome records the Dev Container and the optional platform services that
// were actually started for this lifecycle action.
type Outcome struct {
	Environment   *devcontainer.State
	WebIDEStarted bool
}

// Apply executes one validated lifecycle request against the local Docker daemon.
func Apply(ctx context.Context, request Request, stdout, stderr io.Writer) (_ *Outcome, returnErr error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if request.DevContainer.WebIDE.Enabled() && (request.Action == "create" || request.Action == "resume") {
		err := webide.Prepare(string(request.DevContainer.WebIDE.Provider), request.HostUser.UID, request.HostUser.GID)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "Warning: Web IDE did not start and Codespace access remains available: prepare Provider bundle: %v\n", err)
			request.DevContainer.WebIDE = WebIDEConfiguration{}
		}
	}
	engine, err := containerdocker.New(ctx, stdout, stderr)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, engine.Close()) }()
	switch request.Action {
	case "create":
		options, err := buildCreateOptions(request, platformGiteaCLIPath)
		if err != nil {
			return nil, err
		}
		options.CustomizeImage = imageCustomization(request.DevContainer.LifecycleScript)
		options.PrepareLifecycle = func(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
			if err := prepareContainerStart(ctx, engine, state, request); err != nil {
				return err
			}
			_, _, err := ConfiguredEndpoints(state)
			return err
		}
		state, err := engine.Create(ctx, options)
		if err != nil {
			return nil, err
		}
		webIDEStarted := startWebIDE(ctx, engine, state, request.Secrets, request.DevContainer, true, stderr)
		return &Outcome{Environment: state, WebIDEStarted: webIDEStarted}, nil
	case "resume":
		state, err := engine.Start(ctx, request.Environment, request.HostUser, request.LocalEnvironment, request.Secrets, func(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
			return prepareContainerStart(ctx, engine, state, request)
		}, request.RunLifecycle)
		if err != nil {
			// Start may have started some or all containers before preparing
			// Runtime-owned mounts and lifecycle hooks. Always stop the saved
			// environment on failure so a later resume sees a coherent state.
			stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			_, stopErr := engine.Stop(stopContext, request.Environment)
			return nil, errors.Join(err, stopErr)
		}
		webIDEStarted := startWebIDE(ctx, engine, state, request.Secrets, request.DevContainer, false, stderr)
		return &Outcome{Environment: state, WebIDEStarted: webIDEStarted}, nil
	case "stop":
		lifecycleErr := runTemplateLifecycle(ctx, request, engine, request.Environment, "stop")
		if request.BeforeContainerStop != nil {
			request.BeforeContainerStop()
		}
		stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		state, stopErr := engine.Stop(stopContext, request.Environment)
		return &Outcome{Environment: state}, errors.Join(lifecycleErr, stopErr)
	case "delete":
		if request.BeforeContainerStop != nil {
			request.BeforeContainerStop()
		}
		state, err := engine.Stop(ctx, request.Environment)
		return &Outcome{Environment: state}, err
	default:
		state, err := engine.Inspect(ctx, request.Environment)
		return &Outcome{Environment: state}, err
	}
}

func prepareContainerStart(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, request Request) error {
	// Login-shell probing can load the profile; keep credentials out of saved state.
	for name := range request.Secrets {
		delete(state.RemoteEnvironment, name)
	}
	if err := prepareRuntimeEnvironment(ctx, engine, state); err != nil {
		return err
	}
	if err := prepareUSBIPContainer(ctx, engine, state, request.USBIP); err != nil {
		return err
	}
	maps.Copy(state.RemoteEnvironment, request.Native.Environment)
	if err := configureRuntimeCommands(ctx, engine, state, request); err != nil {
		return err
	}
	if err := runTemplateLifecycle(ctx, request, engine, state, "start"); err != nil {
		return err
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, "0", "/", []string{"/bin/sh", "-c", installCAScript}, nil, nil); err != nil {
		return fmt.Errorf("configure platform CA in Dev Container: %w", err)
	}
	state.RemoteEnvironment["NODE_EXTRA_CA_CERTS"] = containerCABundlePath
	state.RemoteEnvironment["SSL_CERT_FILE"] = containerCABundlePath
	values := map[string]string{"GITEA_URL": request.GiteaURL}
	if request.Action == "create" {
		values["GITEA_GIT_USER_NAME"] = request.GitUserName
		values["GITEA_GIT_USER_EMAIL"] = request.GitUserEmail
		values["GITEA_WORKSPACE"] = state.RemoteWorkdir
	} else {
		values["GITEA_CONFIGURE_CA_ONLY"] = "true"
	}
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", configureGitScript}, values, nil); err != nil {
		return fmt.Errorf("configure Git in Dev Container: %w", err)
	}
	return nil
}

func runTemplateLifecycle(ctx context.Context, request Request, engine *containerdocker.Engine, state *devcontainer.State, action string) error {
	if strings.TrimSpace(request.DevContainer.LifecycleScript) == "" {
		return nil
	}
	run := func(ctx context.Context) error {
		if action == "stop" {
			running, err := engine.ContainerRunning(ctx, state.PrimaryContainerID)
			if cerrdefs.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("inspect Dev Container before stop lifecycle: %w", err)
			}
			if !running {
				return nil
			}
		}
		lifecycleContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := engine.Run(lifecycleContext, state.PrimaryContainerID, "0", state.RemoteWorkdir, []string{"/bin/sh", templateLifecycleScriptPath, action}, nil, nil); err != nil {
			return fmt.Errorf("run environment-template lifecycle %s: %w", action, err)
		}
		return nil
	}
	if request.RunTemplateLifecycle != nil {
		return request.RunTemplateLifecycle(ctx, action, run)
	}
	return run(ctx)
}

func startWebIDE(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, secrets map[string]string, configuration Configuration, initialize bool, stderr io.Writer) bool {
	if !configuration.WebIDE.Enabled() {
		return false
	}
	values := devcontainer.ProcessEnvironment(state.RemoteEnvironment, secrets)
	values["CODESPACE_WEB_IDE_PROVIDER"] = string(configuration.WebIDE.Provider)
	values["CODESPACE_WEB_IDE_PROGRAM_DIR"] = webide.ContainerProgramPath
	values["CODESPACE_WEB_IDE_DATA_DIR"] = webide.ContainerDataPath
	values["CODESPACE_WEB_IDE_WORKSPACE"] = state.RemoteWorkdir
	values["CODESPACE_WEB_IDE_ADDRESS"] = fmt.Sprintf("127.0.0.1:%d", runtimeendpoint.WorkspaceEndpointPort)
	templateOptions := json.RawMessage(configuration.WebIDE.Options.Raw)
	if len(templateOptions) == 0 {
		templateOptions = json.RawMessage(`{}`)
	}
	repositoryOptions := state.Configuration.Customizations[string(configuration.WebIDE.Provider)]
	if len(repositoryOptions) == 0 {
		repositoryOptions = json.RawMessage(`{}`)
	}
	payload, startErr := json.Marshal(struct {
		ProtocolVersion   int             `json:"protocolVersion"`
		Initialize        bool            `json:"initialize"`
		TemplateOptions   json.RawMessage `json:"templateOptions"`
		RepositoryOptions json.RawMessage `json:"repositoryOptions"`
	}{1, initialize, templateOptions, repositoryOptions})
	if startErr == nil {
		prepare := webide.ContainerProgramPath + "/" + webide.PrepareProgram
		if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{prepare}, values, bytes.NewReader(payload)); err != nil {
			startErr = fmt.Errorf("prepare %s Web IDE: %w", configuration.WebIDE.Provider, err)
		}
	}
	if startErr == nil {
		serve := webide.ContainerProgramPath + "/" + webide.ServeProgram
		if err := engine.RunDetached(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{serve}, values); err != nil {
			startErr = fmt.Errorf("start %s Web IDE Provider: %w", configuration.WebIDE.Provider, err)
		}
	}
	if startErr != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: Web IDE did not start and Codespace access remains available: %v\n", startErr)
		return false
	}
	if err := engine.RunPostAttach(ctx, state, secrets); err != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: Web IDE postAttachCommand failed without affecting Codespace access: %v\n", err)
	}
	return true
}
