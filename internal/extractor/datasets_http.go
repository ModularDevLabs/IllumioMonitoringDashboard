package extractor

import (
	"encoding/json"
	"net/http"
	"strings"
)

func registerDatasetHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/datasets", handleDatasetList)
	mux.HandleFunc("/api/datasets/save", handleDatasetSave)
	mux.HandleFunc("/api/datasets/load", handleDatasetLoad)
	mux.HandleFunc("/api/datasets/delete", handleDatasetDelete)
	mux.HandleFunc("/api/results/report-metadata", handleReportMetadataSave)
}

func handleDatasetList(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	state.Mu.Lock()
	currentID := state.DatasetID
	state.Mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "datasets": datasetManager.list(), "current_id": currentID})
}

func handleDatasetSave(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var request struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	state.Mu.Lock()
	dataset := SavedDataset{
		ID: strings.TrimSpace(request.ID), Name: request.Name, FileName: state.FileName,
		Summary: append([]PortProtocolSummary(nil), state.LastSummary...), Insights: state.LastInsights,
		Coverage: state.DatasetCoverage, Report: state.ReportMetadata,
	}
	state.Mu.Unlock()
	if dataset.FileName == "" || len(dataset.Summary) == 0 {
		writeJSONError(w, http.StatusBadRequest, "no analytics dataset is currently loaded")
		return
	}
	saved, err := datasetManager.saveDataset(dataset)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	state.Mu.Lock()
	state.DatasetID = saved.ID
	state.Mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "dataset": saved})
}

func handleDatasetLoad(w http.ResponseWriter, r *http.Request) {
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
	dataset, ok := datasetManager.get(strings.TrimSpace(request.ID))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "saved dataset was not found")
		return
	}
	state.Mu.Lock()
	state.LastSummary = append([]PortProtocolSummary(nil), dataset.Summary...)
	state.LastInsights = dataset.Insights
	state.FileName = dataset.FileName
	state.DatasetID = dataset.ID
	state.DatasetCoverage = dataset.Coverage
	state.ReportMetadata = dataset.Report
	state.IsDone = true
	state.IsCancelled = false
	state.Mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "dataset": dataset})
}

func handleDatasetDelete(w http.ResponseWriter, r *http.Request) {
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
	id := strings.TrimSpace(request.ID)
	if err := datasetManager.delete(id); err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	state.Mu.Lock()
	if state.DatasetID == id {
		state.DatasetID = ""
	}
	state.Mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}

func handleReportMetadataSave(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var metadata ReportMetadata
	if err := decodeJSONBody(w, r, &metadata); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	validated, err := validateReportMetadata(metadata)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	state.Mu.Lock()
	datasetID := state.DatasetID
	state.Mu.Unlock()
	if datasetID != "" {
		if dataset, ok := datasetManager.get(datasetID); ok {
			dataset.Report = validated
			if _, err := datasetManager.saveDataset(dataset); err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	state.Mu.Lock()
	state.ReportMetadata = validated
	state.Mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "report_metadata": validated})
}
