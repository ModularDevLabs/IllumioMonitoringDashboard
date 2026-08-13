package extractor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	datasetStoreVersion = 1
	maxSavedDatasets    = 50
)

var executiveReportSectionOrder = []string{
	"coverage",
	"executive-overview",
	"monthly-trends",
	"service-trends",
	"relationship-trends",
	"period-comparison",
	"headline-findings",
	"risky-services",
	"persistent-relationships",
	"latest-changes",
	"cross-talk",
	"external-spotlight",
	"dimension-scorecard",
}

type DatasetFileCoverage struct {
	Name          string    `json:"name"`
	SHA256        string    `json:"sha256,omitempty"`
	Size          int64     `json:"size"`
	Rows          int       `json:"rows"`
	FirstDetected time.Time `json:"first_detected,omitempty"`
	LastDetected  time.Time `json:"last_detected,omitempty"`
	Months        []string  `json:"months"`
}

type DatasetOverlap struct {
	FirstFile  string    `json:"first_file"`
	SecondFile string    `json:"second_file"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
}

type DatasetCoverage struct {
	Source              string                `json:"source"`
	Files               []DatasetFileCoverage `json:"files"`
	FirstDetected       time.Time             `json:"first_detected,omitempty"`
	LastDetected        time.Time             `json:"last_detected,omitempty"`
	Months              []string              `json:"months"`
	MissingMonths       []string              `json:"missing_months"`
	Overlaps            []DatasetOverlap      `json:"overlaps"`
	DeduplicatedRecords int                   `json:"deduplicated_records,omitempty"`
	DeduplicatedFlows   int                   `json:"deduplicated_flows,omitempty"`
	Warnings            []string              `json:"warnings"`
}

type ReportMetadata struct {
	Title            string   `json:"title"`
	CustomerName     string   `json:"customer_name"`
	PreparedBy       string   `json:"prepared_by"`
	Notes            string   `json:"notes"`
	LogoDataURL      string   `json:"logo_data_url,omitempty"`
	IncludedSections []string `json:"included_sections"`
}

type SavedDataset struct {
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	FileName  string                `json:"file_name"`
	Summary   []PortProtocolSummary `json:"summary"`
	Insights  AnalyticsInsights     `json:"insights"`
	Coverage  DatasetCoverage       `json:"coverage"`
	Report    ReportMetadata        `json:"report"`
	CreatedAt time.Time             `json:"created_at"`
	UpdatedAt time.Time             `json:"updated_at"`
}

type SavedDatasetInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	FileName   string    `json:"file_name"`
	FileCount  int       `json:"file_count"`
	MonthCount int       `json:"month_count"`
	Start      time.Time `json:"start,omitempty"`
	End        time.Time `json:"end,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type datasetStoreData struct {
	Version  int                     `json:"version"`
	Datasets map[string]SavedDataset `json:"datasets"`
}

type DatasetManager struct {
	mu   sync.Mutex
	data datasetStoreData
}

var datasetManager = &DatasetManager{data: datasetStoreData{Version: datasetStoreVersion, Datasets: map[string]SavedDataset{}}}

func datasetStorePath() (string, error) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(configRoot, appConfigDirName, "datasets.json"), nil
}

func (manager *DatasetManager) saveLocked() error {
	path, err := datasetStorePath()
	if err != nil {
		return err
	}
	return writePrivateJSON(path, manager.data)
}

func (manager *DatasetManager) load() error {
	path, err := datasetStorePath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read dataset store: %w", err)
	}
	var loaded datasetStoreData
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("parse dataset store: %w", err)
	}
	if loaded.Version > datasetStoreVersion {
		return fmt.Errorf("dataset store version %d is newer than this application supports", loaded.Version)
	}
	if loaded.Datasets == nil {
		loaded.Datasets = map[string]SavedDataset{}
	}
	loaded.Version = datasetStoreVersion
	manager.mu.Lock()
	manager.data = loaded
	err = manager.saveLocked()
	manager.mu.Unlock()
	return err
}

func validateDatasetName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 || strings.ContainsAny(name, "\r\n") {
		return "", fmt.Errorf("dataset name is required, must be one line, and must be 120 characters or fewer")
	}
	return name, nil
}

