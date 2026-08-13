package extractor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestExpandFileNamePattern(t *testing.T) {
	t.Parallel()
	template := ReportTemplate{Name: "Weekly BU / App Report"}
	when := time.Date(2026, time.August, 5, 14, 3, 4, 0, time.UTC)
	got := expandFileNamePattern("blocked-{template}-{date}-{time}-{run_id}", template, when, "run-123")
	want := "blocked-Weekly-BU-App-Report-2026-08-05-140304-run-123.csv"
	if got != want {
		t.Fatalf("expandFileNamePattern = %q, want %q", got, want)
	}
}

func TestScheduleExpressionsAndNextRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		schedule RunSchedule
		want     string
	}{
		{RunSchedule{Kind: "daily", TimeOfDay: "06:15"}, "15 6 * * *"},
		{RunSchedule{Kind: "weekdays", TimeOfDay: "07:00"}, "0 7 * * 1-5"},
		{RunSchedule{Kind: "weekly", TimeOfDay: "08:30", Weekday: 2}, "30 8 * * 2"},
		{RunSchedule{Kind: "monthly", TimeOfDay: "09:45", DayOfMonth: 15}, "45 9 15 * *"},
		{RunSchedule{Kind: "cron", CronExpression: "5 4 * * 1"}, "5 4 * * 1"},
	}
	for _, test := range tests {
		got, err := scheduleExpression(test.schedule)
		if err != nil {
			t.Fatalf("scheduleExpression(%#v): %v", test.schedule, err)
		}
		if got != test.want {
			t.Fatalf("scheduleExpression(%#v) = %q, want %q", test.schedule, got, test.want)
		}
	}
	after := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	next, err := scheduleNext(RunSchedule{Kind: "daily", TimeOfDay: "06:00", Timezone: "America/Chicago"}, after)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := next.Format(time.RFC3339), "2026-08-06T11:00:00Z"; got != want {
		t.Fatalf("next run = %s, want %s", got, want)
	}
}

func TestValidateScheduleRejectsUnsafeValues(t *testing.T) {
	t.Parallel()
	for _, schedule := range []RunSchedule{
		{Enabled: true, Kind: "daily", TimeOfDay: "25:00", Timezone: "UTC"},
		{Enabled: true, Kind: "monthly", TimeOfDay: "01:00", DayOfMonth: 31, Timezone: "UTC"},
		{Enabled: true, Kind: "cron", CronExpression: "not cron", Timezone: "UTC"},
	} {
		if err := validateSchedule(&schedule); err == nil {
			t.Fatalf("validateSchedule(%#v) unexpectedly succeeded", schedule)
		}
	}
}

func TestPublicDeliveryDestinationRedactsSecrets(t *testing.T) {
	t.Parallel()
	destination := DeliveryDestination{
		ID: "dst-1", Name: "Slack", Type: "slack_api", EndpointURL: "https://hooks.slack.com/services/secret",
		Headers: map[string]string{"Authorization": "Bearer header-secret"}, Token: "token-secret",
		SMTPPassword: "smtp-secret", SFTPPassword: "sftp-secret",
	}
	encoded, err := json.Marshal(destination.public())
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	for _, secret := range []string{"services/secret", "header-secret", "token-secret", "smtp-secret", "sftp-secret"} {
		if strings.Contains(value, secret) {
			t.Fatalf("public destination leaked %q: %s", secret, value)
		}
	}
	if !strings.Contains(value, "Authorization") || !strings.Contains(value, `"has_token":true`) {
		t.Fatalf("public destination omitted safe secret-presence metadata: %s", value)
	}
}

