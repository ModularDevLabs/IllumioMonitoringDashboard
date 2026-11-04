'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const repo = path.join(__dirname, '../..');
function element(tag = 'div') {
    let text = '';
    return {
        tagName: tag.toUpperCase(), children: [], dataset: {}, attributes: {}, listeners: {}, value: '', checked: false,
        hidden: false, disabled: false, className: '',
        get textContent() { return text + this.children.map(child => child.textContent).join(''); },
        set textContent(value) { text = String(value); this.children = []; },
        set innerHTML(_) { throw new Error('Remote values must not be inserted as HTML'); },
        appendChild(child) { this.children.push(child); return child; },
        replaceChildren(...children) { text = ''; this.children = children; },
        setAttribute(key, value) { this.attributes[key] = value; },
        addEventListener(event, callback) {
            const previous = this.listeners[event];
            this.listeners[event] = (...args) => { if (previous) previous(...args); return callback(...args); };
        },
        remove() {}, click() {}
    };
}
function app(file = 'estate.js', storage = new Map()) {
    const elements = new Map();
    const get = id => { if (!elements.has(id)) elements.set(id, element()); return elements.get(id); };
    const sections = ['estate', 'health', 'policy', 'traffic', 'changes', 'quality'].map(value => ({ ...element('input'), value, checked: true }));
    const document = {
        readyState: 'loading', body: element('body'), documentElement: { dataset: { unifiedTheme: 'dark' } },
        getElementById: get, createElement: element, addEventListener() {},
        querySelectorAll(selector) { return selector.endsWith(':checked') ? sections.filter(input => input.checked) : sections; }
    };
    const sessionStorage = { getItem(key) { return storage.get(key) ?? null; }, setItem(key, value) { storage.set(key, value); } };
    const window = { location: { search: '' }, document, URL, Blob, sessionStorage, setTimeout() {} };
    const context = vm.createContext({ document, window, sessionStorage, URL, URLSearchParams, Date, Blob, console, setTimeout() {}, setInterval() {} });
    if (file === 'estate.js') vm.runInContext(fs.readFileSync(path.join(repo, 'static/estate-csv.js'), 'utf8'), context);
    vm.runInContext(fs.readFileSync(path.join(repo, 'static', file), 'utf8'), context);
    return { context, window, document, get, sections, storage, api: file === 'estate.js' ? window.EstateUI : window.EstateSettings };
}
function descendants(root) { return [root, ...root.children.flatMap(descendants)]; }
function find(root, predicate) { return descendants(root).find(predicate); }
function toggle(element, open) { element.open = open; element.listeners.toggle(); }
function inventoryView() {
    const workloads = Array.from({ length: 205 }, (_, index) => ({ id: '/workloads/' + index, name: 'Workload ' + index, managed: index < 150, mode: 'idle', ven_version: '25.2' }));
    return { options: { inventory_enabled: true }, snapshot: { inventory_complete: true, total: workloads.length, managed: 150, unmanaged: 55, modes: { idle: 205 }, workloads } };
}
test('disabled estate features create no dashboard metrics and no fabricated zeros', () => {
    const t = app();
    t.api.renderDashboard({ options: {} });
    assert.equal(t.get('estate-dashboard-row').hidden, true);
    assert.equal(t.get('estate-inventory-breakdown').hidden, true);
    t.api.renderPage({ options: {} });
    assert.equal(t.get('estate-disabled').hidden, false);
    assert.equal(t.get('estate-report-section').hidden, true);
    assert.equal(t.get('estate-live-sections').children.length, 0);
});
test('inventory breakdown is opt-in, clickable, and does not treat an incomplete response as zero', () => {
    const t = app();
    t.api.renderDashboard({ options: { inventory_enabled: true }, snapshot: { inventory_complete: true, total: 10, managed: 8, unmanaged: 2 } });
    assert.equal(t.get('estate-inventory-breakdown').textContent, 'Managed: 8 · Unmanaged: 2');
    assert.equal(t.get('estate-inventory-breakdown').children[0].href, '/estate?inventory=managed');
    t.api.renderDashboard({ options: { inventory_enabled: true }, snapshot: { inventory_complete: false, total: 0, managed: 0 } });
    assert.match(t.get('estate-inventory-breakdown').textContent, /incomplete/);
    assert.doesNotMatch(t.get('estate-inventory-breakdown').textContent, /Managed: 0/);
});
test('dashboard estate changes use the saved weekly report baseline, not retained history totals', () => {
    const t = app();
    const since = '2026-11-02T15:00:00Z';
    const view = {
        options: { change_log_enabled: true, weekly_report_enabled: true, history_days: 365 },
        snapshot: { at: '2026-11-04T15:00:00Z' },
        changes: Array.from({ length: 160 }, (_, index) => ({ name: 'Retained change ' + index, kind: 'labels', at: since })),
        changes_since_report: { status: 'available', count: 7, since, report_week: '2026-10-26', partial: false }
    };
    t.api.renderDashboard(view);
    const root = t.get('estate-dashboard-content');
    const card = root.children.find(node => node.children[0].textContent === 'Changes since last weekly report');
    assert.equal(card.children[1].textContent, '7');
    assert.match(card.textContent, /Since /);
    assert.ok(card.textContent.includes(t.api.timestamp(since)));
    assert.match(card.textContent, /Report week: 2026-10-26/);
    assert.doesNotMatch(root.textContent, /160|Retained estate changes/);
    t.api.renderPage(view);
    const changeLog = t.get('estate-live-sections').children.find(node => node.children[0].textContent === 'Estate Change Log');
    assert.match(changeLog.textContent, /Showing 1–100 of 160/);
    assert.match(changeLog.textContent, /Retained change 0/);
    view.changes_since_report.count = 0;
    t.api.renderDashboard(view);
    assert.equal(root.children[0].children[1].textContent, '0');
});
test('dashboard awaits a report baseline instead of substituting the lifetime change count', () => {
    const t = app();
    const view = { options: { change_log_enabled: true, weekly_report_enabled: true }, snapshot: {}, changes: Array(160).fill({}), changes_since_report: { status: 'awaiting_report', count: 0 } };
    t.api.renderDashboard(view);
    let text = t.get('estate-dashboard-content').textContent;
    assert.match(text, /Changes since last weekly reportAwaiting report/);
    assert.match(text, /Save a completed-week snapshot.*Run report now.*change-baseline checkbox selected/);
    assert.doesNotMatch(text, /160/);
    delete view.changes_since_report;
    t.api.renderDashboard(view);
    text = t.get('estate-dashboard-content').textContent;
    assert.match(text, /Awaiting report/);
    assert.doesNotMatch(text, /160/);
    view.options.weekly_report_enabled = false;
    t.api.renderDashboard(view);
    text = t.get('estate-dashboard-content').textContent;
    assert.match(text, /No saved report baseline/);
    assert.match(text, /Enable Weekly Estate Report in Settings/);
    assert.doesNotMatch(text, /160/);
    view.options.change_log_enabled = false;
    t.api.renderDashboard(view);
    assert.equal(t.get('estate-dashboard-row').hidden, true);
});
test('change log displays the actual observation time and falls back for older history', () => {
    const t = app();
    const observed = '2026-11-04T17:00:00Z';
    const changed = '2026-11-02T13:00:00Z';
    const legacy = '2026-11-03T14:00:00Z';
    t.api.renderPage({ options: { change_log_enabled: true }, snapshot: {}, changes: [
        { name: 'Delayed policy observation', kind: 'policy', at: changed, observed_at: observed },
        { name: 'Older recorded change', kind: 'labels', at: legacy }
    ] });
    const rows = find(t.get('estate-live-sections'), node => node.tagName === 'TBODY').children;
    assert.equal(rows[0].children[0].textContent, t.api.timestamp(observed));
    assert.notEqual(rows[0].children[0].textContent, t.api.timestamp(changed));
    assert.equal(rows[1].children[0].textContent, t.api.timestamp(legacy));
});
test('dashboard report-relative changes disclose partial or unavailable observations without inventing totals', () => {
    const t = app();
    const warning = '<script>untrusted warning</script> History does not cover the entire interval.';
    const view = { options: { change_log_enabled: true }, snapshot: {}, changes: Array(160).fill({}), changes_since_report: { status: 'available', count: 3, partial: true, warning } };
    t.api.renderDashboard(view);
    const root = t.get('estate-dashboard-content');
    assert.equal(root.children[0].children[1].textContent, '3 observed (incomplete)');
    assert.equal(find(root, node => node.className === 'estate-warning').textContent, warning);
    assert.match(root.textContent, /after the last saved weekly report/);
    view.changes_since_report = { status: 'unavailable', count: 0, warning: 'Saved report metadata could not be read.' };
    t.api.renderDashboard(view);
    assert.equal(root.children[0].children[1].textContent, 'Unavailable');
    assert.match(root.textContent, /Saved report metadata could not be read/);
    assert.doesNotMatch(root.textContent, /160/);
    view.changes_since_report = { status: 'available' };
    t.api.renderDashboard(view);
    assert.equal(root.children[0].children[1].textContent, 'Unavailable');
});
test('inventory and its lazy workload list start collapsed, ignoring the former automatic open state', () => {
    const storage = new Map([['illumio_estate_section_Workload Inventory', 'open']]);
    const t = app('estate.js', storage);
    t.window.location.search = '?inventory=managed';
    t.api.renderPage(inventoryView());
    const root = t.get('estate-live-sections');
    const inventory = find(root, node => node.id === 'estate-inventory');
    const list = find(root, node => node.id === 'estate-workload-list');
    assert.equal(inventory.open, false);
    assert.equal(list.open, false);
    assert.equal(find(list, node => node.tagName === 'SELECT').value, 'managed');
    assert.equal(find(list, node => node.tagName === 'TABLE'), undefined);
    inventory.listeners.toggle();
    assert.equal(storage.has('illumio_estate_inventory_section_v2'), false);
    toggle(inventory, true);
    assert.equal(list.open, false);
    assert.equal(find(list, node => node.tagName === 'TABLE'), undefined);
    toggle(list, true);
    assert.equal(find(list, node => node.tagName === 'TBODY').children.length, 100);
    assert.match(list.textContent, /Showing 1–100 of 150/);
    find(list, node => node.tagName === 'BUTTON' && node.textContent === 'Next').listeners.click();
    assert.equal(find(list, node => node.tagName === 'TBODY').children.length, 50);
    assert.match(list.textContent, /Workload 149/);
    assert.match(list.textContent, /Showing 101–150 of 150/);
});
test('explicit inventory/list choices and management filtering survive refresh and navigation', () => {
    const t = app();
    const view = inventoryView();
    t.api.renderPage(view);
    let root = t.get('estate-live-sections');
    toggle(find(root, node => node.id === 'estate-inventory'), true);
    let list = find(root, node => node.id === 'estate-workload-list');
    toggle(list, true);
    const filter = find(list, node => node.tagName === 'SELECT');
    filter.value = 'unmanaged'; filter.listeners.change();
    assert.equal(find(list, node => node.tagName === 'TBODY').children.length, 55);
    t.api.renderPage(view);
    root = t.get('estate-live-sections');
    list = find(root, node => node.id === 'estate-workload-list');
    assert.equal(find(root, node => node.id === 'estate-inventory').open, true);
    assert.equal(list.open, true);
    assert.equal(find(list, node => node.tagName === 'SELECT').value, 'unmanaged');
    assert.equal(find(list, node => node.tagName === 'TBODY').children.length, 55);
    const next = app('estate.js', t.storage);
    next.api.renderPage(view);
    const nextRoot = next.get('estate-live-sections');
    const nextList = find(nextRoot, node => node.id === 'estate-workload-list');
    assert.equal(find(nextRoot, node => node.id === 'estate-inventory').open, true);
    assert.equal(nextList.open, true);
    assert.equal(find(nextList, node => node.tagName === 'SELECT').value, 'unmanaged');
    toggle(nextList, false);
    toggle(find(nextRoot, node => node.id === 'estate-inventory'), false);
    next.api.renderPage(view);
    assert.equal(find(nextRoot, node => node.id === 'estate-inventory').open, false);
    assert.equal(find(nextRoot, node => node.id === 'estate-workload-list').open, false);
});
test('inventory choices persist across refresh even when browser storage is blocked', () => {
    const t = app();
    t.context.sessionStorage.getItem = t.context.sessionStorage.setItem = () => { throw new Error('Storage blocked'); };
    t.api.renderPage(inventoryView());
    const root = t.get('estate-live-sections');
    toggle(find(root, node => node.id === 'estate-inventory'), true);
    toggle(find(root, node => node.id === 'estate-workload-list'), true);
    t.api.renderPage(inventoryView());
    assert.equal(find(root, node => node.id === 'estate-inventory').open, true);
    assert.equal(find(root, node => node.id === 'estate-workload-list').open, true);
});
test('standalone issue and label CSV exports include every displayed snapshot row across pages', () => {
    const t = app();
    const view = {
        options: { label_hygiene_enabled: true, required_label_types: ['app'], issue_aging_enabled: true },
        snapshot: { at: '2026-11-02T15:00:00Z', inventory_complete: true, labels_complete: true, labels_assessed: 205, total: 205, health_complete: { warning: true, error: true, stopped: true, suspended: true },
            label_gaps: Array.from({ length: 205 }, (_, index) => ({ id: 'label-' + index, name: 'Missing ' + index, missing: ['app'] })) },
        issues: Array.from({ length: 205 }, (_, index) => ({ id: 'issue-' + index, workload_id: 'workload-' + index, name: 'Issue ' + index, kind: 'warning', active: true, occurrences: 1, observed_days: ['2026-11-02'] }))
    };
    const exported = [];
    t.window.EstateCSV.download = result => { exported.push(result); return true; };
    t.context.fetch = () => { throw new Error('CSV exports must not fetch data'); };
    t.api.renderPage(view);
    const root = t.get('estate-live-sections');
    const buttons = descendants(root).filter(node => node.dataset.estateExport);
    assert.equal(buttons.length, 2);
    assert.equal(descendants(root).filter(node => node.tagName === 'TBODY').every(body => body.children.length === 100), true);
    assert.match(root.textContent, /Exports all 205 observed rows, including other pages/);
    for (const button of buttons) { assert.equal(button.disabled, false); button.listeners.click(); }
    assert.deepEqual(exported.map(result => result.rowCount), [205, 205]);
    assert.match(exported[0].content, /issue-204/);
    assert.match(exported[1].content, /label-204/);
    assert.equal(exported.every(result => !result.partial), true);
});
test('CSV controls explain disabled unknown/empty results and explicitly flag partial exports', () => {
    const t = app();
    const view = { options: { issue_aging_enabled: true, label_hygiene_enabled: true, required_label_types: ['app'] } };
    t.api.renderPage(view);
    const root = t.get('estate-live-sections');
    assert.equal(descendants(root).filter(node => node.dataset.estateExport).every(node => node.disabled), true);
    assert.match(root.textContent, /Issue observations are unavailable/);
    assert.match(root.textContent, /Label assessment is unavailable/);
    view.snapshot = { at: '2026-11-02T15:00:00Z', inventory_complete: false, labels_complete: false, labels_assessed: 1, total: 2, label_gaps: [{ id: 'workload-1', name: 'Only assessed workload', missing: ['app'] }] };
    view.issues = [{ id: 'issue-1', kind: 'warning' }];
    let exported;
    t.window.EstateCSV.download = result => { exported = result; return true; };
    t.api.renderPage(view);
    assert.match(root.textContent, /Partial collection: the file identifies incomplete observations/);
    find(root, node => node.dataset.estateExport === 'labels').listeners.click();
    assert.equal(exported.partial, true);
    assert.match(root.textContent, /This is a partial collection/);
    view.snapshot.label_gaps = []; view.issues = [];
    t.api.renderPage(view);
    assert.equal(descendants(root).filter(node => node.dataset.estateExport).every(node => node.disabled), true);
    assert.match(root.textContent, /There are no observed missing-label rows to export; unassessed workloads remain unknown/);
    assert.match(root.textContent, /There are no observed issue rows to export/);
});
test('weekly reports can be enabled independently of live version and inventory cards', () => {
    const t = app();
    t.api.renderPage({ options: { weekly_report_enabled: true }, snapshot: { versions: [{ version: '25.2', count: 12 }] } });
    assert.equal(t.get('estate-report-section').hidden, false);
    assert.equal(t.get('estate-live-sections').children.length, 0);
});
test('version distribution includes unknown versions and safely displays PCE strings', () => {
    const t = app();
    t.api.renderPage({ options: { ven_versions_enabled: true }, snapshot: { inventory_complete: true, versions_collected: true, ven_count: 9, versions: [{ version: '<img src=x onerror=alert(1)>', count: 7 }, { version: 'Unknown', count: 2 }] } });
    const text = t.get('estate-live-sections').textContent;
    assert.match(text, /<img src=x onerror=alert\(1\)>/);
    assert.match(text, /Unknown2/);
    assert.match(text, /Distinct installed VENs: 9/);
    assert.doesNotMatch(text, /Managed workloads/);
    t.api.renderPage({ options: { ven_versions_enabled: true }, snapshot: { inventory_complete: false, versions: [] } });
    assert.match(t.get('estate-live-sections').textContent, /unavailable/);
});
test('newly enabled version insights never turn an older inventory into zero installed VENs', () => {
    const t = app();
    const view = { options: { ven_versions_enabled: true }, snapshot: { inventory_complete: true, total: 4, managed: 4, versions: [], ven_count: 0, versions_collected: false } };
    t.api.renderPage(view);
    assert.match(t.get('estate-live-sections').textContent, /Awaiting VEN version data/);
    assert.doesNotMatch(t.get('estate-live-sections').textContent, /No installed VENs observed/);
    t.api.renderDashboard(view);
    assert.match(t.get('estate-dashboard-content').textContent, /Installed VENsUnavailable/);
    view.snapshot.versions_collected = true;
    t.api.renderPage(view);
    assert.match(t.get('estate-live-sections').textContent, /Distinct installed VENs: 0/);
    assert.match(t.get('estate-live-sections').textContent, /No installed VENs observed/);
});
test('partial or newly enabled label hygiene never claims every workload is correctly labelled', () => {
    const t = app();
    const view = { options: { label_hygiene_enabled: true, required_label_types: ['app'] }, snapshot: { inventory_complete: true, total: 10, labels_complete: false, labels_assessed: 4, label_gaps: [] } };
    t.api.renderPage(view);
    assert.match(t.get('estate-live-sections').textContent, /4 of 10 workloads assessed/);
    assert.match(t.get('estate-live-sections').textContent, /Unassessed workloads remain unknown/);
    assert.doesNotMatch(t.get('estate-live-sections').textContent, /All observed workloads have/);
    t.api.renderDashboard(view);
    assert.match(t.get('estate-dashboard-content').textContent, /0 observed \(incomplete\)/);
    view.snapshot.labels_assessed = 0;
    t.api.renderPage(view);
    assert.match(t.get('estate-live-sections').textContent, /Awaiting a complete label assessment/);
    t.api.renderDashboard(view);
    assert.match(t.get('estate-dashboard-content').textContent, /Missing required labelsUnavailable/);
    view.snapshot.labels_complete = true;
    view.snapshot.labels_assessed = 10;
    t.api.renderPage(view);
    assert.match(t.get('estate-live-sections').textContent, /All observed workloads have/);
});
test('policy tables keep active and draft separate and disclose incomplete collections', () => {
    const t = app();
    t.api.renderPage({ options: { policy_enabled: true }, snapshot: { policy: {
        active: { complete: true, rulesets: 4, rules: 20 }, draft: { complete: false, rulesets: 9, rules: 99 }, pending_complete: false,
        pending: [{ name: '<script>unsafe</script>', kind: 'rule', operation: 'updated' }]
    } } });
    const text = t.get('estate-live-sections').textContent;
    assert.match(text, /not added together/);
    assert.match(text, /Active420Complete/);
    assert.match(text, /Draft——Unavailable/);
    assert.match(text, /<script>unsafe<\/script>/);
    assert.doesNotMatch(text, /99/);
});
test('zero dates and unavailable metric values are not displayed as real observations', () => {
    const t = app();
    assert.equal(t.api.timestamp('0001-01-01T00:00:00Z'), 'Not available');
    assert.equal(t.api.timestamp('not a date'), 'Not available');
    assert.equal(t.api.count(undefined), '—');
    assert.equal(t.api.count(0), '0');
});
test('report requests carry selected sections, chosen week, and current unified theme', () => {
    const t = app();
    t.get('estate-week-start').value = '2026-10-19';
    t.sections.forEach(input => { input.checked = input.value === 'estate'; });
    const query = new URL(t.api.reportQuery('html'), 'http://localhost');
    assert.equal(query.searchParams.get('sections'), 'estate');
    assert.equal(query.searchParams.get('week'), '2026-10-19');
    assert.equal(query.searchParams.get('theme'), 'dark');
    assert.equal(query.searchParams.get('format'), 'html');
    t.sections[0].checked = false;
    assert.throws(() => t.api.reportQuery(), /at least one/);
});
test('preview only enables export after metadata and themed HTML are successfully loaded', async () => {
    const t = app();
    const calls = [];
    t.context.fetch = async url => {
        calls.push(url);
        return { ok: true, json: async () => ({ report: { timezone: 'America/Chicago', window: { first_day: '2026-10-19' }, quality: { warnings: ['Missing two collection days.'] } }, html: '<!DOCTYPE html><p>VEN versions</p>' }) };
    };
    await t.api.previewReport();
    assert.equal(t.get('estate-week-start').value, '2026-10-19');
    assert.equal(t.get('estate-report-preview').hidden, false);
    assert.equal(t.get('estate-report-preview').srcdoc, '<!DOCTYPE html><p>VEN versions</p>');
    assert.equal(t.get('estate-download').disabled, false);
    assert.match(t.get('estate-report-status').textContent, /America\/Chicago.*Missing two/);
    assert.equal(calls.length, 1);
    assert.match(calls[0], /theme=dark&format=preview/);
    t.api.invalidateReport();
    assert.equal(t.get('estate-download').disabled, true);
    assert.equal(t.get('estate-report-preview').hidden, true);
});
test('report error text is visible and no stale export is enabled', async () => {
    const t = app();
    t.context.fetch = async () => ({ ok: false, status: 400, text: async () => 'Select a completed Monday week.' });
    await t.api.previewReport();
    assert.equal(t.get('estate-download').disabled, true);
    assert.match(t.get('estate-report-status').textContent, /Select a completed Monday/);
});
test('manual weekly snapshot is an explicit POST and refreshes the baseline and saved history', async () => {
    const t = app();
    t.get('estate-week-start').value = '2026-10-19';
    const calls = [];
    t.context.fetch = async (url, options) => {
        calls.push({ url, options });
        let data;
        if (url === '/api/estate') data = { options: { weekly_report_enabled: true, change_log_enabled: true }, snapshot: {}, changes: Array(160).fill({}), changes_since_report: { status: 'available', count: 3, since: '2026-10-26T00:00:00Z', report_week: '2026-10-19' } };
        else if (url === '/api/estate/report/runs') data = { runs: [] };
        else data = { report: { id: 'weekly-snapshot', timezone: 'UTC', sections: ['estate', 'changes'], window: { first_day: '2026-10-19' }, quality: { warnings: [] } }, html: '<p>Frozen manual report</p>' };
        return { ok: true, json: async () => data };
    };
    await t.api.previewReport(true);
    assert.equal(calls[0].options.method, 'POST');
    assert.match(calls[0].url, /^\/api\/estate\/report\/snapshot\?/);
    assert.match(calls[0].url, /week=2026-10-19/);
    assert.equal(calls.some(call => call.url === '/api/estate'), true);
    assert.equal(calls.some(call => call.url === '/api/estate/report/runs'), true);
    assert.match(t.get('estate-dashboard-content').textContent, /Changes since last weekly report3/);
    assert.doesNotMatch(t.get('estate-dashboard-content').textContent, /160/);
    assert.match(t.get('estate-report-status').textContent, /Saved weekly snapshot ready/);
    assert.equal(t.get('estate-report-preview').srcdoc, '<p>Frozen manual report</p>');
    assert.deepEqual(t.sections.filter(input => input.checked).map(input => input.value), ['estate', 'changes']);
    assert.equal(t.get('estate-save-snapshot').disabled, false);
    t.document.documentElement.dataset.unifiedTheme = 'light';
    await t.api.refreshReportTheme();
    assert.equal(calls.at(-1).options.method, 'GET');
    assert.match(calls.at(-1).url, /^\/api\/estate\/report\/runs\?id=weekly-snapshot&format=preview&theme=light$/);
    assert.equal(calls.filter(call => call.options.method === 'POST').length, 1);
});
test('saved reports force a fresh baseline even while an older estate refresh is in flight', async () => {
    for (const mode of ['now', 'weekly']) {
        for (const staleFailure of [false, true]) {
            const t = app();
            let finishOld;
            let estateCalls = 0;
            const latest = { options: { weekly_report_enabled: true, change_log_enabled: true }, snapshot: { at: '2026-11-04T17:00:00Z' }, changes_since_report: { status: 'available', count: 2 } };
            t.context.fetch = async url => {
                if (url === '/api/estate') {
                    if (++estateCalls === 1) return new Promise(resolve => { finishOld = resolve; });
                    return { ok: true, json: async () => latest };
                }
                if (url === '/api/estate/report/runs') return { ok: true, json: async () => ({ runs: [] }) };
                return { ok: true, json: async () => mode === 'now' ? onDemandBundle('baseline-now', true) : { report: { id: 'weekly-baseline', window: { first_day: '2026-10-26' } }, html: '<p>Saved week</p>' } };
            };
            const older = t.api.load();
            await (mode === 'now' ? t.api.runReportNow() : t.api.previewReport(true));
            assert.equal(estateCalls, 2, mode + ' must not skip the post-save refresh');
            const dashboard = t.get('estate-dashboard-content').textContent;
            const status = t.get('estate-status').textContent;
            assert.match(dashboard, /Changes since last weekly report2/);
            finishOld(staleFailure ? { ok: false, status: 503, text: async () => 'Old request failed' } : { ok: true, json: async () => ({ ...latest, changes_since_report: { status: 'available', count: 160 } }) });
            await older;
            assert.equal(t.get('estate-dashboard-content').textContent, dashboard);
            assert.equal(t.get('estate-status').textContent, status);
        }
    }
});
test('an obsolete refresh cannot unlock a newer refresh or replace the saved report history', async () => {
    const t = app();
    const estateRequests = [];
    t.context.fetch = async url => {
        assert.equal(url, '/api/estate');
        return new Promise(resolve => { estateRequests.push(resolve); });
    };
    const old = t.api.load();
    const fresh = t.api.load(true);
    estateRequests[0]({ ok: true, json: async () => ({ options: {} }) });
    await old;
    await t.api.load();
    assert.equal(estateRequests.length, 2);
    estateRequests[1]({ ok: true, json: async () => ({ options: {} }) });
    await fresh;

    let finishOldHistory;
    t.context.fetch = async () => new Promise(resolve => { finishOldHistory = resolve; });
    const oldHistory = t.api.loadReportRuns();
    reportFetch(t, async () => ({ ok: true, json: async () => onDemandBundle('new-history') }));
    await t.api.runReportNow();
    await t.api.loadReportRuns();
    const history = t.get('estate-report-runs').textContent;
    finishOldHistory({ ok: false, status: 503, text: async () => 'Stale history failed' });
    await oldHistory;
    assert.equal(t.get('estate-report-runs').textContent, history);
    assert.doesNotMatch(history, /Stale history failed/);
});
test('preview events never save snapshots, and failed saves cannot claim success', async () => {
    const t = app();
    const calls = [];
    t.context.fetch = async (url, options) => {
        calls.push({ url, options });
        return { ok: false, status: 500, text: async () => 'Snapshot could not be saved.' };
    };
    await t.api.previewReport({ type: 'click' });
    assert.equal(calls[0].options.method, 'GET');
    assert.doesNotMatch(calls[0].url, /snapshot/);
    await t.api.previewReport(true);
    assert.equal(calls[1].options.method, 'POST');
    assert.equal(calls.length, 2);
    assert.match(t.get('estate-report-status').textContent, /Unable to save weekly snapshot/);
    assert.doesNotMatch(t.get('estate-report-status').textContent, /snapshot ready/);
    assert.equal(t.get('estate-download').disabled, true);
    assert.equal(t.get('estate-save-snapshot').disabled, false);
});
test('incomplete preview bundles never enable stale HTML or email exports', async () => {
    const t = app();
    t.context.fetch = async () => ({ ok: true, json: async () => ({ report: { sections: ['estate'] } }) });
    await t.api.previewReport();
    assert.equal(t.get('estate-download').disabled, true);
    assert.equal(t.get('estate-email-copy').disabled, true);
    assert.equal(t.get('estate-report-preview').hidden, true);
    assert.match(t.get('estate-report-status').textContent, /response is incomplete/);
});
test('a slower previous preview cannot replace the newer report selection', async () => {
    const t = app();
    let resolveFirst;
    let calls = 0;
    t.context.fetch = async () => {
        calls++;
        if (calls === 1) return new Promise(resolve => { resolveFirst = resolve; });
        return { ok: true, json: async () => ({ report: { window: { first_day: '2026-10-26' }, quality: {} }, html: '<p>New selection</p>' }) };
    };
    const first = t.api.previewReport();
    const second = t.api.previewReport();
    await second;
    resolveFirst({ ok: true, json: async () => ({ report: { window: { first_day: '2026-10-19' } }, html: '<p>Older selection</p>' }) });
    await first;
    assert.equal(t.get('estate-week-start').value, '2026-10-26');
    assert.equal(t.get('estate-report-preview').srcdoc, '<p>New selection</p>');
    assert.equal(calls, 2);
});
test('scheduled frozen report downloads retain the selected theme and display delivery failures safely', async () => {
    const t = app();
    t.context.fetch = async () => ({ ok: true, json: async () => ({ runs: [{ id: 'safe-id', week: '2026-10-19', state: 'delivery_failed', created_at: '2026-10-26T14:00:00Z', message: '<svg onload=unsafe()>Delivery uncertain</svg>' }] }) });
    await t.api.loadReportRuns();
    const row = t.get('estate-report-runs').children[0];
    assert.match(row.children[0].href, /^\/api\/estate\/report\/runs\?id=safe-id&format=html&theme=dark$/);
    assert.match(row.textContent, /delivery_failed/);
    assert.match(row.textContent, /<svg onload=unsafe\(\)>Delivery uncertain<\/svg>/);
});
test('issue observed age is bounded by the last actual observation, not wall clock time', () => {
    const t = app();
    assert.equal(t.api.observedDuration('2026-10-19T12:00:00Z', '2026-10-22T15:00:00Z'), '3d 3h');
    assert.equal(t.api.observedDuration('2026-10-22T15:00:00Z', '2026-10-19T12:00:00Z'), '—');
    assert.equal(t.api.observedDuration('', ''), '—');
});
test('settings default to all off and never populate a secret into the password field', () => {
    const t = app('estate-settings.js');
    t.api.populate({ weekly_webhook_configured: true, effective_timezone: 'America/Chicago' });
    for (const id of ['estate-inventory-enabled', 'estate-policy-enabled', 'estate-aging-enabled', 'estate-labels-enabled', 'estate-versions-enabled', 'estate-changes-enabled', 'estate-report-enabled', 'estate-schedule-enabled', 'estate-webhook-enabled']) assert.equal(t.get(id).checked, false);
    assert.equal(t.get('estate-webhook-url').value, '');
    assert.match(t.get('estate-webhook-help').textContent, /saved/);
    assert.match(t.get('estate-report-timezone').textContent, /America\/Chicago/);
    const payload = t.api.payload();
    assert.equal(Object.hasOwn(payload, 'weekly_webhook_url'), false);
    assert.equal(Object.hasOwn(payload, 'effective_timezone'), false);
});
test('settings preserve, replace, and explicitly clear webhook secrets correctly', () => {
    const t = app('estate-settings.js');
    t.api.populate({});
    t.get('estate-webhook-url').value = 'https://hooks.example.test/secret';
    assert.equal(t.api.payload().weekly_webhook_url, 'https://hooks.example.test/secret');
    t.get('estate-clear-webhook').checked = true;
    assert.equal(t.api.payload().weekly_webhook_url, '');
});
test('settings enforce opt-in delivery and explicit required label types', () => {
    const t = app('estate-settings.js');
    t.api.populate({});
    t.get('estate-schedule-enabled').checked = true;
    assert.throws(() => t.api.payload(), /Enable weekly reports/);
    t.get('estate-schedule-enabled').checked = false;
    t.get('estate-labels-enabled').checked = true;
    assert.throws(() => t.api.payload(), /required label type/);
    t.get('estate-required-labels').value = 'env, app\nenv, BU';
    assert.deepEqual(Array.from(t.api.payload().required_label_types), ['env', 'app', 'BU']);
});

