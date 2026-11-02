'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function renderer() {
    const window = {};
    const context = vm.createContext({ window, Intl, Date });
    vm.runInContext(fs.readFileSync(path.join(__dirname, '../../static/estate-email.js'), 'utf8'), context);
    return window.EstateEmail.render;
}

function fixture() {
    return {
        schema_version: 1, generated_at: '2026-11-02T16:00:00Z', version: '1.4.0-dev', timezone: 'America/Chicago',
        window: { first_day: '2026-10-26', last_day: '2026-11-01', start: '2026-10-26T00:00:00-05:00', end_exclusive: '2026-11-02T00:00:00-06:00' },
        previous_window: { first_day: '2026-10-19', last_day: '2026-10-25' },
        sections: ['estate', 'health', 'policy', 'traffic', 'changes', 'quality'],
        estate: {
            observed_at: '2026-11-01T23:00:00Z', previous_observed_at: '2026-10-25T23:00:00Z',
            total: { current: 12345, previous: 12300, change: 45 }, managed: { current: 9000, previous: 8990, change: 10 }, unmanaged: { current: 3345, previous: 3310, change: 35 },
            enforcement: { full: 6000, visibility_only: 3000 }, ven_count: 5, versions_available: true,
            versions: [{ version: '25.2.0', total: 3 }, { version: 'Unknown', total: 2 }]
        },
        health: { unique_affected: { warning: 0, stopped: 2, tampering: 1 }, previous_unique_affected: { warning: 1, stopped: 3 } },
        policy: { at: '2026-11-01T23:00:00Z', active: { rulesets: 20, rules: 500, complete: true }, draft: { rulesets: 21, rules: 510, complete: true }, pending_complete: true, pending: [{ name: 'Payments', kind: 'rule_set', operation: 'update' }], last_provisioned_at: '2026-10-30T18:00:00Z', warnings: [] },
        previous_policy: { active: { rulesets: 18, rules: 480, complete: true }, draft: { rulesets: 20, rules: 500, complete: true } },
        traffic: [{ target: 'Production', total: 123456, previous: 100000, change: 23456, covered_days: 7, previous_covered_days: 7, missing_days: [] }],
        changes: [{ at: '2026-10-29T12:00:00Z', name: 'web-01', kind: 'enforcement', before: 'visibility_only', after: 'full' }],
        quality: { covered_days: 7, previous_covered_days: 7, missing_days: [], first_observation: '2026-10-26T05:01:00Z', last_observation: '2026-11-01T23:00:00Z', warnings: [] }
    };
}

function deepFreeze(value) {
    if (value && typeof value === 'object') { for (const child of Object.values(value)) deepFreeze(child); Object.freeze(value); }
    return value;
}

