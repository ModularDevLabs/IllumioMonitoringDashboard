package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleImportCSVCancellationHasConsistentStatus(t *testing.T) {
	preserveCSVImportState(t)
	request := newCSVImportRequest(t, []struct{ name, data string }{{
		name: "cancelled.csv",
		data: "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App\n10.0.0.1,10.0.0.2,443,TCP,1,Prod,Prod,API,ERP\n",
	}})
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	recorder := httptest.NewRecorder()
	handleImportCSV(recorder, request.WithContext(ctx))
	if recorder.Code != http.StatusRequestTimeout || !strings.Contains(recorder.Body.String(), "CSV import interrupted before results were saved") {
		t.Fatalf("cancelled import = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandleImportCSVTemporaryStorageFailureIsActionable(t *testing.T) {
	preserveCSVImportState(t)
	request := newDiskBackedCSVImportRequest(t, []csvMultipartFile{{
		name: "large.csv", blankBytes: 9 << 20,
		data: "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App\n",
	}})
	blockedPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedPath, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, blockedPath)
	}
	recorder := httptest.NewRecorder()
	handleImportCSV(recorder, request)
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "Check available disk space and write permissions") {
		t.Fatalf("temporary storage failure = %d %s", recorder.Code, recorder.Body.String())
	}
}

const largeCSVBlankBytes int64 = 91_000_000

type csvImportStateSnapshot struct {
	lastSummary     []PortProtocolSummary
	lastInsights    AnalyticsInsights
	fileName        string
	datasetID       string
	datasetCoverage DatasetCoverage
	reportMetadata  ReportMetadata
	trafficScope    string
	isDone          bool
	isCancelled     bool
	isPartial       bool
	failedChunks    int
	runError        string
}

func preserveCSVImportState(t *testing.T) {
	t.Helper()
	state.Mu.Lock()
	snapshot := csvImportStateSnapshot{
		lastSummary:     state.LastSummary,
		lastInsights:    state.LastInsights,
		fileName:        state.FileName,
		datasetID:       state.DatasetID,
		datasetCoverage: state.DatasetCoverage,
		reportMetadata:  state.ReportMetadata,
		trafficScope:    state.TrafficScope,
		isDone:          state.IsDone,
		isCancelled:     state.IsCancelled,
		isPartial:       state.IsPartial,
		failedChunks:    state.FailedChunks,
		runError:        state.RunError,
	}
	state.Mu.Unlock()
	t.Cleanup(func() {
		state.Mu.Lock()
		state.LastSummary = snapshot.lastSummary
		state.LastInsights = snapshot.lastInsights
		state.FileName = snapshot.fileName
		state.DatasetID = snapshot.datasetID
		state.DatasetCoverage = snapshot.datasetCoverage
		state.ReportMetadata = snapshot.reportMetadata
		state.TrafficScope = snapshot.trafficScope
		state.IsDone = snapshot.isDone
		state.IsCancelled = snapshot.isCancelled
		state.IsPartial = snapshot.isPartial
		state.FailedChunks = snapshot.failedChunks
		state.RunError = snapshot.runError
		state.Mu.Unlock()
	})
}

func isolateMultipartTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	// Go consults TMPDIR on Unix and TMP/TEMP on Windows when multipart
	// file parts spill to disk. Set all three so cleanup checks are portable.
	t.Setenv("TMPDIR", directory)
	t.Setenv("TMP", directory)
	t.Setenv("TEMP", directory)
	return directory
}

func assertDirectoryEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read multipart temporary directory: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("multipart temporary files were not removed: %v", names)
	}
}

func writeRepeatedByte(writer io.Writer, value byte, count int64) error {
	chunk := bytes.Repeat([]byte{value}, 1<<20)
	for count > 0 {
		writeSize := int64(len(chunk))
		if writeSize > count {
			writeSize = count
		}
		if _, err := writer.Write(chunk[:int(writeSize)]); err != nil {
			return err
		}
		count -= writeSize
	}
	return nil
}

type csvMultipartFile struct {
	name       string
	blankBytes int64
	data       string
}

