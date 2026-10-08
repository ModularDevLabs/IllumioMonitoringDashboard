package extractor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	maxDeliveryArtifactSize = 64 << 20
	maxInlineArtifactSize   = 4 << 20
	deliveryTimeout         = 2 * time.Minute
	deliveryAttempts        = 3
)

type deliveryMessage struct {
	RunID                   string
	Title                   string
	Text                    string
	ArtifactPath            string
	AdditionalArtifactPaths []string
	Failed                  bool
	Metrics                 RunMetrics
}

func (manager *AutomationManager) saveDestination(destination DeliveryDestination) (DeliveryDestination, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := time.Now().UTC()
	if destination.ID == "" {
		destination.ID = newAutomationID("dst")
		destination.CreatedAt = now
	} else {
		existing, ok := manager.data.Destinations[destination.ID]
		if !ok {
			return DeliveryDestination{}, fmt.Errorf("destination not found")
		}
		destination.CreatedAt = existing.CreatedAt
		if destination.Type == existing.Type {
			preserveDestinationSecrets(&destination, existing)
		}
	}
	if err := validateDestination(&destination); err != nil {
		return DeliveryDestination{}, err
	}
	for id, existing := range manager.data.Destinations {
		if id != destination.ID && strings.EqualFold(existing.Name, destination.Name) {
			return DeliveryDestination{}, fmt.Errorf("a destination named %q already exists", destination.Name)
		}
	}
	destination.UpdatedAt = now
	previous, existed := manager.data.Destinations[destination.ID]
	manager.data.Destinations[destination.ID] = destination
	if err := manager.saveLocked(); err != nil {
		if existed {
			manager.data.Destinations[destination.ID] = previous
		} else {
			delete(manager.data.Destinations, destination.ID)
		}
		return DeliveryDestination{}, err
	}
	return destination, nil
}

func preserveDestinationSecrets(destination *DeliveryDestination, existing DeliveryDestination) {
	if strings.TrimSpace(destination.EndpointURL) == "" {
		destination.EndpointURL = existing.EndpointURL
	}
	if strings.TrimSpace(destination.Token) == "" {
		destination.Token = existing.Token
	}
	if destination.SMTPPassword == "" {
		destination.SMTPPassword = existing.SMTPPassword
	}
	if destination.SFTPPassword == "" {
		destination.SFTPPassword = existing.SFTPPassword
	}
	if len(destination.Headers) == 0 {
		destination.Headers = existing.Headers
	}
}

func (manager *AutomationManager) deleteDestination(id string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if _, ok := manager.data.Destinations[id]; !ok {
		return fmt.Errorf("destination not found")
	}
	for _, template := range manager.data.Templates {
		for _, destinationID := range template.DeliveryDestination {
			if destinationID == id {
				return fmt.Errorf("destination is used by template %q", template.Name)
			}
		}
	}
	destination := manager.data.Destinations[id]
	delete(manager.data.Destinations, id)
	if err := manager.saveLocked(); err != nil {
		manager.data.Destinations[id] = destination
		return err
	}
	return nil
}

