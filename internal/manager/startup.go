// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/provisioner"
	"golang.org/x/crypto/ssh"
)

func (a *Agent) handleCreate(ctx context.Context, operation *codespacev1.OperationPayload, payload *codespacev1.CreateOperationPayload) error {
	startupInput, err := startupInputFromCreatePayload(operation, payload)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(a.config.Environments, func(environment *codespacev1.EnvironmentTag) bool {
		return environment.GetTag() == startupInput.EnvironmentTag
	}) {
		return fmt.Errorf("environment tag %q is not configured", startupInput.EnvironmentTag)
	}
	if err := a.saveStartupInput(startupInput); err != nil {
		return err
	}
	instance, err := a.provisioner.CreateOrStart(ctx, provisioner.InstanceSpec{
		CodespaceUUID:  operation.GetRuntimeUuid(),
		Name:           runtimeInstanceName(operation.GetRuntimeUuid()),
		RepoFullName:   startupInput.RepoFullName,
		EnvironmentTag: payload.GetEnvironmentTag(),
	})
	if err != nil {
		return err
	}
	repository := payload.GetRepository()
	request := lifecycleRequest(operation, startupInput, instance)
	request.Operation = provisioner.LifecycleOperationCreate
	request.RepoCloneHTTPURL = repository.GetCloneHttpUrl()
	request.RepoCloneSSHURL = repository.GetCloneSshUrl()
	request.StartRef = repository.GetStartRef()
	request.CommitSHA = repository.GetCommitSha()
	request.GitProtocol = gitProtocolName(repository.GetPreferredProtocol())
	return a.runStartupOperation(ctx, operation, payload.GetRuntimeSettings(), instance, nil, request)
}

func (a *Agent) handleResume(ctx context.Context, operation *codespacev1.OperationPayload, payload *codespacev1.ResumeOperationPayload) error {
	startupInput, ok, err := a.loadStartupInput(operation.GetRuntimeUuid())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("startup input is missing for codespace %s", operation.GetRuntimeUuid())
	}
	runtimeEnvironment, err := a.loadRuntimeEnvironment(operation.GetRuntimeUuid())
	if err != nil {
		return err
	}
	instance, err := a.provisioner.StartExisting(ctx, provisioner.InstanceSpec{
		CodespaceUUID: operation.GetRuntimeUuid(),
		Name:          runtimeInstanceName(operation.GetRuntimeUuid()),
	})
	if err != nil {
		return err
	}
	instance.Workdir = runtimeEnvironment.Environment.Workspace
	request := lifecycleRequest(operation, startupInput, instance)
	request.Operation = provisioner.LifecycleOperationResume
	request.Environment = &runtimeEnvironment.Environment
	return a.runStartupOperation(ctx, operation, payload.GetRuntimeSettings(), instance, &runtimeEnvironment, request)
}

