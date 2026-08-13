package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func registerAutomationHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/automation/state", handleAutomationState)
	mux.HandleFunc("/api/automation/templates/save", handleAutomationTemplateSave)
	mux.HandleFunc("/api/automation/templates/delete", handleAutomationTemplateDelete)
	mux.HandleFunc("/api/automation/templates/export", handleAutomationTemplateExport)
	mux.HandleFunc("/api/automation/templates/import", handleAutomationTemplateImport)
	mux.HandleFunc("/api/automation/runs/start", handleAutomationRunStart)
	mux.HandleFunc("/api/automation/runs/cancel", handleAutomationRunCancel)
	mux.HandleFunc("/api/automation/runs/artifact", handleAutomationRunArtifact)
	mux.HandleFunc("/api/automation/destinations/save", handleAutomationDestinationSave)
	mux.HandleFunc("/api/automation/destinations/delete", handleAutomationDestinationDelete)
	mux.HandleFunc("/api/automation/destinations/test", handleAutomationDestinationTest)
}

func handleAutomationState(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	automation.mu.Lock()
	templates := make([]ReportTemplate, 0, len(automation.data.Templates))
	for _, template := range automation.data.Templates {
		templates = append(templates, template)
	}
	sort.Slice(templates, func(i, j int) bool { return strings.ToLower(templates[i].Name) < strings.ToLower(templates[j].Name) })
	destinations := sortedPublicDestinations(automation.data.Destinations)
	runs := append([]AutomationRun{}, automation.data.Runs...)
	automation.mu.Unlock()
	if len(runs) > 200 {
		runs = runs[:200]
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "templates": templates, "destinations": destinations, "runs": runs,
		"timezone": time.Local.String(),
	})
}

func handleAutomationTemplateSave(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var template ReportTemplate
	if err := decodeJSONBody(w, r, &template); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := automation.saveTemplate(template)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "template": saved})
}

func handleAutomationTemplateDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := automation.deleteTemplate(request.ID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}

func findTemplate(id string) (ReportTemplate, bool) {
	automation.mu.Lock()
	defer automation.mu.Unlock()
	if template, ok := automation.data.Templates[id]; ok {
		return template, true
	}
	for _, template := range automation.data.Templates {
		if strings.EqualFold(template.Name, id) {
			return template, true
		}
	}
	return ReportTemplate{}, false
}

func handleAutomationTemplateExport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	template, ok := findTemplate(strings.TrimSpace(r.URL.Query().Get("id")))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "template not found")
		return
	}
	template.ID = ""
	template.CreatedAt = time.Time{}
	template.UpdatedAt = time.Time{}
	template.DeliveryDestination = []string{}
	template.Schedule.NextRunAt = time.Time{}
	template.Schedule.LastScheduledAt = time.Time{}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": sanitizeTemplateName(template.Name) + ".json"}))
	_ = json.NewEncoder(w).Encode(template)
}

func handleAutomationTemplateImport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var template ReportTemplate
	if err := decodeJSONBody(w, r, &template); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	template.ID = ""
	template.CreatedAt = time.Time{}
	template.UpdatedAt = time.Time{}
	template.DeliveryDestination = []string{}
	template.Schedule.Enabled = false
	template.Schedule.NextRunAt = time.Time{}
	template.Schedule.LastScheduledAt = time.Time{}
	saved, err := automation.saveTemplate(template)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "template": saved})
}

func handleAutomationRunStart(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var request struct {
		TemplateID string `json:"template_id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := automation.queueRun(request.TemplateID, "manual")
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "run": run})
}

func handleAutomationRunCancel(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var request struct {
		RunID string `json:"run_id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := automation.cancelQueuedRun(request.RunID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}

func handleAutomationRunArtifact(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	runID := strings.TrimSpace(r.URL.Query().Get("id"))
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind == "" {
		kind = "csv"
	}
	automation.mu.Lock()
	var artifactPath string
	for _, run := range automation.data.Runs {
		if run.ID == runID && run.Status == "completed" {
			if kind == "csv" {
				artifactPath = run.ArtifactPath
			} else {
				for _, candidate := range run.AdditionalArtifactPaths {
					if strings.EqualFold(strings.TrimPrefix(filepath.Ext(candidate), "."), kind) {
						artifactPath = candidate
						break
					}
				}
			}
			break
		}
	}
	automation.mu.Unlock()
	if artifactPath == "" || !filepath.IsAbs(artifactPath) {
		writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}
	file, err := os.Open(artifactPath)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "artifact is no longer available")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeJSONError(w, http.StatusNotFound, "artifact is unavailable")
		return
	}
	contentType := "application/octet-stream"
	if kind == "csv" {
		contentType = "text/csv; charset=utf-8"
	}
	if kind == "html" {
		contentType = "text/html; charset=utf-8"
	}
	if kind == "pdf" {
		contentType = "application/pdf"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(artifactPath)}))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(artifactPath), info.ModTime(), file)
}

func handleAutomationDestinationSave(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var destination DeliveryDestination
	if err := decodeJSONBody(w, r, &destination); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := automation.saveDestination(destination)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "destination": saved.public()})
}

func handleAutomationDestinationDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := automation.deleteDestination(request.ID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}

func handleAutomationDestinationTest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), deliveryTimeout)
	defer cancel()
	if err := automation.testDestination(ctx, request.ID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}

func waitForAutomationRun(ctx context.Context, runID string) (AutomationRun, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		automation.mu.Lock()
		index := automation.runIndexLocked(runID)
		if index >= 0 {
			run := automation.data.Runs[index]
			automation.mu.Unlock()
			switch run.Status {
			case "completed":
				return run, nil
			case "failed", "cancelled":
				return run, fmt.Errorf("automation run %s: %s", run.Status, run.Error)
			}
		} else {
			automation.mu.Unlock()
			return AutomationRun{}, fmt.Errorf("automation run disappeared from history")
		}
		select {
		case <-ctx.Done():
			return AutomationRun{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
