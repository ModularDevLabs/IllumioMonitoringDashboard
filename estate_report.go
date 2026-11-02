package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// Calendar weeks are local Monday midnight through the next Monday midnight.
// AddDate is intentional: a week crossing a DST transition is not 168 hours.
type estateReportWindow struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end_exclusive"`
	FirstDay string    `json:"first_day"`
	LastDay  string    `json:"last_day"`
}

func estateCompletedWeek(now time.Time, loc *time.Location) estateReportWindow {
	if loc == nil {
		loc = time.UTC
	}
	today := localDayStart(now, loc)
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	return estateWindow(monday.AddDate(0, 0, -7), monday)
}

func estateWindow(start, end time.Time) estateReportWindow {
	return estateReportWindow{Start: start, End: end, FirstDay: start.Format("2006-01-02"), LastDay: end.AddDate(0, 0, -1).Format("2006-01-02")}
}

func estateRequestedWeek(raw string, now time.Time, loc *time.Location) (estateReportWindow, error) {
	if loc == nil {
		loc = time.UTC
	}
	if strings.TrimSpace(raw) == "" {
		return estateCompletedWeek(now, loc), nil
	}
	start, err := time.ParseInLocation("2006-01-02", raw, loc)
	if err != nil || start.Weekday() != time.Monday {
		return estateReportWindow{}, errors.New("week_start must be a Monday in YYYY-MM-DD format")
	}
	end := start.AddDate(0, 0, 7)
	if end.After(now) {
		return estateReportWindow{}, errors.New("choose a completed calendar week")
	}
	return estateWindow(start, end), nil
}

var estateReportSections = []string{"estate", "health", "policy", "traffic", "changes", "quality"}

func estateSelectedSections(values []string) ([]string, error) {
	if len(values) == 0 {
		return append([]string(nil), estateReportSections...), nil
	}
	wanted := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		found := false
		for _, allowed := range estateReportSections {
			if value == allowed {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown report section %q", value)
		}
		wanted[value] = true
	}
	result := []string{}
	for _, section := range estateReportSections {
		if wanted[section] {
			result = append(result, section)
		}
	}
	return result, nil
}

type estateReportCount struct {
	Current  int  `json:"current"`
	Previous *int `json:"previous,omitempty"`
	Change   *int `json:"change,omitempty"`
}

type estateReportVersion struct {
	Version string `json:"version"`
	Total   int    `json:"total"`
}

type estateReportInventory struct {
	ObservedAt         time.Time             `json:"observed_at"`
	PreviousObservedAt *time.Time            `json:"previous_observed_at,omitempty"`
	Total              estateReportCount     `json:"total"`
	Managed            estateReportCount     `json:"managed"`
	Unmanaged          estateReportCount     `json:"unmanaged"`
	Enforcement        map[string]int        `json:"enforcement"`
	Versions           []estateReportVersion `json:"versions"`
	VersionsAvailable  bool                  `json:"versions_available"`
	VENCount           int                   `json:"ven_count"`
}

type estateReportHealth struct {
	UniqueAffected         map[string]int `json:"unique_affected"`
	PreviousUniqueAffected map[string]int `json:"previous_unique_affected,omitempty"`
	Description            string         `json:"description"`
}

type estateReportTraffic struct {
	Target              string   `json:"target"`
	Total               int      `json:"total"`
	Previous            *int     `json:"previous,omitempty"`
	Change              *int     `json:"change,omitempty"`
	CoveredDays         int      `json:"covered_days"`
	PreviousCoveredDays int      `json:"previous_covered_days"`
	MissingDays         []string `json:"missing_days"`
}

type estateReportQuality struct {
	CoveredDays         int        `json:"covered_days"`
	MissingDays         []string   `json:"missing_days"`
	PreviousCoveredDays int        `json:"previous_covered_days"`
	Warnings            []string   `json:"warnings"`
	FirstObservation    *time.Time `json:"first_observation,omitempty"`
	LastObservation     *time.Time `json:"last_observation,omitempty"`
}

func estateReportDelta(current int, previous *int) estateReportCount {
	count := estateReportCount{Current: current, Previous: previous}
	if previous != nil {
		delta := current - *previous
		count.Change = &delta
	}
	return count
}

