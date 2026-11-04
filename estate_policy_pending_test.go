package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const estatePendingTestBase = "https://pce.invalid/api/v2/orgs/1"

func estatePendingTestRule(state, set, id string, port int) map[string]interface{} {
	return map[string]interface{}{
		"href":              "/orgs/1/sec_policy/" + state + "/rule_sets/" + set + "/sec_rules/" + id,
		"enabled":           true,
		"providers":         []interface{}{map[string]interface{}{"label": map[string]interface{}{"href": "/orgs/1/labels/1"}}},
		"consumers":         []interface{}{map[string]interface{}{"ip_list": map[string]interface{}{"href": "/orgs/1/sec_policy/" + state + "/ip_lists/2"}}},
		"ingress_services":  []interface{}{map[string]interface{}{"port": port, "proto": 6}},
		"resolve_labels_as": map[string]interface{}{"providers": []interface{}{"workloads"}, "consumers": []interface{}{"workloads"}},
	}
}

func estatePendingTestRuleset(state, id string, rules ...map[string]interface{}) map[string]interface{} {
	if rules == nil {
		rules = []map[string]interface{}{}
	}
	return map[string]interface{}{"href": "/orgs/1/sec_policy/" + state + "/rule_sets/" + id, "rules": rules}
}

func estatePendingTestPayload(operations map[string]string) map[string]interface{} {
	rows := []map[string]interface{}{}
	for id, operation := range operations {
		rows = append(rows, map[string]interface{}{"href": "/orgs/1/sec_policy/draft/rule_sets/" + id, "update_type": operation})
	}
	return map[string]interface{}{"rule_sets": rows, "services": []interface{}{}}
}

func estatePendingTestFetch(t *testing.T, active, draft []map[string]interface{}) func(string) ([]map[string]interface{}, error) {
	t.Helper()
	return func(endpoint string) ([]map[string]interface{}, error) {
		switch endpoint {
		case estatePendingTestBase + "/sec_policy/active/rule_sets":
			return active, nil
		case estatePendingTestBase + "/sec_policy/draft/rule_sets":
			return draft, nil
		default:
			t.Errorf("unnecessary rule detail request: %s", endpoint)
			return nil, errors.New("unexpected request")
		}
	}
}

func TestEstatePendingCountsAddedModifiedDeletedNotNetInventory(t *testing.T) {
	active := []map[string]interface{}{
		estatePendingTestRuleset("active", "1", estatePendingTestRule("active", "1", "1", 443), estatePendingTestRule("active", "1", "2", 80), estatePendingTestRule("active", "1", "3", 22)),
		estatePendingTestRuleset("active", "3", estatePendingTestRule("active", "3", "4", 53)),
	}
	draft := []map[string]interface{}{
		estatePendingTestRuleset("draft", "1", estatePendingTestRule("draft", "1", "1", 443), estatePendingTestRule("draft", "1", "2", 8080), estatePendingTestRule("draft", "1", "5", 9300)),
		estatePendingTestRuleset("draft", "2", estatePendingTestRule("draft", "2", "6", 445)),
	}
	got := estateCountPendingPolicy(estatePendingTestBase, estatePendingTestPayload(map[string]string{"1": "update", "2": "create", "3": "delete"}), true, estatePendingTestFetch(t, active, draft))
	wantSets := EstatePendingPolicyDelta{Added: 1, Modified: 1, Deleted: 1, Total: 3, Complete: true}
	wantRules := EstatePendingPolicyDelta{Added: 2, Modified: 1, Deleted: 2, Total: 5, Complete: true}
	if got.Rulesets != wantSets || got.Rules != wantRules || len(got.Warnings) != 0 {
		t.Fatalf("equal inventories hide five pending rule changes: %+v", got)
	}
}

func TestEstatePendingCountsMetadataOnlyRulesetAndReferences(t *testing.T) {
	before := estatePendingTestRule("active", "1", "1", 443)
	after := estatePendingTestRule("draft", "1", "1", 443)
	before["created_at"], before["updated_at"], before["caps"] = "earlier", "earlier", []string{"read"}
	after["created_at"], after["updated_at"], after["caps"] = "later", "later", []string{"read", "write"}
	after["consumers"] = []interface{}{map[string]interface{}{"ip_list": map[string]interface{}{"href": estatePendingTestBase + "/sec_policy/draft/ip_lists/2", "name": "Expanded metadata"}}}
	before["ingress_services"] = []interface{}{map[string]interface{}{"port": 443, "proto": 6}, map[string]interface{}{"port": 53, "proto": 17}}
	after["ingress_services"] = []interface{}{map[string]interface{}{"port": 53, "proto": 17}, map[string]interface{}{"port": 443, "proto": 6}}
	active := []map[string]interface{}{estatePendingTestRuleset("active", "1", before)}
	draft := []map[string]interface{}{estatePendingTestRuleset("draft", "1", after)}
	draft[0]["name"] = "Ruleset renamed; rules unchanged"
	got := estateCountPendingPolicy(estatePendingTestBase, estatePendingTestPayload(map[string]string{"1": "update"}), true, estatePendingTestFetch(t, active, draft))
	if !got.Rules.Complete || got.Rules.Total != 0 || got.Rulesets.Modified != 1 {
		t.Fatalf("response metadata or ruleset-only edit incorrectly changed a rule: %+v", got)
	}
	after["update_type"] = "update"
	got = estateCountPendingPolicy(estatePendingTestBase, estatePendingTestPayload(map[string]string{"1": "update"}), true, estatePendingTestFetch(t, active, draft))
	if !got.Rules.Complete || got.Rules.Modified != 1 {
		t.Fatalf("explicit PCE pending rule update was lost: %+v", got)
	}
}

