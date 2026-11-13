// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/controlplane"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const managerLeaderLabel = "codespace.gitea.dev/manager-leader"

type ManagerOptions struct {
	Namespace          string
	ManagerURL         string
	ComponentURL       string
	PlatformImage      string
	WebIDEProgramSize  string
	ImagePullSecrets   []string
	IdentitySecretName string
	HealthAddress      string
	PodName            string
}

type Manager struct {
	manager.Manager
	Leadership     *Leadership
	AgentControl   *AgentControlServer
	Components     *ComponentServer
	Identity       *IdentityAuthority
	WebIDEResolver webIDEImageResolver
}

// Leadership ties request cancellation to the elected term, including open streams.
type Leadership struct {
	mu        sync.RWMutex
	ctx       context.Context
	Client    client.Client
	Namespace string
	PodName   string
}

func (l *Leadership) NeedLeaderElection() bool { return true }

func (l *Leadership) Start(ctx context.Context) error {
	if l.PodName != "" {
		if err := l.selectLeaderPod(ctx); err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.ctx = ctx
	l.mu.Unlock()
	<-ctx.Done()
	l.mu.Lock()
	l.ctx = nil
	l.mu.Unlock()
	if l.PodName != "" {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.updateLeaderPod(cleanup, l.PodName, false)
	}
	return nil
}

func (l *Leadership) selectLeaderPod(ctx context.Context) error {
	if l.Client == nil || l.Namespace == "" {
		return fmt.Errorf("manager Pod routing is not configured")
	}
	var pods corev1.PodList
	if err := l.Client.List(ctx, &pods, client.InNamespace(l.Namespace), client.MatchingLabels{"app.kubernetes.io/name": "codespace", "app.kubernetes.io/component": "manager"}); err != nil {
		return err
	}
	found := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name != l.PodName {
			continue
		}
		found = true
		if pod.Labels[managerLeaderLabel] != "true" {
			if err := l.updateLeaderPod(ctx, pod.Name, true); err != nil {
				return err
			}
		}
		break
	}
	if !found {
		return fmt.Errorf("manager Pod %s was not found", l.PodName)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name == l.PodName || pod.Labels[managerLeaderLabel] != "true" {
			continue
		}
		if err := l.updateLeaderPod(ctx, pod.Name, false); err != nil {
			return err
		}
	}
	return nil
}

func (l *Leadership) updateLeaderPod(ctx context.Context, name string, selected bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var pod corev1.Pod
		if err := l.Client.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: name}, &pod); err != nil {
			return client.IgnoreNotFound(err)
		}
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		if selected {
			pod.Labels[managerLeaderLabel] = "true"
		} else {
			delete(pod.Labels, managerLeaderLabel)
		}
		return l.Client.Update(ctx, &pod)
	})
}