async function emailPreviewApp() {
    const t = app();
    let calls = 0;
    t.context.fetch = async () => {
        calls++;
        return { ok: true, json: async () => ({ report: { window: { first_day: '2026-10-26' }, sections: ['estate'], quality: {} }, html: '<p>Browser report</p>' }) };
    };
    t.window.EstateEmail = { render(report) {
        assert.deepEqual(Array.from(report.sections), ['estate']);
        return { subject: 'Estate week of ' + report.window.first_day, html: '<table><tr><td>Safe email report</td></tr></table>', text: 'Safe email report' };
    } };
    let rendered = '';
    Object.defineProperty(t.get('estate-email-content'), 'innerHTML', { set(value) { rendered = value; }, get() { return rendered; } });
    t.get('estate-email-content').focus = () => {};
    t.get('estate-email-subject').focus = () => {};
    t.get('estate-email-subject').select = () => { t.subjectSelected = true; };
    t.document.createRange = () => ({ selectNodeContents(content) { t.selectedContent = content; } });
    t.window.getSelection = () => ({ removeAllRanges() {}, addRange() {} });
    await t.api.previewReport();
    t.fetchCalls = () => calls;
    return t;
}
test('email copy uses the loaded selection without new requests and includes rich and plain text', async () => {
    const t = await emailPreviewApp();
    assert.equal(t.get('estate-email-copy').disabled, false);
    assert.equal(t.get('estate-email-preview').disabled, false);
    let copied;
    t.window.EstateEmailCopy = { copy: async payload => { copied = payload; return true; } };
    await t.api.copyEmail();
    assert.match(copied.html, /Safe email report/);
    assert.equal(copied.text, 'Safe email report');
    assert.equal(t.fetchCalls(), 1);
    assert.match(t.get('estate-email-status').textContent, /Report copied/);
    await t.api.copyEmail(true);
    assert.equal(copied.text, 'Estate week of 2026-10-26');
    assert.equal(copied.html, undefined);
});
test('email clipboard failures reveal and select the manual report without claiming success', async () => {
    const t = await emailPreviewApp();
    t.window.EstateEmailCopy = { copy: async () => { throw new Error('Clipboard denied'); } };
    await t.api.copyEmail();
    assert.equal(t.get('estate-email-panel').hidden, false);
    assert.equal(t.get('estate-email-panel').open, true);
    assert.match(t.get('estate-email-content').innerHTML, /Safe email report/);
    assert.equal(t.selectedContent, t.get('estate-email-content'));
    assert.match(t.get('estate-email-status').textContent, /Report selected/);
    assert.doesNotMatch(t.get('estate-email-status').textContent, /Report copied/);
    await t.api.copyEmail(true);
    assert.equal(t.subjectSelected, true);
    assert.match(t.get('estate-email-status').textContent, /Automatic copying is blocked/);
});
test('new report selection invalidates email content and ignores delayed copy completion', async () => {
    const t = await emailPreviewApp();
    let finish;
    t.window.EstateEmailCopy = { copy: () => new Promise(resolve => { finish = resolve; }) };
    t.api.previewEmail();
    const pending = t.api.copyEmail(true);
    t.api.invalidateReport();
    finish(true);
    await pending;
    assert.equal(t.get('estate-email-copy').disabled, true);
    assert.equal(t.get('estate-email-preview').disabled, true);
    assert.equal(t.get('estate-email-panel').hidden, true);
    assert.equal(t.get('estate-email-subject').value, '');
    assert.equal(t.get('estate-email-status').textContent, '');
    assert.equal(t.api.previewEmail(), false);
    await t.api.previewReport();
    assert.equal(t.get('estate-email-copy-subject').disabled, false);
});
test('email renderer failure leaves ordinary HTML and PDF exports working', async () => {
    const t = await emailPreviewApp();
    t.window.EstateEmail.render = () => { throw new Error('Malformed report'); };
    await t.api.previewReport();
    assert.equal(t.get('estate-email-copy').disabled, true);
    assert.equal(t.get('estate-download').disabled, false);
    assert.equal(t.get('estate-print').disabled, false);
    assert.match(t.get('estate-email-status').textContent, /email layout could not be prepared/);
});
test('disabling weekly reporting clears loaded and in-flight email exports until a new preview', async () => {
    const t = await emailPreviewApp();
    t.api.previewEmail();
    t.api.renderPage({ options: { weekly_report_enabled: false } });
    t.api.renderPage({ options: { weekly_report_enabled: true } });
    assert.equal(t.get('estate-email-copy').disabled, true);
    assert.equal(t.get('estate-email-panel').hidden, true);
    assert.equal(t.api.previewEmail(), false);
    let resolveMetadata;
    t.context.fetch = () => new Promise(resolve => { resolveMetadata = resolve; });
    const pending = t.api.previewReport();
    t.api.renderPage({ options: { weekly_report_enabled: false } });
    resolveMetadata({ ok: true, json: async () => ({ report: { sections: ['estate'], window: { first_day: '2026-10-26' } }, html: '<p>Disabled</p>' }) });
    await pending;
    t.api.renderPage({ options: { weekly_report_enabled: true } });
    assert.equal(t.get('estate-email-copy').disabled, true);
    assert.equal(t.get('estate-download').disabled, true);
    assert.equal(t.api.previewEmail(), false);
});

