package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Pending counts describe unprovisioned changes, not the complete draft
// inventory. A complete zero is meaningful; an incomplete zero is unknown.
type EstatePendingPolicyDelta struct {
	Added    int  `json:"added"`
	Modified int  `json:"modified"`
	Deleted  int  `json:"deleted"`
	Total    int  `json:"total"`
	Complete bool `json:"complete"`
}

type EstatePendingPolicyCounts struct {
	Rulesets EstatePendingPolicyDelta `json:"rulesets"`
	Rules    EstatePendingPolicyDelta `json:"rules"`
	Warnings []string                 `json:"warnings"`
}

func estatePolicyCachedCollection(fetch func(string) ([]map[string]interface{}, error)) func(string) ([]map[string]interface{}, error) {
	type result struct {
		rows []map[string]interface{}
		err  error
	}
	cache := map[string]result{}
	return func(endpoint string) ([]map[string]interface{}, error) {
		if value, found := cache[endpoint]; found {
			return value.rows, value.err
		}
		rows, err := fetch(endpoint)
		cache[endpoint] = result{rows, err}
		return rows, err
	}
}

// The documented /sec_policy/pending response contains changed rule_sets,
// not the individual security rules. Count affected rulesets using that
// authoritative update_type, then compare the complete active/draft rules of
// those sets. Inventory subtraction cannot detect edits or offsetting changes.
func estateCountPendingPolicy(baseURL string, pending map[string]interface{}, pendingComplete bool, fetch func(string) ([]map[string]interface{}, error)) *EstatePendingPolicyCounts {
	counts := &EstatePendingPolicyCounts{Warnings: []string{}}
	rows, known := estatePolicyObjectArray(pending["rule_sets"])
	counts.Rulesets.Complete = pendingComplete && known
	counts.Rules.Complete = counts.Rulesets.Complete
	if !counts.Rulesets.Complete {
		counts.Warnings = append(counts.Warnings, "Pending ruleset inventory is incomplete; pending change counts are unavailable")
	}
	type pendingSet struct{ id, operation string }
	sets := []pendingSet{}
	seen := map[string]string{}
	for _, row := range rows {
		id, err := estatePolicyResourceID(baseURL+"/sec_policy/draft", mapString(row, "href"), "rule_sets")
		operation := mapString(row, "update_type")
		if err != nil || (operation != "create" && operation != "update" && operation != "delete") {
			counts.Rulesets.Complete, counts.Rules.Complete = false, false
			counts.Warnings = append(counts.Warnings, "Pending ruleset has an invalid identity or unsupported change type")
			continue
		}
		if previous, exists := seen[id]; exists {
			if previous != operation {
				counts.Rulesets.Complete, counts.Rules.Complete = false, false
				counts.Warnings = append(counts.Warnings, "Pending ruleset has conflicting change types")
			}
			continue
		}
		seen[id] = operation
		sets = append(sets, pendingSet{id, operation})
		estateAddPolicyDelta(&counts.Rulesets, operation)
	}
	if len(sets) == 0 {
		return counts
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].id < sets[j].id })
	active, activeErr := estatePolicyRulesetIndex(baseURL+"/sec_policy/active", fetch)
	draft, draftErr := estatePolicyRulesetIndex(baseURL+"/sec_policy/draft", fetch)
	if activeErr != nil || draftErr != nil {
		counts.Rules.Complete = false
		counts.Warnings = append(counts.Warnings, "Pending rule counts require complete active and draft ruleset inventories")
		return counts
	}
	for _, set := range sets {
		before, beforeFound := active[set.id]
		after, afterFound := draft[set.id]
		var oldRules, newRules map[string]map[string]interface{}
		var err error
		switch set.operation {
		case "create":
			if beforeFound || !afterFound {
				err = errors.New("new ruleset is inconsistent with active/draft inventories")
			} else {
				newRules, err = estatePolicyRuleIndex(baseURL+"/sec_policy/draft", after, fetch)
			}
		case "delete":
			if !beforeFound || (afterFound && mapString(after, "update_type") != "delete") {
				err = errors.New("deleted ruleset is inconsistent with active/draft inventories")
			} else {
				oldRules, err = estatePolicyRuleIndex(baseURL+"/sec_policy/active", before, fetch)
			}
		case "update":
			if !beforeFound || !afterFound {
				err = errors.New("modified ruleset is absent from active or draft inventory")
			} else {
				oldRules, err = estatePolicyRuleIndex(baseURL+"/sec_policy/active", before, fetch)
				if err == nil {
					newRules, err = estatePolicyRuleIndex(baseURL+"/sec_policy/draft", after, fetch)
				}
			}
		}
		if err == nil {
			var delta EstatePendingPolicyDelta
			delta, err = estateComparePolicyRules(baseURL, oldRules, newRules)
			if err == nil {
				counts.Rules.Added += delta.Added
				counts.Rules.Modified += delta.Modified
				counts.Rules.Deleted += delta.Deleted
				counts.Rules.Total += delta.Total
			}
		}
		if err != nil {
			counts.Rules.Complete = false
			counts.Warnings = append(counts.Warnings, fmt.Sprintf("Pending rules for ruleset %s are incomplete: %s", set.id, err))
		}
	}
	return counts
}