func (l *Leadership) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.RLock()
		term := l.ctx
		l.mu.RUnlock()
		if term == nil || term.Err() != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "manager is not the active leader", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(term, cancel)
		defer func() { stop(); cancel() }()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func NewManager(config *rest.Config, options ManagerOptions) (*Manager, error) {
	if options.Namespace == "" || options.ManagerURL == "" || options.ComponentURL == "" || options.PlatformImage == "" || options.IdentitySecretName == "" {
		return nil, fmt.Errorf("management namespace, Manager and component URLs, platform image and internal CA Secret name are required")
	}
	if len(validation.IsDNS1123Subdomain(options.IdentitySecretName)) != 0 {
		return nil, fmt.Errorf("internal CA Secret name %q is not a valid Kubernetes name", options.IdentitySecretName)
	}
	if _, err := platformImagePullPolicy(options.PlatformImage); err != nil {
		return nil, err
	}
	programSize, err := resource.ParseQuantity(options.WebIDEProgramSize)
	if err != nil || programSize.Sign() <= 0 {
		return nil, fmt.Errorf("web IDE program volume size must be a positive Kubernetes quantity")
	}
	seenPullSecrets := make(map[string]struct{}, len(options.ImagePullSecrets))
	for _, name := range options.ImagePullSecrets {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return nil, fmt.Errorf("image pull Secret %q is not a valid Kubernetes name", name)
		}
		if _, exists := seenPullSecrets[name]; exists {
			return nil, fmt.Errorf("image pull Secret %q is duplicated", name)
		}
		seenPullSecrets[name] = struct{}{}
	}
	for name, address := range map[string]string{"Agent": options.ManagerURL, "component": options.ComponentURL} {
		endpoint, err := url.Parse(address)
		if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
			return nil, fmt.Errorf("manager %s Service URL must be an HTTPS origin", name)
		}
	}
	types := runtime.NewScheme()
	if err := scheme.AddToScheme(types); err != nil {
		return nil, err
	}
	if err := api.AddToScheme(types); err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:         types,
		LeaderElection: true, LeaderElectionID: "codespace-manager", LeaderElectionNamespace: options.Namespace,
		// Lease expiration follows worker cancellation; a voluntary release must not
		// allow a new writer while the old process is still draining.
		LeaderElectionReleaseOnCancel: false,
		HealthProbeBindAddress:        options.HealthAddress,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return nil, err
	}
	for _, object := range []client.Object{&api.CodespaceComponent{}, &api.GiteaSite{}, &api.Codespace{}} {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), object, IdentityUIDIndex, func(object client.Object) []string {
			return []string{string(object.GetUID())}
		}); err != nil {
			return nil, err
		}
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &api.GiteaSite{}, SiteGatewayUIDIndex, func(object client.Object) []string {
		uid := object.(*api.GiteaSite).Spec.Gateway.UID
		if uid == "" {
			return nil
		}
		return []string{string(uid)}
	}); err != nil {
		return nil, err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &api.Codespace{}, RuntimeSiteUIDIndex, func(object client.Object) []string {
		uid := object.(*api.Codespace).Spec.Site.UID
		if uid == "" {
			return nil
		}
		return []string{string(uid)}
	}); err != nil {
		return nil, err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &api.Codespace{}, RuntimeUUIDIndex, func(object client.Object) []string {
		return []string{object.(*api.Codespace).Spec.RuntimeUUID}
	}); err != nil {
		return nil, err
	}
	leadership := &Leadership{}
	if err := mgr.Add(leadership); err != nil {
		return nil, err
	}
	if err := mgr.AddHealthzCheck("process", healthz.Ping); err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck("process", healthz.Ping); err != nil {
		return nil, err
	}
	// Controllers use live reads for ownership checks; Watch still drives work queues.
	liveClient, err := client.New(config, client.Options{Scheme: types})
	if err != nil {
		return nil, err
	}
	leadership.Client, leadership.Namespace, leadership.PodName = liveClient, options.Namespace, options.PodName
	identityContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	identityAuthority, err := LoadIdentityAuthority(identityContext, liveClient, options.Namespace, options.IdentitySecretName)
	cancel()
	if err != nil {
		return nil, err
	}
	if err := mgr.Add(identityAuthority); err != nil {
		return nil, err
	}
	keyContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	tickets, err := LoadAccessTickets(keyContext, liveClient, options.Namespace)
	cancel()
	if err != nil {
		return nil, err
	}
	activity := &RuntimeActivityTracker{}
	connections := newSiteConnectionStore(liveClient, options.Namespace)
	if err := mgr.Add(connections); err != nil {
		return nil, err
	}
	componentChanges := &ChangeNotifier{}
	siteChanges := &ChangeNotifier{}
	for _, object := range []client.Object{&api.GiteaSite{}, &api.Codespace{}, &api.CodespaceComponent{}, &corev1.Pod{}} {
		informer, err := mgr.GetCache().GetInformer(context.Background(), object)
		if err != nil {
			return nil, err
		}
		if _, err := informer.AddEventHandler(kcache.ResourceEventHandlerFuncs{
			AddFunc: func(any) {
				componentChanges.Notify()
				siteChanges.Notify()
			},
			UpdateFunc: func(previous, current any) {
				if coordinationUpdateChanged(previous, current) {
					componentChanges.Notify()
					siteChanges.Notify()
				}
			},
			DeleteFunc: func(any) {
				componentChanges.Notify()
				siteChanges.Notify()
			},
		}); err != nil {
			return nil, err
		}
	}
	for _, object := range []client.Object{&api.EnvironmentTemplate{}, &corev1.Namespace{}, &corev1.Secret{}, &corev1.PersistentVolumeClaim{}, &corev1.ResourceQuota{}} {
		informer, err := mgr.GetCache().GetInformer(context.Background(), object)
		if err != nil {
			return nil, err
		}
		if _, err := informer.AddEventHandler(kcache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { siteChanges.Notify() },
			UpdateFunc: func(any, any) { siteChanges.Notify() },
			DeleteFunc: func(any) { siteChanges.Notify() },
		}); err != nil {
			return nil, err
		}
	}
	components := &ComponentServer{Client: liveClient, IdentityIndex: mgr.GetClient(), ManagementNamespace: options.Namespace, Tickets: tickets, Activity: activity, Changes: componentChanges, Connections: connections}
	logs := &operationLogs{remote: func(ctx context.Context, ref api.ResourceReference) (codespacev1connect.ManagerServiceClient, error) {
		var site api.GiteaSite
		if err := liveClient.Get(ctx, client.ObjectKey{Name: ref.Name}, &site); err != nil {
			return nil, err
		}
		if site.UID != ref.UID || !site.Spec.Enabled || !site.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("operation log site is no longer active")
		}
		return connections.ForSite(ctx, &site)
	}}
	control := &AgentControlServer{Client: liveClient, IdentityIndex: mgr.GetClient(), ManagementNamespace: options.Namespace, Tickets: tickets, Caches: components, Connections: connections, Logs: logs}
	resourceMetrics, err := metricsclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	coreClient, err := coreclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	diagnostics := &runtimeDiagnostics{Reader: liveClient, Pods: coreClient, Logs: logs}
	control.Diagnostics = diagnostics
	if err := mgr.Add(&ResourceCollector{Reader: mgr.GetClient(), Metrics: resourceMetrics, Nodes: coreClient.RESTClient(), Samples: &control.Samples}); err != nil {
		return nil, err
	}
	if err := mgr.Add(&SiteCoordinator{Operations: Operations{Client: liveClient, Authority: &control.Authority, Samples: &control.Samples, Activity: activity, Connections: connections, Logs: logs, ManagementNamespace: options.Namespace, PlatformImage: options.PlatformImage}, Changes: siteChanges, ObservationReader: mgr.GetClient()}); err != nil {
		return nil, err
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&api.GiteaSite{}).Complete(&SiteReconciler{Client: liveClient, ManagementNamespace: options.Namespace, ImagePullSecrets: options.ImagePullSecrets, HTTPClient: &http.Client{Timeout: 15 * time.Second}}); err != nil {
		return nil, err
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&api.EnvironmentTemplate{}).Complete(&TemplateReconciler{Client: liveClient}); err != nil {
		return nil, err
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&api.Codespace{}).Owns(&corev1.Pod{}).Owns(&corev1.Secret{}).Complete(&RuntimeReconciler{Client: liveClient, Authority: &control.Authority, Identity: identityAuthority, ManagementNamespace: options.Namespace, ManagerURL: options.ManagerURL, ManagerIdentity: identityAuthority.ManagerIdentity(), ImagePullSecrets: options.ImagePullSecrets, WebIDEProgramSize: programSize, Diagnostics: diagnostics}); err != nil {
		return nil, err
	}
	componentController := ctrl.NewControllerManagedBy(mgr).For(&api.CodespaceComponent{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&policyv1.PodDisruptionBudget{})
	if err := componentController.Complete(&ComponentReconciler{
		Client: liveClient, Identity: identityAuthority, ManagementNamespace: options.Namespace, ManagerURL: options.ComponentURL, ManagerIdentity: identityAuthority.ManagerIdentity(), PlatformImage: options.PlatformImage, PlatformRevision: controlplane.BuildVersion(), ImagePullSecrets: options.ImagePullSecrets,
	}); err != nil {
		return nil, err
	}
	return &Manager{
		Manager: mgr, Leadership: leadership, AgentControl: control, Components: components, Identity: identityAuthority,
		WebIDEResolver: newRegistryWebIDEImageResolver(liveClient, options.Namespace, options.ImagePullSecrets),
	}, nil
}
