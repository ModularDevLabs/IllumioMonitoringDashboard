package extractor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"illumio-dash/internal/extractor/illumio"
)

//go:embed frontend/*.html frontend/tailwind.css frontend/app-shell.css frontend/product-shell.css frontend/theme-init.js frontend/collapsible.js frontend/app-version.js frontend/product-shell.js
var staticFiles embed.FS

// appVersion is replaced by scripts/build_release.sh using -ldflags. Source
// builds keep an explicit development label so the UI never implies a release.
var appVersion = "development"

type PCEProfile struct {
	Name              string `json:"name"`
	PCEURL            string `json:"pce_url"`
	OrgID             string `json:"org_id"`
	APIKey            string `json:"api_key"`
	APISecret         string `json:"api_secret"`
	SrcLabels         string `json:"src_labels"`
	DstLabels         string `json:"dst_labels"`
	ExcludeSrc        string `json:"exclude_src"`
	ExcludeDst        string `json:"exclude_dst"`
	Services          string `json:"services"`
	ExcludeServices   string `json:"exclude_services"`
	SavePath          string `json:"save_path"`
	FileName          string `json:"file_name"`
	Days              int    `json:"days"`
	StartDate         string `json:"start_date"`
	EndDate           string `json:"end_date"`
	ChunkIntvl        string `json:"chunk_interval"`
	AnalysisPrimary   string `json:"analysis_primary_label"`
	AnalysisSecondary string `json:"analysis_secondary_label"`
	TrafficScope      string `json:"traffic_scope"`
}

type PublicPCEProfile struct {
	Name              string `json:"name"`
	PCEURL            string `json:"pce_url"`
	OrgID             string `json:"org_id"`
	SrcLabels         string `json:"src_labels"`
	DstLabels         string `json:"dst_labels"`
	ExcludeSrc        string `json:"exclude_src"`
	ExcludeDst        string `json:"exclude_dst"`
	Services          string `json:"services"`
	ExcludeServices   string `json:"exclude_services"`
	SavePath          string `json:"save_path"`
	FileName          string `json:"file_name"`
	Days              int    `json:"days"`
	StartDate         string `json:"start_date"`
	EndDate           string `json:"end_date"`
	ChunkIntvl        string `json:"chunk_interval"`
	AnalysisPrimary   string `json:"analysis_primary_label"`
	AnalysisSecondary string `json:"analysis_secondary_label"`
	TrafficScope      string `json:"traffic_scope"`
}

func (profile PCEProfile) public() PublicPCEProfile {
	return PublicPCEProfile{
		Name:              profile.Name,
		PCEURL:            profile.PCEURL,
		OrgID:             profile.OrgID,
		SrcLabels:         profile.SrcLabels,
		DstLabels:         profile.DstLabels,
		ExcludeSrc:        profile.ExcludeSrc,
		ExcludeDst:        profile.ExcludeDst,
		Services:          profile.Services,
		ExcludeServices:   profile.ExcludeServices,
		SavePath:          profile.SavePath,
		FileName:          profile.FileName,
		Days:              profile.Days,
		StartDate:         profile.StartDate,
		EndDate:           profile.EndDate,
		ChunkIntvl:        profile.ChunkIntvl,
		AnalysisPrimary:   profile.AnalysisPrimary,
		AnalysisSecondary: profile.AnalysisSecondary,
		TrafficScope:      normalizedTrafficScope(profile.TrafficScope),
	}
}

type AppState struct {
	Mu               sync.Mutex
	CompletedChunks  int
	RequestedDays    int
	RequestedChunks  int
	ChunkInterval    string
	ActiveChunks     int
	RunStartedAt     time.Time
	LastProgressAt   time.Time
	DiscoveryDone    int
	DiscoveryTotal   int
	DiscoveryActive  bool
	TotalConnections int
	Logs             []string
	IsDone           bool
	IsCancelled      bool
	FileName         string
	Profiles         map[string]PCEProfile
	CancelFunc       context.CancelFunc
	LastSummary      []PortProtocolSummary
	LastInsights     AnalyticsInsights
	DatasetID        string
	DatasetCoverage  DatasetCoverage
	ReportMetadata   ReportMetadata
	TrafficScope     string
	DiscoveryCache   *DiscoveryData
	DiscoveryKey     string
	RunError         string
}

type PortProtocolSummary struct {
	Port              int    `json:"port"`
	Protocol          string `json:"protocol"`
	ProtocolNumber    int    `json:"protocol_number"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
}

type MatrixSummary struct {
	Source            string `json:"source"`
	Destination       string `json:"destination"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
}

type TalkerSummary struct {
	Name              string `json:"name"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
}

type TrafficCategorySummary struct {
	Name              string `json:"name"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
}

type EnvServicePivotSummary struct {
	SourceEnv         string `json:"source_env"`
	DestinationEnv    string `json:"destination_env"`
	Protocol          string `json:"protocol"`
	Port              int    `json:"port"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
}

type AppServicePivotSummary struct {
	SourceApp         string `json:"source_app"`
	DestinationApp    string `json:"destination_app"`
	Protocol          string `json:"protocol"`
	Port              int    `json:"port"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
}

type CombinedServicePivotSummary struct {
	SourceCombined      string `json:"source_combined"`
	DestinationCombined string `json:"destination_combined"`
	Protocol            string `json:"protocol"`
	Port                int    `json:"port"`
	FlowCount           int    `json:"flow_count"`
	UniqueConnections   int    `json:"unique_connections"`
}

type MonthlyPortProtocolSummary struct {
	Month             string `json:"month"`
	Protocol          string `json:"protocol"`
	Port              int    `json:"port"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
	ActiveConnections int    `json:"active_connections"`
}

type MonthlyRelationshipSummary struct {
	Month             string `json:"month"`
	Source            string `json:"source"`
	Destination       string `json:"destination"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
}

type MonthlyDestinationSummary struct {
	Month             string `json:"month"`
	Destination       string `json:"destination"`
	FlowCount         int    `json:"flow_count"`
	UniqueConnections int    `json:"unique_connections"`
}

type AnalyticsInsights struct {
	PrimaryLabelKey             string                        `json:"primary_label_key"`
	SecondaryLabelKey           string                        `json:"secondary_label_key"`
	EnvMatrix                   []MatrixSummary               `json:"env_matrix"`
	AppMatrix                   []MatrixSummary               `json:"app_matrix"`
	TopSourceEnvs               []TalkerSummary               `json:"top_source_envs"`
	TopDestinationEnvs          []TalkerSummary               `json:"top_destination_envs"`
	TopSourceIPs                []TalkerSummary               `json:"top_source_ips"`
	TopDestinationIPs           []TalkerSummary               `json:"top_destination_ips"`
	TopExternalDestinationIPs   []TalkerSummary               `json:"top_external_destination_ips"`
	TopAppPairs                 []TalkerSummary               `json:"top_app_pairs"`
	TrafficCategories           []TrafficCategorySummary      `json:"traffic_categories"`
	EnvServicePivot             []EnvServicePivotSummary      `json:"env_service_pivot"`
	SourceEnvOptions            []string                      `json:"source_env_options"`
	AppServicePivot             []AppServicePivotSummary      `json:"app_service_pivot"`
	SourceAppOptions            []string                      `json:"source_app_options"`
	CombinedServicePivot        []CombinedServicePivotSummary `json:"combined_service_pivot"`
	SourceCombinedOptions       []string                      `json:"source_combined_options"`
	MonthlyPortProtocol         []MonthlyPortProtocolSummary  `json:"monthly_port_protocol"`
	MonthlyRelationships        []MonthlyRelationshipSummary  `json:"monthly_relationships"`
	MonthlyExternalDestinations []MonthlyDestinationSummary   `json:"monthly_external_destinations"`
}

type AnalyticsRecord struct {
	Identity       string
	SrcEnv         string
	DstEnv         string
	SrcApp         string
	DstApp         string
	SrcIP          string
	DstIP          string
	DstFQDN        string
	SrcManaged     bool
	DstManaged     bool
	Protocol       string
	ProtocolNumber int
	Port           int
	Month          string
	FlowCount      int
	FirstSeen      time.Time
	LastSeen       time.Time
	PolicyDecision string
	DraftDecision  string
	TrafficScope   string
}

type DiscoveryData struct {
	Labels          []illumio.Label
	Services        []illumio.Service
	IPLists         []illumio.IPList
	LabelGroups     []illumio.LabelGroup
	UserGroups      []illumio.UserGroup
	VirtualServices []illumio.VirtualService
	VirtualServers  []illumio.VirtualServer
}

var state = &AppState{
	Logs:     []string{},
	Profiles: make(map[string]PCEProfile),
}

const (
	appConfigDirName    = "illumio-monitoring-dashboard-extractor"
	maxJSONRequestSize  = 1 << 20
	maxCSVUploadSize    = 64 << 20
	maxCSVUploadFiles   = 60
	maxExtractionTime   = 24 * time.Hour
	maxChunkQueryTime   = 30 * time.Minute
	maxChunkAttempts    = 3
	maxExtractionChunks = 200000
)

func addLog(msg string) {
	log.Println(msg)
	state.Mu.Lock()
	defer state.Mu.Unlock()
	state.Logs = append(state.Logs, msg)
	lower := strings.ToLower(strings.TrimSpace(msg))
	if strings.HasPrefix(lower, "error") || strings.Contains(lower, "aborted without writing") || strings.Contains(lower, "time limit") {
		state.RunError = msg
	}
}

func markRunFinished(fileName string, cancelled bool) {
	state.Mu.Lock()
	cancel := state.CancelFunc
	state.ActiveChunks = 0
	state.LastProgressAt = time.Now().UTC()
	state.IsDone = true
	state.IsCancelled = cancelled
	state.FileName = fileName
	state.CancelFunc = nil
	state.Mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func discoveryCacheKey(cfg Config) string {
	return strings.Join([]string{
		strings.TrimSpace(cfg.PCEURL),
		strings.TrimSpace(cfg.OrgID),
		strings.TrimSpace(cfg.APIKey),
		strings.TrimSpace(cfg.APISecret),
	}, "|")
}

func fetchDiscoveryData(ctx context.Context, client *illumio.Client, logPrefix string) (DiscoveryData, error) {
	type discoveryTask struct {
		name  string
		fetch func(context.Context) error
	}

	var (
		results  DiscoveryData
		resultMu sync.Mutex
		firstErr error
		errOnce  sync.Once
		cancelFn context.CancelFunc
	)
	ctx, cancelFn = context.WithCancel(ctx)
	defer cancelFn()

	tasks := []discoveryTask{
		{name: "labels", fetch: func(ctx context.Context) error {
			items, err := client.GetLabels(ctx)
			if err == nil {
				resultMu.Lock()
				results.Labels = items
				resultMu.Unlock()
			}
			return err
		}},
		{name: "services", fetch: func(ctx context.Context) error {
			items, err := client.GetServices(ctx)
			if err == nil {
				resultMu.Lock()
				results.Services = items
				resultMu.Unlock()
			}
			return err
		}},
		{name: "IP lists", fetch: func(ctx context.Context) error {
			items, err := client.GetIPLists(ctx)
			if err == nil {
				resultMu.Lock()
				results.IPLists = items
				resultMu.Unlock()
			}
			return err
		}},
		{name: "label groups", fetch: func(ctx context.Context) error {
			items, err := client.GetLabelGroups(ctx)
			if err == nil {
				resultMu.Lock()
				results.LabelGroups = items
				resultMu.Unlock()
			}
			return err
		}},
		{name: "user groups", fetch: func(ctx context.Context) error {
			items, err := client.GetUserGroups(ctx)
			if err == nil {
				resultMu.Lock()
				results.UserGroups = items
				resultMu.Unlock()
			}
			return err
		}},
		{name: "virtual services", fetch: func(ctx context.Context) error {
			items, err := client.GetVirtualServices(ctx)
			if err == nil {
				resultMu.Lock()
				results.VirtualServices = items
				resultMu.Unlock()
			}
			return err
		}},
		{name: "virtual servers", fetch: func(ctx context.Context) error {
			items, err := client.GetVirtualServers(ctx)
			if err == nil {
				resultMu.Lock()
				results.VirtualServers = items
				resultMu.Unlock()
			}
			return err
		}},
	}

	jobs := make(chan discoveryTask)
	var wg sync.WaitGroup
	workerCount := 3

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				if ctx.Err() != nil {
					return
				}

				if logPrefix != "" {
					addLog(fmt.Sprintf("%s loading %s...", logPrefix, task.name))
				}
				if err := task.fetch(ctx); err != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("%s: %w", task.name, err)
						cancelFn()
					})
					return
				}
				if logPrefix != "" {
					count := 0
					resultMu.Lock()
					switch task.name {
					case "labels":
						count = len(results.Labels)
					case "services":
						count = len(results.Services)
					case "IP lists":
						count = len(results.IPLists)
					case "label groups":
						count = len(results.LabelGroups)
					case "user groups":
						count = len(results.UserGroups)
					case "virtual services":
						count = len(results.VirtualServices)
					case "virtual servers":
						count = len(results.VirtualServers)
					}
					resultMu.Unlock()
					addLog(fmt.Sprintf("%s loaded %d %s.", logPrefix, count, task.name))
				}
				state.Mu.Lock()
				state.DiscoveryDone++
				state.Mu.Unlock()
			}
		}()
	}

queueTasks:
	for _, task := range tasks {
		select {
		case jobs <- task:
		case <-ctx.Done():
			break queueTasks
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return DiscoveryData{}, firstErr
	}

	return results, nil
}

func getConfigPath() (string, error) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(configRoot, appConfigDirName, "pce_profiles.json"), nil
}

func loadProfiles() {
	path, err := getConfigPath()
	if err != nil {
		log.Printf("failed to locate profile store: %v", err)
		return
	}
	data, err := os.ReadFile(path)
	legacyPath := ""
	if errors.Is(err, os.ErrNotExist) {
		if executable, executableErr := os.Executable(); executableErr == nil {
			candidatePath := filepath.Join(filepath.Dir(executable), "pce_profiles.json")
			if legacyData, legacyErr := os.ReadFile(candidatePath); legacyErr == nil {
				data = legacyData
				err = nil
				legacyPath = candidatePath
				log.Printf("migrating profiles from legacy path %s to %s", candidatePath, path)
			}
		}
		if errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	if err != nil {
		log.Printf("failed to read profile store: %v", err)
		return
	}

	profiles := make(map[string]PCEProfile)
	if err := json.Unmarshal(data, &profiles); err != nil {
		log.Printf("failed to parse profile store: %v", err)
		return
	}
	state.Mu.Lock()
	state.Profiles = profiles
	state.Mu.Unlock()
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		if err := saveProfiles(); err != nil {
			log.Printf("failed to migrate profile store: %v", err)
		} else if legacyPath != "" {
			if err := os.Remove(legacyPath); err != nil {
				log.Printf("profiles migrated, but the legacy profile file could not be removed: %v", err)
			} else {
				log.Printf("profiles migrated and legacy profile file removed")
			}
		}
	}
}

