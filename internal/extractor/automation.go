package extractor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	automationStoreVersion = 2
	maxAutomationRuns      = 500
	defaultRetentionCount  = 20
)

type ReportTemplate struct {
	ID                    string      `json:"id"`
	Name                  string      `json:"name"`
	ProfileName           string      `json:"profile_name"`
	SrcLabels             string      `json:"src_labels"`
	DstLabels             string      `json:"dst_labels"`
	ExcludeSrc            string      `json:"exclude_src"`
	ExcludeDst            string      `json:"exclude_dst"`
	Services              string      `json:"services"`
	ExcludeServices       string      `json:"exclude_services"`
	SavePath              string      `json:"save_path"`
	FileNamePattern       string      `json:"file_name_pattern"`
	Days                  int         `json:"days"`
	ChunkInterval         string      `json:"chunk_interval"`
	AnalysisPrimary       string      `json:"analysis_primary_label"`
	AnalysisSecondary     string      `json:"analysis_secondary_label"`
	TrafficScope          string      `json:"traffic_scope"`
	RetentionCount        int         `json:"retention_count"`
	GenerateExecutiveHTML bool        `json:"generate_executive_html"`
	GenerateExecutivePDF  bool        `json:"generate_executive_pdf"`
	ReportTitle           string      `json:"report_title,omitempty"`
	ReportCustomer        string      `json:"report_customer,omitempty"`
	ReportPreparedBy      string      `json:"report_prepared_by,omitempty"`
	ReportNotes           string      `json:"report_notes,omitempty"`
	DeliveryDestination   []string    `json:"delivery_destination_ids"`
	Schedule              RunSchedule `json:"schedule"`
	AlertPolicy           AlertPolicy `json:"alert_policy"`
	CreatedAt             time.Time   `json:"created_at"`
	UpdatedAt             time.Time   `json:"updated_at"`
}

type RunSchedule struct {
	Enabled         bool      `json:"enabled"`
	Kind            string    `json:"kind"`
	TimeOfDay       string    `json:"time_of_day"`
	Weekday         int       `json:"weekday"`
	DayOfMonth      int       `json:"day_of_month"`
	CronExpression  string    `json:"cron_expression"`
	Timezone        string    `json:"timezone"`
	MissedRunPolicy string    `json:"missed_run_policy"`
	OverlapPolicy   string    `json:"overlap_policy"`
	NextRunAt       time.Time `json:"next_run_at,omitempty"`
	LastScheduledAt time.Time `json:"last_scheduled_at,omitempty"`
}

type AlertPolicy struct {
	Mode               string  `json:"mode"`
	MinimumFlows       int     `json:"minimum_flows"`
	FlowChangePercent  float64 `json:"flow_change_percent"`
	OnNewRelationships bool    `json:"on_new_relationships"`
	OnNewServices      bool    `json:"on_new_services"`
	OnExternalTraffic  bool    `json:"on_external_traffic"`
	DeliverOnFailure   bool    `json:"deliver_on_failure"`
	DeliverManualRuns  bool    `json:"deliver_manual_runs"`
}