func estateAddPolicyDelta(delta *EstatePendingPolicyDelta, operation string) {
	switch operation {
	case "create":
		delta.Added++
	case "update":
		delta.Modified++
	case "delete":
		delta.Deleted++
	}
	delta.Total = delta.Added + delta.Modified + delta.Deleted
}

// Hrefs must belong to the requested policy version and PCE organization.
// Compare the terminal ID across versions, never names or collection position.
func estatePolicyResourceID(baseURL, rawHref, resource string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return "", errors.New("invalid policy collection URL")
	}
	href, err := url.Parse(rawHref)
	if err != nil || rawHref == "" || href.User != nil || href.RawQuery != "" || href.Fragment != "" || (href.Scheme != "" && href.Host == "") || (href.Host != "" && (href.Host != base.Host || href.Scheme != base.Scheme)) {
		return "", errors.New("invalid policy resource href")
	}
	path := href.Path
	if strings.HasPrefix(path, "/orgs/") {
		path = "/api/v2" + path
	}
	prefix := strings.TrimRight(base.Path, "/") + "/" + resource + "/"
	id := strings.TrimPrefix(path, prefix)
	if !strings.HasPrefix(path, prefix) || id == "" || id == "." || id == ".." || strings.Contains(id, "/") {
		return "", errors.New("policy resource href is outside the requested collection")
	}
	return id, nil
}

func estatePolicyRulesetIndex(baseURL string, fetch func(string) ([]map[string]interface{}, error)) (map[string]map[string]interface{}, error) {
	rows, err := fetch(baseURL + "/rule_sets")
	if err != nil {
		return nil, err
	}
	index := map[string]map[string]interface{}{}
	for _, row := range rows {
		id, err := estatePolicyResourceID(baseURL, mapString(row, "href"), "rule_sets")
		if err != nil {
			return nil, err
		}
		if _, duplicate := index[id]; duplicate {
			return nil, errors.New("duplicate ruleset identity")
		}
		index[id] = row
	}
	return index, nil
}

func estatePolicyRuleIndex(baseURL string, ruleset map[string]interface{}, fetch func(string) ([]map[string]interface{}, error)) (map[string]map[string]interface{}, error) {
	id, err := estatePolicyResourceID(baseURL, mapString(ruleset, "href"), "rule_sets")
	if err != nil {
		return nil, err
	}
	ruleBase := baseURL + "/rule_sets/" + url.PathEscape(id)
	inlineMinimum := 0
	for _, key := range []string{"rules", "sec_rules"} {
		// Abbreviated or malformed definitions still establish evidence that
		// rows existed. A later empty/shorter response must not erase that
		// evidence and turn an unavailable comparison into a confirmed zero.
		length := 0
		switch values := ruleset[key].(type) {
		case []map[string]interface{}:
			length = len(values)
		case []interface{}:
			length = len(values)
		}
		if length > inlineMinimum {
			inlineMinimum = length
		}
	}
	for _, key := range []string{"rules", "sec_rules"} {
		if rows, valid := estatePolicyObjectArray(ruleset[key]); valid {
			expected, known := estateExplicitRulesetCount(ruleset)
			if len(rows) < inlineMinimum || (known && len(rows) != expected) || (!known && len(rows) >= estateCollectionSyncLimit) {
				continue
			}
			if index, err := estateValidatedRuleIndex(ruleBase, rows); err == nil {
				return index, nil
			}
		}
	}
	// A count or abbreviated nested rules cannot establish which rules changed.
	// Fetch the complete documented per-ruleset collection, sharing the cache
	// with inventory totals so omitted inline arrays do not duplicate requests.
	rows, err := fetch(ruleBase + "/sec_rules")
	if err != nil {
		return nil, err
	}
	if len(rows) < inlineMinimum {
		return nil, errors.New("rule collection has fewer rows than the ruleset's inline rule evidence")
	}
	if expected, known := estateExplicitRulesetCount(ruleset); known && len(rows) != expected {
		return nil, errors.New("rule collection does not match the ruleset's declared count")
	}
	return estateValidatedRuleIndex(ruleBase, rows)
}

func estateExplicitRulesetCount(row map[string]interface{}) (int, bool) {
	for _, key := range []string{"rules_count", "num_rules", "rule_count", "rules_total"} {
		if count, valid := estateNonnegativeInteger(row[key]); valid {
			return count, true
		}
	}
	return 0, false
}