func (a *Agent) runStartupOperation(
	ctx context.Context,
	operation *codespacev1.OperationPayload,
	settings *codespacev1.EffectiveCodespaceRuntimeSettings,
	instance *provisioner.Instance,
	existing *provisioner.RuntimeEnvironment,
	request provisioner.LifecycleRequest,
) (returnErr error) {
	codespaceUUID := operation.GetRuntimeUuid()
	a.applyRuntimeSettings(codespaceUUID, settings, time.Now())
	startedUnix := time.Now().Unix()
	logSink := newOperationLogSink(a, operation)
	defer func() {
		flushCtx := context.WithoutCancel(ctx)
		logSink.closeGroups(flushCtx)
		_ = logSink.FlushLifecycleLog(flushCtx)
	}()
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStagePrepareRuntime, startedUnix); err != nil {
		return err
	}
	if err := logSink.startGroup(ctx, "Prepare runtime access"); err != nil {
		return err
	}
	key, err := a.runtimeGitSSHKeySeed(ctx, instance.Name)
	if err != nil {
		return err
	}
	if err := a.provisioner.SeedRuntimeGitSSHKey(ctx, instance.Name, provisioner.RuntimeGitSSHKeySeedRequest{
		GitSSHPrivateKey: key.privateKey,
		GitSSHPublicKey:  key.publicKey,
	}); err != nil {
		return err
	}
	access, err := a.requestRuntimeAccess(ctx, codespaceUUID, operation.GetOperationRversion(), key.publicWire)
	if err != nil {
		return err
	}
	runtimeSecrets, redactionValues, err := runtimeSecretsFromAccess(access)
	if err != nil {
		return err
	}
	logSink.redactionValues = append([]string{access.GetGiteaToken()}, redactionValues...)
	if err := a.provisioner.SeedRuntimeCredentials(ctx, instance.Name, provisioner.RuntimeCredentialSeedRequest{
		CodespaceUUID:    codespaceUUID,
		GiteaToken:       access.GetGiteaToken(),
		GitSSHKnownHosts: access.GetGitSshTrust().GetKnownHostsLines(),
	}); err != nil {
		return err
	}
	request.GiteaToken = access.GetGiteaToken()
	request.ServerURL = access.GetGiteaServerUrl()
	request.LogSink = logSink
	if err := logSink.endGroup(ctx); err != nil {
		return err
	}
	identity := provisioner.SystemIdentity{}
	if request.Operation == provisioner.LifecycleOperationCreate {
		if err := logSink.startGroup(ctx, "Initialize system and workspace"); err != nil {
			return err
		}
		identity, err = a.provisioner.BootstrapSystem(ctx, instance.Name, request)
		if err != nil {
			return err
		}
		if err := logSink.endGroup(ctx); err != nil {
			return err
		}
		if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageBootstrapSystem, startedUnix); err != nil {
			return err
		}
		instance.Workdir = identity.Workspace
		request.Workdir = identity.Workspace
	} else {
		if existing == nil {
			return fmt.Errorf("runtime environment is required for resume")
		}
		identity.UID = existing.User
		identity.GID = existing.Group
		identity.Workspace = existing.Environment.Workspace
		if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageBootstrapSystem, startedUnix); err != nil {
			return err
		}
	}
	startupComplete := false
	defer func() {
		if !startupComplete {
			cleanupCtx, cancel := a.newCleanupContext()
			defer cancel()
			returnErr = errors.Join(returnErr, a.provisioner.ClearRuntimeSecrets(cleanupCtx, instance.Name))
		}
	}()
	if err := a.provisioner.WriteRuntimeSecrets(ctx, instance.Name, identity.UID, identity.GID, runtimeSecrets); err != nil {
		return err
	}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStagePrepareWorkspace, startedUnix); err != nil {
		return err
	}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageStartEnvironment, startedUnix); err != nil {
		return err
	}
	if err := logSink.startGroup(ctx, "Start Dev Container"); err != nil {
		return err
	}
	result, err := a.provisioner.StartEnvironment(ctx, instance.Name, request)
	if err != nil {
		return err
	}
	if err := logSink.endGroup(ctx); err != nil {
		return err
	}
	instance.Workdir = result.Environment.Workspace
	if err := a.saveRuntimeEnvironment(codespaceUUID, provisioner.RuntimeEnvironment{User: identity.UID, Group: identity.GID, Environment: result.Environment}); err != nil {
		return err
	}
	if err := logSink.startGroup(ctx, "Publish access endpoints"); err != nil {
		return err
	}
	if err := a.validateRuntimeReady(ctx, codespaceUUID, instance); err != nil {
		return err
	}
	if err := a.syncRuntimeEndpointManifest(ctx, codespaceUUID, instance); err != nil {
		return err
	}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStagePublishReady, startedUnix); err != nil {
		return err
	}
	if err := a.reportBootMetadata(ctx, operation, instance, RuntimeBootStageReady, startedUnix); err != nil {
		return err
	}
	if err := logSink.endGroup(ctx); err != nil {
		return err
	}
	a.markRuntimeReady(codespaceUUID)
	startupComplete = true
	return nil
}