func TestEstatePendingCountsReferencesStillDetectChangedObjects(t *testing.T) {
	before, after := estatePendingTestRule("active", "1", "1", 443), estatePendingTestRule("draft", "1", "1", 443)
	after["providers"] = []interface{}{map[string]interface{}{"label": map[string]interface{}{"href": "/orgs/1/labels/2"}}}
	delta, err := estateComparePolicyRules(estatePendingTestBase, map[string]map[string]interface{}{"1": before}, map[string]map[string]interface{}{"1": after})
	if err != nil || delta.Modified != 1 {
		t.Fatalf("changed reference not detected: %+v %v", delta, err)
	}
}

func TestEstatePendingCountsIncompleteEvidenceNeverCompleteZero(t *testing.T) {
	for _, scenario := range []string{"missing-pending-bucket", "invalid-pending-array", "pending-partial", "unsupported-operation", "foreign-org", "foreign-origin", "conflicting-operation", "count-only", "malformed-rule", "duplicate-rule", "missing-ruleset", "truncated-rules", "inventory-failure"} {
		t.Run(scenario, func(t *testing.T) {
			pending := estatePendingTestPayload(map[string]string{"1": "update"})
			active := []map[string]interface{}{estatePendingTestRuleset("active", "1", estatePendingTestRule("active", "1", "1", 443))}
			draft := []map[string]interface{}{estatePendingTestRuleset("draft", "1", estatePendingTestRule("draft", "1", "1", 443))}
			pendingComplete := true
			switch scenario {
			case "missing-pending-bucket":
				delete(pending, "rule_sets")
			case "invalid-pending-array":
				pending["rule_sets"] = []interface{}{nil}
			case "pending-partial":
				pendingComplete = false
			case "unsupported-operation":
				pending = estatePendingTestPayload(map[string]string{"1": "unexpected"})
			case "foreign-org":
				pending["rule_sets"].([]map[string]interface{})[0]["href"] = "/orgs/2/sec_policy/draft/rule_sets/1"
			case "foreign-origin":
				pending["rule_sets"].([]map[string]interface{})[0]["href"] = "https://other.invalid/api/v2/orgs/1/sec_policy/draft/rule_sets/1"
			case "conflicting-operation":
				pending["rule_sets"] = append(pending["rule_sets"].([]map[string]interface{}), map[string]interface{}{"href": "/orgs/1/sec_policy/draft/rule_sets/1", "update_type": "delete"})
			case "count-only":
				delete(draft[0], "rules")
				draft[0]["rules_count"] = 1
			case "malformed-rule":
				delete(draft[0]["rules"].([]map[string]interface{})[0], "enabled")
			case "duplicate-rule":
				draft[0]["rules"] = append(draft[0]["rules"].([]map[string]interface{}), estatePendingTestRule("draft", "1", "1", 80))
			case "missing-ruleset":
				draft = nil
			case "truncated-rules":
				draft[0]["rules_count"] = 2
			}
			fetch := func(endpoint string) ([]map[string]interface{}, error) {
				if scenario == "inventory-failure" {
					return nil, errors.New("partial inventory")
				}
				if strings.HasSuffix(endpoint, "/active/rule_sets") {
					return active, nil
				}
				if strings.HasSuffix(endpoint, "/draft/rule_sets") {
					return draft, nil
				}
				return nil, errors.New("full rule definitions unavailable")
			}
			got := estateCountPendingPolicy(estatePendingTestBase, pending, pendingComplete, fetch)
			if got.Rules.Complete || len(got.Warnings) == 0 {
				t.Fatalf("incomplete evidence presented as complete: %+v", got)
			}
		})
	}
}

