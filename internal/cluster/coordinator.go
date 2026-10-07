// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/controlplane"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const ComponentUIDLabel = "codespace.gitea.dev/component-uid"

// SiteCoordinator runs only in the elected term. One site's outage cannot delay another's leases.
type SiteCoordinator struct {
	Operations
	Changes *ChangeNotifier
}

func (*SiteCoordinator) NeedLeaderElection() bool { return true }

func (c *SiteCoordinator) Start(ctx context.Context) error {
	type worker struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	workers := make(map[types.UID]worker)
	var wg sync.WaitGroup
	defer func() {
		for _, w := range workers {
			w.cancel()
		}
		wg.Wait()
	}()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var changes <-chan struct{}
	removeSubscriber := func() {}
	if c.Changes != nil {
		changes, removeSubscriber = c.Changes.Subscribe()
	}
	defer removeSubscriber()
	for {
		var sites api.GiteaSiteList
		if err := c.Client.List(ctx, &sites); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.FromContext(ctx).Error(err, "List Gitea sites")
		} else {
			active := make(map[types.UID]bool)
			for i := range sites.Items {
				site := &sites.Items[i]
				if !site.Spec.Enabled || !site.DeletionTimestamp.IsZero() {
					continue
				}
				active[site.UID] = true
				if _, exists := workers[site.UID]; exists {
					continue
				}
				workCtx, cancel := context.WithCancel(ctx)
				w := worker{cancel: cancel, done: make(chan struct{})}
				workers[site.UID] = w
				wg.Add(1)
				go func(name string, uid types.UID) {
					defer wg.Done()
					defer close(w.done)
					c.runSite(workCtx, name, uid)
				}(site.Name, site.UID)
			}
			for uid, w := range workers {
				if !active[uid] {
					w.cancel()
				}
				select {
				case <-w.done:
					delete(workers, uid)
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-changes:
		}
	}
}

func (c *SiteCoordinator) runSite(ctx context.Context, name string, uid types.UID) {
	logger := log.FromContext(ctx).WithValues("site", name)
	var recovered bool
	var generation int64
	var inventoryAt, heartbeatAt time.Time
	heartbeat := 10 * time.Second
	var declaration *codespacev1.DeclareManagerRequest
	var tags []string
	var metadataInterval atomic.Int64
	metadataInterval.Store(int64(10 * time.Second))
	reportCtx, cancelReports := context.WithCancel(ctx)
	reportsDone := make(chan struct{})
	go func() {
		defer close(reportsDone)
		c.reportSite(reportCtx, name, uid, &metadataInterval)
	}()
	defer func() { cancelReports(); <-reportsDone }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var changes <-chan struct{}
	removeSubscriber := func() {}
	if c.Changes != nil {
		changes, removeSubscriber = c.Changes.Subscribe()
	}
	defer removeSubscriber()
	defer func() {
		// The site may already be deleted. Grants also have a finite deadline.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		namespace, err := api.SiteNamespace(name, c.ManagementNamespace)
		if err != nil {
			return
		}
		var rows api.CodespaceList
		if c.Client.List(cleanupCtx, &rows, client.InNamespace(namespace)) == nil {
			for _, cs := range rows.Items {
				if cs.Spec.Site.UID == uid {
					c.Authority.Revoke(cs.UID)
				}
			}
		}
	}()
	for {
		err := func() error {
			var site api.GiteaSite
			if err := c.Client.Get(ctx, types.NamespacedName{Name: name}, &site); err != nil {
				return err
			}
			if site.UID != uid || !site.Spec.Enabled || !site.DeletionTimestamp.IsZero() {
				return fmt.Errorf("site identity is no longer active")
			}
			ready := meta.FindStatusCondition(site.Status.Conditions, "InfrastructureReady")
			if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != site.Generation {
				return fmt.Errorf("site infrastructure and identity are not verified")
			}
			if generation != site.Generation {
				recovered = false
				generation = site.Generation
			}
			remote, err := siteManagerClient(ctx, c.Client, c.ManagementNamespace, &site)
			if err != nil {
				return err
			}
			namespace, err := api.SiteNamespace(name, c.ManagementNamespace)
			if err != nil {
				return err
			}
			var rows api.CodespaceList
			if err := c.Client.List(ctx, &rows, client.InNamespace(namespace)); err != nil {
				return err
			}
			for _, cs := range rows.Items {
				if cs.Spec.Site.UID != uid {
					return fmt.Errorf("site namespace contains a runtime owned by another site")
				}
			}
			if c.Activity != nil {
				current := make(map[string]bool, len(rows.Items))
				for i := range rows.Items {
					current[rows.Items[i].Spec.RuntimeUUID] = true
				}
				c.Activity.RetainSite(string(uid), current)
			}
			for i := range rows.Items {
				if !rows.Items[i].Status.Bound {
					recovered = false
					break
				}
			}
			if !recovered || time.Since(heartbeatAt) >= heartbeat {
				declaration, tags, err = c.declaration(ctx, &site)
				if err != nil {
					return err
				}
				declaration.ManagerRuntimeState = codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_RECOVERING
				if recovered {
					declaration.ManagerRuntimeState = codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE
				}
				response, err := declareManager(ctx, remote, declaration, site.Status.CanonicalURL)
				if err != nil {
					return err
				}
				heartbeat = time.Duration(max(int64(1000), min(response.HeartbeatIntervalMilliseconds, int64(30000)))) * time.Millisecond
				metadataInterval.Store(int64(time.Duration(max(int64(1000), min(response.RuntimeMetadataRefreshIntervalMilliseconds, int64(30000)))) * time.Millisecond))
				heartbeatAt = time.Now()
			}
			bindingsReady := true
			for i := range rows.Items {
				cs := &rows.Items[i]
				if cs.Status.Bound {
					continue
				}
				if err := c.bind(ctx, cs, remote); err != nil {
					bindingsReady = false
					recovered = false
					logger.Error(err, "Bind persisted runtime identity", "codespace", cs.Name)
				}
			}
			if bindingsReady && (!recovered || time.Since(inventoryAt) >= 30*time.Second) {
				if err := c.inventory(ctx, &site, rows.Items, remote); err != nil {
					logger.Error(err, "Report complete runtime inventory")
					bindingsReady = false
					recovered = false
				} else {
					inventoryAt = time.Now()
					if err := c.Client.List(ctx, &rows, client.InNamespace(namespace)); err != nil {
						return err
					}
				}
			}
			c.requestIdleStops(ctx, &site, rows.Items, remote)
			request := &codespacev1.FetchOperationsRequest{ProtocolVersion: 1, WaitTimeoutMilliseconds: 5000}
			for _, cs := range rows.Items {
				if cs.Status.Bound && cs.Status.RecoveryAction == "" && cs.Status.SettledOperationVersion < cs.Spec.Operation.Version {
					request.ObservedOperations = append(request.ObservedOperations, &codespacev1.ObservedOperation{RuntimeUuid: cs.Spec.RuntimeUUID, OperationRversion: cs.Spec.Operation.Version})
				}
			}
			if recovered {
				if err := c.capacity(ctx, &site, rows.Items, tags, request); err != nil {
					// Quota observation failures pause startup, not renewal or cleanup.
					logger.Error(err, "Read startup capacity")
				}
			}
			sent := time.Now()
			response, err := remote.FetchOperations(ctx, connect.NewRequest(request))
			if err != nil {
				return err
			}
			for _, renewal := range response.Msg.RenewedLeases {
				if renewal == nil || renewal.LeaseValidForMilliseconds <= 0 || renewal.LeaseValidForMilliseconds > int64(24*time.Hour/time.Millisecond) {
					return fmt.Errorf("invalid operation renewal")
				}
				for _, cs := range rows.Items {
					if cs.Spec.RuntimeUUID == renewal.RuntimeUuid && cs.Spec.Operation.Version == renewal.OperationRversion && cs.Status.Bound && cs.Status.RecoveryAction == "" && cs.Status.SettledOperationVersion < cs.Spec.Operation.Version && cs.Spec.Operation.Type != "abort_create" && cs.Spec.Operation.Type != "abort_resume" {
						c.Authority.Grant(cs.UID, renewal.OperationRversion, sent.Add(time.Duration(renewal.LeaseValidForMilliseconds)*time.Millisecond))
					}
				}
			}
			for _, operation := range response.Msg.Operations {
				if err := c.Accept(ctx, &site, remote, operation, sent); err != nil {
					logger.Error(err, "Accept operation")
				}
			}
			if !recovered && bindingsReady {
				declaration.ManagerRuntimeState = codespacev1.ManagerRuntimeState_MANAGER_RUNTIME_STATE_ONLINE
				if _, err := declareManager(ctx, remote, declaration, site.Status.CanonicalURL); err != nil {
					return err
				}
				recovered, heartbeatAt = true, time.Now()
			}
			return nil
		}()
		if err != nil && ctx.Err() == nil {
			logger.Error(err, "Coordinate Gitea site")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-changes:
		}
	}
}

func declareManager(ctx context.Context, remote codespacev1connect.ManagerServiceClient, request *codespacev1.DeclareManagerRequest, canonicalURL string) (*codespacev1.DeclareManagerResponse, error) {
	response, err := remote.DeclareManager(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, err
	}
	message := response.Msg
	if message.HeartbeatIntervalMilliseconds <= 0 || message.RuntimeMetadataRefreshIntervalMilliseconds <= 0 || message.ControlPlaneMaxMessageSizeBytes <= 0 {
		return nil, fmt.Errorf("gitea declaration returned invalid coordination limits")
	}
	if strings.TrimRight(message.GiteaWebUrl, "/") != canonicalURL {
		return nil, fmt.Errorf("gitea declaration changed the verified site identity")
	}
	return message, nil
}

func siteManagerClient(ctx context.Context, c client.Client, managementNamespace string, site *api.GiteaSite) (codespacev1connect.ManagerServiceClient, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: site.Spec.Credential.Name}, &secret); err != nil {
		return nil, err
	}
	if secret.UID != site.Spec.Credential.UID || secret.Labels[SiteUIDLabel] != string(site.UID) || len(secret.Data["managerSecret"]) == 0 || !secret.DeletionTimestamp.IsZero() || site.Status.CanonicalURL == "" {
		return nil, fmt.Errorf("site credential ownership or identity is invalid")
	}
	transport := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return controlplane.NewManagerServiceClient(transport, strings.TrimRight(site.Status.CanonicalURL, "/")+"/api/codespace", site.Spec.ManagerID, string(secret.Data["managerSecret"]), api.MaxObjectBytes), nil
}