func saveProfiles() error {
	path, err := getConfigPath()
	if err != nil {
		return err
	}
	state.Mu.Lock()
	data, err := json.MarshalIndent(state.Profiles, "", "  ")
	state.Mu.Unlock()
	if err != nil {
		return fmt.Errorf("encode profile store: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("secure profile directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".pce_profiles-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary profile store: %w", err)
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
		return fmt.Errorf("secure temporary profile store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary profile store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary profile store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary profile store: %w", err)
	}
	if err := replaceFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace profile store: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("secure profile store: %w", err)
	}
	committed = true
	return nil
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, target interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONRequestSize)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request body must contain one JSON object")
		}
		return err
	}
	return nil
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   message,
	})
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeJSONError(w, http.StatusMethodNotAllowed, fmt.Sprintf("method %s not allowed", r.Method))
	return false
}

func sameOriginRequest(r *http.Request) bool {
	candidates := []string{r.Header.Get("Origin"), r.Header.Get("Referer")}
	found := false
	for _, raw := range candidates {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		found = true
		parsed, err := url.Parse(raw)
		if err != nil {
			return false
		}
		if !strings.EqualFold(parsed.Host, r.Host) {
			return false
		}
	}
	return found
}

func requireSameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if sameOriginRequest(r) {
		return true
	}
	writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
	return false
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func boolEnvOrDefault(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	switch strings.ToLower(value) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func loopbackHost(hostport string) bool {
	host := hostport
	if parsedHost, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackRemote(remoteAddress string) bool {
	host := strings.TrimSpace(remoteAddress)
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) || !loopbackRemote(r.RemoteAddr) {
			http.Error(w, "the Blocked Traffic Extractor is available only through localhost", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func serveEmbeddedHTML(w http.ResponseWriter, fileName string) {
	data, err := staticFiles.ReadFile(fileName)
	if err != nil {
		http.Error(w, "page unavailable", http.StatusInternalServerError)
		return
	}
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		http.Error(w, "failed to prepare page security policy", http.StatusInternalServerError)
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	data = []byte(strings.Replace(string(data), "<script>", `<script nonce="`+nonce+`">`, 1))
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'nonce-"+nonce+"'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func serveEmbeddedAsset(w http.ResponseWriter, r *http.Request, fileName, contentType string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := staticFiles.ReadFile(fileName)
	if err != nil {
		http.Error(w, "asset unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// ServeProductShellCSS exposes the shared product navigation styles on the
// dashboard's neutral /static route without relaxing Extractor route security.
func ServeProductShellCSS(w http.ResponseWriter, r *http.Request) {
	serveEmbeddedAsset(w, r, "frontend/product-shell.css", "text/css; charset=utf-8")
}

// ServeProductShellJS exposes the shared product navigation behavior on the
// dashboard's neutral /static route without relaxing Extractor route security.
func ServeProductShellJS(w http.ResponseWriter, r *http.Request) {
	serveEmbeddedAsset(w, r, "frontend/product-shell.js", "text/javascript; charset=utf-8")
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	version := strings.TrimSpace(appVersion)
	if version == "" {
		version = "development"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"version": version})
}

// NewHandler initializes the Blocked Traffic Extractor as a module within the
// monitoring dashboard. Its profiles, API credentials, datasets, templates,
// delivery destinations, and run history remain in the extractor's dedicated
// user configuration directory and are never populated from dashboard config.
func NewHandler(appCtx context.Context, version string) (http.Handler, error) {
	if appCtx == nil {
		appCtx = context.Background()
	}
	if strings.TrimSpace(version) != "" {
		appVersion = strings.TrimSpace(version)
	}
	loadProfiles()
	if err := automation.load(); err != nil {
		return nil, fmt.Errorf("load extractor automation store: %w", err)
	}
	if err := datasetManager.load(); err != nil {
		return nil, fmt.Errorf("load extractor datasets: %w", err)
	}
	automation.start(appCtx, true)

	mux := http.NewServeMux()
	mux.HandleFunc("/assets/tailwind.css", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedAsset(w, r, "frontend/tailwind.css", "text/css; charset=utf-8")
	})
	mux.HandleFunc("/assets/app-shell.css", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedAsset(w, r, "frontend/app-shell.css", "text/css; charset=utf-8")
	})
	mux.HandleFunc("/assets/product-shell.css", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedAsset(w, r, "frontend/product-shell.css", "text/css; charset=utf-8")
	})
	mux.HandleFunc("/assets/product-shell.js", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedAsset(w, r, "frontend/product-shell.js", "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("/assets/theme-init.js", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedAsset(w, r, "frontend/theme-init.js", "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("/assets/collapsible.js", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedAsset(w, r, "frontend/collapsible.js", "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("/assets/app-version.js", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedAsset(w, r, "frontend/app-version.js", "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serveEmbeddedHTML(w, "frontend/index.html")
	})
	mux.HandleFunc("/summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serveEmbeddedHTML(w, "frontend/summary.html")
	})
	mux.HandleFunc("/executive-summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serveEmbeddedHTML(w, "frontend/executive-summary.html")
	})
	mux.HandleFunc("/heatmaps", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serveEmbeddedHTML(w, "frontend/heatmaps.html")
	})
	mux.HandleFunc("/automation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serveEmbeddedHTML(w, "frontend/automation.html")
	})

	mux.HandleFunc("/api/test", handleTest)
	mux.HandleFunc("/api/version", handleVersion)
	mux.HandleFunc("/api/traffic-db-metrics", handleTrafficDBMetrics)
	mux.HandleFunc("/api/discovery", handleDiscovery)
	mux.HandleFunc("/api/start", handleStart)
	mux.HandleFunc("/api/cancel", handleCancel)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/results/summary", handleSummary)
	mux.HandleFunc("/api/results/import-csv", handleImportCSV)
	registerAutomationHandlers(mux)
	registerDatasetHandlers(mux)

	mux.HandleFunc("/api/profiles/get", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		state.Mu.Lock()
		profiles := make(map[string]PublicPCEProfile, len(state.Profiles))
		for name, profile := range state.Profiles {
			profiles[name] = profile.public()
		}
		state.Mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(profiles)
	})
	mux.HandleFunc("/api/profiles/save", handleSaveProfile)
	mux.HandleFunc("/api/profiles/delete", handleDeleteProfile)

	return securityHeaders(mux), nil
}

func handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var cfg Config
	if err := decodeJSONBody(w, r, &cfg); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	var err error
	cfg, err = resolveConfigCredentials(cfg)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	const discoveryTimeout = 15 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()

	state.Mu.Lock()
	state.DiscoveryDone = 0
	state.DiscoveryTotal = 7
	state.DiscoveryActive = true
	state.Mu.Unlock()
	defer func() {
		state.Mu.Lock()
		state.DiscoveryActive = false
		state.Mu.Unlock()
	}()

	client := illumio.NewClient(cfg.PCEURL, cfg.OrgID, cfg.APIKey, cfg.APISecret)
	addLog("Discovery: starting parallel collection load (up to 3 collection jobs at a time)...")
	discoveryData, err := fetchDiscoveryData(ctx, client, "Discovery:")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("discovery timed out after %s while loading large policy collections; no new discovery cache was saved", discoveryTimeout)
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	labelNames := make([]string, 0, len(discoveryData.Labels))
	for _, item := range discoveryData.Labels {
		labelNames = append(labelNames, item.Value)
	}
	labelTypes := labelTypesFromLabels(discoveryData.Labels)
	serviceNames := make([]string, 0, len(discoveryData.Services))
	for _, item := range discoveryData.Services {
		serviceNames = append(serviceNames, item.Name)
	}
	ipListNames := make([]string, 0, len(discoveryData.IPLists))
	for _, item := range discoveryData.IPLists {
		ipListNames = append(ipListNames, item.Name)
	}
	lgNames := make([]string, 0, len(discoveryData.LabelGroups))
	for _, item := range discoveryData.LabelGroups {
		lgNames = append(lgNames, item.Name)
	}
	ugNames := make([]string, 0, len(discoveryData.UserGroups))
	for _, item := range discoveryData.UserGroups {
		ugNames = append(ugNames, item.Name)
	}
	vsNames := make([]string, 0, len(discoveryData.VirtualServices))
	for _, item := range discoveryData.VirtualServices {
		vsNames = append(vsNames, item.Name)
	}
	vsvrNames := make([]string, 0, len(discoveryData.VirtualServers))
	for _, item := range discoveryData.VirtualServers {
		vsvrNames = append(vsvrNames, item.Name)
	}

	state.Mu.Lock()
	state.DiscoveryCache = &discoveryData
	state.DiscoveryKey = discoveryCacheKey(cfg)
	state.Mu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"labels":          labelNames,
		"labelTypes":      labelTypes,
		"services":        serviceNames,
		"ipLists":         ipListNames,
		"labelGroups":     lgNames,
		"userGroups":      ugNames,
		"virtualServices": vsNames,
		"virtualServers":  vsvrNames,
	})
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var cancel context.CancelFunc
	state.Mu.Lock()
	if state.CancelFunc != nil {
		cancel = state.CancelFunc
		state.IsCancelled = true
	}
	state.Mu.Unlock()
	if cancel != nil {
		cancel()
		addLog("!!! CANCEL SIGNAL RECEIVED !!!")
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var prof PCEProfile
	if err := decodeJSONBody(w, r, &prof); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	prof.Name = strings.TrimSpace(prof.Name)
	if prof.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "profile name required")
		return
	}
	if len(prof.Name) > 100 {
		writeJSONError(w, http.StatusBadRequest, "profile name must be 100 characters or fewer")
		return
	}
	normalizedURL, err := validatePCEURL(prof.PCEURL)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	prof.PCEURL = normalizedURL
	prof.OrgID, err = validateOrgID(prof.OrgID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	prof.AnalysisPrimary, prof.AnalysisSecondary, err = normalizeAnalysisLabelKeys(prof.AnalysisPrimary, prof.AnalysisSecondary)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	prof.TrafficScope, err = normalizeTrafficScope(prof.TrafficScope)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	state.Mu.Lock()
	previous, existed := state.Profiles[prof.Name]
	state.Mu.Unlock()
	if existed {
		if strings.TrimSpace(prof.APIKey) == "" {
			prof.APIKey = previous.APIKey
		}
		if prof.APISecret == "" {
			prof.APISecret = previous.APISecret
		}
	}
	prof.APIKey = strings.TrimSpace(prof.APIKey)
	if strings.TrimSpace(prof.OrgID) == "" || strings.TrimSpace(prof.APIKey) == "" || prof.APISecret == "" {
		writeJSONError(w, http.StatusBadRequest, "PCE URL, Org ID, API Key, and API Secret are required")
		return
	}
	state.Mu.Lock()
	state.Profiles[prof.Name] = prof
	state.Mu.Unlock()
	if err := saveProfiles(); err != nil {
		state.Mu.Lock()
		if existed {
			state.Profiles[prof.Name] = previous
		} else {
			delete(state.Profiles, prof.Name)
		}
		state.Mu.Unlock()
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	automation.mu.Lock()
	for _, template := range automation.data.Templates {
		if template.ProfileName == req.Name {
			automation.mu.Unlock()
			writeJSONError(w, http.StatusConflict, fmt.Sprintf("profile is used by automation template %q", template.Name))
			return
		}
	}
	automation.mu.Unlock()
	state.Mu.Lock()
	previous, existed := state.Profiles[req.Name]
	delete(state.Profiles, req.Name)
	state.Mu.Unlock()
	if !existed {
		writeJSONError(w, http.StatusNotFound, "profile not found")
		return
	}
	if err := saveProfiles(); err != nil {
		state.Mu.Lock()
		state.Profiles[req.Name] = previous
		state.Mu.Unlock()
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

type Config struct {
	ProfileName       string `json:"profile_name"`
	PCEURL            string `json:"pce_url"`
	OrgID             string `json:"org_id"`
	APIKey            string `json:"api_key"`
	APISecret         string `json:"api_secret"`
	SrcLabels         string `json:"src_labels"`
	DstLabels         string `json:"dst_labels"`
	ExcludeSrc        string `json:"exclude_src"`
	ExcludeDst        string `json:"exclude_dst"`
	Services          string `json:"services"`
	ExcludeServices   string `json:"exclude_services"`
	SavePath          string `json:"save_path"`
	FileName          string `json:"file_name"`
	Days              int    `json:"days"`
	StartDate         string `json:"start_date"`
	EndDate           string `json:"end_date"`
	ChunkIntvl        string `json:"chunk_interval"`
	AnalysisPrimary   string `json:"analysis_primary_label"`
	AnalysisSecondary string `json:"analysis_secondary_label"`
	TrafficScope      string `json:"traffic_scope"`
}

const (
	trafficScopeBlocked = "blocked"
	trafficScopeAll     = "all"
)

func normalizedTrafficScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), trafficScopeAll) {
		return trafficScopeAll
	}
	return trafficScopeBlocked
}

func normalizeTrafficScope(scope string) (string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return trafficScopeBlocked, nil
	}
	if scope != trafficScopeBlocked && scope != trafficScopeAll {
		return "", fmt.Errorf("traffic scope must be blocked or all")
	}
	return scope, nil
}

func policyDecisionsForScope(scope string) []string {
	if normalizedTrafficScope(scope) == trafficScopeAll {
		return []string{}
	}
	return []string{"blocked"}
}

func reportTitleForScope(scope string) string {
	if normalizedTrafficScope(scope) == trafficScopeAll {
		return "All Traffic Executive Summary"
	}
	return "Blocked Traffic Executive Summary"
}

func normalizeAnalysisLabelKeys(primary, secondary string) (string, string, error) {
	primary = strings.TrimSpace(primary)
	secondary = strings.TrimSpace(secondary)
	if primary == "" {
		primary = "env"
	}
	if secondary == "" {
		secondary = "app"
	}
	if len(primary) > 100 || len(secondary) > 100 || strings.ContainsAny(primary+secondary, "\r\n") {
		return "", "", fmt.Errorf("analysis label types must be single-line values of 100 characters or fewer")
	}
	if strings.EqualFold(primary, secondary) {
		return "", "", fmt.Errorf("primary and secondary analysis label types must be different")
	}
	return primary, secondary, nil
}

