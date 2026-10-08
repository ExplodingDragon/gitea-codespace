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

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/distribution/reference"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const managerLeaderLabel = "codespace.gitea.dev/manager-leader"

type ManagerOptions struct {
	Namespace              string
	ManagerURL             string
	ComponentURL           string
	PlatformImage          string
	ImagePullSecrets       []string
	IdentityIssuer         string
	HealthAddress          string
	GatewayParentName      string
	GatewayParentNamespace string
	GatewayHTTPSectionName string
	GatewaySSHSectionName  string
	PodName                string
}

type Manager struct {
	manager.Manager
	Leadership   *Leadership
	AgentControl *AgentControlServer
	Components   *ComponentServer
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
	if options.Namespace == "" || options.ManagerURL == "" || options.ComponentURL == "" || options.PlatformImage == "" || options.IdentityIssuer == "" {
		return nil, fmt.Errorf("management namespace, Manager and component URLs, platform image and identity issuer are required")
	}
	platformImage, err := reference.ParseNormalizedNamed(options.PlatformImage)
	if err != nil {
		return nil, fmt.Errorf("invalid platform image: %w", err)
	}
	if _, ok := platformImage.(reference.Digested); !ok {
		return nil, fmt.Errorf("platform image must be pinned by digest")
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
	if options.GatewayParentName == "" {
		if options.GatewayParentNamespace != "" || options.GatewayHTTPSectionName != "" || options.GatewaySSHSectionName != "" {
			return nil, fmt.Errorf("gateway parent name is required when route settings are configured")
		}
	} else {
		if len(validation.IsDNS1123Subdomain(options.GatewayParentName)) != 0 || options.GatewayParentNamespace != "" && len(validation.IsDNS1123Label(options.GatewayParentNamespace)) != 0 {
			return nil, fmt.Errorf("gateway parent name and namespace must be valid Kubernetes names")
		}
		for _, section := range []string{options.GatewayHTTPSectionName, options.GatewaySSHSectionName} {
			if section != "" && len(validation.IsDNS1123Label(section)) != 0 {
				return nil, fmt.Errorf("gateway listener names must be valid Kubernetes names")
			}
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
	for _, object := range []client.Object{&api.GiteaSite{}, &api.Codespace{}} {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), object, IdentityUIDIndex, func(object client.Object) []string {
			return []string{string(object.GetUID())}
		}); err != nil {
			return nil, err
		}
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
	keyContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	tickets, err := LoadAccessTickets(keyContext, liveClient, options.Namespace)
	cancel()
	if err != nil {
		return nil, err
	}
	activity := &RuntimeActivityTracker{}
	gatewayChanges := &ChangeNotifier{}
	siteChanges := &ChangeNotifier{}
	for _, object := range []client.Object{&api.GiteaSite{}, &api.Codespace{}, &corev1.Pod{}, &corev1.ConfigMap{}} {
		informer, err := mgr.GetCache().GetInformer(context.Background(), object)
		if err != nil {
			return nil, err
		}
		if _, err := informer.AddEventHandler(kcache.ResourceEventHandlerFuncs{
			AddFunc: func(any) {
				gatewayChanges.Notify()
				siteChanges.Notify()
			},
			UpdateFunc: func(any, any) {
				gatewayChanges.Notify()
				siteChanges.Notify()
			},
			DeleteFunc: func(any) {
				gatewayChanges.Notify()
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
	components := &ComponentServer{Client: liveClient, IdentityIndex: mgr.GetClient(), ManagementNamespace: options.Namespace, Tickets: tickets, Activity: activity, GatewayChanges: gatewayChanges, CacheChanges: gatewayChanges}
	control := &AgentControlServer{Client: liveClient, IdentityIndex: mgr.GetClient(), ManagementNamespace: options.Namespace, Tickets: tickets, Caches: components}
	if err := mgr.Add(&SiteCoordinator{Operations: Operations{Client: liveClient, Authority: &control.Authority, Samples: &control.Samples, Activity: activity, ManagementNamespace: options.Namespace, PlatformImage: options.PlatformImage}, Changes: siteChanges}); err != nil {
		return nil, err
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&api.GiteaSite{}).Complete(&SiteReconciler{Client: liveClient, ManagementNamespace: options.Namespace, ImagePullSecrets: options.ImagePullSecrets, HTTPClient: &http.Client{Timeout: 15 * time.Second}}); err != nil {
		return nil, err
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&api.EnvironmentTemplate{}).Complete(&TemplateReconciler{Client: liveClient}); err != nil {
		return nil, err
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&api.Codespace{}).Owns(&corev1.Pod{}).Complete(&RuntimeReconciler{Client: liveClient, Authority: &control.Authority, ManagementNamespace: options.Namespace, ManagerURL: options.ManagerURL, IdentityIssuer: options.IdentityIssuer, ImagePullSecrets: options.ImagePullSecrets}); err != nil {
		return nil, err
	}
	componentController := ctrl.NewControllerManagedBy(mgr).For(&corev1.ConfigMap{}).Owns(&appsv1.Deployment{})
	if options.GatewayParentName != "" {
		for _, routeType := range []struct{ version, kind string }{{"v1", "HTTPRoute"}, {"v1alpha2", "TCPRoute"}} {
			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: routeType.version, Kind: routeType.kind})
			componentController = componentController.Owns(route)
		}
	}
	if err := componentController.Complete(&ComponentReconciler{
		Client: liveClient, ManagementNamespace: options.Namespace, ManagerURL: options.ComponentURL, IdentityIssuer: options.IdentityIssuer, PlatformImage: options.PlatformImage, ImagePullSecrets: options.ImagePullSecrets,
		GatewayParentName: options.GatewayParentName, GatewayParentNamespace: options.GatewayParentNamespace,
		GatewayHTTPSectionName: options.GatewayHTTPSectionName, GatewaySSHSectionName: options.GatewaySSHSectionName,
	}); err != nil {
		return nil, err
	}
	return &Manager{Manager: mgr, Leadership: leadership, AgentControl: control, Components: components}, nil
}
