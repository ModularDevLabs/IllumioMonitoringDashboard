package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"illumio-dash/internal/extractor/illumio"
)

func TestAdaptiveExtractionSplitsWithoutCountingParent(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)
	chunk := extractionChunk{Start: start, End: start.Add(time.Hour)}
	for _, limit := range []error{illumio.ErrQueryResultTruncated} {
		t.Run(limit.Error(), func(t *testing.T) {
			calls, total := 0, 0
			var windows []ExtractionWindow
			fetch := func(_ context.Context, req illumio.AsyncQueryRequest, _ func(string)) ([]illumio.TrafficFlow, error) {
				calls++
				from, _ := time.Parse(time.RFC3339, req.StartDate)
				to, _ := time.Parse(time.RFC3339, req.EndDate)
				if len(req.Services.Exclude) != 1 || req.PolicyDecisions == nil || len(req.PolicyDecisions) != 0 {
					t.Fatal("split request lost service filter or all-traffic scope")
				}
				if to.Sub(from) > 30*time.Minute {
					return []illumio.TrafficFlow{{NumConnections: 999}}, fmt.Errorf("wrapped limit: %w", limit)
				}
				return []illumio.TrafficFlow{{NumConnections: 5}}, nil
			}
			gaps := fetchExtractionChunk(context.Background(), illumio.AsyncQueryRequest{Services: illumio.ServiceFilter{Exclude: []interface{}{illumio.PortProtoService{Port: 9300, Proto: 6}}}, PolicyDecisions: []string{}}, chunk, fetch, func(window extractionChunk, flows []illumio.TrafficFlow) {
				windows = append(windows, ExtractionWindow{Start: window.Start, EndExclusive: window.End})
				for _, flow := range flows {
					total += flow.NumConnections
				}
			}, nil)
			if calls != 3 || total != 10 || len(gaps) != 0 || len(windows) != 2 {
				t.Fatalf("calls=%d total=%d windows=%v gaps=%v", calls, total, windows, gaps)
			}
			if windows[0].Start != chunk.Start || windows[0].EndExclusive != windows[1].Start || windows[1].EndExclusive != chunk.End {
				t.Fatalf("subdivision introduced gap/overlap: %#v", windows)
			}
		})
	}
}

func TestAdaptiveExtractionDoesNotSplitOversizedControlResponse(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)
	chunk := extractionChunk{Start: start, End: start.Add(time.Hour)}
	calls := 0
	gaps := fetchExtractionChunk(context.Background(), illumio.AsyncQueryRequest{}, chunk, func(_ context.Context, req illumio.AsyncQueryRequest, _ func(string)) ([]illumio.TrafficFlow, error) {
		calls++
		if req.StartDate != chunk.Start.Format(time.RFC3339) || req.EndDate != chunk.End.Format(time.RFC3339) {
			t.Fatal("an oversized control response must not trigger smaller traffic queries")
		}
		return nil, illumio.ErrResponseTooLarge
	}, func(extractionChunk, []illumio.TrafficFlow) { t.Fatal("committed failed control response") }, nil)
	if calls != maxChunkAttempts || len(gaps) != 1 {
		t.Fatalf("calls=%d gaps=%v", calls, gaps)
	}
}

func TestAdaptiveExtractionPreservesSiblingAndStopsAtMinimum(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)
	chunk := extractionChunk{Start: start, End: start.Add(2 * time.Minute)}
	calls, committed := 0, 0
	gaps := fetchExtractionChunk(context.Background(), illumio.AsyncQueryRequest{}, chunk, func(_ context.Context, req illumio.AsyncQueryRequest, _ func(string)) ([]illumio.TrafficFlow, error) {
		calls++
		if req.StartDate == start.Format(time.RFC3339) {
			return nil, illumio.ErrQueryResultTruncated
		}
		return []illumio.TrafficFlow{{NumConnections: 1}}, nil
	}, func(extractionChunk, []illumio.TrafficFlow) { committed++ }, nil)
	if calls != 3 || committed != 1 || len(gaps) != 1 || gaps[0].EndExclusive != start.Add(time.Minute) {
		t.Fatalf("calls=%d committed=%d gaps=%#v", calls, committed, gaps)
	}
}

