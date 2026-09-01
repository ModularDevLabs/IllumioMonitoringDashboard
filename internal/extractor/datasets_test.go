package extractor

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeCoverageFindsMissingMonthsAndOverlaps(t *testing.T) {
	t.Parallel()
	coverage := normalizeCoverage(DatasetCoverage{Source: "csv_import", Files: []DatasetFileCoverage{
		{Name: "january.csv", FirstDetected: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), LastDetected: time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC), Months: []string{"2026-01"}},
		{Name: "march-a.csv", FirstDetected: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), LastDetected: time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC), Months: []string{"2026-03"}},
		{Name: "march-b.csv", FirstDetected: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), LastDetected: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC), Months: []string{"2026-03"}},
	}})
	if len(coverage.MissingMonths) != 1 || coverage.MissingMonths[0] != "2026-02" {
		t.Fatalf("missing months = %#v", coverage.MissingMonths)
	}
	if len(coverage.Overlaps) != 1 || coverage.Overlaps[0].FirstFile != "march-a.csv" || coverage.Overlaps[0].SecondFile != "march-b.csv" {
		t.Fatalf("overlaps = %#v", coverage.Overlaps)
	}
	if len(coverage.Warnings) < 2 {
		t.Fatalf("coverage warnings = %#v", coverage.Warnings)
	}
}

func TestDetailedCSVImportBuildsCoverageAndRelationshipTrends(t *testing.T) {
	t.Parallel()
	header := "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App,First Detected,Last Detected"
	january := strings.Join([]string{header, "10.0.0.1,10.0.0.2,443,TCP,3,Prod,Shared,Web,API,2026-01-02 01:00:00,2026-01-29 22:00:00"}, "\n")
	march := strings.Join([]string{header, "10.0.0.3,198.51.100.5,445,TCP,7,Prod,External/Unmanaged,Web,External/Unmanaged,2026-03-03 01:00:00,2026-03-28 22:00:00"}, "\n")
	dataset, err := parseCSVAnalyticsInputsDetailed([]csvAnalyticsInput{{Name: "jan.csv", Reader: strings.NewReader(january)}, {Name: "mar.csv", Reader: strings.NewReader(march)}}, "env", "app")
	if err != nil {
		t.Fatalf("parseCSVAnalyticsInputsDetailed: %v", err)
	}
	if len(dataset.Coverage.MissingMonths) != 1 || dataset.Coverage.MissingMonths[0] != "2026-02" {
		t.Fatalf("coverage = %#v", dataset.Coverage)
	}
	if len(dataset.Insights.MonthlyRelationships) != 2 {
		t.Fatalf("monthly relationships = %#v", dataset.Insights.MonthlyRelationships)
	}
	if len(dataset.Insights.MonthlyExternalDestinations) != 1 || dataset.Insights.MonthlyExternalDestinations[0].Destination != "198.51.100.5" {
		t.Fatalf("monthly external destinations = %#v", dataset.Insights.MonthlyExternalDestinations)
	}
}

func TestDetailedCSVImportDeduplicatesExactRowsAcrossDifferentFiles(t *testing.T) {
	t.Parallel()
	header := "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App,First Detected,Last Detected"
	duplicate := "10.0.0.1,10.0.0.2,443,TCP,12,Prod,Shared,Web,API,2026-01-02 01:00:00,2026-01-29 22:00:00"
	first := strings.Join([]string{header, duplicate}, "\n")
	second := strings.Join([]string{header, duplicate, "10.0.0.3,10.0.0.4,22,TCP,4,Prod,Shared,Batch,SSH,2026-01-04 01:00:00,2026-01-20 22:00:00"}, "\n")
	dataset, err := parseCSVAnalyticsInputsDetailed([]csvAnalyticsInput{{Name: "first.csv", Reader: strings.NewReader(first)}, {Name: "second.csv", Reader: strings.NewReader(second)}}, "env", "app")
	if err != nil {
		t.Fatalf("parseCSVAnalyticsInputsDetailed: %v", err)
	}
	flows := 0
	for _, row := range dataset.Summary {
		flows += row.FlowCount
	}
	if flows != 16 {
		t.Fatalf("total flows = %d, want 16 after removing the duplicated 12-flow row", flows)
	}
	if dataset.Coverage.DeduplicatedRecords != 1 || dataset.Coverage.DeduplicatedFlows != 12 {
		t.Fatalf("deduplication coverage = %#v", dataset.Coverage)
	}
	monthlyFlows := 0
	for _, row := range dataset.Insights.MonthlyPortProtocol {
		monthlyFlows += row.FlowCount
	}
	if monthlyFlows != 16 {
		t.Fatalf("monthly flows = %d, want 16 after deduplication", monthlyFlows)
	}
}

func TestAllTrafficCSVImportPreservesDecisionRowsWithoutInflatingUniqueConnections(t *testing.T) {
	t.Parallel()

	header := "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App,First Detected,Last Detected,Policy Decision,Draft Policy Decision,Traffic Scope"
	rows := strings.Join([]string{
		header,
		"10.0.0.1,10.0.0.2,443,TCP,3,Prod,Shared,Web,API,2026-01-02 01:00:00,2026-01-02 02:00:00,allowed,allowed,all",
		"10.0.0.1,10.0.0.2,443,TCP,2,Prod,Shared,Web,API,2026-01-02 01:00:00,2026-01-02 02:00:00,blocked,blocked,all",
	}, "\n")
	dataset, err := parseCSVAnalyticsInputsDetailed([]csvAnalyticsInput{{Name: "all.csv", Reader: strings.NewReader(rows)}}, "env", "app")
	if err != nil {
		t.Fatalf("parseCSVAnalyticsInputsDetailed: %v", err)
	}
	if dataset.Coverage.TrafficScope != trafficScopeAll {
		t.Fatalf("traffic scope = %q, want all", dataset.Coverage.TrafficScope)
	}
	if len(dataset.Summary) != 1 || dataset.Summary[0].FlowCount != 5 || dataset.Summary[0].UniqueConnections != 1 {
		t.Fatalf("summary = %#v, want 5 flows across one unique connection", dataset.Summary)
	}
	if len(dataset.Insights.MonthlyPortProtocol) != 1 || dataset.Insights.MonthlyPortProtocol[0].FlowCount != 5 || dataset.Insights.MonthlyPortProtocol[0].UniqueConnections != 1 {
		t.Fatalf("monthly summary = %#v, want decision rows combined without inflating connections", dataset.Insights.MonthlyPortProtocol)
	}
}

func TestValidateReportMetadataCanonicalizesIncludedSections(t *testing.T) {
	t.Parallel()

	metadata, err := validateReportMetadata(ReportMetadata{IncludedSections: []string{"risky-services", "coverage", "risky-services"}})
	if err != nil {
		t.Fatalf("validateReportMetadata: %v", err)
	}
	if got, want := strings.Join(metadata.IncludedSections, ","), "coverage,risky-services"; got != want {
		t.Fatalf("included sections = %q, want %q", got, want)
	}
	if _, err := validateReportMetadata(ReportMetadata{IncludedSections: []string{"unknown-section"}}); err == nil {
		t.Fatal("validateReportMetadata accepted an unknown report section")
	}
	empty, err := validateReportMetadata(ReportMetadata{IncludedSections: []string{}})
	if err != nil {
		t.Fatalf("validateReportMetadata empty selection: %v", err)
	}
	if empty.IncludedSections == nil || len(empty.IncludedSections) != 0 {
		t.Fatalf("empty explicit selection was not preserved: %#v", empty.IncludedSections)
	}
}
