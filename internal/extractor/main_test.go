package extractor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"illumio-dash/internal/extractor/illumio"
)

func TestParseDirectService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		want   illumio.PortProtoService
		wantOK bool
	}{
		{
			name:   "tcp single port",
			input:  "TCP:445",
			want:   illumio.PortProtoService{Port: 445, Proto: 6},
			wantOK: true,
		},
		{
			name:   "udp single port with spaces",
			input:  " UDP : 5355 ",
			want:   illumio.PortProtoService{Port: 5355, Proto: 17},
			wantOK: true,
		},
		{
			name:   "numeric proto range",
			input:  "47:1024-2048",
			want:   illumio.PortProtoService{Port: 1024, ToPort: 2048, Proto: 47},
			wantOK: true,
		},
		{
			name:   "igmp proto",
			input:  "IGMP:2",
			want:   illumio.PortProtoService{Port: 2, Proto: 2},
			wantOK: true,
		},
		{
			name:   "missing port",
			input:  "TCP:",
			wantOK: false,
		},
		{
			name:   "invalid proto",
			input:  "BOGUS:445",
			wantOK: false,
		},
		{
			name:   "invalid range",
			input:  "TCP:200-100",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := parseDirectService(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("parseDirectService(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseDirectService(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildServiceIncludeEntries(t *testing.T) {
	t.Parallel()

	serviceMap := map[string][]interface{}{
		"SSH": {
			illumio.PortProtoService{Port: 22, Proto: 6},
		},
	}

	got, warnings := buildServiceIncludeEntries("SSH, TCP:445, UDP:5355, Unknown Service", serviceMap)
	if len(warnings) != 1 || warnings[0] != "Unknown Service" {
		t.Fatalf("buildServiceIncludeEntries warnings = %#v, want only Unknown Service", warnings)
	}

	if len(got) != 3 {
		t.Fatalf("buildServiceIncludeEntries returned %d entries, want 3", len(got))
	}

	serviceRef, ok := got[0].(illumio.PortProtoService)
	if !ok || serviceRef != (illumio.PortProtoService{Port: 22, Proto: 6}) {
		t.Fatalf("first include = %#v, want expanded service entry for SSH", got[0])
	}

	tcpRef, ok := got[1].(illumio.PortProtoService)
	if !ok || tcpRef != (illumio.PortProtoService{Port: 445, Proto: 6}) {
		t.Fatalf("second include = %#v, want TCP:445", got[1])
	}

	udpRef, ok := got[2].(illumio.PortProtoService)
	if !ok || udpRef != (illumio.PortProtoService{Port: 5355, Proto: 17}) {
		t.Fatalf("third include = %#v, want UDP:5355", got[2])
	}
}

func TestBuildServiceFilterIncludesAndExcludes(t *testing.T) {
	t.Parallel()
	serviceMap := map[string][]interface{}{
		"SSH": {illumio.PortProtoService{Port: 22, Proto: 6}},
		"DNS": {
			illumio.PortProtoService{Port: 53, Proto: 6},
			illumio.PortProtoService{Port: 53, Proto: 17},
		},
	}

	filter, includeWarnings, excludeWarnings := buildServiceFilter(
		"SSH, TCP:443, Missing Include",
		"DNS, UDP:5355, Missing Exclusion",
		serviceMap,
	)
	if !reflect.DeepEqual(includeWarnings, []string{"Missing Include"}) {
		t.Fatalf("include warnings = %#v", includeWarnings)
	}
	if !reflect.DeepEqual(excludeWarnings, []string{"Missing Exclusion"}) {
		t.Fatalf("exclude warnings = %#v", excludeWarnings)
	}
	if len(filter.Include) != 2 {
		t.Fatalf("service includes = %#v", filter.Include)
	}
	if len(filter.Exclude) != 3 {
		t.Fatalf("service exclusions = %#v", filter.Exclude)
	}
	if got, ok := filter.Exclude[2].(illumio.PortProtoService); !ok || got != (illumio.PortProtoService{Port: 5355, Proto: 17}) {
		t.Fatalf("explicit exclusion = %#v", filter.Exclude[2])
	}
}

func TestServiceEntriesFromService(t *testing.T) {
	t.Parallel()

	icmpType := 8
	icmpCode := 0
	service := illumio.Service{
		Name: "Complex Service",
		ServicePorts: []illumio.ServicePort{
			{Port: 443, Proto: 6},
			{Proto: 1, ICMPType: &icmpType, ICMPCode: &icmpCode},
		},
	}

	got := serviceEntriesFromService(service)
	if len(got) != 2 {
		t.Fatalf("serviceEntriesFromService returned %d entries, want 2", len(got))
	}

	first, ok := got[0].(illumio.PortProtoService)
	if !ok || first.Port != 443 || first.Proto != 6 {
		t.Fatalf("first expanded entry = %#v", got[0])
	}

	second, ok := got[1].(illumio.PortProtoService)
	if !ok || second.Proto != 1 || second.ICMPType == nil || *second.ICMPType != icmpType {
		t.Fatalf("second expanded entry = %#v", got[1])
	}
}

func TestParseSelectorRejectsUnknownNonIP(t *testing.T) {
	t.Parallel()

	_, ok := parseSelector("A-RXCONNECT", map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{})
	if ok {
		t.Fatal("parseSelector should reject unknown non-IP tokens")
	}

	ref, ok := parseSelector("10.10.10.10", map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{})
	if !ok {
		t.Fatal("parseSelector should accept valid IP addresses")
	}
	if ref.IPAddress != "10.10.10.10" {
		t.Fatalf("parseSelector returned %#v, want IPAddress 10.10.10.10", ref)
	}
}

func TestExtractionDateRange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.March, 10, 12, 0, 0, 0, time.UTC)

	t.Run("explicit inclusive range", func(t *testing.T) {
		t.Parallel()

		cfg := Config{StartDate: "2026-02-01", EndDate: "2026-02-28", Days: 90}
		start, end, days, err := extractionDateRange(cfg, now)
		if err != nil {
			t.Fatalf("extractionDateRange returned error: %v", err)
		}
		if got, want := start.Format("2006-01-02"), "2026-02-01"; got != want {
			t.Fatalf("start = %s, want %s", got, want)
		}
		if got, want := end.Format("2006-01-02"), "2026-02-28"; got != want {
			t.Fatalf("end = %s, want %s", got, want)
		}
		if days != 28 {
			t.Fatalf("days = %d, want 28", days)
		}
	})

	t.Run("trailing days defaults to yesterday", func(t *testing.T) {
		t.Parallel()

		cfg := Config{Days: 7}
		start, end, days, err := extractionDateRange(cfg, now)
		if err != nil {
			t.Fatalf("extractionDateRange returned error: %v", err)
		}
		if got, want := end.Format("2006-01-02"), "2026-03-09"; got != want {
			t.Fatalf("end = %s, want %s", got, want)
		}
		if got, want := start.Format("2006-01-02"), "2026-03-03"; got != want {
			t.Fatalf("start = %s, want %s", got, want)
		}
		if days != 7 {
			t.Fatalf("days = %d, want 7", days)
		}
	})

	t.Run("requires both explicit dates", func(t *testing.T) {
		t.Parallel()

		_, _, _, err := extractionDateRange(Config{StartDate: "2026-02-01"}, now)
		if err == nil {
			t.Fatal("expected error when only one explicit date is provided")
		}
	})
}

func TestInclusiveCalendarDaysHandlesLongRanges(t *testing.T) {
	t.Parallel()

	start := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2400, time.December, 31, 0, 0, 0, 0, time.UTC)
	if got, want := inclusiveCalendarDays(start, end), 146463; got != want {
		t.Fatalf("inclusiveCalendarDays = %d, want %d", got, want)
	}
}

func TestParseChunkInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		want      time.Duration
		wantLabel string
		wantErr   bool
	}{
		{name: "default empty", input: "", want: 24 * time.Hour, wantLabel: "1 day"},
		{name: "explicit 24h", input: "24h", want: 24 * time.Hour, wantLabel: "1 day"},
		{name: "hourly", input: "1h", want: time.Hour, wantLabel: "1h"},
		{name: "ten minutes", input: "10m", want: 10 * time.Minute, wantLabel: "10m"},
		{name: "invalid", input: "2h", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, label, err := parseChunkInterval(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseChunkInterval(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Fatalf("parseChunkInterval(%q) duration = %v, want %v", tt.input, got, tt.want)
			}
			if label != tt.wantLabel {
				t.Fatalf("parseChunkInterval(%q) label = %q, want %q", tt.input, label, tt.wantLabel)
			}
		})
	}
}

func TestBuildExtractionChunks(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.March, 2, 0, 0, 0, 0, time.UTC)

	t.Run("daily range split into half days", func(t *testing.T) {
		t.Parallel()

		chunks := buildExtractionChunks(start, end, 12*time.Hour)
		if got, want := len(chunks), 4; got != want {
			t.Fatalf("len(chunks) = %d, want %d", got, want)
		}
		if got, want := chunks[0].Start.Format(time.RFC3339), "2026-03-02T12:00:00Z"; got != want {
			t.Fatalf("chunks[0].Start = %s, want %s", got, want)
		}
		if got, want := chunks[0].End.Format(time.RFC3339), "2026-03-03T00:00:00Z"; got != want {
			t.Fatalf("chunks[0].End = %s, want %s", got, want)
		}
		if got, want := chunks[3].Start.Format(time.RFC3339), "2026-03-01T00:00:00Z"; got != want {
			t.Fatalf("chunks[3].Start = %s, want %s", got, want)
		}
	})

	t.Run("partial leading chunk preserved", func(t *testing.T) {
		t.Parallel()

		chunks := buildExtractionChunks(start, start, 36*time.Hour)
		if got, want := len(chunks), 1; got != want {
			t.Fatalf("len(chunks) = %d, want %d", got, want)
		}
		if got, want := chunks[0].Start.Format(time.RFC3339), "2026-03-01T00:00:00Z"; got != want {
			t.Fatalf("chunks[0].Start = %s, want %s", got, want)
		}
		if got, want := chunks[0].End.Format(time.RFC3339), "2026-03-02T00:00:00Z"; got != want {
			t.Fatalf("chunks[0].End = %s, want %s", got, want)
		}
	})
}

func TestMonthlyPortProtocolFromRecordsTracksActiveConnectionsAcrossMonths(t *testing.T) {
	t.Parallel()

	records := []AnalyticsRecord{
		{
			Protocol:  "TCP",
			Port:      5985,
			Month:     "2026-01",
			FlowCount: 90,
			FirstSeen: time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC),
			LastSeen:  time.Date(2026, time.March, 3, 0, 0, 0, 0, time.UTC),
		},
	}

	got := monthlyPortProtocolFromRecords(records)
	if len(got) != 3 {
		t.Fatalf("monthlyPortProtocolFromRecords returned %d rows, want 3", len(got))
	}

	byMonth := map[string]MonthlyPortProtocolSummary{}
	for _, row := range got {
		byMonth[row.Month] = row
	}

	if byMonth["2026-01"].FlowCount != 90 || byMonth["2026-01"].UniqueConnections != 1 || byMonth["2026-01"].ActiveConnections != 1 {
		t.Fatalf("january row = %#v, want flow 90 unique 1 active 1", byMonth["2026-01"])
	}
	if byMonth["2026-02"].FlowCount != 0 || byMonth["2026-02"].UniqueConnections != 0 || byMonth["2026-02"].ActiveConnections != 1 {
		t.Fatalf("february row = %#v, want flow 0 unique 0 active 1", byMonth["2026-02"])
	}
	if byMonth["2026-03"].FlowCount != 0 || byMonth["2026-03"].UniqueConnections != 0 || byMonth["2026-03"].ActiveConnections != 1 {
		t.Fatalf("march row = %#v, want flow 0 unique 0 active 1", byMonth["2026-03"])
	}
}

func TestSameOriginRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		host    string
		origin  string
		referer string
		want    bool
	}{
		{name: "missing origin evidence rejected", host: "localhost:8080", want: false},
		{name: "matching origin allowed", host: "localhost:8080", origin: "http://localhost:8080", want: true},
		{name: "matching referer allowed", host: "localhost:8080", referer: "http://localhost:8080/summary", want: true},
		{name: "mismatched origin rejected", host: "localhost:8080", origin: "http://evil.example", want: false},
		{name: "mismatched referer rejected", host: "localhost:8080", referer: "http://evil.example/form", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "http://"+tt.host+"/api/start", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.referer != "" {
				req.Header.Set("Referer", tt.referer)
			}

			if got := sameOriginRequest(req); got != tt.want {
				t.Fatalf("sameOriginRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanonicalFlowLabelsIsStableAndIncludesValues(t *testing.T) {
	t.Parallel()

	left := []illumio.FlowLabel{{Key: "Env", Value: "Prod"}, {Key: "App", Value: "API"}}
	right := []illumio.FlowLabel{{Key: "App", Value: "API"}, {Key: "env", Value: "Prod"}}
	if got, want := canonicalFlowLabels(left), canonicalFlowLabels(right); got != want {
		t.Fatalf("canonical label keys differ by input order/case: %q != %q", got, want)
	}
	changed := []illumio.FlowLabel{{Key: "App", Value: "Web"}, {Key: "Env", Value: "Prod"}}
	if canonicalFlowLabels(left) == canonicalFlowLabels(changed) {
		t.Fatal("canonical label keys should distinguish different label values")
	}
}

func TestCSVFormulaProtectionRoundTrips(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"=cmd()", "+SUM(A1:A2)", "-1+2", "@payload"} {
		protected := safeCSVCell(value)
		if !strings.HasPrefix(protected, "'") {
			t.Fatalf("safeCSVCell(%q) = %q, want apostrophe prefix", value, protected)
		}
		if got := normalizeImportedCSVCell(protected); got != value {
			t.Fatalf("normalizeImportedCSVCell(%q) = %q, want %q", protected, got, value)
		}
	}
	if got := safeCSVCell("normal"); got != "normal" {
		t.Fatalf("safeCSVCell changed a safe value: %q", got)
	}
}

func TestOutputCSVPathValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, err := outputCSVPath(dir, "report")
	if err != nil {
		t.Fatalf("outputCSVPath returned error: %v", err)
	}
	if want := filepath.Join(dir, "report.csv"); got != want {
		t.Fatalf("outputCSVPath = %q, want %q", got, want)
	}
	if _, err := outputCSVPath(dir, "../profiles.json"); err == nil {
		t.Fatal("outputCSVPath should reject traversal filenames")
	}
	if _, err := outputCSVPath("relative", "report.csv"); err == nil {
		t.Fatal("outputCSVPath should reject relative output directories")
	}
}

func TestValidatePCEURL(t *testing.T) {
	t.Parallel()

	got, err := validatePCEURL(" https://pce.example.com:8443/ ")
	if err != nil || got != "https://pce.example.com:8443" {
		t.Fatalf("validatePCEURL returned %q, %v", got, err)
	}
	for _, invalid := range []string{"file:///tmp/pce", "http://pce.example.com", "https://user:pass@pce.example.com", "https://pce.example.com/api/v2", "https://pce.example.com?q=x"} {
		if _, err := validatePCEURL(invalid); err == nil {
			t.Fatalf("validatePCEURL(%q) should fail", invalid)
		}
	}
}

func TestValidatePort(t *testing.T) {
	t.Parallel()

	if got, err := validatePort(" 8080 "); err != nil || got != "8080" {
		t.Fatalf("validatePort returned %q, %v", got, err)
	}
	for _, invalid := range []string{"", "0", "65536", "http"} {
		if _, err := validatePort(invalid); err == nil {
			t.Fatalf("validatePort(%q) should fail", invalid)
		}
	}
}

func TestLoopbackHost(t *testing.T) {
	t.Parallel()

	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"} {
		if !loopbackHost(host) {
			t.Fatalf("loopbackHost(%q) = false", host)
		}
	}
	for _, host := range []string{"example.com:8080", "192.0.2.10:8080"} {
		if loopbackHost(host) {
			t.Fatalf("loopbackHost(%q) = true", host)
		}
	}
}

func TestPublicProfileRedactsCredentials(t *testing.T) {
	t.Parallel()

	profile := PCEProfile{
		Name: "prod", APIKey: "key", APISecret: "secret", PCEURL: "https://pce.example.com", OrgID: "1",
		AnalysisPrimary: "BU", AnalysisSecondary: "app", ExcludeServices: "DNS, TCP:22",
	}
	public := profile.public()
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("marshal public profile: %v", err)
	}
	if strings.Contains(string(encoded), profile.APIKey) || strings.Contains(string(encoded), profile.APISecret) {
		t.Fatal("public profile exposed credentials")
	}
	if public.AnalysisPrimary != "BU" || public.AnalysisSecondary != "app" {
		t.Fatalf("public profile analysis labels = %q/%q", public.AnalysisPrimary, public.AnalysisSecondary)
	}
	if public.ExcludeServices != profile.ExcludeServices {
		t.Fatalf("public profile service exclusions = %q, want %q", public.ExcludeServices, profile.ExcludeServices)
	}
}