type DeliveryDestination struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Type                string            `json:"type"`
	Enabled             bool              `json:"enabled"`
	EndpointURL         string            `json:"endpoint_url,omitempty"`
	AllowPrivateNetwork bool              `json:"allow_private_network,omitempty"`
	WebhookMode         string            `json:"webhook_mode,omitempty"`
	Headers             map[string]string `json:"headers,omitempty"`
	Token               string            `json:"token,omitempty"`
	ChannelID           string            `json:"channel_id,omitempty"`
	SMTPHost            string            `json:"smtp_host,omitempty"`
	SMTPPort            int               `json:"smtp_port,omitempty"`
	SMTPUsername        string            `json:"smtp_username,omitempty"`
	SMTPPassword        string            `json:"smtp_password,omitempty"`
	SMTPFrom            string            `json:"smtp_from,omitempty"`
	SMTPTo              []string          `json:"smtp_to,omitempty"`
	SMTPUseTLS          bool              `json:"smtp_use_tls,omitempty"`
	FolderPath          string            `json:"folder_path,omitempty"`
	SFTPHost            string            `json:"sftp_host,omitempty"`
	SFTPPort            int               `json:"sftp_port,omitempty"`
	SFTPUsername        string            `json:"sftp_username,omitempty"`
	SFTPPassword        string            `json:"sftp_password,omitempty"`
	SFTPPrivateKeyPath  string            `json:"sftp_private_key_path,omitempty"`
	SFTPHostKey         string            `json:"sftp_host_key,omitempty"`
	SFTPRemotePath      string            `json:"sftp_remote_path,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type PublicDeliveryDestination struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Type                string    `json:"type"`
	Enabled             bool      `json:"enabled"`
	EndpointHost        string    `json:"endpoint_host,omitempty"`
	AllowPrivateNetwork bool      `json:"allow_private_network,omitempty"`
	WebhookMode         string    `json:"webhook_mode,omitempty"`
	HeaderNames         []string  `json:"header_names,omitempty"`
	HasToken            bool      `json:"has_token"`
	ChannelID           string    `json:"channel_id,omitempty"`
	SMTPHost            string    `json:"smtp_host,omitempty"`
	SMTPPort            int       `json:"smtp_port,omitempty"`
	SMTPUsername        string    `json:"smtp_username,omitempty"`
	HasSMTPPassword     bool      `json:"has_smtp_password"`
	SMTPFrom            string    `json:"smtp_from,omitempty"`
	SMTPTo              []string  `json:"smtp_to,omitempty"`
	SMTPUseTLS          bool      `json:"smtp_use_tls,omitempty"`
	FolderPath          string    `json:"folder_path,omitempty"`
	SFTPHost            string    `json:"sftp_host,omitempty"`
	SFTPPort            int       `json:"sftp_port,omitempty"`
	SFTPUsername        string    `json:"sftp_username,omitempty"`
	HasSFTPPassword     bool      `json:"has_sftp_password"`
	SFTPPrivateKeyPath  string    `json:"sftp_private_key_path,omitempty"`
	SFTPHostKey         string    `json:"sftp_host_key,omitempty"`
	SFTPRemotePath      string    `json:"sftp_remote_path,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func (destination DeliveryDestination) public() PublicDeliveryDestination {
	host := ""
	if parsed, err := url.Parse(destination.EndpointURL); err == nil {
		host = parsed.Hostname()
	}
	headerNames := make([]string, 0, len(destination.Headers))
	for name := range destination.Headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	return PublicDeliveryDestination{
		ID: destination.ID, Name: destination.Name, Type: destination.Type, Enabled: destination.Enabled,
		EndpointHost: host, AllowPrivateNetwork: destination.AllowPrivateNetwork, WebhookMode: destination.WebhookMode,
		HeaderNames: headerNames, HasToken: destination.Token != "", ChannelID: destination.ChannelID,
		SMTPHost: destination.SMTPHost, SMTPPort: destination.SMTPPort, SMTPUsername: destination.SMTPUsername,
		HasSMTPPassword: destination.SMTPPassword != "", SMTPFrom: destination.SMTPFrom,
		SMTPTo: append([]string(nil), destination.SMTPTo...), SMTPUseTLS: destination.SMTPUseTLS,
		FolderPath: destination.FolderPath, SFTPHost: destination.SFTPHost, SFTPPort: destination.SFTPPort,
		SFTPUsername: destination.SFTPUsername, HasSFTPPassword: destination.SFTPPassword != "",
		SFTPPrivateKeyPath: destination.SFTPPrivateKeyPath, SFTPHostKey: destination.SFTPHostKey,
		SFTPRemotePath: destination.SFTPRemotePath, CreatedAt: destination.CreatedAt, UpdatedAt: destination.UpdatedAt,
	}
}

type DeliveryResult struct {
	DestinationID   string    `json:"destination_id"`
	DestinationName string    `json:"destination_name"`
	Success         bool      `json:"success"`
	Message         string    `json:"message"`
	AttemptedAt     time.Time `json:"attempted_at"`
}

type RunMetrics struct {
	TotalFlows             int      `json:"total_flows"`
	UniqueConnections      int      `json:"unique_connections"`
	ExternalFlows          int      `json:"external_flows"`
	FlowChangePercent      float64  `json:"flow_change_percent"`
	NewRelationships       []string `json:"new_relationships,omitempty"`
	NewServices            []string `json:"new_services,omitempty"`
	PrimaryRelationships   []string `json:"primary_relationships,omitempty"`
	ObservedServices       []string `json:"observed_services,omitempty"`
	PreviousCompletedRunID string   `json:"previous_completed_run_id,omitempty"`
}

type AutomationRun struct {
	ID                      string           `json:"id"`
	TemplateID              string           `json:"template_id"`
	TemplateName            string           `json:"template_name"`
	Trigger                 string           `json:"trigger"`
	Status                  string           `json:"status"`
	QueuedAt                time.Time        `json:"queued_at"`
	StartedAt               time.Time        `json:"started_at,omitempty"`
	CompletedAt             time.Time        `json:"completed_at,omitempty"`
	ArtifactPath            string           `json:"artifact_path,omitempty"`
	AdditionalArtifactPaths []string         `json:"additional_artifact_paths,omitempty"`
	Error                   string           `json:"error,omitempty"`
	Metrics                 RunMetrics       `json:"metrics"`
	DeliveryResults         []DeliveryResult `json:"delivery_results,omitempty"`
	DeliverySkipped         string           `json:"delivery_skipped,omitempty"`
}