test('email renderer is a pure function with the agreed fragment and alternative contract', () => {
    const render = renderer();
    const report = deepFreeze(fixture());
    const before = JSON.stringify(report);
    const result = render(report);
    assert.deepEqual(Object.keys(result).sort(), ['html', 'subject', 'text']);
    assert.equal(result.subject, 'Illumio Weekly Estate Report: 2026-10-26 through 2026-11-01');
    assert.match(result.html, /^<table /);
    assert.match(result.html, /<\/table>$/);
    assert.doesNotMatch(result.html, /<!doctype|<html|<head|<body|<script|<style|<link|<img|<iframe|var\(|display\s*:\s*(?:grid|flex)/i);
    assert.match(result.html, /cellpadding="0"/);
    assert.match(result.html, /font-family:Arial,Helvetica,sans-serif/);
    assert.match(result.html, /<td bgcolor="#ffffff" style="[^"]*background-color:#ffffff/);
    assert.match(result.html, /border-top:5px solid #f26322/);
    assert.equal(JSON.stringify(report), before);
    assert.equal(render(report).html, result.html);
    assert.match(result.text, /Workloads \| This week \| Previous week \| Change/);
    assert.match(result.text, /Total \| 12,345 \| 12,300 \| \+45/);
    assert.match(result.text, /Production \| 123,456 \| 7\/7 \| 100,000 \| \+23,456/);
});

test('selected sections alone determine data included in both HTML and plain text', () => {
    const render = renderer();
    const headings = { estate: 'Estate overview', health: 'VEN health', policy: 'Policy overview', traffic: 'Monitored blocked traffic', changes: 'Estate change log', quality: 'Data quality and scope' };
    for (const [section, title] of Object.entries(headings)) {
        const report = fixture(); report.sections = [section];
        const result = render(report);
        assert.ok(result.html.includes('>' + title + '</h2>'), 'missing selected heading ' + title);
        assert.ok(result.text.includes(title), 'missing plain heading ' + title);
        for (const [otherSection, otherTitle] of Object.entries(headings)) if (otherSection !== section) {
            assert.ok(!result.html.includes('>' + otherTitle + '</h2>'), 'included deselected heading ' + otherTitle);
            assert.ok(!result.text.includes('\n' + otherTitle + '\n'), 'included deselected plain section ' + otherTitle);
        }
        if (section !== 'changes') assert.ok(!result.text.includes('web-01'));
        if (section !== 'policy') assert.ok(!result.text.includes('Payments'));
        if (section !== 'traffic') assert.ok(!result.text.includes('Production'));
    }
    const report = fixture(); delete report.sections;
    const result = render(report);
    assert.doesNotMatch(result.text, /Payments|Production|web-01|Distinct deployed VENs/);
});

test('all hostile input values are escaped and never become HTML markup or attributes', () => {
    const render = renderer();
    const attack = '"><img src=x onerror=alert(1)><script>evil()</script>&\'unsafe';
    const report = fixture();
    report.version = attack;
    report.timezone = attack;
    report.estate.versions[0].version = attack;
    report.estate.enforcement = { [attack]: 2 };
    report.health.unique_affected = { [attack]: 1 };
    report.policy.pending = [{ name: attack, kind: attack, operation: attack }];
    report.policy.warnings = [attack];
    report.traffic[0].target = attack;
    report.changes = [{ at: attack, name: attack, kind: attack, before: attack, after: attack }];
    report.quality.warnings = [attack];
    const result = render(report);
    assert.match(result.html, /&lt;img src=x onerror=alert\(1\)&gt;/);
    assert.match(result.html, /&lt;script&gt;evil\(\)&lt;\/script&gt;/);
    assert.match(result.html, /&amp;&#39;unsafe/);
    assert.doesNotMatch(result.html, /<(?:img|script|iframe|object)\b/i);
    for (const tag of result.html.match(/<[^>]*>/g)) assert.doesNotMatch(tag, /\bon\w+\s*=|javascript:|evil\(\)/i);
    assert.ok(result.text.includes(attack), 'plain text should preserve data without interpreting markup');
    assert.match(result.text, /report timezone unavailable/);
});

test('VEN versions count distinct installed VENs rather than managed workload records', () => {
    const report = fixture(); report.sections = ['estate'];
    const result = renderer()(report);
    assert.match(result.text, /Managed \| 9,000/);
    assert.match(result.text, /Distinct deployed VENs: 5\./);
    assert.match(result.text, /25\.2\.0 \| 3/);
    assert.match(result.text, /Unknown \| 2/);
    assert.match(result.text, /shared by multiple workload records is counted once/);
    assert.doesNotMatch(result.text, /Distinct deployed VENs: 9,000|Unknown \| 8,99/);
});

test('uncollected versions and empty distributions do not fabricate zero installed VENs', () => {
    const render = renderer();
    const report = fixture(); report.sections = ['estate'];
    report.estate.versions_available = false;
    report.estate.ven_count = 0;
    assert.match(render(report).text, /unavailable, not zero deployed VENs/);
    assert.doesNotMatch(render(report).text, /25\.2\.0|Distinct deployed VENs: 0/);
    report.estate.versions_available = true;
    report.estate.versions = [];
    assert.match(render(report).text, /No deployed VENs were observed/);
    report.estate.ven_count = 4;
    assert.match(render(report).text, /Per-version counts are unavailable/);
    assert.doesNotMatch(render(report).text, /No deployed VENs were observed/);
});

test('missing totals and comparison values remain unavailable while real zeros remain zero', () => {
    const report = fixture(); report.sections = ['estate', 'health'];
    report.estate.total = { current: 0 };
    report.estate.managed = { current: null, previous: undefined, change: 999 };
    report.estate.unmanaged = { previous: 100, change: -100 };
    report.health.unique_affected = { warning: 0, stopped: 2, error: null };
    report.health.previous_unique_affected = { warning: 1 };
    const result = renderer()(report);
    assert.match(result.text, /Total \| 0 \| Unavailable \| Unavailable/);
    assert.match(result.text, /Managed \| Unavailable \| Unavailable \| Unavailable/);
    assert.match(result.text, /Unmanaged \| Unavailable \| 100 \| Unavailable/);
    assert.match(result.text, /Warning \| 0 \| 1/);
    assert.match(result.text, /Stopped \| 2 \| Unavailable/);
    assert.match(result.text, /Error \| Unavailable \| Unavailable/);
    assert.doesNotMatch(result.text, /999/);
});

test('failed or missing policy data never displays numeric payloads as successful counts', () => {
    const report = fixture(); report.sections = ['policy'];
    report.policy.active.complete = false;
    report.policy.pending_complete = false;
    report.policy.last_provisioned_at = '0001-01-01T00:00:00Z';
    report.previous_policy.draft.complete = false;
    const result = renderer()(report);
    assert.match(result.text, /Active \| Unavailable \| 18 \| Unavailable \| 480/);
    assert.match(result.text, /Draft \| 21 \| Unavailable \| 510 \| Unavailable/);
    assert.match(result.text, /Last observed provisioning time: Unavailable/);
    assert.match(result.text, /Pending changes are unavailable or incomplete/);
    assert.doesNotMatch(result.text, /Payments|No pending changes were observed/);
    assert.match(result.text, /must not be added into a single unique total/);
});

test('partial traffic coverage is explicit and never used for a full-week comparison', () => {
    const report = fixture(); report.sections = ['traffic'];
    report.traffic = [
        { target: 'Partial', total: 9000, covered_days: 3, previous: 10000, previous_covered_days: 7, change: -1000, missing_days: ['2026-10-27'] },
        { target: 'No observations', total: 0, covered_days: 0, previous: 20, previous_covered_days: 4, change: -20 },
        { target: 'Invalid number', total: null, covered_days: 7, previous: 100, previous_covered_days: 7, change: 999 },
        { target: 'Real zero', total: 0, covered_days: 7, previous: 100, previous_covered_days: 7, change: -100 }
    ];
    const result = renderer()(report);
    assert.match(result.text, /Partial \| 9,000 \(partial\) \| 3\/7 \| 10,000 \| Unavailable/);
    assert.match(result.text, /No observations \| Unavailable \| 0\/7 \| Unavailable \| Unavailable/);
    assert.match(result.text, /Invalid number \| Unavailable \| 7\/7 \| 100 \| Unavailable/);
    assert.match(result.text, /Real zero \| 0 \| 7\/7 \| 100 \| -100/);
    assert.match(result.text, /Missing verified days for Partial: 2026-10-27/);
    assert.match(result.text, /Targets may overlap/);
    assert.match(result.text, /must not be added into an estate-wide total/);
    assert.match(result.text, /Legacy history without this provenance is excluded/);
    assert.doesNotMatch(result.text, /999/);
});

test('coverage warnings remain visible when users exclude the detailed quality section', () => {
    const report = fixture(); report.sections = ['health'];
    report.quality.covered_days = 2;
    report.quality.previous_covered_days = 0;
    report.quality.missing_days = ['2026-10-27', '2026-10-28'];
    report.quality.warnings = ['Health collection was incomplete for tampering; affected counts are observed minimums.', 'Collection failed during part of the week.'];
    const result = renderer()(report);
    for (const content of [result.html, result.text]) {
        assert.match(content, /2\/7 days this week; 0\/7 in the comparison week/);
        assert.match(content, /Missing periods are unknown, not zero activity/);
        assert.match(content, /2026-10-27, 2026-10-28/);
        assert.match(content, /Health collection was incomplete/);
        assert.match(content, /Collection failed during part of the week/);
    }
    assert.doesNotMatch(result.html, />Data quality and scope<\/h2>/);
    assert.doesNotMatch(result.text, /First observation \|/);
});

test('report timezone is used across DST and invalid dates do not become year-one dates', () => {
    const report = fixture(); report.sections = ['estate', 'quality'];
    const result = renderer()(report);
    assert.match(result.text, /Nov 2, 2026, 10:00 AM CST/);
    assert.match(result.text, /Nov 1, 2026, 05:00 PM CST/);
    assert.match(result.text, /Oct 25, 2026, 06:00 PM CDT/);
    report.estate.observed_at = '0001-01-01T00:00:00Z';
    report.quality.first_observation = 'invalid';
    report.quality.last_observation = null;
    report.generated_at = '0';
    report.window.first_day = '2026-02-31';
    report.window.last_day = '\r\nInjected-Header: value';
    const invalid = renderer()(report);
    assert.match(invalid.subject, /Unavailable through Unavailable$/);
    assert.doesNotMatch(invalid.subject, /\r|\n|Injected/);
    assert.match(invalid.text, /Latest retained inventory observation: Unavailable/);
    assert.match(invalid.text, /First observation \| Unavailable/);
    assert.match(invalid.text, /Last observation \| Unavailable/);
    assert.doesNotMatch(invalid.text, /Dec 31, 1|Jan 1, 2000/);
});

test('no selected rows are silently truncated and all values have a plain-text alternative', () => {
    const report = fixture(); report.sections = ['changes', 'policy'];
    report.changes = Array.from({ length: 1200 }, (_, index) => ({ at: '2026-10-29T12:00:00Z', name: 'change-' + index, kind: 'service', before: 'TCP:9300', after: 'TCP:9400' }));
    report.policy.pending = Array.from({ length: 1200 }, (_, index) => ({ name: 'pending-' + index, kind: 'rule_set', operation: 'update' }));
    const result = renderer()(report);
    for (const content of [result.html, result.text]) {
        assert.match(content, /change-1199/);
        assert.match(content, /pending-1199/);
        assert.match(content, /TCP:9300/);
        assert.doesNotMatch(content, /TCP:9,300/);
    }
});

test('missing and malformed optional objects remain unavailable without throwing or coercing blanks to zero', () => {
    const render = renderer();
    assert.doesNotThrow(() => render(null));
    assert.doesNotThrow(() => render({ sections: ['estate', 'health', 'policy', 'traffic', 'changes', 'quality'] }));
    const report = fixture(); report.sections = ['estate', 'traffic'];
    report.estate.total = { current: '', previous: false, change: Infinity };
    report.estate.managed = { current: NaN, previous: 0, change: 0 };
    report.estate.unmanaged = { current: -1, previous: 0, change: -1 };
    report.traffic = [{ target: 'Malformed', covered_days: '7', total: '100', previous_covered_days: 7, previous: '50', change: 50 }];
    const result = render(report);
    assert.match(result.text, /Total \| Unavailable \| Unavailable \| Unavailable/);
    assert.match(result.text, /Managed \| Unavailable \| 0 \| Unavailable/);
    assert.match(result.text, /Unmanaged \| Unavailable \| 0 \| Unavailable/);
    assert.match(result.text, /Malformed \| Unavailable \| Unavailable \| Unavailable \| Unavailable/);
    assert.doesNotMatch(result.text, /NaN|Infinity/);
});