func validateDestination(destination *DeliveryDestination) error {
	destination.Name = strings.TrimSpace(destination.Name)
	destination.Type = strings.ToLower(strings.TrimSpace(destination.Type))
	if destination.Name == "" || len(destination.Name) > 120 {
		return fmt.Errorf("destination name is required and must be 120 characters or fewer")
	}
	if strings.ContainsAny(destination.Name, "\r\n") {
		return fmt.Errorf("destination name must be a single line")
	}
	switch destination.Type {
	case "generic_webhook", "slack_webhook", "teams_workflow":
		if strings.TrimSpace(destination.EndpointURL) == "" {
			return fmt.Errorf("webhook URL is required")
		}
		parsed, err := validateOutboundURLSyntax(destination.EndpointURL, destination.AllowPrivateNetwork)
		if err != nil {
			return err
		}
		if destination.Type == "slack_webhook" {
			host := strings.ToLower(parsed.Hostname())
			if host != "hooks.slack.com" && host != "hooks.slack-gov.com" {
				return fmt.Errorf("Slack webhook URL must use hooks.slack.com or hooks.slack-gov.com")
			}
		}
		if destination.WebhookMode == "" {
			destination.WebhookMode = "notification"
		}
		allowedModes := map[string]bool{"notification": true}
		if destination.Type == "generic_webhook" {
			allowedModes["base64_file"] = true
			allowedModes["multipart"] = true
		}
		if destination.Type == "teams_workflow" {
			allowedModes["base64_file"] = true
		}
		if !allowedModes[destination.WebhookMode] {
			return fmt.Errorf("unsupported webhook delivery mode %q", destination.WebhookMode)
		}
		for name, value := range destination.Headers {
			if strings.TrimSpace(name) == "" || strings.ContainsAny(name+value, "\r\n") {
				return fmt.Errorf("webhook headers must use non-empty single-line names and values")
			}
		}
	case "slack_api":
		if strings.TrimSpace(destination.Token) == "" || strings.TrimSpace(destination.ChannelID) == "" {
			return fmt.Errorf("Slack bot token and channel ID are required")
		}
		if strings.ContainsAny(destination.ChannelID, "\r\n") {
			return fmt.Errorf("Slack channel ID must be a single line")
		}
	case "email":
		if destination.SMTPHost == "" || destination.SMTPFrom == "" || len(destination.SMTPTo) == 0 {
			return fmt.Errorf("SMTP host, from address, and at least one recipient are required")
		}
		if destination.SMTPPort == 0 {
			destination.SMTPPort = 587
		}
		if destination.SMTPPort < 1 || destination.SMTPPort > 65535 {
			return fmt.Errorf("SMTP port must be from 1 through 65535")
		}
		if strings.ContainsAny(destination.SMTPHost+destination.SMTPUsername, "\r\n") {
			return fmt.Errorf("SMTP host and username must be single-line values")
		}
		if destination.SMTPUsername != "" && !destination.SMTPUseTLS {
			return fmt.Errorf("SMTP authentication requires TLS")
		}
		if _, err := mail.ParseAddress(destination.SMTPFrom); err != nil {
			return fmt.Errorf("invalid SMTP from address: %w", err)
		}
		for _, recipient := range destination.SMTPTo {
			if _, err := mail.ParseAddress(recipient); err != nil {
				return fmt.Errorf("invalid SMTP recipient %q", recipient)
			}
		}
	case "shared_folder":
		if !filepath.IsAbs(destination.FolderPath) {
			return fmt.Errorf("shared-folder destination must be an absolute path")
		}
		root, err := openExistingRoot(destination.FolderPath)
		if err != nil {
			return fmt.Errorf("shared-folder destination must already exist: %w", err)
		}
		_ = root.Close()
	case "sftp":
		if destination.SFTPHost == "" || destination.SFTPUsername == "" || destination.SFTPRemotePath == "" || destination.SFTPHostKey == "" {
			return fmt.Errorf("SFTP host, username, remote path, and pinned host public key are required")
		}
		if destination.SFTPPort == 0 {
			destination.SFTPPort = 22
		}
		if destination.SFTPPort < 1 || destination.SFTPPort > 65535 {
			return fmt.Errorf("SFTP port must be from 1 through 65535")
		}
		if strings.ContainsAny(destination.SFTPHost+destination.SFTPUsername+destination.SFTPRemotePath+destination.SFTPHostKey, "\r\n") {
			return fmt.Errorf("SFTP connection fields must be single-line values")
		}
		if destination.SFTPPassword == "" && destination.SFTPPrivateKeyPath == "" {
			return fmt.Errorf("SFTP password or private-key path is required")
		}
		if destination.SFTPPrivateKeyPath != "" && !filepath.IsAbs(destination.SFTPPrivateKeyPath) {
			return fmt.Errorf("SFTP private-key path must be absolute")
		}
		if destination.SFTPPrivateKeyPath != "" {
			info, err := statRootedFile(destination.SFTPPrivateKeyPath)
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("SFTP private-key path must identify a regular file")
			}
		}
		remotePath, err := normalizeSFTPRemoteDirectory(destination.SFTPRemotePath)
		if err != nil {
			return err
		}
		destination.SFTPRemotePath = remotePath
	default:
		return fmt.Errorf("destination type must be generic_webhook, slack_webhook, slack_api, teams_workflow, email, shared_folder, or sftp")
	}
	return nil
}

func validateOutboundURLSyntax(raw string, allowPrivate bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("invalid webhook URL")
	}
	if parsed.Scheme != "https" && !(allowPrivate && parsed.Scheme == "http") {
		return nil, fmt.Errorf("webhook URLs must use HTTPS; HTTP is allowed only with explicit private-network access")
	}
	if parsed.Fragment != "" {
		return nil, fmt.Errorf("webhook URL must not contain a fragment")
	}
	return parsed, nil
}