func labelTypesFromLabels(labels []illumio.Label) []string {
	byLowerKey := make(map[string]string)
	for _, label := range labels {
		key := strings.TrimSpace(label.Key)
		if key == "" {
			continue
		}
		lowerKey := strings.ToLower(key)
		if _, exists := byLowerKey[lowerKey]; !exists {
			byLowerKey[lowerKey] = key
		}
	}

	keys := make([]string, 0, len(byLowerKey))
	for _, key := range byLowerKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	return keys
}

func resolveAnalysisLabelKeys(primary, secondary string, labels []illumio.Label) (string, string, error) {
	primary, secondary, err := normalizeAnalysisLabelKeys(primary, secondary)
	if err != nil {
		return "", "", err
	}

	available := make(map[string]string)
	for _, key := range labelTypesFromLabels(labels) {
		available[strings.ToLower(key)] = key
	}
	resolvedPrimary, primaryOK := available[strings.ToLower(primary)]
	resolvedSecondary, secondaryOK := available[strings.ToLower(secondary)]
	if !primaryOK || !secondaryOK {
		availableKeys := labelTypesFromLabels(labels)
		availableText := strings.Join(availableKeys, ", ")
		if availableText == "" {
			availableText = "none"
		}
		missing := primary
		if primaryOK {
			missing = secondary
		}
		return "", "", fmt.Errorf("analysis label type %q was not found; available label types: %s", missing, availableText)
	}
	return resolvedPrimary, resolvedSecondary, nil
}

func validatePCEURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid PCE URL: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("PCE URL must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("PCE URL must contain a host and must not contain user credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("PCE URL must be an origin only, without a path, query, or fragment")
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Host) {
		return "", fmt.Errorf("PCE URL must use HTTPS unless it is a loopback development endpoint")
	}
	parsed.Path = ""
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func validateOrgID(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	orgID, err := strconv.ParseUint(value, 10, 64)
	if err != nil || orgID == 0 {
		return "", fmt.Errorf("Org ID must be a positive integer")
	}
	return value, nil
}

func validatePort(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("port must be an integer from 1 through 65535")
	}
	return value, nil
}

func resolveConfigCredentials(cfg Config) (Config, error) {
	profileName := strings.TrimSpace(cfg.ProfileName)
	if profileName == "" {
		return Config{}, fmt.Errorf("save and select an Extractor PCE profile before connecting")
	}
	state.Mu.Lock()
	profile, ok := state.Profiles[profileName]
	state.Mu.Unlock()
	if !ok {
		return Config{}, fmt.Errorf("saved profile %q was not found", profileName)
	}
	// Request bodies may select a profile and report parameters, but they never
	// provide the network destination or credentials used by the HTTP client.
	cfg.PCEURL = profile.PCEURL
	cfg.OrgID = profile.OrgID
	cfg.APIKey = profile.APIKey
	cfg.APISecret = profile.APISecret
	if strings.TrimSpace(cfg.AnalysisPrimary) == "" {
		cfg.AnalysisPrimary = profile.AnalysisPrimary
	}
	if strings.TrimSpace(cfg.AnalysisSecondary) == "" {
		cfg.AnalysisSecondary = profile.AnalysisSecondary
	}
	if strings.TrimSpace(cfg.TrafficScope) == "" {
		cfg.TrafficScope = profile.TrafficScope
	}

	normalizedURL, err := validatePCEURL(cfg.PCEURL)
	if err != nil {
		return Config{}, err
	}
	cfg.PCEURL = normalizedURL
	cfg.OrgID, err = validateOrgID(cfg.OrgID)
	if err != nil {
		return Config{}, err
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.OrgID == "" || cfg.APIKey == "" || cfg.APISecret == "" {
		return Config{}, fmt.Errorf("PCE URL, Org ID, API Key, and API Secret are required")
	}
	cfg.AnalysisPrimary, cfg.AnalysisSecondary, err = normalizeAnalysisLabelKeys(cfg.AnalysisPrimary, cfg.AnalysisSecondary)
	if err != nil {
		return Config{}, err
	}
	cfg.TrafficScope, err = normalizeTrafficScope(cfg.TrafficScope)
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

type extractionChunk struct {
	Start time.Time
	End   time.Time
}

func handleTest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var cfg Config
	if err := decodeJSONBody(w, r, &cfg); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	var err error
	cfg, err = resolveConfigCredentials(cfg)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := illumio.NewClient(cfg.PCEURL, cfg.OrgID, cfg.APIKey, cfg.APISecret)
	err = client.TestConnection(ctx)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func handleTrafficDBMetrics(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var cfg Config
	if err := decodeJSONBody(w, r, &cfg); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	var err error
	cfg, err = resolveConfigCredentials(cfg)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := illumio.NewClient(cfg.PCEURL, cfg.OrgID, cfg.APIKey, cfg.APISecret)
	metrics, err := client.GetTrafficFlowsDatabaseMetrics(ctx)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"metrics": metrics,
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	state.Mu.Lock()
	response := map[string]interface{}{
		"completedChunks":  state.CompletedChunks,
		"requestedDays":    state.RequestedDays,
		"requestedChunks":  state.RequestedChunks,
		"chunkInterval":    state.ChunkInterval,
		"activeChunks":     state.ActiveChunks,
		"runStartedAt":     state.RunStartedAt,
		"lastProgressAt":   state.LastProgressAt,
		"totalConnections": state.TotalConnections,
		"newLogs":          state.Logs,
		"done":             state.IsDone,
		"cancelled":        state.IsCancelled,
		"fileName":         state.FileName,
		"discoveryDone":    state.DiscoveryDone,
		"discoveryTotal":   state.DiscoveryTotal,
		"discoveryActive":  state.DiscoveryActive,
		"trafficScope":     normalizedTrafficScope(state.TrafficScope),
	}
	state.Logs = []string{}
	state.Mu.Unlock()
	_ = json.NewEncoder(w).Encode(response)
}

func handleSummary(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	state.Mu.Lock()
	fileName := state.FileName
	summary := state.LastSummary
	insights := state.LastInsights
	datasetID := state.DatasetID
	coverage := state.DatasetCoverage
	reportMetadata := state.ReportMetadata
	trafficScope := normalizedTrafficScope(state.TrafficScope)
	state.Mu.Unlock()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         fileName != "" && len(summary) > 0,
		"fileName":        fileName,
		"datasetId":       datasetID,
		"summary":         summary,
		"insights":        insights,
		"coverage":        coverage,
		"report_metadata": reportMetadata,
		"traffic_scope":   trafficScope,
	})
}

type csvAnalyticsRow struct {
	SrcEnv            string
	DstEnv            string
	SrcApp            string
	DstApp            string
	SrcIP             string
	DstEndpoint       string
	Port              int
	Protocol          string
	ProtocolNumber    int
	FlowCount         int
	UniqueConnections int
	SrcManaged        bool
	DstManaged        bool
}

func normalizeCSVValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return trimmed
	}
	switch trimmed {
	case "No Env Label", "No App Label":
		return trimmed
	default:
		return trimmed
	}
}

func protocolNumberFromName(value string) int {
	protoMap := map[string]int{
		"ICMP":   1,
		"IGMP":   2,
		"TCP":    6,
		"UDP":    17,
		"GRE":    47,
		"ESP":    50,
		"AH":     51,
		"ICMPV6": 58,
		"OSPF":   89,
		"VRRP":   112,
		"SCTP":   132,
	}
	if number, ok := protoMap[strings.ToUpper(strings.TrimSpace(value))]; ok {
		return number
	}
	return 0
}

func monthBucketFromTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01")
}

func parseCSVMonthBucket(row []string, getValue func([]string, string) string) string {
	candidates := []string{
		getValue(row, "Last Detected"),
		getValue(row, "First Detected"),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if ts := parseCSVTimestamp(candidate); !ts.IsZero() {
			return monthBucketFromTime(ts)
		}
	}
	return ""
}

func parseCSVTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
		"01/02/06 03:04 PM",
		"01/02/2006 03:04 PM",
	} {
		ts, err := time.Parse(layout, value)
		if err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}

func monthSpan(start, end time.Time) []string {
	if start.IsZero() && end.IsZero() {
		return nil
	}
	if start.IsZero() {
		start = end
	}
	if end.IsZero() {
		end = start
	}
	start = time.Date(start.UTC().Year(), start.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	end = time.Date(end.UTC().Year(), end.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if end.Before(start) {
		start, end = end, start
	}

	months := []string{}
	for current := start; !current.After(end); current = current.AddDate(0, 1, 0) {
		months = append(months, current.Format("2006-01"))
	}
	return months
}

func parseDateInput(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02", value)
}

func inclusiveCalendarDays(start, end time.Time) int {
	if end.Before(start) {
		start, end = end, start
	}
	if start.Year() == end.Year() {
		return end.YearDay() - start.YearDay() + 1
	}
	days := 0
	for year := start.Year(); year <= end.Year(); year++ {
		firstDay := 1
		lastDay := 365
		if time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC).YearDay() == 366 {
			lastDay = 366
		}
		if year == start.Year() {
			firstDay = start.YearDay()
		}
		if year == end.Year() {
			lastDay = end.YearDay()
		}
		days += lastDay - firstDay + 1
	}
	return days
}

func extractionDateRange(cfg Config, now time.Time) (time.Time, time.Time, int, error) {
	start, err := parseDateInput(cfg.StartDate)
	if err != nil {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("invalid start date %q; use YYYY-MM-DD", cfg.StartDate)
	}
	end, err := parseDateInput(cfg.EndDate)
	if err != nil {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("invalid end date %q; use YYYY-MM-DD", cfg.EndDate)
	}

	if !start.IsZero() || !end.IsZero() {
		if start.IsZero() || end.IsZero() {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("both start date and end date are required when using an explicit date range")
		}
		start = start.UTC()
		end = end.UTC()
		if end.Before(start) {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("end date must be on or after start date")
		}
		days := inclusiveCalendarDays(start, end)
		return start, end, days, nil
	}

	days := cfg.Days
	if days <= 0 {
		days = 90
	}
	if days > maxExtractionChunks {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("days to fetch must be %d or fewer", maxExtractionChunks)
	}
	end = now.UTC().AddDate(0, 0, -1)
	start = end.AddDate(0, 0, -(days - 1))
	return start, end, days, nil
}

func parseChunkInterval(raw string) (time.Duration, string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "24h", "1d", "1day":
		return 24 * time.Hour, "1 day", nil
	case "12h":
		return 12 * time.Hour, "12h", nil
	case "6h":
		return 6 * time.Hour, "6h", nil
	case "3h":
		return 3 * time.Hour, "3h", nil
	case "1h":
		return 1 * time.Hour, "1h", nil
	case "30m":
		return 30 * time.Minute, "30m", nil
	case "10m":
		return 10 * time.Minute, "10m", nil
	case "5m":
		return 5 * time.Minute, "5m", nil
	default:
		return 0, "", fmt.Errorf("invalid chunk interval %q; valid options are 24h, 12h, 6h, 3h, 1h, 30m, 10m, 5m", raw)
	}
}

func buildExtractionChunks(start, end time.Time, interval time.Duration) []extractionChunk {
	rangeStart := start.UTC()
	rangeEndExclusive := end.UTC().Add(24 * time.Hour)
	if !rangeEndExclusive.After(rangeStart) || interval <= 0 {
		return nil
	}

	chunks := make([]extractionChunk, 0)
	for chunkEnd := rangeEndExclusive; chunkEnd.After(rangeStart); {
		chunkStart := chunkEnd.Add(-interval)
		if chunkStart.Before(rangeStart) {
			chunkStart = rangeStart
		}
		chunks = append(chunks, extractionChunk{Start: chunkStart, End: chunkEnd})
		chunkEnd = chunkStart
	}
	return chunks
}

func monthlyPortProtocolFromRecords(records []AnalyticsRecord) []MonthlyPortProtocolSummary {
	items := make(map[string]MonthlyPortProtocolSummary)
	observedConnections := make(map[string]map[string]struct{})
	activeConnections := make(map[string]map[string]struct{})
	for index, record := range records {
		identity := record.Identity
		if identity == "" {
			identity = fmt.Sprintf("record-%d", index)
		}
		month := strings.TrimSpace(record.Month)
		if month != "" {
			key := fmt.Sprintf("%s|%s|%d", month, record.Protocol, record.Port)
			entry := items[key]
			entry.Month = month
			entry.Protocol = record.Protocol
			entry.Port = record.Port
			entry.FlowCount += record.FlowCount
			items[key] = entry
			if observedConnections[key] == nil {
				observedConnections[key] = make(map[string]struct{})
			}
			observedConnections[key][identity] = struct{}{}
		}

		for _, activeMonth := range monthSpan(record.FirstSeen, record.LastSeen) {
			key := fmt.Sprintf("%s|%s|%d", activeMonth, record.Protocol, record.Port)
			entry := items[key]
			entry.Month = activeMonth
			entry.Protocol = record.Protocol
			entry.Port = record.Port
			items[key] = entry
			if activeConnections[key] == nil {
				activeConnections[key] = make(map[string]struct{})
			}
			activeConnections[key][identity] = struct{}{}
		}
	}

	results := make([]MonthlyPortProtocolSummary, 0, len(items))
	for key, item := range items {
		item.UniqueConnections = len(observedConnections[key])
		item.ActiveConnections = len(activeConnections[key])
		results = append(results, item)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Month != results[j].Month {
			return results[i].Month > results[j].Month
		}
		if results[i].FlowCount != results[j].FlowCount {
			return results[i].FlowCount > results[j].FlowCount
		}
		if results[i].Protocol != results[j].Protocol {
			return results[i].Protocol < results[j].Protocol
		}
		return results[i].Port < results[j].Port
	})
	return results
}

func parseCSVAnalytics(reader io.Reader) ([]PortProtocolSummary, AnalyticsInsights, error) {
	return parseCSVAnalyticsWithDimensions(reader, "env", "app")
}

type csvAnalyticsInput struct {
	Name   string
	Reader io.Reader
	SHA256 string
	Size   int64
}

type parsedAnalyticsDataset struct {
	Summary  []PortProtocolSummary
	Insights AnalyticsInsights
	Coverage DatasetCoverage
}