func estateVersionRows(versions map[string]int) []estateReportVersion {
	merged := map[string]int{}
	for version, count := range versions {
		version = strings.TrimSpace(version)
		if version == "" {
			version = "Unknown"
		}
		merged[version] += count
	}
	rows := make([]estateReportVersion, 0, len(merged))
	for version, count := range merged {
		rows = append(rows, estateReportVersion{Version: version, Total: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Total == rows[j].Total {
			return rows[i].Version < rows[j].Version
		}
		return rows[i].Total > rows[j].Total
	})
	return rows
}

type estateWeeklyReport struct {
	SchemaVersion  int                    `json:"schema_version"`
	GeneratedAt    time.Time              `json:"generated_at"`
	Version        string                 `json:"version"`
	Timezone       string                 `json:"timezone"`
	Window         estateReportWindow     `json:"window"`
	PreviousWindow estateReportWindow     `json:"previous_window"`
	Sections       []string               `json:"sections"`
	Estate         *estateReportInventory `json:"estate,omitempty"`
	Health         *estateReportHealth    `json:"health,omitempty"`
	Policy         *EstatePolicy          `json:"policy,omitempty"`
	PreviousPolicy *EstatePolicy          `json:"previous_policy,omitempty"`
	Traffic        []estateReportTraffic  `json:"traffic,omitempty"`
	Changes        []EstateChange         `json:"changes,omitempty"`
	Quality        estateReportQuality    `json:"quality"`
}

func estateWindowContains(at time.Time, window estateReportWindow) bool {
	return !at.IsZero() && !at.Before(window.Start) && at.Before(window.End)
}

func estateLatestInventory(history EstateHistory, window estateReportWindow) *EstateSnapshot {
	var latest *EstateSnapshot
	for _, snapshot := range history.Days {
		if snapshot.InventoryComplete && estateWindowContains(snapshot.At, window) && (latest == nil || snapshot.At.After(latest.At)) {
			copy := snapshot
			latest = &copy
		}
	}
	return latest
}

func estateLatestPolicy(history EstateHistory, window estateReportWindow) *EstatePolicy {
	var latest *EstatePolicy
	for _, snapshot := range history.Days {
		policy := snapshot.Policy
		if policy != nil && estateWindowContains(policy.At, window) && (latest == nil || policy.At.After(latest.At)) {
			latest = policy
		}
	}
	return latest
}

func estateWeeklyHealth(history EstateHistory, window estateReportWindow) map[string]int {
	sets := map[string]map[string]bool{}
	for _, snapshot := range history.Days {
		if estateWindowContains(snapshot.At, window) {
			for kind, complete := range snapshot.HealthComplete {
				if complete || snapshot.HealthSamples[kind] > 0 {
					sets[kind] = map[string]bool{}
				}
			}
		}
	}
	for _, issue := range history.Issues {
		if issue.Kind == "tampering" {
			continue
		} // Rolling observations are not event timestamps.
		for _, day := range issue.ObservedDays {
			if day >= window.FirstDay && day <= window.LastDay {
				if sets[issue.Kind] == nil {
					sets[issue.Kind] = map[string]bool{}
				}
				id := issue.WorkloadID
				if id == "" {
					id = issue.ID
				}
				if id != "" {
					sets[issue.Kind][id] = true
				}
				break
			}
		}
	}
	for _, snapshot := range history.Days {
		for id, at := range snapshot.Tampered {
			if id != "" && estateWindowContains(at, window) {
				if sets["tampering"] == nil {
					sets["tampering"] = map[string]bool{}
				}
				sets["tampering"][id] = true
			}
		}
	}
	counts := map[string]int{}
	for kind, identities := range sets {
		counts[kind] = len(identities)
	}
	return counts
}

func estateWeekCoverage(history EstateHistory, window estateReportWindow) (int, []string) {
	observed := map[string]bool{}
	for _, snapshot := range history.Days {
		if estateWindowContains(snapshot.At, window) {
			observed[snapshot.At.In(window.Start.Location()).Format("2006-01-02")] = true
		}
	}
	missing := []string{}
	for day := window.Start; day.Before(window.End); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		if !observed[key] {
			missing = append(missing, key)
		}
	}
	return 7 - len(missing), missing
}

