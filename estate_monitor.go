package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var estateMonitor = struct {
	sync.RWMutex
	history       EstateHistory
	loaded        bool
	warnings      []string
	policyAttempt time.Time
	policy        *EstatePolicy
}{}

var estateCollectionMu sync.Mutex

// resetEstateMonitorContinuity marks an intentional settings boundary. It does
// not write history when monitoring is disabled or contact the PCE.
func resetEstateMonitorContinuity() {
	estateMonitor.Lock()
	defer estateMonitor.Unlock()
	estateMonitor.history.Baseline = nil
	estateMonitor.history.BaselineAt = time.Time{}
	estateMonitor.history.OptionsKey = ""
	estateMonitor.policyAttempt = time.Time{}
}

func estateScopeForConfig() string {
	configMutex.RLock()
	base := strings.TrimSuffix(strings.TrimSpace(config.PCEURL), "/") + "/api/v2/orgs/" + strings.TrimSpace(config.OrgID)
	configMutex.RUnlock()
	return estateIdentity(base)
}

func estateHistoryPath(scope string, zone ...string) string {
	timezone := ""
	if len(zone) > 0 {
		timezone = zone[0]
	} else {
		timezone = configuredDayLocation().String()
	}
	sum := sha256.Sum256([]byte(timezone))
	return dataFilePath("estate_history_" + scope + "_" + hex.EncodeToString(sum[:8]) + ".json")
}

// Load lazily: leaving every new option disabled performs no estate disk IO.
// A restart is an observation gap, so persisted inventory is never used to
// infer additions or removals until a fresh complete baseline is established.
func estateLoadLocked(scope, timezone string) {
	if estateMonitor.loaded && estateMonitor.history.Scope == scope && estateMonitor.history.Timezone == timezone {
		return
	}
	estateMonitor.loaded = true
	estateMonitor.history = EstateHistory{Scope: scope, Timezone: timezone}
	estateMonitor.warnings = nil
	estateMonitor.policy, estateMonitor.policyAttempt = nil, time.Time{}
	body, err := os.ReadFile(estateHistoryPath(scope, timezone))
	if err != nil {
		if !os.IsNotExist(err) {
			estateMonitor.warnings = append(estateMonitor.warnings, "Estate history could not be loaded: "+err.Error())
		}
		return
	}
	var history EstateHistory
	if err := json.Unmarshal(body, &history); err != nil || history.Scope != scope || history.Timezone != timezone {
		estateMonitor.warnings = append(estateMonitor.warnings, "Estate history could not be loaded for this PCE and organization")
		return
	}
	history.Baseline, history.BaselineAt = nil, time.Time{}
	history.OptionsKey = ""
	estateMonitor.history = history
}

func estateHistoryCopy() EstateHistory {
	if !configuredEstateOptions().Enabled() {
		return EstateHistory{}
	}
	scope := estateScopeForConfig()
	timezone := configuredDayLocation().String()
	estateMonitor.Lock()
	defer estateMonitor.Unlock()
	estateLoadLocked(scope, timezone)
	return estateCloneHistory(estateMonitor.history)
}

func estateCloneHistory(history EstateHistory) EstateHistory {
	// Public readers must never retain maps shared with the collector.
	body, _ := json.Marshal(history)
	var copied EstateHistory
	_ = json.Unmarshal(body, &copied)
	return copied
}

func estateView() EstateView {
	options := configuredEstateOptions()
	view := EstateView{Options: options, Issues: []EstateIssue{}, Changes: []EstateChange{}, Warnings: []string{}}
	view.Options.WeeklyWebhookURL = ""
	if !options.Enabled() {
		return view
	}
	history := estateHistoryCopy()
	view.Snapshot, view.StartedAt = history.Latest, history.StartedAt
	if view.Snapshot != nil && options.LabelHygieneEnabled && history.OptionsKey != estateOptionsKey(options) {
		view.Snapshot.LabelsComplete = false
		view.Snapshot.LabelsAssessed = 0
		view.Snapshot.LabelGaps = nil
		view.Warnings = append(view.Warnings, "Waiting for a label assessment using the current estate settings")
	}
	if options.IssueAgingEnabled {
		view.Issues = history.Issues
	}
	if options.ChangeLogEnabled {
		view.Changes = history.Changes
		summary := estateChangesSinceReport(history, options, time.Now())
		view.ChangesSinceReport = &summary
	}
	estateMonitor.RLock()
	view.Warnings = append(view.Warnings, estateMonitor.warnings...)
	estateMonitor.RUnlock()
	if view.Snapshot != nil {
		view.Warnings = append(view.Warnings, view.Snapshot.Warnings...)
	}
	return view
}

