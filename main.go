package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed index.html settings.html details.html report.html executive.html static/*
var templateFS embed.FS

const configFileName = "config.json"
const blockedHistoryFileName = "blocked_daily_history.json"
const blockedPortHistoryFileName = "blocked_port_daily_history.json"
const venHistoryFileName = "ven_daily_history.json"
const rollingStateFileName = "rolling_state.json"
const alertStateFileName = "alert_state.json"
const anomalyHistoryFileName = "anomaly_history.jsonl"
const trafficQueryMaxResults = 200000
const venQueryMaxDuration = 4 * time.Minute
const tamperingQueryMaxDuration = 4 * time.Minute
const rollingStateSchemaVersion = 1
const defaultDataDirName = ".illumio-monitoring-dashboard"
const defaultBindAddress = ":18443"
const defaultPublicBaseURL = "http://localhost:18443"

const pceTimeFormat = "2006-01-02T15:04:05.000Z"
const maxHistoryDays = 3650
const slowRequestLogThreshold = 200 * time.Millisecond

type Config struct {
	PCEURL                   string          `json:"pce_url"`
	APIKey                   string          `json:"api_key"`
	APISecret                string          `json:"api_secret"`
	OrgID                    string          `json:"org_id"`
	Timezone                 string          `json:"timezone,omitempty"`
	BindAddress              string          `json:"bind_address,omitempty"`
	PublicBaseURL            string          `json:"public_base_url,omitempty"`
	DataDir                  string          `json:"data_dir,omitempty"`
	TrafficTargets           []TrafficTarget `json:"traffic_targets,omitempty"`
	SourceExclusions         []TrafficTarget `json:"traffic_source_exclusions,omitempty"`
	HistoryDays              int             `json:"history_days,omitempty"`
	BlockedPortDailyEnabled  *bool           `json:"blocked_port_daily_enabled,omitempty"`
	BlockedMAWindow          int             `json:"blocked_ma_window,omitempty"`
	BlockedAnomalyPct        float64         `json:"blocked_anomaly_pct,omitempty"`
	BlockedAnomalyBaseline   string          `json:"blocked_anomaly_baseline,omitempty"`
	BlockedAnomalyDays       int             `json:"blocked_anomaly_days,omitempty"`
	BlockedAnomalyMinPct     float64         `json:"blocked_anomaly_min_coverage_pct,omitempty"`
	DailyMAWindow            int             `json:"daily_ma_window,omitempty"`
	VENMAWindow              int             `json:"ven_ma_window,omitempty"`
	VENAnomalyPct            float64         `json:"ven_anomaly_pct,omitempty"`
	VENAnomalyBaseline       string          `json:"ven_anomaly_baseline,omitempty"`
	VENAnomalyDays           int             `json:"ven_anomaly_days,omitempty"`
	VENAnomalyMinPct         float64         `json:"ven_anomaly_min_coverage_pct,omitempty"`
	TamperingMAWindow        int             `json:"tampering_ma_window,omitempty"`
	TamperingAnomalyPct      float64         `json:"tampering_anomaly_pct,omitempty"`
	TamperingAnomalyBaseline string          `json:"tampering_anomaly_baseline,omitempty"`
	TamperingAnomalyDays     int             `json:"tampering_anomaly_days,omitempty"`
	TamperingAnomalyMinPct   float64         `json:"tampering_anomaly_min_coverage_pct,omitempty"`
	TamperingDailyAnomalyPct float64         `json:"tampering_daily_anomaly_pct,omitempty"`
	WebhookURL               string          `json:"webhook_url,omitempty"`
	WebhookEnabled           bool            `json:"webhook_enabled,omitempty"`
	WebhookProvider          string          `json:"webhook_provider,omitempty"`
	WebhookSlackChannel      string          `json:"webhook_slack_channel,omitempty"`
	WebhookSlackUsername     string          `json:"webhook_slack_username,omitempty"`
	WebhookSlackIconEmoji    string          `json:"webhook_slack_icon_emoji,omitempty"`
	WebhookTeamsTitlePrefix  string          `json:"webhook_teams_title_prefix,omitempty"`
}

type TrafficTarget struct {
	Name              string  `json:"name"`
	Kind              string  `json:"kind"`
	BlockedMAWindow   int     `json:"blocked_ma_window,omitempty"`
	BlockedAnomalyPct float64 `json:"blocked_anomaly_pct,omitempty"`
}

type FetchStatus struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type BlockedTargetResult struct {
	Name             string      `json:"name"`
	Kind             string      `json:"kind"`
	Count            int         `json:"count"`
	Baseline24h      int         `json:"baseline_24h,omitempty"`
	IncrementalCount int         `json:"incremental_count,omitempty"`
	IncrementalMins  int         `json:"incremental_minutes,omitempty"`
	Warmup           bool        `json:"warmup"`
	Warning          string      `json:"warning,omitempty"`
	Latest5m         int         `json:"latest_5m,omitempty"`
	MovingAvg5m      float64     `json:"moving_avg_5m,omitempty"`
	Anomalous        bool        `json:"anomalous"`
	AnomalyReason    string      `json:"anomaly_reason,omitempty"`
	AnomalyWindow    int         `json:"anomaly_window,omitempty"`
	AnomalyPct       float64     `json:"anomaly_pct,omitempty"`
	AnomalySource    string      `json:"anomaly_source,omitempty"`
	AnomalyCoverage  float64     `json:"anomaly_coverage_pct,omitempty"`
	Status           FetchStatus `json:"status"`
}

type DashboardStats struct {
	VENStatus struct {
		Warning []string    `json:"warning"`
		Error   []string    `json:"error"`
		Status  FetchStatus `json:"status"`
	} `json:"ven_status"`
	Workloads struct {
		Total            int                 `json:"total"`
		Unmanaged        int                 `json:"unmanaged"`
		EnforcementModes map[string]int      `json:"enforcement_modes"`
		ModeMembers      map[string][]string `json:"mode_members,omitempty"`
		Status           FetchStatus         `json:"status"`
	} `json:"workloads"`
	Tampering struct {
		Count     int         `json:"count"`
		Workloads []string    `json:"workloads,omitempty"`
		Status    FetchStatus `json:"status"`
	} `json:"tampering"`
	Blocked struct {
		PROD          int                   `json:"prod"`
		NONPROD       int                   `json:"nonprod"`
		PRODStatus    FetchStatus           `json:"prod_status"`
		NONPRODStatus FetchStatus           `json:"nonprod_status"`
		Targets       []BlockedTargetResult `json:"targets"`
		Partial       bool                  `json:"partial"`
		Warning       string                `json:"warning,omitempty"`
		Status        FetchStatus           `json:"status"`
	} `json:"blocked"`
	Collection struct {
		Mode        string    `json:"mode"`
		WindowStart time.Time `json:"window_start"`
		WindowEnd   time.Time `json:"window_end"`
		Warmup      bool      `json:"warmup"`
	} `json:"collection"`
	Timestamp time.Time `json:"timestamp"`
}

type DrilldownResponse struct {
	Metric                  string           `json:"metric"`
	Target                  string           `json:"target,omitempty"`
	Title                   string           `json:"title"`
	Count                   int              `json:"count"`
	Items                   []string         `json:"items"`
	Trend                   []TrendPoint     `json:"trend,omitempty"`
	Trend24h                []TrendPoint     `json:"trend_24h,omitempty"`
	TrendDaily              []TrendPoint     `json:"trend_daily,omitempty"`
	TrendMA24h              []TrendPointF    `json:"trend_ma_24h,omitempty"`
	TrendMADaily            []TrendPointF    `json:"trend_ma_daily,omitempty"`
	BlockedMAWindow         int              `json:"blocked_ma_window,omitempty"`
	BlockedAnomalyPct       float64          `json:"blocked_anomaly_pct,omitempty"`
	Anomalous               bool             `json:"anomalous,omitempty"`
	AnomalyReason           string           `json:"anomaly_reason,omitempty"`
	AnomalyWindow           int              `json:"anomaly_window,omitempty"`
	AnomalyPct              float64          `json:"anomaly_pct,omitempty"`
	AnomalySource           string           `json:"anomaly_source,omitempty"`
	AnomalyCoveragePct      float64          `json:"anomaly_coverage_pct,omitempty"`
	LatestValue             int              `json:"latest_value,omitempty"`
	MovingAvgValue          float64          `json:"moving_avg_value,omitempty"`
	Baseline24h             int              `json:"baseline_24h,omitempty"`
	BaselineCapturedUTC     *time.Time       `json:"baseline_captured_utc,omitempty"`
	BlockedPortsDaily       []BlockedPortDay `json:"blocked_ports_daily,omitempty"`
	BlockedPortDailyEnabled bool             `json:"blocked_port_daily_enabled"`
}

type TrendPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     int       `json:"value"`
}

type TrendPointF struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

type PortCount struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type BlockedPortDay struct {
	Timestamp time.Time   `json:"timestamp"`
	Ports     []PortCount `json:"ports,omitempty"`
}

type rollingBucket struct {
	EndUTC             time.Time
	VENWarningCount    int
	VENErrorCount      int
	ModeIdleCount      int
	ModeVisCount       int
	ModeSelectiveCount int
	ModeFullCount      int
	ModeUnmanagedCount int
	TamperingCount     int
	TamperingWorkloads map[string]struct{}
	BlockedByTarget    map[string]int
}

type rollingState struct {
	Initialized bool
	LastCycle   time.Time

	BaselineCapturedUTC time.Time
	BaselineTampering   int
	BaselineWorkloads   map[string]struct{}
	BaselineBlocked     map[string]targetBaseline

	Buckets []rollingBucket
}

type persistedTargetBaseline struct {
	Count       int       `json:"count"`
	CapturedUTC time.Time `json:"captured_utc"`
}

type persistedRollingBucket struct {
	EndUTC             time.Time      `json:"end_utc"`
	VENWarningCount    int            `json:"ven_warning_count"`
	VENErrorCount      int            `json:"ven_error_count"`
	ModeIdleCount      int            `json:"mode_idle_count"`
	ModeVisCount       int            `json:"mode_vis_count"`
	ModeSelectiveCount int            `json:"mode_selective_count"`
	ModeFullCount      int            `json:"mode_full_count"`
	ModeUnmanagedCount int            `json:"mode_unmanaged_count"`
	TamperingCount     int            `json:"tampering_count"`
	TamperingWorkloads []string       `json:"tampering_workloads,omitempty"`
	BlockedByTarget    map[string]int `json:"blocked_by_target,omitempty"`
}

type persistedRollingState struct {
	SchemaVersion       int                                `json:"schema_version"`
	Initialized         bool                               `json:"initialized"`
	LastCycle           time.Time                          `json:"last_cycle"`
	BaselineCapturedUTC time.Time                          `json:"baseline_captured_utc"`
	BaselineTampering   int                                `json:"baseline_tampering"`
	BaselineWorkloads   []string                           `json:"baseline_workloads,omitempty"`
	BaselineBlocked     map[string]persistedTargetBaseline `json:"baseline_blocked,omitempty"`
	Buckets             []persistedRollingBucket           `json:"buckets,omitempty"`
}

type targetBaseline struct {
	Count       int
	CapturedUTC time.Time
}

type trafficQueryResult struct {
	Count     int
	Truncated bool
	Warning   string
}

type dailyBlockedRecord struct {
	Day    string `json:"day"`
	Target string `json:"target"`
	Count  int    `json:"count"`
}

type dailyBlockedPortRecord struct {
	Day    string `json:"day"`
	Target string `json:"target"`
	Port   string `json:"port"`
	Count  int    `json:"count"`
}

type venDailySnapshot struct {
	WarningMax       int `json:"warning_max"`
	ErrorMax         int `json:"error_max"`
	TamperingMax     int `json:"tampering_max,omitempty"`
	ModeIdleMax      int `json:"mode_idle_max,omitempty"`
	ModeVisMax       int `json:"mode_vis_max,omitempty"`
	ModeSelectiveMax int `json:"mode_selective_max,omitempty"`
	ModeFullMax      int `json:"mode_full_max,omitempty"`
	ModeUnmanagedMax int `json:"mode_unmanaged_max,omitempty"`
}

type venDailyRecord struct {
	Day              string `json:"day"`
	WarningMax       int    `json:"warning_max"`
	ErrorMax         int    `json:"error_max"`
	TamperingMax     int    `json:"tampering_max,omitempty"`
	ModeIdleMax      int    `json:"mode_idle_max,omitempty"`
	ModeVisMax       int    `json:"mode_vis_max,omitempty"`
	ModeSelectiveMax int    `json:"mode_selective_max,omitempty"`
	ModeFullMax      int    `json:"mode_full_max,omitempty"`
	ModeUnmanagedMax int    `json:"mode_unmanaged_max,omitempty"`
}

type alertTargetState struct {
	Active        bool      `json:"active"`
	LastEventUTC  time.Time `json:"last_event_utc,omitempty"`
	LastEventType string    `json:"last_event_type,omitempty"`
}

type persistedAlertState struct {
	SchemaVersion int                         `json:"schema_version"`
	Targets       map[string]alertTargetState `json:"targets,omitempty"`
}

type anomalyHistoryEvent struct {
	Timestamp           time.Time `json:"timestamp"`
	Event               string    `json:"event"`
	State               string    `json:"state"`
	Metric              string    `json:"metric"`
	TargetName          string    `json:"target_name,omitempty"`
	TargetKind          string    `json:"target_kind,omitempty"`
	Latest5m            int       `json:"latest_5m,omitempty"`
	MovingAvg5m         float64   `json:"moving_avg_5m,omitempty"`
	AnomalyThresholdPct float64   `json:"anomaly_threshold_pct,omitempty"`
	MAWindowPoints      int       `json:"ma_window_points,omitempty"`
	AnomalySource       string    `json:"anomaly_source,omitempty"`
	AnomalyCoveragePct  float64   `json:"anomaly_coverage_pct,omitempty"`
	Reason              string    `json:"reason,omitempty"`
}

var (
	config       Config
	configMutex  sync.RWMutex
	currentStats DashboardStats
	statsMutex   sync.RWMutex
	isRefreshing atomic.Bool

	rollingMu         sync.Mutex
	rollingCache      rollingState
	historyMu         sync.Mutex
	blockedDaily      = map[string]map[string]int{}
	blockedPortsDaily = map[string]map[string]map[string]int{}
	venHistoryMu      sync.Mutex
	venDaily          = map[string]venDailySnapshot{}
	alertMu           sync.Mutex
	alertState        = persistedAlertState{SchemaVersion: 1, Targets: map[string]alertTargetState{}}
	anomalyHistoryMu  sync.Mutex
	anomalyHistory    = make([]anomalyHistoryEvent, 0)

	httpClient        = &http.Client{Timeout: 60 * time.Second}
	dashboardTmpl     = template.Must(template.ParseFS(templateFS, "index.html"))
	settingsTmpl      = template.Must(template.ParseFS(templateFS, "settings.html"))
	detailsPageTmpl   = template.Must(template.ParseFS(templateFS, "details.html"))
	reportPageTmpl    = template.Must(template.ParseFS(templateFS, "report.html"))
	executivePageTmpl = template.Must(template.ParseFS(templateFS, "executive.html"))
	dataDir           string
)

func main() {
	if !loadConfig() {
		promptConfig()
	} else {
		fmt.Printf("Loaded configuration for PCE: %s\n", config.PCEURL)
	}
	initDataDir()
	loadBlockedHistory()
	loadBlockedPortHistory()
	loadVENHistory()
	loadRollingState()
	loadAlertState()
	loadAnomalyHistory()

	initPendingStats()
	go backgroundCollector()

	http.HandleFunc("/", withRequestTiming("dashboard", serveDashboard))
	http.HandleFunc("/settings", withRequestTiming("settings", serveSettings))
	http.HandleFunc("/details", withRequestTiming("details", serveDetails))
	http.HandleFunc("/report", withRequestTiming("report", serveReport))
	http.HandleFunc("/trends", withRequestTiming("trends", serveTrends))
	http.HandleFunc("/executive", withRequestTiming("executive", serveExecutive))
	staticFS, err := fs.Sub(templateFS, "static")
	if err == nil {
		http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	}
	http.HandleFunc("/api/stats", withRequestTiming("api.stats", handleStats))
	http.HandleFunc("/api/drilldown", withRequestTiming("api.drilldown", handleDrilldown))
	http.HandleFunc("/api/export/drilldown.csv", withRequestTiming("api.export.drilldown_csv", handleExportDrilldownCSV))
	http.HandleFunc("/api/export/report.csv", withRequestTiming("api.export.report_csv", handleExportReportCSV))
	http.HandleFunc("/api/debug/ven-status", withRequestTiming("api.debug.ven_status", handleDebugVENStatus))
	http.HandleFunc("/api/config/targets", withRequestTiming("api.config.targets", handleConfigTargets))
	http.HandleFunc("/api/config/alerts", withRequestTiming("api.config.alerts", handleConfigAlerts))
	http.HandleFunc("/api/refresh", withRequestTiming("api.refresh", handleRefreshNow))
	http.HandleFunc("/api/webhook/test", withRequestTiming("api.webhook.test", handleWebhookTest))
	http.HandleFunc("/api/anomalies/history", withRequestTiming("api.anomalies.history", handleAnomalyHistory))

	listenAddr := configuredBindAddress()
	publicURL := configuredPublicBaseURL()
	fmt.Printf("\nDashboard starting. Bind: %s | Public URL: %s\n", listenAddr, publicURL)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}

func initPendingStats() {
	stats := DashboardStats{Timestamp: time.Now()}
	stats.Workloads.EnforcementModes = make(map[string]int)
	stats.Workloads.ModeMembers = make(map[string][]string)
	stats.Blocked.Targets = make([]BlockedTargetResult, 0)
	pending := FetchStatus{Success: false, Error: "Initial collection in progress"}
	stats.VENStatus.Status = pending
	stats.Workloads.Status = pending
	stats.Tampering.Status = pending
	stats.Blocked.Status = pending
	stats.Blocked.PRODStatus = pending
	stats.Blocked.NONPRODStatus = pending
	stats.Collection.Mode = "initializing"

	statsMutex.Lock()
	currentStats = stats
	statsMutex.Unlock()
}

func backgroundCollector() {
	for {
		runCollectionCycle()
		log.Println("[COLLECTOR] Waiting 5 minutes.")
		time.Sleep(5 * time.Minute)
	}
}

func runCollectionCycle() {
	isRefreshing.Store(true)
	log.Println("[COLLECTOR] Starting PCE data collection cycle...")

	newStats := getIllumioStats()

	statsMutex.Lock()
	currentStats = newStats
	statsMutex.Unlock()
	saveRollingState()
	processWebhookAlerts(newStats)

	isRefreshing.Store(false)
	log.Println("[COLLECTOR] Cycle complete.")
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	statsMutex.RLock()
	snapshot := currentStats
	statsMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	if isRefreshing.Load() {
		w.Header().Set("X-Refreshing", "true")
	}
	if err := json.NewEncoder(w).Encode(snapshot); err != nil {
		log.Printf("[API] failed to encode /api/stats response: %v", err)
	}
}

func handleDrilldown(w http.ResponseWriter, r *http.Request) {
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	if metric == "" {
		http.Error(w, "missing metric", http.StatusBadRequest)
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	includePorts := parseBoolQuery(r.URL.Query().Get("include_ports"))
	includeLivePorts := parseBoolQuery(r.URL.Query().Get("include_live_ports"))

	statsMutex.RLock()
	snapshot := currentStats
	statsMutex.RUnlock()

	title, items, trend := drilldownData(metric, target, snapshot)
	if title == "" {
		http.Error(w, "unknown metric", http.StatusBadRequest)
		return
	}
	sort.Strings(items)

	resp := DrilldownResponse{Metric: metric, Target: target, Title: title, Count: len(items), Items: items, Trend: trend}
	if metric == "ven_warning" {
		window := configuredVENMAWindow()
		dailyWindow := configuredDailyMAWindow()
		pct := configuredVENAnomalyPct()
		baselineSource := configuredVENAnomalyBaselineSource()
		baselineDays := configuredVENAnomalyBaselineDays()
		minCoverage := configuredVENAnomalyMinCoveragePct()
		resp.Trend24h = venTrendSeries("warning")
		resp.TrendDaily = venDailyTrendSeries("warning", configuredHistoryDays())
		resp.AnomalySource = baselineSource
		eval := blockedAnomalyFromConfig(resp.Trend24h, resp.TrendDaily, window, pct, baselineSource, baselineDays, minCoverage)
		resp.Anomalous = eval.Anomalous
		resp.AnomalyReason = eval.Reason
		resp.AnomalyWindow = eval.Window
		resp.AnomalyPct = pct
		resp.AnomalyCoveragePct = eval.CoveragePct
		resp.LatestValue = eval.Latest
		resp.MovingAvgValue = eval.Baseline
		resp.BlockedMAWindow = window
		resp.BlockedAnomalyPct = pct
		if baselineSource == "daily" {
			resp.TrendMA24h = flatTrendLine(resp.Trend24h, eval.Baseline)
			resp.TrendMADaily = movingAverageTrend(resp.TrendDaily, dailyWindow)
		} else {
			resp.TrendMA24h = movingAverageTrend(resp.Trend24h, window)
			resp.TrendMADaily = movingAverageTrend(resp.TrendDaily, dailyWindow)
		}
		resp.Trend = resp.Trend24h
	}
	if metric == "ven_error" {
		window := configuredVENMAWindow()
		dailyWindow := configuredDailyMAWindow()
		pct := configuredVENAnomalyPct()
		baselineSource := configuredVENAnomalyBaselineSource()
		baselineDays := configuredVENAnomalyBaselineDays()
		minCoverage := configuredVENAnomalyMinCoveragePct()
		resp.Trend24h = venTrendSeries("error")
		resp.TrendDaily = venDailyTrendSeries("error", configuredHistoryDays())
		resp.AnomalySource = baselineSource
		eval := blockedAnomalyFromConfig(resp.Trend24h, resp.TrendDaily, window, pct, baselineSource, baselineDays, minCoverage)
		resp.Anomalous = eval.Anomalous
		resp.AnomalyReason = eval.Reason
		resp.AnomalyWindow = eval.Window
		resp.AnomalyPct = pct
		resp.AnomalyCoveragePct = eval.CoveragePct
		resp.LatestValue = eval.Latest
		resp.MovingAvgValue = eval.Baseline
		resp.BlockedMAWindow = window
		resp.BlockedAnomalyPct = pct
		if baselineSource == "daily" {
			resp.TrendMA24h = flatTrendLine(resp.Trend24h, eval.Baseline)
			resp.TrendMADaily = movingAverageTrend(resp.TrendDaily, dailyWindow)
		} else {
			resp.TrendMA24h = movingAverageTrend(resp.Trend24h, window)
			resp.TrendMADaily = movingAverageTrend(resp.TrendDaily, dailyWindow)
		}
		resp.Trend = resp.Trend24h
	}
	if metric == "tampering" {
		window := configuredTamperingMAWindow()
		dailyWindow := configuredDailyMAWindow()
		pct := configuredTamperingAnomalyPct()
		baselineSource := configuredTamperingAnomalyBaselineSource()
		baselineDays := configuredTamperingAnomalyBaselineDays()
		minCoverage := configuredTamperingAnomalyMinCoveragePct()
		if baselineSource == "daily" {
			pct = configuredTamperingDailyAnomalyPct()
		}
		resp.Trend24h = tamperingTrendSeries()
		resp.TrendDaily = tamperingDailyTrendSeries(configuredHistoryDays())
		resp.AnomalySource = baselineSource
		eval := blockedAnomalyFromConfig(resp.Trend24h, resp.TrendDaily, window, pct, baselineSource, baselineDays, minCoverage)
		resp.Anomalous = eval.Anomalous
		resp.AnomalyReason = eval.Reason
		resp.AnomalyWindow = eval.Window
		resp.AnomalyCoveragePct = eval.CoveragePct
		resp.LatestValue = eval.Latest
		resp.MovingAvgValue = eval.Baseline
		resp.BlockedMAWindow = window
		resp.BlockedAnomalyPct = pct
		if baselineSource == "daily" {
			resp.TrendMA24h = flatTrendLine(resp.Trend24h, eval.Baseline)
			resp.TrendMADaily = movingAverageTrend(resp.TrendDaily, dailyWindow)
		} else {
			resp.TrendMA24h = movingAverageTrend(resp.Trend24h, window)
			resp.TrendMADaily = movingAverageTrend(resp.TrendDaily, dailyWindow)
		}
		resp.Trend = resp.Trend24h
	}
	if isEnforcementMetric(metric) {
		mode := enforcementModeFromMetric(metric)
		resp.Trend24h = modeTrendSeries(mode)
		resp.TrendDaily = modeDailyTrendSeries(mode, configuredHistoryDays())
		resp.Trend = resp.Trend24h
	}
	if metric == "blocked_target" && target != "" {
		window := configuredBlockedMAWindow()
		pct := configuredBlockedAnomalyPct()
		baselineSource := configuredBlockedAnomalyBaselineSource()
		baselineDays := configuredBlockedAnomalyBaselineDays()
		minCoverage := configuredBlockedAnomalyMinCoveragePct()
		if tt, ok := configuredTrafficTargetByName(target); ok {
			window, pct = effectiveBlockedAnomalySettingsForTarget(tt, window, pct)
		}
		resp.Trend24h = blockedTrendSeries(target)
		resp.TrendDaily = blockedDailyTrendSeries(target, configuredHistoryDays())
		resp.BlockedPortDailyEnabled = configuredBlockedPortDailyEnabled()
		if resp.BlockedPortDailyEnabled && includePorts {
			resp.BlockedPortsDaily = blockedPortDailySeries(target, configuredHistoryDays())
			if includeLivePorts {
				configMutex.RLock()
				pceURL := config.PCEURL
				orgID := config.OrgID
				configMutex.RUnlock()
				baseURL := fmt.Sprintf("%s/api/v2/orgs/%s", strings.TrimSuffix(pceURL, "/"), orgID)
				excludedHRefs, exclusionWarn := resolveSourceExclusionHRefs(baseURL, configuredSourceExclusions())
				if exclusionWarn != "" {
					log.Printf("[DRILLDOWN] source exclusion warning: %s", exclusionWarn)
				}
				loc := configuredDayLocation()
				now := time.Now()
				todayStart := localDayStart(now, loc)
				targetCfg, ok := configuredTrafficTargetByName(target)
				if !ok {
					targetCfg = TrafficTarget{Name: target, Kind: "auto"}
				}
				todayPorts, err := getBlockedPortCountsForTargetWindow(baseURL, targetCfg, todayStart.UTC(), now.UTC(), excludedHRefs)
				if err != nil {
					log.Printf("[DRILLDOWN] blocked ports today-so-far query failed for %s: %v", target, err)
				} else if len(todayPorts) > 0 {
					resp.BlockedPortsDaily = append(resp.BlockedPortsDaily, BlockedPortDay{
						Timestamp: now.UTC(),
						Ports:     portCountMapToSortedSlice(todayPorts),
					})
				}
			}
		}
		resp.BlockedMAWindow = window
		resp.BlockedAnomalyPct = pct
		resp.AnomalySource = baselineSource
		eval := blockedAnomalyFromConfig(resp.Trend24h, resp.TrendDaily, window, pct, baselineSource, baselineDays, minCoverage)
		resp.Anomalous = eval.Anomalous
		resp.AnomalyReason = eval.Reason
		resp.AnomalyWindow = eval.Window
		resp.AnomalyPct = pct
		resp.AnomalyCoveragePct = eval.CoveragePct
		resp.LatestValue = eval.Latest
		resp.MovingAvgValue = eval.Baseline
		if baselineSource == "daily" {
			resp.TrendMA24h = flatTrendLine(resp.Trend24h, eval.Baseline)
			resp.TrendMADaily = movingAverageTrend(resp.TrendDaily, configuredDailyMAWindow())
		} else {
			resp.TrendMA24h = movingAverageTrend(resp.Trend24h, window)
			resp.TrendMADaily = movingAverageTrend(resp.TrendDaily, configuredDailyMAWindow())
		}
		resp.Trend = resp.Trend24h
		if baseline, capturedUTC, ok := blockedTargetBaseline(target); ok {
			resp.Baseline24h = baseline
			resp.BaselineCapturedUTC = capturedUTC
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[API] failed to encode /api/drilldown response: %v", err)
	}
}

func handleExportDrilldownCSV(w http.ResponseWriter, r *http.Request) {
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	if metric == "" {
		http.Error(w, "missing metric", http.StatusBadRequest)
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))

	statsMutex.RLock()
	snapshot := currentStats
	statsMutex.RUnlock()

	title, items, trend := drilldownData(metric, target, snapshot)
	if title == "" {
		http.Error(w, "unknown metric", http.StatusBadRequest)
		return
	}
	sort.Strings(items)

	trend24h := []TrendPoint{}
	trendDaily := []TrendPoint{}
	if metric == "ven_warning" {
		trend24h = venTrendSeries("warning")
		trendDaily = venDailyTrendSeries("warning", configuredHistoryDays())
	} else if metric == "ven_error" {
		trend24h = venTrendSeries("error")
		trendDaily = venDailyTrendSeries("error", configuredHistoryDays())
	} else if isEnforcementMetric(metric) {
		mode := enforcementModeFromMetric(metric)
		trend24h = modeTrendSeries(mode)
		trendDaily = modeDailyTrendSeries(mode, configuredHistoryDays())
	} else if metric == "blocked_target" {
		trend24h = blockedTrendSeries(target)
		trendDaily = blockedDailyTrendSeries(target, configuredHistoryDays())
	} else if metric == "tampering" {
		trend24h = tamperingTrendSeries()
		trendDaily = tamperingDailyTrendSeries(configuredHistoryDays())
	} else {
		trend24h = trend
	}

	fileTS := time.Now().UTC().Format("20060102-150405")
	metricSafe := strings.ReplaceAll(strings.ToLower(metric), " ", "_")
	if target != "" {
		metricSafe += "_" + strings.ReplaceAll(strings.ToLower(target), " ", "_")
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"illumio-drilldown-%s-%s.csv\"", metricSafe, fileTS))
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	write := func(fields ...string) bool {
		if err := cw.Write(fields); err != nil {
			log.Printf("[API] failed writing drilldown csv row: %v", err)
			return false
		}
		return true
	}
	if !write("Illumio Drilldown Export") {
		return
	}
	if !write("Generated UTC", time.Now().UTC().Format(time.RFC3339)) {
		return
	}
	if !write("Metric", metric) {
		return
	}
	if target != "" && !write("Target", target) {
		return
	}
	if !write("Title", title) {
		return
	}
	if !write("Item Count", strconv.Itoa(len(items))) {
		return
	}
	if !write() {
		return
	}
	if !write("Item") {
		return
	}
	for _, it := range items {
		if !write(it) {
			return
		}
	}
	if !write() {
		return
	}
	if !write("Trend", "Timestamp UTC", "Value") {
		return
	}
	for _, p := range trend24h {
		if !write("24h (5m)", p.Timestamp.UTC().Format(time.RFC3339), strconv.Itoa(p.Value)) {
			return
		}
	}
	for _, p := range trendDaily {
		if !write("Daily", p.Timestamp.UTC().Format(time.RFC3339), strconv.Itoa(p.Value)) {
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Printf("[API] failed to flush drilldown csv export: %v", err)
	}
}

func handleConfigTargets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMutex.RLock()
		targets := append([]TrafficTarget(nil), config.TrafficTargets...)
		exclusions := append([]TrafficTarget(nil), config.SourceExclusions...)
		historyDays := configuredHistoryDaysLocked()
		blockedPortDailyEnabled := configuredBlockedPortDailyEnabledLocked()
		maWindow := configuredBlockedMAWindowLocked()
		dailyMAWindow := configuredDailyMAWindowLocked()
		anomalyPct := configuredBlockedAnomalyPctLocked()
		anomalyBaseline := configuredBlockedAnomalyBaselineSourceLocked()
		anomalyDays := configuredBlockedAnomalyBaselineDaysLocked()
		anomalyMinCoverage := configuredBlockedAnomalyMinCoveragePctLocked()
		venMAWindow := configuredVENMAWindowLocked()
		venAnomalyPct := configuredVENAnomalyPctLocked()
		venAnomalyBaseline := configuredVENAnomalyBaselineSourceLocked()
		venAnomalyDays := configuredVENAnomalyBaselineDaysLocked()
		venAnomalyMinCoverage := configuredVENAnomalyMinCoveragePctLocked()
		tamperMAWindow := configuredTamperingMAWindowLocked()
		tamperAnomalyPct := configuredTamperingAnomalyPctLocked()
		tamperAnomalyBaseline := configuredTamperingAnomalyBaselineSourceLocked()
		tamperAnomalyDays := configuredTamperingAnomalyBaselineDaysLocked()
		tamperAnomalyMinCoverage := configuredTamperingAnomalyMinCoveragePctLocked()
		tamperDailyAnomalyPct := configuredTamperingDailyAnomalyPctLocked()
		timezone := configuredTimezoneLocked()
		effectiveTimezone := configuredEffectiveTimezoneLocked()
		bindAddress := configuredBindAddressLocked()
		publicBaseURL := configuredPublicBaseURLLocked()
		webhookURL := strings.TrimSpace(config.WebhookURL)
		webhookEnabled := config.WebhookEnabled && webhookURL != ""
		webhookProvider := configuredWebhookProviderLocked()
		slackChannel := strings.TrimSpace(config.WebhookSlackChannel)
		slackUsername := strings.TrimSpace(config.WebhookSlackUsername)
		slackIconEmoji := strings.TrimSpace(config.WebhookSlackIconEmoji)
		teamsTitlePrefix := strings.TrimSpace(config.WebhookTeamsTitlePrefix)
		configMutex.RUnlock()
		if len(targets) == 0 {
			targets = configuredTrafficTargets()
		}
		if len(exclusions) == 0 {
			exclusions = configuredSourceExclusions()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"traffic_targets":             targets,
			"traffic_source_exclusions":   exclusions,
			"history_days":                historyDays,
			"blocked_port_daily_enabled":  blockedPortDailyEnabled,
			"blocked_ma_window":           maWindow,
			"daily_ma_window":             dailyMAWindow,
			"blocked_anomaly_pct":         anomalyPct,
			"blocked_anomaly_baseline":    anomalyBaseline,
			"blocked_anomaly_days":        anomalyDays,
			"blocked_anomaly_min_pct":     anomalyMinCoverage,
			"ven_ma_window":               venMAWindow,
			"ven_anomaly_pct":             venAnomalyPct,
			"ven_anomaly_baseline":        venAnomalyBaseline,
			"ven_anomaly_days":            venAnomalyDays,
			"ven_anomaly_min_pct":         venAnomalyMinCoverage,
			"tampering_ma_window":         tamperMAWindow,
			"tampering_anomaly_pct":       tamperAnomalyPct,
			"tampering_anomaly_baseline":  tamperAnomalyBaseline,
			"tampering_anomaly_days":      tamperAnomalyDays,
			"tampering_anomaly_min_pct":   tamperAnomalyMinCoverage,
			"tampering_daily_anomaly_pct": tamperDailyAnomalyPct,
			"timezone":                    timezone,
			"timezone_effective":          effectiveTimezone,
			"bind_address":                bindAddress,
			"public_base_url":             publicBaseURL,
			"webhook_url":                 webhookURL,
			"webhook_enabled":             webhookEnabled,
			"webhook_provider":            webhookProvider,
			"webhook_slack_channel":       slackChannel,
			"webhook_slack_username":      slackUsername,
			"webhook_slack_icon_emoji":    slackIconEmoji,
			"webhook_teams_title_prefix":  teamsTitlePrefix,
		})
	case http.MethodPut:
		var req struct {
			TrafficTargets           []TrafficTarget `json:"traffic_targets"`
			SourceExclusions         []TrafficTarget `json:"traffic_source_exclusions"`
			HistoryDays              int             `json:"history_days"`
			BlockedPortDailyEnabled  *bool           `json:"blocked_port_daily_enabled"`
			BlockedMAWindow          int             `json:"blocked_ma_window"`
			DailyMAWindow            int             `json:"daily_ma_window"`
			BlockedAnomalyPct        float64         `json:"blocked_anomaly_pct"`
			BlockedAnomalyBaseline   *string         `json:"blocked_anomaly_baseline"`
			BlockedAnomalyDays       int             `json:"blocked_anomaly_days"`
			BlockedAnomalyMinPct     float64         `json:"blocked_anomaly_min_pct"`
			VENMAWindow              int             `json:"ven_ma_window"`
			VENAnomalyPct            float64         `json:"ven_anomaly_pct"`
			VENAnomalyBaseline       *string         `json:"ven_anomaly_baseline"`
			VENAnomalyDays           int             `json:"ven_anomaly_days"`
			VENAnomalyMinPct         float64         `json:"ven_anomaly_min_pct"`
			TamperingMAWindow        int             `json:"tampering_ma_window"`
			TamperingAnomalyPct      float64         `json:"tampering_anomaly_pct"`
			TamperingAnomalyBaseline *string         `json:"tampering_anomaly_baseline"`
			TamperingAnomalyDays     int             `json:"tampering_anomaly_days"`
			TamperingAnomalyMinPct   float64         `json:"tampering_anomaly_min_pct"`
			TamperingDailyAnomalyPct float64         `json:"tampering_daily_anomaly_pct"`
			Timezone                 *string         `json:"timezone"`
			BindAddress              *string         `json:"bind_address"`
			PublicBaseURL            *string         `json:"public_base_url"`
			WebhookURL               *string         `json:"webhook_url"`
			WebhookEnabled           *bool           `json:"webhook_enabled"`
			WebhookProvider          *string         `json:"webhook_provider"`
			WebhookSlackChannel      *string         `json:"webhook_slack_channel"`
			WebhookSlackUsername     *string         `json:"webhook_slack_username"`
			WebhookSlackIconEmoji    *string         `json:"webhook_slack_icon_emoji"`
			WebhookTeamsTitlePrefix  *string         `json:"webhook_teams_title_prefix"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		cleaned := sanitizeTargets(req.TrafficTargets)
		if len(cleaned) == 0 {
			http.Error(w, "at least one target is required", http.StatusBadRequest)
			return
		}
		configMutex.Lock()
		config.TrafficTargets = cleaned
		config.SourceExclusions = sanitizeTargets(req.SourceExclusions)
		if req.HistoryDays > 0 {
			if req.HistoryDays > maxHistoryDays {
				req.HistoryDays = maxHistoryDays
			}
			config.HistoryDays = req.HistoryDays
		}
		if req.BlockedPortDailyEnabled != nil {
			v := *req.BlockedPortDailyEnabled
			config.BlockedPortDailyEnabled = &v
		}
		if req.BlockedMAWindow > 0 {
			config.BlockedMAWindow = req.BlockedMAWindow
		}
		if req.DailyMAWindow > 0 {
			config.DailyMAWindow = req.DailyMAWindow
		}
		if req.BlockedAnomalyPct > 0 {
			config.BlockedAnomalyPct = req.BlockedAnomalyPct
		}
		if req.BlockedAnomalyBaseline != nil {
			config.BlockedAnomalyBaseline = normalizeAnomalyBaselineSource(*req.BlockedAnomalyBaseline)
		}
		if req.BlockedAnomalyDays > 0 {
			config.BlockedAnomalyDays = normalizeAnomalyBaselineDays(req.BlockedAnomalyDays, configuredBlockedAnomalyBaselineDaysLocked())
		}
		if req.BlockedAnomalyMinPct > 0 {
			config.BlockedAnomalyMinPct = normalizeCoveragePct(req.BlockedAnomalyMinPct, configuredBlockedAnomalyMinCoveragePctLocked())
		}
		if req.VENMAWindow > 0 {
			config.VENMAWindow = req.VENMAWindow
		}
		if req.VENAnomalyPct > 0 {
			config.VENAnomalyPct = req.VENAnomalyPct
		}
		if req.VENAnomalyBaseline != nil {
			config.VENAnomalyBaseline = normalizeAnomalyBaselineSource(*req.VENAnomalyBaseline)
		}
		if req.VENAnomalyDays > 0 {
			config.VENAnomalyDays = normalizeAnomalyBaselineDays(req.VENAnomalyDays, configuredVENAnomalyBaselineDaysLocked())
		}
		if req.VENAnomalyMinPct > 0 {
			config.VENAnomalyMinPct = normalizeCoveragePct(req.VENAnomalyMinPct, configuredVENAnomalyMinCoveragePctLocked())
		}
		if req.TamperingMAWindow > 0 {
			config.TamperingMAWindow = req.TamperingMAWindow
		}
		if req.TamperingAnomalyPct > 0 {
			config.TamperingAnomalyPct = req.TamperingAnomalyPct
		}
		if req.TamperingAnomalyBaseline != nil {
			config.TamperingAnomalyBaseline = normalizeAnomalyBaselineSource(*req.TamperingAnomalyBaseline)
		}
		if req.TamperingAnomalyDays > 0 {
			config.TamperingAnomalyDays = normalizeAnomalyBaselineDays(req.TamperingAnomalyDays, configuredTamperingAnomalyBaselineDaysLocked())
		}
		if req.TamperingAnomalyMinPct > 0 {
			config.TamperingAnomalyMinPct = normalizeCoveragePct(req.TamperingAnomalyMinPct, configuredTamperingAnomalyMinCoveragePctLocked())
		}
		if req.TamperingDailyAnomalyPct > 0 {
			config.TamperingDailyAnomalyPct = req.TamperingDailyAnomalyPct
		}
		if req.Timezone != nil {
			config.Timezone = normalizeTimezone(*req.Timezone)
		}
		if req.BindAddress != nil {
			config.BindAddress = normalizeBindAddress(*req.BindAddress)
		}
		if req.PublicBaseURL != nil {
			config.PublicBaseURL = normalizePublicBaseURL(*req.PublicBaseURL)
		}
		if req.WebhookURL != nil {
			config.WebhookURL = strings.TrimSpace(*req.WebhookURL)
		}
		if req.WebhookEnabled != nil {
			config.WebhookEnabled = *req.WebhookEnabled && strings.TrimSpace(config.WebhookURL) != ""
		}
		if req.WebhookProvider != nil {
			config.WebhookProvider = normalizeWebhookProvider(*req.WebhookProvider)
		}
		if req.WebhookSlackChannel != nil {
			config.WebhookSlackChannel = strings.TrimSpace(*req.WebhookSlackChannel)
		}
		if req.WebhookSlackUsername != nil {
			config.WebhookSlackUsername = strings.TrimSpace(*req.WebhookSlackUsername)
		}
		if req.WebhookSlackIconEmoji != nil {
			config.WebhookSlackIconEmoji = strings.TrimSpace(*req.WebhookSlackIconEmoji)
		}
		if req.WebhookTeamsTitlePrefix != nil {
			config.WebhookTeamsTitlePrefix = strings.TrimSpace(*req.WebhookTeamsTitlePrefix)
		}
		historyDays := configuredHistoryDaysLocked()
		blockedPortDailyEnabled := configuredBlockedPortDailyEnabledLocked()
		maWindow := configuredBlockedMAWindowLocked()
		dailyMAWindow := configuredDailyMAWindowLocked()
		anomalyPct := configuredBlockedAnomalyPctLocked()
		anomalyBaseline := configuredBlockedAnomalyBaselineSourceLocked()
		anomalyDays := configuredBlockedAnomalyBaselineDaysLocked()
		anomalyMinCoverage := configuredBlockedAnomalyMinCoveragePctLocked()
		venMAWindow := configuredVENMAWindowLocked()
		venAnomalyPct := configuredVENAnomalyPctLocked()
		venAnomalyBaseline := configuredVENAnomalyBaselineSourceLocked()
		venAnomalyDays := configuredVENAnomalyBaselineDaysLocked()
		venAnomalyMinCoverage := configuredVENAnomalyMinCoveragePctLocked()
		tamperMAWindow := configuredTamperingMAWindowLocked()
		tamperAnomalyPct := configuredTamperingAnomalyPctLocked()
		tamperAnomalyBaseline := configuredTamperingAnomalyBaselineSourceLocked()
		tamperAnomalyDays := configuredTamperingAnomalyBaselineDaysLocked()
		tamperAnomalyMinCoverage := configuredTamperingAnomalyMinCoveragePctLocked()
		tamperDailyAnomalyPct := configuredTamperingDailyAnomalyPctLocked()
		timezone := configuredTimezoneLocked()
		effectiveTimezone := configuredEffectiveTimezoneLocked()
		bindAddress := configuredBindAddressLocked()
		publicBaseURL := configuredPublicBaseURLLocked()
		webhookURL := strings.TrimSpace(config.WebhookURL)
		webhookEnabled := configuredWebhookEnabledLocked()
		webhookProvider := configuredWebhookProviderLocked()
		slackChannel := strings.TrimSpace(config.WebhookSlackChannel)
		slackUsername := strings.TrimSpace(config.WebhookSlackUsername)
		slackIconEmoji := strings.TrimSpace(config.WebhookSlackIconEmoji)
		teamsTitlePrefix := strings.TrimSpace(config.WebhookTeamsTitlePrefix)
		saveConfigLocked()
		configMutex.Unlock()
		pruneBlockedHistory(time.Now().UTC(), historyDays)
		pruneVENHistory(time.Now().UTC(), historyDays)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"saved":                       true,
			"traffic_targets":             cleaned,
			"traffic_source_exclusions":   config.SourceExclusions,
			"history_days":                historyDays,
			"blocked_port_daily_enabled":  blockedPortDailyEnabled,
			"blocked_ma_window":           maWindow,
			"daily_ma_window":             dailyMAWindow,
			"blocked_anomaly_pct":         anomalyPct,
			"blocked_anomaly_baseline":    anomalyBaseline,
			"blocked_anomaly_days":        anomalyDays,
			"blocked_anomaly_min_pct":     anomalyMinCoverage,
			"ven_ma_window":               venMAWindow,
			"ven_anomaly_pct":             venAnomalyPct,
			"ven_anomaly_baseline":        venAnomalyBaseline,
			"ven_anomaly_days":            venAnomalyDays,
			"ven_anomaly_min_pct":         venAnomalyMinCoverage,
			"tampering_ma_window":         tamperMAWindow,
			"tampering_anomaly_pct":       tamperAnomalyPct,
			"tampering_anomaly_baseline":  tamperAnomalyBaseline,
			"tampering_anomaly_days":      tamperAnomalyDays,
			"tampering_anomaly_min_pct":   tamperAnomalyMinCoverage,
			"tampering_daily_anomaly_pct": tamperDailyAnomalyPct,
			"timezone":                    timezone,
			"timezone_effective":          effectiveTimezone,
			"bind_address":                bindAddress,
			"public_base_url":             publicBaseURL,
			"webhook_url":                 webhookURL,
			"webhook_enabled":             webhookEnabled,
			"webhook_provider":            webhookProvider,
			"webhook_slack_channel":       slackChannel,
			"webhook_slack_username":      slackUsername,
			"webhook_slack_icon_emoji":    slackIconEmoji,
			"webhook_teams_title_prefix":  teamsTitlePrefix,
			"message":                     "Saved. Click Refresh Now to apply immediately.",
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleConfigAlerts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMutex.RLock()
		webhookURL := strings.TrimSpace(config.WebhookURL)
		webhookEnabled := configuredWebhookEnabledLocked()
		webhookProvider := configuredWebhookProviderLocked()
		slackChannel := strings.TrimSpace(config.WebhookSlackChannel)
		slackUsername := strings.TrimSpace(config.WebhookSlackUsername)
		slackIconEmoji := strings.TrimSpace(config.WebhookSlackIconEmoji)
		teamsTitlePrefix := strings.TrimSpace(config.WebhookTeamsTitlePrefix)
		configMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"webhook_enabled":            webhookEnabled,
			"webhook_url":                webhookURL,
			"webhook_provider":           webhookProvider,
			"webhook_slack_channel":      slackChannel,
			"webhook_slack_username":     slackUsername,
			"webhook_slack_icon_emoji":   slackIconEmoji,
			"webhook_teams_title_prefix": teamsTitlePrefix,
		})
	case http.MethodPut:
		var req struct {
			WebhookEnabled          bool   `json:"webhook_enabled"`
			WebhookURL              string `json:"webhook_url"`
			WebhookProvider         string `json:"webhook_provider"`
			WebhookSlackChannel     string `json:"webhook_slack_channel"`
			WebhookSlackUsername    string `json:"webhook_slack_username"`
			WebhookSlackIconEmoji   string `json:"webhook_slack_icon_emoji"`
			WebhookTeamsTitlePrefix string `json:"webhook_teams_title_prefix"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		configMutex.Lock()
		config.WebhookURL = strings.TrimSpace(req.WebhookURL)
		config.WebhookEnabled = req.WebhookEnabled && config.WebhookURL != ""
		config.WebhookProvider = normalizeWebhookProvider(req.WebhookProvider)
		config.WebhookSlackChannel = strings.TrimSpace(req.WebhookSlackChannel)
		config.WebhookSlackUsername = strings.TrimSpace(req.WebhookSlackUsername)
		config.WebhookSlackIconEmoji = strings.TrimSpace(req.WebhookSlackIconEmoji)
		config.WebhookTeamsTitlePrefix = strings.TrimSpace(req.WebhookTeamsTitlePrefix)
		saveConfigLocked()
		webhookURL := strings.TrimSpace(config.WebhookURL)
		webhookEnabled := configuredWebhookEnabledLocked()
		webhookProvider := configuredWebhookProviderLocked()
		slackChannel := strings.TrimSpace(config.WebhookSlackChannel)
		slackUsername := strings.TrimSpace(config.WebhookSlackUsername)
		slackIconEmoji := strings.TrimSpace(config.WebhookSlackIconEmoji)
		teamsTitlePrefix := strings.TrimSpace(config.WebhookTeamsTitlePrefix)
		configMutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"saved":                      true,
			"webhook_enabled":            webhookEnabled,
			"webhook_url":                webhookURL,
			"webhook_provider":           webhookProvider,
			"webhook_slack_channel":      slackChannel,
			"webhook_slack_username":     slackUsername,
			"webhook_slack_icon_emoji":   slackIconEmoji,
			"webhook_teams_title_prefix": teamsTitlePrefix,
			"message":                    "Alerting settings saved.",
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleRefreshNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	go runCollectionCycle()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
}

func handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !configuredWebhookEnabled() {
		http.Error(w, "webhook is not enabled", http.StatusBadRequest)
		return
	}
	if err := sendWebhookEvent(map[string]interface{}{
		"event":      "webhook_test",
		"state":      "test",
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
		"message":    "Illumio dashboard webhook test event",
		"dashboard":  configuredPublicBaseURL(),
		"source_app": "illumio-monitoring-dashboard",
	}); err != nil {
		http.Error(w, "webhook test failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": "webhook test sent"})
}

func handleAnomalyHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	days := 7
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			days = v
		}
	}
	if days > maxHistoryDays {
		days = maxHistoryDays
	}
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 5000 {
		limit = 5000
	}

	now := time.Now().UTC()
	cutoffPeriod := now.AddDate(0, 0, -days)
	cutoff24 := now.Add(-24 * time.Hour)
	events := make([]anomalyHistoryEvent, 0)
	triggered24 := 0
	resolved24 := 0
	triggeredPeriod := 0
	resolvedPeriod := 0
	anomalyHistoryMu.Lock()
	for i := len(anomalyHistory) - 1; i >= 0; i-- {
		ev := anomalyHistory[i]
		if ev.Timestamp.Before(cutoffPeriod) {
			break
		}
		if strings.EqualFold(strings.TrimSpace(ev.State), "triggered") {
			triggeredPeriod++
			if !ev.Timestamp.Before(cutoff24) {
				triggered24++
			}
		}
		if strings.EqualFold(strings.TrimSpace(ev.State), "resolved") {
			resolvedPeriod++
			if !ev.Timestamp.Before(cutoff24) {
				resolved24++
			}
		}
		if len(events) < limit {
			events = append(events, ev)
		}
	}
	anomalyHistoryMu.Unlock()

	activeBlocked := 0
	alertMu.Lock()
	for _, st := range alertState.Targets {
		if st.Active {
			activeBlocked++
		}
	}
	alertMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"events": events,
		"summary": map[string]interface{}{
			"period_days":            days,
			"triggered_24h":          triggered24,
			"resolved_24h":           resolved24,
			"triggered_period":       triggeredPeriod,
			"resolved_period":        resolvedPeriod,
			"active_blocked_targets": activeBlocked,
		},
	})
}

func processWebhookAlerts(stats DashboardStats) {
	webhookEnabled := configuredWebhookEnabled()
	baseURL := configuredPublicBaseURL()
	now := time.Now().UTC()
	targets := coalesceBlockedAlertTargets(stats.Blocked.Targets)
	if len(targets) == 0 {
		return
	}
	alertMu.Lock()
	if alertState.Targets == nil {
		alertState.Targets = map[string]alertTargetState{}
	}
	prevByKey := make(map[string]alertTargetState, len(alertState.Targets))
	for k, v := range alertState.Targets {
		prevByKey[k] = v
	}
	alertMu.Unlock()

	type ev struct {
		key       string
		name      string
		eventType string
		payload   map[string]interface{}
		history   anomalyHistoryEvent
	}
	events := make([]ev, 0)
	for _, t := range targets {
		key := strings.ToLower(strings.TrimSpace(t.Name))
		if key == "" {
			continue
		}
		prev := prevByKey[key]
		if t.Anomalous && !prev.Active {
			reason := strings.TrimSpace(t.AnomalyReason)
			events = append(events, ev{
				key:       key,
				name:      t.Name,
				eventType: "triggered",
				payload: map[string]interface{}{
					"event":                 "blocked_target_anomaly",
					"state":                 "triggered",
					"timestamp":             now.Format(time.RFC3339),
					"target_name":           t.Name,
					"target_kind":           t.Kind,
					"latest_5m":             t.Latest5m,
					"moving_avg_5m":         t.MovingAvg5m,
					"anomaly_threshold_pct": t.AnomalyPct,
					"ma_window_points":      t.AnomalyWindow,
					"anomaly_source":        t.AnomalySource,
					"anomaly_coverage_pct":  t.AnomalyCoverage,
					"reason":                reason,
					"dashboard_url":         fmt.Sprintf("%s/details?metric=blocked_target&target=%s", baseURL, url.QueryEscape(t.Name)),
				},
				history: anomalyHistoryEvent{
					Timestamp:           now,
					Event:               "blocked_target_anomaly",
					State:               "triggered",
					Metric:              "blocked_target",
					TargetName:          t.Name,
					TargetKind:          t.Kind,
					Latest5m:            t.Latest5m,
					MovingAvg5m:         t.MovingAvg5m,
					AnomalyThresholdPct: t.AnomalyPct,
					MAWindowPoints:      t.AnomalyWindow,
					AnomalySource:       t.AnomalySource,
					AnomalyCoveragePct:  t.AnomalyCoverage,
					Reason:              reason,
				},
			})
			continue
		}
		if !t.Anomalous && prev.Active {
			resolveReason := "latest 5m value returned within configured threshold over moving average"
			if strings.EqualFold(strings.TrimSpace(t.AnomalySource), "daily") {
				resolveReason = "latest 5m value returned within configured threshold over daily baseline"
			}
			if strings.TrimSpace(t.AnomalyReason) != "" {
				resolveReason = strings.TrimSpace(t.AnomalyReason)
			}
			events = append(events, ev{
				key:       key,
				name:      t.Name,
				eventType: "resolved",
				payload: map[string]interface{}{
					"event":                 "blocked_target_anomaly",
					"state":                 "resolved",
					"timestamp":             now.Format(time.RFC3339),
					"target_name":           t.Name,
					"target_kind":           t.Kind,
					"latest_5m":             t.Latest5m,
					"moving_avg_5m":         t.MovingAvg5m,
					"anomaly_threshold_pct": t.AnomalyPct,
					"ma_window_points":      t.AnomalyWindow,
					"anomaly_source":        t.AnomalySource,
					"anomaly_coverage_pct":  t.AnomalyCoverage,
					"reason":                resolveReason,
					"dashboard_url":         fmt.Sprintf("%s/details?metric=blocked_target&target=%s", baseURL, url.QueryEscape(t.Name)),
				},
				history: anomalyHistoryEvent{
					Timestamp:           now,
					Event:               "blocked_target_anomaly",
					State:               "resolved",
					Metric:              "blocked_target",
					TargetName:          t.Name,
					TargetKind:          t.Kind,
					Latest5m:            t.Latest5m,
					MovingAvg5m:         t.MovingAvg5m,
					AnomalyThresholdPct: t.AnomalyPct,
					MAWindowPoints:      t.AnomalyWindow,
					AnomalySource:       t.AnomalySource,
					AnomalyCoveragePct:  t.AnomalyCoverage,
					Reason:              resolveReason,
				},
			})
		}
	}

	changed := false
	historyBatch := make([]anomalyHistoryEvent, 0, len(events))
	for _, e := range events {
		if webhookEnabled {
			if err := sendWebhookEvent(e.payload); err != nil {
				log.Printf("[WEBHOOK] %s send failed for %s: %v", e.eventType, e.name, err)
			}
		}
		alertMu.Lock()
		prev := alertState.Targets[e.key]
		prev.Active = e.eventType == "triggered"
		prev.LastEventUTC = now
		prev.LastEventType = e.eventType
		alertState.Targets[e.key] = prev
		alertMu.Unlock()
		changed = true
		historyBatch = append(historyBatch, e.history)
	}
	if changed {
		saveAlertState()
		appendAnomalyHistoryEvents(historyBatch)
	}
}

func coalesceBlockedAlertTargets(targets []BlockedTargetResult) []BlockedTargetResult {
	if len(targets) == 0 {
		return nil
	}
	out := make([]BlockedTargetResult, 0, len(targets))
	idxByKey := make(map[string]int, len(targets))
	dupes := 0
	for _, t := range targets {
		key := strings.ToLower(strings.TrimSpace(t.Name))
		if key == "" {
			continue
		}
		if idx, ok := idxByKey[key]; ok {
			dupes++
			if blockedAlertPriority(t) > blockedAlertPriority(out[idx]) {
				out[idx] = t
			}
			continue
		}
		idxByKey[key] = len(out)
		out = append(out, t)
	}
	if dupes > 0 {
		log.Printf("[WEBHOOK] coalesced %d duplicate blocked target entries before alert evaluation", dupes)
	}
	return out
}

func blockedAlertPriority(t BlockedTargetResult) int {
	score := 0
	if t.Anomalous {
		score += 8
	}
	if strings.TrimSpace(t.AnomalyReason) != "" {
		score += 4
	}
	if t.AnomalyPct > 0 {
		score += 2
	}
	if t.AnomalyWindow > 0 {
		score += 1
	}
	if strings.TrimSpace(t.Kind) != "" {
		score += 1
	}
	return score
}

func sendWebhookEvent(payload map[string]interface{}) error {
	webhookURL := configuredWebhookURL()
	if strings.TrimSpace(webhookURL) == "" {
		return errors.New("webhook URL is empty")
	}
	opts := configuredWebhookFormatOptions()
	bodyPayload := formatWebhookPayload(opts, payload)
	b, err := json.Marshal(bodyPayload)
	if err != nil {
		return fmt.Errorf("encode webhook payload: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("webhook HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func formatWebhookPayload(opts webhookFormatOptions, payload map[string]interface{}) interface{} {
	switch normalizeWebhookProvider(opts.Provider) {
	case "slack":
		out := map[string]interface{}{
			"text": buildWebhookText(payload),
		}
		if opts.SlackChannel != "" {
			out["channel"] = opts.SlackChannel
		}
		if opts.SlackUsername != "" {
			out["username"] = opts.SlackUsername
		}
		if opts.SlackIconEmoji != "" {
			out["icon_emoji"] = opts.SlackIconEmoji
		}
		return out
	case "teams":
		title := buildWebhookSummary(payload)
		if opts.TeamsTitlePrefix != "" {
			title = strings.TrimSpace(opts.TeamsTitlePrefix + " | " + title)
		}
		return map[string]interface{}{
			"@type":    "MessageCard",
			"@context": "https://schema.org/extensions",
			"summary":  title,
			"themeColor": func() string {
				if strings.EqualFold(strings.TrimSpace(fmt.Sprint(payload["state"])), "resolved") {
					return "28A745"
				}
				return "DC3545"
			}(),
			"title": title,
			"sections": []map[string]interface{}{
				{
					"activityTitle": buildWebhookText(payload),
					"facts":         buildTeamsFacts(payload),
					"markdown":      true,
				},
			},
		}
	default:
		return payload
	}
}

func buildWebhookSummary(payload map[string]interface{}) string {
	event := payloadString(payload, "event")
	state := payloadString(payload, "state")
	target := payloadString(payload, "target_name")
	if event == "" {
		event = "webhook_event"
	}
	parts := []string{event}
	if state != "" {
		parts = append(parts, state)
	}
	if target != "" {
		parts = append(parts, target)
	}
	return strings.Join(parts, " | ")
}

func buildWebhookText(payload map[string]interface{}) string {
	event := payloadString(payload, "event")
	state := payloadString(payload, "state")
	target := payloadString(payload, "target_name")
	latest := payloadString(payload, "latest_5m")
	ma := payloadString(payload, "moving_avg_5m")
	threshold := payloadString(payload, "anomaly_threshold_pct")
	reason := payloadString(payload, "reason")
	dashboard := payloadString(payload, "dashboard_url")
	if event == "webhook_test" {
		msg := payloadString(payload, "message")
		if msg == "" {
			msg = "Illumio dashboard webhook test event"
		}
		return msg
	}
	lines := []string{fmt.Sprintf("Illumio Alert: %s (%s)", event, state)}
	if target != "" {
		lines = append(lines, "Target: "+target)
	}
	if latest != "" || ma != "" {
		lines = append(lines, fmt.Sprintf("Latest 5m: %s | Moving Avg: %s", latest, ma))
	}
	if threshold != "" {
		lines = append(lines, "Threshold: "+threshold+"%")
	}
	if reason != "" {
		lines = append(lines, "Reason: "+reason)
	}
	if dashboard != "" {
		lines = append(lines, "Details: "+dashboard)
	}
	return strings.Join(lines, "\n")
}

func buildTeamsFacts(payload map[string]interface{}) []map[string]string {
	keys := []struct {
		Key   string
		Title string
	}{
		{"event", "Event"},
		{"state", "State"},
		{"target_name", "Target"},
		{"target_kind", "Target Kind"},
		{"latest_5m", "Latest 5m"},
		{"moving_avg_5m", "Moving Avg 5m"},
		{"anomaly_threshold_pct", "Threshold %"},
		{"ma_window_points", "MA Window"},
		{"reason", "Reason"},
		{"dashboard_url", "Dashboard"},
		{"timestamp", "Timestamp"},
	}
	facts := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		v := payloadString(payload, k.Key)
		if v == "" {
			continue
		}
		facts = append(facts, map[string]string{
			"name":  k.Title,
			"value": v,
		})
	}
	return facts
}

func payloadString(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	raw, ok := payload[key]
	if !ok || raw == nil {
		return ""
	}
	s := strings.TrimSpace(fmt.Sprint(raw))
	if s == "<nil>" || s == "null" {
		return ""
	}
	return s
}

func handleDebugVENStatus(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	includeAll := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("all")), "true")

	configMutex.RLock()
	pceURL := config.PCEURL
	orgID := config.OrgID
	configMutex.RUnlock()
	baseURL := fmt.Sprintf("%s/api/v2/orgs/%s", strings.TrimSuffix(pceURL, "/"), orgID)

	vens, err := getAllVENs(baseURL)
	if err != nil {
		http.Error(w, "debug ven fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	warningVENs, warningErr := getAllVENsByHealth(baseURL, "warning")
	errorVENs, errorErr := getAllVENsByHealth(baseURL, "error")

	type item struct {
		Name           string      `json:"name"`
		HRef           string      `json:"href,omitempty"`
		ExtractedState string      `json:"extracted_state"`
		RawHealth      interface{} `json:"raw_health,omitempty"`
		RawStatus      interface{} `json:"raw_status,omitempty"`
		RawVENStatus   interface{} `json:"raw_ven_status,omitempty"`
	}

	out := make([]item, 0, minInt(limit, len(vens)))
	summary := map[string]int{
		"warning": 0,
		"error":   0,
		"other":   0,
		"empty":   0,
	}

	for _, ven := range vens {
		state := venHealthFromVEN(ven)
		switch state {
		case "warning":
			summary["warning"]++
		case "error":
			summary["error"]++
		case "":
			summary["empty"]++
		default:
			summary["other"]++
		}
		if !includeAll && state != "warning" && state != "error" {
			continue
		}
		name := venDisplayName(ven)
		href, _ := ven["href"].(string)
		out = append(out, item{
			Name:           name,
			HRef:           href,
			ExtractedState: state,
			RawHealth:      ven["health"],
			RawStatus:      ven["status"],
			RawVENStatus:   ven["ven_status"],
		})
		if len(out) >= limit {
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	healthQuery := map[string]interface{}{
		"warning_count": len(warningVENs),
		"error_count":   len(errorVENs),
	}
	if warningErr != nil {
		healthQuery["warning_error"] = warningErr.Error()
	}
	if errorErr != nil {
		healthQuery["error_error"] = errorErr.Error()
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"total_vens":   len(vens),
		"health_query": healthQuery,
		"summary":      summary,
		"returned":     len(out),
		"items":        out,
	})
}

func drilldownData(metric, target string, stats DashboardStats) (string, []string, []TrendPoint) {
	switch metric {
	case "ven_warning":
		return "VEN Warnings", append([]string(nil), stats.VENStatus.Warning...), venTrendSeries("warning")
	case "ven_error":
		return "VEN Errors", append([]string(nil), stats.VENStatus.Error...), venTrendSeries("error")
	case "mode_idle":
		return "Workloads in Idle Mode", append([]string(nil), stats.Workloads.ModeMembers["idle"]...), modeTrendSeries("idle")
	case "mode_visibility_only":
		return "Workloads in Visibility Mode", append([]string(nil), stats.Workloads.ModeMembers["visibility_only"]...), modeTrendSeries("visibility_only")
	case "mode_selective":
		return "Workloads in Selective Mode", append([]string(nil), stats.Workloads.ModeMembers["selective"]...), modeTrendSeries("selective")
	case "mode_full":
		return "Workloads in Full Mode", append([]string(nil), stats.Workloads.ModeMembers["full"]...), modeTrendSeries("full")
	case "mode_unmanaged":
		return "Unmanaged Workloads", append([]string(nil), stats.Workloads.ModeMembers["unmanaged"]...), modeTrendSeries("unmanaged")
	case "tampering":
		return "Tampered VENs/Workloads (Deduped)", append([]string(nil), stats.Tampering.Workloads...), tamperingTrendSeries()
	case "blocked_target":
		target = strings.TrimSpace(target)
		if target == "" {
			return "", nil, nil
		}
		if !blockedTargetExists(target, stats) {
			return "", nil, nil
		}
		trend := blockedTrendSeries(target)
		return fmt.Sprintf("Blocked Traffic Trend - %s", target), nil, trend
	default:
		return "", nil, nil
	}
}

func isEnforcementMetric(metric string) bool {
	switch strings.TrimSpace(metric) {
	case "mode_idle", "mode_visibility_only", "mode_selective", "mode_full", "mode_unmanaged":
		return true
	default:
		return false
	}
}

func enforcementModeFromMetric(metric string) string {
	switch strings.TrimSpace(metric) {
	case "mode_idle":
		return "idle"
	case "mode_visibility_only":
		return "visibility_only"
	case "mode_selective":
		return "selective"
	case "mode_full":
		return "full"
	case "mode_unmanaged":
		return "unmanaged"
	default:
		return ""
	}
}

func parseBoolQuery(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}
	n, err := sw.ResponseWriter.Write(b)
	sw.bytes += n
	return n, err
}

func withRequestTiming(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		dur := time.Since(start)
		if dur >= slowRequestLogThreshold || sw.status >= http.StatusBadRequest {
			route := r.URL.Path
			if q := strings.TrimSpace(r.URL.RawQuery); q != "" {
				route = route + "?" + q
			}
			log.Printf("[HTTP] route=%s name=%s method=%s status=%d dur=%s bytes=%d", route, name, r.Method, sw.status, dur.Round(time.Millisecond), sw.bytes)
		}
	}
}

func venTrendSeries(kind string) []TrendPoint {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	points := make([]TrendPoint, 0)
	for _, b := range rollingCache.Buckets {
		if !b.EndUTC.After(cutoff) {
			continue
		}
		v := 0
		if kind == "warning" {
			v = b.VENWarningCount
		} else {
			v = b.VENErrorCount
		}
		points = append(points, TrendPoint{
			Timestamp: b.EndUTC,
			Value:     v,
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return points
}

func modeTrendSeries(kind string) []TrendPoint {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	points := make([]TrendPoint, 0)
	for _, b := range rollingCache.Buckets {
		if !b.EndUTC.After(cutoff) {
			continue
		}
		v := 0
		switch kind {
		case "idle":
			v = b.ModeIdleCount
		case "visibility_only":
			v = b.ModeVisCount
		case "selective":
			v = b.ModeSelectiveCount
		case "full":
			v = b.ModeFullCount
		case "unmanaged":
			v = b.ModeUnmanagedCount
		}
		points = append(points, TrendPoint{Timestamp: b.EndUTC, Value: v})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return points
}

func tamperingTrendSeries() []TrendPoint {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	points := make([]TrendPoint, 0)
	for _, b := range rollingCache.Buckets {
		if !b.EndUTC.After(cutoff) {
			continue
		}
		points = append(points, TrendPoint{
			Timestamp: b.EndUTC,
			Value:     b.TamperingCount,
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return points
}

func venDailyTrendSeries(kind string, keepDays int) []TrendPoint {
	if keepDays <= 0 {
		keepDays = 365
	}
	loc := configuredDayLocation()
	cutoff := localDayStart(time.Now(), loc).AddDate(0, 0, -keepDays)
	venHistoryMu.Lock()
	defer venHistoryMu.Unlock()
	points := make([]TrendPoint, 0, len(venDaily))
	for day, snap := range venDaily {
		d, err := parseDayKeyInLocation(day, loc)
		if err != nil || d.Before(cutoff) {
			continue
		}
		v := snap.WarningMax
		if kind == "error" {
			v = snap.ErrorMax
		}
		points = append(points, TrendPoint{
			Timestamp: d.Add(12 * time.Hour).UTC(),
			Value:     v,
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return points
}

func modeDailyTrendSeries(kind string, keepDays int) []TrendPoint {
	if keepDays <= 0 {
		keepDays = 365
	}
	loc := configuredDayLocation()
	cutoff := localDayStart(time.Now(), loc).AddDate(0, 0, -keepDays)
	venHistoryMu.Lock()
	defer venHistoryMu.Unlock()
	points := make([]TrendPoint, 0, len(venDaily))
	for day, snap := range venDaily {
		d, err := parseDayKeyInLocation(day, loc)
		if err != nil || d.Before(cutoff) {
			continue
		}
		v := 0
		switch kind {
		case "idle":
			v = snap.ModeIdleMax
		case "visibility_only":
			v = snap.ModeVisMax
		case "selective":
			v = snap.ModeSelectiveMax
		case "full":
			v = snap.ModeFullMax
		case "unmanaged":
			v = snap.ModeUnmanagedMax
		}
		points = append(points, TrendPoint{
			Timestamp: d.Add(12 * time.Hour).UTC(),
			Value:     v,
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return points
}

func tamperingDailyTrendSeries(keepDays int) []TrendPoint {
	if keepDays <= 0 {
		keepDays = 365
	}
	loc := configuredDayLocation()
	cutoff := localDayStart(time.Now(), loc).AddDate(0, 0, -keepDays)
	venHistoryMu.Lock()
	defer venHistoryMu.Unlock()
	points := make([]TrendPoint, 0, len(venDaily))
	for day, snap := range venDaily {
		d, err := parseDayKeyInLocation(day, loc)
		if err != nil || d.Before(cutoff) {
			continue
		}
		points = append(points, TrendPoint{
			Timestamp: d.Add(12 * time.Hour).UTC(),
			Value:     snap.TamperingMax,
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return points
}

func blockedTargetExists(target string, stats DashboardStats) bool {
	for _, t := range stats.Blocked.Targets {
		if strings.EqualFold(strings.TrimSpace(t.Name), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

func blockedTrendSeries(target string) []TrendPoint {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	points := make([]TrendPoint, 0)
	for _, b := range rollingCache.Buckets {
		if !b.EndUTC.After(cutoff) {
			continue
		}
		if v, ok := b.BlockedByTarget[target]; ok {
			points = append(points, TrendPoint{
				Timestamp: b.EndUTC,
				Value:     v,
			})
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return points
}

func blockedDailyTrendSeries(target string, keepDays int) []TrendPoint {
	if keepDays <= 0 {
		keepDays = 365
	}
	loc := configuredDayLocation()
	now := time.Now()
	cutoff := localDayStart(now, loc).AddDate(0, 0, -keepDays)
	historyMu.Lock()
	points := make([]TrendPoint, 0, len(blockedDaily))
	hasToday := false
	for day, targets := range blockedDaily {
		d, err := parseDayKeyInLocation(day, loc)
		if err != nil || d.Before(cutoff) {
			continue
		}
		v, ok := targets[target]
		if !ok {
			continue
		}
		points = append(points, TrendPoint{
			Timestamp: d.Add(12 * time.Hour).UTC(),
			Value:     v,
		})
		if day == localDayStart(now, loc).Format("2006-01-02") {
			hasToday = true
		}
	}
	historyMu.Unlock()
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })

	// Add a live "today so far" point so daily charts continue through the current day.
	todayStartLocal := localDayStart(now, loc)
	todayStartUTC := todayStartLocal.UTC()
	if !hasToday {
		todayCount := 0
		rollingMu.Lock()
		for _, b := range rollingCache.Buckets {
			if !b.EndUTC.After(todayStartUTC) {
				continue
			}
			if v, ok := b.BlockedByTarget[target]; ok {
				todayCount += v
			}
		}
		rollingMu.Unlock()
		points = append(points, TrendPoint{
			Timestamp: now.UTC(),
			Value:     todayCount,
		})
		sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	}
	return points
}

func blockedPortDailySeries(target string, keepDays int) []BlockedPortDay {
	if !configuredBlockedPortDailyEnabled() {
		return nil
	}
	if keepDays <= 0 {
		keepDays = 365
	}
	loc := configuredDayLocation()
	now := time.Now()
	cutoff := localDayStart(now, loc).AddDate(0, 0, -keepDays)

	historyMu.Lock()
	defer historyMu.Unlock()
	out := make([]BlockedPortDay, 0, len(blockedPortsDaily))
	for day, targets := range blockedPortsDaily {
		d, err := parseDayKeyInLocation(day, loc)
		if err != nil || d.Before(cutoff) {
			continue
		}
		portMap, ok := targets[target]
		if !ok {
			continue
		}
		ports := portCountMapToSortedSlice(portMap)
		out = append(out, BlockedPortDay{
			Timestamp: d.Add(12 * time.Hour).UTC(),
			Ports:     ports,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

func portCountMapToSortedSlice(portMap map[string]int) []PortCount {
	ports := make([]PortCount, 0, len(portMap))
	for key, count := range portMap {
		if strings.TrimSpace(key) == "" || count <= 0 {
			continue
		}
		ports = append(ports, PortCount{Key: key, Count: count})
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Count == ports[j].Count {
			return ports[i].Key < ports[j].Key
		}
		return ports[i].Count > ports[j].Count
	})
	return ports
}

func movingAverageTrend(points []TrendPoint, window int) []TrendPointF {
	if window <= 0 || len(points) <= window {
		return nil
	}
	out := make([]TrendPointF, 0, len(points)-window)
	sum := 0
	for i := 0; i < len(points); i++ {
		v := points[i].Value
		sum += v
		if i < window {
			continue
		}
		sum -= points[i-window].Value
		avg := float64(sum) / float64(window)
		out = append(out, TrendPointF{
			Timestamp: points[i].Timestamp,
			Value:     math.Round(avg*100) / 100,
		})
	}
	return out
}

func anomalyFromSeries(points []TrendPoint, window int, thresholdPct float64) (int, float64, bool, string) {
	if len(points) == 0 {
		return 0, 0, false, ""
	}
	latest := points[len(points)-1].Value
	if window <= 0 {
		return latest, 0, false, "moving average window is not configured"
	}
	if len(points) <= window {
		return latest, 0, false, fmt.Sprintf("needs at least %d points", window+1)
	}
	start := len(points) - 1 - window
	if start < 0 {
		start = 0
	}
	sum := 0
	for i := start; i < len(points)-1; i++ {
		sum += points[i].Value
	}
	avg := float64(sum) / float64(window)
	if avg <= 0 {
		return latest, avg, false, "moving average is zero"
	}
	thresholdMultiplier := 1.0 + (thresholdPct / 100.0)
	trigger := float64(latest) > avg*thresholdMultiplier
	if trigger {
		return latest, math.Round(avg*100) / 100, true, fmt.Sprintf("latest value exceeded %.0f%% threshold over moving average", thresholdPct)
	}
	return latest, math.Round(avg*100) / 100, false, ""
}

type anomalyEvalResult struct {
	Latest      int
	Baseline    float64
	Anomalous   bool
	Reason      string
	Window      int
	Source      string
	CoveragePct float64
}

func latestTrendValue(points []TrendPoint) int {
	if len(points) == 0 {
		return 0
	}
	return points[len(points)-1].Value
}

func averageLast(points []TrendPoint, n int) (float64, int) {
	if n <= 0 || len(points) == 0 {
		return 0, 0
	}
	if len(points) < n {
		n = len(points)
	}
	sum := 0
	start := len(points) - n
	for i := start; i < len(points); i++ {
		sum += points[i].Value
	}
	return float64(sum) / float64(n), n
}

func flatTrendLine(points []TrendPoint, value float64) []TrendPointF {
	if len(points) == 0 {
		return nil
	}
	v := math.Round(value*100) / 100
	out := make([]TrendPointF, 0, len(points))
	for _, p := range points {
		out = append(out, TrendPointF{Timestamp: p.Timestamp, Value: v})
	}
	return out
}

func blockedAnomalyFromConfig(series5m, seriesDaily []TrendPoint, window5m int, thresholdPct float64, source string, baselineDays int, minCoveragePct float64) anomalyEvalResult {
	src := normalizeAnomalyBaselineSource(source)
	if src == "daily" {
		latest := latestTrendValue(series5m)
		if baselineDays <= 0 {
			baselineDays = 7
		}
		avg, used := averageLast(seriesDaily, baselineDays)
		if used == 0 {
			return anomalyEvalResult{
				Latest:      latest,
				Baseline:    0,
				Anomalous:   false,
				Reason:      fmt.Sprintf("baseline warmup: 0/%d day(s) available", baselineDays),
				Window:      baselineDays,
				Source:      "daily",
				CoveragePct: 0,
			}
		}
		coverage := (float64(used) / float64(baselineDays)) * 100.0
		if coverage < minCoveragePct {
			return anomalyEvalResult{
				Latest:      latest,
				Baseline:    math.Round(avg*100) / 100,
				Anomalous:   false,
				Reason:      fmt.Sprintf("baseline warmup: %.0f%% coverage (%d/%d day(s)); threshold %.0f%%", coverage, used, baselineDays, minCoveragePct),
				Window:      baselineDays,
				Source:      "daily",
				CoveragePct: math.Round(coverage*100) / 100,
			}
		}
		if avg <= 0 {
			return anomalyEvalResult{
				Latest:      latest,
				Baseline:    0,
				Anomalous:   false,
				Reason:      "baseline is zero",
				Window:      baselineDays,
				Source:      "daily",
				CoveragePct: math.Round(coverage*100) / 100,
			}
		}
		trigger := float64(latest) > avg*(1.0+thresholdPct/100.0)
		reason := ""
		if trigger {
			reason = fmt.Sprintf("latest 5m value exceeded %.0f%% threshold over %d-day baseline", thresholdPct, baselineDays)
		}
		return anomalyEvalResult{
			Latest:      latest,
			Baseline:    math.Round(avg*100) / 100,
			Anomalous:   trigger,
			Reason:      reason,
			Window:      baselineDays,
			Source:      "daily",
			CoveragePct: math.Round(coverage*100) / 100,
		}
	}
	latest, avg, anomalous, reason := anomalyFromSeries(series5m, window5m, thresholdPct)
	if anomalous {
		reason = fmt.Sprintf("latest 5m value exceeded %.0f%% threshold over moving average", thresholdPct)
	}
	return anomalyEvalResult{
		Latest:      latest,
		Baseline:    avg,
		Anomalous:   anomalous,
		Reason:      reason,
		Window:      window5m,
		Source:      "5m",
		CoveragePct: 100,
	}
}

func effectiveBlockedAnomalySettingsForTarget(target TrafficTarget, defaultWindow int, defaultPct float64) (int, float64) {
	window := defaultWindow
	pct := defaultPct
	if target.BlockedMAWindow > 0 {
		window = normalizeMAWindow(target.BlockedMAWindow, defaultWindow)
	}
	if target.BlockedAnomalyPct > 0 {
		pct = normalizeAnomalyPct(target.BlockedAnomalyPct, defaultPct)
	}
	return window, pct
}

func configuredTrafficTargetByName(name string) (TrafficTarget, bool) {
	targets := configuredTrafficTargets()
	for _, t := range targets {
		if strings.EqualFold(strings.TrimSpace(t.Name), strings.TrimSpace(name)) {
			return t, true
		}
	}
	return TrafficTarget{}, false
}

func blockedTargetBaseline(target string) (int, *time.Time, bool) {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	entry, ok := rollingCache.BaselineBlocked[target]
	if !ok {
		return 0, nil, false
	}
	t := entry.CapturedUTC
	return entry.Count, &t, true
}

func getAllWorkloads(baseURL string) ([]map[string]interface{}, error) {
	const pageSize = 500
	const maxPages = 200
	bulkQ := url.Values{}
	bulkQ.Set("max_results", "200000")
	bulk, bulkErr := fetchCollectionPage(baseURL + "/workloads?" + bulkQ.Encode())
	if bulkErr == nil && len(bulk) > 0 {
		log.Printf("[COLLECTOR] Workloads bulk fetch returned %d", len(bulk))
		return bulk, nil
	}
	all := make([]map[string]interface{}, 0, pageSize)
	seen := make(map[string]struct{}, pageSize)

	for page := 0; page < maxPages; page++ {
		skip := page * pageSize
		q := url.Values{}
		q.Set("max_results", strconv.Itoa(pageSize))
		q.Set("skip", strconv.Itoa(skip))

		batch, err := fetchCollectionPage(baseURL + "/workloads?" + q.Encode())
		if err != nil {
			if page == 0 {
				fallback, fbErr := fetchCollectionPage(baseURL + "/workloads")
				if fbErr != nil {
					return nil, err
				}
				return fallback, nil
			}
			return all, fmt.Errorf("partial workload result: page=%d failed: %w", page, err)
		}
		if len(batch) == 0 {
			break
		}

		newCount := 0
		for _, w := range batch {
			key, _ := w["href"].(string)
			if key == "" {
				name, _ := w["name"].(string)
				hostname, _ := w["hostname"].(string)
				key = name + "|" + hostname
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			all = append(all, w)
			newCount++
		}
		if newCount == 0 && len(batch) == pageSize && page > 0 {
			q2 := url.Values{}
			q2.Set("max_results", "200000")
			bulk, bulkErr := fetchCollectionPage(baseURL + "/workloads?" + q2.Encode())
			if bulkErr == nil && len(bulk) > len(all) {
				return bulk, nil
			}
			log.Printf("[COLLECTOR] Workloads pagination did not advance at page=%d; keeping %d collected rows", page, len(all))
			break
		}
		if len(batch) < pageSize || newCount == 0 {
			break
		}
	}
	if len(all) == 0 && bulkErr != nil {
		return nil, bulkErr
	}
	log.Printf("[COLLECTOR] Workloads paged fetch returned %d", len(all))
	return all, nil
}

func getAllVENs(baseURL string) ([]map[string]interface{}, error) {
	return getAllVENsWithFilters(baseURL, nil)
}

func getAllVENsByHealth(baseURL, health string) ([]map[string]interface{}, error) {
	return getAllVENsWithFilters(baseURL, map[string]string{"health": strings.TrimSpace(strings.ToLower(health))})
}

func getAllVENsWithFilters(baseURL string, extraFilters map[string]string) ([]map[string]interface{}, error) {
	const pageSize = 500
	all := make([]map[string]interface{}, 0, pageSize)
	seen := make(map[string]struct{}, pageSize)
	started := time.Now()

	for page := 0; ; page++ {
		if time.Since(started) > venQueryMaxDuration {
			return all, fmt.Errorf("partial VEN result: pagination timed out after %s at page=%d", venQueryMaxDuration, page)
		}
		skip := page * pageSize
		q := url.Values{}
		q.Set("max_results", strconv.Itoa(pageSize))
		q.Set("skip", strconv.Itoa(skip))
		for k, v := range extraFilters {
			if strings.TrimSpace(v) != "" {
				q.Set(k, v)
			}
		}

		batch, err := fetchCollectionPage(baseURL + "/vens?" + q.Encode())
		if err != nil {
			if page == 0 {
				return nil, err
			}
			return all, fmt.Errorf("partial VEN result: page=%d failed: %w", page, err)
		}
		if len(batch) == 0 {
			break
		}

		newCount := 0
		for _, v := range batch {
			key, _ := v["href"].(string)
			if key == "" {
				name, _ := v["name"].(string)
				hostname, _ := v["hostname"].(string)
				key = name + "|" + hostname
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			all = append(all, v)
			newCount++
		}
		if newCount == 0 && len(batch) == pageSize && page > 0 {
			q2 := url.Values{}
			q2.Set("max_results", "200000")
			for k, v := range extraFilters {
				if strings.TrimSpace(v) != "" {
					q2.Set(k, v)
				}
			}
			bulk, bulkErr := fetchCollectionPage(baseURL + "/vens?" + q2.Encode())
			if bulkErr == nil && len(bulk) > len(all) {
				return bulk, nil
			}
		}
		if len(batch) < pageSize || newCount == 0 {
			break
		}
	}
	return all, nil
}

func stringFromMap(m map[string]interface{}, keys ...string) string {
	cur := interface{}(m)
	for _, k := range keys {
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = obj[k]
	}
	s, _ := cur.(string)
	return strings.TrimSpace(strings.ToLower(s))
}

func venHealthFromVEN(v map[string]interface{}) string {
	if s := stringFromMap(v, "health"); s != "" {
		return s
	}
	if s := stringFromMap(v, "ven_status"); s != "" {
		return s
	}
	if s := stringFromMap(v, "status", "health"); s != "" {
		return s
	}
	if s := stringFromMap(v, "status", "overall_health"); s != "" {
		return s
	}
	if s := stringFromMap(v, "ven", "health"); s != "" {
		return s
	}
	if s := stringFromMap(v, "ven_health"); s != "" {
		return s
	}
	return ""
}

func venDisplayName(v map[string]interface{}) string {
	name, _ := v["name"].(string)
	if strings.TrimSpace(name) != "" {
		return name
	}
	hostname, _ := v["hostname"].(string)
	if strings.TrimSpace(hostname) != "" {
		return hostname
	}
	href, _ := v["href"].(string)
	if strings.TrimSpace(href) != "" {
		return href
	}
	return "unknown"
}

func venDisplayWithReason(v map[string]interface{}) string {
	name := venDisplayName(v)
	reason := strings.TrimSpace(venHealthReason(v))
	if reason == "" {
		reason = "No reason returned by API"
	}
	return fmt.Sprintf("%s - %s", name, reason)
}

func venHealthReason(v map[string]interface{}) string {
	raw, ok := v["conditions"]
	if !ok || raw == nil {
		return ""
	}
	conditions, ok := raw.([]interface{})
	if !ok || len(conditions) == 0 {
		return ""
	}
	parts := make([]string, 0, 2)
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		nt := venMapString(cond, "notification_type")
		latestRaw := cond["latest_event"]
		latest := ""
		switch evt := latestRaw.(type) {
		case string:
			latest = strings.TrimSpace(evt)
		case map[string]interface{}:
			if nt == "" {
				nt = venMapString(evt, "notification_type")
			}
			latest = firstNonEmpty(
				venMapString(evt, "description"),
				venMapString(evt, "message"),
				venMapString(evt, "notification_type"),
				venMapString(evt, "event_type"),
				venMapString(evt, "type"),
				venMapString(evt, "name"),
				venMapString(evt, "reason"),
			)
		}
		if nt != "" && latest != "" {
			if nt == latest {
				parts = append(parts, nt)
			} else {
				parts = append(parts, nt+": "+latest)
			}
		} else if nt != "" {
			parts = append(parts, nt+": "+latest)
		} else if latest != "" {
			parts = append(parts, latest)
		}
		if len(parts) == 0 {
			if first := firstNonEmpty(
				venMapString(cond, "notification_type"),
				venMapString(cond, "latest_event"),
				venMapString(cond, "first_reported_timestamp"),
			); first != "" {
				parts = append(parts, first)
			}
		}
		if len(parts) >= 2 {
			break
		}
	}
	return strings.TrimSpace(strings.Join(parts, " | "))
}

func venMapString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return strings.TrimSpace(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func isManagedWorkload(w map[string]interface{}) bool {
	if v, ok := w["unmanaged"].(bool); ok && v {
		return false
	}
	if v, ok := w["managed"].(bool); ok {
		return v
	}
	if v, ok := w["ven"].(bool); ok {
		return v
	}
	if agent, ok := w["agent"].(map[string]interface{}); ok && len(agent) > 0 {
		return true
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getIllumioStats() DashboardStats {
	var stats DashboardStats
	nowUTC := time.Now().UTC()
	stats.Timestamp = nowUTC
	stats.Workloads.EnforcementModes = make(map[string]int)
	stats.Workloads.ModeMembers = make(map[string][]string)
	stats.Blocked.Targets = make([]BlockedTargetResult, 0)

	configMutex.RLock()
	pceURL := config.PCEURL
	orgID := config.OrgID
	configMutex.RUnlock()

	baseURL := fmt.Sprintf("%s/api/v2/orgs/%s", strings.TrimSuffix(pceURL, "/"), orgID)

	workloads, wErr := getAllWorkloads(baseURL)
	if len(workloads) == 0 && wErr != nil {
		stats.Workloads.Status = FetchStatus{Success: false, Error: "Workloads: " + wErr.Error()}
	} else {
		if wErr != nil {
			stats.Workloads.Status = FetchStatus{Success: true, Error: "Workloads partial: " + wErr.Error()}
		} else {
			stats.Workloads.Status = FetchStatus{Success: true}
		}
		stats.Workloads.Total = len(workloads)
		log.Printf("[COLLECTOR] Workloads total=%d err=%v", len(workloads), wErr)
		for _, w := range workloads {
			managed := isManagedWorkload(w)
			mode, _ := w["enforcement_mode"].(string)
			if mode == "" {
				mode = "unknown"
			}

			name, _ := w["name"].(string)
			if name == "" {
				name, _ = w["hostname"].(string)
			}
			if name == "" {
				name = "unknown"
			}
			if managed {
				stats.Workloads.EnforcementModes[mode]++
				stats.Workloads.ModeMembers[mode] = append(stats.Workloads.ModeMembers[mode], name)
			} else {
				stats.Workloads.Unmanaged++
				stats.Workloads.ModeMembers["unmanaged"] = append(stats.Workloads.ModeMembers["unmanaged"], name)
			}
		}
	}

	warningVENs, warnErr := getAllVENsByHealth(baseURL, "warning")
	errorVENs, errErr := getAllVENsByHealth(baseURL, "error")
	log.Printf("[COLLECTOR] VEN warning=%d error=%d warnErr=%v errErr=%v", len(warningVENs), len(errorVENs), warnErr, errErr)
	for _, v := range warningVENs {
		stats.VENStatus.Warning = append(stats.VENStatus.Warning, venDisplayWithReason(v))
	}
	for _, v := range errorVENs {
		stats.VENStatus.Error = append(stats.VENStatus.Error, venDisplayWithReason(v))
	}
	if warnErr != nil || errErr != nil {
		errs := make([]string, 0, 2)
		if warnErr != nil {
			errs = append(errs, "warning query: "+warnErr.Error())
		}
		if errErr != nil {
			errs = append(errs, "error query: "+errErr.Error())
		}
		stats.VENStatus.Status = FetchStatus{Success: false, Error: "VENs partial: " + strings.Join(errs, " | ")}
	} else {
		stats.VENStatus.Status = FetchStatus{Success: true}
	}
	windowStart, windowEnd, baseline := collectionWindow(nowUTC)
	stats.Collection.WindowStart = windowStart
	stats.Collection.WindowEnd = windowEnd
	stats.Collection.Mode = "rolling_24h"
	tamperCount, tamperNames, tErr := getTamperingWindow(baseURL, windowStart, windowEnd)
	log.Printf("[COLLECTOR] Tampering count=%d workloads=%d err=%v", tamperCount, len(tamperNames), tErr)
	if tErr != nil && tamperCount == 0 {
		stats.Tampering.Status = FetchStatus{Success: false, Error: "Events: " + tErr.Error()}
	} else if tErr != nil {
		stats.Tampering.Status = FetchStatus{Success: true, Error: "Events partial: " + tErr.Error()}
	} else {
		stats.Tampering.Status = FetchStatus{Success: true}
	}

	targets := configuredTrafficTargets()
	warningParts := make([]string, 0)
	exclusions := configuredSourceExclusions()
	excludedHRefs, exclusionWarn := resolveSourceExclusionHRefs(baseURL, exclusions)
	if exclusionWarn != "" {
		warningParts = append(warningParts, exclusionWarn)
	}
	blockedDeltaStart := blockedDeltaWindowStart(nowUTC)
	blockedCurrent := make(map[string]int, len(targets))
	blockedBaseline := make(map[string]int)
	newlyBaselined := make(map[string]bool)
	successCount := 0

	for _, target := range targets {
		count := 0
		warning := ""
		err := error(nil)
		if baseline || !hasTargetBaseline(target.Name) {
			var qRes trafficQueryResult
			qRes, err = getBlockedCountForTargetWindow(baseURL, target, nowUTC.Add(-24*time.Hour), nowUTC, excludedHRefs)
			count = qRes.Count
			warning = qRes.Warning
			if err == nil {
				blockedBaseline[target.Name] = count
				newlyBaselined[target.Name] = true
			}
		} else {
			var qRes trafficQueryResult
			qRes, err = getBlockedCountForTargetWindow(baseURL, target, blockedDeltaStart, nowUTC, excludedHRefs)
			count = qRes.Count
			warning = qRes.Warning
		}
		result := BlockedTargetResult{Name: target.Name, Kind: target.Kind, Count: count}
		if err != nil {
			result.Status = FetchStatus{Success: false, Error: err.Error()}
			warningParts = append(warningParts, fmt.Sprintf("%s: %s", target.Name, err.Error()))
		} else {
			result.Status = FetchStatus{Success: true}
			if warning != "" {
				result.Warning = warning
				warningParts = append(warningParts, fmt.Sprintf("%s: %s", target.Name, warning))
			}
			successCount++
			if !newlyBaselined[target.Name] {
				blockedCurrent[target.Name] = count
			}
		}
		stats.Blocked.Targets = append(stats.Blocked.Targets, result)
	}
	log.Printf("[COLLECTOR] Blocked targets=%d successful=%d warnings=%d", len(targets), successCount, len(warningParts))

	_, rollingWorkloads, rollingBlocked, baselineBlocked, incrementalBlocked, warmupByTarget, warmupMinutesByTarget := updateRollingAndBuildView(
		nowUTC,
		baseline,
		len(stats.VENStatus.Warning),
		len(stats.VENStatus.Error),
		stats.Workloads.EnforcementModes["idle"],
		stats.Workloads.EnforcementModes["visibility_only"],
		stats.Workloads.EnforcementModes["selective"],
		stats.Workloads.EnforcementModes["full"],
		stats.Workloads.Unmanaged,
		tErr == nil,
		tamperCount,
		tamperNames,
		blockedCurrent,
		blockedBaseline,
		targets,
	)
	stats.Tampering.Workloads = rollingWorkloads
	stats.Tampering.Count = len(rollingWorkloads)
	updateVENDailyHistory(nowUTC,
		len(stats.VENStatus.Warning),
		len(stats.VENStatus.Error),
		stats.Tampering.Count,
		stats.Workloads.EnforcementModes["idle"],
		stats.Workloads.EnforcementModes["visibility_only"],
		stats.Workloads.EnforcementModes["selective"],
		stats.Workloads.EnforcementModes["full"],
		stats.Workloads.Unmanaged,
	)
	pruneVENHistory(nowUTC, configuredHistoryDays())

	anyWarmup := false
	defaultMAWindow := configuredBlockedMAWindow()
	defaultAnomalyPct := configuredBlockedAnomalyPct()
	defaultBaselineSource := configuredBlockedAnomalyBaselineSource()
	defaultBaselineDays := configuredBlockedAnomalyBaselineDays()
	defaultMinCoverage := configuredBlockedAnomalyMinCoveragePct()
	targetCfgByName := make(map[string]TrafficTarget, len(targets))
	for _, t := range targets {
		targetCfgByName[strings.ToLower(strings.TrimSpace(t.Name))] = t
	}
	for i := range stats.Blocked.Targets {
		name := stats.Blocked.Targets[i].Name
		if v, ok := rollingBlocked[name]; ok {
			stats.Blocked.Targets[i].Count = v
		}
		if warmupByTarget[name] {
			stats.Blocked.Targets[i].Warmup = true
			stats.Blocked.Targets[i].Baseline24h = baselineBlocked[name]
			stats.Blocked.Targets[i].IncrementalCount = incrementalBlocked[name]
			stats.Blocked.Targets[i].IncrementalMins = warmupMinutesByTarget[name]
			anyWarmup = true
		}
		series := blockedTrendSeries(name)
		maWindow := defaultMAWindow
		anomalyPct := defaultAnomalyPct
		if t, ok := targetCfgByName[strings.ToLower(strings.TrimSpace(name))]; ok {
			maWindow, anomalyPct = effectiveBlockedAnomalySettingsForTarget(t, defaultMAWindow, defaultAnomalyPct)
		}
		dailySeries := blockedDailyTrendSeries(name, configuredHistoryDays())
		eval := blockedAnomalyFromConfig(series, dailySeries, maWindow, anomalyPct, defaultBaselineSource, defaultBaselineDays, defaultMinCoverage)
		stats.Blocked.Targets[i].Latest5m = eval.Latest
		stats.Blocked.Targets[i].MovingAvg5m = eval.Baseline
		stats.Blocked.Targets[i].Anomalous = eval.Anomalous
		stats.Blocked.Targets[i].AnomalyWindow = eval.Window
		stats.Blocked.Targets[i].AnomalyPct = anomalyPct
		stats.Blocked.Targets[i].AnomalySource = eval.Source
		stats.Blocked.Targets[i].AnomalyCoverage = eval.CoveragePct
		if eval.Reason != "" {
			stats.Blocked.Targets[i].AnomalyReason = eval.Reason
		}
	}
	stats.Collection.Warmup = anyWarmup
	if baseline {
		stats.Collection.Mode = "baseline_24h"
	} else if anyWarmup {
		stats.Collection.Mode = "warmup_5m"
	} else {
		stats.Collection.Mode = "rolling_24h"
	}

	for _, t := range stats.Blocked.Targets {
		nameUpper := strings.ToUpper(strings.TrimSpace(t.Name))
		switch nameUpper {
		case "LG-E-PROD-ENVS":
			stats.Blocked.PROD = t.Count
			stats.Blocked.PRODStatus = t.Status
		case "LG-E-NONPROD-ENVS", "LG-E_NONPROD-ENVS":
			stats.Blocked.NONPROD = t.Count
			stats.Blocked.NONPRODStatus = t.Status
		}
	}
	if stats.Blocked.PRODStatus == (FetchStatus{}) {
		stats.Blocked.PRODStatus = FetchStatus{Success: false, Error: "Target not configured"}
	}
	if stats.Blocked.NONPRODStatus == (FetchStatus{}) {
		stats.Blocked.NONPRODStatus = FetchStatus{Success: false, Error: "Target not configured"}
	}

	if len(targets) == 0 || successCount == 0 {
		stats.Blocked.Status = FetchStatus{Success: false, Error: strings.Join(warningParts, " | ")}
	} else if successCount < len(targets) {
		stats.Blocked.Partial = true
		stats.Blocked.Warning = strings.Join(warningParts, " | ")
		stats.Blocked.Status = FetchStatus{Success: true, Error: stats.Blocked.Warning}
	} else {
		stats.Blocked.Status = FetchStatus{Success: true}
	}
	collectDailyBlockedHistory(baseURL, targets, nowUTC, excludedHRefs)

	return stats
}

func collectionWindow(nowUTC time.Time) (time.Time, time.Time, bool) {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	if !rollingCache.Initialized {
		return nowUTC.Add(-24 * time.Hour), nowUTC, true
	}
	start := rollingCache.LastCycle
	if start.IsZero() || !start.Before(nowUTC) {
		start = nowUTC.Add(-5 * time.Minute)
	}
	if nowUTC.Sub(start) > 30*time.Minute {
		start = nowUTC.Add(-5 * time.Minute)
	}
	return start, nowUTC, false
}

func blockedDeltaWindowStart(nowUTC time.Time) time.Time {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	start := rollingCache.LastCycle
	if start.IsZero() || !start.Before(nowUTC) {
		return nowUTC.Add(-5 * time.Minute)
	}
	if nowUTC.Sub(start) > 30*time.Minute {
		return nowUTC.Add(-5 * time.Minute)
	}
	return start
}

func hasTargetBaseline(targetName string) bool {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	_, ok := rollingCache.BaselineBlocked[targetName]
	return ok
}

func updateRollingAndBuildView(
	nowUTC time.Time,
	baseline bool,
	venWarningCount int,
	venErrorCount int,
	modeIdleCount int,
	modeVisCount int,
	modeSelectiveCount int,
	modeFullCount int,
	modeUnmanagedCount int,
	tamperingOK bool,
	tamperingCount int,
	tamperingNames []string,
	blockedCurrent map[string]int,
	blockedBaseline map[string]int,
	targets []TrafficTarget,
) (int, []string, map[string]int, map[string]int, map[string]int, map[string]bool, map[string]int) {
	rollingMu.Lock()
	defer rollingMu.Unlock()

	if baseline || !rollingCache.Initialized {
		rollingCache = rollingState{
			Initialized:         true,
			LastCycle:           nowUTC,
			BaselineCapturedUTC: nowUTC,
			BaselineWorkloads:   map[string]struct{}{},
			BaselineBlocked:     map[string]targetBaseline{},
			Buckets:             []rollingBucket{},
		}
		if tamperingOK {
			rollingCache.BaselineTampering = tamperingCount
			for _, n := range tamperingNames {
				rollingCache.BaselineWorkloads[n] = struct{}{}
			}
		}
		for name, count := range blockedBaseline {
			rollingCache.BaselineBlocked[name] = targetBaseline{Count: count, CapturedUTC: nowUTC}
		}
		rollingCache.Buckets = append(rollingCache.Buckets, rollingBucket{
			EndUTC:             nowUTC,
			VENWarningCount:    venWarningCount,
			VENErrorCount:      venErrorCount,
			ModeIdleCount:      modeIdleCount,
			ModeVisCount:       modeVisCount,
			ModeSelectiveCount: modeSelectiveCount,
			ModeFullCount:      modeFullCount,
			ModeUnmanagedCount: modeUnmanagedCount,
			TamperingCount:     0,
			TamperingWorkloads: map[string]struct{}{},
			BlockedByTarget:    map[string]int{},
		})
	} else {
		rollingCache.LastCycle = nowUTC
		bucket := rollingBucket{
			EndUTC:             nowUTC,
			VENWarningCount:    venWarningCount,
			VENErrorCount:      venErrorCount,
			ModeIdleCount:      modeIdleCount,
			ModeVisCount:       modeVisCount,
			ModeSelectiveCount: modeSelectiveCount,
			ModeFullCount:      modeFullCount,
			ModeUnmanagedCount: modeUnmanagedCount,
			TamperingCount:     0,
			TamperingWorkloads: map[string]struct{}{},
			BlockedByTarget:    map[string]int{},
		}
		if tamperingOK {
			bucket.TamperingCount = tamperingCount
			for _, n := range tamperingNames {
				bucket.TamperingWorkloads[n] = struct{}{}
			}
		}
		for k, v := range blockedCurrent {
			bucket.BlockedByTarget[k] = v
		}
		rollingCache.Buckets = append(rollingCache.Buckets, bucket)
		for name, count := range blockedBaseline {
			rollingCache.BaselineBlocked[name] = targetBaseline{Count: count, CapturedUTC: nowUTC}
		}
	}

	cutoff := nowUTC.Add(-24 * time.Hour)
	kept := make([]rollingBucket, 0, len(rollingCache.Buckets))
	for _, b := range rollingCache.Buckets {
		if b.EndUTC.After(cutoff) {
			kept = append(kept, b)
		}
	}
	rollingCache.Buckets = kept

	useBaseline := cutoff.Before(rollingCache.BaselineCapturedUTC)
	totalTampering := 0
	workloads := map[string]struct{}{}
	blockedRollingTotals := map[string]int{}
	blockedBaselineTotals := map[string]int{}
	blockedIncrementalTotals := map[string]int{}
	blockedWarmup := map[string]bool{}
	blockedWarmupMinutes := map[string]int{}

	if useBaseline {
		totalTampering += rollingCache.BaselineTampering
		for n := range rollingCache.BaselineWorkloads {
			workloads[n] = struct{}{}
		}
	}

	for _, b := range rollingCache.Buckets {
		totalTampering += b.TamperingCount
		for n := range b.TamperingWorkloads {
			workloads[n] = struct{}{}
		}
	}

	for _, t := range targets {
		name := t.Name
		baselineEntry, hasBaseline := rollingCache.BaselineBlocked[name]
		rolling24 := 0
		incremental := 0
		for _, b := range rollingCache.Buckets {
			v := b.BlockedByTarget[name]
			rolling24 += v
			if hasBaseline && b.EndUTC.After(baselineEntry.CapturedUTC) {
				incremental += v
			}
		}
		if hasBaseline && cutoff.Before(baselineEntry.CapturedUTC) {
			blockedWarmup[name] = true
			blockedBaselineTotals[name] = baselineEntry.Count
			blockedIncrementalTotals[name] = incremental
			elapsed := nowUTC.Sub(baselineEntry.CapturedUTC)
			mins := int(elapsed / time.Minute)
			if mins > 0 {
				mins = (mins / 5) * 5
				if mins == 0 {
					mins = 5
				}
			}
			if mins > 24*60 {
				mins = 24 * 60
			}
			blockedWarmupMinutes[name] = mins
			blockedRollingTotals[name] = baselineEntry.Count
		} else {
			blockedRollingTotals[name] = rolling24
		}
	}

	w := make([]string, 0, len(workloads))
	for n := range workloads {
		w = append(w, n)
	}
	sort.Strings(w)
	return totalTampering, w, blockedRollingTotals, blockedBaselineTotals, blockedIncrementalTotals, blockedWarmup, blockedWarmupMinutes
}

func getTamperingWindow(baseURL string, startUTC, endUTC time.Time) (int, []string, error) {
	events, err := fetchTamperingEventsPaged(baseURL, startUTC, endUTC)
	if len(events) == 0 && err != nil {
		return 0, nil, err
	}

	dedup := map[string]struct{}{}
	for _, evt := range events {
		if name := extractEventWorkloadName(evt); name != "" {
			dedup[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(dedup))
	for n := range dedup {
		names = append(names, n)
	}
	sort.Strings(names)
	return len(events), names, err
}

func fetchTamperingEventsPaged(baseURL string, startUTC, endUTC time.Time) ([]map[string]interface{}, error) {
	const pageSize = 1000
	all := make([]map[string]interface{}, 0, pageSize)
	started := time.Now()
	lastPageSig := ""

	for page := 0; ; page++ {
		if time.Since(started) > tamperingQueryMaxDuration {
			break
		}
		q := url.Values{}
		q.Set("event_type", "agent.tampering")
		q.Set("start_date", startUTC.Format(pceTimeFormat))
		q.Set("end_date", endUTC.Format(pceTimeFormat))
		q.Set("max_results", strconv.Itoa(pageSize))
		q.Set("skip", strconv.Itoa(page*pageSize))
		batch, err := fetchCollectionPage(baseURL + "/events?" + q.Encode())
		if err != nil {
			q2 := url.Values{}
			q2.Set("event_type", "agent.tampering")
			q2.Set("start_time", startUTC.Format(pceTimeFormat))
			q2.Set("end_time", endUTC.Format(pceTimeFormat))
			q2.Set("max_results", strconv.Itoa(pageSize))
			q2.Set("skip", strconv.Itoa(page*pageSize))
			batch, err = fetchCollectionPage(baseURL + "/events?" + q2.Encode())
			if err != nil {
				if page == 0 {
					return fetchTamperingEventsBySlices(baseURL, startUTC, endUTC, time.Hour)
				}
				break
			}
		}
		if len(batch) == 0 {
			break
		}

		pageSig := tamperingPageSignature(batch)
		if page > 0 && pageSig != "" && pageSig == lastPageSig {
			break
		}
		lastPageSig = pageSig

		inWindow := 0
		oldest := time.Now().UTC()
		oldestSet := false
		for _, evt := range batch {
			ts, ok := eventTimestampUTC(evt)
			if ok {
				if !oldestSet || ts.Before(oldest) {
					oldest = ts
					oldestSet = true
				}
				if ts.Before(startUTC) || ts.After(endUTC) {
					continue
				}
			}
			inWindow++
			all = append(all, evt)
		}
		if inWindow == 0 && oldestSet && oldest.Before(startUTC) {
			break
		}
		if len(batch) < pageSize {
			break
		}
	}
	if len(all) == 0 {
		return fetchTamperingEventsBySlices(baseURL, startUTC, endUTC, time.Hour)
	}
	return all, nil
}

func fetchTamperingEventsBySlices(baseURL string, startUTC, endUTC time.Time, step time.Duration) ([]map[string]interface{}, error) {
	if step <= 0 {
		step = time.Hour
	}
	all := make([]map[string]interface{}, 0, 2000)
	seen := map[string]struct{}{}
	for cur := startUTC; cur.Before(endUTC); cur = cur.Add(step) {
		next := cur.Add(step)
		if next.After(endUTC) {
			next = endUTC
		}
		batch, err := fetchTamperingSlice(baseURL, cur, next)
		if err != nil {
			if len(all) == 0 {
				return nil, err
			}
			return all, fmt.Errorf("partial tampering slice result: %v", err)
		}
		for _, evt := range batch {
			sig := tamperingEventSignature(evt)
			if _, ok := seen[sig]; ok {
				continue
			}
			seen[sig] = struct{}{}
			all = append(all, evt)
		}
	}
	return all, nil
}

func fetchTamperingSlice(baseURL string, startUTC, endUTC time.Time) ([]map[string]interface{}, error) {
	q := url.Values{}
	q.Set("event_type", "agent.tampering")
	q.Set("start_date", startUTC.Format(pceTimeFormat))
	q.Set("end_date", endUTC.Format(pceTimeFormat))
	q.Set("max_results", "200000")
	batch, err := fetchCollectionPage(baseURL + "/events?" + q.Encode())
	if err == nil {
		return batch, nil
	}
	q2 := url.Values{}
	q2.Set("event_type", "agent.tampering")
	q2.Set("start_time", startUTC.Format(pceTimeFormat))
	q2.Set("end_time", endUTC.Format(pceTimeFormat))
	q2.Set("max_results", "200000")
	return fetchCollectionPage(baseURL + "/events?" + q2.Encode())
}

func tamperingEventSignature(event map[string]interface{}) string {
	return strings.Join([]string{
		mapString(event, "href"),
		mapString(event, "event_type"),
		findTimestampString(event),
		extractEventWorkloadName(event),
	}, "|")
}

func tamperingPageSignature(batch []map[string]interface{}) string {
	if len(batch) == 0 {
		return ""
	}
	first := batch[0]
	last := batch[len(batch)-1]
	f := fmt.Sprintf("%s|%s|%s", mapString(first, "href"), mapString(first, "event_type"), mapString(first, "timestamp"))
	l := fmt.Sprintf("%s|%s|%s", mapString(last, "href"), mapString(last, "event_type"), mapString(last, "timestamp"))
	return f + "->" + l
}

func mapString(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

func eventTimestampUTC(event map[string]interface{}) (time.Time, bool) {
	if ts := findTimestampString(event); ts != "" {
		if t, err := parseFlexibleTime(ts); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func findTimestampString(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, k := range []string{"timestamp", "event_timestamp", "created_at", "time", "occurred_at", "start_time"} {
			if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		for _, child := range t {
			if s := findTimestampString(child); s != "" {
				return s
			}
		}
	case []interface{}:
		for _, child := range t {
			if s := findTimestampString(child); s != "" {
				return s
			}
		}
	}
	return ""
}

func parseFlexibleTime(raw string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		pceTimeFormat,
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05Z07:00",
	}
	var last error
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		} else {
			last = err
		}
	}
	return time.Time{}, last
}

func configuredTrafficTargets() []TrafficTarget {
	configMutex.RLock()
	rawTargets := append([]TrafficTarget(nil), config.TrafficTargets...)
	configMutex.RUnlock()
	cleaned := sanitizeTargets(rawTargets)
	if len(cleaned) > 0 {
		return cleaned
	}
	return []TrafficTarget{{Name: "LG-E-PROD-ENVS", Kind: "auto"}, {Name: "LG-E-NONPROD-ENVS", Kind: "auto"}}
}

func configuredSourceExclusions() []TrafficTarget {
	configMutex.RLock()
	raw := append([]TrafficTarget(nil), config.SourceExclusions...)
	configMutex.RUnlock()
	cleaned := sanitizeTargets(raw)
	if len(cleaned) > 0 {
		return cleaned
	}
	return []TrafficTarget{{Name: "LG-SCANNERS", Kind: "auto"}}
}

func configuredHistoryDays() int {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredHistoryDaysLocked()
}

func configuredHistoryDaysLocked() int {
	days := config.HistoryDays
	if days <= 0 {
		return 365
	}
	if days > maxHistoryDays {
		return maxHistoryDays
	}
	return days
}

func configuredBlockedPortDailyEnabled() bool {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredBlockedPortDailyEnabledLocked()
}

func configuredBlockedPortDailyEnabledLocked() bool {
	if config.BlockedPortDailyEnabled == nil {
		return true
	}
	return *config.BlockedPortDailyEnabled
}

func configuredBlockedMAWindow() int {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredBlockedMAWindowLocked()
}

func configuredBlockedMAWindowLocked() int {
	return normalizeMAWindow(config.BlockedMAWindow, 12)
}

func configuredDailyMAWindow() int {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredDailyMAWindowLocked()
}

func configuredDailyMAWindowLocked() int {
	base := normalizeAnomalyBaselineDays(config.BlockedAnomalyDays, 7)
	if config.DailyMAWindow > 0 {
		return normalizeAnomalyBaselineDays(config.DailyMAWindow, base)
	}
	return base
}

func configuredBlockedAnomalyPct() float64 {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredBlockedAnomalyPctLocked()
}

func configuredBlockedAnomalyPctLocked() float64 {
	return normalizeAnomalyPct(config.BlockedAnomalyPct, 50.0)
}

func configuredBlockedAnomalyBaselineSource() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredBlockedAnomalyBaselineSourceLocked()
}

func configuredBlockedAnomalyBaselineSourceLocked() string {
	return normalizeAnomalyBaselineSource(config.BlockedAnomalyBaseline)
}

func configuredBlockedAnomalyBaselineDays() int {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredBlockedAnomalyBaselineDaysLocked()
}

func configuredBlockedAnomalyBaselineDaysLocked() int {
	return normalizeAnomalyBaselineDays(config.BlockedAnomalyDays, 7)
}

func configuredBlockedAnomalyMinCoveragePct() float64 {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredBlockedAnomalyMinCoveragePctLocked()
}

func configuredBlockedAnomalyMinCoveragePctLocked() float64 {
	return normalizeCoveragePct(config.BlockedAnomalyMinPct, 70.0)
}

func configuredVENMAWindow() int {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredVENMAWindowLocked()
}

func configuredVENMAWindowLocked() int {
	base := configuredBlockedMAWindowLocked()
	if config.VENMAWindow > 0 {
		return normalizeMAWindow(config.VENMAWindow, base)
	}
	return base
}

func configuredVENAnomalyPct() float64 {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredVENAnomalyPctLocked()
}

func configuredVENAnomalyPctLocked() float64 {
	base := configuredBlockedAnomalyPctLocked()
	if config.VENAnomalyPct > 0 {
		return normalizeAnomalyPct(config.VENAnomalyPct, base)
	}
	return base
}

func configuredVENAnomalyBaselineSource() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredVENAnomalyBaselineSourceLocked()
}

func configuredVENAnomalyBaselineSourceLocked() string {
	return normalizeAnomalyBaselineSource(config.VENAnomalyBaseline)
}

func configuredVENAnomalyBaselineDays() int {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredVENAnomalyBaselineDaysLocked()
}

func configuredVENAnomalyBaselineDaysLocked() int {
	base := configuredBlockedAnomalyBaselineDaysLocked()
	if config.VENAnomalyDays > 0 {
		return normalizeAnomalyBaselineDays(config.VENAnomalyDays, base)
	}
	return base
}

func configuredVENAnomalyMinCoveragePct() float64 {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredVENAnomalyMinCoveragePctLocked()
}

func configuredVENAnomalyMinCoveragePctLocked() float64 {
	base := configuredBlockedAnomalyMinCoveragePctLocked()
	if config.VENAnomalyMinPct > 0 {
		return normalizeCoveragePct(config.VENAnomalyMinPct, base)
	}
	return base
}

func configuredTamperingMAWindow() int {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredTamperingMAWindowLocked()
}

func configuredTamperingMAWindowLocked() int {
	base := configuredBlockedMAWindowLocked()
	if config.TamperingMAWindow > 0 {
		return normalizeMAWindow(config.TamperingMAWindow, base)
	}
	return base
}

func configuredTamperingAnomalyPct() float64 {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredTamperingAnomalyPctLocked()
}

func configuredTamperingAnomalyPctLocked() float64 {
	base := configuredBlockedAnomalyPctLocked()
	if config.TamperingAnomalyPct > 0 {
		return normalizeAnomalyPct(config.TamperingAnomalyPct, base)
	}
	return base
}

func configuredTamperingAnomalyBaselineSource() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredTamperingAnomalyBaselineSourceLocked()
}

func configuredTamperingAnomalyBaselineSourceLocked() string {
	if strings.TrimSpace(config.TamperingAnomalyBaseline) == "" {
		return "daily"
	}
	return normalizeAnomalyBaselineSource(config.TamperingAnomalyBaseline)
}

func configuredTamperingAnomalyBaselineDays() int {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredTamperingAnomalyBaselineDaysLocked()
}

func configuredTamperingAnomalyBaselineDaysLocked() int {
	base := configuredBlockedAnomalyBaselineDaysLocked()
	if config.TamperingAnomalyDays > 0 {
		return normalizeAnomalyBaselineDays(config.TamperingAnomalyDays, base)
	}
	return base
}

func configuredTamperingAnomalyMinCoveragePct() float64 {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredTamperingAnomalyMinCoveragePctLocked()
}

func configuredTamperingAnomalyMinCoveragePctLocked() float64 {
	base := configuredBlockedAnomalyMinCoveragePctLocked()
	if config.TamperingAnomalyMinPct > 0 {
		return normalizeCoveragePct(config.TamperingAnomalyMinPct, base)
	}
	return base
}

func configuredTamperingDailyAnomalyPct() float64 {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredTamperingDailyAnomalyPctLocked()
}

func configuredTamperingDailyAnomalyPctLocked() float64 {
	base := configuredTamperingAnomalyPctLocked()
	if config.TamperingDailyAnomalyPct > 0 {
		return normalizeAnomalyPct(config.TamperingDailyAnomalyPct, base)
	}
	return base
}

func configuredTimezone() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredTimezoneLocked()
}

func configuredTimezoneLocked() string {
	return normalizeTimezone(config.Timezone)
}

func configuredEffectiveTimezone() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredEffectiveTimezoneLocked()
}

func configuredEffectiveTimezoneLocked() string {
	return configuredDayLocationLocked().String()
}

func configuredDayLocation() *time.Location {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredDayLocationLocked()
}

func configuredDayLocationLocked() *time.Location {
	return loadTimezoneOrLocal(config.Timezone)
}

func configuredBindAddress() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredBindAddressLocked()
}

func configuredBindAddressLocked() string {
	return normalizeBindAddress(config.BindAddress)
}

func configuredPublicBaseURL() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredPublicBaseURLLocked()
}

func configuredPublicBaseURLLocked() string {
	return normalizePublicBaseURL(config.PublicBaseURL)
}

func configuredWebhookEnabled() bool {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredWebhookEnabledLocked()
}

func configuredWebhookEnabledLocked() bool {
	return config.WebhookEnabled && strings.TrimSpace(config.WebhookURL) != ""
}

func configuredWebhookURL() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return strings.TrimSpace(config.WebhookURL)
}

func configuredWebhookProvider() string {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configuredWebhookProviderLocked()
}

func configuredWebhookProviderLocked() string {
	return normalizeWebhookProvider(config.WebhookProvider)
}

func normalizeWebhookProvider(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "slack":
		return "slack"
	case "teams":
		return "teams"
	default:
		return "generic"
	}
}

func normalizeBindAddress(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return defaultBindAddress
	}
	if strings.HasPrefix(s, ":") {
		return s
	}
	if strings.EqualFold(s, "localhost") {
		return "127.0.0.1:18443"
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		host = strings.TrimSpace(host)
		port = strings.TrimSpace(port)
		if host == "" {
			host = "0.0.0.0"
		}
		if port == "" {
			port = "18443"
		}
		return net.JoinHostPort(host, port)
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		ipv6Host := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
		return net.JoinHostPort(ipv6Host, "18443")
	}
	if ip := net.ParseIP(s); ip != nil {
		return net.JoinHostPort(s, "18443")
	}
	return net.JoinHostPort(s, "18443")
}

func normalizePublicBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return defaultPublicBaseURL
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || strings.TrimSpace(u.Host) == "" {
		return defaultPublicBaseURL
	}
	if strings.TrimSpace(u.Scheme) == "" {
		u.Scheme = "http"
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	base := strings.TrimRight(strings.TrimSpace(u.String()), "/")
	if base == "" {
		return defaultPublicBaseURL
	}
	return base
}

func normalizeTimezone(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if _, err := time.LoadLocation(s); err != nil {
		log.Printf("[CONFIG] invalid timezone %q; defaulting to system local time", s)
		return ""
	}
	return s
}

func loadTimezoneOrLocal(raw string) *time.Location {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		return time.Local
	}
	return loc
}

func localDayStart(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
}

func parseDayKeyInLocation(day string, loc *time.Location) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", day, loc)
}

type webhookFormatOptions struct {
	Provider         string
	SlackChannel     string
	SlackUsername    string
	SlackIconEmoji   string
	TeamsTitlePrefix string
}

func configuredWebhookFormatOptions() webhookFormatOptions {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return webhookFormatOptions{
		Provider:         configuredWebhookProviderLocked(),
		SlackChannel:     strings.TrimSpace(config.WebhookSlackChannel),
		SlackUsername:    strings.TrimSpace(config.WebhookSlackUsername),
		SlackIconEmoji:   strings.TrimSpace(config.WebhookSlackIconEmoji),
		TeamsTitlePrefix: strings.TrimSpace(config.WebhookTeamsTitlePrefix),
	}
}

func normalizeMAWindow(raw int, fallback int) int {
	if fallback <= 0 {
		fallback = 12
	}
	window := raw
	if window <= 0 {
		window = fallback
	}
	if window < 2 {
		window = 2
	}
	if window > 288 {
		window = 288
	}
	return window
}

func normalizeAnomalyPct(raw float64, fallback float64) float64 {
	if fallback <= 0 {
		fallback = 50.0
	}
	pct := raw
	if pct <= 0 {
		pct = fallback
	}
	if pct > 10000 {
		pct = 10000
	}
	return pct
}

func normalizeAnomalyBaselineSource(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "daily":
		return "daily"
	default:
		return "5m"
	}
}

func normalizeAnomalyBaselineDays(raw int, fallback int) int {
	if fallback <= 0 {
		fallback = 7
	}
	v := raw
	if v <= 0 {
		v = fallback
	}
	if v < 1 {
		v = 1
	}
	if v > maxHistoryDays {
		v = maxHistoryDays
	}
	return v
}

func normalizeCoveragePct(raw float64, fallback float64) float64 {
	if fallback <= 0 {
		fallback = 70
	}
	v := raw
	if v <= 0 {
		v = fallback
	}
	if v < 1 {
		v = 1
	}
	if v > 100 {
		v = 100
	}
	return v
}

func collectDailyBlockedHistory(baseURL string, targets []TrafficTarget, nowUTC time.Time, sourceExcludeHRefs []string) {
	if len(targets) == 0 {
		return
	}
	historyDays := configuredHistoryDays()
	portDailyEnabled := configuredBlockedPortDailyEnabled()
	loc := configuredDayLocation()
	dayEnd := localDayStart(nowUTC, loc)
	if dayEnd.IsZero() {
		return
	}
	// Use calendar-day math to avoid DST gaps/overlaps (23h/25h days).
	dayStart := dayEnd.AddDate(0, 0, -1)
	dayKey := dayStart.Format("2006-01-02")

	missingCounts := make([]TrafficTarget, 0)
	missingPorts := make([]TrafficTarget, 0)
	historyMu.Lock()
	for _, target := range targets {
		targetMap := blockedDaily[dayKey]
		if targetMap == nil {
			missingCounts = append(missingCounts, target)
		} else if _, ok := targetMap[target.Name]; !ok {
			missingCounts = append(missingCounts, target)
		}
		if portDailyEnabled {
			portTargetMap := blockedPortsDaily[dayKey]
			if portTargetMap == nil {
				missingPorts = append(missingPorts, target)
			} else if _, ok := portTargetMap[target.Name]; !ok {
				missingPorts = append(missingPorts, target)
			}
		}
	}
	historyMu.Unlock()
	if len(missingCounts) == 0 && len(missingPorts) == 0 {
		pruneBlockedHistory(nowUTC, historyDays)
		return
	}

	changed := false
	for _, target := range missingCounts {
		qRes, err := getBlockedCountForTargetWindow(baseURL, target, dayStart.UTC(), dayEnd.UTC(), sourceExcludeHRefs)
		if err != nil {
			log.Printf("[HISTORY] daily blocked snapshot failed for %s (%s): %v", target.Name, dayKey, err)
			continue
		}
		if qRes.Warning != "" {
			log.Printf("[HISTORY] daily blocked snapshot warning for %s (%s): %s", target.Name, dayKey, qRes.Warning)
		}
		historyMu.Lock()
		if blockedDaily[dayKey] == nil {
			blockedDaily[dayKey] = map[string]int{}
		}
		blockedDaily[dayKey][target.Name] = qRes.Count
		historyMu.Unlock()
		changed = true
	}
	for _, target := range missingPorts {
		portCounts, err := getBlockedPortCountsForTargetWindow(baseURL, target, dayStart.UTC(), dayEnd.UTC(), sourceExcludeHRefs)
		if err != nil {
			log.Printf("[HISTORY] daily blocked port snapshot failed for %s (%s): %v", target.Name, dayKey, err)
			continue
		}
		historyMu.Lock()
		if blockedPortsDaily[dayKey] == nil {
			blockedPortsDaily[dayKey] = map[string]map[string]int{}
		}
		if portCounts == nil {
			portCounts = map[string]int{}
		}
		blockedPortsDaily[dayKey][target.Name] = portCounts
		historyMu.Unlock()
		changed = true
	}
	pruneBlockedHistory(nowUTC, historyDays)
	if changed {
		saveBlockedHistory()
		saveBlockedPortHistory()
	}
}

func sanitizeTargets(targets []TrafficTarget) []TrafficTarget {
	cleaned := make([]TrafficTarget, 0, len(targets))
	for _, t := range targets {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(t.Kind))
		switch kind {
		case "", "auto", "label", "label_group":
		default:
			kind = "auto"
		}
		if kind == "" {
			kind = "auto"
		}
		window := 0
		if t.BlockedMAWindow > 0 {
			window = normalizeMAWindow(t.BlockedMAWindow, 12)
		}
		pct := 0.0
		if t.BlockedAnomalyPct > 0 {
			pct = normalizeAnomalyPct(t.BlockedAnomalyPct, 50.0)
		}
		cleaned = append(cleaned, TrafficTarget{
			Name:              name,
			Kind:              kind,
			BlockedMAWindow:   window,
			BlockedAnomalyPct: pct,
		})
	}
	return cleaned
}

func getBlockedCountForTargetWindow(baseURL string, target TrafficTarget, startUTC, endUTC time.Time, sourceExcludeHRefs []string) (trafficQueryResult, error) {
	labelHRefs, err := getBlockedCountTargetLabelHRefs(baseURL, target)
	if err != nil {
		return trafficQueryResult{}, err
	}
	runBothDirections := func(labelHRefs []string, queryName string) (trafficQueryResult, error) {
		sourceRes, err := performAsyncTrafficQueryWindow(baseURL, labelHRefs, sourceExcludeHRefs, queryName+"_src", startUTC, endUTC, true)
		if err != nil {
			return trafficQueryResult{}, err
		}
		destRes, err := performAsyncTrafficQueryWindow(baseURL, labelHRefs, sourceExcludeHRefs, queryName+"_dst", startUTC, endUTC, false)
		if err != nil {
			return trafficQueryResult{}, err
		}
		combined := trafficQueryResult{
			Count:     sourceRes.Count + destRes.Count,
			Truncated: sourceRes.Truncated || destRes.Truncated,
		}
		if combined.Truncated {
			combined.Warning = fmt.Sprintf("result may be truncated at max_results=%d (source and/or destination leg reached cap)", trafficQueryMaxResults)
		}
		return combined, nil
	}
	return runBothDirections(labelHRefs, target.Name)
}

func getBlockedPortCountsForTargetWindow(baseURL string, target TrafficTarget, startUTC, endUTC time.Time, sourceExcludeHRefs []string) (map[string]int, error) {
	labelHRefs, err := getBlockedCountTargetLabelHRefs(baseURL, target)
	if err != nil {
		return nil, err
	}
	sourceMap, err := performAsyncTrafficQueryWindowPortCounts(baseURL, labelHRefs, sourceExcludeHRefs, target.Name+"_ports_src", startUTC, endUTC, true)
	if err != nil {
		return nil, err
	}
	destMap, err := performAsyncTrafficQueryWindowPortCounts(baseURL, labelHRefs, sourceExcludeHRefs, target.Name+"_ports_dst", startUTC, endUTC, false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int)
	for k, v := range sourceMap {
		if v > 0 {
			out[k] += v
		}
	}
	for k, v := range destMap {
		if v > 0 {
			out[k] += v
		}
	}
	return out, nil
}

func resolveLabelHref(baseURL, targetName string) (string, error) {
	lookup := strings.TrimSpace(targetName)
	labels, err := getAllLabels(baseURL)
	if err != nil {
		return "", err
	}
	for _, l := range labels {
		if val, _ := l["value"].(string); strings.TrimSpace(val) == lookup {
			h, _ := l["href"].(string)
			if h != "" {
				return h, nil
			}
			return "", fmt.Errorf("label '%s' missing href", targetName)
		}
	}
	return "", fmt.Errorf("label '%s' not found (scanned %d labels)", targetName, len(labels))
}

func resolveLabelGroupMemberHRefs(baseURL, targetName string) ([]string, error) {
	lookup := strings.TrimSpace(targetName)
	for _, version := range []string{"active", "draft"} {
		lgURL := baseURL + "/sec_policy/" + version + "/label_groups"
		groups, err := getAllLabelGroups(lgURL)
		if err != nil {
			continue
		}
		for _, g := range groups {
			if gName, _ := g["name"].(string); strings.TrimSpace(gName) == lookup {
				h, _ := g["href"].(string)
				if h == "" {
					return nil, fmt.Errorf("group '%s' missing href", targetName)
				}
				var detail map[string]interface{}
				if err := apiCall(resolveHrefToURL(h), "GET", nil, &detail); err != nil {
					return nil, fmt.Errorf("failed to fetch group detail: %w", err)
				}
				memberHRefs, err := collectHRefs(detail, map[string]struct{}{h: {}})
				if err != nil {
					return nil, err
				}
				if len(memberHRefs) == 0 {
					return nil, fmt.Errorf("group '%s' has no members", targetName)
				}
				log.Printf("[COLLECTOR] Resolved group '%s' to %d member labels", targetName, len(memberHRefs))
				return memberHRefs, nil
			}
		}
	}
	activeCount := 0
	draftCount := 0
	if groups, err := getAllLabelGroups(baseURL + "/sec_policy/active/label_groups"); err == nil {
		activeCount = len(groups)
	}
	if groups, err := getAllLabelGroups(baseURL + "/sec_policy/draft/label_groups"); err == nil {
		draftCount = len(groups)
	}
	return nil, fmt.Errorf("group '%s' not found (scanned active=%d, draft=%d)", targetName, activeCount, draftCount)
}

func getAllLabels(baseURL string) ([]map[string]interface{}, error) {
	const pageSize = 500
	qBulk := url.Values{}
	qBulk.Set("max_results", "200000")
	bulk, bulkErr := fetchCollectionPage(baseURL + "/labels?" + qBulk.Encode())
	if bulkErr == nil && len(bulk) > 0 {
		return bulk, nil
	}

	all := make([]map[string]interface{}, 0, pageSize)
	seen := map[string]struct{}{}
	for page := 0; ; page++ {
		q := url.Values{}
		q.Set("max_results", strconv.Itoa(pageSize))
		q.Set("skip", strconv.Itoa(page*pageSize))
		batch, err := fetchCollectionPage(baseURL + "/labels?" + q.Encode())
		if err != nil {
			if page == 0 {
				if bulkErr != nil {
					return nil, bulkErr
				}
				return nil, err
			}
			break
		}
		if len(batch) == 0 {
			break
		}
		added := 0
		for _, l := range batch {
			k, _ := l["href"].(string)
			if k == "" {
				v, _ := l["value"].(string)
				t, _ := l["key"].(string)
				k = t + "|" + v
			}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			all = append(all, l)
			added++
		}
		if len(batch) < pageSize || added == 0 {
			break
		}
	}
	if len(all) == 0 && bulkErr != nil {
		return nil, bulkErr
	}
	return all, nil
}

func getAllLabelGroups(listURL string) ([]map[string]interface{}, error) {
	const pageSize = 500
	qBulk := url.Values{}
	qBulk.Set("max_results", "200000")
	bulk, bulkErr := fetchCollectionPage(listURL + "?" + qBulk.Encode())
	if bulkErr == nil && len(bulk) > 0 {
		return bulk, nil
	}

	all := make([]map[string]interface{}, 0, pageSize)
	seen := map[string]struct{}{}
	for page := 0; ; page++ {
		q := url.Values{}
		q.Set("max_results", strconv.Itoa(pageSize))
		q.Set("skip", strconv.Itoa(page*pageSize))
		batch, err := fetchCollectionPage(listURL + "?" + q.Encode())
		if err != nil {
			if page == 0 {
				if bulkErr != nil {
					return nil, bulkErr
				}
				return nil, err
			}
			break
		}
		if len(batch) == 0 {
			break
		}
		added := 0
		for _, g := range batch {
			k, _ := g["href"].(string)
			if k == "" {
				n, _ := g["name"].(string)
				k = n
			}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			all = append(all, g)
			added++
		}
		if len(batch) < pageSize || added == 0 {
			break
		}
	}
	if len(all) == 0 && bulkErr != nil {
		return nil, bulkErr
	}
	return all, nil
}

func collectHRefs(obj map[string]interface{}, visitedGroups map[string]struct{}) ([]string, error) {
	refs := make(map[string]struct{})
	if labels, ok := obj["labels"].([]interface{}); ok {
		for _, l := range labels {
			lm, ok := l.(map[string]interface{})
			if !ok {
				continue
			}
			h, ok := lm["href"].(string)
			if ok && h != "" {
				refs[h] = struct{}{}
			}
		}
	}
	if subGroups, ok := obj["sub_groups"].([]interface{}); ok {
		for _, sg := range subGroups {
			sgm, ok := sg.(map[string]interface{})
			if !ok {
				continue
			}
			sgHRef, _ := sgm["href"].(string)
			if sgHRef == "" {
				continue
			}
			if _, seen := visitedGroups[sgHRef]; seen {
				continue
			}
			visitedGroups[sgHRef] = struct{}{}
			var sgDetail map[string]interface{}
			if err := apiCall(resolveHrefToURL(sgHRef), "GET", nil, &sgDetail); err != nil {
				return nil, fmt.Errorf("failed to fetch subgroup %s: %w", sgHRef, err)
			}
			nestedRefs, err := collectHRefs(sgDetail, visitedGroups)
			if err != nil {
				return nil, err
			}
			for _, ref := range nestedRefs {
				refs[ref] = struct{}{}
			}
		}
	}
	flat := make([]string, 0, len(refs))
	for ref := range refs {
		flat = append(flat, ref)
	}
	return flat, nil
}

func resolveSourceExclusionHRefs(baseURL string, exclusions []TrafficTarget) ([]string, string) {
	hrefsSet := map[string]struct{}{}
	warnings := make([]string, 0)
	for _, ex := range exclusions {
		res, err := getBlockedCountTargetLabelHRefs(baseURL, ex)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("source exclusion %s unresolved: %v", ex.Name, err))
			continue
		}
		for _, h := range res {
			hrefsSet[h] = struct{}{}
		}
	}
	hrefs := make([]string, 0, len(hrefsSet))
	for h := range hrefsSet {
		hrefs = append(hrefs, h)
	}
	sort.Strings(hrefs)
	return hrefs, strings.Join(warnings, " | ")
}

func getBlockedCountTargetLabelHRefs(baseURL string, target TrafficTarget) ([]string, error) {
	kind := strings.ToLower(strings.TrimSpace(target.Kind))
	if kind == "" {
		kind = "auto"
	}
	switch kind {
	case "label":
		href, err := resolveLabelHref(baseURL, target.Name)
		if err != nil {
			return nil, err
		}
		return []string{href}, nil
	case "label_group":
		return resolveLabelGroupMemberHRefs(baseURL, target.Name)
	default:
		href, labelErr := resolveLabelHref(baseURL, target.Name)
		if labelErr == nil {
			return []string{href}, nil
		}
		hrefs, groupErr := resolveLabelGroupMemberHRefs(baseURL, target.Name)
		if groupErr == nil {
			return hrefs, nil
		}
		return nil, fmt.Errorf("label lookup: %v; label-group lookup: %v", labelErr, groupErr)
	}
}

func performAsyncTrafficQueryWindow(baseURL string, labelHRefs []string, sourceExcludeHRefs []string, queryName string, startUTC, endUTC time.Time, asSource bool) (trafficQueryResult, error) {
	if len(labelHRefs) == 0 {
		return trafficQueryResult{}, errors.New("cannot run blocked traffic query with empty label list")
	}
	includeList := make([]map[string]interface{}, 0, len(labelHRefs))
	seen := make(map[string]struct{}, len(labelHRefs))
	for _, h := range labelHRefs {
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		includeList = append(includeList, map[string]interface{}{"label": map[string]string{"href": h}})
	}
	if len(includeList) == 0 {
		return trafficQueryResult{}, errors.New("resolved labels are empty after normalization")
	}
	includeAny := make([]interface{}, 0, len(includeList))
	for _, item := range includeList {
		includeAny = append(includeAny, []interface{}{item})
	}
	sourceExcludes := make([]map[string]interface{}, 0, len(sourceExcludeHRefs))
	for _, h := range sourceExcludeHRefs {
		if strings.TrimSpace(h) == "" {
			continue
		}
		sourceExcludes = append(sourceExcludes, map[string]interface{}{"label": map[string]string{"href": h}})
	}
	sources := map[string]interface{}{"include": []interface{}{}, "exclude": []interface{}{}}
	destinations := map[string]interface{}{"include": []interface{}{}, "exclude": []interface{}{}}
	if asSource {
		sources["include"] = includeAny
	} else {
		destinations["include"] = includeAny
	}
	if len(sourceExcludes) > 0 {
		ex := make([]interface{}, 0, len(sourceExcludes))
		for _, e := range sourceExcludes {
			ex = append(ex, e)
		}
		sources["exclude"] = ex
	}

	payload := map[string]interface{}{
		"query_name":   fmt.Sprintf("Dash_%s_%d", queryName, time.Now().Unix()),
		"sources":      sources,
		"destinations": destinations,
		"services": map[string]interface{}{
			"include": []interface{}{},
			"exclude": []interface{}{},
		},
		"policy_decisions": []string{"blocked"},
		"start_date":       startUTC.Format(pceTimeFormat),
		"end_date":         endUTC.Format(pceTimeFormat),
		"max_results":      trafficQueryMaxResults,
	}

	var job map[string]interface{}
	if err := apiCall(baseURL+"/traffic_flows/async_queries", "POST", payload, &job); err != nil {
		return trafficQueryResult{}, err
	}
	jobHref, _ := job["href"].(string)
	if jobHref == "" {
		return trafficQueryResult{}, errors.New("async query did not return job href")
	}
	jobURL := resolveHrefToURL(jobHref)
	for i := 0; i < 60; i++ {
		var status map[string]interface{}
		if err := apiCall(jobURL, "GET", nil, &status); err != nil {
			return trafficQueryResult{}, err
		}
		s, _ := status["status"].(string)
		switch s {
		case "completed":
			if count, ok := extractResultCount(status); ok {
				return trafficQueryResult{
					Count:     count,
					Truncated: count >= trafficQueryMaxResults,
				}, nil
			}
			count, err := getAsyncQueryResultCount(jobURL)
			if err != nil {
				return trafficQueryResult{}, err
			}
			return trafficQueryResult{
				Count:     count,
				Truncated: count >= trafficQueryMaxResults,
			}, nil
		case "failed":
			return trafficQueryResult{}, fmt.Errorf("async job failed: %s", statusMessage(status))
		}
		time.Sleep(2 * time.Second)
	}
	return trafficQueryResult{}, fmt.Errorf("async job timed out")
}

func performAsyncTrafficQueryWindowPortCounts(baseURL string, labelHRefs []string, sourceExcludeHRefs []string, queryName string, startUTC, endUTC time.Time, asSource bool) (map[string]int, error) {
	if len(labelHRefs) == 0 {
		return nil, errors.New("cannot run blocked traffic query with empty label list")
	}
	includeList := make([]map[string]interface{}, 0, len(labelHRefs))
	seen := make(map[string]struct{}, len(labelHRefs))
	for _, h := range labelHRefs {
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		includeList = append(includeList, map[string]interface{}{"label": map[string]string{"href": h}})
	}
	if len(includeList) == 0 {
		return nil, errors.New("resolved labels are empty after normalization")
	}
	includeAny := make([]interface{}, 0, len(includeList))
	for _, item := range includeList {
		includeAny = append(includeAny, []interface{}{item})
	}
	sourceExcludes := make([]map[string]interface{}, 0, len(sourceExcludeHRefs))
	for _, h := range sourceExcludeHRefs {
		if strings.TrimSpace(h) == "" {
			continue
		}
		sourceExcludes = append(sourceExcludes, map[string]interface{}{"label": map[string]string{"href": h}})
	}
	sources := map[string]interface{}{"include": []interface{}{}, "exclude": []interface{}{}}
	destinations := map[string]interface{}{"include": []interface{}{}, "exclude": []interface{}{}}
	if asSource {
		sources["include"] = includeAny
	} else {
		destinations["include"] = includeAny
	}
	if len(sourceExcludes) > 0 {
		ex := make([]interface{}, 0, len(sourceExcludes))
		for _, e := range sourceExcludes {
			ex = append(ex, e)
		}
		sources["exclude"] = ex
	}
	payload := map[string]interface{}{
		"query_name":   fmt.Sprintf("Dash_%s_%d", queryName, time.Now().Unix()),
		"sources":      sources,
		"destinations": destinations,
		"services": map[string]interface{}{
			"include": []interface{}{},
			"exclude": []interface{}{},
		},
		"policy_decisions": []string{"blocked"},
		"start_date":       startUTC.Format(pceTimeFormat),
		"end_date":         endUTC.Format(pceTimeFormat),
		"max_results":      trafficQueryMaxResults,
	}
	var job map[string]interface{}
	if err := apiCall(baseURL+"/traffic_flows/async_queries", "POST", payload, &job); err != nil {
		return nil, err
	}
	jobHref, _ := job["href"].(string)
	if jobHref == "" {
		return nil, errors.New("async query did not return job href")
	}
	jobURL := resolveHrefToURL(jobHref)
	for i := 0; i < 60; i++ {
		var status map[string]interface{}
		if err := apiCall(jobURL, "GET", nil, &status); err != nil {
			return nil, err
		}
		s, _ := status["status"].(string)
		switch s {
		case "completed":
			rows, err := getAsyncQueryResultRows(jobURL)
			if err != nil {
				return nil, err
			}
			return aggregatePortCounts(rows), nil
		case "failed":
			return nil, fmt.Errorf("async job failed: %s", statusMessage(status))
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("async job timed out")
}

func getAsyncQueryResultRows(jobURL string) ([]map[string]interface{}, error) {
	for attempt := 0; attempt < 45; attempt++ {
		for _, suffix := range []string{"/download", "/results"} {
			rows, err := readAsyncResultRows(jobURL + suffix)
			if err == nil {
				return rows, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil, errors.New("async job completed but no readable result endpoint")
}

func readAsyncResultRows(url string) ([]map[string]interface{}, error) {
	body, err := apiCallRaw(url, "GET", nil)
	if err != nil {
		return nil, err
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr, nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err == nil {
		for _, k := range []string{"results", "items", "flows"} {
			items, ok := obj[k].([]interface{})
			if !ok {
				continue
			}
			out := make([]map[string]interface{}, 0, len(items))
			for _, it := range items {
				m, ok := it.(map[string]interface{})
				if ok {
					out = append(out, m)
				}
			}
			return out, nil
		}
	}
	return nil, errors.New("unexpected async result payload format")
}

func aggregatePortCounts(rows []map[string]interface{}) map[string]int {
	out := make(map[string]int)
	for _, row := range rows {
		service, _ := row["service"].(map[string]interface{})
		port := intFromAny(service["port"])
		proto := intFromAny(service["proto"])
		key := formatPortProtoKey(port, proto)
		if key == "" {
			continue
		}
		n := intFromAny(row["num_connections"])
		if n <= 0 {
			n = 1
		}
		out[key] += n
	}
	return out
}

func intFromAny(v interface{}) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(t))
		return i
	default:
		return 0
	}
}

func formatPortProtoKey(port, proto int) string {
	if port <= 0 {
		return ""
	}
	switch proto {
	case 6:
		return fmt.Sprintf("%d/tcp", port)
	case 17:
		return fmt.Sprintf("%d/udp", port)
	case 1:
		return fmt.Sprintf("%d/icmp", port)
	case 0:
		return fmt.Sprintf("%d", port)
	default:
		return fmt.Sprintf("%d/p%d", port, proto)
	}
}

func extractResultCount(status map[string]interface{}) (int, bool) {
	if count, ok := status["result_count"].(float64); ok {
		return int(count), true
	}
	if count, ok := status["result_count"].(int); ok {
		return count, true
	}
	return 0, false
}

func getAsyncQueryResultCount(jobURL string) (int, error) {
	for attempt := 0; attempt < 45; attempt++ {
		for _, suffix := range []string{"/download", "/results"} {
			count, err := countAsyncResults(jobURL + suffix)
			if err == nil {
				return count, nil
			}
			log.Printf("[COLLECTOR] async result endpoint %s failed: %v", jobURL+suffix, err)
		}
		time.Sleep(2 * time.Second)
	}
	return 0, errors.New("async job completed but no result_count and no readable result endpoint")
}

func countAsyncResults(url string) (int, error) {
	body, err := apiCallRaw(url, "GET", nil)
	if err != nil {
		return 0, err
	}
	var arr []interface{}
	if err := json.Unmarshal(body, &arr); err == nil {
		return len(arr), nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err == nil {
		for _, k := range []string{"results", "items", "flows"} {
			if items, ok := obj[k].([]interface{}); ok {
				return len(items), nil
			}
		}
	}
	return 0, errors.New("unexpected async result payload format")
}

func statusMessage(status map[string]interface{}) string {
	for _, key := range []string{"error", "message", "details"} {
		if v, ok := status[key].(string); ok && v != "" {
			return v
		}
	}
	return "unknown error"
}

func extractEventWorkloadName(event map[string]interface{}) string {
	for _, key := range []string{"workload", "affected_workload", "src_workload", "dst_workload"} {
		if nested, ok := event[key].(map[string]interface{}); ok {
			if name := mapName(nested); name != "" {
				return name
			}
		}
	}
	if name := findNameRecursive(event); name != "" {
		return name
	}
	if name := mapName(event); name != "" {
		return name
	}
	return ""
}

func findNameRecursive(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		if n := mapName(t); n != "" {
			return n
		}
		for _, val := range t {
			if n := findNameRecursive(val); n != "" {
				return n
			}
		}
	case []interface{}:
		for _, val := range t {
			if n := findNameRecursive(val); n != "" {
				return n
			}
		}
	}
	return ""
}

func mapName(obj map[string]interface{}) string {
	for _, key := range []string{"name", "hostname", "workload_name"} {
		if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func apiCall(url, method string, payload interface{}, target interface{}) error {
	body, err := apiCallRaw(url, method, payload)
	if err != nil {
		return err
	}
	if target == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func fetchCollectionPage(url string) ([]map[string]interface{}, error) {
	body, err := apiCallRaw(url, "GET", nil)
	if err != nil {
		return nil, err
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr, nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	for _, key := range []string{"items", "results", "vens", "workloads", "events"} {
		if items, ok := obj[key].([]interface{}); ok {
			out := make([]map[string]interface{}, 0, len(items))
			for _, it := range items {
				m, ok := it.(map[string]interface{})
				if ok {
					out = append(out, m)
				}
			}
			return out, nil
		}
	}
	return nil, errors.New("unexpected collection payload format")
}

func apiCallRaw(url, method string, payload interface{}) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode request payload: %w", err)
		}
		body = bytes.NewBuffer(b)
	}

	configMutex.RLock()
	apiKey := config.APIKey
	apiSecret := config.APISecret
	configMutex.RUnlock()

	log.Printf("[API] %s %s", method, url)
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(apiKey, apiSecret)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

func resolveHrefToURL(href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	configMutex.RLock()
	pceBase := strings.TrimSuffix(config.PCEURL, "/")
	configMutex.RUnlock()
	if strings.HasPrefix(href, "/api/") {
		return pceBase + href
	}
	if strings.HasPrefix(href, "/") {
		return pceBase + "/api/v2" + href
	}
	return pceBase + "/" + href
}

func loadConfig() bool {
	configMutex.Lock()
	defer configMutex.Unlock()

	file, err := os.Open(configFileName)
	if err != nil {
		return false
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return false
	}
	if strings.TrimSpace(config.OrgID) == "" {
		config.OrgID = "1"
	}
	config.Timezone = normalizeTimezone(config.Timezone)
	config.BindAddress = normalizeBindAddress(config.BindAddress)
	config.PublicBaseURL = normalizePublicBaseURL(config.PublicBaseURL)
	config.HistoryDays = configuredHistoryDaysLocked()
	config.TrafficTargets = sanitizeTargets(config.TrafficTargets)
	config.SourceExclusions = sanitizeTargets(config.SourceExclusions)
	return config.PCEURL != "" && config.APIKey != "" && config.APISecret != ""
}

func saveConfig() {
	configMutex.Lock()
	defer configMutex.Unlock()
	saveConfigLocked()
}

func saveConfigLocked() {
	file, err := os.OpenFile(configFileName, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("failed to save config: %v", err)
		return
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(config); err != nil {
		log.Printf("failed to write config: %v", err)
	}
}

func initDataDir() {
	configMutex.RLock()
	cfgDir := strings.TrimSpace(config.DataDir)
	configMutex.RUnlock()

	candidates := []string{
		strings.TrimSpace(os.Getenv("ILLUMIO_DASH_DATA_DIR")),
		cfgDir,
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		candidates = append(candidates, filepath.Join(home, defaultDataDirName))
	}
	candidates = append(candidates, ".")

	for _, c := range candidates {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if err := os.MkdirAll(c, 0o700); err != nil {
			log.Printf("[STATE] failed creating data directory %q: %v", c, err)
			continue
		}
		dataDir = c
		break
	}
	if dataDir == "" {
		dataDir = "."
	}
	log.Printf("[STATE] using data directory: %s", dataDir)
	migrateLegacyDataFiles()
}

func dataFilePath(name string) string {
	if strings.TrimSpace(dataDir) == "" || dataDir == "." {
		return name
	}
	return filepath.Join(dataDir, name)
}

func migrateLegacyDataFiles() {
	if strings.TrimSpace(dataDir) == "" || dataDir == "." {
		return
	}
	for _, name := range []string{blockedHistoryFileName, blockedPortHistoryFileName, venHistoryFileName, rollingStateFileName, alertStateFileName, anomalyHistoryFileName} {
		dst := dataFilePath(name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		src := name
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := copyFile(src, dst); err != nil {
			log.Printf("[STATE] failed migrating %s to %s: %v", src, dst, err)
			continue
		}
		log.Printf("[STATE] migrated legacy %s into shared data directory", name)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func writeJSONFileAtomic(path string, value interface{}) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadBlockedHistory() {
	historyMu.Lock()
	defer historyMu.Unlock()
	blockedDaily = map[string]map[string]int{}

	file, err := os.Open(dataFilePath(blockedHistoryFileName))
	if err != nil {
		return
	}
	defer file.Close()

	var records []dailyBlockedRecord
	if err := json.NewDecoder(file).Decode(&records); err != nil {
		log.Printf("[HISTORY] failed loading blocked history: %v", err)
		return
	}
	for _, rec := range records {
		day := strings.TrimSpace(rec.Day)
		target := strings.TrimSpace(rec.Target)
		if day == "" || target == "" {
			continue
		}
		if blockedDaily[day] == nil {
			blockedDaily[day] = map[string]int{}
		}
		blockedDaily[day][target] = rec.Count
	}
}

func loadBlockedPortHistory() {
	historyMu.Lock()
	defer historyMu.Unlock()
	blockedPortsDaily = map[string]map[string]map[string]int{}

	file, err := os.Open(dataFilePath(blockedPortHistoryFileName))
	if err != nil {
		return
	}
	defer file.Close()

	var records []dailyBlockedPortRecord
	if err := json.NewDecoder(file).Decode(&records); err != nil {
		log.Printf("[HISTORY] failed loading blocked port history: %v", err)
		return
	}
	for _, rec := range records {
		day := strings.TrimSpace(rec.Day)
		target := strings.TrimSpace(rec.Target)
		port := strings.TrimSpace(rec.Port)
		if day == "" || target == "" || port == "" {
			continue
		}
		if blockedPortsDaily[day] == nil {
			blockedPortsDaily[day] = map[string]map[string]int{}
		}
		if blockedPortsDaily[day][target] == nil {
			blockedPortsDaily[day][target] = map[string]int{}
		}
		blockedPortsDaily[day][target][port] = rec.Count
	}
}

func loadVENHistory() {
	venHistoryMu.Lock()
	defer venHistoryMu.Unlock()
	venDaily = map[string]venDailySnapshot{}

	file, err := os.Open(dataFilePath(venHistoryFileName))
	if err != nil {
		return
	}
	defer file.Close()

	var records []venDailyRecord
	if err := json.NewDecoder(file).Decode(&records); err != nil {
		log.Printf("[HISTORY] failed loading VEN history: %v", err)
		return
	}
	for _, rec := range records {
		day := strings.TrimSpace(rec.Day)
		if day == "" {
			continue
		}
		venDaily[day] = venDailySnapshot{
			WarningMax:       rec.WarningMax,
			ErrorMax:         rec.ErrorMax,
			TamperingMax:     rec.TamperingMax,
			ModeIdleMax:      rec.ModeIdleMax,
			ModeVisMax:       rec.ModeVisMax,
			ModeSelectiveMax: rec.ModeSelectiveMax,
			ModeFullMax:      rec.ModeFullMax,
			ModeUnmanagedMax: rec.ModeUnmanagedMax,
		}
	}
}

func loadRollingState() {
	rollingMu.Lock()
	defer rollingMu.Unlock()
	rollingCache = rollingState{}

	file, err := os.Open(dataFilePath(rollingStateFileName))
	if err != nil {
		return
	}
	defer file.Close()

	var persisted persistedRollingState
	if err := json.NewDecoder(file).Decode(&persisted); err != nil {
		log.Printf("[STATE] failed loading rolling state: %v", err)
		return
	}
	if persisted.SchemaVersion == 0 {
		persisted.SchemaVersion = 1
	}
	if persisted.SchemaVersion != rollingStateSchemaVersion {
		log.Printf("[STATE] ignoring rolling state schema=%d (expected %d)", persisted.SchemaVersion, rollingStateSchemaVersion)
		return
	}

	loaded := rollingState{
		Initialized:         persisted.Initialized,
		LastCycle:           persisted.LastCycle.UTC(),
		BaselineCapturedUTC: persisted.BaselineCapturedUTC.UTC(),
		BaselineTampering:   persisted.BaselineTampering,
		BaselineWorkloads:   map[string]struct{}{},
		BaselineBlocked:     map[string]targetBaseline{},
		Buckets:             make([]rollingBucket, 0, len(persisted.Buckets)),
	}
	for _, n := range persisted.BaselineWorkloads {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		loaded.BaselineWorkloads[n] = struct{}{}
	}
	for target, base := range persisted.BaselineBlocked {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		loaded.BaselineBlocked[target] = targetBaseline{
			Count:       base.Count,
			CapturedUTC: base.CapturedUTC.UTC(),
		}
	}
	for _, b := range persisted.Buckets {
		bucket := rollingBucket{
			EndUTC:             b.EndUTC.UTC(),
			VENWarningCount:    b.VENWarningCount,
			VENErrorCount:      b.VENErrorCount,
			ModeIdleCount:      b.ModeIdleCount,
			ModeVisCount:       b.ModeVisCount,
			ModeSelectiveCount: b.ModeSelectiveCount,
			ModeFullCount:      b.ModeFullCount,
			ModeUnmanagedCount: b.ModeUnmanagedCount,
			TamperingCount:     b.TamperingCount,
			TamperingWorkloads: map[string]struct{}{},
			BlockedByTarget:    map[string]int{},
		}
		for _, n := range b.TamperingWorkloads {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			bucket.TamperingWorkloads[n] = struct{}{}
		}
		for target, count := range b.BlockedByTarget {
			target = strings.TrimSpace(target)
			if target == "" {
				continue
			}
			bucket.BlockedByTarget[target] = count
		}
		loaded.Buckets = append(loaded.Buckets, bucket)
	}
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	kept := make([]rollingBucket, 0, len(loaded.Buckets))
	for _, b := range loaded.Buckets {
		if b.EndUTC.After(cutoff) {
			kept = append(kept, b)
		}
	}
	loaded.Buckets = kept
	rollingCache = loaded
	if rollingCache.Initialized {
		log.Printf("[STATE] loaded rolling state: buckets=%d baseline_targets=%d", len(rollingCache.Buckets), len(rollingCache.BaselineBlocked))
	}
}

func saveRollingState() {
	rollingMu.Lock()
	persisted := persistedRollingState{
		SchemaVersion:       rollingStateSchemaVersion,
		Initialized:         rollingCache.Initialized,
		LastCycle:           rollingCache.LastCycle.UTC(),
		BaselineCapturedUTC: rollingCache.BaselineCapturedUTC.UTC(),
		BaselineTampering:   rollingCache.BaselineTampering,
		BaselineWorkloads:   make([]string, 0, len(rollingCache.BaselineWorkloads)),
		BaselineBlocked:     make(map[string]persistedTargetBaseline, len(rollingCache.BaselineBlocked)),
		Buckets:             make([]persistedRollingBucket, 0, len(rollingCache.Buckets)),
	}
	for n := range rollingCache.BaselineWorkloads {
		if strings.TrimSpace(n) == "" {
			continue
		}
		persisted.BaselineWorkloads = append(persisted.BaselineWorkloads, n)
	}
	sort.Strings(persisted.BaselineWorkloads)
	for target, base := range rollingCache.BaselineBlocked {
		if strings.TrimSpace(target) == "" {
			continue
		}
		persisted.BaselineBlocked[target] = persistedTargetBaseline{
			Count:       base.Count,
			CapturedUTC: base.CapturedUTC.UTC(),
		}
	}
	for _, b := range rollingCache.Buckets {
		item := persistedRollingBucket{
			EndUTC:             b.EndUTC.UTC(),
			VENWarningCount:    b.VENWarningCount,
			VENErrorCount:      b.VENErrorCount,
			ModeIdleCount:      b.ModeIdleCount,
			ModeVisCount:       b.ModeVisCount,
			ModeSelectiveCount: b.ModeSelectiveCount,
			ModeFullCount:      b.ModeFullCount,
			ModeUnmanagedCount: b.ModeUnmanagedCount,
			TamperingCount:     b.TamperingCount,
			TamperingWorkloads: make([]string, 0, len(b.TamperingWorkloads)),
			BlockedByTarget:    make(map[string]int, len(b.BlockedByTarget)),
		}
		for n := range b.TamperingWorkloads {
			if strings.TrimSpace(n) == "" {
				continue
			}
			item.TamperingWorkloads = append(item.TamperingWorkloads, n)
		}
		sort.Strings(item.TamperingWorkloads)
		for target, count := range b.BlockedByTarget {
			if strings.TrimSpace(target) == "" {
				continue
			}
			item.BlockedByTarget[target] = count
		}
		persisted.Buckets = append(persisted.Buckets, item)
	}
	rollingMu.Unlock()

	if err := writeJSONFileAtomic(dataFilePath(rollingStateFileName), persisted); err != nil {
		log.Printf("[STATE] failed to save rolling state: %v", err)
	}
}

func loadAlertState() {
	alertMu.Lock()
	defer alertMu.Unlock()
	alertState = persistedAlertState{SchemaVersion: 1, Targets: map[string]alertTargetState{}}

	file, err := os.Open(dataFilePath(alertStateFileName))
	if err != nil {
		return
	}
	defer file.Close()

	var persisted persistedAlertState
	if err := json.NewDecoder(file).Decode(&persisted); err != nil {
		log.Printf("[WEBHOOK] failed loading alert state: %v", err)
		return
	}
	if persisted.SchemaVersion == 0 {
		persisted.SchemaVersion = 1
	}
	if persisted.SchemaVersion != 1 {
		log.Printf("[WEBHOOK] ignoring alert state schema=%d", persisted.SchemaVersion)
		return
	}
	if persisted.Targets == nil {
		persisted.Targets = map[string]alertTargetState{}
	}
	alertState = persisted
}

func loadAnomalyHistory() {
	anomalyHistoryMu.Lock()
	defer anomalyHistoryMu.Unlock()
	anomalyHistory = make([]anomalyHistoryEvent, 0)

	file, err := os.Open(dataFilePath(anomalyHistoryFileName))
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev anomalyHistoryEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Timestamp.IsZero() {
			continue
		}
		anomalyHistory = append(anomalyHistory, ev)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[ANOMALY] failed reading history: %v", err)
	}
	pruneAnomalyHistoryLocked(time.Now().UTC(), configuredHistoryDays())
	if err := saveAnomalyHistoryLocked(); err != nil {
		log.Printf("[ANOMALY] failed saving pruned history: %v", err)
	}
}

func pruneAnomalyHistoryLocked(nowUTC time.Time, keepDays int) {
	if keepDays <= 0 {
		keepDays = 365
	}
	if keepDays > maxHistoryDays {
		keepDays = maxHistoryDays
	}
	cutoff := nowUTC.AddDate(0, 0, -keepDays)
	keep := anomalyHistory[:0]
	for _, ev := range anomalyHistory {
		if ev.Timestamp.Before(cutoff) {
			continue
		}
		keep = append(keep, ev)
	}
	anomalyHistory = keep
	sort.Slice(anomalyHistory, func(i, j int) bool {
		return anomalyHistory[i].Timestamp.Before(anomalyHistory[j].Timestamp)
	})
}

func saveAnomalyHistoryLocked() error {
	path := dataFilePath(anomalyHistoryFileName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, ev := range anomalyHistory {
		if ev.Timestamp.IsZero() {
			continue
		}
		if err := enc.Encode(ev); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendAnomalyHistoryEvents(events []anomalyHistoryEvent) {
	if len(events) == 0 {
		return
	}
	keepDays := configuredHistoryDays()
	nowUTC := time.Now().UTC()
	anomalyHistoryMu.Lock()
	for _, ev := range events {
		if ev.Timestamp.IsZero() {
			ev.Timestamp = nowUTC
		}
		ev.Event = strings.TrimSpace(ev.Event)
		ev.State = strings.ToLower(strings.TrimSpace(ev.State))
		ev.Metric = strings.TrimSpace(ev.Metric)
		ev.TargetName = strings.TrimSpace(ev.TargetName)
		ev.TargetKind = strings.TrimSpace(ev.TargetKind)
		ev.Reason = strings.TrimSpace(ev.Reason)
		anomalyHistory = append(anomalyHistory, ev)
	}
	pruneAnomalyHistoryLocked(nowUTC, keepDays)
	if err := saveAnomalyHistoryLocked(); err != nil {
		log.Printf("[ANOMALY] failed saving history: %v", err)
	}
	anomalyHistoryMu.Unlock()
}

func saveAlertState() {
	alertMu.Lock()
	persisted := persistedAlertState{
		SchemaVersion: 1,
		Targets:       make(map[string]alertTargetState, len(alertState.Targets)),
	}
	for k, v := range alertState.Targets {
		if strings.TrimSpace(k) == "" {
			continue
		}
		persisted.Targets[k] = v
	}
	alertMu.Unlock()
	if err := writeJSONFileAtomic(dataFilePath(alertStateFileName), persisted); err != nil {
		log.Printf("[WEBHOOK] failed saving alert state: %v", err)
	}
}

func pruneBlockedHistory(nowUTC time.Time, keepDays int) {
	if keepDays <= 0 {
		keepDays = 365
	}
	if keepDays > maxHistoryDays {
		keepDays = maxHistoryDays
	}
	loc := configuredDayLocation()
	cutoff := localDayStart(nowUTC, loc).AddDate(0, 0, -keepDays)
	changed := false
	historyMu.Lock()
	for day := range blockedDaily {
		parsed, err := parseDayKeyInLocation(day, loc)
		if err != nil || parsed.Before(cutoff) {
			delete(blockedDaily, day)
			changed = true
		}
	}
	for day := range blockedPortsDaily {
		parsed, err := parseDayKeyInLocation(day, loc)
		if err != nil || parsed.Before(cutoff) {
			delete(blockedPortsDaily, day)
			changed = true
		}
	}
	historyMu.Unlock()
	if changed {
		saveBlockedHistory()
		saveBlockedPortHistory()
	}
}

func saveBlockedHistory() {
	historyMu.Lock()
	records := make([]dailyBlockedRecord, 0)
	for day, targets := range blockedDaily {
		for target, count := range targets {
			records = append(records, dailyBlockedRecord{
				Day:    day,
				Target: target,
				Count:  count,
			})
		}
	}
	historyMu.Unlock()
	sort.Slice(records, func(i, j int) bool {
		if records[i].Day == records[j].Day {
			return records[i].Target < records[j].Target
		}
		return records[i].Day < records[j].Day
	})

	if err := writeJSONFileAtomic(dataFilePath(blockedHistoryFileName), records); err != nil {
		log.Printf("[HISTORY] failed to save blocked history: %v", err)
	}
}

func saveBlockedPortHistory() {
	historyMu.Lock()
	records := make([]dailyBlockedPortRecord, 0)
	for day, targets := range blockedPortsDaily {
		for target, ports := range targets {
			for port, count := range ports {
				if strings.TrimSpace(port) == "" {
					continue
				}
				records = append(records, dailyBlockedPortRecord{
					Day:    day,
					Target: target,
					Port:   port,
					Count:  count,
				})
			}
		}
	}
	historyMu.Unlock()
	sort.Slice(records, func(i, j int) bool {
		if records[i].Day == records[j].Day {
			if records[i].Target == records[j].Target {
				return records[i].Port < records[j].Port
			}
			return records[i].Target < records[j].Target
		}
		return records[i].Day < records[j].Day
	})
	if err := writeJSONFileAtomic(dataFilePath(blockedPortHistoryFileName), records); err != nil {
		log.Printf("[HISTORY] failed to save blocked port history: %v", err)
	}
}

func pruneVENHistory(nowUTC time.Time, keepDays int) {
	if keepDays <= 0 {
		keepDays = 365
	}
	if keepDays > maxHistoryDays {
		keepDays = maxHistoryDays
	}
	loc := configuredDayLocation()
	cutoff := localDayStart(nowUTC, loc).AddDate(0, 0, -keepDays)
	changed := false
	venHistoryMu.Lock()
	for day := range venDaily {
		parsed, err := parseDayKeyInLocation(day, loc)
		if err != nil || parsed.Before(cutoff) {
			delete(venDaily, day)
			changed = true
		}
	}
	venHistoryMu.Unlock()
	if changed {
		saveVENHistory()
	}
}

func saveVENHistory() {
	venHistoryMu.Lock()
	records := make([]venDailyRecord, 0, len(venDaily))
	for day, snap := range venDaily {
		records = append(records, venDailyRecord{
			Day:              day,
			WarningMax:       snap.WarningMax,
			ErrorMax:         snap.ErrorMax,
			TamperingMax:     snap.TamperingMax,
			ModeIdleMax:      snap.ModeIdleMax,
			ModeVisMax:       snap.ModeVisMax,
			ModeSelectiveMax: snap.ModeSelectiveMax,
			ModeFullMax:      snap.ModeFullMax,
			ModeUnmanagedMax: snap.ModeUnmanagedMax,
		})
	}
	venHistoryMu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].Day < records[j].Day })

	if err := writeJSONFileAtomic(dataFilePath(venHistoryFileName), records); err != nil {
		log.Printf("[HISTORY] failed to save VEN history: %v", err)
	}
}

func updateVENDailyHistory(
	nowUTC time.Time,
	warningCount int,
	errorCount int,
	tamperingCount int,
	modeIdle int,
	modeVisibility int,
	modeSelective int,
	modeFull int,
	modeUnmanaged int,
) {
	loc := configuredDayLocation()
	dayKey := nowUTC.In(loc).Format("2006-01-02")
	venHistoryMu.Lock()
	prev := venDaily[dayKey]
	changed := false
	if warningCount > prev.WarningMax {
		prev.WarningMax = warningCount
		changed = true
	}
	if errorCount > prev.ErrorMax {
		prev.ErrorMax = errorCount
		changed = true
	}
	if tamperingCount > prev.TamperingMax {
		prev.TamperingMax = tamperingCount
		changed = true
	}
	if modeIdle > prev.ModeIdleMax {
		prev.ModeIdleMax = modeIdle
		changed = true
	}
	if modeVisibility > prev.ModeVisMax {
		prev.ModeVisMax = modeVisibility
		changed = true
	}
	if modeSelective > prev.ModeSelectiveMax {
		prev.ModeSelectiveMax = modeSelective
		changed = true
	}
	if modeFull > prev.ModeFullMax {
		prev.ModeFullMax = modeFull
		changed = true
	}
	if modeUnmanaged > prev.ModeUnmanagedMax {
		prev.ModeUnmanagedMax = modeUnmanaged
		changed = true
	}
	if changed {
		venDaily[dayKey] = prev
	}
	venHistoryMu.Unlock()
	if changed {
		saveVENHistory()
	}
}

func promptConfig() {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("--- Illumio Go Dashboard Setup ---")
	fmt.Print("PCE URL (e.g., https://pce.example.com:8443): ")
	config.PCEURL = strings.TrimSpace(readInput(reader))
	fmt.Print("API Key ID: ")
	config.APIKey = strings.TrimSpace(readInput(reader))
	fmt.Print("API Secret: ")
	config.APISecret = strings.TrimSpace(readInput(reader))
	fmt.Print("Org ID [1]: ")
	config.OrgID = strings.TrimSpace(readInput(reader))
	if config.OrgID == "" {
		config.OrgID = "1"
	}
	config.Timezone = normalizeTimezone(config.Timezone)
	config.BindAddress = normalizeBindAddress(config.BindAddress)
	config.PublicBaseURL = normalizePublicBaseURL(config.PublicBaseURL)
	fmt.Print("Save these credentials to config.json? (y/n): ")
	save := strings.ToLower(strings.TrimSpace(readInput(reader)))
	if save == "y" || save == "yes" {
		saveConfig()
	}
}

func readInput(r *bufio.Reader) string {
	val, _ := r.ReadString('\n')
	return val
}

func serveDashboard(w http.ResponseWriter, r *http.Request) {
	if err := dashboardTmpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func serveSettings(w http.ResponseWriter, r *http.Request) {
	if err := settingsTmpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func serveDetails(w http.ResponseWriter, r *http.Request) {
	if err := detailsPageTmpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func serveReport(w http.ResponseWriter, r *http.Request) {
	if err := reportPageTmpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func serveTrends(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/report?live=1", http.StatusFound)
}

func serveExecutive(w http.ResponseWriter, r *http.Request) {
	if err := executivePageTmpl.Execute(w, nil); err != nil {
		http.Error(w, "failed to render executive page", http.StatusInternalServerError)
	}
}

func handleExportReportCSV(w http.ResponseWriter, r *http.Request) {
	statsMutex.RLock()
	snapshot := currentStats
	statsMutex.RUnlock()

	fileTS := time.Now().UTC().Format("20060102-150405")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"illumio-monitoring-report-%s.csv\"", fileTS))
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	writeRow := func(fields ...string) bool {
		if err := cw.Write(fields); err != nil {
			log.Printf("[API] failed writing csv row: %v", err)
			return false
		}
		return true
	}

	if !writeRow("Illumio Monitoring Dashboard Export") {
		return
	}
	if !writeRow("Generated UTC", time.Now().UTC().Format(time.RFC3339)) {
		return
	}
	if !writeRow("Snapshot UTC", snapshot.Timestamp.UTC().Format(time.RFC3339)) {
		return
	}
	if !writeRow("Collection Mode", snapshot.Collection.Mode) {
		return
	}
	if !writeRow("Collection Window UTC", snapshot.Collection.WindowStart.UTC().Format(time.RFC3339), snapshot.Collection.WindowEnd.UTC().Format(time.RFC3339)) {
		return
	}
	if !writeRow() {
		return
	}

	if !writeRow("Summary Metric", "Value") {
		return
	}
	if !writeRow("Total Workloads", strconv.Itoa(snapshot.Workloads.Total)) {
		return
	}
	if !writeRow("Unmanaged Workloads", strconv.Itoa(snapshot.Workloads.Unmanaged)) {
		return
	}
	if !writeRow("Mode Idle", strconv.Itoa(snapshot.Workloads.EnforcementModes["idle"])) {
		return
	}
	if !writeRow("Mode Visibility Only", strconv.Itoa(snapshot.Workloads.EnforcementModes["visibility_only"])) {
		return
	}
	if !writeRow("Mode Selective", strconv.Itoa(snapshot.Workloads.EnforcementModes["selective"])) {
		return
	}
	if !writeRow("Mode Full", strconv.Itoa(snapshot.Workloads.EnforcementModes["full"])) {
		return
	}
	if !writeRow("VEN Warnings", strconv.Itoa(len(snapshot.VENStatus.Warning))) {
		return
	}
	if !writeRow("VEN Errors", strconv.Itoa(len(snapshot.VENStatus.Error))) {
		return
	}
	tamperedCount := len(snapshot.Tampering.Workloads)
	if tamperedCount == 0 {
		tamperedCount = snapshot.Tampering.Count
	}
	if !writeRow("Tampered VENs (Deduped)", strconv.Itoa(tamperedCount)) {
		return
	}
	for _, t := range snapshot.Blocked.Targets {
		if !writeRow(fmt.Sprintf("Blocked %s (24h)", t.Name), strconv.Itoa(t.Count)) {
			return
		}
	}
	if !writeRow() {
		return
	}

	if !writeRow("List Type", "Name") {
		return
	}
	for _, n := range snapshot.VENStatus.Warning {
		if !writeRow("VEN Warning", n) {
			return
		}
	}
	for _, n := range snapshot.VENStatus.Error {
		if !writeRow("VEN Error", n) {
			return
		}
	}
	for _, n := range snapshot.Tampering.Workloads {
		if !writeRow("Tampered VEN/Workload", n) {
			return
		}
	}
	if !writeRow() {
		return
	}

	if !writeRow("Trend", "Timestamp UTC", "Value") {
		return
	}
	for _, p := range venTrendSeries("warning") {
		if !writeRow("VEN Warning 24h (5m)", p.Timestamp.UTC().Format(time.RFC3339), strconv.Itoa(p.Value)) {
			return
		}
	}
	for _, p := range venTrendSeries("error") {
		if !writeRow("VEN Error 24h (5m)", p.Timestamp.UTC().Format(time.RFC3339), strconv.Itoa(p.Value)) {
			return
		}
	}
	for _, p := range venDailyTrendSeries("warning", configuredHistoryDays()) {
		if !writeRow("VEN Warning Daily", p.Timestamp.UTC().Format(time.RFC3339), strconv.Itoa(p.Value)) {
			return
		}
	}
	for _, p := range venDailyTrendSeries("error", configuredHistoryDays()) {
		if !writeRow("VEN Error Daily", p.Timestamp.UTC().Format(time.RFC3339), strconv.Itoa(p.Value)) {
			return
		}
	}
	for _, t := range snapshot.Blocked.Targets {
		for _, p := range blockedTrendSeries(t.Name) {
			if !writeRow("Blocked "+t.Name+" 24h (5m)", p.Timestamp.UTC().Format(time.RFC3339), strconv.Itoa(p.Value)) {
				return
			}
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Printf("[API] failed to flush csv export: %v", err)
	}
}