func TestGenericWebhookBase64Delivery(t *testing.T) {
	t.Parallel()
	artifact := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(artifact, []byte("Port,Flows\n443,7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ITT-Run-ID") != "run-test" {
			t.Errorf("X-ITT-Run-ID = %q", r.Header.Get("X-ITT-Run-ID"))
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		received <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	destination := DeliveryDestination{Type: "generic_webhook", EndpointURL: server.URL, AllowPrivateNetwork: true, WebhookMode: "base64_file"}
	err := deliverGenericWebhook(context.Background(), destination, deliveryMessage{RunID: "run-test", Title: "Done", Text: "Completed", ArtifactPath: artifact})
	if err != nil {
		t.Fatal(err)
	}
	payload := <-received
	decoded, err := base64.StdEncoding.DecodeString(payload["file_base64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "Port,Flows\n443,7\n" {
		t.Fatalf("decoded artifact = %q", decoded)
	}
}

func TestOutboundWebhookBlocksPrivateTargetsByDefault(t *testing.T) {
	t.Parallel()
	if _, err := outboundHTTPClient(DeliveryDestination{EndpointURL: "https://127.0.0.1/webhook"}); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("private target error = %v", err)
	}
	if _, err := validateOutboundURLSyntax("http://example.com/webhook", false); err == nil {
		t.Fatal("plain HTTP public webhook should be rejected")
	}
}

func TestSharedFolderDeliveryDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	source := filepath.Join(sourceDir, "report.csv")
	if err := os.WriteFile(source, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyArtifactNoOverwrite(source, destinationDir); err != nil {
		t.Fatal(err)
	}
	if err := copyArtifactNoOverwrite(source, destinationDir); err == nil {
		t.Fatal("second delivery should refuse to overwrite the first artifact")
	}
}

func TestSharedFolderDeliveryRejectsOversizedArtifact(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "large.csv")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxDeliveryArtifactSize + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	err = copyArtifactNoOverwrite(source, destination)
	if err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("oversized artifact error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "large.csv")); !os.IsNotExist(err) {
		t.Fatalf("oversized artifact should not be copied, stat error = %v", err)
	}
}

func TestShouldDeliverRunPolicies(t *testing.T) {
	t.Parallel()
	template := ReportTemplate{AlertPolicy: AlertPolicy{Mode: "on_change", OnNewRelationships: true, DeliverManualRuns: true}}
	if deliver, _ := shouldDeliverRun(template, "manual", RunMetrics{PreviousCompletedRunID: "run-old"}); deliver {
		t.Fatal("unchanged run should not be delivered")
	}
	if deliver, _ := shouldDeliverRun(template, "manual", RunMetrics{PreviousCompletedRunID: "run-old", NewRelationships: []string{"A -> B"}}); !deliver {
		t.Fatal("new relationship should trigger delivery")
	}
	template.AlertPolicy.DeliverManualRuns = false
	if deliver, reason := shouldDeliverRun(template, "manual", RunMetrics{NewRelationships: []string{"A -> B"}}); deliver || !strings.Contains(reason, "manual") {
		t.Fatalf("manual delivery = %v, reason %q", deliver, reason)
	}
}

func TestCalculateRunMetricsFindsChanges(t *testing.T) {
	t.Parallel()
	manager := &AutomationManager{data: automationStoreData{Runs: []AutomationRun{{
		ID: "old", TemplateID: "tpl", Status: "completed", Metrics: RunMetrics{
			TotalFlows: 10, PrimaryRelationships: []string{"A -> B"}, ObservedServices: []string{"TCP:443"},
		},
	}}}}
	metrics := manager.calculateMetrics("tpl", "new", []PortProtocolSummary{{Protocol: "TCP", Port: 443, FlowCount: 10}, {Protocol: "TCP", Port: 8443, FlowCount: 5}}, AnalyticsInsights{
		EnvMatrix: []MatrixSummary{{Source: "A", Destination: "B"}, {Source: "B", Destination: "C"}},
	})
	if metrics.FlowChangePercent != 50 || !reflect.DeepEqual(metrics.NewRelationships, []string{"B -> C"}) || !reflect.DeepEqual(metrics.NewServices, []string{"TCP:8443"}) {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestBuildEmailMessageIncludesAttachment(t *testing.T) {
	t.Parallel()
	artifact := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(artifact, []byte("a,b\n1,2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := buildEmailMessage(DeliveryDestination{SMTPFrom: "sender@example.com", SMTPTo: []string{"recipient@example.com"}}, deliveryMessage{Title: "Report", Text: "Attached", ArtifactPath: artifact})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	if !strings.Contains(text, `filename="report.csv"`) || !strings.Contains(text, base64.StdEncoding.EncodeToString([]byte("a,b\n1,2\n"))) {
		t.Fatalf("email payload missing attachment: %s", text)
	}
}

func TestGenericWebhookMultipartDelivery(t *testing.T) {
	t.Parallel()
	artifact := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(artifact, []byte("Port,Flows\n443,7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var artifactBody string
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				break
			}
			body, _ := io.ReadAll(part)
			if part.FormName() == "file" {
				if part.FileName() != "report.csv" {
					t.Errorf("filename = %q", part.FileName())
				}
				artifactBody = string(body)
			}
		}
		received <- artifactBody
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	destination := DeliveryDestination{Type: "generic_webhook", EndpointURL: server.URL, AllowPrivateNetwork: true, WebhookMode: "multipart"}
	if err := deliverGenericWebhook(context.Background(), destination, deliveryMessage{RunID: "run-test", ArtifactPath: artifact}); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "Port,Flows\n443,7\n" {
		t.Fatalf("multipart artifact = %q", got)
	}
}

func TestAutomationStorePermissionsAndRestartRecovery(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	store := automationStoreData{
		Version:      automationStoreVersion,
		Templates:    map[string]ReportTemplate{"tpl": {ID: "tpl", Name: "Stored"}},
		Destinations: map[string]DeliveryDestination{},
		Runs:         []AutomationRun{{ID: "running", Status: "running"}, {ID: "queued", Status: "queued"}},
	}
	path, err := automationStorePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(path, store); err != nil {
		t.Fatal(err)
	}
	manager := &AutomationManager{}
	if err := manager.load(); err != nil {
		t.Fatal(err)
	}
	if manager.data.Runs[0].Status != "failed" || !strings.Contains(manager.data.Runs[0].Error, "stopped") {
		t.Fatalf("interrupted run was not recovered as failed: %#v", manager.data.Runs[0])
	}
	if manager.data.Runs[1].Status != "queued" {
		t.Fatalf("queued run was not preserved: %#v", manager.data.Runs[1])
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("automation store permissions = %o, want 600", info.Mode().Perm())
	}
	store.Version = automationStoreVersion + 1
	if err := writePrivateJSON(path, store); err != nil {
		t.Fatal(err)
	}
	if err := (&AutomationManager{}).load(); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("future store version error = %v", err)
	}
}

func TestRetentionOnlyRemovesTrackedFilesInsideOutputFolder(t *testing.T) {
	t.Parallel()
	output := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.csv")
	newest := filepath.Join(output, "newest.csv")
	old := filepath.Join(output, "old.csv")
	oldHTML := filepath.Join(output, "old-executive.html")
	oldPDF := filepath.Join(output, "old-executive.pdf")
	for _, path := range []string{outside, newest, old, oldHTML, oldPDF} {
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &AutomationManager{data: automationStoreData{Runs: []AutomationRun{
		{ID: "new", TemplateID: "tpl", Status: "completed", ArtifactPath: newest},
		{ID: "old", TemplateID: "tpl", Status: "completed", ArtifactPath: old, AdditionalArtifactPaths: []string{oldHTML, oldPDF}},
		{ID: "outside", TemplateID: "tpl", Status: "completed", ArtifactPath: outside},
	}}}
	manager.applyRetention(ReportTemplate{ID: "tpl", SavePath: output, RetentionCount: 1})
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newest report should remain: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old report should be removed, stat error = %v", err)
	}
	for _, path := range []string{oldHTML, oldPDF} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("old executive artifact %s should be removed, stat error = %v", path, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside report must remain: %v", err)
	}
}

func TestDeliveryValidationAndErrorRedaction(t *testing.T) {
	t.Parallel()
	for _, destination := range []DeliveryDestination{
		{Name: "bad\nname", Type: "shared_folder", FolderPath: t.TempDir()},
		{Name: "mail", Type: "email", SMTPHost: "mail.example\ninvalid", SMTPFrom: "a@example.com", SMTPTo: []string{"b@example.com"}},
		{Name: "slack", Type: "slack_api", Token: "secret", ChannelID: "C123\ninvalid"},
	} {
		if err := validateDestination(&destination); err == nil {
			t.Fatalf("destination unexpectedly validated: %#v", destination)
		}
	}
	destination := DeliveryDestination{EndpointURL: "https://secret.example/hook", Token: "token", Headers: map[string]string{"Authorization": "header-secret"}}
	got := redactDeliveryError("https://secret.example/hook token header-secret", destination)
	for _, secret := range []string{"secret.example", "token", "header-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted error leaked %q: %s", secret, got)
		}
	}
}

func TestDestinationTestRedactsWebhookURL(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL + "/secret-hook-path"
	server.Close()
	manager := &AutomationManager{data: automationStoreData{Destinations: map[string]DeliveryDestination{"dst": {
		ID: "dst", Type: "generic_webhook", EndpointURL: endpoint, AllowPrivateNetwork: true, WebhookMode: "notification",
	}}}}
	err := manager.testDestination(context.Background(), "dst")
	if err == nil {
		t.Fatal("closed webhook unexpectedly passed its destination test")
	}
	if strings.Contains(err.Error(), "secret-hook-path") || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("destination test leaked webhook URL: %v", err)
	}
}