func parseCSVAnalyticsWithDimensions(reader io.Reader, primaryLabelKey, secondaryLabelKey string) ([]PortProtocolSummary, AnalyticsInsights, error) {
	return parseCSVAnalyticsInputs([]csvAnalyticsInput{{Name: "CSV", Reader: reader}}, primaryLabelKey, secondaryLabelKey)
}

func parseCSVAnalyticsInputs(inputs []csvAnalyticsInput, primaryLabelKey, secondaryLabelKey string) ([]PortProtocolSummary, AnalyticsInsights, error) {
	dataset, err := parseCSVAnalyticsInputsDetailed(inputs, primaryLabelKey, secondaryLabelKey)
	return dataset.Summary, dataset.Insights, err
}

func parseCSVAnalyticsInputsDetailed(inputs []csvAnalyticsInput, primaryLabelKey, secondaryLabelKey string) (parsedAnalyticsDataset, error) {
	primaryLabelKey, secondaryLabelKey, err := normalizeAnalysisLabelKeys(primaryLabelKey, secondaryLabelKey)
	if err != nil {
		return parsedAnalyticsDataset{}, err
	}
	if len(inputs) == 0 {
		return parsedAnalyticsDataset{}, fmt.Errorf("at least one CSV file is required")
	}

	allRecords := []AnalyticsRecord{}
	coverage := DatasetCoverage{Source: "csv_import", TrafficScope: trafficScopeBlocked, Files: make([]DatasetFileCoverage, 0, len(inputs))}
	seenExactRecords := make(map[string]int)
	for inputIndex, input := range inputs {
		records, err := parseCSVAnalyticsRecords(input.Reader, input.Name, primaryLabelKey, secondaryLabelKey)
		if err != nil {
			return parsedAnalyticsDataset{}, err
		}
		fileCoverage := DatasetFileCoverage{Name: input.Name, SHA256: input.SHA256, Size: input.Size, Rows: len(records)}
		monthSet := map[string]bool{}
		for _, record := range records {
			if normalizedTrafficScope(record.TrafficScope) == trafficScopeAll || (record.PolicyDecision != "" && !strings.EqualFold(record.PolicyDecision, "blocked")) {
				coverage.TrafficScope = trafficScopeAll
			}
			if fileCoverage.FirstDetected.IsZero() || (!record.FirstSeen.IsZero() && record.FirstSeen.Before(fileCoverage.FirstDetected)) {
				fileCoverage.FirstDetected = record.FirstSeen
			}
			if record.LastSeen.After(fileCoverage.LastDetected) {
				fileCoverage.LastDetected = record.LastSeen
			}
			if record.Month != "" {
				monthSet[record.Month] = true
			}
			for _, month := range monthSpan(record.FirstSeen, record.LastSeen) {
				monthSet[month] = true
			}
		}
		for month := range monthSet {
			fileCoverage.Months = append(fileCoverage.Months, month)
		}
		sort.Strings(fileCoverage.Months)
		coverage.Files = append(coverage.Files, fileCoverage)
		for _, record := range records {
			fingerprint := importedAnalyticsRecordFingerprint(record)
			if firstInput, seen := seenExactRecords[fingerprint]; seen && firstInput != inputIndex {
				coverage.DeduplicatedRecords++
				coverage.DeduplicatedFlows += record.FlowCount
				continue
			}
			seenExactRecords[fingerprint] = inputIndex
			allRecords = append(allRecords, record)
		}
	}

	mergedRecords := mergeImportedAnalyticsRecords(allRecords)
	summary := portProtocolSummaryFromRecords(mergedRecords)
	insights := buildInsightsForDimensions(mergedRecords, primaryLabelKey, secondaryLabelKey)
	// Monthly rows intentionally use the unmerged per-file records so one connection
	// observed in several monthly exports remains visible in each respective month.
	insights.MonthlyPortProtocol = monthlyPortProtocolFromRecords(allRecords)
	insights.MonthlyRelationships, insights.MonthlyExternalDestinations = monthlyDimensionAnalyticsFromRecords(allRecords)
	return parsedAnalyticsDataset{Summary: summary, Insights: insights, Coverage: normalizeCoverage(coverage)}, nil
}

func importedAnalyticsRecordFingerprint(record AnalyticsRecord) string {
	return strings.Join([]string{
		record.Identity,
		record.PolicyDecision,
		record.DraftDecision,
		normalizedTrafficScope(record.TrafficScope),
		record.Month,
		strconv.Itoa(record.FlowCount),
		record.FirstSeen.UTC().Format(time.RFC3339Nano),
		record.LastSeen.UTC().Format(time.RFC3339Nano),
		strconv.FormatBool(record.SrcManaged),
		strconv.FormatBool(record.DstManaged),
	}, "\x1f")
}

func monthlyDimensionAnalyticsFromRecords(records []AnalyticsRecord) ([]MonthlyRelationshipSummary, []MonthlyDestinationSummary) {
	relationships := map[string]MonthlyRelationshipSummary{}
	relationshipConnections := map[string]map[string]struct{}{}
	externalDestinations := map[string]MonthlyDestinationSummary{}
	externalConnections := map[string]map[string]struct{}{}
	for index, record := range records {
		month := strings.TrimSpace(record.Month)
		if month == "" {
			continue
		}
		identity := record.Identity
		if identity == "" {
			identity = fmt.Sprintf("record-%d", index)
		}
		relationshipKey := strings.Join([]string{month, record.SrcEnv, record.DstEnv}, "\x1f")
		relationship := relationships[relationshipKey]
		relationship.Month = month
		relationship.Source = record.SrcEnv
		relationship.Destination = record.DstEnv
		relationship.FlowCount += record.FlowCount
		relationships[relationshipKey] = relationship
		if relationshipConnections[relationshipKey] == nil {
			relationshipConnections[relationshipKey] = map[string]struct{}{}
		}
		relationshipConnections[relationshipKey][identity] = struct{}{}

		if !record.DstManaged {
			destinationKey := strings.Join([]string{month, record.DstIP}, "\x1f")
			destination := externalDestinations[destinationKey]
			destination.Month = month
			destination.Destination = record.DstIP
			destination.FlowCount += record.FlowCount
			externalDestinations[destinationKey] = destination
			if externalConnections[destinationKey] == nil {
				externalConnections[destinationKey] = map[string]struct{}{}
			}
			externalConnections[destinationKey][identity] = struct{}{}
		}
	}
	relationshipRows := make([]MonthlyRelationshipSummary, 0, len(relationships))
	for key, row := range relationships {
		row.UniqueConnections = len(relationshipConnections[key])
		relationshipRows = append(relationshipRows, row)
	}
	sort.Slice(relationshipRows, func(i, j int) bool {
		if relationshipRows[i].Month != relationshipRows[j].Month {
			return relationshipRows[i].Month < relationshipRows[j].Month
		}
		if relationshipRows[i].FlowCount != relationshipRows[j].FlowCount {
			return relationshipRows[i].FlowCount > relationshipRows[j].FlowCount
		}
		if relationshipRows[i].Source != relationshipRows[j].Source {
			return relationshipRows[i].Source < relationshipRows[j].Source
		}
		return relationshipRows[i].Destination < relationshipRows[j].Destination
	})
	externalRows := make([]MonthlyDestinationSummary, 0, len(externalDestinations))
	for key, row := range externalDestinations {
		row.UniqueConnections = len(externalConnections[key])
		externalRows = append(externalRows, row)
	}
	sort.Slice(externalRows, func(i, j int) bool {
		if externalRows[i].Month != externalRows[j].Month {
			return externalRows[i].Month < externalRows[j].Month
		}
		if externalRows[i].FlowCount != externalRows[j].FlowCount {
			return externalRows[i].FlowCount > externalRows[j].FlowCount
		}
		return externalRows[i].Destination < externalRows[j].Destination
	})
	return relationshipRows, externalRows
}

func parseCSVAnalyticsRecords(reader io.Reader, sourceName, primaryLabelKey, secondaryLabelKey string) ([]AnalyticsRecord, error) {
	csvReader := csv.NewReader(reader)
	rows, err := csvReader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sourceName, err)
	}
	if len(rows) < 1 {
		return nil, fmt.Errorf("%s is empty", sourceName)
	}

	headerIndex := make(map[string]int)
	sourceLabelHeaders := []string{}
	destinationLabelHeaders := []string{}
	for i, header := range rows[0] {
		normalizedHeader := strings.TrimSpace(header)
		headerIndex[strings.ToLower(normalizedHeader)] = i
		if strings.HasPrefix(strings.ToLower(normalizedHeader), "src ") {
			sourceLabelHeaders = append(sourceLabelHeaders, normalizedHeader)
		}
		if strings.HasPrefix(strings.ToLower(normalizedHeader), "dst ") {
			destinationLabelHeaders = append(destinationLabelHeaders, normalizedHeader)
		}
	}

	primarySourceHeader := "Src " + primaryLabelKey
	primaryDestinationHeader := "Dst " + primaryLabelKey
	secondarySourceHeader := "Src " + secondaryLabelKey
	secondaryDestinationHeader := "Dst " + secondaryLabelKey
	requiredHeaders := []string{
		"Source IP", "Destination IP", "Port", "Protocol", "Flows",
		primarySourceHeader, primaryDestinationHeader,
		secondarySourceHeader, secondaryDestinationHeader,
	}
	for _, header := range requiredHeaders {
		if _, ok := headerIndex[strings.ToLower(header)]; !ok {
			return nil, fmt.Errorf("%s is missing required header: %s", sourceName, header)
		}
	}

	getValue := func(row []string, header string) string {
		index, ok := headerIndex[strings.ToLower(strings.TrimSpace(header))]
		if !ok || index >= len(row) {
			return ""
		}
		return normalizeImportedCSVCell(row[index])
	}

	records := []AnalyticsRecord{}
	for rowIndex, row := range rows[1:] {
		if len(row) == 0 {
			continue
		}

		flowCount, err := strconv.Atoi(getValue(row, "Flows"))
		if err != nil || flowCount < 0 {
			return nil, fmt.Errorf("%s row %d has an invalid Flows value", sourceName, rowIndex+2)
		}
		port, err := strconv.Atoi(getValue(row, "Port"))
		if err != nil || port < 0 || port > 65535 {
			return nil, fmt.Errorf("%s row %d has an invalid Port value", sourceName, rowIndex+2)
		}
		protocol := getValue(row, "Protocol")
		if protocol == "" {
			return nil, fmt.Errorf("%s row %d has an empty Protocol value", sourceName, rowIndex+2)
		}
		protoNumber := protocolNumberFromName(protocol)
		if protoNumber == 0 {
			if numericProtocol, err := strconv.Atoi(protocol); err == nil && numericProtocol >= 0 && numericProtocol <= 255 {
				protoNumber = numericProtocol
			}
		}

		hasAnyValue := func(headers []string) bool {
			for _, header := range headers {
				value := normalizeCSVValue(getValue(row, header))
				if value != "" && !strings.EqualFold(value, "External/Unmanaged") {
					return true
				}
			}
			return false
		}
		sourceManaged := hasAnyValue(sourceLabelHeaders)
		destinationManaged := hasAnyValue(destinationLabelHeaders)

		srcEnv := normalizeCSVValue(getValue(row, primarySourceHeader))
		if srcEnv == "" {
			srcEnv = "External/Unmanaged"
			if sourceManaged {
				srcEnv = "No " + strings.ToUpper(primaryLabelKey[:1]) + primaryLabelKey[1:] + " Label"
			}
		}
		dstEnv := normalizeCSVValue(getValue(row, primaryDestinationHeader))
		if dstEnv == "" {
			dstEnv = "External/Unmanaged"
			if destinationManaged {
				dstEnv = "No " + strings.ToUpper(primaryLabelKey[:1]) + primaryLabelKey[1:] + " Label"
			}
		}
		srcApp := normalizeCSVValue(getValue(row, secondarySourceHeader))
		if srcApp == "" {
			srcApp = "External/Unmanaged"
			if sourceManaged {
				srcApp = "No " + strings.ToUpper(secondaryLabelKey[:1]) + secondaryLabelKey[1:] + " Label"
			}
		}
		dstApp := normalizeCSVValue(getValue(row, secondaryDestinationHeader))
		if dstApp == "" {
			dstApp = "External/Unmanaged"
			if destinationManaged {
				dstApp = "No " + strings.ToUpper(secondaryLabelKey[:1]) + secondaryLabelKey[1:] + " Label"
			}
		}

		dstEndpoint := getValue(row, "Destination IP")
		if fqdn := getValue(row, "FQDN"); fqdn != "" {
			dstEndpoint = fqdn
		}
		firstSeenRaw := getValue(row, "First Detected")
		lastSeenRaw := getValue(row, "Last Detected")
		firstSeen := parseCSVTimestamp(firstSeenRaw)
		lastSeen := parseCSVTimestamp(lastSeenRaw)
		if firstSeenRaw != "" && firstSeen.IsZero() {
			return nil, fmt.Errorf("%s row %d has an invalid First Detected timestamp", sourceName, rowIndex+2)
		}
		if lastSeenRaw != "" && lastSeen.IsZero() {
			return nil, fmt.Errorf("%s row %d has an invalid Last Detected timestamp", sourceName, rowIndex+2)
		}

		records = append(records, AnalyticsRecord{
			Identity:       importedCSVConnectionIdentity(row, getValue, sourceLabelHeaders, destinationLabelHeaders),
			SrcEnv:         srcEnv,
			DstEnv:         dstEnv,
			SrcApp:         srcApp,
			DstApp:         dstApp,
			SrcIP:          getValue(row, "Source IP"),
			DstIP:          dstEndpoint,
			SrcManaged:     sourceManaged,
			DstManaged:     destinationManaged,
			Protocol:       protocol,
			ProtocolNumber: protoNumber,
			Port:           port,
			Month:          parseCSVMonthBucket(row, getValue),
			FlowCount:      flowCount,
			FirstSeen:      firstSeen,
			LastSeen:       lastSeen,
			PolicyDecision: strings.ToLower(strings.TrimSpace(getValue(row, "Policy Decision"))),
			DraftDecision:  strings.ToLower(strings.TrimSpace(getValue(row, "Draft Policy Decision"))),
			TrafficScope:   normalizedTrafficScope(getValue(row, "Traffic Scope")),
		})
	}
	return records, nil
}