func validateOutboundHost(ctx context.Context, parsed *url.URL, allowPrivate bool) error {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil {
		return fmt.Errorf("resolve webhook host: %w", err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("webhook host resolved to no addresses")
	}
	if allowPrivate {
		return nil
	}
	for _, address := range addresses {
		ip := address.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("webhook host resolves to a private or special-purpose address; enable private-network access only when intentional")
		}
	}
	return nil
}

func outboundHTTPClient(destination DeliveryDestination) (*http.Client, error) {
	parsed, err := validateOutboundURLSyntax(destination.EndpointURL, destination.AllowPrivateNetwork)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := validateOutboundHost(ctx, parsed, destination.AllowPrivateNetwork); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Webhook hosts are resolved and dialed directly so a configured proxy cannot
	// bypass the private-address checks by resolving the target independently.
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, item := range addresses {
			ip := item.IP
			if !destination.AllowPrivateNetwork && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()) {
				lastErr = fmt.Errorf("outbound destination resolved to a private or special-purpose address")
				continue
			}
			connection, err := (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return connection, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("outbound destination resolved to no usable addresses")
		}
		return nil, lastErr
	}
	client := &http.Client{Timeout: deliveryTimeout, Transport: transport}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many webhook redirects")
		}
		redirected, err := validateOutboundURLSyntax(req.URL.String(), destination.AllowPrivateNetwork)
		if err != nil {
			return err
		}
		redirectCtx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
		defer cancel()
		return validateOutboundHost(redirectCtx, redirected, destination.AllowPrivateNetwork)
	}
	return client, nil
}

func (manager *AutomationManager) deliverCompletedRun(ctx context.Context, runID string, template ReportTemplate, artifactPath string, additionalArtifactPaths []string, metrics RunMetrics) {
	manager.mu.Lock()
	index := manager.runIndexLocked(runID)
	trigger := "schedule"
	if index >= 0 {
		trigger = manager.data.Runs[index].Trigger
	}
	manager.mu.Unlock()
	shouldDeliver, reason := shouldDeliverRun(template, trigger, metrics)
	if !shouldDeliver {
		manager.mu.Lock()
		if index := manager.runIndexLocked(runID); index >= 0 {
			manager.data.Runs[index].DeliverySkipped = reason
			_ = manager.saveLocked()
		}
		manager.mu.Unlock()
		return
	}

	flowDescription := "blocked flows"
	if normalizedTrafficScope(template.TrafficScope) == trafficScopeAll {
		flowDescription = "traffic flows across all policy decisions"
	}
	message := deliveryMessage{
		RunID: runID, Title: template.Name + " completed",
		Text: fmt.Sprintf("%s completed with %d %s across %d unique connections. External/unmanaged flows: %d. New relationships: %d. New services: %d.",
			template.Name, metrics.TotalFlows, flowDescription, metrics.UniqueConnections, metrics.ExternalFlows, len(metrics.NewRelationships), len(metrics.NewServices)),
		ArtifactPath: artifactPath, AdditionalArtifactPaths: additionalArtifactPaths, Metrics: metrics,
	}
	manager.deliverToTemplateDestinations(ctx, runID, template, message)
}

func (manager *AutomationManager) deliverFailedRun(ctx context.Context, runID string, template ReportTemplate, runErr error) {
	message := deliveryMessage{
		RunID: runID, Title: template.Name + " failed", Text: fmt.Sprintf("Scheduled report %s failed: %v", template.Name, runErr), Failed: true,
	}
	manager.deliverToTemplateDestinations(ctx, runID, template, message)
}

func (manager *AutomationManager) deliverToTemplateDestinations(parent context.Context, runID string, template ReportTemplate, message deliveryMessage) {
	manager.mu.Lock()
	destinations := make([]DeliveryDestination, 0, len(template.DeliveryDestination))
	for _, id := range template.DeliveryDestination {
		if destination, ok := manager.data.Destinations[id]; ok && destination.Enabled {
			destinations = append(destinations, destination)
		}
	}
	manager.mu.Unlock()

	results := make([]DeliveryResult, 0, len(destinations))
	for _, destination := range destinations {
		result := DeliveryResult{DestinationID: destination.ID, DestinationName: destination.Name, AttemptedAt: time.Now().UTC()}
		err := deliverMessageArtifacts(parent, destination, message)
		result.Success = err == nil
		if err != nil {
			result.Message = redactDeliveryError(err.Error(), destination)
		} else {
			result.Message = "delivered"
		}
		results = append(results, result)
	}

	manager.mu.Lock()
	if index := manager.runIndexLocked(runID); index >= 0 {
		manager.data.Runs[index].DeliveryResults = append(manager.data.Runs[index].DeliveryResults, results...)
		_ = manager.saveLocked()
	}
	manager.mu.Unlock()
}