func (c *SiteCoordinator) declaration(ctx context.Context, site *api.GiteaSite) (*codespacev1.DeclareManagerRequest, []string, error) {
	var gateway corev1.ConfigMap
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: c.ManagementNamespace, Name: site.Spec.Gateway.Name}, &gateway); err != nil {
		return nil, nil, err
	}
	if gateway.UID != site.Spec.Gateway.UID || gateway.Labels[ComponentLabel] != "gateway" || !gateway.DeletionTimestamp.IsZero() {
		return nil, nil, fmt.Errorf("gateway identity changed")
	}
	publicURL, err := url.Parse(gateway.Data["url"])
	if err != nil || publicURL.Hostname() == "" || (publicURL.Scheme != "http" && publicURL.Scheme != "https") || publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" || (publicURL.Path != "" && publicURL.Path != "/") {
		return nil, nil, fmt.Errorf("invalid gateway public URL")
	}
	host, port, err := net.SplitHostPort(gateway.Data["sshAddress"])
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || host == "" || portNumber < 1 || portNumber > 65535 {
		return nil, nil, fmt.Errorf("invalid gateway SSH address")
	}
	var hostKey corev1.Secret
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: c.ManagementNamespace, Name: gateway.Data["sshHostKeySecret"]}, &hostKey); err != nil {
		return nil, nil, err
	}
	if string(hostKey.UID) != gateway.Data["sshHostKeySecretUID"] || hostKey.Labels[ComponentUIDLabel] != string(gateway.UID) || !hostKey.DeletionTimestamp.IsZero() {
		return nil, nil, fmt.Errorf("gateway SSH identity changed")
	}
	signer, err := ssh.ParsePrivateKey(hostKey.Data["hostKey"])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid gateway SSH host key")
	}
	declaration := &codespacev1.DeclareManagerRequest{ProtocolVersion: 1, Version: controlplane.BuildVersion(), GatewayUrl: strings.TrimRight(publicURL.String(), "/"), GatewaySshAddr: gateway.Data["sshAddress"], GatewaySshHostKeyAlgorithm: signer.PublicKey().Type(), GatewaySshHostKeyFingerprintSha256: ssh.FingerprintSHA256(signer.PublicKey()), GatewaySshHostKeyUpdatedUnix: hostKey.CreationTimestamp.Unix()}
	var accepted []string
	seen := make(map[string]bool)
	for _, ref := range site.Spec.Templates {
		var template api.EnvironmentTemplate
		if err := c.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, &template); err != nil {
			return nil, nil, err
		}
		if template.UID != ref.UID || !template.DeletionTimestamp.IsZero() || seen[template.Spec.Tag] {
			return nil, nil, fmt.Errorf("environment identity changed or tag is duplicated")
		}
		if err := template.Validate(); err != nil {
			return nil, nil, err
		}
		seen[template.Spec.Tag] = true
		declaration.Environments = append(declaration.Environments, &codespacev1.EnvironmentTag{Tag: template.Spec.Tag, Description: template.Spec.Description})
		ready := meta.FindStatusCondition(template.Status.Conditions, "Ready")
		if ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == template.Generation {
			accepted = append(accepted, template.Spec.Tag)
		}
	}
	return declaration, accepted, nil
}

