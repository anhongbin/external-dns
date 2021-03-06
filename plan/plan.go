/*
Copyright 2017 The Kubernetes Authors.

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

package plan

import (
	"fmt"
	"strings"

	"github.com/kubernetes-incubator/external-dns/endpoint"
)

// Plan can convert a list of desired and current records to a series of create,
// update and delete actions.
type Plan struct {
	// List of current records
	Current []*endpoint.Endpoint
	// List of desired records
	Desired []*endpoint.Endpoint
	// Policies under which the desired changes are calculated
	Policies []Policy
	// List of changes necessary to move towards desired state
	// Populated after calling Calculate()
	Changes *Changes
}

// Changes holds lists of actions to be executed by dns providers
type Changes struct {
	// Records that need to be created
	Create []*endpoint.Endpoint
	// Records that need to be updated (current data)
	UpdateOld []*endpoint.Endpoint
	// Records that need to be updated (desired data)
	UpdateNew []*endpoint.Endpoint
	// Records that need to be deleted
	Delete []*endpoint.Endpoint
}

// planTable is a supplementary struct for Plan
// each row correspond to a dnsName -> (current record + all desired records)
/*
planTable: (-> = target)
--------------------------------------------------------
DNSName | Current record | Desired Records             |
--------------------------------------------------------
foo.com | -> 1.1.1.1     | [->1.1.1.1, ->elb.com]      |  = no action
--------------------------------------------------------
bar.com |                | [->191.1.1.1, ->190.1.1.1]  |  = create (bar.com -> 190.1.1.1)
--------------------------------------------------------
"=", i.e. result of calculation relies on supplied ConflictResolver
*/
type planTable struct {
	rows     map[string]*planTableRow
	resolver ConflictResolver
}

func newPlanTable() planTable { //TODO: make resolver configurable
	return planTable{map[string]*planTableRow{}, PerResource{}}
}

// planTableRow
// current corresponds to the record currently occupying dns name on the dns provider
// candidates corresponds to the list of records which would like to have this dnsName
type planTableRow struct {
	currents   []*endpoint.Endpoint
	candidates []*endpoint.Endpoint
}

func (t planTableRow) String() string {
	return fmt.Sprintf("planTableRow{current=%v, candidates=%v}", t.currents, t.candidates)
}

func (t planTable) addCurrent(e *endpoint.Endpoint) {
	dnsName := normalizeDNSName(e.DNSName)
	if _, ok := t.rows[dnsName]; !ok {
		t.rows[dnsName] = &planTableRow{}
	}
	t.rows[dnsName].currents = append(t.rows[dnsName].currents, e)
}

func (t planTable) addCandidate(e *endpoint.Endpoint) {
	dnsName := normalizeDNSName(e.DNSName)
	if _, ok := t.rows[dnsName]; !ok {
		t.rows[dnsName] = &planTableRow{}
	}
	t.rows[dnsName].candidates = append(t.rows[dnsName].candidates, e)
}

// TODO: allows record type change, which might not be supported by all dns providers
func (t planTable) getUpdates() (updateNew []*endpoint.Endpoint, updateOld []*endpoint.Endpoint) {
	for _, row := range t.rows {
		for _, current := range row.currents {
			if current != nil && len(row.candidates) > 0 { //dns name is taken
				update := t.resolver.ResolveUpdate(current, row.candidates)
				// compare "update" to "current" to figure out if actual update is required
				if shouldUpdateTTL(update, current) || targetChanged(update, current) || shouldUpdateProviderSpecific(update, current) {
					inheritOwner(current, update)
					updateNew = append(updateNew, update)
					updateOld = append(updateOld, current)
				}
			}
		}
	}
	return removeDuplicate(updateNew), removeDuplicate(updateOld)
}

func removeDuplicate(endpoints []*endpoint.Endpoint) (newEndpoints []*endpoint.Endpoint) {
	newEndpoints = make([]*endpoint.Endpoint, 0)
	for i := 0; i < len(endpoints); i++ {
		repeat := false
		for j := i + 1; j < len(endpoints); j++ {
			if endpoints[i] == endpoints[j] {
				repeat = true
				break
			}
		}
		if !repeat {
			newEndpoints = append(newEndpoints, endpoints[i])
		}
	}
	return
}

