package main

import (
	"strings"
	"testing"
)

func TestDashboardPagesLoadUnifiedProductShell(t *testing.T) {
	t.Parallel()

	for _, page := range []string{"index.html", "details.html", "settings.html", "executive.html", "report.html"} {
		content, err := templateFS.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		html := string(content)
		for _, asset := range []string{
			`<link rel="stylesheet" href="/static/product-shell.css">`,
			`<script src="/static/product-shell.js" defer></script>`,
		} {
			if !strings.Contains(html, asset) {
				t.Fatalf("%s does not load unified shell asset %q", page, asset)
			}
		}
	}
}