test('pending policy counts describe actual added/modified/deleted objects, not full draft inventory', () => {
    const t = app();
    const policy = { active: { complete: true, rules: 800, rulesets: 80 }, draft: { complete: true, rules: 806, rulesets: 82 }, pending_complete: true,
        pending_counts: { rules: { complete: true, added: 8, modified: 3, deleted: 2, total: 13 }, rulesets: { complete: true, added: 2, modified: 1, deleted: 0, total: 3 } },
        pending: [{ kind: 'rule', name: '<img src=x onerror=unsafe()>', operation: 'modified' }] };
    const view = { options: { policy_enabled: true }, snapshot: { policy } };
    t.api.renderDashboard(view);
    const cards = t.get('estate-dashboard-content').children;
    const card = label => cards.find(item => item.children[0].textContent === label);
    assert.match(card('Draft policy inventory').textContent, /806 rules82 rulesets.*not pending changes/);
    assert.equal(card('Pending rules').children[1].textContent, '13');
    assert.match(card('Pending rules').textContent, /Added: 8.*Modified: 3.*Deleted: 2/);
    assert.equal(card('Pending rulesets').children[1].textContent, '3');
    t.api.renderPage(view);
    assert.match(t.get('estate-live-sections').textContent, /Pending rules13Added: 8/);
    assert.match(t.get('estate-live-sections').textContent, /<img src=x onerror=unsafe\(\)>/);
});