func updateEstateMonitor(baseURL string, workloads []map[string]interface{}, workloadErr error, health map[string][]map[string]interface{}, stats DashboardStats, options EstateOptions) {
	if !options.Enabled() {
		resetEstateMonitorContinuity()
		return
	}
	estateCollectionMu.Lock()
	defer estateCollectionMu.Unlock()
	now := stats.Timestamp
	if now.IsZero() {
		now = time.Now().UTC()
	}
	loc := configuredDayLocation()
	scope := estateIdentity(baseURL)
	// A settings change during a slow collector must not save the old estate
	// under the new credentials, nor re-enable something the user just disabled.
	if scope != estateScopeForConfig() || !reflect.DeepEqual(options, configuredEstateOptions()) {
		return
	}
	estateMonitor.Lock()
	estateLoadLocked(scope, loc.String())
	policyNeeded := options.PolicyEnabled && estatePolicyRefreshDue(estateMonitor.policy, estateMonitor.policyAttempt, now)
	cachedPolicy := estateMonitor.policy
	estateMonitor.Unlock()
	var labelWarning string
	if options.LabelHygieneEnabled || options.ChangeLogEnabled {
		workloads, labelWarning = estateResolveWorkloadLabels(baseURL, workloads)
	}
	snapshot := estateBuildSnapshot(now, loc, workloads, workloadErr, health, stats, options)
	if labelWarning != "" {
		snapshot.Warnings = append(snapshot.Warnings, labelWarning)
	}
	if options.PolicyEnabled {
		if policyNeeded {
			policy := estateCollectPolicy(baseURL, now)
			cachedPolicy = &policy
		}
		copyPolicy := *cachedPolicy
		snapshot.Policy = &copyPolicy
	}
	configUpdateMu.Lock()
	defer configUpdateMu.Unlock()
	if scope != estateScopeForConfig() || !reflect.DeepEqual(options, configuredEstateOptions()) || loc.String() != configuredDayLocation().String() {
		return
	}
	estateMonitor.Lock()
	defer estateMonitor.Unlock()
	// A concurrent settings save may have reset the baseline, which must stay
	// reset until this complete, currently configured sample is committed.
	if estateMonitor.history.Scope != scope || estateMonitor.history.Timezone != loc.String() {
		return
	}
	if estateMonitor.history.Latest != nil && snapshot.At.Before(estateMonitor.history.Latest.At) {
		return
	}
	if policyNeeded {
		estateMonitor.policy, estateMonitor.policyAttempt = cachedPolicy, now
	}
	estateApplySnapshot(&estateMonitor.history, snapshot, health, options, loc)
	if err := writeJSONFileAtomic(estateHistoryPath(scope, loc.String()), estateMonitor.history); err != nil {
		estateMonitor.warnings = []string{"Estate history is currently in memory only: " + err.Error()}
		log.Printf("[ESTATE] persist history: %v", err)
	} else {
		estateMonitor.warnings = nil
	}
}

func estateResolveWorkloadLabels(baseURL string, workloads []map[string]interface{}) ([]map[string]interface{}, string) {
	needed := false
	for _, row := range workloads {
		for _, label := range estateObjectArray(row["labels"]) {
			if mapString(label, "key") == "" {
				needed = true
				break
			}
		}
		if needed {
			break
		}
	}
	if !needed {
		return workloads, ""
	}
	labels, err := estateFetchCollection(baseURL + "/labels")
	byHref := map[string]map[string]interface{}{}
	for _, label := range labels {
		if href := mapString(label, "href"); href != "" && mapString(label, "key") != "" {
			byHref[href] = label
		}
	}
	resolved := make([]map[string]interface{}, len(workloads))
	for i, original := range workloads {
		row := make(map[string]interface{}, len(original))
		for key, value := range original {
			row[key] = value
		}
		var values []map[string]interface{}
		for _, label := range estateObjectArray(original["labels"]) {
			if mapString(label, "key") == "" {
				if found, ok := byHref[mapString(label, "href")]; ok {
					label = found
				}
			}
			values = append(values, label)
		}
		row["labels"] = values
		resolved[i] = row
	}
	if err != nil {
		return resolved, "Label catalog incomplete: " + err.Error()
	}
	return resolved, ""
}