func importedCSVConnectionIdentity(row []string, getValue func([]string, string) string, sourceLabelHeaders, destinationLabelHeaders []string) string {
	parts := []string{
		"src_ip=" + getValue(row, "Source IP"),
		"dst_ip=" + getValue(row, "Destination IP"),
		"fqdn=" + getValue(row, "FQDN"),
		"port=" + getValue(row, "Port"),
		"protocol=" + strings.ToUpper(getValue(row, "Protocol")),
		"process=" + getValue(row, "Process Name"),
	}
	labelHeaders := append([]string{}, sourceLabelHeaders...)
	labelHeaders = append(labelHeaders, destinationLabelHeaders...)
	sort.Slice(labelHeaders, func(i, j int) bool { return strings.ToLower(labelHeaders[i]) < strings.ToLower(labelHeaders[j]) })
	for _, header := range labelHeaders {
		if value := getValue(row, header); value != "" {
			parts = append(parts, strings.ToLower(header)+"="+value)
		}
	}
	return strings.Join(parts, "\x1f")
}

func mergeImportedAnalyticsRecords(records []AnalyticsRecord) []AnalyticsRecord {
	merged := make(map[string]AnalyticsRecord, len(records))
	order := make([]string, 0, len(records))
	for index, record := range records {
		key := record.Identity
		if key == "" {
			key = fmt.Sprintf("record-%d", index)
		}
		existing, ok := merged[key]
		if !ok {
			merged[key] = record
			order = append(order, key)
			continue
		}
		existing.FlowCount += record.FlowCount
		if existing.FirstSeen.IsZero() || (!record.FirstSeen.IsZero() && record.FirstSeen.Before(existing.FirstSeen)) {
			existing.FirstSeen = record.FirstSeen
		}
		if record.LastSeen.After(existing.LastSeen) {
			existing.LastSeen = record.LastSeen
		}
		merged[key] = existing
	}
	result := make([]AnalyticsRecord, 0, len(order))
	for _, key := range order {
		result = append(result, merged[key])
	}
	return result
}

func portProtocolSummaryFromRecords(records []AnalyticsRecord) []PortProtocolSummary {
	portSummaryMap := make(map[string]PortProtocolSummary)
	for _, record := range records {
		key := fmt.Sprintf("%s:%d", record.Protocol, record.Port)
		entry := portSummaryMap[key]
		entry.Port = record.Port
		entry.Protocol = record.Protocol
		entry.ProtocolNumber = record.ProtocolNumber
		entry.FlowCount += record.FlowCount
		entry.UniqueConnections++
		portSummaryMap[key] = entry
	}

	finalSummary := make([]PortProtocolSummary, 0, len(portSummaryMap))
	for _, item := range portSummaryMap {
		finalSummary = append(finalSummary, item)
	}
	sort.Slice(finalSummary, func(i, j int) bool {
		if finalSummary[i].FlowCount != finalSummary[j].FlowCount {
			return finalSummary[i].FlowCount > finalSummary[j].FlowCount
		}
		if finalSummary[i].Port != finalSummary[j].Port {
			return finalSummary[i].Port < finalSummary[j].Port
		}
		return finalSummary[i].Protocol < finalSummary[j].Protocol
	})

	return finalSummary
}

func handleImportCSV(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	r.Body = http.MaxBytesReader(w, r.Body, maxCSVUploadSize)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("CSV upload exceeds the %d MiB limit", maxCSVUploadSize>>20))
		} else {
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "multipart CSV upload is required"})
		return
	}

	fileHeaders := append([]*multipart.FileHeader{}, r.MultipartForm.File["files"]...)
	// Keep accepting the original single-file field for API compatibility.
	fileHeaders = append(fileHeaders, r.MultipartForm.File["file"]...)
	if len(fileHeaders) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "at least one CSV file is required"})
		return
	}
	if len(fileHeaders) > maxCSVUploadFiles {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": fmt.Sprintf("select no more than %d CSV files", maxCSVUploadFiles)})
		return
	}

	inputs := make([]csvAnalyticsInput, 0, len(fileHeaders))
	openedFiles := []multipart.File{}
	defer func() {
		for _, file := range openedFiles {
			_ = file.Close()
		}
	}()
	seenDigests := make(map[string]string, len(fileHeaders))
	fileNames := make([]string, 0, len(fileHeaders))
	for _, header := range fileHeaders {
		name := filepath.Base(strings.TrimSpace(header.Filename))
		if name == "." || name == "" {
			name = "unnamed.csv"
		}
		hashFile, err := header.Open()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": fmt.Sprintf("open %s: %v", name, err)})
			return
		}
		hasher := sha256.New()
		_, hashErr := io.Copy(hasher, hashFile)
		_ = hashFile.Close()
		if hashErr != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": fmt.Sprintf("read %s: %v", name, hashErr)})
			return
		}
		digest := fmt.Sprintf("%x", hasher.Sum(nil))
		if duplicateName, duplicate := seenDigests[digest]; duplicate {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": fmt.Sprintf("%s duplicates the contents of %s", name, duplicateName)})
			return
		}
		seenDigests[digest] = name

		file, err := header.Open()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": fmt.Sprintf("open %s: %v", name, err)})
			return
		}
		openedFiles = append(openedFiles, file)
		inputs = append(inputs, csvAnalyticsInput{Name: name, Reader: file, SHA256: digest, Size: header.Size})
		fileNames = append(fileNames, name)
	}

	primaryLabelKey := r.FormValue("primary_label_key")
	secondaryLabelKey := r.FormValue("secondary_label_key")
	parsed, err := parseCSVAnalyticsInputsDetailed(inputs, primaryLabelKey, secondaryLabelKey)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	fileName := "Imported CSV: " + fileNames[0]
	if len(fileNames) > 1 {
		fileName = fmt.Sprintf("Imported CSV set: %d files", len(fileNames))
	}
	reportMetadata := ReportMetadata{Title: reportTitleForScope(parsed.Coverage.TrafficScope)}
	datasetID := ""
	if datasetName := strings.TrimSpace(r.FormValue("dataset_name")); datasetName != "" {
		saved, err := datasetManager.saveDataset(SavedDataset{
			Name: datasetName, FileName: fileName, Summary: parsed.Summary, Insights: parsed.Insights,
			Coverage: parsed.Coverage, Report: reportMetadata,
		})
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
		datasetID = saved.ID
	}

	state.Mu.Lock()
	state.LastSummary = parsed.Summary
	state.LastInsights = parsed.Insights
	state.FileName = fileName
	state.DatasetID = datasetID
	state.DatasetCoverage = parsed.Coverage
	state.ReportMetadata = reportMetadata
	state.TrafficScope = normalizedTrafficScope(parsed.Coverage.TrafficScope)
	state.IsDone = true
	state.IsCancelled = false
	state.Mu.Unlock()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"fileName":  fileName,
		"fileCount": len(fileNames),
		"files":     fileNames,
		"datasetId": datasetID,
		"coverage":  parsed.Coverage,
	})
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireSameOrigin(w, r) {
		return
	}
	var cfg Config
	if err := decodeJSONBody(w, r, &cfg); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	resolved, ctx, err := beginExtraction(cfg)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already running") {
			status = http.StatusConflict
		}
		writeJSONError(w, status, err.Error())
		return
	}

	addLog("Starting extraction...")
	go runExtraction(ctx, resolved)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func beginExtraction(cfg Config) (Config, context.Context, error) {
	return beginExtractionWithContext(context.Background(), cfg)
}

func beginExtractionWithContext(parent context.Context, cfg Config) (Config, context.Context, error) {
	if parent == nil {
		parent = context.Background()
	}
	resolved, err := resolveConfigCredentials(cfg)
	if err != nil {
		return Config{}, nil, err
	}
	_, _, requestedDays, err := extractionDateRange(resolved, time.Now().UTC())
	if err != nil {
		return Config{}, nil, err
	}
	chunkDuration, chunkLabel, err := parseChunkInterval(resolved.ChunkIntvl)
	if err != nil {
		return Config{}, nil, err
	}
	requestedChunks := requestedDays * int((24*time.Hour)/chunkDuration)
	if requestedChunks == 0 {
		return Config{}, nil, fmt.Errorf("the requested extraction window produced no query chunks")
	}
	if requestedChunks > maxExtractionChunks {
		return Config{}, nil, fmt.Errorf("the requested extraction would create %d chunks; the limit is %d", requestedChunks, maxExtractionChunks)
	}

	state.Mu.Lock()
	if state.CancelFunc != nil {
		state.Mu.Unlock()
		return Config{}, nil, fmt.Errorf("an extraction is already running")
	}
	state.CompletedChunks = 0
	state.RequestedDays = requestedDays
	state.RequestedChunks = requestedChunks
	state.ChunkInterval = chunkLabel
	state.ActiveChunks = 0
	state.RunStartedAt = time.Now().UTC()
	state.LastProgressAt = state.RunStartedAt
	state.TotalConnections = 0
	state.IsDone = false
	state.IsCancelled = false
	state.Logs = []string{}
	state.FileName = ""
	state.LastSummary = nil
	state.LastInsights = AnalyticsInsights{}
	state.DatasetID = ""
	state.DatasetCoverage = DatasetCoverage{}
	state.ReportMetadata = ReportMetadata{Title: reportTitleForScope(resolved.TrafficScope)}
	state.TrafficScope = resolved.TrafficScope
	state.RunError = ""

	ctx, cancel := context.WithTimeout(parent, maxExtractionTime)
	state.CancelFunc = cancel
	state.Mu.Unlock()
	return resolved, ctx, nil
}

func canonicalFlowLabels(labels []illumio.FlowLabel) string {
	values := make([]string, 0, len(labels))
	for _, label := range labels {
		values = append(values, strings.ToLower(label.Key)+"="+label.Value)
	}
	sort.Strings(values)
	return strings.Join(values, "\x1f")
}

func safeCSVCell(value string) string {
	if value == "" {
		return value
	}
	trimmedLeft := strings.TrimLeft(value, " \r\n")
	if trimmedLeft == "" {
		return value
	}
	switch trimmedLeft[0] {
	case '=', '+', '-', '@', '\t':
		return "'" + value
	default:
		return value
	}
}

func normalizeImportedCSVCell(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1 && value[0] == '\'' {
		switch value[1] {
		case '=', '+', '-', '@', '\t':
			return value[1:]
		}
	}
	return value
}

func outputCSVPath(savePath, fileName string) (string, error) {
	name := strings.TrimSpace(fileName)
	if name == "" {
		name = fmt.Sprintf("BT_%s.csv", time.Now().Format("20060102_150405"))
	}
	if name != filepath.Base(name) || name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("target filename must be a filename without directory components")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".csv") {
		name += ".csv"
	}
	dir := strings.TrimSpace(savePath)
	if dir == "" {
		return name, nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("target folder must be an absolute path")
	}
	return filepath.Join(filepath.Clean(dir), name), nil
}

func looksLikeIPAddress(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if ip := net.ParseIP(value); ip != nil {
		return true
	}
	if _, _, err := net.ParseCIDR(value); err == nil {
		return true
	}
	return false
}

func parseSelector(token string, labelMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap map[string]string) (illumio.LabelRef, bool) {
	name := strings.TrimSpace(token)
	if name == "" {
		return illumio.LabelRef{}, false
	}

	ref := illumio.LabelRef{}
	switch {
	case labelMap[name] != "":
		ref.Label = &illumio.Href{Href: labelMap[name]}
	case ipListMap[name] != "":
		ref.IPList = &illumio.Href{Href: ipListMap[name]}
	case lgMap[name] != "":
		ref.LabelGroup = &illumio.Href{Href: lgMap[name]}
	case ugMap[name] != "":
		ref.UserGroup = &illumio.Href{Href: ugMap[name]}
	case vsMap[name] != "":
		ref.VirtualService = &illumio.Href{Href: vsMap[name]}
	case vsvrMap[name] != "":
		ref.VirtualServer = &illumio.Href{Href: vsvrMap[name]}
	default:
		if !looksLikeIPAddress(name) {
			return illumio.LabelRef{}, false
		}
		ref.IPAddress = name
	}

	return ref, true
}

func buildIncludeGroups(raw string, labelMap, labelKeyMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap map[string]string) ([][]illumio.LabelRef, []string) {
	groupsByKey := make(map[string][]illumio.LabelRef)
	ipGroup := []illumio.LabelRef{}
	groupOrder := []string{}
	warnings := []string{}

	for _, token := range strings.Split(raw, ",") {
		ref, ok := parseSelector(token, labelMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap)
		if !ok {
			if trimmed := strings.TrimSpace(token); trimmed != "" {
				warnings = append(warnings, trimmed)
			}
			continue
		}

		groupKey := "ip_address"
		switch {
		case ref.Label != nil:
			if key := labelKeyMap[strings.TrimSpace(token)]; key != "" {
				groupKey = "label_key:" + strings.ToLower(key)
			} else {
				groupKey = "label:" + ref.Label.Href
			}
		case ref.LabelGroup != nil:
			groupKey = "label_group"
		case ref.IPList != nil:
			groupKey = "ip_list"
		case ref.UserGroup != nil:
			groupKey = "user_group"
		case ref.VirtualService != nil:
			groupKey = "virtual_service"
		case ref.VirtualServer != nil:
			groupKey = "virtual_server"
		}

		if groupKey == "ip_address" {
			ipGroup = append(ipGroup, ref)
			continue
		}
		if _, exists := groupsByKey[groupKey]; !exists {
			groupOrder = append(groupOrder, groupKey)
		}
		groupsByKey[groupKey] = append(groupsByKey[groupKey], ref)
	}

	groups := make([][]illumio.LabelRef, 0, len(groupOrder)+1)
	for _, key := range groupOrder {
		groups = append(groups, groupsByKey[key])
	}
	if len(ipGroup) > 0 {
		groups = append(groups, ipGroup)
	}

	return groups, warnings
}

func buildExcludeRefs(raw string, labelMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap map[string]string) ([]illumio.LabelRef, []string) {
	refs := []illumio.LabelRef{}
	warnings := []string{}
	for _, token := range strings.Split(raw, ",") {
		ref, ok := parseSelector(token, labelMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap)
		if ok {
			refs = append(refs, ref)
		} else if trimmed := strings.TrimSpace(token); trimmed != "" {
			warnings = append(warnings, trimmed)
		}
	}
	return refs, warnings
}

func parseProtocolNumber(raw string) (int, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "TCP":
		return 6, true
	case "UDP":
		return 17, true
	case "ICMP":
		return 1, true
	case "IGMP":
		return 2, true
	case "GRE":
		return 47, true
	}

	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 || value > 255 {
		return 0, false
	}
	return value, true
}

