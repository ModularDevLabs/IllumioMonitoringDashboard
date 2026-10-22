package main

import "testing"

func TestTamperingWorkloadIdentityIgnoresUnrelatedNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event map[string]interface{}
		want  string
	}{
		{"created-by-agent", map[string]interface{}{
			"created_by": map[string]interface{}{"user": map[string]interface{}{"name": "admin"}, "agent": map[string]interface{}{"name": "ven-a"}},
			"label":      map[string]interface{}{"name": "Production"},
		}, "ven-a"},
		{"notification", map[string]interface{}{
			"notifications": []interface{}{map[string]interface{}{"info": map[string]interface{}{"workload": map[string]interface{}{"hostname": "ven-b"}}}},
			"service":       map[string]interface{}{"name": "ssh"},
		}, "ven-b"},
		{"unrelated-only", map[string]interface{}{"created_by": map[string]interface{}{"user": map[string]interface{}{"name": "admin"}}}, ""},
		{"legacy-flat", map[string]interface{}{"hostname": "legacy"}, "legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 100; i++ {
				if got := extractEventWorkloadName(tc.event); got != tc.want {
					t.Fatalf("workload = %q, want %q", got, tc.want)
				}
			}
		})
	}
}
