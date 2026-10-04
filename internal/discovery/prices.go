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

package discovery

import (
	"cmp"
	"context"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

// Prices are what a scan priced the cluster's workloads at.
type Prices struct {
	// NodeTypes are the cluster's nodes by type and region. Listed is false
	// for one the price table doesn't have, whose pods are priced at the
	// scan's rates.
	NodeTypes []NodeTypePrice `json:"nodeTypes,omitempty"`
	// NodesUnread says why the nodes couldn't be read, when they couldn't,
	// so every workload is priced at the scan's rates.
	NodesUnread string `json:"nodesUnread,omitempty"`
}

// NodeTypePrice is one node type in the cluster and its list price.
type NodeTypePrice struct {
	Provider      string  `json:"provider,omitempty"`
	InstanceType  string  `json:"instanceType"`
	Region        string  `json:"region"`
	Nodes         int     `json:"nodes"`
	SpotNodes     int     `json:"spotNodes,omitempty"`
	Listed        bool    `json:"listed"`
	CPUPerHour    float64 `json:"cpuPerHour,omitempty"`
	MemoryPerHour float64 `json:"memoryPerHour,omitempty"`
}

// nodePricing is what each node costs, read once per scan.
type nodePricing struct {
	byNode map[string]nodePrice
	// typical is the list price of the cluster's most common listed node
	// type, for a workload with no pod on a node to price it at.
	typical *cost.Rates
}

type nodePrice struct {
	rates  cost.Rates
	listed bool
	spot   bool
}

// readNodePrices prices the cluster's nodes from their labels. The nodes
// are read as metadata only, which is all their labels need.
func (s *Scanner) readNodePrices(ctx context.Context) (nodePricing, Prices) {
	var nodes metav1.PartialObjectMetadataList
	nodes.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("NodeList"))
	if err := s.client.List(ctx, &nodes); err != nil {
		reason := err.Error()
		if apierrors.IsForbidden(err) {
			reason = "your access doesn't allow listing nodes"
		}
		return nodePricing{}, Prices{NodesUnread: reason}
	}

	pricing := nodePricing{byNode: make(map[string]nodePrice, len(nodes.Items))}
	byType := map[cost.NodeType]*NodeTypePrice{}
	for _, node := range nodes.Items {
		t := cost.NodeTypeOf(node.Labels)
		p, listed := cost.ListPriceOf(t)
		pricing.byNode[node.Name] = nodePrice{rates: p.Rates, listed: listed, spot: t.Spot}

		key := cost.NodeType{InstanceType: t.InstanceType, Region: t.Region}
		entry, ok := byType[key]
		if !ok {
			entry = &NodeTypePrice{InstanceType: t.InstanceType, Region: t.Region, Listed: listed,
				Provider: p.Provider, CPUPerHour: p.Rates.CPUPerHour, MemoryPerHour: p.Rates.MemoryPerHour}
			byType[key] = entry
		}
		entry.Nodes++
		if t.Spot {
			entry.SpotNodes++
		}
	}

	var prices Prices
	for _, entry := range byType {
		prices.NodeTypes = append(prices.NodeTypes, *entry)
	}
	slices.SortFunc(prices.NodeTypes, func(a, b NodeTypePrice) int {
		if c := cmp.Compare(b.Nodes, a.Nodes); c != 0 {
			return c
		}
		return cmp.Compare(a.InstanceType+" "+a.Region, b.InstanceType+" "+b.Region)
	})
	for _, t := range prices.NodeTypes {
		if t.Listed {
			pricing.typical = &cost.Rates{CPUPerHour: t.CPUPerHour, MemoryPerHour: t.MemoryPerHour,
				StoragePerMonth: cost.DefaultRates.StoragePerMonth}
			break
		}
	}
	return pricing, prices
}

// ratesFor is what a workload is priced at: the listed nodes its pods are
// on. With no pod on a node, such as one Hybernate paused, it's what
// Hybernate recorded of the nodes it last ran on, or else the cluster's
// most common node type. Pods only on nodes the table doesn't have, and
// everything else, get the scan's rates. Prices the user gave override
// each part. It also says whether any of its pods is on a spot node.
func (p nodePricing) ratesFor(pods []corev1.Pod, mw *v1alpha1.ManagedWorkload, opts ClusterOptions) (
	cost.Rates, bool) {
	var listed []cost.Rates
	spot, scheduled := false, false
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		n, ok := p.byNode[pod.Spec.NodeName]
		if !ok {
			continue
		}
		scheduled = true
		spot = spot || n.spot
		if n.listed {
			listed = append(listed, n.rates)
		}
	}

	rates := opts.Rates
	switch {
	case len(listed) > 0:
		rates = cost.Mean(listed)
	case scheduled:
		// Only on nodes the table doesn't have: the scan's rates.
	case mw != nil && mw.Status.Cost != nil && mw.Status.Cost.ListRates != nil:
		r := mw.Status.Cost.ListRates
		if r.CPUPerHour != nil && r.MemoryPerHour != nil {
			rates.CPUPerHour = r.CPUPerHour.AsApproximateFloat64()
			rates.MemoryPerHour = r.MemoryPerHour.AsApproximateFloat64()
		}
	case p.typical != nil:
		rates = *p.typical
	}
	if opts.OwnCPUPrice {
		rates.CPUPerHour = opts.Rates.CPUPerHour
	}
	if opts.OwnMemoryPrice {
		rates.MemoryPerHour = opts.Rates.MemoryPerHour
	}
	return rates, spot
}