func TestAdaptiveExtractionCancellationKeepsCompletedChild(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)
	chunk := extractionChunk{Start: start, End: start.Add(time.Hour)}
	calls, committed := 0, 0
	gaps := fetchExtractionChunk(ctx, illumio.AsyncQueryRequest{}, chunk, func(_ context.Context, req illumio.AsyncQueryRequest, _ func(string)) ([]illumio.TrafficFlow, error) {
		calls++
		if calls == 1 {
			return nil, illumio.ErrQueryResultTruncated
		}
		return nil, nil // A valid empty window is still successfully retrieved.
	}, func(extractionChunk, []illumio.TrafficFlow) { committed++; cancel() }, nil)
	if calls != 2 || committed != 1 || len(gaps) != 1 || gaps[0].Start != start.Add(30*time.Minute) || !strings.Contains(gaps[0].Reason, "canceled") {
		t.Fatalf("calls=%d committed=%d gaps=%#v", calls, committed, gaps)
	}
}

func TestExtractionManifestAndCoverageOnlyIncludeSuccessfulWindows(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	complete := []ExtractionWindow{{Start: start, EndExclusive: start.Add(time.Hour)}}
	missing := []ExtractionWindow{{Start: start.Add(time.Hour), EndExclusive: start.Add(2 * time.Hour), Reason: "download failed"}}
	path := filepath.Join(t.TempDir(), "test_PARTIAL.csv")
	coverage := coverageForExtraction(path, trafficScopeAll, 3, complete, missing, "INCOMPLETE EXTRACTION")
	if !coverage.Partial || len(coverage.Months) != 1 || coverage.Months[0] != "2026-09" || coverage.LastDetected.Month() != time.September || len(coverage.Warnings) == 0 {
		t.Fatalf("coverage invented successful October coverage: %#v", coverage)
	}
	manifest := extractionManifest{Version: 1, Partial: true, CSVFile: filepath.Base(path), CompletedWindows: complete, MissingWindows: missing}
	if err := writeExtractionManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(extractionManifestPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var restored extractionManifest
	if err := json.Unmarshal(data, &restored); err != nil || !restored.Partial || len(restored.MissingWindows) != 1 {
		t.Fatalf("manifest=%s err=%v", data, err)
	}
	if err := writeExtractionManifest(path, manifest); err == nil {
		t.Fatal("manifest must not overwrite an existing file")
	}
}

func TestFailedManifestWriteRemovesIncompleteSidecar(t *testing.T) {
	t.Parallel()
	csvPath := filepath.Join(t.TempDir(), "traffic_PARTIAL.csv")
	csvData := []byte("Flows\n3\n")
	if err := os.WriteFile(csvPath, csvData, 0600); err != nil {
		t.Fatal(err)
	}
	// time.Time refuses to encode years outside its JSON range. This fails
	// after file creation and exercises the same cleanup as disk write errors.
	manifest := extractionManifest{RequestedStart: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := writeExtractionManifest(csvPath, manifest); err == nil {
		t.Fatal("invalid manifest timestamp should fail encoding")
	}
	if _, err := os.Stat(extractionManifestPath(csvPath)); !os.IsNotExist(err) {
		t.Fatalf("failed manifest left an incomplete sidecar: %v", err)
	}
	data, err := os.ReadFile(csvPath)
	if err != nil || string(data) != string(csvData) {
		t.Fatalf("manifest failure changed the retained CSV: %q, %v", data, err)
	}
	if err := writeExtractionManifest(csvPath, extractionManifest{Version: 1}); err != nil {
		t.Fatalf("cleaned-up sidecar should permit a fresh write: %v", err)
	}
}

func TestImportPreservesPartialWarningAfterCSVRename(t *testing.T) {
	t.Parallel()
	csv := "First Detected,Last Detected,Source IP,Destination IP,Src Env,Dst Env,Src App,Dst App,Port,Protocol,Flows,Extraction Status\n2026-09-12 03:00:00,2026-09-12 03:30:00,10.0.0.1,10.0.0.2,Prod,Prod,API,DB,443,TCP,12,partial\n"
	dataset, err := parseCSVAnalyticsInputsDetailed([]csvAnalyticsInput{{Name: "renamed.csv", Reader: strings.NewReader(csv)}}, "env", "app")
	if err != nil {
		t.Fatal(err)
	}
	if !dataset.Coverage.Partial || len(dataset.Coverage.Warnings) != 1 || !strings.Contains(dataset.Coverage.Warnings[0], "INCOMPLETE") {
		t.Fatalf("lost partial warning: %#v", dataset.Coverage)
	}
}