type automationStoreData struct {
	Version      int                            `json:"version"`
	Templates    map[string]ReportTemplate      `json:"templates"`
	Destinations map[string]DeliveryDestination `json:"destinations"`
	Runs         []AutomationRun                `json:"runs"`
}

type AutomationManager struct {
	mu           sync.Mutex
	data         automationStoreData
	queue        chan string
	wakeSchedule chan struct{}
	stop         chan struct{}
	workerOnce   sync.Once
	wg           sync.WaitGroup
}

var automation = &AutomationManager{
	data: automationStoreData{
		Version: automationStoreVersion, Templates: map[string]ReportTemplate{}, Destinations: map[string]DeliveryDestination{}, Runs: []AutomationRun{},
	},
	queue: make(chan string, 256), wakeSchedule: make(chan struct{}, 1), stop: make(chan struct{}),
}

func automationStorePath() (string, error) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(configRoot, appConfigDirName, "automation.json"), nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode store: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create store directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("secure store directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".automation-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary store: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		return fmt.Errorf("secure temporary store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary store: %w", err)
	}
	if err := replaceFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace store: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("secure store: %w", err)
	}
	committed = true
	return nil
}

func (manager *AutomationManager) saveLocked() error {
	path, err := automationStorePath()
	if err != nil {
		return err
	}
	return writePrivateJSON(path, manager.data)
}

func (manager *AutomationManager) load() error {
	path, err := automationStorePath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read automation store: %w", err)
	}
	var loaded automationStoreData
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("parse automation store: %w", err)
	}
	if loaded.Version > automationStoreVersion {
		return fmt.Errorf("automation store version %d is newer than this application supports", loaded.Version)
	}
	if loaded.Templates == nil {
		loaded.Templates = map[string]ReportTemplate{}
	}
	if loaded.Destinations == nil {
		loaded.Destinations = map[string]DeliveryDestination{}
	}
	if loaded.Runs == nil {
		loaded.Runs = []AutomationRun{}
	}
	loaded.Version = automationStoreVersion
	if len(loaded.Runs) > maxAutomationRuns {
		loaded.Runs = loaded.Runs[:maxAutomationRuns]
	}

	now := time.Now().UTC()
	for i := range loaded.Runs {
		if loaded.Runs[i].Status == "running" {
			loaded.Runs[i].Status = "failed"
			loaded.Runs[i].CompletedAt = now
			loaded.Runs[i].Error = "application stopped while this run was active"
		}
	}
	manager.mu.Lock()
	manager.data = loaded
	err = manager.saveLocked()
	manager.mu.Unlock()
	return err
}

func newAutomationID(prefix string) string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buffer)
}

func sanitizeTemplateName(value string) string {
	value = strings.TrimSpace(value)
	var result strings.Builder
	separatorPending := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			if separatorPending && result.Len() > 0 {
				result.WriteRune('-')
			}
			result.WriteRune(r)
			separatorPending = false
		default:
			separatorPending = true
		}
	}
	cleaned := strings.Trim(result.String(), "-_")
	if cleaned == "" {
		return "report"
	}
	return cleaned
}

func expandFileNamePattern(pattern string, template ReportTemplate, when time.Time, runID string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		pattern = "blocked-{template}-{date}-{time}.csv"
	}
	replacements := map[string]string{
		"{template}":  sanitizeTemplateName(template.Name),
		"{date}":      when.Format("2006-01-02"),
		"{time}":      when.Format("150405"),
		"{timestamp}": when.Format("20060102T150405Z"),
		"{run_id}":    runID,
	}
	for token, value := range replacements {
		pattern = strings.ReplaceAll(pattern, token, value)
	}
	if filepath.Ext(pattern) == "" {
		pattern += ".csv"
	}
	return pattern
}

func templateToConfig(template ReportTemplate, runID string, now time.Time) Config {
	return Config{
		ProfileName: template.ProfileName, SrcLabels: template.SrcLabels, DstLabels: template.DstLabels,
		ExcludeSrc: template.ExcludeSrc, ExcludeDst: template.ExcludeDst, Services: template.Services,
		ExcludeServices: template.ExcludeServices,
		SavePath:        template.SavePath, FileName: expandFileNamePattern(template.FileNamePattern, template, now, runID),
		Days: template.Days, ChunkIntvl: template.ChunkInterval,
		AnalysisPrimary: template.AnalysisPrimary, AnalysisSecondary: template.AnalysisSecondary,
		TrafficScope: normalizedTrafficScope(template.TrafficScope),
	}
}

