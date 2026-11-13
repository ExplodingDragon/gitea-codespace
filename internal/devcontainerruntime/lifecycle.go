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
		_, _ = fmt.Fprintf(stdout, "[INFO] [webide] Validating %s Provider bundle\n", request.DevContainer.WebIDE.Provider)
		err := webide.Prepare(string(request.DevContainer.WebIDE.Provider), request.HostUser.UID, request.HostUser.GID)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "[WARN] [webide] Provider bundle preparation failed; continuing without Web IDE: %v\n", err)
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
		options.Debug = request.Debug
		options.HostCgroup = request.HostCgroup
		options.PrepareLifecycle = func(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
			if err := prepareContainerStart(ctx, engine, state, request, stdout); err != nil {
				return err
			}
			_, _, err := ConfiguredEndpoints(state)
			return err
		}
		state, err := engine.Create(ctx, options)
		if err != nil {
			return nil, err
		}
		webIDEStarted := startWebIDE(ctx, engine, state, request.Secrets, request.DevContainer, true, stdout, stderr)
		return &Outcome{Environment: state, WebIDEStarted: webIDEStarted}, nil
	case "resume":
		state, err := engine.Start(ctx, request.Environment, request.HostUser, request.LocalEnvironment, request.Secrets, func(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State) error {
			return prepareContainerStart(ctx, engine, state, request, stdout)
		}, request.RunLifecycle)
		if err != nil {
			// Start may have started some or all containers before preparing
			// Runtime-owned mounts and lifecycle hooks. Always stop the saved
			// environment on failure so a later resume sees a coherent state.
			if request.Debug {
				engine.WriteDiagnostics(ctx, request.CodespaceUUID)
			}
			stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			_, stopErr := engine.Stop(stopContext, request.Environment)
			return nil, errors.Join(err, stopErr)
		}
		webIDEStarted := startWebIDE(ctx, engine, state, request.Secrets, request.DevContainer, false, stdout, stderr)
		return &Outcome{Environment: state, WebIDEStarted: webIDEStarted}, nil
	case "stop", "delete":
		var lifecycleErr error
		if request.Action == "stop" {
			lifecycleErr = runTemplateLifecycle(ctx, request, engine, request.Environment, "stop", stdout)
		}
		if lifecycleErr != nil {
			_, _ = fmt.Fprintf(stderr, "[WARN] [lifecycle] Template stop command failed; continuing container cleanup: %v\n", lifecycleErr)
			if request.Debug {
				engine.WriteDiagnostics(ctx, request.CodespaceUUID)
			}
		}
		if request.BeforeContainerStop != nil {
			request.BeforeContainerStop()
		}
		stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		state, stopErr := engine.Stop(stopContext, request.Environment)
		if stopErr != nil && lifecycleErr == nil && request.Debug {
			engine.WriteDiagnostics(ctx, request.CodespaceUUID)
		}
		return &Outcome{Environment: state}, stopErr
	default:
		state, err := engine.Inspect(ctx, request.Environment)
		return &Outcome{Environment: state}, err
	}
}

func prepareContainerStart(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, request Request, stdout io.Writer) error {
	_, _ = fmt.Fprintln(stdout, "[INFO] [container] Configuring runtime environment and commands")
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
	if err := runTemplateLifecycle(ctx, request, engine, state, "start", stdout); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "[INFO] [container] Configuring CA certificates")
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
	_, _ = fmt.Fprintln(stdout, "[INFO] [container] Configuring Git access")
	if err := engine.Run(ctx, state.PrimaryContainerID, state.RemoteUser, state.RemoteWorkdir, []string{"/bin/sh", "-c", configureGitScript}, values, nil); err != nil {
		return fmt.Errorf("configure Git in Dev Container: %w", err)
	}
	return nil
}

func runTemplateLifecycle(ctx context.Context, request Request, engine *containerdocker.Engine, state *devcontainer.State, action string, stdout io.Writer) error {
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
		_, _ = fmt.Fprintf(stdout, "[INFO] [lifecycle] Running environment-template %s command\n", action)
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

func startWebIDE(ctx context.Context, engine *containerdocker.Engine, state *devcontainer.State, secrets map[string]string, configuration Configuration, initialize bool, stdout, stderr io.Writer) bool {
	if !configuration.WebIDE.Enabled() {
		return false
	}
	_, _ = fmt.Fprintf(stdout, "[INFO] [webide] Preparing %s settings and extensions\n", configuration.WebIDE.Provider)
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
		_, _ = fmt.Fprintf(stderr, "[WARN] [webide] Provider startup failed; continuing without Web IDE: %v\n", startErr)
		return false
	}
	_, _ = fmt.Fprintf(stdout, "[INFO] [webide] %s preparation completed; start request accepted\n", configuration.WebIDE.Provider)
	if err := engine.RunPostAttach(ctx, state, secrets); err != nil {
		_, _ = fmt.Fprintf(stderr, "[WARN] [lifecycle] Web IDE postAttachCommand failed; Codespace access is unaffected: %v\n", err)
	}
	return true
}