func TestAutomationStateDoesNotExposeDestinationSecrets(t *testing.T) {
	previous := automation
	automation = &AutomationManager{data: automationStoreData{
		Templates: map[string]ReportTemplate{},
		Destinations: map[string]DeliveryDestination{"dst": {
			ID: "dst", Name: "Webhook", Type: "generic_webhook", EndpointURL: "https://example.com/private-token",
			Headers: map[string]string{"Authorization": "header-secret"}, Token: "bot-secret", SMTPPassword: "smtp-secret",
		}},
	}}
	t.Cleanup(func() { automation = previous })
	recorder := httptest.NewRecorder()
	handleAutomationState(recorder, httptest.NewRequest(http.MethodGet, "/api/automation/state", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("state status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), `"runs":null`) {
		t.Fatalf("empty run history must be a JSON array: %s", recorder.Body.String())
	}
	for _, secret := range []string{"private-token", "header-secret", "bot-secret", "smtp-secret"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("automation state leaked %q: %s", secret, recorder.Body.String())
		}
	}
}

func TestTemplateValidationRejectsExcessiveChunkCount(t *testing.T) {
	state.Mu.Lock()
	previousProfiles := state.Profiles
	state.Profiles = map[string]PCEProfile{"test": {Name: "test"}}
	state.Mu.Unlock()
	t.Cleanup(func() {
		state.Mu.Lock()
		state.Profiles = previousProfiles
		state.Mu.Unlock()
	})
	template := ReportTemplate{
		Name: "Too many chunks", ProfileName: "test", Days: 10000, ChunkInterval: "1h",
		AnalysisPrimary: "env", AnalysisSecondary: "app", SavePath: t.TempDir(), FileNamePattern: "report.csv",
	}
	if err := validateTemplate(&template); err == nil || !strings.Contains(err.Error(), "chunks") {
		t.Fatalf("chunk-count validation error = %v", err)
	}
}

func TestSchedulerQueuesMissedRunAndAdvancesSchedule(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	manager := &AutomationManager{
		data: automationStoreData{
			Version: automationStoreVersion,
			Templates: map[string]ReportTemplate{"tpl": {
				ID: "tpl", Name: "Scheduled", Schedule: RunSchedule{
					Enabled: true, Kind: "daily", TimeOfDay: "06:00", Timezone: "UTC",
					MissedRunPolicy: "run_once", OverlapPolicy: "queue", NextRunAt: now.Add(-time.Hour),
				},
			}},
			Destinations: map[string]DeliveryDestination{}, Runs: []AutomationRun{},
		},
		queue: make(chan string, 2), wakeSchedule: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	manager.processSchedules(now, true)
	if len(manager.data.Runs) != 1 || manager.data.Runs[0].Status != "queued" || manager.data.Runs[0].Trigger != "schedule" {
		t.Fatalf("missed schedule did not queue one run: %#v", manager.data.Runs)
	}
	if !manager.data.Templates["tpl"].Schedule.NextRunAt.After(now) {
		t.Fatalf("schedule did not advance: %#v", manager.data.Templates["tpl"].Schedule)
	}
	select {
	case id := <-manager.queue:
		if id != manager.data.Runs[0].ID {
			t.Fatalf("queued ID = %q, want %q", id, manager.data.Runs[0].ID)
		}
	default:
		t.Fatal("scheduled run was not sent to the persistent worker queue")
	}
}

func TestAutomationUsesExtractorThemePalette(t *testing.T) {
	t.Parallel()
	indexData, err := staticFiles.ReadFile("frontend/index.html")
	if err != nil {
		t.Fatal(err)
	}
	automationData, err := staticFiles.ReadFile("frontend/automation.html")
	if err != nil {
		t.Fatal(err)
	}
	selectors := []string{":root", `body[data-theme="illumio"]`, `body[data-theme="illumio-light"]`}
	variables := []string{"bg", "shell", "panel-soft", "field", "field-border", "border", "text", "muted", "title", "accent", "accent-strong", "secondary", "warn", "danger"}
	for _, selector := range selectors {
		indexTheme := cssVariablesForSelector(t, string(indexData), selector)
		automationTheme := cssVariablesForSelector(t, string(automationData), selector)
		for _, variable := range variables {
			indexValue, indexOK := indexTheme[variable]
			automationValue, automationOK := automationTheme[variable]
			if indexOK != automationOK || indexValue != automationValue {
				t.Errorf("%s --%s = %q, want extractor value %q", selector, variable, automationValue, indexValue)
			}
		}
	}
}

func TestScheduledExecutiveArtifactsAreValidAndPrivate(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	csvPath := filepath.Join(directory, "monthly.csv")
	if err := os.WriteFile(csvPath, []byte("header\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	template := ReportTemplate{
		Name: "Monthly Review", GenerateExecutiveHTML: true, GenerateExecutivePDF: true,
		ReportCustomer: "Example Customer", ReportPreparedBy: "Security Team", ReportNotes: "Review new services.",
	}
	summary := []PortProtocolSummary{{Protocol: "tcp", Port: 443, FlowCount: 1250, UniqueConnections: 17}}
	insights := AnalyticsInsights{
		MonthlyPortProtocol: []MonthlyPortProtocolSummary{{Month: "2026-07", Protocol: "tcp", Port: 443, FlowCount: 500, UniqueConnections: 8}, {Month: "2026-08", Protocol: "tcp", Port: 443, FlowCount: 750, UniqueConnections: 9}},
		EnvMatrix:           []MatrixSummary{{Source: "Production", Destination: "Shared Services", FlowCount: 700}},
	}
	paths, err := generateScheduledExecutiveArtifacts(csvPath, template, summary, insights, DatasetCoverage{})
	if err != nil {
		t.Fatalf("generateScheduledExecutiveArtifacts: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("artifact paths = %#v, want HTML and PDF", paths)
	}
	htmlData, err := os.ReadFile(paths[0])
	if err != nil || !strings.Contains(string(htmlData), "Monthly Review Executive Summary") || !strings.Contains(string(htmlData), "2026-08") {
		t.Fatalf("generated HTML is incomplete: err=%v", err)
	}
	pdfData, err := os.ReadFile(paths[1])
	if err != nil || !strings.HasPrefix(string(pdfData), "%PDF-1.4") || !strings.HasSuffix(string(pdfData), "%%EOF\n") {
		t.Fatalf("generated PDF is invalid: err=%v", err)
	}
	for _, path := range paths {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat artifact %s: %v", path, statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact %s mode = %v", path, info.Mode().Perm())
		}
	}
}

func cssVariablesForSelector(t *testing.T, html, selector string) map[string]string {
	t.Helper()
	blockPattern := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{([^}]+)\}`)
	match := blockPattern.FindStringSubmatch(html)
	if len(match) != 2 {
		t.Fatalf("CSS selector %q was not found", selector)
	}
	declarationPattern := regexp.MustCompile(`--([a-z0-9-]+)\s*:\s*([^;]+);`)
	variables := map[string]string{}
	for _, declaration := range declarationPattern.FindAllStringSubmatch(match[1], -1) {
		variables[declaration[1]] = strings.TrimSpace(declaration[2])
	}
	return variables
}