func buildEstateWeeklyReport(history EstateHistory, opts EstateOptions, window estateReportWindow, sections []string, generatedAt time.Time, traffic []estateReportTraffic) estateWeeklyReport {
	previousWindow := estateWindow(window.Start.AddDate(0, 0, -7), window.Start)
	report := estateWeeklyReport{SchemaVersion: 1, GeneratedAt: generatedAt.UTC(), Version: dashboardVersionLabel(), Timezone: window.Start.Location().String(), Window: window, PreviousWindow: previousWindow, Sections: sections}
	report.Quality.CoveredDays, report.Quality.MissingDays = estateWeekCoverage(history, window)
	report.Quality.PreviousCoveredDays, _ = estateWeekCoverage(history, previousWindow)
	warnings := []string{"Inventory and policy values are the latest retained observations in each week, not exact midnight totals. Health counts are distinct observed workloads, not sums of daily peaks or rolling 24-hour counts."}
	if report.Quality.CoveredDays < 7 {
		warnings = append(warnings, "This week has missing observation days; unknown periods are not zero activity.")
	}
	if report.Quality.PreviousCoveredDays < 7 {
		warnings = append(warnings, "The comparison week has incomplete observation coverage; week-over-week changes may not be comparable.")
	}
	for _, snapshot := range history.Days {
		if !estateWindowContains(snapshot.At, window) {
			continue
		}
		first := snapshot.FirstAt
		if first.IsZero() {
			first = snapshot.At
		}
		if report.Quality.FirstObservation == nil || first.Before(*report.Quality.FirstObservation) {
			value := first
			report.Quality.FirstObservation = &value
		}
		if report.Quality.LastObservation == nil || snapshot.At.After(*report.Quality.LastObservation) {
			value := snapshot.At
			report.Quality.LastObservation = &value
		}
		if !snapshot.InventoryComplete {
			warnings = append(warnings, "At least one inventory observation was incomplete; partial inventory is not used as a weekly inventory total.")
		}
		if snapshot.CompleteSamples < snapshot.Samples {
			warnings = append(warnings, "Some inventory collection attempts were incomplete, even when the latest daily observation succeeded.")
		}
		for kind, complete := range snapshot.HealthComplete {
			if !complete {
				warnings = append(warnings, "Health collection was incomplete for "+kind+"; affected counts are observed minimums.")
			}
			if snapshot.HealthSamples[kind] < snapshot.Samples {
				warnings = append(warnings, "Some "+kind+" collection attempts were incomplete; affected counts are observed minimums.")
			}
		}
		warnings = append(warnings, snapshot.Warnings...)
	}
	warnings = append(warnings, "A day with observations does not guarantee continuous coverage. Changes between collection samples may be missed.")
	selected := map[string]bool{}
	for _, section := range sections {
		selected[section] = true
	}
	if selected["estate"] {
		latest := estateLatestInventory(history, window)
		previous := estateLatestInventory(history, previousWindow)
		if latest == nil {
			warnings = append(warnings, "No complete inventory observation exists in this week. Estate totals and VEN versions are unavailable, not zero.")
		} else {
			inventory := &estateReportInventory{ObservedAt: latest.At, Enforcement: latest.Modes}
			var total, managed, unmanaged *int
			if previous != nil {
				total, managed, unmanaged = &previous.Total, &previous.Managed, &previous.Unmanaged
				inventory.PreviousObservedAt = &previous.At
			}
			inventory.Total, inventory.Managed, inventory.Unmanaged = estateReportDelta(latest.Total, total), estateReportDelta(latest.Managed, managed), estateReportDelta(latest.Unmanaged, unmanaged)
			versions := map[string]int{}
			versionTotal := 0
			for _, version := range latest.Versions {
				versions[version.Version] += version.Count
				versionTotal += version.Count
			}
			inventory.VersionsAvailable = latest.VersionsCollected || len(latest.Versions) > 0
			inventory.VENCount = latest.VENCount
			if inventory.VENCount == 0 && inventory.VersionsAvailable {
				inventory.VENCount = versionTotal
			}
			if inventory.VersionsAvailable && versionTotal < latest.VENCount {
				versions["Unknown"] += latest.VENCount - versionTotal
			}
			if !inventory.VersionsAvailable {
				warnings = append(warnings, "VEN versions were not retained for this inventory observation; version distribution is unavailable, not zero deployed VENs.")
			}
			inventory.Versions = estateVersionRows(versions)
			report.Estate = inventory
		}
	}
	if selected["health"] && report.Quality.CoveredDays > 0 {
		report.Health = &estateReportHealth{UniqueAffected: estateWeeklyHealth(history, window), Description: "Distinct workload identities observed in each status during the calendar week. One workload may appear in multiple statuses. Tampering uses event timestamps, not rolling scan timestamps."}
		if report.Quality.PreviousCoveredDays > 0 {
			report.Health.PreviousUniqueAffected = estateWeeklyHealth(history, previousWindow)
		}
	}
	if selected["policy"] {
		report.Policy, report.PreviousPolicy = estateLatestPolicy(history, window), estateLatestPolicy(history, previousWindow)
		if !opts.PolicyEnabled {
			warnings = append(warnings, "Active/draft policy insights collection is currently disabled; only any retained policy observations can be shown.")
		}
		if report.Policy == nil {
			warnings = append(warnings, "No active/draft policy observation exists for this week.")
		} else {
			warnings = append(warnings, report.Policy.Warnings...)
		}
	}
	if selected["traffic"] {
		report.Traffic = traffic
		warnings = append(warnings, "Traffic is reported per configured monitoring target with its exclusions. Targets may overlap and must not be added into an estate-wide unique total. Changing target scope can affect comparisons.")
		warnings = append(warnings, "Weekly traffic includes only verified complete-day queries retained while Weekly Estate Reports were enabled for this PCE, organization, timezone and exclusion settings. Legacy traffic history has no estate provenance and is excluded; missing days remain unavailable until a successful daily or manual reconciliation verifies them.")
	}
	if selected["changes"] {
		for _, change := range history.Changes {
			if estateWindowContains(change.At, window) {
				report.Changes = append(report.Changes, change)
			}
		}
		if !opts.ChangeLogEnabled {
			warnings = append(warnings, "The estate change log is currently disabled; only retained changes, if any, are available.")
		}
	}
	seenWarnings := map[string]bool{}
	for _, warning := range warnings {
		if warning != "" && !seenWarnings[warning] {
			report.Quality.Warnings = append(report.Quality.Warnings, warning)
			seenWarnings[warning] = true
		}
	}
	return report
}

