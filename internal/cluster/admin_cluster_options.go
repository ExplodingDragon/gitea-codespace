// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"sort"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type adminClusterOptions struct {
	RuntimeClasses []adminRuntimeClass `json:"runtimeClasses"`
	StorageClasses []adminStorageClass `json:"storageClasses"`
	IngressClasses []adminIngressClass `json:"ingressClasses"`
	TLSSecrets     []string            `json:"tlsSecrets"`
	Issuers        []adminIssuer       `json:"issuers"`
}

type adminRuntimeClass struct {
	Name      string `json:"name"`
	Handler   string `json:"handler"`
	Isolation string `json:"isolation"`
}

type adminStorageClass struct {
	Name        string `json:"name"`
	Provisioner string `json:"provisioner"`
	Default     bool   `json:"default"`
}

type adminIngressClass struct {
	Name       string `json:"name"`
	Controller string `json:"controller"`
	Default    bool   `json:"default"`
}

type adminIssuer struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func (s *AdminServer) clusterOptions(ctx context.Context) (adminClusterOptions, error) {
	options := adminClusterOptions{
		RuntimeClasses: []adminRuntimeClass{}, StorageClasses: []adminStorageClass{},
		IngressClasses: []adminIngressClass{}, TLSSecrets: []string{}, Issuers: []adminIssuer{},
	}
	var runtimes nodev1.RuntimeClassList
	if err := s.Client.List(ctx, &runtimes); err != nil {
		return options, err
	}
	for _, runtime := range runtimes.Items {
		isolation := runtime.Labels[api.RuntimeIsolationLabel]
		if isolation != api.IsolationKata && isolation != api.IsolationSysbox {
			continue
		}
		options.RuntimeClasses = append(options.RuntimeClasses, adminRuntimeClass{Name: runtime.Name, Handler: runtime.Handler, Isolation: isolation})
	}
	var storages storagev1.StorageClassList
	if err := s.Client.List(ctx, &storages); err != nil {
		return options, err
	}
	for _, storage := range storages.Items {
		options.StorageClasses = append(options.StorageClasses, adminStorageClass{
			Name: storage.Name, Provisioner: storage.Provisioner,
			Default: isDefaultStorageClass(storage.Annotations),
		})
	}
	var ingressClasses networkingv1.IngressClassList
	if err := s.Client.List(ctx, &ingressClasses); err != nil {
		return options, err
	}
	for _, ingressClass := range ingressClasses.Items {
		options.IngressClasses = append(options.IngressClasses, adminIngressClass{
			Name: ingressClass.Name, Controller: ingressClass.Spec.Controller,
			Default: ingressClass.Annotations["ingressclass.kubernetes.io/is-default-class"] == "true",
		})
	}
	var secrets corev1.SecretList
	if err := s.Client.List(ctx, &secrets, client.InNamespace(s.Namespace)); err != nil {
		return options, err
	}
	for _, secret := range secrets.Items {
		if secret.Type == corev1.SecretTypeTLS && len(secret.Data[corev1.TLSCertKey]) > 0 && len(secret.Data[corev1.TLSPrivateKeyKey]) > 0 {
			options.TLSSecrets = append(options.TLSSecrets, secret.Name)
		}
	}
	for _, issuerType := range []struct {
		kind      string
		namespace string
	}{{"Issuer", s.Namespace}, {"ClusterIssuer", ""}} {
		issuers := &unstructured.UnstructuredList{}
		issuers.SetAPIVersion("cert-manager.io/v1")
		issuers.SetKind(issuerType.kind + "List")
		listOptions := []client.ListOption{}
		if issuerType.namespace != "" {
			listOptions = append(listOptions, client.InNamespace(issuerType.namespace))
		}
		if err := s.Client.List(ctx, issuers, listOptions...); err != nil {
			if !meta.IsNoMatchError(err) && !kruntime.IsNotRegisteredError(err) {
				return options, err
			}
			continue
		}
		for _, issuer := range issuers.Items {
			options.Issuers = append(options.Issuers, adminIssuer{Kind: issuerType.kind, Name: issuer.GetName()})
		}
	}
	sort.Slice(options.RuntimeClasses, func(i, j int) bool { return options.RuntimeClasses[i].Name < options.RuntimeClasses[j].Name })
	sort.Slice(options.StorageClasses, func(i, j int) bool {
		if options.StorageClasses[i].Default != options.StorageClasses[j].Default {
			return options.StorageClasses[i].Default
		}
		return options.StorageClasses[i].Name < options.StorageClasses[j].Name
	})
	sort.Slice(options.IngressClasses, func(i, j int) bool {
		if options.IngressClasses[i].Default != options.IngressClasses[j].Default {
			return options.IngressClasses[i].Default
		}
		return options.IngressClasses[i].Name < options.IngressClasses[j].Name
	})
	sort.Strings(options.TLSSecrets)
	sort.Slice(options.Issuers, func(i, j int) bool {
		if options.Issuers[i].Kind != options.Issuers[j].Kind {
			return options.Issuers[i].Kind < options.Issuers[j].Kind
		}
		return options.Issuers[i].Name < options.Issuers[j].Name
	})
	return options, nil
}

func isDefaultStorageClass(annotations map[string]string) bool {
	return annotations["storageclass.kubernetes.io/is-default-class"] == "true" || annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}