func estateBuildSnapshot(now time.Time, loc *time.Location, workloads []map[string]interface{}, workloadErr error, health map[string][]map[string]interface{}, stats DashboardStats, options EstateOptions) EstateSnapshot {
	if loc == nil {
		loc = time.UTC
	}
	s := EstateSnapshot{At: now, FirstAt: now, Day: now.In(loc).Format("2006-01-02"), Samples: 1,
		Modes: map[string]int{}, Versions: []EstateVersionCount{}, LabelGaps: []EstateLabelGap{},
		Health: map[string]int{}, HealthComplete: map[string]bool{}, HealthSamples: map[string]int{}, Tampered: map[string]time.Time{}, Warnings: []string{}}
	needsInventory := options.InventoryEnabled || options.VENVersionsEnabled || options.LabelHygieneEnabled || options.ChangeLogEnabled || options.WeeklyReportEnabled
	if needsInventory {
		s.VersionsCollected = options.VENVersionsEnabled || options.WeeklyReportEnabled
		s.InventoryComplete = workloadErr == nil && stats.Workloads.Status.Success && stats.Workloads.Status.Error == ""
		if !s.InventoryComplete {
			s.Warnings = append(s.Warnings, "Workload inventory is incomplete; no inventory changes or removals are inferred")
		}
		versions := map[string]int{}
		versionsByVEN := map[string]string{}
		seen := map[string]bool{}
		for i, row := range workloads {
			id := estateWorkloadIdentity(row)
			if id == "" {
				// Keep anonymous entries in totals but never treat a name as a
				// trustworthy identity for estate change detection.
				id = fmt.Sprintf("unknown-identity:%d", i)
				s.InventoryComplete = false
				s.Warnings = append(s.Warnings, "Some workloads have no stable identity; inventory changes cannot be determined safely")
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			w := EstateWorkload{ID: id, Name: firstNonEmpty(mapString(row, "name"), mapString(row, "hostname"), id), Managed: isManagedWorkload(row),
				Mode: firstNonEmpty(mapString(row, "enforcement_mode"), "unknown"), Labels: map[string]string{}}
			unknownLabels := false
			for _, label := range estateObjectArray(row["labels"]) {
				key := strings.TrimSpace(mapString(label, "key"))
				if key == "" {
					unknownLabels = true
					key = "href:" + mapString(label, "href")
				}
				w.Labels[key] = firstNonEmpty(mapString(label, "value"), mapString(label, "name"), mapString(label, "href"))
			}
			s.Total++
			if w.Managed {
				s.Managed++
				s.Modes[w.Mode]++
				w.VENVersion = estateVENVersion(row)
				venID := firstNonEmpty(estateStringPath(row, "agent", "href"), estateStringPath(row, "ven", "href"), w.ID)
				if old, exists := versionsByVEN[venID]; !exists || old == "Unknown" {
					versionsByVEN[venID] = w.VENVersion
				}
			} else {
				s.Unmanaged++
			}
			if options.LabelHygieneEnabled && len(options.RequiredLabelTypes) > 0 {
				if unknownLabels {
					s.Warnings = append(s.Warnings, "Some workload label types were not returned by the PCE; those workloads are not classified as missing labels")
				} else {
					s.LabelsAssessed++
					gap := EstateLabelGap{ID: w.ID, Name: w.Name}
					for _, key := range options.RequiredLabelTypes {
						found := false
						for existing := range w.Labels {
							if strings.EqualFold(existing, strings.TrimSpace(key)) {
								found = true
								break
							}
						}
						if !found {
							gap.Missing = append(gap.Missing, key)
						}
					}
					if len(gap.Missing) > 0 {
						s.LabelGaps = append(s.LabelGaps, gap)
					}
				}
			}
			s.Workloads = append(s.Workloads, w)
		}
		s.VENCount = len(versionsByVEN)
		if s.VersionsCollected {
			for _, version := range versionsByVEN {
				versions[version]++
			}
		}
		for version, count := range versions {
			s.Versions = append(s.Versions, EstateVersionCount{Version: version, Count: count})
		}
		sort.Slice(s.Versions, func(i, j int) bool {
			if s.Versions[i].Count == s.Versions[j].Count {
				return s.Versions[i].Version < s.Versions[j].Version
			}
			return s.Versions[i].Count > s.Versions[j].Count
		})
		if s.InventoryComplete {
			s.CompleteSamples = 1
		}
		s.LabelsComplete = options.LabelHygieneEnabled && len(options.RequiredLabelTypes) > 0 && s.InventoryComplete && s.LabelsAssessed == s.Total
	}
	if options.IssueAgingEnabled || options.WeeklyReportEnabled {
		for _, kind := range []string{"warning", "error", "suspended", "stopped"} {
			complete := stats.VENStatus.Status.Success && stats.VENStatus.Status.Error == ""
			if kind == "stopped" {
				complete = stats.VENStatus.StoppedStatus.Success && stats.VENStatus.StoppedStatus.Error == ""
			}
			s.Health[kind] = len(estateUniqueHealth(health[kind]))
			s.HealthComplete[kind] = complete
			if complete {
				s.HealthSamples[kind] = 1
			}
			if !complete {
				s.Warnings = append(s.Warnings, kind+" VEN collection is incomplete; existing issues are not marked resolved")
			}
		}
	}
	if options.WeeklyReportEnabled {
		s.HealthComplete["tampering"] = stats.Tampering.Status.Success && stats.Tampering.Status.Error == "" && !stats.Tampering.Partial
		if s.HealthComplete["tampering"] {
			s.HealthSamples["tampering"] = 1
		}
		for name, at := range stats.Tampering.LastSeen {
			if !at.IsZero() {
				s.Tampered[name] = at
			}
		}
		if !s.HealthComplete["tampering"] {
			s.Warnings = append(s.Warnings, "Tampering event collection is incomplete")
		}
	}
	s.Warnings = estateUniqueStrings(s.Warnings)
	return s
}

func estateUniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func estateUniqueHealth(rows []map[string]interface{}) map[string]string {
	values := map[string]string{}
	for _, row := range rows {
		id := estateWorkloadIdentity(row)
		if workload, ok := row["workload"].(map[string]interface{}); ok {
			if workloadID := estateWorkloadIdentity(workload); workloadID != "" {
				id = workloadID
			}
		}
		if id == "" {
			id = "name:" + venDisplayName(row)
		}
		values[id] = venDisplayName(row)
	}
	return values
}

func estateOptionsKey(options EstateOptions) string {
	// Schedule and destination changes are not collection gaps. A collection
	// feature or label-requirement change must establish a fresh inventory baseline.
	body, _ := json.Marshal(struct {
		Inventory, Policy, Issues, Labels, Versions, Changes, Weekly bool
		Required                                                     []string
	}{options.InventoryEnabled, options.PolicyEnabled, options.IssueAgingEnabled, options.LabelHygieneEnabled,
		options.VENVersionsEnabled, options.ChangeLogEnabled, options.WeeklyReportEnabled, options.RequiredLabelTypes})
	return string(body)
}

func estateApplySnapshot(history *EstateHistory, snapshot EstateSnapshot, health map[string][]map[string]interface{}, options EstateOptions, loc *time.Location) {
	if history.StartedAt.IsZero() {
		history.StartedAt = snapshot.At
	}
	key := estateOptionsKey(options)
	continuity := history.OptionsKey == key && !history.BaselineAt.IsZero() && snapshot.At.Sub(history.BaselineAt) <= 20*time.Minute && !snapshot.At.Before(history.BaselineAt)
	if options.PolicyEnabled && options.ChangeLogEnabled && history.OptionsKey == key && history.Latest != nil && history.Latest.Policy != nil && snapshot.Policy != nil {
		before, after := history.Latest.Policy.LastProvisionedAt, snapshot.Policy.LastProvisionedAt
		if !before.IsZero() && after.After(before) {
			history.Changes = append(history.Changes, EstateChange{At: after, Kind: "policy_provisioned", Name: "Latest observed policy provisioning", Before: before.Format(time.RFC3339), After: after.Format(time.RFC3339)})
		}
	}
	if options.ChangeLogEnabled && snapshot.InventoryComplete && continuity {
		history.Changes = append(history.Changes, estateCompareInventory(history.Baseline, snapshot.Workloads, snapshot.At)...)
	}
	if snapshot.InventoryComplete && options.ChangeLogEnabled {
		history.Baseline = snapshot.Workloads
		history.BaselineAt = snapshot.At
	} else if !snapshot.InventoryComplete || !options.ChangeLogEnabled {
		// A missing interval cannot establish when a workload disappeared.
		history.Baseline, history.BaselineAt = nil, time.Time{}
	}
	history.OptionsKey = key
	if options.IssueAgingEnabled || options.WeeklyReportEnabled {
		estateUpdateIssues(history, snapshot, health)
	}
	history.Latest = &snapshot
	// Deep-copy maps: daily peaks/accumulated identities are not the live
	// snapshot's current counts and must never mutate them through shared maps.
	dailyData, _ := json.Marshal(snapshot)
	var daily EstateSnapshot
	_ = json.Unmarshal(dailyData, &daily)
	// Only the latest inventory and comparison baseline retain workload objects.
	// Historical counts/versions stay compact even for a large managed estate.
	daily.Workloads = nil
	daily.LabelGaps = nil
	found := false
	for i := range history.Days {
		if history.Days[i].Day != daily.Day {
			continue
		}
		previous := history.Days[i]
		daily.FirstAt = previous.FirstAt
		if daily.FirstAt.IsZero() {
			daily.FirstAt = previous.At
		}
		daily.Samples += previous.Samples
		daily.CompleteSamples += previous.CompleteSamples
		for kind, count := range previous.HealthSamples {
			daily.HealthSamples[kind] += count
		}
		for kind, count := range previous.Health {
			if count > daily.Health[kind] {
				daily.Health[kind] = count
			}
		}
		for id, at := range previous.Tampered {
			if at.After(daily.Tampered[id]) {
				daily.Tampered[id] = at
			}
		}
		// Never substitute a partial inventory for the last complete sample.
		if !daily.InventoryComplete && previous.InventoryComplete {
			daily.Total, daily.Managed, daily.Unmanaged = previous.Total, previous.Managed, previous.Unmanaged
			daily.Modes, daily.Versions = previous.Modes, previous.Versions
			daily.VENCount, daily.VersionsCollected = previous.VENCount, previous.VersionsCollected
			// Keep incomplete=false: the day's last observation is not a verified
			// snapshot; report consumers can show earlier available days instead.
		}
		history.Days[i], found = daily, true
		break
	}
	if !found {
		history.Days = append(history.Days, daily)
	}
	estatePruneHistory(history, snapshot.At, loc, options.HistoryDays)
}

func estateCompareInventory(before, after []EstateWorkload, at time.Time) []EstateChange {
	old := make(map[string]EstateWorkload, len(before))
	for _, workload := range before {
		old[workload.ID] = workload
	}
	var changes []EstateChange
	add := func(w EstateWorkload, kind, from, to string) {
		changes = append(changes, EstateChange{At: at, WorkloadID: w.ID, Name: w.Name, Kind: kind, Before: from, After: to})
	}
	for _, workload := range after {
		previous, ok := old[workload.ID]
		delete(old, workload.ID)
		if !ok {
			add(workload, "added", "", workload.Name)
			continue
		}
		if previous.Managed != workload.Managed {
			add(workload, "management", strconv.FormatBool(previous.Managed), strconv.FormatBool(workload.Managed))
		}
		if previous.Mode != workload.Mode {
			add(workload, "enforcement", previous.Mode, workload.Mode)
		}
		if previous.VENVersion != workload.VENVersion && previous.Managed && workload.Managed {
			add(workload, "ven_version", previous.VENVersion, workload.VENVersion)
		}
		if estateLabelsResolved(previous.Labels) && estateLabelsResolved(workload.Labels) && !reflect.DeepEqual(previous.Labels, workload.Labels) {
			from, _ := json.Marshal(previous.Labels)
			to, _ := json.Marshal(workload.Labels)
			add(workload, "labels", string(from), string(to))
		}
	}
	for _, workload := range old {
		add(workload, "removed", workload.Name, "")
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].WorkloadID == changes[j].WorkloadID {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].WorkloadID < changes[j].WorkloadID
	})
	return changes
}