func estateTrafficForWeek(window estateReportWindow) []estateReportTraffic {
	targets := configuredTrafficTargets()
	scope, timezone := estateScopeForConfig(), configuredDayLocation().String()
	estateVerifiedTrafficMu.Lock()
	store := estateReadVerifiedTraffic(scope, timezone)
	estateVerifiedTrafficMu.Unlock()
	rows := []estateReportTraffic{}
	for _, target := range targets {
		row := estateReportTraffic{Target: target.Name, MissingDays: []string{}}
		pending := trafficServiceHistoryPending(target.Name)
		fingerprint := estateVerifiedTrafficFingerprint(target)
		prior := 0
		for day := window.Start.AddDate(0, 0, -7); day.Before(window.End); day = day.AddDate(0, 0, 1) {
			key := day.Format("2006-01-02")
			record, ok := store.Records[estateVerifiedTrafficKey(key, target.Name)]
			ok = ok && record.Fingerprint == fingerprint && window.Start.Location().String() == timezone
			value := record.Count
			if pending {
				ok = false
			}
			if day.Before(window.Start) {
				if ok {
					prior += value
					row.PreviousCoveredDays++
				}
				continue
			}
			if ok {
				row.Total += value
				row.CoveredDays++
			} else {
				row.MissingDays = append(row.MissingDays, key)
			}
		}
		// A delta requires both complete weeks, not a partial previous baseline.
		if row.PreviousCoveredDays == 7 {
			row.Previous = &prior
			if row.CoveredDays == 7 {
				change := row.Total - prior
				row.Change = &change
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// Legacy blockedDaily records have no PCE/org identity. Weekly reports never
// reuse them: only complete-day queries observed under a verified estate and
// filter fingerprint can populate this separate opt-in report store.
type estateVerifiedTrafficRecord struct {
	Day         string    `json:"day"`
	Target      string    `json:"target"`
	Fingerprint string    `json:"fingerprint"`
	Count       int       `json:"count"`
	VerifiedAt  time.Time `json:"verified_at"`
}

type estateVerifiedTrafficStore struct {
	Scope    string                                 `json:"scope"`
	Timezone string                                 `json:"timezone"`
	Records  map[string]estateVerifiedTrafficRecord `json:"records"`
}

var estateVerifiedTrafficMu sync.Mutex

func estateVerifiedTrafficPath(scope, timezone string) string {
	sum := sha256.Sum256([]byte(scope + "|" + timezone))
	return dataFilePath("estate_traffic_verified_" + hex.EncodeToString(sum[:]) + ".json")
}

func estateVerifiedTrafficKey(day, target string) string { return day + "\x00" + target }

func estateReadVerifiedTraffic(scope, timezone string) estateVerifiedTrafficStore {
	store := estateVerifiedTrafficStore{Scope: scope, Timezone: timezone, Records: map[string]estateVerifiedTrafficRecord{}}
	data, err := os.ReadFile(estateVerifiedTrafficPath(scope, timezone))
	if err != nil {
		return store
	}
	var saved estateVerifiedTrafficStore
	if json.Unmarshal(data, &saved) != nil || saved.Scope != scope || saved.Timezone != timezone || saved.Records == nil {
		return store
	}
	return saved
}

func estateVerifiedTrafficFingerprint(target TrafficTarget) string {
	sources := []string{}
	for _, source := range configuredSourceExclusions() {
		sources = append(sources, strings.ToLower(strings.TrimSpace(source.Kind))+":"+strings.TrimSpace(source.Name))
	}
	sort.Strings(sources)
	services := append([]string(nil), effectiveTrafficServiceExclusions(target)...)
	sort.Strings(services)
	resolved, known := estateVerifiedTrafficResolvedServices(target, services)
	if !known {
		return ""
	}
	payload, _ := json.Marshal(struct {
		Schema           int
		Scope            string
		Timezone         string
		Target           string
		Kind             string
		Sources          []string
		Services         []string
		ResolvedServices string
	}{2, estateScopeForConfig(), configuredDayLocation().String(), strings.TrimSpace(target.Name), strings.ToLower(strings.TrimSpace(target.Kind)), sources, services, resolved})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func estateVerifiedTrafficResolvedServices(target TrafficTarget, selectors []string) (string, bool) {
	if strings.TrimSpace(target.serviceResolutionError) != "" {
		return "", false
	}
	if len(selectors) == 0 {
		return resolvedServiceExclusionFingerprint(nil), true
	}
	// Query inputs carry the actual resolved service contents. A named policy
	// service can change ports without changing its selector/name.
	if fingerprint := strings.TrimSpace(target.resolvedServiceFingerprint); fingerprint != "" {
		return fingerprint, true
	}
	// Report reads must not call the PCE. Use the current resolved fingerprint
	// already maintained by the collector, and fail closed until it is known.
	rollingMu.Lock()
	fingerprint := rollingCache.ServiceFilterFingerprints[target.Name]
	rollingMu.Unlock()
	return fingerprint, strings.TrimSpace(fingerprint) != ""
}

// Call while configUpdateMu is already held after validating the query inputs.
// capturedFingerprint should be captured alongside those inputs before IO.
func estateRecordVerifiedTraffic(baseURL string, target TrafficTarget, day string, count int, complete bool, capturedFingerprint ...string) {
	opts := configuredEstateOptions()
	if !opts.WeeklyReportEnabled || !complete || count < 0 || strings.TrimSpace(target.Name) == "" || len(capturedFingerprint) != 1 || capturedFingerprint[0] == "" {
		return
	}
	scope := estateScopeForConfig()
	if estateIdentity(baseURL) != scope {
		return
	}
	loc := configuredDayLocation()
	parsed, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil || !parsed.Before(localDayStart(time.Now(), loc)) {
		return
	}
	fingerprint := estateVerifiedTrafficFingerprint(target)
	if capturedFingerprint[0] != fingerprint {
		return
	}
	currentTarget, current := configuredTrafficTargetByIdentity(target)
	if !current || estateVerifiedTrafficFingerprint(currentTarget) != fingerprint {
		return
	}
	estateVerifiedTrafficMu.Lock()
	defer estateVerifiedTrafficMu.Unlock()
	store := estateReadVerifiedTraffic(scope, loc.String())
	store.Records[estateVerifiedTrafficKey(day, target.Name)] = estateVerifiedTrafficRecord{Day: day, Target: target.Name, Fingerprint: fingerprint, Count: count, VerifiedAt: time.Now().UTC()}
	cutoff := localDayStart(time.Now(), loc).AddDate(0, 0, -opts.HistoryDays).Format("2006-01-02")
	for key, record := range store.Records {
		if record.Day < cutoff {
			delete(store.Records, key)
		}
	}
	if err := writeJSONFileAtomic(estateVerifiedTrafficPath(scope, loc.String()), store); err != nil {
		log.Printf("[ESTATE] verified daily traffic could not be saved: %v", err)
	}
}

func renderEstateReportHTML(report estateWeeklyReport, theme string) ([]byte, error) {
	switch theme {
	case "light", "illumio-light":
		theme = "illumio-light"
	case "illumio-dark":
	default:
		theme = "dark"
	}
	funcs := template.FuncMap{
		"selected": func(name string) bool {
			for _, section := range report.Sections {
				if section == name {
					return true
				}
			}
			return false
		},
		"date": func(at time.Time) string {
			if at.IsZero() {
				return "Unavailable"
			}
			loc, err := time.LoadLocation(report.Timezone)
			if err != nil {
				loc = time.UTC
			}
			return at.In(loc).Format("Jan 2, 2006 15:04 MST")
		},
		"signed": func(n *int) string {
			if n == nil {
				return "Unavailable"
			}
			return fmt.Sprintf("%+d", *n)
		},
		"previous": func(n *int) string {
			if n == nil {
				return "Unavailable"
			}
			return fmt.Sprint(*n)
		},
		"previousHealth": func(kind string) string {
			if report.Health == nil {
				return "Unavailable"
			}
			value, ok := report.Health.PreviousUniqueAffected[kind]
			if !ok {
				return "Unavailable"
			}
			return fmt.Sprint(value)
		},
	}
	t, err := template.New("weekly-estate").Funcs(funcs).Parse(estateReportHTML)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, struct {
		Report estateWeeklyReport
		Theme  string
	}{Report: report, Theme: theme})
	return buf.Bytes(), err
}

const estateReportHTML = `<!doctype html>
<html lang="en" data-theme="{{.Theme}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Weekly Estate Report — {{.Report.Window.FirstDay}}</title>
<style>
:root{color-scheme:dark;--bg:#171a1b;--surface:#222627;--text:#f5f4f0;--muted:#b3b8b6;--border:#3f4647;--accent:#ff6b2b}
[data-theme="illumio-light"]{color-scheme:light;--bg:#f5f3ee;--surface:#fff;--text:#272b2d;--muted:#6d706f;--border:#d7d1c5;--accent:#f26322}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:15px/1.6 system-ui,-apple-system,sans-serif}main{max-width:1180px;margin:auto;padding:32px}header{border-bottom:3px solid var(--accent);margin-bottom:24px;padding-bottom:20px}h1{font-size:30px;margin:8px 0}h2{font-size:22px;margin:0 0 16px}h3{margin:24px 0 10px}p{margin:8px 0}.eyebrow{text-transform:uppercase;letter-spacing:.16em;font-size:12px;color:var(--accent)}.muted{color:var(--muted)}section{background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:24px;margin-bottom:22px;break-inside:avoid}.cards{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:16px}.card{border:1px solid var(--border);border-radius:8px;padding:16px}.number{font-size:30px;font-weight:700;color:var(--accent)}table{border-collapse:collapse;width:100%;margin:12px 0}th,td{text-align:left;padding:9px 12px;border-bottom:1px solid var(--border);vertical-align:top}th{color:var(--muted);font-weight:600}td{overflow-wrap:anywhere}.notice{border-left:3px solid var(--accent);padding-left:14px}footer{font-size:12px;color:var(--muted);padding-top:12px}ul{padding-left:24px}.scroll{overflow-x:auto}@media(max-width:650px){main{padding:16px}.cards{grid-template-columns:1fr}section{padding:16px}}@media print{body{background:#fff!important;color:#162126!important}main{max-width:none;padding:0}section{background:#fff!important;color:#162126!important;border-color:#ddd;box-shadow:none;break-inside:auto}.muted,th,footer{color:#4b5563!important}.number,.eyebrow{color:#ad4600!important}table{font-size:11px}thead{display:table-header-group}tr{break-inside:avoid}h2,h3{break-after:avoid}header{break-after:avoid}@page{margin:15mm}
</style></head><body><main><header><div class="eyebrow">Illumio Monitoring Dashboard</div><h1>Weekly Estate Report</h1><p>{{.Report.Window.FirstDay}} through {{.Report.Window.LastDay}} · {{.Report.Timezone}}</p><p class="muted">Compared with {{.Report.PreviousWindow.FirstDay}} through {{.Report.PreviousWindow.LastDay}}. Generated {{date .Report.GeneratedAt}}.</p><p class="notice">{{.Report.Quality.CoveredDays}} of 7 days have retained observations. Counts describe observed data; missing periods are unknown, not zero.</p></header>
{{if selected "estate"}}<section id="estate"><h2>Estate overview</h2>{{with .Report.Estate}}<p class="muted">Inventory observed {{date .ObservedAt}}{{with .PreviousObservedAt}}; comparison observed {{date .}}{{end}}. Management does not imply enforcement.</p><div class="cards"><div class="card">Total workloads<div class="number">{{.Total.Current}}</div><span class="muted">Week-over-week {{signed .Total.Change}}</span></div><div class="card">Managed<div class="number">{{.Managed.Current}}</div><span class="muted">Week-over-week {{signed .Managed.Change}}</span></div><div class="card">Unmanaged<div class="number">{{.Unmanaged.Current}}</div><span class="muted">Week-over-week {{signed .Unmanaged.Change}}</span></div></div><h3>VEN version distribution</h3>{{if .VersionsAvailable}}<p class="muted">{{.VENCount}} unique deployed VENs, grouped by version. A VEN shared by workload records is counted once. Missing version information is shown as Unknown.</p><table><thead><tr><th>VEN version</th><th>Total deployed VENs</th></tr></thead><tbody>{{range .Versions}}<tr><td>{{.Version}}</td><td>{{.Total}}</td></tr>{{else}}<tr><td colspan="2">No deployed VENs in this inventory snapshot.</td></tr>{{end}}</tbody></table>{{else}}<p>Version distribution was not collected for this observation. It is unavailable, not zero deployed VENs.</p>{{end}}<h3>Managed workload enforcement</h3><table><thead><tr><th>Mode</th><th>Workloads</th></tr></thead><tbody>{{range $mode,$count := .Enforcement}}<tr><td>{{$mode}}</td><td>{{$count}}</td></tr>{{end}}</tbody></table>{{else}}<p>No complete inventory observation is available for this week.</p>{{end}}</section>{{end}}
{{if selected "health"}}<section id="health"><h2>VEN health</h2>{{with .Report.Health}}<p class="muted">{{.Description}}</p><table><thead><tr><th>Status</th><th>Distinct observed workloads</th><th>Previous week</th></tr></thead><tbody>{{range $kind,$count := .UniqueAffected}}<tr><td>{{$kind}}</td><td>{{$count}}</td><td>{{previousHealth $kind}}</td></tr>{{else}}<tr><td colspan="3">Health history was not collected successfully for this week.</td></tr>{{end}}</tbody></table>{{else}}<p>No health observation is available for this week.</p>{{end}}</section>{{end}}
{{if selected "policy"}}<section id="policy"><h2>Policy overview</h2><p class="muted">Active and draft represent policy versions. Their counts must not be added into a single unique total.</p>{{with .Report.Policy}}<p>Observed {{date .At}}</p><table><thead><tr><th>Version</th><th>Rulesets</th><th>Rules</th></tr></thead><tbody><tr><td>Active</td>{{if .Active.Complete}}<td>{{.Active.Rulesets}}</td><td>{{.Active.Rules}}</td>{{else}}<td colspan="2">Unavailable / incomplete</td>{{end}}</tr><tr><td>Draft</td>{{if .Draft.Complete}}<td>{{.Draft.Rulesets}}</td><td>{{.Draft.Rules}}</td>{{else}}<td colspan="2">Unavailable / incomplete</td>{{end}}</tr></tbody></table><p>Last observed provisioning time: {{date .LastProvisionedAt}}</p><h3>Pending changes at this observation</h3>{{if .PendingComplete}}<table><thead><tr><th>Object</th><th>Kind</th><th>Change</th></tr></thead><tbody>{{range .Pending}}<tr><td>{{.Name}}</td><td>{{.Kind}}</td><td>{{.Operation}}</td></tr>{{else}}<tr><td colspan="3">No pending changes.</td></tr>{{end}}</tbody></table>{{else}}<p>Pending changes are unavailable or incomplete.</p>{{end}}{{else}}<p>No active/draft policy observation is available for this week. Enable Policy Insights to collect it.</p>{{end}}</section>{{end}}
{{if selected "traffic"}}<section id="traffic"><h2>Monitored blocked traffic</h2><p class="notice">Targets may overlap. Counts are per target, after configured exclusions, and are not added into an estate-wide total. Partial-week counts must not be compared with complete weeks.</p><p class="muted">Only verified complete-day queries retained for this PCE, organization, timezone and exclusion settings are included. Legacy history without this provenance is excluded; successful daily or manual reconciliation can verify missing days.</p><div class="scroll"><table><thead><tr><th>Target</th><th>Observed total</th><th>Covered days</th><th>Previous complete week</th><th>Change</th></tr></thead><tbody>{{range .Report.Traffic}}<tr><td>{{.Target}}</td><td>{{if .CoveredDays}}{{.Total}}{{else}}Unavailable{{end}}</td><td>{{.CoveredDays}} / 7</td><td>{{previous .Previous}}</td><td>{{signed .Change}}</td></tr>{{else}}<tr><td colspan="5">No monitored target history is available.</td></tr>{{end}}</tbody></table></div></section>{{end}}
{{if selected "changes"}}<section id="changes"><h2>Estate change log</h2><p class="muted">Observed changes since the retained baseline; first-time inventory is not treated as newly added workloads.</p><table><thead><tr><th>Observed</th><th>Workload / object</th><th>Change</th><th>Before</th><th>After</th></tr></thead><tbody>{{range .Report.Changes}}<tr><td>{{date .At}}</td><td>{{.Name}}</td><td>{{.Kind}}</td><td>{{.Before}}</td><td>{{.After}}</td></tr>{{else}}<tr><td colspan="5">No retained changes in this week. See data quality for collection availability.</td></tr>{{end}}</tbody></table></section>{{end}}
{{if selected "quality"}}<section id="quality"><h2>Data quality and scope</h2><p>This week: {{.Report.Quality.CoveredDays}} / 7 days with observations. Previous week: {{.Report.Quality.PreviousCoveredDays}} / 7.</p>{{with .Report.Quality.FirstObservation}}<p>First observation: {{date .}}</p>{{end}}{{with .Report.Quality.LastObservation}}<p>Last observation: {{date .}}</p>{{end}}{{with .Report.Quality.MissingDays}}<p>Missing days: {{range $i,$day := .}}{{if $i}}, {{end}}{{$day}}{{end}}</p>{{end}}<ul>{{range .Report.Quality.Warnings}}<li>{{.}}</li>{{end}}</ul></section>{{end}}
<footer>Illumio Monitoring Dashboard {{.Report.Version}} · Frozen report generated {{date .Report.GeneratedAt}} · Use your browser’s Print / Save as PDF command to create a PDF.</footer></main></body></html>`

type estateReportRun struct {
	ID                  string             `json:"id"`
	Scope               string             `json:"scope"`
	CreatedAt           time.Time          `json:"created_at"`
	State               string             `json:"state"`
	DeliveryAttemptedAt *time.Time         `json:"delivery_attempted_at,omitempty"`
	FinishedAt          *time.Time         `json:"finished_at,omitempty"`
	Message             string             `json:"message,omitempty"`
	Report              estateWeeklyReport `json:"report"`
}

type estateReportRunSummary struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	State     string    `json:"state"`
	Week      string    `json:"week"`
	Message   string    `json:"message,omitempty"`
}

var estateReportScheduleMu sync.Mutex

func estateReportDue(now time.Time, opts EstateOptions, loc *time.Location) (estateReportWindow, bool) {
	if loc == nil {
		loc = time.UTC
	}
	window := estateCompletedWeek(now, loc)
	if !opts.WeeklyReportEnabled || !opts.WeeklyScheduleEnabled {
		return window, false
	}
	days := map[string]int{"monday": 0, "tuesday": 1, "wednesday": 2, "thursday": 3, "friday": 4, "saturday": 5, "sunday": 6}
	day, ok := days[strings.ToLower(strings.TrimSpace(opts.WeeklyDay))]
	if !ok {
		day = 0
	}
	hour, minute := 9, 0
	if clock, err := time.Parse("15:04", opts.WeeklyTime); err == nil {
		hour, minute = clock.Hour(), clock.Minute()
	}
	scheduledDay := window.End.AddDate(0, 0, day).In(loc)
	due := time.Date(scheduledDay.Year(), scheduledDay.Month(), scheduledDay.Day(), hour, minute, 0, 0, loc)
	return window, !now.Before(due)
}

func estateReportRunID(scope string, window estateReportWindow) string {
	sum := sha256.Sum256([]byte(scope + "|" + window.Start.Location().String() + "|" + window.FirstDay))
	return hex.EncodeToString(sum[:])
}

func estateReportRunPath(id string) (string, error) {
	if len(id) != 64 {
		return "", errors.New("invalid report ID")
	}
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("invalid report ID")
	}
	return filepath.Join(dataFilePath("estate-reports"), strings.ToLower(id)+".json"), nil
}

func estateWeeklyDeliveryPayload(report estateWeeklyReport, dashboardURL string) map[string]interface{} {
	lines := []string{fmt.Sprintf("Weekly Estate Report: %s through %s (%s)", report.Window.FirstDay, report.Window.LastDay, report.Timezone)}
	if report.Estate != nil {
		lines = append(lines, fmt.Sprintf("Workloads: %d total; %d managed; %d unmanaged.", report.Estate.Total.Current, report.Estate.Managed.Current, report.Estate.Unmanaged.Current))
		versions := []string{}
		for _, version := range report.Estate.Versions {
			versions = append(versions, fmt.Sprintf("%s: %d", version.Version, version.Total))
		}
		if len(versions) > 0 {
			lines = append(lines, "VEN versions — "+strings.Join(versions, "; "))
		}
	}
	lines = append(lines, fmt.Sprintf("Observation coverage: %d/7 days. Download the frozen HTML report from Weekly Report history in the dashboard.", report.Quality.CoveredDays))
	if report.Quality.CoveredDays < 7 {
		lines = append(lines, "Coverage is incomplete; missing periods are not zero activity.")
	}
	return map[string]interface{}{"event": "weekly_estate_report", "state": "completed", "timestamp": report.GeneratedAt.Format(time.RFC3339), "reason": strings.Join(lines, "\n"), "dashboard_url": strings.TrimSuffix(dashboardURL, "/") + "/estate", "source_app": "illumio-monitoring-dashboard", "report": report}
}

// runEstateReportSchedule is independent of the PCE collector: it uses retained
// observations only. A durable attempt marker precedes delivery so restarting
// after a timeout/crash cannot silently send the same report twice.
func runEstateReportSchedule(now time.Time, opts EstateOptions, loc *time.Location, history EstateHistory, traffic []estateReportTraffic, deliver func(estateWeeklyReport) error) error {
	window, due := estateReportDue(now, opts, loc)
	if !due || strings.TrimSpace(history.Scope) == "" {
		return nil
	}
	sections, err := estateSelectedSections(opts.WeeklySections)
	if err != nil {
		return err
	}
	estateReportScheduleMu.Lock()
	defer estateReportScheduleMu.Unlock()
	id := estateReportRunID(history.Scope, window)
	path, err := estateReportRunPath(id)
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	report := buildEstateWeeklyReport(history, opts, window, sections, now, traffic)
	run := estateReportRun{ID: id, Scope: history.Scope, CreatedAt: now.UTC(), State: "generated", Report: report, Message: "Saved locally. Webhook delivery has not been attempted."}
	if !opts.WeeklyWebhookEnabled {
		run.Message = "Saved locally. Webhook delivery was not enabled."
	}
	if err := writeJSONFileAtomic(path, run); err != nil {
		return err
	}
	if !opts.WeeklyWebhookEnabled {
		return nil
	}
	if strings.TrimSpace(opts.WeeklyWebhookURL) == "" || deliver == nil {
		run.State, run.Message = "delivery_failed", "No webhook destination is configured. The frozen report remains available locally."
		return writeJSONFileAtomic(path, run)
	}
	stamp := now.UTC()
	run.State, run.DeliveryAttemptedAt, run.Message = "delivery_attempted", &stamp, "Delivery was attempted. If this remains after a restart, its outcome is uncertain; automatic retries are disabled to avoid duplicate reports."
	if err := writeJSONFileAtomic(path, run); err != nil {
		return err
	}
	deliveryErr := deliver(report)
	finished := time.Now().UTC()
	run.FinishedAt = &finished
	if deliveryErr != nil {
		// Do not persist transport errors, which can contain secret webhook URLs.
		run.State, run.Message = "delivery_failed", "Webhook delivery failed or its outcome is uncertain. Check the destination; automatic retries are disabled to avoid duplicates. The frozen report remains available locally."
	} else {
		run.State, run.Message = "delivered", "Webhook accepted the report. Slack/Teams receive a summary and dashboard link; generic webhooks receive the structured report."
	}
	return writeJSONFileAtomic(path, run)
}

func runEstateReportScheduler(ctx context.Context) {
	tick := func() {
		// Keep the scope/options/history capture consistent with settings saves.
		// The lock is released before report rendering, disk writes or delivery.
		configUpdateMu.Lock()
		opts := configuredEstateOptions()
		if !opts.WeeklyReportEnabled || !opts.WeeklyScheduleEnabled {
			configUpdateMu.Unlock()
			return
		}
		now := time.Now()
		loc := configuredDayLocation()
		window, due := estateReportDue(now, opts, loc)
		if !due {
			configUpdateMu.Unlock()
			return
		}
		history, traffic := estateHistoryCopy(), estateTrafficForWeek(window)
		configUpdateMu.Unlock()
		err := runEstateReportSchedule(now, opts, loc, history, traffic, func(report estateWeeklyReport) error {
			return estateDeliverReportIfCurrent(opts, history.Scope, loc.String(), func() error {
				return sendWebhookEventWith(opts.WeeklyWebhookURL, webhookFormatOptions{Provider: opts.WeeklyWebhookProvider}, estateWeeklyDeliveryPayload(report, configuredPublicBaseURL()))
			})
		})
		if err != nil {
			log.Printf("[ESTATE] weekly report could not be saved: %v", err)
		}
	}
	tick()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

func estateDeliverReportIfCurrent(opts EstateOptions, scope, timezone string, deliver func() error) error {
	configUpdateMu.Lock()
	current := configuredEstateOptions()
	valid := current.WeeklyReportEnabled && current.WeeklyScheduleEnabled && current.WeeklyWebhookEnabled && reflect.DeepEqual(current, opts) && estateScopeForConfig() == scope && configuredDayLocation().String() == timezone
	configUpdateMu.Unlock()
	if !valid {
		return errors.New("weekly report settings or estate changed during generation")
	}
	// Delivery is dispatched using the validated immutable report/options. Never
	// hold the collector/settings lock over a webhook request. A request already
	// dispatched may complete after a later settings change, like any in-flight IO.
	return deliver()
}

func handleEstateReportRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !configuredEstateOptions().WeeklyReportEnabled {
		http.Error(w, "Weekly Estate Reports are disabled.", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	scope := estateHistoryCopy().Scope
	if id := r.URL.Query().Get("id"); id != "" {
		path, err := estateReportRunPath(id)
		if err != nil {
			http.Error(w, "invalid report ID", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(path)
		var run estateReportRun
		if err != nil || json.Unmarshal(data, &run) != nil || run.Scope != scope {
			http.Error(w, "report not found", http.StatusNotFound)
			return
		}
		writeEstateReport(w, r, run.Report)
		return
	}
	entries, err := os.ReadDir(dataFilePath("estate-reports"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "unable to read report history", http.StatusInternalServerError)
		return
	}
	runs := []estateReportRunSummary{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path, err := estateReportRunPath(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		var run estateReportRun
		if err != nil || json.Unmarshal(data, &run) != nil || run.Scope != scope {
			continue
		}
		runs = append(runs, estateReportRunSummary{ID: run.ID, CreatedAt: run.CreatedAt, State: run.State, Week: run.Report.Window.FirstDay, Message: run.Message})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"runs": runs})
}

func handleEstateReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	configUpdateMu.Lock()
	opts := configuredEstateOptions()
	if !opts.WeeklyReportEnabled {
		configUpdateMu.Unlock()
		http.Error(w, "Weekly Estate Reports are disabled. Enable them in Settings to begin retaining report history.", http.StatusForbidden)
		return
	}
	loc := configuredDayLocation()
	week := r.URL.Query().Get("week")
	if week == "" {
		week = r.URL.Query().Get("week_start")
	}
	window, err := estateRequestedWeek(week, time.Now(), loc)
	if err != nil {
		configUpdateMu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	values := opts.WeeklySections
	if raw, ok := r.URL.Query()["sections"]; ok {
		values = strings.Split(strings.Join(raw, ","), ",")
	}
	sections, err := estateSelectedSections(values)
	if err != nil {
		configUpdateMu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	history, traffic := estateHistoryCopy(), estateTrafficForWeek(window)
	configUpdateMu.Unlock()
	report := buildEstateWeeklyReport(history, opts, window, sections, time.Now(), traffic)
	writeEstateReport(w, r, report)
}

func writeEstateReport(w http.ResponseWriter, r *http.Request, report estateWeeklyReport) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	format := r.URL.Query().Get("format")
	switch format {
	case "html", "preview":
		body, err := renderEstateReportHTML(report, r.URL.Query().Get("theme"))
		if err != nil {
			http.Error(w, "unable to render weekly report", http.StatusInternalServerError)
			return
		}
		if format == "preview" {
			// The preview and email renderer must describe exactly the same
			// captured report, even if collection advances during the request.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				Report estateWeeklyReport `json:"report"`
				HTML   string             `json:"html"`
			}{Report: report, HTML: string(body)})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="Illumio_Weekly_Estate_%s.html"`, report.Window.FirstDay))
		_, _ = w.Write(body)
	case "", "json":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	default:
		http.Error(w, "format must be json, html or preview", http.StatusBadRequest)
	}
}