func estateValidatedRuleIndex(baseURL string, rows []map[string]interface{}) (map[string]map[string]interface{}, error) {
	index := map[string]map[string]interface{}{}
	for _, row := range rows {
		id, err := estatePolicyResourceID(baseURL, mapString(row, "href"), "sec_rules")
		if err != nil {
			return nil, err
		}
		if _, duplicate := index[id]; duplicate {
			return nil, errors.New("duplicate rule identity")
		}
		// A deletion tombstone carries its identity, not rule contents. All
		// other rules need a full definition, not just href/update metadata.
		if mapString(row, "update_type") != "delete" {
			if _, valid := row["enabled"].(bool); !valid {
				return nil, errors.New("rule definition omitted its enabled flag")
			}
			for _, key := range []string{"providers", "consumers", "ingress_services"} {
				values, valid := estatePolicyObjectArray(row[key])
				if !valid {
					return nil, fmt.Errorf("rule definition omitted or malformed %s", key)
				}
				for _, value := range values {
					if len(value) == 0 {
						return nil, fmt.Errorf("rule definition contains empty %s objects", key)
					}
				}
			}
			if value, valid := row["resolve_labels_as"].(map[string]interface{}); !valid || value == nil {
				return nil, errors.New("rule definition omitted label resolution")
			}
		}
		index[id] = row
	}
	return index, nil
}

func estateComparePolicyRules(baseURL string, before, after map[string]map[string]interface{}) (EstatePendingPolicyDelta, error) {
	delta := EstatePendingPolicyDelta{Complete: true}
	for id, rule := range after {
		old, existed := before[id]
		operation := mapString(rule, "update_type")
		if operation == "delete" {
			if !existed {
				return delta, errors.New("deleted rule is absent from active policy")
			}
			estateAddPolicyDelta(&delta, "delete")
			continue
		}
		if !existed {
			if operation != "" && operation != "create" {
				return delta, errors.New("new rule has an inconsistent change type")
			}
			estateAddPolicyDelta(&delta, "create")
		} else if operation == "create" || (operation != "" && operation != "update") {
			return delta, errors.New("existing rule has an inconsistent change type")
		} else if operation == "update" || estateCanonicalRule(baseURL, old) != estateCanonicalRule(baseURL, rule) {
			estateAddPolicyDelta(&delta, "update")
		}
	}
	for id := range before {
		if _, exists := after[id]; !exists {
			estateAddPolicyDelta(&delta, "delete")
		}
	}
	return delta, nil
}

// Provisioning rewrites versioned hrefs and response metadata without changing
// a rule. Ignore those fields and order-insensitive actor/service lists; keep
// all other definition fields, including description and disabled state.
func estateCanonicalRule(baseURL string, row map[string]interface{}) string {
	canonical := map[string]interface{}{}
	for key, value := range row {
		switch key {
		case "href", "id", "created_at", "created_by", "updated_at", "updated_by", "deleted_at", "deleted_by", "update_type", "caps", "permissions":
			continue
		}
		canonical[key] = estateCanonicalRuleValue(baseURL, key, value)
	}
	encoded, _ := json.Marshal(canonical)
	return string(encoded)
}

func estateCanonicalRuleValue(baseURL, field string, value interface{}) interface{} {
	switch value := value.(type) {
	case map[string]interface{}:
		if href, found := value["href"].(string); found {
			// References may be expanded with names/metadata in one version.
			return map[string]interface{}{"href": estateCanonicalPolicyHref(baseURL, href)}
		}
		out := map[string]interface{}{}
		for key, child := range value {
			out[key] = estateCanonicalRuleValue(baseURL, key, child)
		}
		return out
	case []map[string]interface{}:
		values := make([]interface{}, 0, len(value))
		for _, child := range value {
			values = append(values, child)
		}
		return estateCanonicalRuleValue(baseURL, field, values)
	case []interface{}:
		out := make([]interface{}, 0, len(value))
		for _, child := range value {
			out = append(out, estateCanonicalRuleValue(baseURL, field, child))
		}
		switch field {
		case "providers", "consumers", "ingress_services", "egress_services", "consuming_security_principals", "use_workload_subnets":
			sort.Slice(out, func(i, j int) bool {
				a, _ := json.Marshal(out[i])
				b, _ := json.Marshal(out[j])
				return string(a) < string(b)
			})
		}
		return out
	default:
		return value
	}
}

func estateCanonicalPolicyHref(baseURL, raw string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return raw
	}
	href, err := url.Parse(raw)
	if err != nil || href.RawQuery != "" || href.Fragment != "" || href.User != nil || (href.Host != "" && (href.Host != base.Host || href.Scheme != base.Scheme)) {
		return raw
	}
	path := href.Path
	if strings.HasPrefix(path, "/orgs/") {
		path = "/api/v2" + path
	}
	prefix := strings.TrimRight(base.Path, "/") + "/sec_policy/"
	if strings.HasPrefix(path, prefix) {
		parts := strings.SplitN(strings.TrimPrefix(path, prefix), "/", 2)
		if len(parts) == 2 && parts[0] != "" {
			path = prefix + "{version}/" + parts[1]
		}
	}
	return path
}