func TestResolveAnalysisLabelKeysSupportsCustomTypesCaseInsensitively(t *testing.T) {
	t.Parallel()

	labels := []illumio.Label{
		{Key: "app", Value: "Payments"},
		{Key: "BU", Value: "Finance"},
		{Key: "bu", Value: "Operations"},
		{Key: "env", Value: "Prod"},
	}
	primary, secondary, err := resolveAnalysisLabelKeys("bu", "APP", labels)
	if err != nil {
		t.Fatalf("resolveAnalysisLabelKeys returned error: %v", err)
	}
	if primary != "BU" || secondary != "app" {
		t.Fatalf("resolved keys = %q/%q, want BU/app", primary, secondary)
	}
	if got, want := labelTypesFromLabels(labels), []string{"app", "BU", "env"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labelTypesFromLabels = %#v, want %#v", got, want)
	}
}

func TestResolveAnalysisLabelKeysRejectsMissingOrDuplicateTypes(t *testing.T) {
	t.Parallel()

	labels := []illumio.Label{{Key: "BU"}, {Key: "app"}}
	if _, _, err := resolveAnalysisLabelKeys("region", "app", labels); err == nil || !strings.Contains(err.Error(), "available label types") {
		t.Fatalf("missing custom key error = %v", err)
	}
	if _, _, err := normalizeAnalysisLabelKeys("BU", "bu"); err == nil {
		t.Fatal("duplicate label types should be rejected case-insensitively")
	}
}

func TestParseCSVAnalyticsRestoresProtectedCellsAndTimestamps(t *testing.T) {
	t.Parallel()

	csvData := strings.Join([]string{
		"Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App,First Detected,Last Detected,FQDN",
		"10.0.0.1,10.0.0.2,443,TCP,7,Prod,Prod,'=cmd(),API,2026-01-31 23:00:00,2026-03-01 01:00:00,api.example.com",
	}, "\n")
	summary, insights, err := parseCSVAnalytics(strings.NewReader(csvData))
	if err != nil {
		t.Fatalf("parseCSVAnalytics returned error: %v", err)
	}
	if len(summary) != 1 || summary[0].FlowCount != 7 || summary[0].Port != 443 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(insights.SourceAppOptions) != 1 || insights.SourceAppOptions[0] != "=cmd()" {
		t.Fatalf("source app options = %#v, want restored protected value", insights.SourceAppOptions)
	}
	if insights.PrimaryLabelKey != "env" || insights.SecondaryLabelKey != "app" {
		t.Fatalf("default analysis labels = %q/%q", insights.PrimaryLabelKey, insights.SecondaryLabelKey)
	}
	activeByMonth := make(map[string]int)
	for _, row := range insights.MonthlyPortProtocol {
		activeByMonth[row.Month] = row.ActiveConnections
	}
	for _, month := range []string{"2026-01", "2026-02", "2026-03"} {
		if activeByMonth[month] != 1 {
			t.Fatalf("active connections for %s = %d, want 1", month, activeByMonth[month])
		}
	}
}

func TestParseCSVAnalyticsWithCustomDimensions(t *testing.T) {
	t.Parallel()

	csvData := strings.Join([]string{
		"Source IP,Destination IP,Port,Protocol,Flows,Src BU,Dst BU,Src App,Dst App",
		"10.0.0.1,10.0.0.2,443,TCP,9,Finance,Operations,Payments,ERP",
	}, "\n")
	summary, insights, err := parseCSVAnalyticsWithDimensions(strings.NewReader(csvData), "BU", "app")
	if err != nil {
		t.Fatalf("parseCSVAnalyticsWithDimensions returned error: %v", err)
	}
	if len(summary) != 1 || summary[0].FlowCount != 9 {
		t.Fatalf("summary = %#v", summary)
	}
	if insights.PrimaryLabelKey != "BU" || insights.SecondaryLabelKey != "app" {
		t.Fatalf("analysis labels = %q/%q", insights.PrimaryLabelKey, insights.SecondaryLabelKey)
	}
	if len(insights.EnvMatrix) != 1 || insights.EnvMatrix[0].Source != "Finance" || insights.EnvMatrix[0].Destination != "Operations" {
		t.Fatalf("primary matrix = %#v", insights.EnvMatrix)
	}
	if len(insights.AppMatrix) != 1 || insights.AppMatrix[0].Source != "Payments" || insights.AppMatrix[0].Destination != "ERP" {
		t.Fatalf("secondary matrix = %#v", insights.AppMatrix)
	}
}

func TestParseCSVAnalyticsInputsCombinesMonthsAndDeduplicatesConnections(t *testing.T) {
	t.Parallel()

	header := "Source IP,Destination IP,Port,Protocol,Flows,Src BU,Dst BU,Src App,Dst App,First Detected,Last Detected,FQDN"
	january := strings.Join([]string{
		header,
		"10.0.0.1,10.0.0.2,443,TCP,3,Finance,Operations,Payments,ERP,01/05/26 01:15 PM,01/31/26 11:45 PM,api.example.com",
	}, "\n")
	february := strings.Join([]string{
		header,
		"10.0.0.1,10.0.0.2,443,TCP,5,Finance,Operations,Payments,ERP,2026-02-01 00:05:00,2026-02-28 22:00:00,api.example.com",
	}, "\n")

	summary, insights, err := parseCSVAnalyticsInputs([]csvAnalyticsInput{
		{Name: "january.csv", Reader: strings.NewReader(january)},
		{Name: "february.csv", Reader: strings.NewReader(february)},
	}, "BU", "app")
	if err != nil {
		t.Fatalf("parseCSVAnalyticsInputs returned error: %v", err)
	}
	if len(summary) != 1 || summary[0].FlowCount != 8 || summary[0].UniqueConnections != 1 {
		t.Fatalf("combined summary = %#v, want 8 flows and 1 unique connection", summary)
	}
	if len(insights.TopSourceIPs) != 1 || insights.TopSourceIPs[0].FlowCount != 8 || insights.TopSourceIPs[0].UniqueConnections != 1 {
		t.Fatalf("combined source ranking = %#v", insights.TopSourceIPs)
	}

	monthly := make(map[string]MonthlyPortProtocolSummary)
	for _, row := range insights.MonthlyPortProtocol {
		monthly[row.Month] = row
	}
	if monthly["2026-01"].FlowCount != 3 || monthly["2026-01"].UniqueConnections != 1 {
		t.Fatalf("January trend = %#v", monthly["2026-01"])
	}
	if monthly["2026-02"].FlowCount != 5 || monthly["2026-02"].UniqueConnections != 1 {
		t.Fatalf("February trend = %#v", monthly["2026-02"])
	}
}