func validateTemplate(template *ReportTemplate) error {
	template.Name = strings.TrimSpace(template.Name)
	template.ProfileName = strings.TrimSpace(template.ProfileName)
	if template.Name == "" || len(template.Name) > 120 {
		return fmt.Errorf("template name is required and must be 120 characters or fewer")
	}
	if strings.ContainsAny(template.Name, "\r\n") {
		return fmt.Errorf("template name must be a single line")
	}
	if template.ProfileName == "" {
		return fmt.Errorf("a saved PCE profile is required")
	}
	state.Mu.Lock()
	_, profileExists := state.Profiles[template.ProfileName]
	state.Mu.Unlock()
	if !profileExists {
		return fmt.Errorf("saved PCE profile %q was not found", template.ProfileName)
	}
	if template.Days <= 0 {
		template.Days = 90
	}
	if template.Days > maxExtractionChunks {
		return fmt.Errorf("days must be %d or fewer", maxExtractionChunks)
	}
	if _, _, err := normalizeAnalysisLabelKeys(template.AnalysisPrimary, template.AnalysisSecondary); err != nil {
		return err
	}
	template.AnalysisPrimary, template.AnalysisSecondary, _ = normalizeAnalysisLabelKeys(template.AnalysisPrimary, template.AnalysisSecondary)
	trafficScope, err := normalizeTrafficScope(template.TrafficScope)
	if err != nil {
		return err
	}
	template.TrafficScope = trafficScope
	chunkDuration, _, err := parseChunkInterval(template.ChunkInterval)
	if err != nil {
		return err
	}
	requestedChunks := template.Days * int((24*time.Hour)/chunkDuration)
	if requestedChunks > maxExtractionChunks {
		return fmt.Errorf("the template would create %d extraction chunks; the limit is %d", requestedChunks, maxExtractionChunks)
	}
	if template.RetentionCount < 0 || template.RetentionCount > 1000 {
		return fmt.Errorf("retention count must be from 0 through 1000")
	}
	if template.RetentionCount == 0 {
		template.RetentionCount = defaultRetentionCount
	}
	template.ReportTitle = strings.TrimSpace(template.ReportTitle)
	template.ReportCustomer = strings.TrimSpace(template.ReportCustomer)
	template.ReportPreparedBy = strings.TrimSpace(template.ReportPreparedBy)
	template.ReportNotes = strings.TrimSpace(template.ReportNotes)
	if len(template.ReportTitle) > 160 || len(template.ReportCustomer) > 160 || len(template.ReportPreparedBy) > 160 || len(template.ReportNotes) > 4000 {
		return fmt.Errorf("executive report metadata exceeds the supported length")
	}
	if strings.ContainsAny(template.ReportTitle+template.ReportCustomer+template.ReportPreparedBy, "\r\n") {
		return fmt.Errorf("report title, customer, and prepared-by values must each be one line")
	}
	if !filepath.IsAbs(strings.TrimSpace(template.SavePath)) {
		return fmt.Errorf("scheduled template output folder must be an absolute path")
	}
	if len(template.FileNamePattern) > 240 || strings.ContainsAny(template.FileNamePattern, "\r\n") {
		return fmt.Errorf("filename pattern must be a single line no longer than 240 characters")
	}
	if _, err := outputCSVPath(template.SavePath, expandFileNamePattern(template.FileNamePattern, *template, time.Now().UTC(), "validation")); err != nil {
		return err
	}
	if err := validateSchedule(&template.Schedule); err != nil {
		return err
	}
	if err := validateAlertPolicy(&template.AlertPolicy); err != nil {
		return err
	}
	seenDestinations := map[string]bool{}
	for _, destinationID := range template.DeliveryDestination {
		if destinationID == "" || seenDestinations[destinationID] {
			continue
		}
		seenDestinations[destinationID] = true
	}
	template.DeliveryDestination = template.DeliveryDestination[:0]
	for destinationID := range seenDestinations {
		template.DeliveryDestination = append(template.DeliveryDestination, destinationID)
	}
	sort.Strings(template.DeliveryDestination)
	return nil
}

func validateAlertPolicy(policy *AlertPolicy) error {
	policy.Mode = strings.ToLower(strings.TrimSpace(policy.Mode))
	if policy.Mode == "" {
		policy.Mode = "always"
	}
	if policy.Mode != "always" && policy.Mode != "on_change" && policy.Mode != "threshold" {
		return fmt.Errorf("alert mode must be always, on_change, or threshold")
	}
	if policy.MinimumFlows < 0 || policy.FlowChangePercent < 0 {
		return fmt.Errorf("alert thresholds cannot be negative")
	}
	return nil
}

func validateSchedule(schedule *RunSchedule) error {
	schedule.Kind = strings.ToLower(strings.TrimSpace(schedule.Kind))
	if schedule.Kind == "" {
		schedule.Kind = "daily"
	}
	if schedule.Timezone == "" {
		schedule.Timezone = time.Local.String()
	}
	if _, err := time.LoadLocation(schedule.Timezone); err != nil {
		return fmt.Errorf("invalid schedule timezone %q", schedule.Timezone)
	}
	if schedule.MissedRunPolicy == "" {
		schedule.MissedRunPolicy = "run_once"
	}
	if schedule.MissedRunPolicy != "run_once" && schedule.MissedRunPolicy != "skip" {
		return fmt.Errorf("missed-run policy must be run_once or skip")
	}
	if schedule.OverlapPolicy == "" {
		schedule.OverlapPolicy = "queue"
	}
	if schedule.OverlapPolicy != "queue" && schedule.OverlapPolicy != "skip" {
		return fmt.Errorf("overlap policy must be queue or skip")
	}
	if !schedule.Enabled {
		return nil
	}
	if _, err := scheduleNext(*schedule, time.Now().UTC()); err != nil {
		return err
	}
	return nil
}