func validateReportMetadata(metadata ReportMetadata) (ReportMetadata, error) {
	metadata.Title = strings.TrimSpace(metadata.Title)
	metadata.CustomerName = strings.TrimSpace(metadata.CustomerName)
	metadata.PreparedBy = strings.TrimSpace(metadata.PreparedBy)
	metadata.Notes = strings.TrimSpace(metadata.Notes)
	if len(metadata.Title) > 160 || len(metadata.CustomerName) > 160 || len(metadata.PreparedBy) > 160 || len(metadata.Notes) > 4000 {
		return ReportMetadata{}, fmt.Errorf("report metadata exceeds the supported length")
	}
	if strings.ContainsAny(metadata.Title+metadata.CustomerName+metadata.PreparedBy, "\r\n") {
		return ReportMetadata{}, fmt.Errorf("report title, customer, and prepared-by values must each be one line")
	}
	if metadata.LogoDataURL != "" {
		if len(metadata.LogoDataURL) > 800<<10 {
			return ReportMetadata{}, fmt.Errorf("report logo must be 600 KB or smaller")
		}
		allowed := strings.HasPrefix(metadata.LogoDataURL, "data:image/png;base64,") || strings.HasPrefix(metadata.LogoDataURL, "data:image/jpeg;base64,") || strings.HasPrefix(metadata.LogoDataURL, "data:image/svg+xml;base64,")
		if !allowed {
			return ReportMetadata{}, fmt.Errorf("report logo must be a PNG, JPEG, or SVG data URL")
		}
	}
	if metadata.IncludedSections != nil {
		requested := make(map[string]struct{}, len(metadata.IncludedSections))
		allowed := make(map[string]struct{}, len(executiveReportSectionOrder))
		for _, section := range executiveReportSectionOrder {
			allowed[section] = struct{}{}
		}
		for _, section := range metadata.IncludedSections {
			section = strings.TrimSpace(section)
			if _, ok := allowed[section]; !ok {
				return ReportMetadata{}, fmt.Errorf("unsupported executive report section %q", section)
			}
			requested[section] = struct{}{}
		}
		metadata.IncludedSections = make([]string, 0, len(requested))
		for _, section := range executiveReportSectionOrder {
			if _, ok := requested[section]; ok {
				metadata.IncludedSections = append(metadata.IncludedSections, section)
			}
		}
	}
	if metadata.Title == "" {
		metadata.Title = "Blocked Traffic Executive Summary"
	}
	return metadata, nil
}

func (manager *DatasetManager) saveDataset(dataset SavedDataset) (SavedDataset, error) {
	name, err := validateDatasetName(dataset.Name)
	if err != nil {
		return SavedDataset{}, err
	}
	report, err := validateReportMetadata(dataset.Report)
	if err != nil {
		return SavedDataset{}, err
	}
	now := time.Now().UTC()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if dataset.ID == "" {
		if len(manager.data.Datasets) >= maxSavedDatasets {
			return SavedDataset{}, fmt.Errorf("no more than %d saved datasets are supported", maxSavedDatasets)
		}
		dataset.ID = newAutomationID("dataset")
		dataset.CreatedAt = now
	} else if existing, ok := manager.data.Datasets[dataset.ID]; ok {
		dataset.CreatedAt = existing.CreatedAt
	} else {
		return SavedDataset{}, fmt.Errorf("saved dataset was not found")
	}
	for id, existing := range manager.data.Datasets {
		if id != dataset.ID && strings.EqualFold(existing.Name, name) {
			return SavedDataset{}, fmt.Errorf("a saved dataset named %q already exists", name)
		}
	}
	dataset.Name = name
	dataset.Report = report
	dataset.UpdatedAt = now
	previous, existed := manager.data.Datasets[dataset.ID]
	manager.data.Datasets[dataset.ID] = dataset
	if err := manager.saveLocked(); err != nil {
		if existed {
			manager.data.Datasets[dataset.ID] = previous
		} else {
			delete(manager.data.Datasets, dataset.ID)
		}
		return SavedDataset{}, err
	}
	return dataset, nil
}