test('unknown and incomplete pending counts never become zero; confirmed zero remains valid', () => {
    const t = app();
    for (const counts of [undefined, {}, { rules: { complete: false, total: 0, added: 0, modified: 0, deleted: 0 } }, { rules: { complete: true, total: 0, added: null, modified: 0, deleted: 0 } }]) {
        t.api.renderDashboard({ options: { policy_enabled: true }, snapshot: { policy: { draft: { complete: true, rules: 700, rulesets: 50 }, pending_counts: counts } } });
        const card = t.get('estate-dashboard-content').children.find(item => item.children[0].textContent === 'Pending rules');
        assert.equal(card.children[1].textContent, 'Unavailable / incomplete');
    }
    const zero = { complete: true, total: 0, added: 0, modified: 0, deleted: 0 };
    t.api.renderPage({ options: { policy_enabled: true }, snapshot: { policy: { warnings: ['<svg>Assessment warning</svg>'], pending_counts: { rules: zero, rulesets: zero, warnings: ['<svg>Assessment warning</svg>'] }, pending_complete: true } } });
    assert.match(t.get('estate-live-sections').textContent, /Pending rules0Added: 0.*Pending rulesets0/);
    assert.match(t.get('estate-live-sections').textContent, /<svg>Assessment warning<\/svg>/);
    assert.equal(t.get('estate-live-sections').textContent.split('<svg>Assessment warning</svg>').length - 1, 1);
});