func (c *SiteCoordinator) inventory(ctx context.Context, site *api.GiteaSite, rows []api.Codespace, remote codespacev1connect.ManagerServiceClient) error {
	request := &codespacev1.ReportInstancesRequest{ProtocolVersion: 1}
	states := make(map[string]codespacev1.RuntimeState)
	runtimes := make(map[string]*api.Codespace, len(rows))
	for i := range rows {
		cs := &rows[i]
		if _, duplicate := runtimes[cs.Spec.RuntimeUUID]; duplicate {
			return fmt.Errorf("site contains duplicate runtime identities")
		}
		runtimes[cs.Spec.RuntimeUUID] = cs
		state := codespacev1.RuntimeState_RUNTIME_STATE_CREATING
		if cs.Status.Pod.UID != "" {
			var pod corev1.Pod
			err := c.Client.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Pod.Name}, &pod)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil && (pod.UID != cs.Status.Pod.UID || !metav1.IsControlledBy(&pod, cs)) {
				return fmt.Errorf("inventory runtime Pod identity changed")
			}
			if err == nil && pod.DeletionTimestamp.IsZero() && pod.Status.Phase == corev1.PodRunning && cs.Status.Target != nil && cs.Status.Target.Ready {
				state = codespacev1.RuntimeState_RUNTIME_STATE_RUNNING
			}
		} else if meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped") {
			state = codespacev1.RuntimeState_RUNTIME_STATE_STOPPED
		}
		observed := cs.Spec.Operation.Version
		if cs.Status.SettledOperationVersion >= observed {
			observed = 0
		}
		request.Instances = append(request.Instances, &codespacev1.RuntimeInstanceRef{RuntimeUuid: cs.Spec.RuntimeUUID, RuntimeState: state, ObservedOperationRversion: observed})
		states[cs.Spec.RuntimeUUID] = state
	}
	if site.Status.InventoryGeneration == int64(^uint64(0)>>1) {
		return fmt.Errorf("site inventory generation exhausted")
	}
	site.Status.InventoryGeneration++
	if err := c.Client.Status().Update(ctx, site); err != nil {
		return err
	}
	request.InventoryGeneration = site.Status.InventoryGeneration
	response, err := remote.ReportInstances(ctx, connect.NewRequest(request))
	if err != nil {
		return err
	}
	if len(response.Msg.Results) != len(rows) {
		return fmt.Errorf("gitea returned an incomplete inventory decision")
	}
	seen := make(map[string]bool, len(rows))
	for _, result := range response.Msg.Results {
		if result == nil || runtimes[result.RuntimeUuid] == nil || seen[result.RuntimeUuid] {
			return fmt.Errorf("gitea returned an unknown or duplicate inventory identity")
		}
		seen[result.RuntimeUuid] = true
	}
	for _, result := range response.Msg.Results {
		cs := runtimes[result.RuntimeUuid]
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(cs), cs); err != nil {
			return err
		}
		if result.Action != codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_UNSPECIFIED && result.Action != codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_CLEANUP_LOCAL_RUNTIME && result.CurrentOperationRversion < cs.Spec.Operation.Version {
			return fmt.Errorf("inventory operation history regressed")
		}
		if states[cs.Spec.RuntimeUUID] == codespacev1.RuntimeState_RUNTIME_STATE_RUNNING && result.Action != codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_CLEANUP_LOCAL_RUNTIME {
			if validateRuntimeSettings(result.RuntimeSettings) != nil {
				return fmt.Errorf("gitea returned a running Runtime without valid effective settings")
			}
			if c.Activity != nil {
				if err := c.Activity.ObserveSettings(string(site.UID), cs.Spec.RuntimeUUID, result.RuntimeSettings, time.Now()); err != nil {
					return err
				}
			}
		}
		switch result.Action {
		case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_UNSPECIFIED, codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_REFETCH_OPERATION:
			continue
		case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_CLEANUP_LOCAL_RUNTIME:
			cs.Status.RecoveryAction = "delete"
		case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_STOP_LOCAL_RUNTIME:
			cs.Status.RecoveryAction = "stop"
		case codespacev1.RuntimeReconcileAction_RUNTIME_RECONCILE_ACTION_CLEAR_OPERATION_CONTEXT:
			cs.Status.SettledOperationVersion = cs.Spec.Operation.Version
		default:
			return fmt.Errorf("unsupported inventory action")
		}
		c.Authority.Revoke(cs.UID)
		if err := c.Client.Status().Update(ctx, cs); err != nil {
			return err
		}
	}
	return nil
}