func estateLabelsResolved(labels map[string]string) bool {
	for key := range labels {
		if strings.HasPrefix(key, "href:") {
			return false
		}
	}
	return true
}

func estateUpdateIssues(history *EstateHistory, snapshot EstateSnapshot, health map[string][]map[string]interface{}) {
	byID := make(map[string]int, len(history.Issues))
	for i, issue := range history.Issues {
		byID[issue.ID] = i
	}
	for _, kind := range []string{"warning", "error", "stopped", "suspended"} {
		seen := estateUniqueHealth(health[kind])
		for workloadID, name := range seen {
			id := kind + ":" + workloadID
			index, exists := byID[id]
			if !exists {
				index = len(history.Issues)
				history.Issues = append(history.Issues, EstateIssue{ID: id, WorkloadID: workloadID, Name: name, Kind: kind, FirstSeen: snapshot.At})
				byID[id] = index
			}
			issue := &history.Issues[index]
			if !issue.Active {
				issue.Occurrences++
				issue.FirstSeen = snapshot.At
			}
			issue.Name, issue.Active, issue.LastSeen, issue.ResolvedAt = name, true, snapshot.At, time.Time{}
			issue.ObservedDays = estateUniqueStrings(append(issue.ObservedDays, snapshot.Day))
		}
		if snapshot.HealthComplete[kind] {
			for i := range history.Issues {
				issue := &history.Issues[i]
				if issue.Kind == kind && issue.Active {
					if _, exists := seen[issue.WorkloadID]; !exists {
						issue.Active, issue.ResolvedAt = false, snapshot.At
					}
				}
			}
		}
	}
	sort.Slice(history.Issues, func(i, j int) bool {
		if history.Issues[i].Active != history.Issues[j].Active {
			return history.Issues[i].Active
		}
		if history.Issues[i].FirstSeen.Equal(history.Issues[j].FirstSeen) {
			return history.Issues[i].ID < history.Issues[j].ID
		}
		return history.Issues[i].FirstSeen.Before(history.Issues[j].FirstSeen)
	})
}