test('an explicitly selected on-demand baseline is not labelled a weekly baseline', () => {
    const t = app();
    t.api.renderDashboard({ options: { change_log_enabled: true }, snapshot: {}, changes_since_report: {
        baseline_kind: 'on_demand', report_id: 'now-123', observation_at: '2026-11-04T16:00:00Z', since: '2026-11-04T16:00:00Z', report_week: '2026-11-02', status: 'available', count: 4
    } });
    const text = t.get('estate-dashboard-content').textContent;
    assert.match(text, /Changes since on-demand report baseline4/);
    assert.match(text, /Explicitly selected on-demand baseline/);
    assert.doesNotMatch(text, /weekly report|Report week/);
});

function onDemandBundle(id = 'now-1', baseline = false) {
    return { report: { id, kind: 'on_demand', timezone: 'America/Chicago', observation_at: '2026-11-04T16:00:00Z', generated_at: '2026-11-04T16:05:00Z', sets_change_baseline: baseline, partial_week: true,
        window: { first_day: '2026-11-02', last_day: '2026-11-04' }, sections: ['estate', 'policy'], quality: { expected_days: 3, comparison_available: false, warnings: ['Latest observation is retained data.'] } }, html: '<p>Frozen ' + id + '</p>' };
}
function reportFetch(t, handler) {
    const calls = [];
    t.context.fetch = async (url, options = {}) => {
        calls.push({ url, options });
        if (url === '/api/estate') return { ok: true, json: async () => ({ options: { weekly_report_enabled: true }, snapshot: {} }) };
        if (url === '/api/estate/report/runs') return { ok: true, json: async () => ({ runs: [] }) };
        return handler(url, options);
    };
    return calls;
}
test('Run report now accepts a partial current week, preserves completed-week selection, and defaults baseline off', async () => {
    const t = app();
    t.get('estate-week-start').value = '2026-11-02'; // Not a completed week: irrelevant to this action.
    t.sections.forEach(input => { input.checked = ['estate', 'policy'].includes(input.value); });
    const calls = reportFetch(t, async () => ({ ok: true, json: async () => onDemandBundle() }));
    await t.api.runReportNow();
    const url = new URL(calls[0].url, 'http://localhost');
    assert.equal(url.pathname, '/api/estate/report/now');
    assert.equal(calls[0].options.method, 'POST');
    assert.equal(url.searchParams.get('set_baseline'), 'false');
    assert.equal(url.searchParams.get('sections'), 'estate,policy');
    assert.equal(url.searchParams.get('theme'), 'dark');
    assert.equal(url.searchParams.has('week'), false);
    assert.equal(t.get('estate-week-start').value, '2026-11-02');
    assert.equal(t.get('estate-download').disabled, false);
    assert.match(t.get('estate-report-status').textContent, /Saved on-demand report.*As of.*Current partial week.*3 elapsed reporting days/);
    assert.match(t.get('estate-report-status').textContent, /future days are not missing data.*comparison is unavailable.*baseline unchanged/);
    assert.equal(calls.some(call => call.url === '/api/estate/report/runs'), true);
    assert.equal(t.api.reportFilename('on_demand', '2026-11-04T16:00:00Z', '2026-11-02', 'now-1'), 'Illumio_Estate_On_Demand_2026-11-04T16-00-00Z_now-1.html');
    assert.equal(t.get('estate-run-now').disabled, false);
});

