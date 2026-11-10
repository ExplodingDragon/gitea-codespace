package cluster

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ResourceCollector struct {
	Reader  client.Reader
	Metrics metricsclient.MetricsV1beta1Interface
	Nodes   rest.Interface
	Samples *ResourceSamples

	mu       sync.Mutex
	failures map[string]bool
}

type resourceTarget struct {
	codespace *api.Codespace
	pod       *corev1.Pod
	volume    *corev1.PersistentVolumeClaim
}

func (*ResourceCollector) NeedLeaderElection() bool { return true }

func (collector *ResourceCollector) Start(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var volumesAt time.Time
	for {
		volumes := time.Since(volumesAt) >= time.Minute
		if volumes {
			volumesAt = time.Now()
		}
		collector.collect(ctx, volumes)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (collector *ResourceCollector) collectionResult(ctx context.Context, source string, err error) {
	if ctx.Err() != nil {
		return
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if collector.failures == nil {
		collector.failures = make(map[string]bool)
	}
	logger := log.FromContext(ctx)
	if err != nil {
		if !collector.failures[source] {
			logger.Info("Runtime resource metrics unavailable", "source", source, "error", err.Error())
		}
		collector.failures[source] = true
	} else if collector.failures[source] {
		logger.Info("Runtime resource metrics recovered", "source", source)
		delete(collector.failures, source)
	}
}

func (collector *ResourceCollector) collect(ctx context.Context, volumes bool) {
	lookup, cancel := context.WithTimeout(ctx, 10*time.Second)
	var codespaces api.CodespaceList
	err := collector.Reader.List(lookup, &codespaces)
	collector.collectionResult(ctx, "runtime inventory", err)
	if err != nil {
		cancel()
		return
	}
	targets := make(map[types.NamespacedName]resourceTarget, len(codespaces.Items))
	nodes := make(map[string][]resourceTarget)
	for index := range codespaces.Items {
		codespace := &codespaces.Items[index]
		if !codespace.DeletionTimestamp.IsZero() || !codespace.Status.Bound || codespace.Status.Pod.UID == "" {
			continue
		}
		var pod corev1.Pod
		key := types.NamespacedName{Namespace: codespace.Namespace, Name: codespace.Status.Pod.Name}
		if err := collector.Reader.Get(lookup, key, &pod); err != nil || pod.UID != codespace.Status.Pod.UID || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		found := false
		for _, container := range pod.Spec.Containers {
			if container.Name == "runtime" {
				collector.Samples.RecordPodLimits(codespace, container.Resources.Limits.Cpu().MilliValue(), container.Resources.Limits.Memory().Value())
				found = true
				break
			}
		}
		if !found {
			continue
		}
		target := resourceTarget{codespace: codespace, pod: &pod}
		targets[key] = target
		if !volumes || pod.Spec.NodeName == "" || codespace.Status.Volume.UID == "" || codespace.Spec.Runtime.StorageVolumeMode() == corev1.PersistentVolumeBlock {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.Name != "data" || volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != codespace.Status.Volume.Name {
				continue
			}
			var claim corev1.PersistentVolumeClaim
			if err := collector.Reader.Get(lookup, types.NamespacedName{Namespace: pod.Namespace, Name: volume.PersistentVolumeClaim.ClaimName}, &claim); err != nil || claim.UID != codespace.Status.Volume.UID || !claim.DeletionTimestamp.IsZero() {
				continue
			}
			if claim.Spec.VolumeMode != nil && *claim.Spec.VolumeMode == corev1.PersistentVolumeBlock {
				continue
			}
			target.volume = &claim
			nodes[pod.Spec.NodeName] = append(nodes[pod.Spec.NodeName], target)
			break
		}
	}
	cancel()
	if len(targets) == 0 || ctx.Err() != nil {
		return
	}
	var group errgroup.Group
	group.SetLimit(4)
	group.Go(func() error {
		request, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		metrics, err := collector.Metrics.PodMetricses("").List(request, metav1.ListOptions{LabelSelector: ComponentLabel + "=runtime"})
		collector.collectionResult(ctx, "pod metrics", err)
		if err != nil {
			return nil
		}
		for _, metric := range metrics.Items {
			target, exists := targets[types.NamespacedName{Namespace: metric.Namespace, Name: metric.Name}]
			if !exists || (metric.UID != "" && metric.UID != target.pod.UID) || metric.Timestamp.Time.Add(-metric.Window.Duration).Before(target.pod.CreationTimestamp.Time) {
				continue
			}
			usage := &codespacev1.RuntimeResourceUsage{ObservedUnix: metric.Timestamp.Unix()}
			for _, container := range metric.Containers {
				if container.Name != "runtime" {
					continue
				}
				if cpu, exists := container.Usage[corev1.ResourceCPU]; exists && cpu.Sign() >= 0 {
					usage.Cpu = &codespacev1.RuntimeCPUUsage{UsedMillicores: cpu.MilliValue()}
				}
				if memory, exists := container.Usage[corev1.ResourceMemory]; exists && memory.Sign() >= 0 {
					usage.Memory = &codespacev1.RuntimeMemoryUsage{UsedBytes: memory.Value()}
				}
				break
			}
			collector.Samples.RecordKubernetes(target.codespace, usage)
		}
		return nil
	})
	for node, nodeTargets := range nodes {
		group.Go(func() error {
			request, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			data, err := collector.Nodes.Get().Resource("nodes").Name(node).SubResource("proxy").Suffix("stats/summary").DoRaw(request)
			var summary stats.Summary
			if err == nil {
				err = json.Unmarshal(data, &summary)
			}
			collector.collectionResult(ctx, "volume metrics: "+node, err)
			if err != nil {
				return nil
			}
			byPod := make(map[types.UID]resourceTarget, len(nodeTargets))
			for _, target := range nodeTargets {
				byPod[target.pod.UID] = target
			}
			for _, pod := range summary.Pods {
				target, exists := byPod[types.UID(pod.PodRef.UID)]
				if !exists || pod.PodRef.Namespace != target.pod.Namespace || pod.PodRef.Name != target.pod.Name {
					continue
				}
				for _, volume := range pod.VolumeStats {
					if volume.Name != "data" || volume.PVCRef == nil || volume.PVCRef.Namespace != target.volume.Namespace || volume.PVCRef.Name != target.volume.Name || volume.Time.Before(&target.volume.CreationTimestamp) || volume.Time.Before(&target.pod.CreationTimestamp) {
						continue
					}
					if volume.CapacityBytes == nil || *volume.CapacityBytes == 0 || *volume.CapacityBytes > math.MaxInt64 || volume.AvailableBytes == nil || *volume.AvailableBytes > *volume.CapacityBytes {
						continue
					}
					collector.Samples.RecordKubernetes(target.codespace, &codespacev1.RuntimeResourceUsage{
						ObservedUnix: volume.Time.Unix(),
						Disk:         &codespacev1.RuntimeDiskUsage{LimitBytes: int64(*volume.CapacityBytes), UsedBytes: int64(*volume.CapacityBytes - *volume.AvailableBytes)},
					})
					break
				}
			}
			return nil
		})
	}
	_ = group.Wait()
}