func estatePruneHistory(history *EstateHistory, now time.Time, loc *time.Location, retention int) {
	if retention <= 0 {
		retention = 90
	}
	if loc == nil {
		loc = time.UTC
	}
	cutoff := now.In(loc).AddDate(0, 0, -retention+1).Format("2006-01-02")
	days := history.Days[:0]
	for _, day := range history.Days {
		if day.Day >= cutoff {
			days = append(days, day)
		}
	}
	history.Days = days
	changes := history.Changes[:0]
	for _, change := range history.Changes {
		if change.At.In(loc).Format("2006-01-02") >= cutoff {
			changes = append(changes, change)
		}
	}
	history.Changes = changes
	issues := history.Issues[:0]
	for _, issue := range history.Issues {
		observedDays := issue.ObservedDays[:0]
		for _, day := range issue.ObservedDays {
			if day >= cutoff {
				observedDays = append(observedDays, day)
			}
		}
		issue.ObservedDays = observedDays
		if issue.Active || len(observedDays) > 0 {
			issues = append(issues, issue)
		}
	}
	history.Issues = issues
}

func estateCollectPolicy(baseURL string, now time.Time) EstatePolicy {
	ctx, cancel := context.WithTimeout(context.Background(), estateCollectionTimeout)
	defer cancel()
	reader, err := configuredEstatePCEReader(ctx)
	if err != nil {
		return EstatePolicy{At: now, Pending: []EstatePolicyChange{}, Warnings: []string{"Policy inventory unavailable: " + err.Error()}}
	}
	return estateCollectPolicyWithReader(baseURL, now, reader)
}