func (a *Agent) loadRuntimeEnvironment(codespaceUUID string) (provisioner.RuntimeEnvironment, error) {
	if a.runtimeEnvStateStore == nil {
		return provisioner.RuntimeEnvironment{}, fmt.Errorf("runtime environment store is missing")
	}
	environment, ok, err := a.runtimeEnvStateStore.LoadRuntimeEnvironment(codespaceUUID)
	if err != nil {
		return provisioner.RuntimeEnvironment{}, fmt.Errorf("load runtime environment %s: %w", codespaceUUID, err)
	}
	if !ok {
		return provisioner.RuntimeEnvironment{}, fmt.Errorf("runtime environment is missing for codespace %s", codespaceUUID)
	}
	return environment, nil
}

func runtimeSecretsFromAccess(access *codespacev1.RuntimeAccessBundle) (provisioner.RuntimeSecretEnvironment, []string, error) {
	secrets := make(provisioner.RuntimeSecretEnvironment, len(access.GetSecrets()))
	values := make([]string, 0, len(access.GetSecrets()))
	maskValues := make(map[string]struct{}, len(access.GetSecrets()))
	for _, secret := range access.GetSecrets() {
		name, value := secret.GetName(), secret.GetValue()
		if !runtimeSecretNamePattern.MatchString(name) || value == "" || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return nil, nil, fmt.Errorf("runtime secret %q is invalid", name)
		}
		if _, exists := secrets[name]; exists {
			return nil, nil, fmt.Errorf("runtime secret %q is duplicated", name)
		}
		secrets[name] = value
		for _, maskValue := range append([]string{value}, strings.FieldsFunc(value, func(r rune) bool { return r == '\r' || r == '\n' })...) {
			maskValue = strings.TrimSpace(maskValue)
			if maskValue == "" {
				continue
			}
			if _, exists := maskValues[maskValue]; !exists {
				maskValues[maskValue] = struct{}{}
				values = append(values, maskValue)
			}
		}
	}
	slices.SortStableFunc(values, func(a, b string) int { return len(b) - len(a) })
	return secrets, values, nil
}

func lifecycleRequest(
	operation *codespacev1.OperationPayload,
	startupInput StartupInput,
	instance *provisioner.Instance,
) provisioner.LifecycleRequest {
	return provisioner.LifecycleRequest{
		CodespaceUUID:    operation.GetRuntimeUuid(),
		CodespaceName:    runtimeInstanceName(operation.GetRuntimeUuid()),
		UserName:         startupInput.Username,
		GitUserEmail:     startupInput.GitUserEmail,
		RuntimeUserName:  startupInput.RuntimeUserName,
		RepoFullName:     startupInput.RepoFullName,
		Workdir:          instance.Workdir,
		EnvironmentTag:   startupInput.EnvironmentTag,
		DevContainer:     startupInput.DevContainer,
		OperationVersion: operation.GetOperationRversion(),
	}
}

func gitProtocolName(protocol codespacev1.GitProtocol) string {
	switch protocol {
	case codespacev1.GitProtocol_GIT_PROTOCOL_SSH:
		return "ssh"
	default:
		return "http"
	}
}

func (a *Agent) saveStartupInput(input StartupInput) error {
	if err := a.startupInputStore.SaveStartupInput(input); err != nil {
		return fmt.Errorf("save startup input: %w", err)
	}
	return nil
}

func (a *Agent) loadStartupInput(codespaceUUID string) (StartupInput, bool, error) {
	input, ok, err := a.startupInputStore.LoadStartupInput(codespaceUUID)
	if err != nil {
		return StartupInput{}, false, fmt.Errorf("load startup input: %w", err)
	}
	return input, ok, nil
}

type runtimeGitSSHKeySeed struct {
	privateKey []byte
	publicKey  []byte
	publicWire []byte
}