test('baseline opt-in applies only to an explicit run and each new run is a new POST', async () => {
    const t = app();
    let number = 0;
    const calls = reportFetch(t, async url => ({ ok: true, json: async () => onDemandBundle('now-' + ++number, new URL(url, 'http://localhost').searchParams.get('set_baseline') === 'true') }));
    t.get('estate-set-baseline').checked = true;
    await t.api.runReportNow();
    assert.equal(new URL(calls[0].url, 'http://localhost').searchParams.get('set_baseline'), 'true');
    assert.equal(t.get('estate-set-baseline').checked, false);
    assert.match(t.get('estate-report-status').textContent, /Explicitly selected as a change baseline.*newer baseline is kept/);
    await t.api.runReportNow();
    const posts = calls.filter(call => call.options.method === 'POST');
    assert.equal(posts.length, 2);
    assert.equal(new URL(posts[1].url, 'http://localhost').searchParams.get('set_baseline'), 'false');
    assert.equal(t.get('estate-report-preview').srcdoc, '<p>Frozen now-2</p>');
});

test('theme changes reopen the same frozen on-demand report with GET and never reset the baseline', async () => {
    const t = app();
    const bundle = onDemandBundle('saved-now', true);
    const calls = reportFetch(t, async () => ({ ok: true, json: async () => bundle }));
    t.get('estate-set-baseline').checked = true;
    await t.api.runReportNow();
    t.document.documentElement.dataset.unifiedTheme = 'light';
    await t.api.refreshReportTheme();
    const last = calls.at(-1), url = new URL(last.url, 'http://localhost');
    assert.equal(last.options.method, 'GET');
    assert.equal(url.pathname, '/api/estate/report/runs');
    assert.equal(url.searchParams.get('id'), 'saved-now');
    assert.equal(url.searchParams.get('format'), 'preview');
    assert.equal(url.searchParams.get('theme'), 'light');
    assert.equal(url.searchParams.has('set_baseline'), false);
    assert.equal(url.searchParams.has('week'), false);
    assert.equal(calls.filter(call => call.options.method === 'POST').length, 1);
    assert.equal(t.get('estate-report-preview').srcdoc, bundle.html);
});