func scheduleExpression(schedule RunSchedule) (string, error) {
	parseTime := func() (int, int, error) {
		parts := strings.Split(schedule.TimeOfDay, ":")
		if len(parts) != 2 {
			return 0, 0, fmt.Errorf("schedule time must use HH:MM")
		}
		hour, hourErr := strconv.Atoi(parts[0])
		minute, minuteErr := strconv.Atoi(parts[1])
		if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			return 0, 0, fmt.Errorf("schedule time must use a valid 24-hour HH:MM value")
		}
		return hour, minute, nil
	}

	switch schedule.Kind {
	case "cron":
		expression := strings.TrimSpace(schedule.CronExpression)
		if expression == "" {
			return "", fmt.Errorf("cron expression is required")
		}
		return expression, nil
	case "daily", "weekdays", "weekly", "monthly":
		hour, minute, err := parseTime()
		if err != nil {
			return "", err
		}
		switch schedule.Kind {
		case "daily":
			return fmt.Sprintf("%d %d * * *", minute, hour), nil
		case "weekdays":
			return fmt.Sprintf("%d %d * * 1-5", minute, hour), nil
		case "weekly":
			if schedule.Weekday < 0 || schedule.Weekday > 6 {
				return "", fmt.Errorf("weekday must be from 0 (Sunday) through 6 (Saturday)")
			}
			return fmt.Sprintf("%d %d * * %d", minute, hour, schedule.Weekday), nil
		case "monthly":
			if schedule.DayOfMonth < 1 || schedule.DayOfMonth > 28 {
				return "", fmt.Errorf("monthly day must be from 1 through 28")
			}
			return fmt.Sprintf("%d %d %d * *", minute, hour, schedule.DayOfMonth), nil
		}
	}
	return "", fmt.Errorf("schedule kind must be daily, weekdays, weekly, monthly, or cron")
}

func scheduleNext(schedule RunSchedule, after time.Time) (time.Time, error) {
	expression, err := scheduleExpression(schedule)
	if err != nil {
		return time.Time{}, err
	}
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := cron.ParseStandard(expression)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron schedule: %w", err)
	}
	return parsed.Next(after.In(location)).UTC(), nil
}

func (manager *AutomationManager) saveTemplate(template ReportTemplate) (ReportTemplate, error) {
	if err := validateTemplate(&template); err != nil {
		return ReportTemplate{}, err
	}
	now := time.Now().UTC()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, destinationID := range template.DeliveryDestination {
		if _, ok := manager.data.Destinations[destinationID]; !ok {
			return ReportTemplate{}, fmt.Errorf("delivery destination %q was not found", destinationID)
		}
	}
	if template.ID == "" {
		template.ID = newAutomationID("tpl")
		template.CreatedAt = now
	} else if existing, ok := manager.data.Templates[template.ID]; ok {
		template.CreatedAt = existing.CreatedAt
	} else {
		return ReportTemplate{}, fmt.Errorf("template not found")
	}
	for id, existing := range manager.data.Templates {
		if id != template.ID && strings.EqualFold(existing.Name, template.Name) {
			return ReportTemplate{}, fmt.Errorf("a template named %q already exists", template.Name)
		}
	}
	template.UpdatedAt = now
	if template.Schedule.Enabled {
		template.Schedule.NextRunAt, _ = scheduleNext(template.Schedule, now)
	} else {
		template.Schedule.NextRunAt = time.Time{}
	}
	previous, existed := manager.data.Templates[template.ID]
	manager.data.Templates[template.ID] = template
	if err := manager.saveLocked(); err != nil {
		if existed {
			manager.data.Templates[template.ID] = previous
		} else {
			delete(manager.data.Templates, template.ID)
		}
		return ReportTemplate{}, err
	}
	manager.signalScheduler()
	return template, nil
}

func (manager *AutomationManager) deleteTemplate(id string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if _, ok := manager.data.Templates[id]; !ok {
		return fmt.Errorf("template not found")
	}
	template := manager.data.Templates[id]
	delete(manager.data.Templates, id)
	if err := manager.saveLocked(); err != nil {
		manager.data.Templates[id] = template
		return err
	}
	return nil
}