func (t planTable) getCreates() (createList []*endpoint.Endpoint) {
	for _, row := range t.rows {
		if len(row.currents) < len(row.candidates) {
			currentResources := make([]string, len(row.currents))
			for _, current := range row.currents {
				currentResources = append(currentResources, current.Labels[endpoint.ResourceLabelKey])
			}
			for _, ep := range row.candidates {
				if !isContainResource(currentResources, ep.Labels[endpoint.ResourceLabelKey]) {
					createList = append(createList, ep)
				}
			}
		}
	}
	return
}

func (t planTable) getDeletes() (deleteList []*endpoint.Endpoint) {
	for _, row := range t.rows {
		if len(row.currents) > 0 {
			candidateResources := make([]string, len(row.candidates))
			for _, candidate := range row.candidates {
				candidateResources = append(candidateResources, candidate.Labels[endpoint.ResourceLabelKey])
			}
			for _, ep := range row.currents {
				if !isContainResource(candidateResources, ep.Labels[endpoint.ResourceLabelKey]) {
					deleteList = append(deleteList, ep)
				}
			}
		}
	}
	return
}

// Calculate computes the actions needed to move current state towards desired
// state. It then passes those changes to the current policy for further
// processing. It returns a copy of Plan with the changes populated.
func (p *Plan) Calculate() *Plan {
	t := newPlanTable()

	for _, current := range filterRecordsForPlan(p.Current) {
		t.addCurrent(current)
	}
	for _, desired := range filterRecordsForPlan(p.Desired) {
		t.addCandidate(desired)
	}
	changes := &Changes{}
	changes.Create = t.getCreates()
	changes.Delete = t.getDeletes()
	changes.UpdateNew, changes.UpdateOld = t.getUpdates()
	for _, pol := range p.Policies {
		changes = pol.Apply(changes)
	}

	plan := &Plan{
		Current: p.Current,
		Desired: p.Desired,
		Changes: changes,
	}
	return plan
}

func inheritOwner(from, to *endpoint.Endpoint) {
	if to.Labels == nil {
		to.Labels = map[string]string{}
	}
	if from.Labels == nil {
		from.Labels = map[string]string{}
	}
	to.Labels[endpoint.OwnerLabelKey] = from.Labels[endpoint.OwnerLabelKey]
}

func targetChanged(desired, current *endpoint.Endpoint) bool {
	if desired == nil {
		return false
	}
	return !desired.Targets.Same(current.Targets)
}

func shouldUpdateTTL(desired, current *endpoint.Endpoint) bool {
	if desired == nil {
		return false
	}
	if !desired.RecordTTL.IsConfigured() {
		return false
	}
	return desired.RecordTTL != current.RecordTTL
}

func shouldUpdateProviderSpecific(desired, current *endpoint.Endpoint) bool {
	if desired == nil {
		return false
	}
	if current.ProviderSpecific == nil && len(desired.ProviderSpecific) == 0 {
		return false
	}
	for _, c := range current.ProviderSpecific {
		// don't consider target health when detecting changes
		// see: https://github.com/kubernetes-incubator/external-dns/issues/869#issuecomment-458576954
		if c.Name == "aws/evaluate-target-health" {
			continue
		}

		for _, d := range desired.ProviderSpecific {
			if d.Name == c.Name && d.Value != c.Value {
				return true
			}
		}
	}

	return false
}

// filterRecordsForPlan removes records that are not relevant to the planner.
// Currently this just removes TXT records to prevent them from being
// deleted erroneously by the planner (only the TXT registry should do this.)
//
// Per RFC 1034, CNAME records conflict with all other records - it is the
// only record with this property. The behavior of the planner may need to be
// made more sophisticated to codify this.
func filterRecordsForPlan(records []*endpoint.Endpoint) []*endpoint.Endpoint {
	filtered := []*endpoint.Endpoint{}

	for _, record := range records {
		// Explicitly specify which records we want to use for planning.
		// TODO: Add AAAA records as well when they are supported.
		switch record.RecordType {
		case endpoint.RecordTypeA, endpoint.RecordTypeCNAME:
			filtered = append(filtered, record)
		default:
			continue
		}
	}

	return filtered
}

// normalizeDNSName converts a DNS name to a canonical form, so that we can use string equality
// it: removes space, converts to lower case, ensures there is a trailing dot
func normalizeDNSName(dnsName string) string {
	s := strings.TrimSpace(strings.ToLower(dnsName))
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

func isContainResource(resources []string, resourceLabel string) bool {
	index := -1
	for i := 0; i < len(resources); i++ {
		if resources[i] == resourceLabel {
			index = i
		}
	}
	if index == -1 {
		return false
	}
	return true
}