func newDiskBackedCSVImportRequest(t *testing.T, files []csvMultipartFile) *http.Request {
	t.Helper()
	bodyPath := filepath.Join(t.TempDir(), "import.multipart")
	body, err := os.OpenFile(bodyPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("create multipart request body: %v", err)
	}
	writer := multipart.NewWriter(body)
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			_ = body.Close()
			t.Fatalf("create multipart file %s: %v", file.name, err)
		}
		if err := writeRepeatedByte(part, '\n', file.blankBytes); err != nil {
			_ = body.Close()
			t.Fatalf("write multipart padding for %s: %v", file.name, err)
		}
		if _, err := io.WriteString(part, file.data); err != nil {
			_ = body.Close()
			t.Fatalf("write multipart CSV %s: %v", file.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		_ = body.Close()
		t.Fatalf("finish multipart request body: %v", err)
	}
	stat, err := body.Stat()
	if err != nil {
		_ = body.Close()
		t.Fatalf("stat multipart request body: %v", err)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		_ = body.Close()
		t.Fatalf("rewind multipart request body: %v", err)
	}
	t.Cleanup(func() { _ = body.Close() })

	request := httptest.NewRequest(http.MethodPost, "http://localhost:8000/api/results/import-csv", body)
	request.ContentLength = stat.Size()
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://localhost:8000")
	return request
}