func TestMonthlyPortProtocolDeduplicatesMatchingImportedConnections(t *testing.T) {
	t.Parallel()

	firstSeen := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	lastSeen := time.Date(2026, time.January, 31, 23, 0, 0, 0, time.UTC)
	rows := monthlyPortProtocolFromRecords([]AnalyticsRecord{
		{Identity: "same-connection", Month: "2026-01", Protocol: "TCP", Port: 443, FlowCount: 3, FirstSeen: firstSeen, LastSeen: lastSeen},
		{Identity: "same-connection", Month: "2026-01", Protocol: "TCP", Port: 443, FlowCount: 5, FirstSeen: firstSeen, LastSeen: lastSeen},
	})
	if len(rows) != 1 || rows[0].FlowCount != 8 || rows[0].UniqueConnections != 1 || rows[0].ActiveConnections != 1 {
		t.Fatalf("monthly rows = %#v, want additive flows and deduplicated connections", rows)
	}
}

func TestHandleImportCSVAcceptsMultipleFilesAndRejectsDuplicates(t *testing.T) {
	header := "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App,First Detected,Last Detected"
	january := header + "\n10.0.0.1,10.0.0.2,443,TCP,3,Prod,Prod,API,ERP,2026-01-01 00:00:00,2026-01-31 23:00:00"
	february := header + "\n10.0.0.1,10.0.0.2,443,TCP,5,Prod,Prod,API,ERP,2026-02-01 00:00:00,2026-02-28 23:00:00"

	state.Mu.Lock()
	previousSummary := state.LastSummary
	previousInsights := state.LastInsights
	previousFileName := state.FileName
	previousDone := state.IsDone
	previousCancelled := state.IsCancelled
	state.Mu.Unlock()
	t.Cleanup(func() {
		state.Mu.Lock()
		state.LastSummary = previousSummary
		state.LastInsights = previousInsights
		state.FileName = previousFileName
		state.IsDone = previousDone
		state.IsCancelled = previousCancelled
		state.Mu.Unlock()
	})

	request := newCSVImportRequest(t, []struct {
		name string
		data string
	}{{"january.csv", january}, {"february.csv", february}})
	recorder := httptest.NewRecorder()
	handleImportCSV(recorder, request)
	var response struct {
		Success   bool     `json:"success"`
		FileCount int      `json:"fileCount"`
		Files     []string `json:"files"`
		Error     string   `json:"error"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || response.FileCount != 2 || len(response.Files) != 2 {
		t.Fatalf("multi-file response = %#v", response)
	}

	duplicateRequest := newCSVImportRequest(t, []struct {
		name string
		data string
	}{{"first.csv", january}, {"copy.csv", january}})
	duplicateRecorder := httptest.NewRecorder()
	handleImportCSV(duplicateRecorder, duplicateRequest)
	response = struct {
		Success   bool     `json:"success"`
		FileCount int      `json:"fileCount"`
		Files     []string `json:"files"`
		Error     string   `json:"error"`
	}{}
	if err := json.NewDecoder(duplicateRecorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Success || !strings.Contains(response.Error, "duplicates the contents") {
		t.Fatalf("duplicate-file response = %#v", response)
	}
}

func newCSVImportRequest(t *testing.T, files []struct {
	name string
	data string
}) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(file.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://localhost:8000/api/results/import-csv", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://localhost:8000")
	return request
}

func TestParseCSVAnalyticsDistinguishesMissingCustomLabelFromExternal(t *testing.T) {
	t.Parallel()

	csvData := strings.Join([]string{
		"Source IP,Destination IP,Port,Protocol,Flows,Src BU,Dst BU,Src App,Dst App,Src Role,Dst Role",
		"10.0.0.1,10.0.0.2,443,TCP,3,,,Payments,ERP,Web,Database",
	}, "\n")
	_, insights, err := parseCSVAnalyticsWithDimensions(strings.NewReader(csvData), "BU", "app")
	if err != nil {
		t.Fatalf("parseCSVAnalyticsWithDimensions returned error: %v", err)
	}
	if len(insights.EnvMatrix) != 1 || insights.EnvMatrix[0].Source != "No BU Label" || insights.EnvMatrix[0].Destination != "No BU Label" {
		t.Fatalf("custom-label matrix = %#v", insights.EnvMatrix)
	}
	if len(insights.TrafficCategories) != 1 || insights.TrafficCategories[0].Name != "Internal -> Internal" {
		t.Fatalf("traffic categories = %#v", insights.TrafficCategories)
	}
}

func TestImportedManagedDestinationIsNotRankedAsExternal(t *testing.T) {
	t.Parallel()

	csvData := strings.Join([]string{
		"Source IP,Destination IP,Port,Protocol,Flows,Src BU,Dst BU,Src App,Dst App,Src OS,Dst OS",
		"198.51.100.10,204.99.40.91,21,TCP,6,,BU-PCW,,A-DISPENSING-TRISTAR,,OS-AIX",
		"204.99.43.130,204.99.40.91,21,TCP,9,BU-PCW,BU-PCW,A-MAIL-ORDER-LINKS,A-DISPENSING-TRISTAR,OS-AIX,OS-AIX",
		"204.99.43.130,203.0.113.25,443,TCP,4,BU-PCW,,A-MAIL-ORDER-LINKS,,OS-AIX,",
	}, "\n")
	_, insights, err := parseCSVAnalyticsWithDimensions(strings.NewReader(csvData), "BU", "app")
	if err != nil {
		t.Fatalf("parseCSVAnalyticsWithDimensions returned error: %v", err)
	}
	if len(insights.TopDestinationIPs) != 2 || insights.TopDestinationIPs[0].Name != "204.99.40.91" || insights.TopDestinationIPs[0].FlowCount != 15 {
		t.Fatalf("general top destinations = %#v", insights.TopDestinationIPs)
	}
	if len(insights.TopExternalDestinationIPs) != 1 {
		t.Fatalf("external destinations = %#v", insights.TopExternalDestinationIPs)
	}
	external := insights.TopExternalDestinationIPs[0]
	if external.Name != "203.0.113.25" || external.FlowCount != 4 || external.UniqueConnections != 1 {
		t.Fatalf("external destination = %#v", external)
	}
	for _, destination := range insights.TopExternalDestinationIPs {
		if destination.Name == "204.99.40.91" {
			t.Fatalf("labeled managed destination was ranked as external: %#v", destination)
		}
	}
}

func TestExternalDestinationViewsUseDedicatedDataset(t *testing.T) {
	t.Parallel()
	summaryPage, err := staticFiles.ReadFile("frontend/summary.html")
	if err != nil {
		t.Fatal(err)
	}
	executivePage, err := staticFiles.ReadFile("frontend/executive-summary.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summaryPage), "const externalDestinations = insights.top_external_destination_ips || [];") {
		t.Fatal("analytics external-destination list is not using the dedicated backend dataset")
	}
	for _, expected := range []string{
		"const topExternal = (insights.top_external_destination_ips || [])[0];",
		"renderExternalSpotlight(insights.top_external_destination_ips || []);",
	} {
		if !strings.Contains(string(executivePage), expected) {
			t.Fatalf("executive summary is missing dedicated external-destination usage %q", expected)
		}
	}
}

func TestCSVImportViewsAllowMultipleFiles(t *testing.T) {
	t.Parallel()

	for _, page := range []string{"frontend/index.html", "frontend/summary.html"} {
		content, err := staticFiles.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		html := string(content)
		if !strings.Contains(html, `type="file" accept=".csv,text/csv" multiple`) {
			t.Fatalf("%s does not expose a multiple CSV file picker", page)
		}
		if !strings.Contains(html, "formData.append('files', file)") {
			t.Fatalf("%s does not submit every selected CSV", page)
		}
	}
}

func TestApplicationHeadersUseConsistentNavigationAndThemeControls(t *testing.T) {
	t.Parallel()

	pages := []struct {
		file        string
		currentHref string
	}{
		{"frontend/index.html", "/blocked-traffic/"},
		{"frontend/summary.html", "/blocked-traffic/summary"},
		{"frontend/heatmaps.html", "/blocked-traffic/heatmaps"},
		{"frontend/executive-summary.html", "/blocked-traffic/executive-summary"},
		{"frontend/automation.html", "/blocked-traffic/automation"},
	}
	wantNavigation := []string{
		`href="/" class="app-nav-link"`,
		`href="/blocked-traffic/" class="app-nav-link"`,
		`href="/blocked-traffic/summary" class="app-nav-link"`,
		`href="/blocked-traffic/heatmaps" class="app-nav-link"`,
		`href="/blocked-traffic/executive-summary" class="app-nav-link"`,
		`href="/blocked-traffic/automation" class="app-nav-link"`,
	}

	for _, page := range pages {
		content, err := staticFiles.ReadFile(page.file)
		if err != nil {
			t.Fatal(err)
		}
		html := string(content)
		headerStart := strings.Index(html, `<header class="app-header`)
		headerEnd := strings.Index(html, `</header>`)
		if headerStart < 0 || headerEnd <= headerStart {
			t.Fatalf("%s is missing the shared application header", page.file)
		}
		header := html[headerStart:headerEnd]
		previousIndex := -1
		for _, navigation := range wantNavigation {
			index := strings.Index(header, navigation)
			if index <= previousIndex {
				t.Fatalf("%s navigation order is inconsistent at %q", page.file, navigation)
			}
			previousIndex = index
		}
		currentMarker := fmt.Sprintf(`href="%s" class="app-nav-link" aria-current="page"`, page.currentHref)
		if strings.Count(header, `aria-current="page"`) != 1 || !strings.Contains(header, currentMarker) {
			t.Fatalf("%s does not identify %s as the current page", page.file, page.currentHref)
		}
		for _, theme := range []string{"default", "illumio", "illumio-light"} {
			if !strings.Contains(header, fmt.Sprintf(`data-theme-option="%s"`, theme)) {
				t.Fatalf("%s is missing the %s theme option", page.file, theme)
			}
		}
		if strings.Contains(header, "theme-button active") {
			t.Fatalf("%s hard-codes a theme as active before saved-theme initialization", page.file)
		}
		if !strings.Contains(html, `<link rel="stylesheet" href="/blocked-traffic/assets/app-shell.css">`) {
			t.Fatalf("%s does not load the shared header styles", page.file)
		}
		if !strings.Contains(html, `<link rel="stylesheet" href="/static/product-shell.css">`) {
			t.Fatalf("%s does not load the unified product shell styles", page.file)
		}
		if !strings.Contains(html, `<script src="/static/product-shell.js" defer></script>`) {
			t.Fatalf("%s does not load the unified product shell behavior", page.file)
		}
		if !strings.Contains(html, `<script src="/blocked-traffic/assets/collapsible.js"></script>`) {
			t.Fatalf("%s does not load the shared collapsible-section behavior", page.file)
		}
		if !strings.Contains(html, `<script src="/blocked-traffic/assets/app-version.js"></script>`) {
			t.Fatalf("%s does not load the shared application-version footer", page.file)
		}
		if !strings.Contains(html, `data-auto-collapsible=`) {
			t.Fatalf("%s does not expose any collapsible sections", page.file)
		}
		themeScript := strings.Index(html, `<script src="/blocked-traffic/assets/theme-init.js"></script>`)
		if themeScript < 0 || themeScript > headerStart {
			t.Fatalf("%s does not initialize the saved theme before rendering its header", page.file)
		}
	}
	shell, err := staticFiles.ReadFile("frontend/app-shell.css")
	if err != nil {
		t.Fatal(err)
	}
	shellCSS := string(shell)
	for _, rule := range []string{"scrollbar-gutter: stable", ".app-header-action {\n        order: 1", ".app-nav {\n        order: 2", ".theme-switcher {\n        order: 3"} {
		if !strings.Contains(shellCSS, rule) {
			t.Fatalf("shared app shell is missing the stable header rule %q", rule)
		}
	}
	productShell, err := staticFiles.ReadFile("frontend/product-shell.js")
	if err != nil {
		t.Fatal(err)
	}
	productShellSource := string(productShell)
	for _, expected := range []string{"Monitoring", "Traffic", "Automation", "Administration", "product-shell-compact", "product-shell-menu-open", "illumio_product_theme", "product-shell-sidebar-tools", "product-shell-page-toolbar"} {
		if !strings.Contains(productShellSource, expected) {
			t.Fatalf("unified product shell is missing %q", expected)
		}
	}
	productShellCSS, err := staticFiles.ReadFile("frontend/product-shell.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{".product-shell-sidebar", ".product-shell-topbar", "@media (max-width: 1099px)", "@media print"} {
		if !strings.Contains(string(productShellCSS), expected) {
			t.Fatalf("unified product shell styles are missing %q", expected)
		}
	}
}