func parseDirectService(entry string) (illumio.PortProtoService, bool) {
	parts := strings.SplitN(entry, ":", 2)
	if len(parts) != 2 {
		return illumio.PortProtoService{}, false
	}

	proto, ok := parseProtocolNumber(parts[0])
	if !ok {
		return illumio.PortProtoService{}, false
	}

	portSpec := strings.TrimSpace(parts[1])
	if portSpec == "" {
		return illumio.PortProtoService{}, false
	}

	service := illumio.PortProtoService{Proto: proto}
	if strings.Contains(portSpec, "-") {
		rangeParts := strings.SplitN(portSpec, "-", 2)
		if len(rangeParts) != 2 {
			return illumio.PortProtoService{}, false
		}

		startPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
		if err != nil || startPort < 1 || startPort > 65535 {
			return illumio.PortProtoService{}, false
		}

		endPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
		if err != nil || endPort < startPort || endPort > 65535 {
			return illumio.PortProtoService{}, false
		}

		service.Port = startPort
		service.ToPort = endPort
		return service, true
	}

	port, err := strconv.Atoi(portSpec)
	if err != nil || port < 1 || port > 65535 {
		return illumio.PortProtoService{}, false
	}

	service.Port = port
	return service, true
}

func serviceEntriesFromService(service illumio.Service) []interface{} {
	entries := make([]interface{}, 0, len(service.ServicePorts))

	for _, port := range service.ServicePorts {
		entry := illumio.PortProtoService{
			Port:     port.Port,
			ToPort:   port.ToPort,
			Proto:    port.Proto,
			ICMPType: port.ICMPType,
			ICMPCode: port.ICMPCode,
		}
		entries = append(entries, entry)
	}

	return entries
}

func buildServiceIncludeEntries(raw string, serviceMap map[string][]interface{}) ([]interface{}, []string) {
	includes := make([]interface{}, 0)
	warnings := make([]string, 0)

	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if directService, ok := parseDirectService(entry); ok {
			includes = append(includes, directService)
			continue
		}
		if expanded, ok := serviceMap[entry]; ok && len(expanded) > 0 {
			includes = append(includes, expanded...)
			continue
		}
		warnings = append(warnings, entry)
	}

	return includes, warnings
}

func buildServiceFilter(includeRaw, excludeRaw string, serviceMap map[string][]interface{}) (illumio.ServiceFilter, []string, []string) {
	includeEntries, includeWarnings := buildServiceIncludeEntries(includeRaw, serviceMap)
	excludeEntries, excludeWarnings := buildServiceIncludeEntries(excludeRaw, serviceMap)
	return illumio.ServiceFilter{
		Include: append(make([]interface{}, 0, len(includeEntries)), includeEntries...),
		Exclude: append(make([]interface{}, 0, len(excludeEntries)), excludeEntries...),
	}, includeWarnings, excludeWarnings
}

func uniqueJoinedLabelValues(labels []illumio.FlowLabel, key string) string {
	seen := make(map[string]bool)
	values := []string{}
	for _, label := range labels {
		if strings.EqualFold(label.Key, key) {
			if !seen[label.Value] {
				seen[label.Value] = true
				values = append(values, label.Value)
			}
		}
	}
	if len(values) == 0 {
		return ""
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}

func managedLabelValue(labels []illumio.FlowLabel, key string) string {
	value := uniqueJoinedLabelValues(labels, key)
	if value == "" {
		return "No " + strings.ToUpper(key[:1]) + key[1:] + " Label"
	}
	return value
}

func endpointHasClassification(flow illumio.TrafficFlow, isSource bool) bool {
	if isSource {
		return flow.SrcWorkloadHref != "" || len(flow.SrcLabels) > 0
	}
	return flow.DstWorkloadHref != "" || len(flow.DstLabels) > 0
}

func externalOrManagedLabel(flow illumio.TrafficFlow, isSource bool, key string) string {
	var labels []illumio.FlowLabel
	if isSource {
		labels = flow.SrcLabels
	} else {
		labels = flow.DstLabels
	}
	if len(labels) == 0 && !endpointHasClassification(flow, isSource) {
		return "External/Unmanaged"
	}
	return managedLabelValue(labels, key)
}

func endpointDisplayName(flow illumio.TrafficFlow, isSource bool) string {
	if isSource {
		if flow.SrcIP != "" {
			return flow.SrcIP
		}
		if flow.SrcWorkloadHref != "" {
			return flow.SrcWorkloadHref
		}
		return "Unknown Source"
	}
	if flow.DstIP != "" {
		return flow.DstIP
	}
	if flow.DstFQDN != "" {
		return flow.DstFQDN
	}
	if flow.DstWorkloadHref != "" {
		return flow.DstWorkloadHref
	}
	return "Unknown Destination"
}

func topTalkersFromMap(items map[string]TalkerSummary, limit int) []TalkerSummary {
	results := make([]TalkerSummary, 0, len(items))
	for _, item := range items {
		results = append(results, item)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].FlowCount != results[j].FlowCount {
			return results[i].FlowCount > results[j].FlowCount
		}
		return results[i].Name < results[j].Name
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func matrixFromMap(items map[string]MatrixSummary) []MatrixSummary {
	results := make([]MatrixSummary, 0, len(items))
	for _, item := range items {
		results = append(results, item)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].FlowCount != results[j].FlowCount {
			return results[i].FlowCount > results[j].FlowCount
		}
		if results[i].Source != results[j].Source {
			return results[i].Source < results[j].Source
		}
		return results[i].Destination < results[j].Destination
	})
	return results
}

func categoryList(items map[string]TrafficCategorySummary) []TrafficCategorySummary {
	results := make([]TrafficCategorySummary, 0, len(items))
	for _, item := range items {
		results = append(results, item)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].FlowCount != results[j].FlowCount {
			return results[i].FlowCount > results[j].FlowCount
		}
		return results[i].Name < results[j].Name
	})
	return results
}

func buildInsights(records []AnalyticsRecord) AnalyticsInsights {
	return buildInsightsForDimensions(records, "env", "app")
}

func buildInsightsForDimensions(records []AnalyticsRecord, primaryLabelKey, secondaryLabelKey string) AnalyticsInsights {
	envMatrixMap := make(map[string]MatrixSummary)
	appMatrixMap := make(map[string]MatrixSummary)
	topSourceEnvMap := make(map[string]TalkerSummary)
	topDestinationEnvMap := make(map[string]TalkerSummary)
	topSourceIPMap := make(map[string]TalkerSummary)
	topDestinationIPMap := make(map[string]TalkerSummary)
	topExternalDestinationIPMap := make(map[string]TalkerSummary)
	topAppPairMap := make(map[string]TalkerSummary)
	categoryMap := make(map[string]TrafficCategorySummary)
	envServicePivotMap := make(map[string]EnvServicePivotSummary)
	appServicePivotMap := make(map[string]AppServicePivotSummary)
	combinedServicePivotMap := make(map[string]CombinedServicePivotSummary)
	sourceEnvSet := make(map[string]bool)
	sourceAppSet := make(map[string]bool)
	sourceCombinedSet := make(map[string]bool)

	for _, record := range records {
		sourceCombined := record.SrcEnv + " / " + record.SrcApp
		destinationCombined := record.DstEnv + " / " + record.DstApp
		sourceEnvSet[record.SrcEnv] = true
		sourceAppSet[record.SrcApp] = true
		sourceCombinedSet[sourceCombined] = true

		envKey := record.SrcEnv + "->" + record.DstEnv
		envEntry := envMatrixMap[envKey]
		envEntry.Source = record.SrcEnv
		envEntry.Destination = record.DstEnv
		envEntry.FlowCount += record.FlowCount
		envEntry.UniqueConnections++
		envMatrixMap[envKey] = envEntry

		appKey := record.SrcApp + "->" + record.DstApp
		appEntry := appMatrixMap[appKey]
		appEntry.Source = record.SrcApp
		appEntry.Destination = record.DstApp
		appEntry.FlowCount += record.FlowCount
		appEntry.UniqueConnections++
		appMatrixMap[appKey] = appEntry

		srcEnvEntry := topSourceEnvMap[record.SrcEnv]
		srcEnvEntry.Name = record.SrcEnv
		srcEnvEntry.FlowCount += record.FlowCount
		srcEnvEntry.UniqueConnections++
		topSourceEnvMap[record.SrcEnv] = srcEnvEntry

		dstEnvEntry := topDestinationEnvMap[record.DstEnv]
		dstEnvEntry.Name = record.DstEnv
		dstEnvEntry.FlowCount += record.FlowCount
		dstEnvEntry.UniqueConnections++
		topDestinationEnvMap[record.DstEnv] = dstEnvEntry

		srcIPEntry := topSourceIPMap[record.SrcIP]
		srcIPEntry.Name = record.SrcIP
		srcIPEntry.FlowCount += record.FlowCount
		srcIPEntry.UniqueConnections++
		topSourceIPMap[record.SrcIP] = srcIPEntry

		dstEndpoint := record.DstIP
		dstIPEntry := topDestinationIPMap[dstEndpoint]
		dstIPEntry.Name = dstEndpoint
		dstIPEntry.FlowCount += record.FlowCount
		dstIPEntry.UniqueConnections++
		topDestinationIPMap[dstEndpoint] = dstIPEntry
		if !record.DstManaged {
			externalDstEntry := topExternalDestinationIPMap[dstEndpoint]
			externalDstEntry.Name = dstEndpoint
			externalDstEntry.FlowCount += record.FlowCount
			externalDstEntry.UniqueConnections++
			topExternalDestinationIPMap[dstEndpoint] = externalDstEntry
		}

		appPairName := record.SrcApp + " -> " + record.DstApp
		appPairEntry := topAppPairMap[appPairName]
		appPairEntry.Name = appPairName
		appPairEntry.FlowCount += record.FlowCount
		appPairEntry.UniqueConnections++
		topAppPairMap[appPairName] = appPairEntry

		categoryName := "Internal -> Internal"
		switch {
		case record.SrcManaged && !record.DstManaged:
			categoryName = "Internal -> External/Unmanaged"
		case !record.SrcManaged && record.DstManaged:
			categoryName = "External/Unmanaged -> Internal"
		case !record.SrcManaged && !record.DstManaged:
			categoryName = "External/Unmanaged -> External/Unmanaged"
		}
		categoryEntry := categoryMap[categoryName]
		categoryEntry.Name = categoryName
		categoryEntry.FlowCount += record.FlowCount
		categoryEntry.UniqueConnections++
		categoryMap[categoryName] = categoryEntry

		pivotKey := fmt.Sprintf("%s|%s|%s|%d", record.SrcEnv, record.DstEnv, record.Protocol, record.Port)
		pivotEntry := envServicePivotMap[pivotKey]
		pivotEntry.SourceEnv = record.SrcEnv
		pivotEntry.DestinationEnv = record.DstEnv
		pivotEntry.Protocol = record.Protocol
		pivotEntry.Port = record.Port
		pivotEntry.FlowCount += record.FlowCount
		pivotEntry.UniqueConnections++
		envServicePivotMap[pivotKey] = pivotEntry

		appPivotKey := fmt.Sprintf("%s|%s|%s|%d", record.SrcApp, record.DstApp, record.Protocol, record.Port)
		appPivotEntry := appServicePivotMap[appPivotKey]
		appPivotEntry.SourceApp = record.SrcApp
		appPivotEntry.DestinationApp = record.DstApp
		appPivotEntry.Protocol = record.Protocol
		appPivotEntry.Port = record.Port
		appPivotEntry.FlowCount += record.FlowCount
		appPivotEntry.UniqueConnections++
		appServicePivotMap[appPivotKey] = appPivotEntry

		combinedPivotKey := fmt.Sprintf("%s|%s|%s|%d", sourceCombined, destinationCombined, record.Protocol, record.Port)
		combinedPivotEntry := combinedServicePivotMap[combinedPivotKey]
		combinedPivotEntry.SourceCombined = sourceCombined
		combinedPivotEntry.DestinationCombined = destinationCombined
		combinedPivotEntry.Protocol = record.Protocol
		combinedPivotEntry.Port = record.Port
		combinedPivotEntry.FlowCount += record.FlowCount
		combinedPivotEntry.UniqueConnections++
		combinedServicePivotMap[combinedPivotKey] = combinedPivotEntry
	}

	envServicePivot := make([]EnvServicePivotSummary, 0, len(envServicePivotMap))
	for _, item := range envServicePivotMap {
		envServicePivot = append(envServicePivot, item)
	}
	sort.Slice(envServicePivot, func(i, j int) bool {
		if envServicePivot[i].SourceEnv != envServicePivot[j].SourceEnv {
			return envServicePivot[i].SourceEnv < envServicePivot[j].SourceEnv
		}
		if envServicePivot[i].Protocol != envServicePivot[j].Protocol {
			return envServicePivot[i].Protocol < envServicePivot[j].Protocol
		}
		if envServicePivot[i].Port != envServicePivot[j].Port {
			return envServicePivot[i].Port < envServicePivot[j].Port
		}
		if envServicePivot[i].FlowCount != envServicePivot[j].FlowCount {
			return envServicePivot[i].FlowCount > envServicePivot[j].FlowCount
		}
		return envServicePivot[i].DestinationEnv < envServicePivot[j].DestinationEnv
	})

	sourceEnvOptions := make([]string, 0, len(sourceEnvSet))
	for name := range sourceEnvSet {
		sourceEnvOptions = append(sourceEnvOptions, name)
	}
	sort.Strings(sourceEnvOptions)

	appServicePivot := make([]AppServicePivotSummary, 0, len(appServicePivotMap))
	for _, item := range appServicePivotMap {
		appServicePivot = append(appServicePivot, item)
	}
	sort.Slice(appServicePivot, func(i, j int) bool {
		if appServicePivot[i].SourceApp != appServicePivot[j].SourceApp {
			return appServicePivot[i].SourceApp < appServicePivot[j].SourceApp
		}
		if appServicePivot[i].Protocol != appServicePivot[j].Protocol {
			return appServicePivot[i].Protocol < appServicePivot[j].Protocol
		}
		if appServicePivot[i].Port != appServicePivot[j].Port {
			return appServicePivot[i].Port < appServicePivot[j].Port
		}
		if appServicePivot[i].FlowCount != appServicePivot[j].FlowCount {
			return appServicePivot[i].FlowCount > appServicePivot[j].FlowCount
		}
		return appServicePivot[i].DestinationApp < appServicePivot[j].DestinationApp
	})

	sourceAppOptions := make([]string, 0, len(sourceAppSet))
	for name := range sourceAppSet {
		sourceAppOptions = append(sourceAppOptions, name)
	}
	sort.Strings(sourceAppOptions)

	combinedServicePivot := make([]CombinedServicePivotSummary, 0, len(combinedServicePivotMap))
	for _, item := range combinedServicePivotMap {
		combinedServicePivot = append(combinedServicePivot, item)
	}
	sort.Slice(combinedServicePivot, func(i, j int) bool {
		if combinedServicePivot[i].SourceCombined != combinedServicePivot[j].SourceCombined {
			return combinedServicePivot[i].SourceCombined < combinedServicePivot[j].SourceCombined
		}
		if combinedServicePivot[i].Protocol != combinedServicePivot[j].Protocol {
			return combinedServicePivot[i].Protocol < combinedServicePivot[j].Protocol
		}
		if combinedServicePivot[i].Port != combinedServicePivot[j].Port {
			return combinedServicePivot[i].Port < combinedServicePivot[j].Port
		}
		if combinedServicePivot[i].FlowCount != combinedServicePivot[j].FlowCount {
			return combinedServicePivot[i].FlowCount > combinedServicePivot[j].FlowCount
		}
		return combinedServicePivot[i].DestinationCombined < combinedServicePivot[j].DestinationCombined
	})

	sourceCombinedOptions := make([]string, 0, len(sourceCombinedSet))
	for name := range sourceCombinedSet {
		sourceCombinedOptions = append(sourceCombinedOptions, name)
	}
	sort.Strings(sourceCombinedOptions)

	return AnalyticsInsights{
		PrimaryLabelKey:           primaryLabelKey,
		SecondaryLabelKey:         secondaryLabelKey,
		EnvMatrix:                 matrixFromMap(envMatrixMap),
		AppMatrix:                 matrixFromMap(appMatrixMap),
		TopSourceEnvs:             topTalkersFromMap(topSourceEnvMap, 12),
		TopDestinationEnvs:        topTalkersFromMap(topDestinationEnvMap, 12),
		TopSourceIPs:              topTalkersFromMap(topSourceIPMap, 12),
		TopDestinationIPs:         topTalkersFromMap(topDestinationIPMap, 12),
		TopExternalDestinationIPs: topTalkersFromMap(topExternalDestinationIPMap, 12),
		TopAppPairs:               topTalkersFromMap(topAppPairMap, 15),
		TrafficCategories:         categoryList(categoryMap),
		EnvServicePivot:           envServicePivot,
		SourceEnvOptions:          sourceEnvOptions,
		AppServicePivot:           appServicePivot,
		SourceAppOptions:          sourceAppOptions,
		CombinedServicePivot:      combinedServicePivot,
		SourceCombinedOptions:     sourceCombinedOptions,
	}
}