func generateRuntimeGitSSHKey(keyType string) (runtimeGitSSHKeySeed, error) {
	switch normalizeRuntimeGitSSHKeyType(keyType) {
	case gitSSHKeyTypeRSA4096:
		return generateRSARuntimeGitSSHKey()
	default:
		return generateEd25519RuntimeGitSSHKey()
	}
}

func (a *Agent) runtimeGitSSHKeySeed(ctx context.Context, instanceName string) (runtimeGitSSHKeySeed, error) {
	status, err := a.provisioner.CheckCredentials(ctx, instanceName)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("check runtime git ssh key: %w", err)
	}
	if len(bytes.TrimSpace(status.GitSSHPrivateKey)) == 0 && len(bytes.TrimSpace(status.GitSSHPublicKey)) == 0 {
		return generateRuntimeGitSSHKey(a.gitSSHKeyType)
	}
	return runtimeGitSSHKeySeedFromCredentials(status.GitSSHPrivateKey, status.GitSSHPublicKey)
}

func runtimeGitSSHKeySeedFromCredentials(privateKeyPEM, publicKeyAuthorized []byte) (runtimeGitSSHKeySeed, error) {
	privateKeyPEM = bytes.TrimSpace(privateKeyPEM)
	publicKeyAuthorized = bytes.TrimSpace(publicKeyAuthorized)
	if len(privateKeyPEM) == 0 || len(publicKeyAuthorized) == 0 {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("runtime git ssh private and public key files must both exist")
	}
	rawPrivateKey, err := ssh.ParseRawPrivateKey(privateKeyPEM)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("parse runtime git ssh private key: %w", err)
	}
	signer, ok := rawPrivateKey.(crypto.Signer)
	if !ok {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("runtime git ssh private key is not a signer")
	}
	privatePublicKey, err := ssh.NewPublicKey(signer.Public())
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh private key public half: %w", err)
	}
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey(publicKeyAuthorized)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("parse runtime git ssh public key: %w", err)
	}
	if !bytes.Equal(privatePublicKey.Marshal(), publicKey.Marshal()) {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("runtime git ssh private and public key do not match")
	}
	privateKeyCopy := append([]byte(nil), privateKeyPEM...)
	privateKeyCopy = append(privateKeyCopy, '\n')
	return runtimeGitSSHKeySeed{
		privateKey: privateKeyCopy,
		publicKey:  ssh.MarshalAuthorizedKey(publicKey),
		publicWire: publicKey.Marshal(),
	}, nil
}

func generateEd25519RuntimeGitSSHKey() (runtimeGitSSHKeySeed, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("generate runtime git ssh key: %w", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh public key: %w", err)
	}
	privateBlock, err := ssh.MarshalPrivateKey(privateKey, "gitea-codespace")
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh private key: %w", err)
	}
	return runtimeGitSSHKeySeed{
		privateKey: pem.EncodeToMemory(privateBlock),
		publicKey:  ssh.MarshalAuthorizedKey(sshPublicKey),
		publicWire: sshPublicKey.Marshal(),
	}, nil
}

func generateRSARuntimeGitSSHKey() (runtimeGitSSHKeySeed, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("generate runtime git ssh key: %w", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh public key: %w", err)
	}
	privateBlock, err := ssh.MarshalPrivateKey(privateKey, "gitea-codespace")
	if err != nil {
		return runtimeGitSSHKeySeed{}, fmt.Errorf("marshal runtime git ssh private key: %w", err)
	}
	return runtimeGitSSHKeySeed{
		privateKey: pem.EncodeToMemory(privateBlock),
		publicKey:  ssh.MarshalAuthorizedKey(sshPublicKey),
		publicWire: sshPublicKey.Marshal(),
	}, nil
}

func normalizeRuntimeGitSSHKeyType(keyType string) string {
	switch strings.ToLower(strings.TrimSpace(keyType)) {
	case "", gitSSHKeyTypeEd25519:
		return gitSSHKeyTypeEd25519
	case gitSSHKeyTypeRSA4096:
		return gitSSHKeyTypeRSA4096
	default:
		return gitSSHKeyTypeEd25519
	}
}