func TestUnifiedProductShellAssetsAreServed(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		fileName    string
		contentType string
		contains    string
	}{
		{"frontend/product-shell.css", "text/css; charset=utf-8", ".product-shell-sidebar"},
		{"frontend/product-shell.js", "text/javascript; charset=utf-8", "Illumio Operations Hub"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://localhost/assets/test", nil)
		serveEmbeddedAsset(recorder, request, test.fileName, test.contentType)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s returned HTTP %d", test.fileName, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != test.contentType {
			t.Fatalf("%s content type = %q, want %q", test.fileName, got, test.contentType)
		}
		if !strings.Contains(recorder.Body.String(), test.contains) {
			t.Fatalf("%s response is missing %q", test.fileName, test.contains)
		}
	}
}

func TestExecutiveHTMLExportKeepsOfflineInteractions(t *testing.T) {
	t.Parallel()

	content, err := staticFiles.ReadFile("frontend/executive-summary.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, expected := range []string{
		"window.__ITT_EXECUTIVE_PAYLOAD__",
		"collapsibleSource = await (await fetch('/blocked-traffic/assets/collapsible.js')).text()",
		"body.querySelectorAll('.app-nav, .app-header-action, [data-offline-remove], script')",
		"setExportFilter(body, selectedSections)",
		"initializeSectionImageActions()",
		"downloadSectionImage(section.dataset.imageSection, button)",
		"window.__ITT_EXECUTIVE_THEME__",
		"body.dataset.theme = exportTheme",
		"if (latestExecutivePayload) renderTrendCharts()",
		"background-color: var(--panel-strong)",
		"color-scheme: dark",
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("executive HTML export is missing offline behavior %q", expected)
		}
	}
	if strings.Contains(html, "clone.querySelectorAll('.app-nav, .app-header-action, .no-export')") {
		t.Fatal("executive HTML export removes interactive report controls")
	}
	if count := strings.Count(html, `data-export-section=`); count != len(executiveReportSectionOrder) {
		t.Fatalf("executive report exposes %d selectable sections, want %d", count, len(executiveReportSectionOrder))
	}
	if count := strings.Count(html, `data-image-section=`); count != len(executiveReportSectionOrder)+5 {
		t.Fatalf("executive report exposes %d image sections, want %d", count, len(executiveReportSectionOrder)+5)
	}
	collapsible, err := staticFiles.ReadFile("frontend/collapsible.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(collapsible), "window.ITTSections = { initialize, apply }") {
		t.Fatal("shared collapsible behavior does not expose offline initialization")
	}
}