func deliverMessageArtifacts(parent context.Context, destination DeliveryDestination, message deliveryMessage) error {
	paths := make([]string, 0, 1+len(message.AdditionalArtifactPaths))
	if message.ArtifactPath != "" {
		paths = append(paths, message.ArtifactPath)
	}
	paths = append(paths, message.AdditionalArtifactPaths...)
	if len(paths) == 0 {
		return deliverMessageWithRetries(parent, destination, message)
	}
	fileCapable := destination.Type == "slack_api" || destination.Type == "email" || destination.Type == "shared_folder" || destination.Type == "sftp" ||
		((destination.Type == "generic_webhook" || destination.Type == "teams_workflow") && (destination.WebhookMode == "multipart" || destination.WebhookMode == "base64_file"))
	if !fileCapable {
		names := make([]string, 0, len(paths))
		for _, path := range paths {
			names = append(names, filepath.Base(path))
		}
		message.Text += " Generated artifacts: " + strings.Join(names, ", ") + "."
		message.AdditionalArtifactPaths = nil
		return deliverMessageWithRetries(parent, destination, message)
	}
	for index, path := range paths {
		artifactMessage := message
		artifactMessage.ArtifactPath = path
		artifactMessage.AdditionalArtifactPaths = nil
		if index > 0 {
			artifactMessage.Title = message.Title + " — " + strings.ToUpper(strings.TrimPrefix(filepath.Ext(path), ".")) + " report"
			artifactMessage.Text = message.Text + " Artifact: " + filepath.Base(path) + "."
		}
		if err := deliverMessageWithRetries(parent, destination, artifactMessage); err != nil {
			return fmt.Errorf("deliver %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func deliverMessageWithRetries(parent context.Context, destination DeliveryDestination, message deliveryMessage) error {
	var err error
	for attempt := 1; attempt <= deliveryAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(parent, deliveryTimeout)
		err = deliverMessage(ctx, destination, message)
		cancel()
		if err == nil {
			return nil
		}
		if attempt < deliveryAttempts {
			select {
			case <-parent.Done():
				return parent.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return err
}

func redactDeliveryError(message string, destination DeliveryDestination) string {
	secrets := []string{destination.EndpointURL, destination.Token, destination.SMTPPassword, destination.SFTPPassword}
	for _, value := range destination.Headers {
		secrets = append(secrets, value)
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}

func destinationError(err error, destination DeliveryDestination) error {
	if err == nil {
		return nil
	}
	return errors.New(redactDeliveryError(err.Error(), destination))
}

func deliverMessage(ctx context.Context, destination DeliveryDestination, message deliveryMessage) error {
	switch destination.Type {
	case "generic_webhook":
		return deliverGenericWebhook(ctx, destination, message)
	case "slack_webhook":
		return deliverSlackWebhook(ctx, destination, message)
	case "slack_api":
		return deliverSlackFile(ctx, destination, message)
	case "teams_workflow":
		return deliverTeamsWorkflow(ctx, destination, message)
	case "email":
		return deliverEmail(ctx, destination, message)
	case "shared_folder":
		return deliverSharedFolder(destination, message)
	case "sftp":
		return deliverSFTP(ctx, destination, message)
	default:
		return fmt.Errorf("unsupported destination type %q", destination.Type)
	}
}

func artifactData(message deliveryMessage) ([]byte, string, error) {
	if message.ArtifactPath == "" {
		return nil, "", nil
	}
	info, err := statRootedFile(message.ArtifactPath)
	if err != nil {
		return nil, "", fmt.Errorf("read report artifact: %w", err)
	}
	if info.Size() > maxDeliveryArtifactSize {
		return nil, "", fmt.Errorf("report artifact exceeds the %d MiB delivery limit", maxDeliveryArtifactSize>>20)
	}
	data, err := readRootedFile(message.ArtifactPath)
	if err != nil {
		return nil, "", err
	}
	return data, filepath.Base(message.ArtifactPath), nil
}

func artifactContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

func webhookPayload(message deliveryMessage, includeFile bool) (map[string]any, error) {
	payload := map[string]any{
		"event": "report.completed", "run_id": message.RunID, "title": message.Title, "text": message.Text,
		"failed": message.Failed, "metrics": message.Metrics,
	}
	if message.Failed {
		payload["event"] = "report.failed"
	}
	if message.ArtifactPath != "" {
		payload["file_name"] = filepath.Base(message.ArtifactPath)
	}
	if includeFile && message.ArtifactPath != "" {
		data, name, err := artifactData(message)
		if err != nil {
			return nil, err
		}
		if len(data) > maxInlineArtifactSize {
			return nil, fmt.Errorf("report artifact exceeds the %d MiB inline base64 limit; use multipart, Slack file upload, email, shared-folder, or SFTP delivery", maxInlineArtifactSize>>20)
		}
		hash := sha256.Sum256(data)
		payload["file_name"] = name
		payload["file_base64"] = base64.StdEncoding.EncodeToString(data)
		payload["file_sha256"] = hex.EncodeToString(hash[:])
	}
	return payload, nil
}

func deliverGenericWebhook(ctx context.Context, destination DeliveryDestination, message deliveryMessage) error {
	client, err := outboundHTTPClient(destination)
	if err != nil {
		return err
	}
	var body io.Reader
	contentType := "application/json"
	if destination.WebhookMode == "multipart" && message.ArtifactPath != "" {
		data, name, err := artifactData(message)
		if err != nil {
			return err
		}
		buffer := &bytes.Buffer{}
		writer := multipart.NewWriter(buffer)
		payload, _ := webhookPayload(message, false)
		metadata, _ := json.Marshal(payload)
		_ = writer.WriteField("metadata", string(metadata))
		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, name))
		partHeader.Set("Content-Type", artifactContentType(name))
		part, err := writer.CreatePart(partHeader)
		if err != nil {
			return err
		}
		if _, err := part.Write(data); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		contentType = writer.FormDataContentType()
		body = buffer
	} else {
		payload, err := webhookPayload(message, destination.WebhookMode == "base64_file")
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.EndpointURL, body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-ITT-Run-ID", message.RunID)
	for name, value := range destination.Headers {
		request.Header.Set(name, value)
	}
	return doWebhookRequest(client, request)
}

func deliverSlackWebhook(ctx context.Context, destination DeliveryDestination, message deliveryMessage) error {
	client, err := outboundHTTPClient(destination)
	if err != nil {
		return err
	}
	payload := map[string]any{"text": "*" + message.Title + "*\n" + message.Text}
	encoded, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.EndpointURL, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return doWebhookRequest(client, request)
}

func deliverTeamsWorkflow(ctx context.Context, destination DeliveryDestination, message deliveryMessage) error {
	client, err := outboundHTTPClient(destination)
	if err != nil {
		return err
	}
	fileName := "None"
	if message.ArtifactPath != "" {
		fileName = filepath.Base(message.ArtifactPath)
	}
	card := map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil,
			"content": map[string]any{
				"$schema": "https://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4",
				"body": []any{
					map[string]any{"type": "TextBlock", "size": "Large", "weight": "Bolder", "text": message.Title},
					map[string]any{"type": "TextBlock", "wrap": true, "text": message.Text},
					map[string]any{"type": "FactSet", "facts": []any{
						map[string]any{"title": "Run", "value": message.RunID},
						map[string]any{"title": "File", "value": fileName},
					}},
				},
			},
		}},
	}
	if destination.WebhookMode == "base64_file" && message.ArtifactPath != "" {
		data, name, err := artifactData(message)
		if err != nil {
			return err
		}
		if len(data) > maxInlineArtifactSize {
			return fmt.Errorf("report artifact exceeds the %d MiB Teams inline limit", maxInlineArtifactSize>>20)
		}
		card["file_name"] = name
		card["file_base64"] = base64.StdEncoding.EncodeToString(data)
		hash := sha256.Sum256(data)
		card["file_sha256"] = hex.EncodeToString(hash[:])
	}
	encoded, _ := json.Marshal(card)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.EndpointURL, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-ITT-Run-ID", message.RunID)
	return doWebhookRequest(client, request)
}

func doWebhookRequest(client *http.Client, request *http.Request) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("delivery returned HTTP %d", response.StatusCode)
	}
	return nil
}