func (manager *AutomationManager) queueRun(templateID, trigger string) (AutomationRun, error) {
	manager.mu.Lock()
	template, ok := manager.data.Templates[templateID]
	if !ok {
		manager.mu.Unlock()
		return AutomationRun{}, fmt.Errorf("template not found")
	}
	if template.Schedule.OverlapPolicy == "skip" {
		for _, run := range manager.data.Runs {
			if run.TemplateID == templateID && (run.Status == "queued" || run.Status == "running") {
				manager.mu.Unlock()
				return AutomationRun{}, fmt.Errorf("template already has a queued or running job")
			}
		}
	}
	run := AutomationRun{
		ID: newAutomationID("run"), TemplateID: template.ID, TemplateName: template.Name,
		Trigger: trigger, Status: "queued", QueuedAt: time.Now().UTC(),
	}
	previousRuns := append([]AutomationRun(nil), manager.data.Runs...)
	manager.data.Runs = append([]AutomationRun{run}, manager.data.Runs...)
	manager.trimRunsLocked()
	if err := manager.saveLocked(); err != nil {
		manager.data.Runs = previousRuns
		manager.mu.Unlock()
		return AutomationRun{}, err
	}
	manager.mu.Unlock()
	select {
	case manager.queue <- run.ID:
	default:
		go func() { manager.queue <- run.ID }()
	}
	return run, nil
}

func (manager *AutomationManager) cancelQueuedRun(runID string) error {
	manager.mu.Lock()
	index := manager.runIndexLocked(runID)
	if index < 0 {
		manager.mu.Unlock()
		return fmt.Errorf("run not found")
	}
	status := manager.data.Runs[index].Status
	if status == "running" {
		manager.mu.Unlock()
		state.Mu.Lock()
		cancel := state.CancelFunc
		state.Mu.Unlock()
		if cancel == nil {
			return fmt.Errorf("running extraction no longer has an active cancel function")
		}
		cancel()
		return nil
	}
	if status != "queued" {
		manager.mu.Unlock()
		return fmt.Errorf("only queued or running jobs can be cancelled")
	}
	previousRun := manager.data.Runs[index]
	manager.data.Runs[index].Status = "cancelled"
	manager.data.Runs[index].CompletedAt = time.Now().UTC()
	err := manager.saveLocked()
	if err != nil {
		manager.data.Runs[index] = previousRun
	}
	manager.mu.Unlock()
	return err
}

func (manager *AutomationManager) runIndexLocked(runID string) int {
	for i := range manager.data.Runs {
		if manager.data.Runs[i].ID == runID {
			return i
		}
	}
	return -1
}

func (manager *AutomationManager) trimRunsLocked() {
	if len(manager.data.Runs) > maxAutomationRuns {
		manager.data.Runs = manager.data.Runs[:maxAutomationRuns]
	}
}

func (manager *AutomationManager) signalScheduler() {
	select {
	case manager.wakeSchedule <- struct{}{}:
	default:
	}
}

func (manager *AutomationManager) start(ctx context.Context, enableScheduler bool) {
	manager.workerOnce.Do(func() {
		manager.wg.Add(1)
		go func() {
			defer manager.wg.Done()
			manager.worker(ctx)
		}()
		if enableScheduler {
			manager.wg.Add(1)
			go func() {
				defer manager.wg.Done()
				manager.scheduler(ctx)
			}()
		}
		manager.recoverQueuedRuns()
	})
}

func (manager *AutomationManager) waitForStop(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		manager.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (manager *AutomationManager) recoverQueuedRuns() {
	manager.mu.Lock()
	queued := []string{}
	for _, run := range manager.data.Runs {
		if run.Status == "queued" {
			queued = append(queued, run.ID)
		}
	}
	manager.mu.Unlock()
	for i := len(queued) - 1; i >= 0; i-- {
		manager.queue <- queued[i]
	}
}

func (manager *AutomationManager) scheduler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	manager.processSchedules(time.Now().UTC(), true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-manager.stop:
			return
		case <-ticker.C:
			manager.processSchedules(time.Now().UTC(), false)
		case <-manager.wakeSchedule:
			manager.processSchedules(time.Now().UTC(), false)
		}
	}
}

func (manager *AutomationManager) processSchedules(now time.Time, startup bool) {
	type dueTemplate struct{ id string }
	due := []dueTemplate{}
	manager.mu.Lock()
	changed := false
	for id, template := range manager.data.Templates {
		schedule := template.Schedule
		if !schedule.Enabled {
			continue
		}
		if schedule.NextRunAt.IsZero() {
			next, err := scheduleNext(schedule, now)
			if err != nil {
				log.Printf("template %s schedule error: %v", template.Name, err)
				continue
			}
			schedule.NextRunAt = next
			template.Schedule = schedule
			manager.data.Templates[id] = template
			changed = true
			continue
		}
		if schedule.NextRunAt.After(now) {
			continue
		}
		shouldRun := !startup || schedule.MissedRunPolicy == "run_once"
		if shouldRun {
			due = append(due, dueTemplate{id: id})
		}
		schedule.LastScheduledAt = schedule.NextRunAt
		next, err := scheduleNext(schedule, now)
		if err == nil {
			schedule.NextRunAt = next
		}
		template.Schedule = schedule
		manager.data.Templates[id] = template
		changed = true
	}
	if changed {
		_ = manager.saveLocked()
	}
	manager.mu.Unlock()
	for _, item := range due {
		if _, err := manager.queueRun(item.id, "schedule"); err != nil {
			log.Printf("schedule queue failed for %s: %v", item.id, err)
		}
	}
}