func estatePolicyRefreshDue(policy *EstatePolicy, attemptedAt, now time.Time) bool {
	if policy == nil || attemptedAt.IsZero() {
		return true
	}
	interval := 15 * time.Minute
	if !policy.Active.Complete || !policy.Draft.Complete || !policy.PendingComplete || len(policy.Warnings) != 0 {
		interval = 5 * time.Minute // Retry incomplete evidence on the next normal sample.
	}
	return now.Sub(attemptedAt) >= interval
}

func estateCollectPolicyWithReader(baseURL string, now time.Time, reader *estatePCEReader) EstatePolicy {
	policy := EstatePolicy{At: now, Pending: []EstatePolicyChange{}, Warnings: []string{}}
	for _, state := range []string{"active", "draft"} {
		count, err := estatePolicyCounts(baseURL+"/sec_policy/"+state, reader.collection)
		if state == "active" {
			policy.Active = count
		} else {
			policy.Draft = count
		}
		if err != nil {
			policy.Warnings = append(policy.Warnings, state+" policy inventory incomplete: "+err.Error())
		}
	}
	pending, pendingErr := reader.pending(baseURL + "/sec_policy/pending")
	policy.Pending, policy.PendingComplete = estateParsePendingPolicy(pending)
	if pendingErr != nil {
		policy.PendingComplete = false
		policy.Warnings = append(policy.Warnings, "Pending policy changes unavailable: "+pendingErr.Error())
	} else if !policy.PendingComplete {
		policy.Warnings = append(policy.Warnings, "Pending policy response had an unsupported format")
	}
	provisionedAt, err := estateLastProvisionedAt(baseURL, reader)
	if err != nil {
		policy.Warnings = append(policy.Warnings, "Last policy provisioning time unavailable: "+err.Error())
	} else {
		policy.LastProvisionedAt = provisionedAt
	}
	return policy
}