func TestEstatePendingCountsFetchMissingRuleDetailsAndCache(t *testing.T) {
	calls := map[string]int{}
	fetch := estatePolicyCachedCollection(func(endpoint string) ([]map[string]interface{}, error) {
		calls[endpoint]++
		state := "active"
		if strings.Contains(endpoint, "/draft/") {
			state = "draft"
		}
		if strings.HasSuffix(endpoint, "/rule_sets") {
			return []map[string]interface{}{{"href": "/orgs/1/sec_policy/" + state + "/rule_sets/1", "rules_count": 1}}, nil
		}
		port := 80
		if state == "draft" {
			port = 443
		}
		return []map[string]interface{}{estatePendingTestRule(state, "1", "1", port)}, nil
	})
	for _, state := range []string{"active", "draft"} {
		if count, err := estatePolicyCounts(estatePendingTestBase+"/sec_policy/"+state, fetch); err != nil || !count.Complete {
			t.Fatalf("inventory failed: %+v %v", count, err)
		}
	}
	got := estateCountPendingPolicy(estatePendingTestBase, estatePendingTestPayload(map[string]string{"1": "update"}), true, fetch)
	if !got.Rules.Complete || got.Rules.Modified != 1 || len(calls) != 4 {
		t.Fatalf("detail fallback failed: %+v calls=%v", got, calls)
	}
	for endpoint, count := range calls {
		if count != 1 {
			t.Errorf("collection fetched twice: %s %d", endpoint, count)
		}
	}
}

func TestEstatePendingCountsKnownZeroAndDuplicateMetadata(t *testing.T) {
	got := estateCountPendingPolicy(estatePendingTestBase, estatePendingTestPayload(nil), true, func(string) ([]map[string]interface{}, error) {
		t.Fatal("known zero pending rulesets should not fetch rule collections")
		return nil, nil
	})
	if !got.Rules.Complete || !got.Rulesets.Complete || got.Rules.Total != 0 || got.Rulesets.Total != 0 {
		t.Fatalf("known zero: %+v", got)
	}
	pending := estatePendingTestPayload(map[string]string{"1": "create"})
	rows := pending["rule_sets"].([]map[string]interface{})
	pending["rule_sets"] = append(rows, rows[0])
	got = estateCountPendingPolicy(estatePendingTestBase, pending, true, estatePendingTestFetch(t, nil, []map[string]interface{}{estatePendingTestRuleset("draft", "1")}))
	if !got.Rules.Complete || got.Rulesets.Added != 1 || got.Rules.Total != 0 {
		t.Fatalf("duplicate metadata inflated count: %+v", got)
	}
}

func TestEstatePendingCountsDeletionTombstones(t *testing.T) {
	before := estatePendingTestRule("active", "1", "1", 443)
	after := map[string]interface{}{"href": "/orgs/1/sec_policy/draft/rule_sets/1/sec_rules/1", "update_type": "delete"}
	got := estateCountPendingPolicy(estatePendingTestBase, estatePendingTestPayload(map[string]string{"1": "update"}), true, estatePendingTestFetch(t,
		[]map[string]interface{}{estatePendingTestRuleset("active", "1", before)},
		[]map[string]interface{}{estatePendingTestRuleset("draft", "1", after)}))
	if !got.Rules.Complete || got.Rules.Deleted != 1 || got.Rules.Total != 1 {
		t.Fatalf("deleted rule tombstone: %+v", got)
	}
}

func TestEstatePendingCountsFullInventoryUnknownCountsDoNotCauseDuplicateRuleRequests(t *testing.T) {
	calls := map[string]int{}
	fetch := estatePolicyCachedCollection(func(endpoint string) ([]map[string]interface{}, error) {
		calls[endpoint]++
		state := "active"
		if strings.Contains(endpoint, "/draft/") {
			state = "draft"
		}
		if strings.HasSuffix(endpoint, "/rule_sets") {
			return []map[string]interface{}{{"href": fmt.Sprintf("/orgs/1/sec_policy/%s/rule_sets/1", state)}}, nil
		}
		return []map[string]interface{}{estatePendingTestRule(state, "1", "1", 443)}, nil
	})
	for _, state := range []string{"active", "draft"} {
		_, _ = estatePolicyCounts(estatePendingTestBase+"/sec_policy/"+state, fetch)
	}
	got := estateCountPendingPolicy(estatePendingTestBase, estatePendingTestPayload(map[string]string{"1": "update"}), true, fetch)
	if !got.Rules.Complete || got.Rules.Total != 0 || len(calls) != 4 {
		t.Fatalf("cached count fallback: %+v %v", got, calls)
	}
	for _, count := range calls {
		if count != 1 {
			t.Fatalf("duplicate fetch: %v", calls)
		}
	}
}