func TestHandleVersionReportsBuildVersion(t *testing.T) {
	previousVersion := appVersion
	appVersion = "v9.8.7-test"
	t.Cleanup(func() { appVersion = previousVersion })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	handleVersion(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("handleVersion status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("version Cache-Control = %q", got)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["version"] != "v9.8.7-test" {
		t.Fatalf("version payload = %#v", payload)
	}

	methodRecorder := httptest.NewRecorder()
	handleVersion(methodRecorder, httptest.NewRequest(http.MethodPost, "/api/version", nil))
	if methodRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST version status = %d", methodRecorder.Code)
	}
}

func TestApplicationVersionFooterUsesVersionAPI(t *testing.T) {
	t.Parallel()
	content, err := staticFiles.ReadFile("frontend/app-version.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	for _, expected := range []string{"app-version-footer", "payload.version", "'/blocked-traffic/api/version'"} {
		if !strings.Contains(source, expected) {
			t.Fatalf("application version footer is missing %q", expected)
		}
	}
}

func TestResolveConfigCredentialsUsesServerSideProfile(t *testing.T) {
	state.Mu.Lock()
	previousProfiles := state.Profiles
	state.Profiles = map[string]PCEProfile{
		"prod": {PCEURL: "https://pce.example.com", OrgID: "7", APIKey: "stored-key", APISecret: "stored-secret"},
	}
	state.Mu.Unlock()
	t.Cleanup(func() {
		state.Mu.Lock()
		state.Profiles = previousProfiles
		state.Mu.Unlock()
	})

	cfg, err := resolveConfigCredentials(Config{ProfileName: "prod", PCEURL: "https://evil.example", OrgID: "9"})
	if err != nil {
		t.Fatalf("resolveConfigCredentials returned error: %v", err)
	}
	if cfg.PCEURL != "https://pce.example.com" || cfg.OrgID != "7" || cfg.APIKey != "stored-key" || cfg.APISecret != "stored-secret" {
		t.Fatalf("resolved config = %#v", cfg)
	}
}