test('a theme change during report creation repaints the saved ID once without a second POST', async () => {
    const t = app();
    let finish;
    const calls = reportFetch(t, async (url, options) => options.method === 'POST' ? new Promise(resolve => { finish = resolve; }) : { ok: true, json: async () => onDemandBundle('in-flight') });
    const pending = t.api.runReportNow();
    t.document.documentElement.dataset.unifiedTheme = 'light';
    await t.api.refreshReportTheme();
    assert.equal(calls.length, 1);
    finish({ ok: true, json: async () => onDemandBundle('in-flight') });
    await pending;
    const requests = calls.filter(call => call.options.method);
    assert.deepEqual(requests.map(call => call.options.method), ['POST', 'GET']);
    assert.match(requests[1].url, /id=in-flight&format=preview&theme=light/);
    assert.equal(t.get('estate-run-now').disabled, false);
});

test('pending save prevents duplicate runs and stale responses cannot replace a changed selection', async () => {
    const t = app();
    let finish;
    const calls = reportFetch(t, async () => new Promise(resolve => { finish = resolve; }));
    t.get('estate-set-baseline').checked = true;
    const pending = t.api.runReportNow();
    assert.equal(t.get('estate-set-baseline').checked, false);
    assert.equal(t.get('estate-run-now').disabled, true);
    t.api.invalidateReport();
    await t.api.runReportNow();
    assert.equal(calls.filter(call => call.options.method === 'POST').length, 1);
    finish({ ok: true, json: async () => onDemandBundle('stale') });
    await pending;
    assert.equal(t.get('estate-report-preview').hidden, true);
    assert.equal(t.get('estate-download').disabled, true);
    assert.equal(t.get('estate-run-now').disabled, false);
    assert.doesNotMatch(t.get('estate-report-status').textContent, /Saved on-demand report/);
});

