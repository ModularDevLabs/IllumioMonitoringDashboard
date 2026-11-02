'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../../static/estate-csv.js'), 'utf8');
function app(window = {}) {
    vm.runInContext(source, vm.createContext({ window }));
    return window.EstateCSV;
}
// Parse RFC4180 cells, including multiline quoted cells, to assert actual values
// rather than assuming that one line always equals one exported record.
function parse(csv) {
    const text = csv.replace(/^\uFEFF/, '');
    const rows = []; let row = [], value = '', quoted = false;
    for (let i = 0; i < text.length; i++) {
        const ch = text[i];
        if (quoted) {
            if (ch === '"' && text[i + 1] === '"') { value += '"'; i++; }
            else if (ch === '"') quoted = false;
            else value += ch;
        } else if (ch === '"') quoted = true;
        else if (ch === ',') { row.push(value); value = ''; }
        else if (ch === '\r' && text[i + 1] === '\n') { row.push(value); rows.push(row); row = []; value = ''; i++; }
        else value += ch;
    }
    assert.equal(quoted, false);
    assert.deepEqual(row, []);
    assert.equal(value, '');
    return rows;
}
function snapshot() {
    return {
        at: '2026-11-02T09:30:00-06:00', inventory_complete: true, labels_complete: true, labels_assessed: 205, total: 205,
        health_complete: { warning: true, error: true, stopped: true, suspended: true },
        label_gaps: Array.from({ length: 205 }, (_, n) => ({ id: '/workloads/' + n, name: 'Workload ' + n, missing: ['app', 'BU'] }))
    };
}
function issue(n = 0) {
    return { id: 'warning:/workloads/' + n, workload_id: '/workloads/' + n, name: 'Workload ' + n, kind: 'warning', active: true,
        first_seen: '2026-10-31T12:15:00Z', last_seen: '2026-11-02T15:30:00Z', occurrences: 1000, observed_days: ['2026-10-31', '2026-11-01', '2026-11-02'] };
}
test('CSV is UTF-8 BOM + CRLF, preserves commas/quotes/newlines, and keeps numbers unformatted', () => {
    const csv = app().serialize(['Name', 'Total', 'Other'], [['Comma, "quote"\nnext line', 12000, null], ['Unicode • 世', -42.5, 0]]);
    assert.equal(csv.charCodeAt(0), 0xfeff);
    assert.equal(csv.endsWith('\r\n'), true);
    assert.match(csv, /"Comma, ""quote""\nnext line",12000,\r\n/);
    assert.deepEqual(parse(csv), [['Name', 'Total', 'Other'], ['Comma, "quote"\nnext line', '12000', ''], ['Unicode • 世', '-42.5', '0']]);
});
test('Excel formulas are neutralized in every textual cell, including whitespace/control prefixes', () => {
    const inputs = ['=SUM(1,2)', '+1+2', '-1+2', '@SUM(A1)', ' =SUM(A1)', '\t=SUM(A1)', '\r=SUM(A1)', '\n=SUM(A1)', '\u00a0=SUM(A1)', '\uFEFF=SUM(A1)', '\u0000=SUM(A1)'];
    const result = parse(app().serialize(['Value'], inputs.map(value => [value]))).slice(1).flat();
    assert.deepEqual(result, inputs.map(value => "'" + value));
    assert.deepEqual(parse(app().serialize(['Number', '=Header'], [[-1, '=1+1'], [0, '@evil']]))[1], ['-1', "'=1+1"]);
});
test('UTC conversion requires a valid explicit timezone and omits unknown dates and durations', () => {
    const api = app();
    assert.equal(api.utc('2026-11-02T09:30:00-06:00'), '2026-11-02T15:30:00.000Z');
    assert.equal(api.utc('2026-11-02T15:30:00Z'), '2026-11-02T15:30:00.000Z');
    for (const missing of [null, undefined, '', '0001-01-01T00:00:00Z', 'invalid', '2026-11-02', '2026-11-02T09:30:00']) assert.equal(api.utc(missing), '');
    assert.equal(api.ageHours('2026-11-02T12:00:00Z', '2026-11-02T15:30:00Z'), 3.5);
    assert.equal(api.ageHours('', '2026-11-02T15:30:00Z'), null);
    assert.equal(api.ageHours('2026-11-02T15:30:00Z', '2026-11-02T12:00:00Z'), null);
});
test('label export includes all pages plus requirements, identity, assessment, and UTC observation', () => {
    const view = { options: { label_hygiene_enabled: true, required_label_types: ['app', 'BU'] }, snapshot: snapshot() };
    view.snapshot.label_gaps[0].name = '=HYPERLINK("https://invalid.test")';
    const result = app().labelHygiene(view);
    assert.equal(result.available, true);
    assert.equal(result.rowCount, 205);
    assert.equal(result.partial, false);
    assert.equal(result.filename, 'Illumio_Label_Hygiene_2026-11-02.csv');
    const rows = parse(result.content);
    assert.equal(rows.length, 206);
    assert.equal(rows.at(-1)[1], '/workloads/204');
    assert.equal(rows[1][0], "'=HYPERLINK(\"https://invalid.test\")");
    assert.deepEqual(rows[1].slice(1), ['/workloads/0', 'app; BU', 'app; BU', 'Complete assessment', '205', '205', 'true', 'true', '2026-11-02T15:30:00.000Z']);
    const metadata = app().labelHygiene(view, { includeContent: false });
    assert.equal(metadata.rowCount, 205);
    assert.equal(metadata.content, '');
});
test('issue export includes every retained observation and raw numeric age/counts, not just active rows', () => {
    const view = { options: { issue_aging_enabled: true }, snapshot: snapshot(), issues: Array.from({ length: 205 }, (_, n) => issue(n)) };
    view.issues[0].active = false; view.issues[0].resolved_at = '2026-11-02T15:45:00Z';
    const result = app().issues(view);
    assert.equal(result.available, true);
    assert.equal(result.rowCount, 205);
    assert.equal(result.partial, false);
    const rows = parse(result.content);
    assert.equal(rows.length, 206);
    assert.equal(rows.at(-1)[1], '/workloads/204');
    assert.deepEqual(rows[1].slice(0, 13), ['Workload 0', '/workloads/0', 'warning:/workloads/0', 'warning', 'Resolved', '2026-10-31T12:15:00.000Z', '2026-11-02T15:30:00.000Z', '2026-11-02T15:45:00.000Z', '51.25', '1000', '3', '2026-10-31; 2026-11-01; 2026-11-02', 'true']);
    assert.equal(rows[2][7], '');
    assert.match(rows[1][13], /^Complete current collection/);
    assert.equal(app().issues(view, { includeContent: false }).content, '');
});
test('partial results are marked in the filename and every row; unknown metrics stay blank, not zero', () => {
    const view = { options: { label_hygiene_enabled: true, required_label_types: ['app'], issue_aging_enabled: true }, snapshot: snapshot(), issues: [issue()] };
    view.snapshot.inventory_complete = false; view.snapshot.labels_complete = false;
    view.snapshot.labels_assessed = 1; delete view.snapshot.total;
    view.snapshot.label_gaps = [view.snapshot.label_gaps[0]];
    delete view.snapshot.health_complete.error;
    const labels = app().labelHygiene(view);
    assert.equal(labels.partial, true);
    assert.match(labels.filename, /_partial\.csv$/);
    assert.match(parse(labels.content)[1][4], /^Partial observed results/);
    assert.equal(parse(labels.content)[1][6], '');
    view.issues[0] = { name: 'Incomplete observation', kind: 'error' };
    const issues = app().issues(view);
    assert.equal(issues.partial, true);
    assert.match(issues.filename, /_partial\.csv$/);
    const row = parse(issues.content)[1];
    assert.deepEqual(row.slice(5, 13), ['', '', '', '', '', '', '', '']);
    assert.match(row[13], /^Partial collection/);
    assert.equal(row[4], 'Unknown');
});
test('disabled, empty, or unassessed sources cannot produce a misleading blank export', () => {
    const api = app();
    assert.equal(api.issues({}).available, false);
    assert.equal(api.labelHygiene({}).available, false);
    const view = { options: { issue_aging_enabled: true, label_hygiene_enabled: true, required_label_types: ['app'] } };
    assert.match(api.issues(view).reason, /unavailable/);
    assert.match(api.labelHygiene(view).reason, /unavailable/);
    view.snapshot = snapshot(); view.snapshot.label_gaps = []; view.issues = [];
    assert.match(api.labelHygiene(view).reason, /no missing-label rows/);
    assert.match(api.issues(view).reason, /no observed issue rows/);
    view.snapshot.label_gaps = [{ id: 'stale' }]; view.snapshot.labels_complete = false; view.snapshot.labels_assessed = 0;
    assert.equal(api.labelHygiene(view).available, false);
    view.options.required_label_types = [];
    assert.match(api.labelHygiene(view).reason, /Choose required label types/);
});
test('large exports have no presentation page cap', () => {
    const view = { options: { issue_aging_enabled: true }, snapshot: snapshot(), issues: Array.from({ length: 10050 }, (_, n) => issue(n)) };
    const result = app().issues(view);
    assert.equal(result.rowCount, 10050);
    assert.equal(parse(result.content).length, 10051);
    assert.match(result.content, /warning:\/workloads\/10049/);
});
test('local download uses a CSV Blob, cleans up its link and revokes its URL', async () => {
    let blob, clicked = false, removed = false, revoked;
    const link = { click() { clicked = true; }, remove() { removed = true; } };
    const api = app({ Blob, document: { body: { appendChild() {} }, createElement() { return link; } }, URL: { createObjectURL(value) { blob = value; return 'blob:csv'; }, revokeObjectURL(url) { revoked = url; } }, setTimeout(callback) { callback(); } });
    assert.equal(api.download({ available: true, content: '\uFEFFName\r\nOne\r\n', filename: 'export.csv' }), true);
    assert.equal(blob.type, 'text/csv;charset=utf-8');
    assert.equal(await blob.text(), 'Name\r\nOne\r\n');
    assert.equal(new Uint8Array(await blob.arrayBuffer())[0], 0xef);
    assert.equal(clicked, true); assert.equal(removed, true); assert.equal(revoked, 'blob:csv');
    assert.equal(link.download, 'export.csv');
    assert.equal(api.download({ available: false }), false);
    assert.equal(app().download({ available: true, content: 'x' }), false);
});
