/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package doorman

import (
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// EndpointSliceCache is the doorman's cache of EndpointSlices. It watches
// them so a held connection is passed on the moment its workload has a
// Ready pod, and it can't know in advance which Services that will be, so
// it caches every slice in the namespaces it serves. To keep that small, it
// holds only the slices it trusts, those the EndpointSlice controller wrote
// (see trusted), cut down to the fields it reads: a slice's ports, and
// each endpoint's first address, readiness and pod reference kind.
func EndpointSliceCache() cache.ByObject {
	return cache.ByObject{
		Label:     labels.SelectorFromSet(labels.Set{discoveryv1.LabelManagedBy: endpointSliceController}),
		Transform: trimEndpointSlice,
	}
}

func trimEndpointSlice(in any) (any, error) {
	slice, ok := in.(*discoveryv1.EndpointSlice)
	if !ok {
		return in, nil
	}
	out := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            slice.Name,
			Namespace:       slice.Namespace,
			ResourceVersion: slice.ResourceVersion,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: slice.Labels[discoveryv1.LabelServiceName],
				discoveryv1.LabelManagedBy:   slice.Labels[discoveryv1.LabelManagedBy],
			},
		},
		AddressType: slice.AddressType,
		Ports:       slice.Ports,
		Endpoints:   make([]discoveryv1.Endpoint, 0, len(slice.Endpoints)),
	}
	for _, ep := range slice.Endpoints {
		trimmed := discoveryv1.Endpoint{Conditions: discoveryv1.EndpointConditions{
			Ready:       ep.Conditions.Ready,
			Terminating: ep.Conditions.Terminating,
		}}
		if len(ep.Addresses) > 0 {
			trimmed.Addresses = []string{ep.Addresses[0]}
		}
		if ep.TargetRef != nil {
			trimmed.TargetRef = &corev1.ObjectReference{Kind: ep.TargetRef.Kind}
		}
		out.Endpoints = append(out.Endpoints, trimmed)
	}
	return out, nil
}