func estateLastProvisionedAt(baseURL string, reader *estatePCEReader) (time.Time, error) {
	// The active version exposes the actual provisioning created_at directly;
	// do not page every historical version merely to find its newest timestamp.
	var active map[string]interface{}
	err := reader.object(baseURL+"/sec_policy/active", &active)
	if err == nil {
		if at, parseErr := time.Parse(time.RFC3339Nano, mapString(active, "created_at")); parseErr == nil {
			return at, nil
		}
	} else {
		var statusErr *estateCollectionHTTPError
		if !errors.As(err, &statusErr) || (statusErr.status != 400 && statusErr.status != 404 && statusErr.status != 405 && statusErr.status != 406 && statusErr.status != 501) {
			return time.Time{}, err
		}
	}
	// Older PCEs may require a numeric version instead of the active alias.
	// A fallback history must still be complete, not an ignored-offset page.
	versions, err := reader.collection(baseURL + "/sec_policy")
	if err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, version := range versions {
		at, err := time.Parse(time.RFC3339Nano, mapString(version, "created_at"))
		if err != nil {
			return time.Time{}, errors.New("policy version history contains an invalid provisioning timestamp")
		}
		if at.After(latest) {
			latest = at
		}
	}
	return latest, nil
}

func estatePolicyCounts(baseURL string, fetch func(string) ([]map[string]interface{}, error)) (EstatePolicyCount, error) {
	base, parseErr := url.Parse(baseURL)
	if parseErr != nil || base == nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return EstatePolicyCount{}, errors.New("rules count unavailable: invalid policy collection URL")
	}
	rulesets, err := fetch(baseURL + "/rule_sets")
	count := EstatePolicyCount{Rulesets: len(rulesets)}
	var unknownRules []map[string]interface{}
	for _, row := range rulesets {
		if rules, known := estateRulesetCount(row); known {
			count.Rules += rules
		} else {
			unknownRules = append(unknownRules, row)
		}
	}
	if err != nil {
		return count, err
	}
	for _, ruleset := range unknownRules {
		// Rules belong to a specific ruleset; /sec_policy/{state}/rules is
		// not the documented collection endpoint. Only follow same-state hrefs.
		href, parseErr := url.Parse(mapString(ruleset, "href"))
		if parseErr != nil || href == nil {
			return count, errors.New("rules count unavailable: invalid ruleset href")
		}
		path := href.Path
		if strings.HasPrefix(path, "/orgs/") {
			path = "/api/v2" + path
		}
		prefix := strings.TrimRight(base.Path, "/") + "/rule_sets/"
		id := strings.TrimPrefix(path, prefix)
		if !strings.HasPrefix(path, prefix) || id == "" || id == "." || id == ".." || strings.Contains(id, "/") || (href.Host != "" && (href.Host != base.Host || href.Scheme != base.Scheme)) || href.RawQuery != "" || href.Fragment != "" {
			return count, errors.New("rules count unavailable: ruleset href is outside the requested policy version")
		}
		rules, ruleErr := fetch(baseURL + "/rule_sets/" + url.PathEscape(id) + "/sec_rules")
		if ruleErr != nil {
			return count, fmt.Errorf("rules count unavailable: %w", ruleErr)
		}
		count.Rules += len(rules)
	}
	count.Complete = true
	return count, nil
}

// A present field is not necessarily a count: null, fractional/negative values,
// malformed strings and non-object arrays must never imply a known zero.
func estateRulesetCount(row map[string]interface{}) (int, bool) {
	for _, key := range []string{"rules_count", "num_rules", "rule_count", "rules_total"} {
		if count, ok := estateNonnegativeInteger(row[key]); ok {
			return count, true
		}
	}
	for _, key := range []string{"rules", "sec_rules"} {
		if rules, ok := estatePolicyObjectArray(row[key]); ok {
			return len(rules), true
		}
	}
	return 0, false
}

