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

// Command prices regenerates the on-demand list prices Hybernate prices
// nodes with, from the instance data published by instances.vantage.sh
// (github.com/vantage-sh/ec2instances.info, MIT), which collects each
// provider's public price lists.
//
//	go run ./hack/prices internal/cost/prices.csv.gz
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type source struct {
	provider string
	url      string
}

var sources = []source{
	{"aws", "https://instances.vantage.sh/instances.json"},
	{"azure", "https://instances.vantage.sh/azure/instances.json"},
	{"gcp", "https://instances.vantage.sh/gcp/instances.json"},
}

// instance is the part of each provider's records the table needs. AWS and
// GCP spell vCPUs "vCPU", Azure "vcpu"; prices are strings or numbers.
type instance struct {
	InstanceType string                     `json:"instance_type"`
	VCPU         json.Number                `json:"vCPU"`
	VCPULower    json.Number                `json:"vcpu"`
	Memory       json.Number                `json:"memory"`
	Pricing      map[string]json.RawMessage `json:"pricing"`
	Regions      map[string]string          `json:"regions"`
}

type osPrices struct {
	Linux struct {
		OnDemand json.RawMessage `json:"ondemand"`
	} `json:"linux"`
}

type row struct {
	provider, instanceType string
	vcpu, memoryGiB        float64
	prices                 []regionPrice
}

type regionPrice struct {
	region string
	price  float64
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: prices OUTPUT.csv.gz")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out string) error {
	var rows []row
	for _, s := range sources {
		instances, err := fetch(s.url)
		if err != nil {
			return fmt.Errorf("fetching %s prices: %w", s.provider, err)
		}
		for _, in := range instances {
			if r, ok := rowFor(s.provider, in); ok {
				rows = append(rows, r)
			}
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		return strings.Compare(a.provider+" "+a.instanceType, b.provider+" "+b.instanceType)
	})

	var csv bytes.Buffer
	csv.WriteString("# provider,instance type,vCPUs,memory GiB,region=on-demand USD per hour...\n")
	for _, r := range rows {
		fmt.Fprintf(&csv, "%s,%s,%s,%s", r.provider, r.instanceType, number(r.vcpu), number(r.memoryGiB))
		for _, p := range r.prices {
			fmt.Fprintf(&csv, ",%s=%s", p.region, number(p.price))
		}
		csv.WriteByte('\n')
	}

	var gz bytes.Buffer
	// No name or modification time in the header, so the same prices
	// always produce the same file.
	w, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("compressing prices: %w", err)
	}
	if _, err := w.Write(csv.Bytes()); err != nil {
		return fmt.Errorf("compressing prices: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("compressing prices: %w", err)
	}
	if err := os.WriteFile(out, gz.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	fmt.Printf("wrote %d instance types to %s\n", len(rows), out)
	return nil
}

func fetch(url string) ([]instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // read-only, nothing to do if closing fails
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	var instances []instance
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&instances); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", url, err)
	}
	return instances, nil
}

var space = regexp.MustCompile(`\s+`)

func rowFor(provider string, in instance) (row, bool) {
	vcpu, _ := in.VCPU.Float64()
	if vcpu == 0 {
		vcpu, _ = in.VCPULower.Float64()
	}
	memory, _ := in.Memory.Float64()
	if vcpu <= 0 || memory <= 0 {
		return row{}, false
	}
	r := row{provider: provider, instanceType: strings.ToLower(in.InstanceType), vcpu: vcpu, memoryGiB: memory}
	for region, raw := range in.Pricing {
		var p osPrices
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		price, err := strconv.ParseFloat(strings.Trim(string(p.Linux.OnDemand), `"`), 64)
		if err != nil || price <= 0 {
			continue
		}
		// Azure's records key regions by a slug of their display name;
		// nodes are labelled with the ARM name, which is that display name
		// lowercased without spaces ("East US 2" is eastus2).
		if provider == "azure" {
			name, ok := in.Regions[region]
			if !ok {
				continue
			}
			region = strings.ToLower(space.ReplaceAllString(name, ""))
		}
		r.prices = append(r.prices, regionPrice{region: region, price: price})
	}
	if len(r.prices) == 0 {
		return row{}, false
	}
	slices.SortFunc(r.prices, func(a, b regionPrice) int { return strings.Compare(a.region, b.region) })
	return r, true
}

func number(f float64) string {
	return strconv.FormatFloat(f, 'g', 6, 64)
}
