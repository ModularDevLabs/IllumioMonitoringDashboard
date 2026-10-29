package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"illumio-dash/internal/extractor/illumio"
)

const (
	minAdaptiveChunkDuration = time.Minute
	maxAdaptiveChunkDepth    = 10
)

// ExtractionWindow uses an exclusive end, matching the extraction query windows.
type ExtractionWindow struct {
	Start        time.Time `json:"start"`
	EndExclusive time.Time `json:"end_exclusive"`
	Reason       string    `json:"reason,omitempty"`
}

type extractionManifest struct {
	Version          int                `json:"version"`
	Partial          bool               `json:"partial"`
	CSVFile          string             `json:"csv_file"`
	RequestedStart   time.Time          `json:"requested_start"`
	RequestedEnd     time.Time          `json:"requested_end_exclusive"`
	RequestedChunks  int                `json:"requested_chunks"`
	CompletedChunks  int                `json:"completed_chunks"`
	CompletedWindows []ExtractionWindow `json:"completed_windows"`
	MissingWindows   []ExtractionWindow `json:"missing_windows"`
	Warning          string             `json:"warning,omitempty"`
}

type trafficChunkFetcher func(context.Context, illumio.AsyncQueryRequest, func(string)) ([]illumio.TrafficFlow, error)

// Each successful leaf is committed exactly once. A failed parent is discarded
// before subdivision; a failed sibling cannot discard previously committed data.
func fetchExtractionChunk(ctx context.Context, req illumio.AsyncQueryRequest, chunk extractionChunk, fetch trafficChunkFetcher, commit func(extractionChunk, []illumio.TrafficFlow), logFn func(string)) []ExtractionWindow {
	var visit func(extractionChunk, int) []ExtractionWindow
	visit = func(window extractionChunk, depth int) []ExtractionWindow {
		failed := func(err error) []ExtractionWindow {
			return []ExtractionWindow{{Start: window.Start, EndExclusive: window.End, Reason: err.Error()}}
		}
		if err := ctx.Err(); err != nil {
			return failed(err)
		}
		query := req
		query.StartDate = window.Start.Format(time.RFC3339)
		query.EndDate = window.End.Format(time.RFC3339)
		for attempt := 1; attempt <= maxChunkAttempts; attempt++ {
			queryCtx, cancel := context.WithTimeout(ctx, maxChunkQueryTime)
			flows, err := fetch(queryCtx, query, logFn)
			cancel()
			if err == nil {
				// Keep even a query that finished just as cancellation arrived.
				commit(window, flows)
				return nil
			}
			if ctx.Err() != nil {
				return failed(ctx.Err())
			}
			if errors.Is(err, illumio.ErrQueryResultTruncated) {
				midpoint := window.Start.Add(window.End.Sub(window.Start) / 2).Truncate(time.Second)
				if depth >= maxAdaptiveChunkDepth || midpoint.Sub(window.Start) < minAdaptiveChunkDuration || window.End.Sub(midpoint) < minAdaptiveChunkDuration {
					return failed(fmt.Errorf("%w; automatic subdivision reached its safety limit (minimum %s, maximum depth %d)", err, minAdaptiveChunkDuration, maxAdaptiveChunkDepth))
				}
				if logFn != nil {
					logFn(fmt.Sprintf("%s to %s exceeded a result limit; splitting at %s and keeping successful smaller windows. %v", window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339), midpoint.Format(time.RFC3339), err))
				}
				left := visit(extractionChunk{Start: window.Start, End: midpoint}, depth+1)
				right := visit(extractionChunk{Start: midpoint, End: window.End}, depth+1)
				return append(left, right...)
			}
			if attempt == maxChunkAttempts {
				return failed(fmt.Errorf("failed after %d attempts: %w", attempt, err))
			}
			if logFn != nil {
				logFn(fmt.Sprintf("%s to %s attempt %d/%d failed: %v; retrying...", window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339), attempt, maxChunkAttempts, err))
			}
			timer := time.NewTimer(time.Duration(1<<uint(attempt-1)) * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return failed(ctx.Err())
			}
		}
		return nil
	}
	return visit(chunk, 0)
}

func partialCSVPath(path string) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + "_PARTIAL" + ext
}

func extractionManifestPath(csvPath string) string {
	return strings.TrimSuffix(csvPath, filepath.Ext(csvPath)) + ".extraction.json"
}

func writeExtractionManifest(csvPath string, manifest extractionManifest) error {
	path := extractionManifestPath(csvPath)
	f, closeRoot, err := createExclusiveRootedFile(path, 0600)
	if err != nil {
		return err
	}
	defer closeRoot()
	committed := false
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if !committed {
			_ = removeRootedFile(path)
		}
	}()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	committed = true
	return nil
}

func sortExtractionWindows(windows []ExtractionWindow) {
	sort.Slice(windows, func(i, j int) bool { return windows[i].Start.Before(windows[j].Start) })
}

func coverageForExtraction(path, scope string, rows int, complete, missing []ExtractionWindow, warning string) DatasetCoverage {
	file := DatasetFileCoverage{Name: filepath.Base(path), Rows: rows}
	months := map[string]bool{}
	for _, window := range complete {
		last := window.EndExclusive.Add(-time.Second)
		if file.FirstDetected.IsZero() || window.Start.Before(file.FirstDetected) {
			file.FirstDetected = window.Start
		}
		if last.After(file.LastDetected) {
			file.LastDetected = last
		}
		for _, month := range monthSpan(window.Start, last) {
			months[month] = true
		}
	}
	for month := range months {
		file.Months = append(file.Months, month)
	}
	sort.Strings(file.Months)
	coverage := DatasetCoverage{Source: "live_extraction", TrafficScope: scope, Files: []DatasetFileCoverage{file}, Partial: len(missing) > 0, MissingWindows: missing}
	if warning != "" {
		coverage.Warnings = append(coverage.Warnings, warning)
	}
	return normalizeCoverage(coverage)
}