func slackAPIRequest(ctx context.Context, token, method string, values url.Values) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/"+method, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: deliveryTimeout}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 4<<20)
	var payload map[string]any
	if err := json.NewDecoder(limited).Decode(&payload); err != nil {
		return nil, err
	}
	if ok, _ := payload["ok"].(bool); !ok {
		return nil, fmt.Errorf("Slack API %s failed: %v", method, payload["error"])
	}
	return payload, nil
}

func deliverSlackFile(ctx context.Context, destination DeliveryDestination, message deliveryMessage) error {
	if message.ArtifactPath == "" {
		_, err := slackAPIRequest(ctx, destination.Token, "chat.postMessage", url.Values{"channel": {destination.ChannelID}, "text": {message.Title + "\n" + message.Text}})
		return err
	}
	data, name, err := artifactData(message)
	if err != nil {
		return err
	}
	payload, err := slackAPIRequest(ctx, destination.Token, "files.getUploadURLExternal", url.Values{
		"filename": {name}, "length": {strconv.Itoa(len(data))},
	})
	if err != nil {
		return err
	}
	uploadURL, _ := payload["upload_url"].(string)
	fileID, _ := payload["file_id"].(string)
	if uploadURL == "" || fileID == "" {
		return fmt.Errorf("Slack upload initialization returned incomplete data")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	uploadClient, err := outboundHTTPClient(DeliveryDestination{EndpointURL: uploadURL})
	if err != nil {
		return fmt.Errorf("validate Slack upload URL: %w", err)
	}
	response, err := uploadClient.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Slack file upload returned HTTP %d", response.StatusCode)
	}
	filesJSON, _ := json.Marshal([]map[string]string{{"id": fileID, "title": name}})
	_, err = slackAPIRequest(ctx, destination.Token, "files.completeUploadExternal", url.Values{
		"files": {string(filesJSON)}, "channel_id": {destination.ChannelID}, "initial_comment": {message.Text},
	})
	return err
}

