package main

import "time"

// EstateOptions is deliberately opt-in. A zero value enables no new collection,
// history, UI cards, reports, or outbound delivery.
type EstateOptions struct {
	InventoryEnabled      bool     `json:"inventory_enabled"`
	PolicyEnabled         bool     `json:"policy_enabled"`
	IssueAgingEnabled     bool     `json:"issue_aging_enabled"`
	LabelHygieneEnabled   bool     `json:"label_hygiene_enabled"`
	VENVersionsEnabled    bool     `json:"ven_versions_enabled"`
	ChangeLogEnabled      bool     `json:"change_log_enabled"`
	WeeklyReportEnabled   bool     `json:"weekly_report_enabled"`
	RequiredLabelTypes    []string `json:"required_label_types"`
	HistoryDays           int      `json:"history_days"`
	WeeklyScheduleEnabled bool     `json:"weekly_schedule_enabled"`
	WeeklyDay             string   `json:"weekly_day"`
	WeeklyTime            string   `json:"weekly_time"`
	WeeklySections        []string `json:"weekly_sections"`
	WeeklyWebhookEnabled  bool     `json:"weekly_webhook_enabled"`
	WeeklyWebhookURL      string   `json:"weekly_webhook_url,omitempty"`
	WeeklyWebhookProvider string   `json:"weekly_webhook_provider"`
}

func (o EstateOptions) Enabled() bool {
	return o.InventoryEnabled || o.PolicyEnabled || o.IssueAgingEnabled || o.LabelHygieneEnabled || o.VENVersionsEnabled || o.ChangeLogEnabled || o.WeeklyReportEnabled
}

type EstateWorkload struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Managed    bool              `json:"managed"`
	Mode       string            `json:"mode"`
	VENVersion string            `json:"ven_version"`
	Labels     map[string]string `json:"labels"`
}

type EstateVersionCount struct {
	Version string `json:"version"`
	Count   int    `json:"count"`
}

type EstateLabelGap struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Missing []string `json:"missing"`
}

type EstatePolicyCount struct {
	Rulesets int  `json:"rulesets"`
	Rules    int  `json:"rules"`
	Complete bool `json:"complete"`
}

type EstatePolicyChange struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Operation string    `json:"operation"`
	UpdatedAt time.Time `json:"updated_at,omitempty,omitzero"`
}

type EstatePolicy struct {
	At                time.Time                  `json:"at"`
	Active            EstatePolicyCount          `json:"active"`
	Draft             EstatePolicyCount          `json:"draft"`
	Pending           []EstatePolicyChange       `json:"pending"`
	PendingComplete   bool                       `json:"pending_complete"`
	PendingCounts     *EstatePendingPolicyCounts `json:"pending_counts,omitempty"`
	LastProvisionedAt time.Time                  `json:"last_provisioned_at,omitempty,omitzero"`
	Warnings          []string                   `json:"warnings"`
}

type EstateIssue struct {
	ID           string    `json:"id"`
	WorkloadID   string    `json:"workload_id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	ResolvedAt   time.Time `json:"resolved_at,omitempty,omitzero"`
	Active       bool      `json:"active"`
	Occurrences  int       `json:"occurrences"`
	ObservedDays []string  `json:"observed_days"`
}

type EstateChange struct {
	At         time.Time  `json:"at"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	WorkloadID string     `json:"workload_id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Before     string     `json:"before,omitempty"`
	After      string     `json:"after,omitempty"`
}

// At preserves the source event time when known. Cached policy observations
// may discover an earlier provisioning event after a report was saved, so
// report-relative counts and retention use the time it was actually observed.
// Older history and inventory changes have no separate observation timestamp.
func estateChangeObservedAt(change EstateChange) time.Time {
	if change.ObservedAt != nil && !change.ObservedAt.IsZero() {
		return *change.ObservedAt
	}
	return change.At
}

type EstateSnapshot struct {
	At                time.Time            `json:"at"`
	FirstAt           time.Time            `json:"first_at"`
	Day               string               `json:"day"`
	Samples           int                  `json:"samples"`
	CompleteSamples   int                  `json:"complete_samples"`
	HealthSamples     map[string]int       `json:"health_samples"`
	InventoryComplete bool                 `json:"inventory_complete"`
	Total             int                  `json:"total"`
	Managed           int                  `json:"managed"`
	Unmanaged         int                  `json:"unmanaged"`
	VENCount          int                  `json:"ven_count"`
	VersionsCollected bool                 `json:"versions_collected"`
	Modes             map[string]int       `json:"modes"`
	Versions          []EstateVersionCount `json:"versions"`
	Workloads         []EstateWorkload     `json:"workloads,omitempty"`
	LabelGaps         []EstateLabelGap     `json:"label_gaps"`
	LabelsComplete    bool                 `json:"labels_complete"`
	LabelsAssessed    int                  `json:"labels_assessed"`
	Health            map[string]int       `json:"health"`
	HealthComplete    map[string]bool      `json:"health_complete"`
	Tampered          map[string]time.Time `json:"tampered"`
	Policy            *EstatePolicy        `json:"policy,omitempty"`
	Warnings          []string             `json:"warnings"`
}

type EstateHistory struct {
	Scope      string           `json:"scope"`
	Timezone   string           `json:"timezone"`
	StartedAt  time.Time        `json:"started_at"`
	Latest     *EstateSnapshot  `json:"latest,omitempty"`
	Days       []EstateSnapshot `json:"days"`
	Issues     []EstateIssue    `json:"issues"`
	Changes    []EstateChange   `json:"changes"`
	Baseline   []EstateWorkload `json:"baseline,omitempty"`
	BaselineAt time.Time        `json:"baseline_at,omitempty,omitzero"`
	OptionsKey string           `json:"options_key,omitempty"`
}

type EstateView struct {
	Options            EstateOptions             `json:"options"`
	Snapshot           *EstateSnapshot           `json:"snapshot,omitempty"`
	Issues             []EstateIssue             `json:"issues"`
	Changes            []EstateChange            `json:"changes"`
	ChangesSinceReport *EstateChangesSinceReport `json:"changes_since_report,omitempty"`
	StartedAt          time.Time                 `json:"started_at,omitempty,omitzero"`
	Warnings           []string                  `json:"warnings"`
}

// The dashboard counter is a view of retained events, not a new history store.
// Since is a completed week's exclusive end, or an explicitly selected
// on-demand report's observation timestamp (whose events are already included).
type EstateChangesSinceReport struct {
	Status        string     `json:"status"`
	Count         int        `json:"count"`
	Since         time.Time  `json:"since,omitempty,omitzero"`
	ReportWeek    string     `json:"report_week,omitempty"`
	Partial       bool       `json:"partial"`
	Warning       string     `json:"warning,omitempty"`
	BaselineKind  string     `json:"baseline_kind,omitempty"`
	ReportID      string     `json:"report_id,omitempty"`
	ObservationAt *time.Time `json:"observation_at,omitempty"`
}
