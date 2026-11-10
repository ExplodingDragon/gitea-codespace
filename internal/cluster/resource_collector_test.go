package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResourceCollector(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable=%v", unavailable), func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			var objects []client.Object
			var metricsObjects []runtime.Object
			var codespaces []*api.Codespace
			var summary stats.Summary
			samples := &ResourceSamples{}
			for index, isolation := range []string{api.IsolationNative, api.IsolationNative, api.IsolationKata} {
				name := fmt.Sprintf("runtime-%d", index)
				codespace := &api.Codespace{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "site", UID: types.UID(name)},
					Spec:       api.CodespaceSpec{Site: api.ResourceReference{UID: "site-uid"}, Operation: api.Operation{Version: 1}, Runtime: api.RuntimeConfiguration{EnvironmentConfiguration: api.EnvironmentConfiguration{Isolation: isolation}}},
					Status:     api.CodespaceStatus{Bound: true, Pod: api.ObservedResource{Name: name, UID: types.UID(name + "-pod")}, Volume: api.ObservedResource{Name: name, UID: types.UID(name + "-volume")}},
				}
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "site", UID: codespace.Status.Pod.UID, CreationTimestamp: metav1.NewTime(now.Add(-time.Minute))},
					Spec:       corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{{Name: "runtime", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("4Gi")}}}}, Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}},
				}
				if index == 1 {
					pod.Spec.Containers[0].Resources.Limits = nil
				}
				claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "site", UID: codespace.Status.Volume.UID, CreationTimestamp: pod.CreationTimestamp}, Spec: corev1.PersistentVolumeClaimSpec{VolumeMode: ptr.To(codespace.Spec.Runtime.StorageVolumeMode())}}
				objects = append(objects, codespace, pod, claim)
				codespaces = append(codespaces, codespace)
				samples.Record(codespace, &codespacev1.RuntimeResourceUsage{ObservedUnix: now.Add(-40 * time.Second).Unix(), Cpu: &codespacev1.RuntimeCPUUsage{UsedMillicores: 100, LimitMillicores: 800}, Memory: &codespacev1.RuntimeMemoryUsage{UsedBytes: 1024, LimitBytes: 2048}, Disk: &codespacev1.RuntimeDiskUsage{UsedBytes: 40, LimitBytes: 1000}})
				metric := &metricsapi.PodMetrics{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "site", UID: pod.UID, Labels: map[string]string{ComponentLabel: "runtime"}},
					Timestamp:  metav1.NewTime(now.Add(-20 * time.Second)), Window: metav1.Duration{Duration: 15 * time.Second},
					Containers: []metricsapi.ContainerMetrics{{Name: "runtime", Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("2Gi")}}, {Name: "web-ide-init", Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("9"), corev1.ResourceMemory: resource.MustParse("9Gi")}}},
				}
				if index == 1 {
					metric.UID = "previous-pod"
				}
				metricsObjects = append(metricsObjects, metric)
				volume := stats.VolumeStats{Name: "data", PVCRef: &stats.PVCReference{Name: name, Namespace: "site"}, FsStats: stats.FsStats{Time: metav1.NewTime(now.Add(-10 * time.Second)), CapacityBytes: ptr.To(uint64(5000)), AvailableBytes: ptr.To(uint64(100))}}
				if index == 1 {
					volume.AvailableBytes = ptr.To(uint64(0))
				}
				summary.Pods = append(summary.Pods, stats.PodStats{PodRef: stats.PodReference{Name: name, Namespace: "site", UID: string(pod.UID)}, VolumeStats: []stats.VolumeStats{volume}})
			}
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.URL.Path != "/api/v1/nodes/worker/proxy/stats/summary" {
					t.Errorf("unexpected Summary path %q", request.URL.Path)
					http.NotFound(writer, request)
					return
				}
				if unavailable {
					http.Error(writer, "unavailable", http.StatusForbidden)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(writer).Encode(summary); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			coreClient, err := coreclient.NewForConfig(&rest.Config{Host: server.URL})
			require.NoError(t, err)
			metrics := metricsfake.NewSimpleClientset()
			for _, metric := range metricsObjects {
				require.NoError(t, metrics.Tracker().Create(metricsapi.SchemeGroupVersion.WithResource("pods"), metric, "site"))
			}
			if unavailable {
				metrics.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("metrics API unavailable")
				})
			}
			collector := &ResourceCollector{Reader: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build(), Metrics: metrics.MetricsV1beta1(), Nodes: coreClient.RESTClient(), Samples: samples}
			collector.collect(t.Context(), true)
			require.EqualValues(t, 1, requests.Load())
			require.Len(t, metrics.Actions(), 1)
			for index, codespace := range codespaces {
				usage := samples.Load(codespace)
				require.NotNil(t, usage)
				require.EqualValues(t, 1000, usage.Disk.LimitBytes)
				if unavailable || index == 1 {
					require.EqualValues(t, 100, usage.Cpu.UsedMillicores)
					require.EqualValues(t, 1024, usage.Memory.UsedBytes)
				} else {
					require.EqualValues(t, 250, usage.Cpu.UsedMillicores)
					require.EqualValues(t, 2<<30, usage.Memory.UsedBytes)
				}
				if index == 1 {
					require.Zero(t, usage.Cpu.LimitMillicores)
					require.Zero(t, usage.Memory.LimitBytes)
				} else {
					require.EqualValues(t, 1000, usage.Cpu.LimitMillicores)
					require.EqualValues(t, 4<<30, usage.Memory.LimitBytes)
				}
				switch {
				case unavailable || index == 2:
					require.EqualValues(t, 40, usage.Disk.UsedBytes)
				case index == 1:
					require.EqualValues(t, 1000, usage.Disk.UsedBytes)
				default:
					require.EqualValues(t, 900, usage.Disk.UsedBytes)
				}
			}
		})
	}
}