func TestHandleImportCSVAcceptsDiskBackedMultipartOver64MiB(t *testing.T) {
	preserveCSVImportState(t)

	header := "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App,First Detected,Last Detected\n"
	january := "10.0.0.1,10.0.0.2,443,TCP,3,Prod,Prod,API,ERP,2026-01-01 00:00:00,2026-01-31 23:00:00\n"
	february := "10.0.0.1,10.0.0.2,443,TCP,5,Prod,Prod,API,ERP,2026-02-01 00:00:00,2026-02-28 23:00:00\n"
	march := "10.0.0.1,10.0.0.2,443,TCP,7,Prod,Prod,API,ERP,2026-03-01 00:00:00,2026-03-31 23:00:00\n"
	request := newDiskBackedCSVImportRequest(t, []csvMultipartFile{
		{name: "january-large.csv", blankBytes: largeCSVBlankBytes, data: header + january},
		{name: "february.csv", data: header + february},
		// The repeated February row verifies cross-file exact-row deduplication
		// while keeping all three uploaded files byte-distinct.
		{name: "february-march.csv", data: header + february + march},
	})
	spillDirectory := isolateMultipartTempDir(t)

	recorder := httptest.NewRecorder()
	handleImportCSV(recorder, request)

	var response struct {
		Success   bool            `json:"success"`
		Error     string          `json:"error"`
		FileCount int             `json:"fileCount"`
		Coverage  DatasetCoverage `json:"coverage"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode large import response (status %d): %v; body=%q", recorder.Code, err, recorder.Body.String())
	}
	if !response.Success {
		t.Fatalf("large multi-file import failed (status %d): %s", recorder.Code, response.Error)
	}
	if response.FileCount != 3 || len(response.Coverage.Files) != 3 {
		t.Fatalf("large import coverage files=%d fileCount=%d, want 3", len(response.Coverage.Files), response.FileCount)
	}
	if response.Coverage.Files[0].Size < largeCSVBlankBytes {
		t.Fatalf("large CSV size=%d, want at least %d", response.Coverage.Files[0].Size, largeCSVBlankBytes)
	}
	if response.Coverage.DeduplicatedRecords != 1 || response.Coverage.DeduplicatedFlows != 5 {
		t.Fatalf("dedup coverage records=%d flows=%d, want 1 and 5", response.Coverage.DeduplicatedRecords, response.Coverage.DeduplicatedFlows)
	}
	if strings.Join(response.Coverage.Months, ",") != "2026-01,2026-02,2026-03" {
		t.Fatalf("coverage months=%v, want January through March", response.Coverage.Months)
	}

	state.Mu.Lock()
	summary := append([]PortProtocolSummary(nil), state.LastSummary...)
	monthly := append([]MonthlyPortProtocolSummary(nil), state.LastInsights.MonthlyPortProtocol...)
	state.Mu.Unlock()
	if len(summary) != 1 || summary[0].FlowCount != 15 || summary[0].UniqueConnections != 1 {
		t.Fatalf("summary=%#v, want 15 flows for one unique connection", summary)
	}
	monthlyFlows := make(map[string]int, len(monthly))
	for _, item := range monthly {
		monthlyFlows[item.Month] += item.FlowCount
	}
	if monthlyFlows["2026-01"] != 3 || monthlyFlows["2026-02"] != 5 || monthlyFlows["2026-03"] != 7 {
		t.Fatalf("monthly flows=%v, want 3, 5, and 7", monthlyFlows)
	}
	assertDirectoryEmpty(t, spillDirectory)
}

func TestHandleImportCSVMalformedRowPreservesPreviousAnalytics(t *testing.T) {
	preserveCSVImportState(t)
	sentinel := []PortProtocolSummary{{Port: 8443, Protocol: "TCP", FlowCount: 99, UniqueConnections: 1}}
	state.Mu.Lock()
	state.LastSummary = sentinel
	state.FileName = "previous.csv"
	state.IsDone = true
	state.Mu.Unlock()

	header := "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App\n"
	request := newDiskBackedCSVImportRequest(t, []csvMultipartFile{{
		name: "broken.csv",
		data: header + "10.0.0.1,10.0.0.2,443,TCP,not-a-number,Prod,Prod,API,ERP\n",
	}})
	recorder := httptest.NewRecorder()
	handleImportCSV(recorder, request)

	var response struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode malformed-row response: %v; body=%q", err, recorder.Body.String())
	}
	if response.Success || !strings.Contains(response.Error, "broken.csv row 2") || !strings.Contains(response.Error, "invalid Flows") {
		t.Fatalf("malformed-row response=%#v, want a file and row-specific Flows error", response)
	}
	state.Mu.Lock()
	defer state.Mu.Unlock()
	if state.FileName != "previous.csv" || len(state.LastSummary) != 1 || state.LastSummary[0] != sentinel[0] {
		t.Fatalf("failed import changed previous analytics: file=%q summary=%#v", state.FileName, state.LastSummary)
	}
}

type terminalErrorReader struct {
	err error
}

func (reader terminalErrorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

type repeatedByteReader struct {
	value     byte
	remaining int64
}

func (reader *repeatedByteReader) Read(buffer []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	readSize := int64(len(buffer))
	if readSize > reader.remaining {
		readSize = reader.remaining
	}
	for index := 0; index < int(readSize); index++ {
		buffer[index] = reader.value
	}
	reader.remaining -= readSize
	return int(readSize), nil
}

func TestHandleImportCSVInterruptedUploadReturnsJSONAndCleansTempFiles(t *testing.T) {
	preserveCSVImportState(t)
	spillDirectory := isolateMultipartTempDir(t)

	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	if _, err := writer.CreateFormFile("files", "interrupted.csv"); err != nil {
		t.Fatal(err)
	}
	readFailure := errors.New("simulated interrupted upload")
	body := io.MultiReader(
		bytes.NewReader(prefix.Bytes()),
		&repeatedByteReader{value: '\n', remaining: 9 << 20},
		terminalErrorReader{err: readFailure},
	)
	request := httptest.NewRequest(http.MethodPost, "http://localhost:8000/api/results/import-csv", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://localhost:8000")
	recorder := httptest.NewRecorder()

	handleImportCSV(recorder, request)

	var response struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode interrupted-upload response (status %d): %v; body=%q", recorder.Code, err, recorder.Body.String())
	}
	if response.Success || !strings.Contains(response.Error, readFailure.Error()) {
		t.Fatalf("interrupted-upload response=%#v, want clear read failure", response)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("interrupted upload status=%d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertDirectoryEmpty(t, spillDirectory)
}

func TestHandleImportCSVRejectsConcurrentImportWithoutChangingAnalytics(t *testing.T) {
	preserveCSVImportState(t)
	state.Mu.Lock()
	state.FileName = "existing.csv"
	state.LastSummary = []PortProtocolSummary{{Port: 443, Protocol: "TCP", FlowCount: 11, UniqueConnections: 1}}
	state.Mu.Unlock()

	select {
	case csvImportSlot <- struct{}{}:
		defer func() { <-csvImportSlot }()
	default:
		t.Fatal("CSV import slot was unexpectedly occupied before the test")
	}
	request := newDiskBackedCSVImportRequest(t, []csvMultipartFile{{
		name: "queued.csv",
		data: "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App\n" +
			"10.0.0.1,10.0.0.2,443,TCP,1,Prod,Prod,API,ERP\n",
	}})
	recorder := httptest.NewRecorder()

	handleImportCSV(recorder, request)

	var response struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode concurrent-import response: %v; body=%q", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusConflict || response.Success || !strings.Contains(response.Error, "Another CSV import is in progress") {
		t.Fatalf("concurrent-import status=%d response=%#v, want a clear conflict", recorder.Code, response)
	}
	state.Mu.Lock()
	defer state.Mu.Unlock()
	if state.FileName != "existing.csv" || len(state.LastSummary) != 1 || state.LastSummary[0].FlowCount != 11 {
		t.Fatalf("rejected concurrent import changed analytics: file=%q summary=%#v", state.FileName, state.LastSummary)
	}
}
