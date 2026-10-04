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

package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/hybernate/internal/discovery"
)

// prices are the rates for what node prices can't price, and whether each
// is the built-in assumption rather than the user's own.
type prices struct {
	CPUPerHour    float64 `json:"cpuPerHour"`
	MemoryPerHour float64 `json:"memoryPerHour"`
	CPUAssumed    bool    `json:"cpuAssumed"`
	MemoryAssumed bool    `json:"memoryAssumed"`
}

// ownPrices says which prices the user gave.
func ownPrices(cmd *cobra.Command) (cpu, memory bool) {
	return cmd.Flags().Changed("cpu-price"), cmd.Flags().Changed("memory-price")
}

func pricesFor(opts scanOptions) prices {
	return prices{
		CPUPerHour:    opts.cpuPrice,
		MemoryPerHour: opts.memoryPrice,
		CPUAssumed:    !opts.ownCPUPrice,
		MemoryAssumed: !opts.ownMemoryPrice,
	}
}

func (p prices) cpu() string    { return fmt.Sprintf("$%.3f per vCPU-hour", p.CPUPerHour) }
func (p prices) memory() string { return fmt.Sprintf("$%.3f per GiB-hour of memory", p.MemoryPerHour) }

// sentence says what costs were priced at without node prices, and which
// prices are the built-in assumption rather than the user's own.
func (p prices) sentence() string {
	switch {
	case p.CPUAssumed && p.MemoryAssumed:
		return "Assumed list prices: " + p.cpu() + " and " + p.memory() + ", from AWS on-demand in us-east-1."
	case p.CPUAssumed:
		return "Your memory price, " + p.memory() + ", and the assumed CPU price, " + p.cpu() +
			" (AWS on-demand, us-east-1)."
	case p.MemoryAssumed:
		return "Your CPU price, " + p.cpu() + ", and the assumed memory price, " + p.memory() +
			" (AWS on-demand, us-east-1)."
	default:
		return "Your prices: " + p.cpu() + " and " + p.memory() + "."
	}
}

// pricesSentence says what the scan's costs were priced at: the list
// prices of the cluster's node types where it has them, the user's own
// prices, or assumed ones, and which workloads cost less than shown
// because they run on spot nodes.
func (r scanResult) pricesSentence() string {
	p := r.Prices
	if !p.CPUAssumed && !p.MemoryAssumed {
		return p.sentence()
	}
	var nodes discovery.Prices
	var onSpot int
	if r.ClusterReport != nil {
		nodes = r.NodePrices
		for _, wl := range r.Workloads {
			if wl.OnSpot {
				onSpot++
			}
		}
	}

	var listed []discovery.NodeTypePrice
	var unlisted []string
	unlistedNodes := 0
	for _, t := range nodes.NodeTypes {
		if t.Listed {
			listed = append(listed, t)
			continue
		}
		unlistedNodes += t.Nodes
		if t.InstanceType != "" {
			unlisted = append(unlisted, t.InstanceType)
		}
	}

	var s string
	switch {
	case nodes.NodesUnread != "":
		s = p.sentence() + " The nodes weren't read: " + nodes.NodesUnread + "."
	case len(listed) == 0:
		s = p.sentence()
		if unlistedNodes > 0 {
			s += " None of the nodes' instance types has a list price."
		}
	default:
		s = "On-demand list prices for " + nodeTypesPhrase(listed) + "."
		if !p.CPUAssumed {
			s += " CPU at your price, " + p.cpu() + "."
		}
		if !p.MemoryAssumed {
			s += " Memory at your price, " + p.memory() + "."
		}
		if unlistedNodes > 0 {
			use := "use"
			if unlistedNodes == 1 {
				use = "uses"
			}
			s += " " + unlistedPhrase(unlistedNodes, unlisted) + " " + use + " " + p.unlistedRates() + "."
		}
	}
	if onSpot > 0 {
		costs := "they cost"
		if onSpot == 1 {
			costs = "it costs"
		}
		s += fmt.Sprintf(" %s on spot nodes, priced at on-demand, so %s less than shown.",
			plural(onSpot, "workload runs", "workloads run"), costs)
	}
	return s
}

// unlistedRates names the rates pods on nodes without a list price use.
func (p prices) unlistedRates() string {
	switch {
	case p.CPUAssumed && p.MemoryAssumed:
		return "the assumed " + p.cpu() + " and " + p.memory() + " (AWS on-demand, us-east-1)"
	case p.CPUAssumed:
		return "the assumed " + p.cpu() + " (AWS on-demand, us-east-1) and your memory price"
	default:
		return "your CPU price and the assumed " + p.memory() + " (AWS on-demand, us-east-1)"
	}
}

// nodeTypesPhrase names the node types costs were priced at, and where.
func nodeTypesPhrase(types []discovery.NodeTypePrice) string {
	names := make([]string, 0, len(types))
	var places []string
	for _, t := range types {
		names = append(names, t.InstanceType)
		place := providerName(t.Provider) + " " + t.Region
		if !slices.Contains(places, place) {
			places = append(places, place)
		}
	}
	slices.Sort(places)
	where := " in " + joinAnd(places)
	if len(types) == 1 {
		return "its node type, " + names[0] + "," + where
	}
	const shown = 3
	listed := joinAnd(names)
	if len(names) > shown {
		listed = fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
	}
	return fmt.Sprintf("its %d node types, %s,%s", len(types), listed, where)
}

func unlistedPhrase(nodes int, types []string) string {
	s := plural(nodes, "node", "nodes")
	if len(types) == 0 {
		return s + " without an instance type"
	}
	return s + " of a type without a list price (" + strings.Join(types, ", ") + ")"
}

func providerName(provider string) string {
	switch provider {
	case "aws":
		return "AWS"
	case "gcp":
		return "Google Cloud"
	case "azure":
		return "Azure"
	}
	return provider
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
