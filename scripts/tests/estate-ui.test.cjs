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
        addEventListener(event, callback) { this.listeners[event] = callback; },
        remove() {}, click() {}
    };
}
function app(file = 'estate.js') {
    const elements = new Map();
    const get = id => { if (!elements.has(id)) elements.set(id, element()); return elements.get(id); };
    const sections = ['estate', 'health', 'policy', 'traffic', 'changes', 'quality'].map(value => ({ ...element('input'), value, checked: true }));
    const document = {
        readyState: 'loading', body: element('body'), documentElement: { dataset: { unifiedTheme: 'dark' } },
        getElementById: get, createElement: element, addEventListener() {},
        querySelectorAll(selector) { return selector.endsWith(':checked') ? sections.filter(input => input.checked) : sections; }
    };
    const window = { location: { search: '' } };
    const context = vm.createContext({ document, window, URL, URLSearchParams, Date, Blob, console, setTimeout() {}, setInterval() {} });
    vm.runInContext(fs.readFileSync(path.join(repo, 'static', file), 'utf8'), context);
    return { context, window, document, get, sections, api: file === 'estate.js' ? window.EstateUI : window.EstateSettings };
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