test('failed or malformed on-demand responses never enable old exports or claim the baseline changed', async () => {
    for (const response of [
        { ok: false, status: 503, text: async () => 'No current observation available.' },
        { ok: true, json: async () => ({ report: { kind: 'on_demand' }, html: '<p>Missing identity</p>' }) }
    ]) {
        const t = app();
        reportFetch(t, async () => ({ ok: true, json: async () => onDemandBundle() }));
        await t.api.runReportNow();
        reportFetch(t, async () => response);
        await t.api.runReportNow();
        assert.equal(t.get('estate-download').disabled, true);
        assert.equal(t.get('estate-report-preview').hidden, true);
        assert.equal(t.get('estate-email-copy').disabled, true);
        assert.equal(t.get('estate-run-now').disabled, false);
        assert.match(t.get('estate-report-status').textContent, /Unable to run report now/);
        assert.doesNotMatch(t.get('estate-report-status').textContent, /Saved on-demand report|new change baseline/);
    }
});

test('a frozen report identity mismatch is rejected instead of silently substituting another report', async () => {
    const t = app();
    reportFetch(t, async () => ({ ok: true, json: async () => onDemandBundle('correct') }));
    await t.api.runReportNow();
    reportFetch(t, async () => ({ ok: true, json: async () => onDemandBundle('different') }));
    await t.api.refreshReportTheme();
    assert.match(t.get('estate-report-status').textContent, /did not match/);
    assert.equal(t.get('estate-download').disabled, true);
});

test('saved on-demand reports have distinct names and the baseline control starts unchecked', async () => {
    const t = app();
    t.context.fetch = async () => ({ ok: true, json: async () => ({ runs: [{ ...onDemandBundle('now-list').report, created_at: '2026-11-04T16:05:00Z', state: 'generated', week: '2026-11-02' }] }) });
    await t.api.loadReportRuns();
    const row = t.get('estate-report-runs').children[0];
    assert.match(row.children[0].textContent, /Download on-demand report/);
    assert.match(row.children[0].download, /Estate_On_Demand_.*now-list/);
    assert.match(row.textContent, /Change baseline unchanged/);
    const html = fs.readFileSync(path.join(repo, 'estate.html'), 'utf8');
    const checkbox = html.match(/<input[^>]+id="estate-set-baseline"[^>]*>/)[0];
    assert.doesNotMatch(checkbox, /\bchecked\b/);
    assert.match(html, /id="estate-run-now"[^>]*>Run report now/);
    assert.match(html, /id="estate-preview"/);
    assert.match(html, /id="estate-save-snapshot"/);
});