func (manager *AutomationManager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-manager.stop:
			return
		case runID := <-manager.queue:
			manager.executeRun(ctx, runID)
		}
	}
}

func (manager *AutomationManager) executeRun(parent context.Context, runID string) {
	manager.mu.Lock()
	index := manager.runIndexLocked(runID)
	if index < 0 || manager.data.Runs[index].Status != "queued" {
		manager.mu.Unlock()
		return
	}
	template, ok := manager.data.Templates[manager.data.Runs[index].TemplateID]
	if !ok {
		manager.data.Runs[index].Status = "failed"
		manager.data.Runs[index].Error = "template was deleted before the queued run started"
		manager.data.Runs[index].CompletedAt = time.Now().UTC()
		_ = manager.saveLocked()
		manager.mu.Unlock()
		return
	}
	manager.mu.Unlock()

	for {
		state.Mu.Lock()
		busy := state.CancelFunc != nil
		state.Mu.Unlock()
		if !busy {
			break
		}
		select {
		case <-parent.Done():
			return
		case <-time.After(time.Second):
		}
	}

	startedAt := time.Now().UTC()
	cfg := templateToConfig(template, runID, startedAt)
	resolved, runCtx, err := beginExtractionWithContext(parent, cfg)
	if err != nil {
		manager.finishFailedRun(parent, runID, err)
		return
	}

	manager.mu.Lock()
	index = manager.runIndexLocked(runID)
	if index >= 0 {
		manager.data.Runs[index].Status = "running"
		manager.data.Runs[index].StartedAt = startedAt
		_ = manager.saveLocked()
	}
	manager.mu.Unlock()

	runExtraction(runCtx, resolved)

	state.Mu.Lock()
	artifactPath := state.FileName
	cancelled := state.IsCancelled
	summary := append([]PortProtocolSummary(nil), state.LastSummary...)
	insights := state.LastInsights
	coverage := state.DatasetCoverage
	runError := state.RunError
	state.Mu.Unlock()
	if artifactPath == "" {
		if runError == "" {
			runError = "extraction did not produce an artifact"
		}
		if cancelled {
			manager.finishCancelledRun(runID)
			return
		}
		manager.finishFailedRun(parent, runID, errors.New(runError))
		return
	}

	metrics := manager.calculateMetrics(template.ID, runID, summary, insights)
	additionalArtifacts, reportErr := generateScheduledExecutiveArtifacts(artifactPath, template, summary, insights, coverage)
	if reportErr != nil {
		manager.finishFailedRun(parent, runID, reportErr)
		return
	}
	manager.mu.Lock()
	index = manager.runIndexLocked(runID)
	if index >= 0 {
		manager.data.Runs[index].Status = "completed"
		manager.data.Runs[index].CompletedAt = time.Now().UTC()
		manager.data.Runs[index].ArtifactPath = artifactPath
		manager.data.Runs[index].AdditionalArtifactPaths = additionalArtifacts
		manager.data.Runs[index].Metrics = metrics
		_ = manager.saveLocked()
	}
	manager.mu.Unlock()

	manager.deliverCompletedRun(parent, runID, template, artifactPath, additionalArtifacts, metrics)
	manager.applyRetention(template)
}

func (manager *AutomationManager) finishCancelledRun(runID string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if index := manager.runIndexLocked(runID); index >= 0 {
		manager.data.Runs[index].Status = "cancelled"
		manager.data.Runs[index].CompletedAt = time.Now().UTC()
		manager.data.Runs[index].Error = "extraction was cancelled"
		_ = manager.saveLocked()
	}
}

func (manager *AutomationManager) finishFailedRun(ctx context.Context, runID string, runErr error) {
	manager.mu.Lock()
	index := manager.runIndexLocked(runID)
	if index >= 0 {
		manager.data.Runs[index].Status = "failed"
		manager.data.Runs[index].CompletedAt = time.Now().UTC()
		manager.data.Runs[index].Error = runErr.Error()
		_ = manager.saveLocked()
	}
	var template ReportTemplate
	if index >= 0 {
		template = manager.data.Templates[manager.data.Runs[index].TemplateID]
	}
	manager.mu.Unlock()
	if template.AlertPolicy.DeliverOnFailure {
		manager.deliverFailedRun(ctx, runID, template, runErr)
	}
}