func (c *SiteCoordinator) requestIdleStops(ctx context.Context, site *api.GiteaSite, rows []api.Codespace, remote codespacev1connect.ManagerServiceClient) {
	if c.Activity == nil {
		return
	}
	now := time.Now()
	for i := range rows {
		cs := &rows[i]
		eligible := cs.Status.Bound && cs.Status.RecoveryAction == "" && cs.Status.SettledOperationVersion >= cs.Spec.Operation.Version && cs.Status.Pod.UID != "" && cs.Status.Target != nil && cs.Status.Target.Ready && cs.DeletionTimestamp.IsZero()
		settings, ready := c.Activity.IdleRequest(string(site.UID), cs.Spec.RuntimeUUID, string(site.Spec.Gateway.UID), eligible, now)
		if !ready {
			continue
		}
		response, err := remote.RequestIdleStop(ctx, connect.NewRequest(&codespacev1.RequestIdleStopRequest{ProtocolVersion: 1, RuntimeUuid: cs.Spec.RuntimeUUID, ObservedSettings: settings}))
		if err != nil {
			c.Activity.IdleStopFailed(cs.Spec.RuntimeUUID, now)
			log.FromContext(ctx).Error(err, "Request idle stop", "codespace", cs.Name)
			continue
		}
		if err := c.Activity.IdleStopResult(string(site.UID), cs.Spec.RuntimeUUID, response.Msg, now); err != nil {
			c.Activity.IdleStopFailed(cs.Spec.RuntimeUUID, now)
			log.FromContext(ctx).Error(err, "Apply idle stop response", "codespace", cs.Name)
		}
	}
}
