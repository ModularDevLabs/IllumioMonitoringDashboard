package extractor

import (
	"bytes"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type reportMonth struct {
	Month       string
	Flows       int
	Connections int
}

func reportMonthlyRollup(rows []MonthlyPortProtocolSummary) []reportMonth {
	byMonth := map[string]*reportMonth{}
	for _, row := range rows {
		if row.Month == "" {
			continue
		}
		entry := byMonth[row.Month]
		if entry == nil {
			entry = &reportMonth{Month: row.Month}
			byMonth[row.Month] = entry
		}
		entry.Flows += row.FlowCount
		entry.Connections += row.UniqueConnections
	}
	months := make([]reportMonth, 0, len(byMonth))
	for _, row := range byMonth {
		months = append(months, *row)
	}
	sort.Slice(months, func(i, j int) bool { return months[i].Month < months[j].Month })
	return months
}

func scheduledReportMetadata(template ReportTemplate) ReportMetadata {
	title := template.ReportTitle
	if title == "" {
		title = template.Name + " Executive Summary"
	}
	return ReportMetadata{Title: title, CustomerName: template.ReportCustomer, PreparedBy: template.ReportPreparedBy, Notes: template.ReportNotes}
}

func totalReportMetrics(summary []PortProtocolSummary, insights AnalyticsInsights) (int, int, int) {
	flows, connections, external := 0, 0, 0
	for _, row := range summary {
		flows += row.FlowCount
		connections += row.UniqueConnections
	}
	for _, row := range insights.TrafficCategories {
		if strings.Contains(row.Name, "External/Unmanaged") {
			external += row.FlowCount
		}
	}
	return flows, connections, external
}

func reportTrendSVG(months []reportMonth) string {
	if len(months) == 0 {
		return `<div class="empty">No monthly trend data is available.</div>`
	}
	const width, height, left, right, top, bottom = 960, 300, 70, 20, 25, 50
	plotWidth, plotHeight := width-left-right, height-top-bottom
	maxValue := 1
	for _, month := range months {
		if month.Flows > maxValue {
			maxValue = month.Flows
		}
	}
	xAt := func(index int) float64 {
		if len(months) == 1 {
			return float64(left + plotWidth/2)
		}
		return float64(left) + float64(plotWidth*index)/float64(len(months)-1)
	}
	var out strings.Builder
	fmt.Fprintf(&out, `<svg viewBox="0 0 %d %d" role="img" aria-label="Monthly blocked flow trend"><rect width="%d" height="%d" rx="16" fill="#fff"/>`, width, height, width, height)
	for step := 0; step <= 4; step++ {
		y := float64(top) + float64(plotHeight*step)/4
		value := maxValue * (4 - step) / 4
		fmt.Fprintf(&out, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" stroke="#eadfce"/><text x="%d" y="%.1f" text-anchor="end" font-size="11" fill="#6f7274">%s</text>`, left, width-right, y, y, left-8, y+4, html.EscapeString(strconv.Itoa(value)))
	}
	var path strings.Builder
	for index, month := range months {
		x := xAt(index)
		y := float64(top+plotHeight) - float64(month.Flows)*float64(plotHeight)/float64(maxValue)
		if index == 0 {
			fmt.Fprintf(&path, "M %.1f %.1f", x, y)
		} else {
			fmt.Fprintf(&path, " L %.1f %.1f", x, y)
		}
		fmt.Fprintf(&out, `<circle cx="%.1f" cy="%.1f" r="4" fill="#ff5500"><title>%s: %d flows</title></circle><text x="%.1f" y="%d" text-anchor="middle" font-size="11" fill="#6f7274">%s</text>`, x, y, html.EscapeString(month.Month), month.Flows, x, height-20, html.EscapeString(month.Month))
	}
	fmt.Fprintf(&out, `<path d="%s" fill="none" stroke="#ff5500" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"/></svg>`, path.String())
	return out.String()
}

func writeExclusiveFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func generateExecutiveHTML(path string, metadata ReportMetadata, summary []PortProtocolSummary, insights AnalyticsInsights, coverage DatasetCoverage) error {
	flows, connections, external := totalReportMetrics(summary, insights)
	months := reportMonthlyRollup(insights.MonthlyPortProtocol)
	services := append([]PortProtocolSummary(nil), summary...)
	sort.Slice(services, func(i, j int) bool { return services[i].FlowCount > services[j].FlowCount })
	if len(services) > 12 {
		services = services[:12]
	}
	relationships := append([]MatrixSummary(nil), insights.EnvMatrix...)
	if len(relationships) > 12 {
		relationships = relationships[:12]
	}
	var serviceRows, relationshipRows strings.Builder
	for _, row := range services {
		fmt.Fprintf(&serviceRows, "<tr><td>%s</td><td>%d</td><td>%s</td><td>%s</td></tr>", html.EscapeString(strings.ToUpper(row.Protocol)), row.Port, formatInteger(row.FlowCount), formatInteger(row.UniqueConnections))
	}
	for _, row := range relationships {
		fmt.Fprintf(&relationshipRows, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>", html.EscapeString(row.Source), html.EscapeString(row.Destination), formatInteger(row.FlowCount))
	}
	coverageText := "Coverage unavailable"
	if !coverage.FirstDetected.IsZero() && !coverage.LastDetected.IsZero() {
		coverageText = coverage.FirstDetected.Format("Jan 2, 2006") + " through " + coverage.LastDetected.Format("Jan 2, 2006")
	}
	document := fmt.Sprintf(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>
body{margin:0;background:#f5f2e9;color:#232b2e;font:14px Inter,Arial,sans-serif}main{max-width:1100px;margin:auto;padding:36px}.panel{background:#fff;border:1px solid #d9cdbb;border-radius:20px;padding:24px;margin:18px 0}.eyebrow{color:#d94700;text-transform:uppercase;letter-spacing:.18em;font-weight:700}.muted{color:#6f7274}.cards{display:grid;grid-template-columns:repeat(3,1fr);gap:16px}.card{background:#faf8f2;border:1px solid #eadfce;border-radius:16px;padding:18px}.value{color:#e84b00;font-size:30px;font-weight:800;margin-top:8px}h1{font-size:38px;margin:8px 0}h2{margin-top:0}table{width:100%%;border-collapse:collapse}th,td{text-align:left;border-bottom:1px solid #eadfce;padding:10px}th{font-size:11px;text-transform:uppercase;letter-spacing:.1em;color:#6f7274}.grid{display:grid;grid-template-columns:1fr 1fr;gap:18px}.notes{white-space:pre-wrap}@media(max-width:760px){.cards,.grid{grid-template-columns:1fr}main{padding:18px}}@media print{body{background:#fff}main{max-width:none}.panel{break-inside:avoid}}
</style></head><body><main><section class="panel"><div class="eyebrow">%s</div><h1>%s</h1><p class="muted">%s%s</p><p class="notes">%s</p></section><section class="cards"><div class="card"><div class="eyebrow">Blocked Flows</div><div class="value">%s</div></div><div class="card"><div class="eyebrow">Connections</div><div class="value">%s</div></div><div class="card"><div class="eyebrow">External / Unmanaged</div><div class="value">%s</div></div></section><section class="panel"><h2>Month-over-month blocked traffic</h2>%s</section><section class="grid"><div class="panel"><h2>Top services</h2><table><thead><tr><th>Protocol</th><th>Port</th><th>Flows</th><th>Connections</th></tr></thead><tbody>%s</tbody></table></div><div class="panel"><h2>Top relationships</h2><table><thead><tr><th>Source</th><th>Destination</th><th>Flows</th></tr></thead><tbody>%s</tbody></table></div></section><p class="muted">Generated %s by Illumio Blocked Traffic Extractor.</p></main></body></html>`, html.EscapeString(metadata.Title), html.EscapeString(metadata.CustomerName), html.EscapeString(metadata.Title), html.EscapeString(coverageText), preparedSuffix(metadata.PreparedBy), html.EscapeString(metadata.Notes), formatInteger(flows), formatInteger(connections), formatInteger(external), reportTrendSVG(months), serviceRows.String(), relationshipRows.String(), time.Now().Format(time.RFC1123))
	return writeExclusiveFile(path, []byte(document))
}

func preparedSuffix(value string) string {
	if value == "" {
		return ""
	}
	return " · Prepared by " + html.EscapeString(value)
}

func formatInteger(value int) string {
	raw := strconv.Itoa(value)
	for index := len(raw) - 3; index > 0; index -= 3 {
		raw = raw[:index] + "," + raw[index:]
	}
	return raw
}

func pdfEscape(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return ' '
		}
		return r
	}, value)
	return strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)").Replace(value)
}

func generateExecutivePDF(path string, metadata ReportMetadata, summary []PortProtocolSummary, insights AnalyticsInsights, coverage DatasetCoverage) error {
	flows, connections, external := totalReportMetrics(summary, insights)
	months := reportMonthlyRollup(insights.MonthlyPortProtocol)
	var content strings.Builder
	content.WriteString("0.13 0.17 0.18 rg\n")
	pdfText := func(x, y, size int, value string) {
		fmt.Fprintf(&content, "BT /F1 %d Tf %d %d Td (%s) Tj ET\n", size, x, y, pdfEscape(value))
	}
	pdfText(54, 750, 11, metadata.CustomerName)
	pdfText(54, 720, 24, metadata.Title)
	pdfText(54, 695, 10, "Generated "+time.Now().Format(time.RFC1123))
	if metadata.PreparedBy != "" {
		pdfText(54, 678, 10, "Prepared by "+metadata.PreparedBy)
	}
	pdfText(54, 635, 12, "TOTAL BLOCKED FLOWS")
	pdfText(54, 607, 22, formatInteger(flows))
	pdfText(235, 635, 12, "CONNECTIONS")
	pdfText(235, 607, 22, formatInteger(connections))
	pdfText(410, 635, 12, "EXTERNAL / UNMANAGED")
	pdfText(410, 607, 22, formatInteger(external))
	pdfText(54, 560, 15, "Monthly blocked traffic")
	start := 0
	if len(months) > 12 {
		start = len(months) - 12
	}
	visibleMonths := months[start:]
	if len(visibleMonths) > 0 {
		maxFlows := 1
		for _, month := range visibleMonths {
			if month.Flows > maxFlows {
				maxFlows = month.Flows
			}
		}
		content.WriteString("0.91 0.29 0 RG 2 w\n")
		for index, month := range visibleMonths {
			x := 64.0
			if len(visibleMonths) > 1 {
				x += 480.0 * float64(index) / float64(len(visibleMonths)-1)
			}
			y := 425.0 + 110.0*float64(month.Flows)/float64(maxFlows)
			if index == 0 {
				fmt.Fprintf(&content, "%.1f %.1f m\n", x, y)
			} else {
				fmt.Fprintf(&content, "%.1f %.1f l\n", x, y)
			}
		}
		content.WriteString("S\n0.13 0.17 0.18 rg\n")
	}
	y := 395
	for _, month := range visibleMonths {
		pdfText(66, y, 10, month.Month)
		pdfText(180, y, 10, formatInteger(month.Flows)+" flows")
		pdfText(340, y, 10, formatInteger(month.Connections)+" connections")
		y -= 15
	}
	if metadata.Notes != "" {
		notesY := y - 15
		pdfText(54, notesY, 15, "Report notes")
		for _, line := range strings.Split(metadata.Notes, "\n") {
			if len(line) > 90 {
				pdfText(54, notesY-22, 9, line[:90])
			} else {
				pdfText(54, notesY-22, 9, line)
			}
			break
		}
	}
	stream := content.String()
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for index := 1; index <= len(objects); index++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return writeExclusiveFile(path, pdf.Bytes())
}

func generateScheduledExecutiveArtifacts(csvPath string, template ReportTemplate, summary []PortProtocolSummary, insights AnalyticsInsights, coverage DatasetCoverage) ([]string, error) {
	base := strings.TrimSuffix(csvPath, filepath.Ext(csvPath))
	metadata := scheduledReportMetadata(template)
	paths := make([]string, 0, 2)
	if template.GenerateExecutiveHTML {
		path := base + "-executive.html"
		if err := generateExecutiveHTML(path, metadata, summary, insights, coverage); err != nil {
			return paths, fmt.Errorf("generate executive HTML: %w", err)
		}
		paths = append(paths, path)
	}
	if template.GenerateExecutivePDF {
		path := base + "-executive.pdf"
		if err := generateExecutivePDF(path, metadata, summary, insights, coverage); err != nil {
			for _, generatedPath := range paths {
				_ = os.Remove(generatedPath)
			}
			return paths, fmt.Errorf("generate executive PDF: %w", err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}