func TestResolveConfigCredentialsRejectsRequestCredentialsWithoutSavedProfile(t *testing.T) {
	_, err := resolveConfigCredentials(Config{
		PCEURL: "https://attacker.example", OrgID: "1", APIKey: "request-key", APISecret: "request-secret",
	})
	if err == nil || !strings.Contains(err.Error(), "save and select") {
		t.Fatalf("missing-profile error = %v", err)
	}
}

func TestServeEmbeddedHTMLAddsMatchingCSPNonce(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	serveEmbeddedHTML(recorder, "frontend/index.html")
	response := recorder.Result()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("serveEmbeddedHTML status = %d", response.StatusCode)
	}
	policy := response.Header.Get("Content-Security-Policy")
	marker := "'nonce-"
	start := strings.Index(policy, marker)
	if start < 0 {
		t.Fatalf("CSP does not contain a nonce: %q", policy)
	}
	start += len(marker)
	end := strings.Index(policy[start:], "'")
	if end < 0 {
		t.Fatalf("CSP nonce is malformed: %q", policy)
	}
	nonce := policy[start : start+end]
	if !strings.Contains(recorder.Body.String(), `<script nonce="`+nonce+`">`) {
		t.Fatal("HTML script nonce does not match the CSP nonce")
	}
	if strings.Contains(policy, "script-src 'self' 'unsafe-inline'") {
		t.Fatal("script CSP should not allow unsafe-inline")
	}
}

func TestSecurityHeadersRejectsNonLoopbackDashboardHost(t *testing.T) {
	t.Parallel()

	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	request.Host = "example.com"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-loopback dashboard request status = %d, want 403", recorder.Code)
	}
}

func TestSecurityHeadersRejectsSpoofedLocalhostFromRemoteClient(t *testing.T) {
	t.Parallel()

	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://localhost:18443/", nil)
	request.Host = "localhost:18443"
	request.RemoteAddr = "192.0.2.25:54321"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("spoofed localhost request status = %d, want 403", recorder.Code)
	}
}

func TestSecurityHeadersAllowLoopbackHostAndRemote(t *testing.T) {
	t.Parallel()

	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://localhost:18443/", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("loopback request status = %d, want 204", recorder.Code)
	}
}

func TestTrafficScopeDefaultsAndPolicyDecisionFilters(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		input        string
		wantScope    string
		wantDecision []string
	}{
		{"", trafficScopeBlocked, []string{"blocked"}},
		{"blocked", trafficScopeBlocked, []string{"blocked"}},
		{"ALL", trafficScopeAll, []string{}},
	} {
		scope, err := normalizeTrafficScope(test.input)
		if err != nil {
			t.Fatalf("normalizeTrafficScope(%q): %v", test.input, err)
		}
		if scope != test.wantScope {
			t.Fatalf("normalizeTrafficScope(%q) = %q, want %q", test.input, scope, test.wantScope)
		}
		decisions := policyDecisionsForScope(scope)
		if len(decisions) != len(test.wantDecision) {
			t.Fatalf("policy decisions for %q = %#v, want %#v", scope, decisions, test.wantDecision)
		}
		for index := range decisions {
			if decisions[index] != test.wantDecision[index] {
				t.Fatalf("policy decisions for %q = %#v, want %#v", scope, decisions, test.wantDecision)
			}
		}
	}
	if _, err := normalizeTrafficScope("allowed-only"); err == nil {
		t.Fatal("unsupported traffic scope was accepted")
	}
}

func TestTrafficScopeSelectorsAreAvailableForManualAndAutomatedRuns(t *testing.T) {
	t.Parallel()

	for _, fileName := range []string{"frontend/index.html", "frontend/automation.html"} {
		page, err := staticFiles.ReadFile(fileName)
		if err != nil {
			t.Fatal(err)
		}
		html := string(page)
		for _, expected := range []string{`value="blocked"`, `value="all"`, "traffic_scope"} {
			if !strings.Contains(html, expected) {
				t.Fatalf("%s is missing %q", fileName, expected)
			}
		}
	}
}

func TestServiceExclusionsAndProgressHeartbeatAreExposedInUI(t *testing.T) {
	t.Parallel()
	manual, err := staticFiles.ReadFile("frontend/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`id="exclude_services"`, "exclude_services:", "activeChunks", "lastProgressAt", "PCE activity"} {
		if !strings.Contains(string(manual), expected) {
			t.Fatalf("manual extractor UI is missing %q", expected)
		}
	}
	automationPage, err := staticFiles.ReadFile("frontend/automation.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`id="templateExcludeServices"`, "exclude_services:"} {
		if !strings.Contains(string(automationPage), expected) {
			t.Fatalf("automation UI is missing %q", expected)
		}
	}
}