func runExtraction(ctx context.Context, cfg Config) {
	addLog("Dashboard monitoring refreshes continue independently while this extraction runs.")
	client := illumio.NewClient(cfg.PCEURL, cfg.OrgID, cfg.APIKey, cfg.APISecret)
	var discoveryData DiscoveryData
	cacheKey := discoveryCacheKey(cfg)
	state.Mu.Lock()
	if state.DiscoveryCache != nil && state.DiscoveryKey == cacheKey {
		discoveryData = *state.DiscoveryCache
	}
	state.Mu.Unlock()

	if len(discoveryData.Labels) == 0 {
		addLog("Extraction: loading policy objects for request building...")
		fetchedDiscovery, err := fetchDiscoveryData(ctx, client, "Extraction discovery:")
		if err != nil {
			addLog(fmt.Sprintf("Error: %v", err))
			markRunFinished("", ctx.Err() != nil)
			return
		}
		discoveryData = fetchedDiscovery
		state.Mu.Lock()
		state.DiscoveryCache = &fetchedDiscovery
		state.DiscoveryKey = cacheKey
		state.Mu.Unlock()
	} else {
		addLog("Extraction: using cached discovery objects from the last policy-object load.")
	}

	allLabels := discoveryData.Labels
	allServices := discoveryData.Services
	allIPLists := discoveryData.IPLists
	allLabelGroups := discoveryData.LabelGroups
	allUserGroups := discoveryData.UserGroups
	allVServices := discoveryData.VirtualServices
	allVServers := discoveryData.VirtualServers
	primaryLabelKey, secondaryLabelKey, err := resolveAnalysisLabelKeys(cfg.AnalysisPrimary, cfg.AnalysisSecondary, allLabels)
	if err != nil {
		addLog(fmt.Sprintf("Error: %v", err))
		markRunFinished("", false)
		return
	}
	addLog(fmt.Sprintf("Analysis dimensions: primary=%s, secondary=%s", primaryLabelKey, secondaryLabelKey))

	labelMap := make(map[string]string)
	labelKeyMap := make(map[string]string)
	uniqueKeys := make(map[string]bool)
	for _, l := range allLabels {
		labelMap[l.Value] = l.Href
		labelKeyMap[l.Value] = l.Key
		if l.Key != "" {
			uniqueKeys[l.Key] = true
		}
	}

	standard := []string{"role", "app", "env", "loc"}
	orderedKeys := []string{}
	for _, k := range standard {
		if uniqueKeys[k] {
			orderedKeys = append(orderedKeys, k)
			delete(uniqueKeys, k)
		}
	}
	standardKeyCount := len(orderedKeys)
	for k := range uniqueKeys {
		orderedKeys = append(orderedKeys, k)
	}
	sort.Strings(orderedKeys[standardKeyCount:])

	serviceMap := make(map[string][]interface{})
	for _, s := range allServices {
		serviceMap[s.Name] = serviceEntriesFromService(s)
	}
	ipListMap := make(map[string]string)
	for _, i := range allIPLists {
		ipListMap[i.Name] = i.Href
	}
	lgMap := make(map[string]string)
	for _, i := range allLabelGroups {
		lgMap[i.Name] = i.Href
	}
	ugMap := make(map[string]string)
	for _, i := range allUserGroups {
		ugMap[i.Name] = i.Href
	}
	vsMap := make(map[string]string)
	for _, i := range allVServices {
		vsMap[i.Name] = i.Href
	}
	vsvrMap := make(map[string]string)
	for _, i := range allVServers {
		vsvrMap[i.Name] = i.Href
	}

	serviceFilter, serviceWarnings, serviceExclusionWarnings := buildServiceFilter(cfg.Services, cfg.ExcludeServices, serviceMap)
	req := illumio.AsyncQueryRequest{
		Sources: illumio.IncludeExclude{
			Include: [][]illumio.LabelRef{},
			Exclude: []illumio.LabelRef{},
		},
		Destinations: illumio.IncludeExclude{
			Include: [][]illumio.LabelRef{},
			Exclude: []illumio.LabelRef{},
		},
		Services:        serviceFilter,
		PolicyDecisions: policyDecisionsForScope(cfg.TrafficScope),
	}
	for _, entry := range serviceWarnings {
		addLog(fmt.Sprintf("Warning: skipped unknown service filter '%s'", entry))
	}
	for _, entry := range serviceExclusionWarnings {
		addLog(fmt.Sprintf("Warning: skipped unknown service exclusion '%s'", entry))
	}

	var selectorWarnings []string
	req.Sources.Include, selectorWarnings = buildIncludeGroups(cfg.SrcLabels, labelMap, labelKeyMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap)
	for _, warning := range selectorWarnings {
		addLog(fmt.Sprintf("Warning: skipped unknown source selector '%s'", warning))
	}
	req.Destinations.Include, selectorWarnings = buildIncludeGroups(cfg.DstLabels, labelMap, labelKeyMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap)
	for _, warning := range selectorWarnings {
		addLog(fmt.Sprintf("Warning: skipped unknown destination selector '%s'", warning))
	}
	req.Sources.Exclude, selectorWarnings = buildExcludeRefs(cfg.ExcludeSrc, labelMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap)
	for _, warning := range selectorWarnings {
		addLog(fmt.Sprintf("Warning: skipped unknown source exclusion '%s'", warning))
	}
	req.Destinations.Exclude, selectorWarnings = buildExcludeRefs(cfg.ExcludeDst, labelMap, ipListMap, lgMap, ugMap, vsMap, vsvrMap)
	for _, warning := range selectorWarnings {
		addLog(fmt.Sprintf("Warning: skipped unknown destination exclusion '%s'", warning))
	}

	type FlowKey struct {
		SrcIP, DstIP         string
		Port, Proto          int
		SrcWkld, DstWkld     string
		Process, FQDN        string
		SrcLabels, DstLabels string
	}
	type AggregateKey struct {
		Connection     FlowKey
		PolicyDecision string
		DraftDecision  string
	}
	flowIdentity := func(key FlowKey) string {
		return strings.Join([]string{
			key.SrcIP, key.DstIP, strconv.Itoa(key.Port), strconv.Itoa(key.Proto),
			key.SrcWkld, key.DstWkld, key.Process, key.FQDN, key.SrcLabels, key.DstLabels,
		}, "\x1f")
	}
	aggregatedFlows := make(map[AggregateKey]struct {
		TotalCount          int
		FirstSeen, LastSeen time.Time
		Raw                 illumio.TrafficFlow
	})
	connectionSet := make(map[FlowKey]struct{})
	monthlySummaryMap := make(map[string]MonthlyPortProtocolSummary)
	monthlyUniqueConnectionSet := make(map[string]map[FlowKey]struct{})
	monthlyActiveConnectionSet := make(map[string]map[FlowKey]struct{})
	monthlyRelationshipMap := make(map[string]MonthlyRelationshipSummary)
	monthlyRelationshipSet := make(map[string]map[FlowKey]struct{})
	monthlyExternalDestinationMap := make(map[string]MonthlyDestinationSummary)
	monthlyExternalDestinationSet := make(map[string]map[FlowKey]struct{})
	var aggMu sync.Mutex
	protoMap := map[int]string{1: "ICMP", 2: "IGMP", 6: "TCP", 17: "UDP", 47: "GRE", 50: "ESP", 51: "AH", 58: "ICMPv6", 89: "OSPF", 112: "VRRP", 132: "SCTP"}

	now := time.Now().UTC()
	rangeStart, rangeEnd, requestedDays, err := extractionDateRange(cfg, now)
	if err != nil {
		addLog(fmt.Sprintf("Error: %v", err))
		markRunFinished("", false)
		return
	}
	chunkDuration, chunkLabel, err := parseChunkInterval(cfg.ChunkIntvl)
	if err != nil {
		addLog(fmt.Sprintf("Error: %v", err))
		markRunFinished("", false)
		return
	}
	chunks := buildExtractionChunks(rangeStart, rangeEnd, chunkDuration)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	jobs := make(chan int, len(chunks))
	var wg sync.WaitGroup
	var extractionErr error
	var extractionErrOnce sync.Once
	for w := 1; w <= 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunkIdx := range jobs {
				select {
				case <-runCtx.Done():
					return
				default:
					state.Mu.Lock()
					state.ActiveChunks++
					state.LastProgressAt = time.Now().UTC()
					state.Mu.Unlock()
					finishChunk := func() {
						state.Mu.Lock()
						if state.ActiveChunks > 0 {
							state.ActiveChunks--
						}
						state.LastProgressAt = time.Now().UTC()
						state.Mu.Unlock()
					}
					chunkLog := func(message string) {
						state.Mu.Lock()
						state.LastProgressAt = time.Now().UTC()
						state.Mu.Unlock()
						addLog(fmt.Sprintf("Chunk %d/%d: %s", chunkIdx+1, len(chunks), message))
					}
					chunkReq := req
					chunkReq.StartDate = chunks[chunkIdx].Start.Format(time.RFC3339)
					chunkReq.EndDate = chunks[chunkIdx].End.Format(time.RFC3339)
					var flows []illumio.TrafficFlow
					var err error
					for attempt := 1; attempt <= maxChunkAttempts; attempt++ {
						chunkCtx, chunkCancel := context.WithTimeout(runCtx, maxChunkQueryTime)
						flows, err = client.FetchDayOfTraffic(chunkCtx, chunkReq, chunkLog)
						chunkCancel()
						if err == nil || runCtx.Err() != nil {
							break
						}
						if attempt < maxChunkAttempts {
							chunkLog(fmt.Sprintf("attempt %d/%d failed: %v; retrying...", attempt, maxChunkAttempts, err))
							delay := time.Duration(1<<uint(attempt-1)) * time.Second
							select {
							case <-time.After(delay):
							case <-runCtx.Done():
								finishChunk()
								return
							}
						}
					}
					finishChunk()
					if err == nil {
						aggMu.Lock()
						for _, f := range flows {
							connectionKey := FlowKey{
								SrcIP: f.SrcIP, DstIP: f.DstIP, Port: f.DstPort, Proto: f.Proto,
								SrcWkld: f.SrcWorkloadHref, DstWkld: f.DstWorkloadHref,
								Process: f.ProcessName, FQDN: f.DstFQDN,
								SrcLabels: canonicalFlowLabels(f.SrcLabels), DstLabels: canonicalFlowLabels(f.DstLabels),
							}
							aggregateKey := AggregateKey{Connection: connectionKey, PolicyDecision: f.PolicyDecision, DraftDecision: f.DraftDecision}
							entry, exists := aggregatedFlows[aggregateKey]
							if !exists {
								entry.FirstSeen = f.FirstDetected
								entry.LastSeen = f.LastDetected
								entry.Raw = f
							}
							entry.TotalCount += f.NumConnections
							if entry.FirstSeen.IsZero() || (!f.FirstDetected.IsZero() && f.FirstDetected.Before(entry.FirstSeen)) {
								entry.FirstSeen = f.FirstDetected
							}
							if f.LastDetected.After(entry.LastSeen) {
								entry.LastSeen = f.LastDetected
							}
							aggregatedFlows[aggregateKey] = entry
							connectionSet[connectionKey] = struct{}{}

							protocol := fmt.Sprintf("%d", f.Proto)
							if name, ok := protoMap[f.Proto]; ok {
								protocol = name
							}
							monthKey := monthBucketFromTime(f.FirstDetected)
							summaryKey := fmt.Sprintf("%s|%s|%d", monthKey, protocol, f.DstPort)
							monthEntry := monthlySummaryMap[summaryKey]
							monthEntry.Month = monthKey
							monthEntry.Protocol = protocol
							monthEntry.Port = f.DstPort
							monthEntry.FlowCount += f.NumConnections
							monthlySummaryMap[summaryKey] = monthEntry

							if monthKey != "" {
								set := monthlyUniqueConnectionSet[summaryKey]
								if set == nil {
									set = make(map[FlowKey]struct{})
									monthlyUniqueConnectionSet[summaryKey] = set
								}
								set[connectionKey] = struct{}{}

								sourcePrimary := externalOrManagedLabel(f, true, primaryLabelKey)
								destinationPrimary := externalOrManagedLabel(f, false, primaryLabelKey)
								relationshipKey := strings.Join([]string{monthKey, sourcePrimary, destinationPrimary}, "\x1f")
								relationship := monthlyRelationshipMap[relationshipKey]
								relationship.Month = monthKey
								relationship.Source = sourcePrimary
								relationship.Destination = destinationPrimary
								relationship.FlowCount += f.NumConnections
								monthlyRelationshipMap[relationshipKey] = relationship
								if monthlyRelationshipSet[relationshipKey] == nil {
									monthlyRelationshipSet[relationshipKey] = map[FlowKey]struct{}{}
								}
								monthlyRelationshipSet[relationshipKey][connectionKey] = struct{}{}

								if !endpointHasClassification(f, false) {
									destinationName := endpointDisplayName(f, false)
									destinationKey := strings.Join([]string{monthKey, destinationName}, "\x1f")
									destination := monthlyExternalDestinationMap[destinationKey]
									destination.Month = monthKey
									destination.Destination = destinationName
									destination.FlowCount += f.NumConnections
									monthlyExternalDestinationMap[destinationKey] = destination
									if monthlyExternalDestinationSet[destinationKey] == nil {
										monthlyExternalDestinationSet[destinationKey] = map[FlowKey]struct{}{}
									}
									monthlyExternalDestinationSet[destinationKey][connectionKey] = struct{}{}
								}
							}
						}
						state.Mu.Lock()
						state.CompletedChunks++
						state.TotalConnections = len(connectionSet)
						state.Mu.Unlock()
						aggMu.Unlock()
						addLog(fmt.Sprintf("Chunk %d/%d (%s to %s): %d connections gathered", chunkIdx+1, len(chunks), chunks[chunkIdx].Start.Format("2006-01-02 15:04Z"), chunks[chunkIdx].End.Format("2006-01-02 15:04Z"), len(flows)))
					} else {
						addLog(fmt.Sprintf("Error chunk %d/%d (%s to %s): %v", chunkIdx+1, len(chunks), chunks[chunkIdx].Start.Format("2006-01-02 15:04Z"), chunks[chunkIdx].End.Format("2006-01-02 15:04Z"), err))
						extractionErrOnce.Do(func() {
							extractionErr = fmt.Errorf("chunk %d/%d failed after %d attempts: %w", chunkIdx+1, len(chunks), maxChunkAttempts, err)
							cancelRun()
						})
						return
					}
				}
			}
		}()
	}
	addLog(fmt.Sprintf("Extraction window: %s through %s (%d days) using %s chunks (%d total); traffic scope: %s.", rangeStart.Format("2006-01-02"), rangeEnd.Format("2006-01-02"), requestedDays, chunkLabel, len(chunks), normalizedTrafficScope(cfg.TrafficScope)))
	for i := 0; i < len(chunks); i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	if extractionErr != nil {
		addLog(fmt.Sprintf("Extraction aborted without writing a CSV: %v", extractionErr))
		markRunFinished("", false)
		return
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			addLog(fmt.Sprintf("Extraction exceeded the %s overall time limit; no CSV was written.", maxExtractionTime))
			markRunFinished("", false)
		} else {
			markRunFinished("", true)
		}
		return
	}

	finalPath, err := outputCSVPath(cfg.SavePath, cfg.FileName)
	if err != nil {
		addLog(fmt.Sprintf("Error: %v", err))
		markRunFinished("", false)
		return
	}

	f, closeRoot, err := createExclusiveRootedFile(finalPath, 0600)
	if err != nil {
		addLog(fmt.Sprintf("Error: %v", err))
		markRunFinished("", false)
		return
	}
	outputComplete := false
	defer func() {
		_ = f.Close()
		closeRoot()
		if !outputComplete {
			_ = removeRootedFile(finalPath)
		}
	}()

	w := csv.NewWriter(f)

	// Reordered Header
	header := []string{"First Detected", "Last Detected", "Source IP"}
	for _, k := range orderedKeys {
		header = append(header, "Src "+strings.ToUpper(k[:1])+k[1:])
	}
	header = append(header, "Destination IP")
	for _, k := range orderedKeys {
		header = append(header, "Dst "+strings.ToUpper(k[:1])+k[1:])
	}
	header = append(header, "FQDN", "Port", "Protocol", "Process Name", "Policy Decision", "Draft Policy Decision", "Traffic Scope", "Flows")
	if err := w.Write(header); err != nil {
		addLog(fmt.Sprintf("Error writing CSV header: %v", err))
		markRunFinished("", false)
		return
	}

	summaryMap := make(map[string]PortProtocolSummary)
	summaryUniqueConnectionSet := make(map[string]map[FlowKey]struct{})
	analyticsRecords := make([]AnalyticsRecord, 0, len(aggregatedFlows))
	for aggregateKey, entry := range aggregatedFlows {
		flow := entry.Raw
		protocol := fmt.Sprintf("%d", flow.Proto)
		if name, ok := protoMap[flow.Proto]; ok {
			protocol = name
		}
		srcL := make(map[string][]string)
		for _, l := range flow.SrcLabels {
			key := strings.ToLower(l.Key)
			srcL[key] = append(srcL[key], l.Value)
		}
		dstL := make(map[string][]string)
		for _, l := range flow.DstLabels {
			key := strings.ToLower(l.Key)
			dstL[key] = append(dstL[key], l.Value)
		}

		row := []string{
			entry.FirstSeen.Format("2006-01-02 15:04:05"),
			entry.LastSeen.Format("2006-01-02 15:04:05"),
			flow.SrcIP,
		}
		// Source Labels
		for _, k := range orderedKeys {
			row = append(row, strings.Join(srcL[strings.ToLower(k)], ", "))
		}
		// Destination IP
		row = append(row, flow.DstIP)
		// Destination Labels
		for _, k := range orderedKeys {
			row = append(row, strings.Join(dstL[strings.ToLower(k)], ", "))
		}
		// Final metadata
		row = append(row,
			flow.DstFQDN,
			fmt.Sprintf("%d", flow.DstPort),
			protocol,
			flow.ProcessName,
			flow.PolicyDecision,
			flow.DraftDecision,
			normalizedTrafficScope(cfg.TrafficScope),
			fmt.Sprintf("%d", entry.TotalCount),
		)
		for i := range row {
			row[i] = safeCSVCell(row[i])
		}
		if err := w.Write(row); err != nil {
			addLog(fmt.Sprintf("Error writing CSV row: %v", err))
			markRunFinished("", false)
			return
		}

		summaryKey := fmt.Sprintf("%s:%d", protocol, flow.DstPort)
		summaryEntry := summaryMap[summaryKey]
		summaryEntry.Port = flow.DstPort
		summaryEntry.Protocol = protocol
		summaryEntry.ProtocolNumber = flow.Proto
		summaryEntry.FlowCount += entry.TotalCount
		summaryMap[summaryKey] = summaryEntry
		if summaryUniqueConnectionSet[summaryKey] == nil {
			summaryUniqueConnectionSet[summaryKey] = map[FlowKey]struct{}{}
		}
		summaryUniqueConnectionSet[summaryKey][aggregateKey.Connection] = struct{}{}

		analyticsRecords = append(analyticsRecords, AnalyticsRecord{
			Identity:       flowIdentity(aggregateKey.Connection),
			SrcEnv:         externalOrManagedLabel(flow, true, primaryLabelKey),
			DstEnv:         externalOrManagedLabel(flow, false, primaryLabelKey),
			SrcApp:         externalOrManagedLabel(flow, true, secondaryLabelKey),
			DstApp:         externalOrManagedLabel(flow, false, secondaryLabelKey),
			SrcIP:          endpointDisplayName(flow, true),
			DstIP:          endpointDisplayName(flow, false),
			DstFQDN:        flow.DstFQDN,
			SrcManaged:     endpointHasClassification(flow, true),
			DstManaged:     endpointHasClassification(flow, false),
			Protocol:       protocol,
			Port:           flow.DstPort,
			FlowCount:      entry.TotalCount,
			FirstSeen:      entry.FirstSeen,
			LastSeen:       entry.LastSeen,
			PolicyDecision: flow.PolicyDecision,
			DraftDecision:  flow.DraftDecision,
			TrafficScope:   normalizedTrafficScope(cfg.TrafficScope),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		addLog(fmt.Sprintf("Error writing CSV: %v", err))
		markRunFinished("", false)
		return
	}
	if err := f.Sync(); err != nil {
		addLog(fmt.Sprintf("Error syncing CSV: %v", err))
		markRunFinished("", false)
		return
	}
	if err := f.Close(); err != nil {
		addLog(fmt.Sprintf("Error closing CSV: %v", err))
		markRunFinished("", false)
		return
	}

	summary := make([]PortProtocolSummary, 0, len(summaryMap))
	for key, item := range summaryMap {
		item.UniqueConnections = len(summaryUniqueConnectionSet[key])
		summary = append(summary, item)
	}
	sort.Slice(summary, func(i, j int) bool {
		if summary[i].FlowCount != summary[j].FlowCount {
			return summary[i].FlowCount > summary[j].FlowCount
		}
		if summary[i].Port != summary[j].Port {
			return summary[i].Port < summary[j].Port
		}
		return summary[i].Protocol < summary[j].Protocol
	})

	monthlySummaries := make([]MonthlyPortProtocolSummary, 0, len(monthlySummaryMap))
	for summaryKey, set := range monthlyUniqueConnectionSet {
		entry := monthlySummaryMap[summaryKey]
		entry.UniqueConnections = len(set)
		monthlySummaryMap[summaryKey] = entry
	}
	for aggregateKey, entry := range aggregatedFlows {
		flow := entry.Raw
		protocol := fmt.Sprintf("%d", flow.Proto)
		if name, ok := protoMap[flow.Proto]; ok {
			protocol = name
		}
		for _, activeMonth := range monthSpan(entry.FirstSeen, entry.LastSeen) {
			summaryKey := fmt.Sprintf("%s|%s|%d", activeMonth, protocol, flow.DstPort)
			monthEntry := monthlySummaryMap[summaryKey]
			monthEntry.Month = activeMonth
			monthEntry.Protocol = protocol
			monthEntry.Port = flow.DstPort
			monthlySummaryMap[summaryKey] = monthEntry
			if monthlyActiveConnectionSet[summaryKey] == nil {
				monthlyActiveConnectionSet[summaryKey] = map[FlowKey]struct{}{}
			}
			monthlyActiveConnectionSet[summaryKey][aggregateKey.Connection] = struct{}{}
		}
	}
	for key, item := range monthlySummaryMap {
		item.ActiveConnections = len(monthlyActiveConnectionSet[key])
		monthlySummaries = append(monthlySummaries, item)
	}
	sort.Slice(monthlySummaries, func(i, j int) bool {
		if monthlySummaries[i].Month != monthlySummaries[j].Month {
			return monthlySummaries[i].Month > monthlySummaries[j].Month
		}
		if monthlySummaries[i].FlowCount != monthlySummaries[j].FlowCount {
			return monthlySummaries[i].FlowCount > monthlySummaries[j].FlowCount
		}
		if monthlySummaries[i].Protocol != monthlySummaries[j].Protocol {
			return monthlySummaries[i].Protocol < monthlySummaries[j].Protocol
		}
		return monthlySummaries[i].Port < monthlySummaries[j].Port
	})

	analyticsRecords = mergeImportedAnalyticsRecords(analyticsRecords)
	insights := buildInsightsForDimensions(analyticsRecords, primaryLabelKey, secondaryLabelKey)
	insights.MonthlyPortProtocol = monthlySummaries
	for key, row := range monthlyRelationshipMap {
		row.UniqueConnections = len(monthlyRelationshipSet[key])
		insights.MonthlyRelationships = append(insights.MonthlyRelationships, row)
	}
	sort.Slice(insights.MonthlyRelationships, func(i, j int) bool {
		if insights.MonthlyRelationships[i].Month != insights.MonthlyRelationships[j].Month {
			return insights.MonthlyRelationships[i].Month < insights.MonthlyRelationships[j].Month
		}
		if insights.MonthlyRelationships[i].FlowCount != insights.MonthlyRelationships[j].FlowCount {
			return insights.MonthlyRelationships[i].FlowCount > insights.MonthlyRelationships[j].FlowCount
		}
		return insights.MonthlyRelationships[i].Source+insights.MonthlyRelationships[i].Destination < insights.MonthlyRelationships[j].Source+insights.MonthlyRelationships[j].Destination
	})
	for key, row := range monthlyExternalDestinationMap {
		row.UniqueConnections = len(monthlyExternalDestinationSet[key])
		insights.MonthlyExternalDestinations = append(insights.MonthlyExternalDestinations, row)
	}
	sort.Slice(insights.MonthlyExternalDestinations, func(i, j int) bool {
		if insights.MonthlyExternalDestinations[i].Month != insights.MonthlyExternalDestinations[j].Month {
			return insights.MonthlyExternalDestinations[i].Month < insights.MonthlyExternalDestinations[j].Month
		}
		if insights.MonthlyExternalDestinations[i].FlowCount != insights.MonthlyExternalDestinations[j].FlowCount {
			return insights.MonthlyExternalDestinations[i].FlowCount > insights.MonthlyExternalDestinations[j].FlowCount
		}
		return insights.MonthlyExternalDestinations[i].Destination < insights.MonthlyExternalDestinations[j].Destination
	})
	coverageEnd := rangeEnd.Add(24*time.Hour - time.Second)
	coverage := normalizeCoverage(DatasetCoverage{Source: "live_extraction", TrafficScope: normalizedTrafficScope(cfg.TrafficScope), Files: []DatasetFileCoverage{{
		Name: filepath.Base(finalPath), Rows: len(aggregatedFlows), FirstDetected: rangeStart, LastDetected: coverageEnd,
		Months: monthSpan(rangeStart, coverageEnd),
	}}})
	if info, err := statRootedFile(finalPath); err == nil {
		coverage.Files[0].Size = info.Size()
	}

	state.Mu.Lock()
	state.LastSummary = summary
	state.LastInsights = insights
	state.DatasetCoverage = coverage
	state.TrafficScope = normalizedTrafficScope(cfg.TrafficScope)
	state.Mu.Unlock()

	outputComplete = true
	addLog(fmt.Sprintf("SUCCESS: Final data saved to %s", finalPath))
	markRunFinished(finalPath, false)
}