func buildEmailMessage(destination DeliveryDestination, message deliveryMessage) ([]byte, error) {
	from, err := mail.ParseAddress(destination.SMTPFrom)
	if err != nil {
		return nil, fmt.Errorf("invalid SMTP from address: %w", err)
	}
	recipients := make([]string, 0, len(destination.SMTPTo))
	for _, rawRecipient := range destination.SMTPTo {
		recipient, err := mail.ParseAddress(rawRecipient)
		if err != nil {
			return nil, fmt.Errorf("invalid SMTP recipient: %w", err)
		}
		recipients = append(recipients, recipient.String())
	}
	title, err := singleLineEmailHeader(message.Title)
	if err != nil {
		return nil, err
	}
	boundary := "itt-" + newAutomationID("mail")
	buffer := &bytes.Buffer{}
	fmt.Fprintf(buffer, "From: %s\r\n", from.String())
	fmt.Fprintf(buffer, "To: %s\r\n", strings.Join(recipients, ", "))
	fmt.Fprintf(buffer, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", title))
	fmt.Fprintf(buffer, "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(buffer, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary)
	bodyWriter := quotedprintable.NewWriter(buffer)
	if _, err := bodyWriter.Write([]byte(message.Text)); err != nil {
		return nil, err
	}
	if err := bodyWriter.Close(); err != nil {
		return nil, err
	}
	buffer.WriteString("\r\n")
	if message.ArtifactPath != "" {
		data, name, err := artifactData(message)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(buffer, "--%s\r\nContent-Type: %s\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n", boundary, artifactContentType(name), name)
		encoded := base64.StdEncoding.EncodeToString(data)
		for len(encoded) > 76 {
			fmt.Fprintf(buffer, "%s\r\n", encoded[:76])
			encoded = encoded[76:]
		}
		fmt.Fprintf(buffer, "%s\r\n", encoded)
	}
	fmt.Fprintf(buffer, "--%s--\r\n", boundary)
	return buffer.Bytes(), nil
}

func singleLineEmailHeader(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("email subject must be a non-empty single-line value")
	}
	for _, character := range value {
		if character < 0x20 && character != '\t' {
			return "", fmt.Errorf("email subject contains an unsupported control character")
		}
	}
	return value, nil
}

func deliverEmail(ctx context.Context, destination DeliveryDestination, message deliveryMessage) error {
	payload, err := buildEmailMessage(destination, message)
	if err != nil {
		return err
	}
	address := net.JoinHostPort(destination.SMTPHost, strconv.Itoa(destination.SMTPPort))
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	var connection net.Conn
	if destination.SMTPUseTLS && destination.SMTPPort == 465 {
		connection, err = tls.DialWithDialer(dialer, "tcp", address, &tls.Config{ServerName: destination.SMTPHost, MinVersion: tls.VersionTLS12})
	} else {
		connection, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer connection.Close()
	deadline := time.Now().Add(deliveryTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	client, err := smtp.NewClient(connection, destination.SMTPHost)
	if err != nil {
		return err
	}
	defer client.Close()
	if destination.SMTPUseTLS && destination.SMTPPort != 465 {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP server does not advertise STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: destination.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if destination.SMTPUsername != "" {
		if err := client.Auth(smtp.PlainAuth("", destination.SMTPUsername, destination.SMTPPassword, destination.SMTPHost)); err != nil {
			return err
		}
	}
	from, _ := mail.ParseAddress(destination.SMTPFrom)
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	for _, recipient := range destination.SMTPTo {
		parsed, _ := mail.ParseAddress(recipient)
		if err := client.Rcpt(parsed.Address); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	// buildEmailMessage parses every address, rejects control characters in
	// headers, RFC 2047-encodes the subject, quoted-printable-encodes the body,
	// and base64-encodes attachments before the SMTP DATA sink.
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func copyArtifactNoOverwrite(sourcePath, destinationFolder string) error {
	if sourcePath == "" {
		return fmt.Errorf("this destination requires a report artifact")
	}
	if !filepath.IsAbs(destinationFolder) {
		return fmt.Errorf("destination folder must be absolute")
	}
	source, closeSourceRoot, err := openRootedFile(sourcePath)
	if err != nil {
		return err
	}
	defer closeSourceRoot()
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("report artifact must be a regular file")
	}
	if info.Size() > maxDeliveryArtifactSize {
		return fmt.Errorf("report artifact exceeds the %d MiB delivery limit", maxDeliveryArtifactSize>>20)
	}
	destinationRoot, err := openExistingRoot(destinationFolder)
	if err != nil {
		return err
	}
	defer destinationRoot.Close()
	destinationName := filepath.Base(sourcePath)
	destination, err := destinationRoot.OpenFile(destinationName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		_ = destination.Close()
		if !complete {
			_ = destinationRoot.Remove(destinationName)
		}
	}()
	if _, err := io.Copy(destination, source); err != nil {
		return err
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func deliverSharedFolder(destination DeliveryDestination, message deliveryMessage) error {
	return copyArtifactNoOverwrite(message.ArtifactPath, destination.FolderPath)
}

func parsePinnedHostKey(raw string) (ssh.PublicKey, error) {
	value := strings.TrimSpace(raw)
	parts := strings.Fields(value)
	if len(parts) >= 3 && !strings.HasPrefix(parts[0], "ssh-") && !strings.HasPrefix(parts[0], "ecdsa-") {
		value = strings.Join(parts[1:], " ")
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("parse pinned SFTP host key: %w", err)
	}
	return key, nil
}

func sftpSSHConfig(destination DeliveryDestination) (*ssh.ClientConfig, error) {
	hostKey, err := parsePinnedHostKey(destination.SFTPHostKey)
	if err != nil {
		return nil, err
	}
	auth := []ssh.AuthMethod{}
	if destination.SFTPPassword != "" {
		auth = append(auth, ssh.Password(destination.SFTPPassword))
	}
	if destination.SFTPPrivateKeyPath != "" {
		keyData, err := readRootedFile(destination.SFTPPrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read SFTP private key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(keyData)
		if err != nil {
			return nil, fmt.Errorf("parse SFTP private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	return &ssh.ClientConfig{
		User: destination.SFTPUsername, Auth: auth, Timeout: 20 * time.Second,
		HostKeyCallback: ssh.FixedHostKey(hostKey),
	}, nil
}

func deliverSFTP(ctx context.Context, destination DeliveryDestination, message deliveryMessage) error {
	if message.ArtifactPath == "" {
		return fmt.Errorf("SFTP delivery requires a report artifact")
	}
	artifactInfo, err := statRootedFile(message.ArtifactPath)
	if err != nil {
		return err
	}
	if !artifactInfo.Mode().IsRegular() {
		return fmt.Errorf("report artifact must be a regular file")
	}
	if artifactInfo.Size() > maxDeliveryArtifactSize {
		return fmt.Errorf("report artifact exceeds the %d MiB delivery limit", maxDeliveryArtifactSize>>20)
	}
	client, cleanup, err := openSFTPClient(ctx, destination)
	if err != nil {
		return err
	}
	defer cleanup()

	remoteDirectory, err := normalizeSFTPRemoteDirectory(destination.SFTPRemotePath)
	if err != nil {
		return err
	}
	// The remote directory is normalized and traversal-free before it reaches
	// the SFTP server. lgtm[go/path-injection]
	if err := client.MkdirAll(remoteDirectory); err != nil {
		return err
	}
	remotePath := pathpkg.Join(remoteDirectory, filepath.Base(message.ArtifactPath))
	// remotePath is confined to the validated SFTP directory and a local base
	// filename. lgtm[go/path-injection]
	if _, err := client.Stat(remotePath); err == nil {
		return fmt.Errorf("remote artifact already exists: %s", remotePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		// Servers do not consistently wrap missing-path errors; creation with O_EXCL is authoritative below.
	}
	// The SFTP target cannot contain traversal after normalization.
	// lgtm[go/path-injection]
	remoteFile, err := client.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		_ = remoteFile.Close()
		if !complete {
			// remotePath passed the traversal-free SFTP normalization above.
			// lgtm[go/path-injection]
			_ = client.Remove(remotePath)
		}
	}()
	localFile, closeLocalRoot, err := openRootedFile(message.ArtifactPath)
	if err != nil {
		return err
	}
	defer closeLocalRoot()
	defer localFile.Close()
	if _, err := io.Copy(remoteFile, localFile); err != nil {
		return err
	}
	if err := remoteFile.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func normalizeSFTPRemoteDirectory(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", fmt.Errorf("SFTP remote path must be a non-empty POSIX path")
	}
	cleaned := pathpkg.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("SFTP remote path must not contain traversal")
	}
	return cleaned, nil
}

func openSFTPClient(ctx context.Context, destination DeliveryDestination) (*sftp.Client, func(), error) {
	config, err := sftpSSHConfig(destination)
	if err != nil {
		return nil, nil, err
	}
	address := net.JoinHostPort(destination.SFTPHost, strconv.Itoa(destination.SFTPPort))
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(deliveryTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		connection.Close()
		return nil, nil, err
	}
	sshConnection, channels, requests, err := ssh.NewClientConn(connection, address, config)
	if err != nil {
		connection.Close()
		return nil, nil, err
	}
	sshClient := ssh.NewClient(sshConnection, channels, requests)
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = client.Close()
		_ = sshClient.Close()
	}
	return client, cleanup, nil
}

func (manager *AutomationManager) testDestination(ctx context.Context, id string) error {
	manager.mu.Lock()
	destination, ok := manager.data.Destinations[id]
	manager.mu.Unlock()
	if !ok {
		return fmt.Errorf("destination not found")
	}
	message := deliveryMessage{
		RunID: newAutomationID("test"), Title: "Illumio Blocked Traffic Extractor test",
		Text: "This is a delivery test from the local Illumio Blocked Traffic Extractor.",
	}
	if destination.Type == "shared_folder" {
		testFile, err := os.CreateTemp("", "itt-delivery-test-*.txt")
		if err != nil {
			return err
		}
		testPath := testFile.Name()
		_, _ = testFile.WriteString(message.Text)
		_ = testFile.Close()
		defer os.Remove(testPath)
		message.ArtifactPath = testPath
		destinationRoot, rootErr := openExistingRoot(destination.FolderPath)
		if rootErr != nil {
			return destinationError(rootErr, destination)
		}
		defer destinationRoot.Close()
		defer destinationRoot.Remove(filepath.Base(testPath))
	}
	if destination.Type == "sftp" {
		client, cleanup, err := openSFTPClient(ctx, destination)
		if err != nil {
			return destinationError(err, destination)
		}
		defer cleanup()
		remotePath, pathErr := normalizeSFTPRemoteDirectory(destination.SFTPRemotePath)
		if pathErr != nil {
			return destinationError(pathErr, destination)
		}
		// remotePath is a normalized, traversal-free POSIX directory.
		// lgtm[go/path-injection]
		_, err = client.Stat(remotePath)
		return destinationError(err, destination)
	}
	if destination.Type == "slack_api" {
		_, err := slackAPIRequest(ctx, destination.Token, "auth.test", url.Values{})
		return destinationError(err, destination)
	}
	return destinationError(deliverMessage(ctx, destination, message), destination)
}

func sortedPublicDestinations(items map[string]DeliveryDestination) []PublicDeliveryDestination {
	result := make([]PublicDeliveryDestination, 0, len(items))
	for _, item := range items {
		result = append(result, item.public())
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name) })
	return result
}