func TestEstatePendingCountsRetryLegacyAndIncompleteEvidence(t *testing.T) {
	at := time.Now()
	policy := &EstatePolicy{Active: EstatePolicyCount{Complete: true}, Draft: EstatePolicyCount{Complete: true}, PendingComplete: true}
	if !estatePolicyRefreshDue(policy, at, at.Add(5*time.Minute)) {
		t.Fatal("legacy policy without pending counts must refresh at next sample")
	}
	policy.PendingCounts = &EstatePendingPolicyCounts{Rulesets: EstatePendingPolicyDelta{Complete: true}, Rules: EstatePendingPolicyDelta{Complete: true}}
	if estatePolicyRefreshDue(policy, at, at.Add(5*time.Minute)) {
		t.Fatal("complete pending counts should retain normal policy cache")
	}
	policy.PendingCounts.Rules.Complete = false
	if !estatePolicyRefreshDue(policy, at, at.Add(5*time.Minute)) {
		t.Fatal("incomplete rules must retry at next sample")
	}
}

func TestEstatePendingComparisonDoesNotMutateSource(t *testing.T) {
	before := estatePendingTestRule("active", "1", "1", 443)
	copyBefore := estatePendingTestRule("active", "1", "1", 443)
	_ = estateCanonicalRule(estatePendingTestBase, before)
	if !reflect.DeepEqual(before, copyBefore) {
		t.Fatal("normalization mutated shared collection")
	}
}

func TestEstatePendingRuleFallbackCannotEraseInlineEvidence(t *testing.T) {
	for _, scenario := range []string{"empty-fallback", "short-fallback", "malformed-inline", "alternate-array", "complete-larger-fallback"} {
		t.Run(scenario, func(t *testing.T) {
			first := estatePendingTestRule("draft", "1", "1", 443)
			delete(first, "resolve_labels_as") // Abbreviated inline result requires full definitions.
			second := estatePendingTestRule("draft", "1", "2", 22)
			set := estatePendingTestRuleset("draft", "1", first, second)
			fetched := []map[string]interface{}{}
			switch scenario {
			case "short-fallback":
				fetched = []map[string]interface{}{estatePendingTestRule("draft", "1", "1", 443)}
			case "malformed-inline":
				set["rules"] = []interface{}{first, nil}
				fetched = []map[string]interface{}{estatePendingTestRule("draft", "1", "1", 443)}
			case "alternate-array":
				set["sec_rules"] = set["rules"]
				set["rules"] = []interface{}{} // An empty alias must not hide the populated one.
			case "complete-larger-fallback":
				fetched = []map[string]interface{}{estatePendingTestRule("draft", "1", "1", 443), second, estatePendingTestRule("draft", "1", "3", 80)}
			}
			calls := 0
			index, err := estatePolicyRuleIndex(estatePendingTestBase+"/sec_policy/draft", set, func(endpoint string) ([]map[string]interface{}, error) {
				calls++
				if endpoint != estatePendingTestBase+"/sec_policy/draft/rule_sets/1/sec_rules" {
					t.Fatalf("unexpected fallback endpoint: %s", endpoint)
				}
				return fetched, nil
			})
			if calls != 1 {
				t.Fatalf("full fallback was not attempted: %d", calls)
			}
			if scenario == "complete-larger-fallback" {
				if err != nil || len(index) != 3 {
					t.Fatalf("complete larger collection rejected: %v %v", index, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "fewer rows") {
				t.Fatalf("short fallback erased inline evidence: index=%v err=%v", index, err)
			}
		})
	}
}

func TestEstatePendingCountsAbbreviatedDefinitionsAndEmptyFallbackStayUnknown(t *testing.T) {
	for _, mode := range []string{"abbreviated-inline", "older-pce-omits-resolution"} {
		t.Run(mode, func(t *testing.T) {
			fetch := estatePolicyCachedCollection(func(endpoint string) ([]map[string]interface{}, error) {
				state := "active"
				if strings.Contains(endpoint, "/draft/") {
					state = "draft"
				}
				rule := estatePendingTestRule(state, "1", "1", 443)
				delete(rule, "resolve_labels_as")
				if strings.HasSuffix(endpoint, "/rule_sets") {
					return []map[string]interface{}{estatePendingTestRuleset(state, "1", rule)}, nil
				}
				if mode == "older-pce-omits-resolution" {
					return []map[string]interface{}{rule}, nil
				}
				return []map[string]interface{}{}, nil
			})
			got := estateCountPendingPolicy(estatePendingTestBase, estatePendingTestPayload(map[string]string{"1": "update"}), true, fetch)
			if !got.Rulesets.Complete || got.Rulesets.Modified != 1 || got.Rules.Complete || len(got.Warnings) == 0 {
				t.Fatalf("missing rule evidence became complete zero: %+v", got)
			}
		})
	}
}