func estateNonnegativeInteger(raw interface{}) (int, bool) {
	var text string
	switch value := raw.(type) {
	case int:
		return value, value >= 0
	case float64:
		// JSON numbers decode to float64. Stay below the rounded native-int
		// ceiling so conversion cannot overflow on either supported word size.
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || math.Trunc(value) != value || value >= float64(^uint(0)>>1) {
			return 0, false
		}
		return int(value), true
	case json.Number:
		text = string(value)
	case string:
		text = strings.TrimSpace(value)
	default:
		return 0, false
	}
	count, err := strconv.ParseUint(text, 10, strconv.IntSize-1)
	return int(count), err == nil
}

func estatePolicyObjectArray(raw interface{}) ([]map[string]interface{}, bool) {
	var rows []map[string]interface{}
	valid := true
	switch values := raw.(type) {
	case []map[string]interface{}:
		if values == nil {
			return nil, false
		}
		for _, row := range values {
			if row == nil {
				valid = false
				continue
			}
			rows = append(rows, row)
		}
	case []interface{}:
		if values == nil {
			return nil, false
		}
		for _, value := range values {
			if row, ok := value.(map[string]interface{}); ok && row != nil {
				rows = append(rows, row)
			} else {
				valid = false
			}
		}
	default:
		return nil, false
	}
	return rows, valid
}

func estateParsePendingPolicy(payload map[string]interface{}) ([]EstatePolicyChange, bool) {
	changes := []EstatePolicyChange{}
	if payload == nil {
		return changes, false
	}
	known := false
	complete := true
	seen := map[string]bool{}
	for kind, raw := range payload {
		rows, valid := estatePolicyObjectArray(raw)
		if !valid {
			complete = false
		}
		known = true
		for _, row := range rows {
			change := EstatePolicyChange{ID: mapString(row, "href"), Name: firstNonEmpty(mapString(row, "name"), mapString(row, "href")), Kind: kind, Operation: mapString(row, "update_type")}
			if change.ID == "" || change.Operation == "" {
				complete = false
				continue
			}
			key := kind + ":" + change.ID + ":" + change.Operation
			if seen[key] {
				continue
			}
			seen[key] = true
			if at, err := time.Parse(time.RFC3339Nano, mapString(row, "updated_at")); err == nil {
				change.UpdatedAt = at
			}
			changes = append(changes, change)
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Kind == changes[j].Kind {
			return changes[i].ID < changes[j].ID
		}
		return changes[i].Kind < changes[j].Kind
	})
	return changes, known && complete
}

// estateIdentity deliberately excludes credentials. Changing API keys must not
// split an estate, while changing the PCE or organization must never mix history.
func estateIdentity(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err == nil {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		u.Path = strings.TrimSuffix(u.Path, "/")
		baseURL = u.String()
	}
	sum := sha256.Sum256([]byte(baseURL))
	return hex.EncodeToString(sum[:])
}

func estateStringPath(row map[string]interface{}, path ...string) string {
	var current interface{} = row
	for _, part := range path {
		obj, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current = obj[part]
	}
	value, _ := current.(string)
	return strings.TrimSpace(value)
}

func estateVENVersion(row map[string]interface{}) string {
	for _, path := range [][]string{
		{"agent", "status", "agent_version"}, {"agent", "status", "version"},
		{"agent", "agent_version"}, {"agent", "version"},
		{"ven", "status", "agent_version"}, {"ven", "agent_version"}, {"ven", "version"},
		{"agent_version"}, {"ven_version"},
	} {
		if version := estateStringPath(row, path...); version != "" {
			return version
		}
	}
	return "Unknown"
}

func estateWorkloadIdentity(row map[string]interface{}) string {
	for _, field := range []string{"href", "id"} {
		if id := mapString(row, field); id != "" {
			return id
		}
	}
	// Names alone are not stable identities: duplicate names are legal and a
	// renamed workload is not proof that one was removed and another was added.
	return ""
}

func estateObjectArray(raw interface{}) []map[string]interface{} {
	switch values := raw.(type) {
	case []map[string]interface{}:
		return values
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(values))
		for _, value := range values {
			if row, ok := value.(map[string]interface{}); ok {
				out = append(out, row)
			}
		}
		return out
	default:
		return nil
	}
}