func (manager *DatasetManager) list() []SavedDatasetInfo {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	items := make([]SavedDatasetInfo, 0, len(manager.data.Datasets))
	for _, dataset := range manager.data.Datasets {
		items = append(items, SavedDatasetInfo{
			ID: dataset.ID, Name: dataset.Name, FileName: dataset.FileName,
			FileCount: len(dataset.Coverage.Files), MonthCount: len(dataset.Coverage.Months),
			Start: dataset.Coverage.FirstDetected, End: dataset.Coverage.LastDetected, UpdatedAt: dataset.UpdatedAt,
		})
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	return items
}

func (manager *DatasetManager) get(id string) (SavedDataset, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	dataset, ok := manager.data.Datasets[strings.TrimSpace(id)]
	return dataset, ok
}

func (manager *DatasetManager) delete(id string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	dataset, ok := manager.data.Datasets[id]
	if !ok {
		return fmt.Errorf("saved dataset was not found")
	}
	delete(manager.data.Datasets, id)
	if err := manager.saveLocked(); err != nil {
		manager.data.Datasets[id] = dataset
		return err
	}
	return nil
}

func normalizeCoverage(coverage DatasetCoverage) DatasetCoverage {
	monthSet := map[string]bool{}
	for _, file := range coverage.Files {
		for _, month := range file.Months {
			if month != "" {
				monthSet[month] = true
			}
		}
		if coverage.FirstDetected.IsZero() || (!file.FirstDetected.IsZero() && file.FirstDetected.Before(coverage.FirstDetected)) {
			coverage.FirstDetected = file.FirstDetected
		}
		if file.LastDetected.After(coverage.LastDetected) {
			coverage.LastDetected = file.LastDetected
		}
	}
	coverage.Months = coverage.Months[:0]
	for month := range monthSet {
		coverage.Months = append(coverage.Months, month)
	}
	sort.Strings(coverage.Months)
	if len(coverage.Months) > 1 {
		start, _ := time.Parse("2006-01", coverage.Months[0])
		end, _ := time.Parse("2006-01", coverage.Months[len(coverage.Months)-1])
		for cursor := start; !cursor.After(end); cursor = cursor.AddDate(0, 1, 0) {
			month := cursor.Format("2006-01")
			if !monthSet[month] {
				coverage.MissingMonths = append(coverage.MissingMonths, month)
			}
		}
	}
	files := append([]DatasetFileCoverage(nil), coverage.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].FirstDetected.Before(files[j].FirstDetected) })
	for i := 0; i < len(files); i++ {
		if files[i].FirstDetected.IsZero() || files[i].LastDetected.IsZero() {
			continue
		}
		for j := i + 1; j < len(files); j++ {
			if files[j].FirstDetected.After(files[i].LastDetected) {
				break
			}
			end := files[i].LastDetected
			if files[j].LastDetected.Before(end) {
				end = files[j].LastDetected
			}
			coverage.Overlaps = append(coverage.Overlaps, DatasetOverlap{FirstFile: files[i].Name, SecondFile: files[j].Name, Start: files[j].FirstDetected, End: end})
		}
	}
	if len(coverage.MissingMonths) > 0 {
		coverage.Warnings = append(coverage.Warnings, "Missing months: "+strings.Join(coverage.MissingMonths, ", ")+". They are shown as gaps rather than zero activity.")
	}
	if coverage.DeduplicatedRecords > 0 {
		coverage.Warnings = append(coverage.Warnings, fmt.Sprintf("Removed %d exact duplicate row(s), representing %d duplicate flow(s), found in more than one source file.", coverage.DeduplicatedRecords, coverage.DeduplicatedFlows))
	}
	if len(coverage.Overlaps) > 0 {
		pairs := make([]string, 0, len(coverage.Overlaps))
		for _, overlap := range coverage.Overlaps {
			pairs = append(pairs, overlap.FirstFile+" / "+overlap.SecondFile)
		}
		coverage.Warnings = append(coverage.Warnings, "Overlapping windows: "+strings.Join(pairs, ", ")+". Exact duplicate rows and unique connections are deduplicated; other aggregate flow totals remain additive because the CSV does not contain per-flow event IDs.")
	}
	return coverage
}