func (manager *AutomationManager) calculateMetrics(templateID, runID string, summary []PortProtocolSummary, insights AnalyticsInsights) RunMetrics {
	metrics := RunMetrics{}
	for _, row := range summary {
		metrics.TotalFlows += row.FlowCount
		metrics.UniqueConnections += row.UniqueConnections
		metrics.ObservedServices = append(metrics.ObservedServices, fmt.Sprintf("%s:%d", row.Protocol, row.Port))
	}
	for _, row := range insights.TrafficCategories {
		if strings.Contains(row.Name, "External/Unmanaged") {
			metrics.ExternalFlows += row.FlowCount
		}
	}
	for _, row := range insights.EnvMatrix {
		metrics.PrimaryRelationships = append(metrics.PrimaryRelationships, row.Source+" -> "+row.Destination)
	}
	sort.Strings(metrics.PrimaryRelationships)
	sort.Strings(metrics.ObservedServices)

	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, prior := range manager.data.Runs {
		if prior.ID == runID || prior.TemplateID != templateID || prior.Status != "completed" {
			continue
		}
		metrics.PreviousCompletedRunID = prior.ID
		if prior.Metrics.TotalFlows > 0 {
			metrics.FlowChangePercent = (float64(metrics.TotalFlows-prior.Metrics.TotalFlows) / float64(prior.Metrics.TotalFlows)) * 100
		} else if metrics.TotalFlows > 0 {
			metrics.FlowChangePercent = 100
		}
		priorRelationships := make(map[string]bool, len(prior.Metrics.PrimaryRelationships))
		for _, value := range prior.Metrics.PrimaryRelationships {
			priorRelationships[value] = true
		}
		for _, value := range metrics.PrimaryRelationships {
			if !priorRelationships[value] {
				metrics.NewRelationships = append(metrics.NewRelationships, value)
			}
		}
		priorServices := make(map[string]bool, len(prior.Metrics.ObservedServices))
		for _, value := range prior.Metrics.ObservedServices {
			priorServices[value] = true
		}
		for _, value := range metrics.ObservedServices {
			if !priorServices[value] {
				metrics.NewServices = append(metrics.NewServices, value)
			}
		}
		break
	}
	return metrics
}

func shouldDeliverRun(template ReportTemplate, trigger string, metrics RunMetrics) (bool, string) {
	policy := template.AlertPolicy
	if trigger == "manual" && !policy.DeliverManualRuns {
		return false, "manual-run delivery is disabled"
	}
	switch policy.Mode {
	case "always", "":
		return true, ""
	case "on_change":
		if metrics.PreviousCompletedRunID == "" {
			return true, ""
		}
		if (policy.OnNewRelationships && len(metrics.NewRelationships) > 0) ||
			(policy.OnNewServices && len(metrics.NewServices) > 0) ||
			(policy.OnExternalTraffic && metrics.ExternalFlows > 0) ||
			(policy.FlowChangePercent > 0 && absFloat(metrics.FlowChangePercent) >= policy.FlowChangePercent) {
			return true, ""
		}
		return false, "no configured change condition was met"
	case "threshold":
		if policy.MinimumFlows > 0 && metrics.TotalFlows >= policy.MinimumFlows {
			return true, ""
		}
		if policy.FlowChangePercent > 0 && absFloat(metrics.FlowChangePercent) >= policy.FlowChangePercent {
			return true, ""
		}
		if policy.OnExternalTraffic && metrics.ExternalFlows > 0 {
			return true, ""
		}
		return false, "no configured threshold was met"
	}
	return true, ""
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func (manager *AutomationManager) applyRetention(template ReportTemplate) {
	manager.mu.Lock()
	completed := []AutomationRun{}
	for _, run := range manager.data.Runs {
		if run.TemplateID == template.ID && run.Status == "completed" && run.ArtifactPath != "" {
			completed = append(completed, run)
		}
	}
	manager.mu.Unlock()
	retention := template.RetentionCount
	if retention <= 0 {
		retention = defaultRetentionCount
	}
	if len(completed) <= retention {
		return
	}
	for _, run := range completed[retention:] {
		artifactPaths := append([]string{run.ArtifactPath}, run.AdditionalArtifactPaths...)
		for _, artifactPath := range artifactPaths {
			cleaned := filepath.Clean(artifactPath)
			root := filepath.Clean(template.SavePath)
			relative, relErr := filepath.Rel(root, cleaned)
			if !filepath.IsAbs(cleaned) || relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				log.Printf("retention skipped artifact outside the template output folder: %s", cleaned)
				continue
			}
			info, err := lstatRootedFile(cleaned)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				log.Printf("retention could not inspect %s: %v", cleaned, err)
				continue
			}
			if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				log.Printf("retention refused to remove non-file artifact %s", cleaned)
				continue
			}
			if err := removeRootedFile(cleaned); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("retention could not remove %s: %v", cleaned, err)
			}
		}
	}
}
