const assert = require('node:assert/strict');
const { existsSync, readFileSync } = require('node:fs');
const path = require('node:path');
const { test } = require('node:test');
const vm = require('node:vm');

const repo = path.join(__dirname, '../..');
const frontend = existsSync(path.join(repo, 'frontend/summary.html')) ? path.join(repo, 'frontend') : path.join(repo, 'internal/extractor/frontend');
const { createStore } = require(path.join(frontend, 'analysis-state.js'));
const source = page => readFileSync(path.join(frontend, `${page}.html`), 'utf8');
const inline = page => [...source(page).matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].find(match => match[1].includes(page === 'summary' ? 'function renderDashboard' : 'function renderExecutiveSummary'))[1];
const plain = value => JSON.parse(JSON.stringify(value));

function storage() {
  const records = new Map();
  return { getItem: key => records.get(key) || null, setItem: (key, value) => records.set(key, value) };
}

function element(id = '') {
  const classes = new Set();
  let value = '';
  let html = '';
  return {
    id, options: [], dataset: {}, style: {}, textContent: '', innerText: '', checked: false,
    classList: { contains: key => classes.has(key), add: (...keys) => keys.forEach(key => classes.add(key)), remove: (...keys) => keys.forEach(key => classes.delete(key)), toggle: (key, enabled) => { if (enabled) classes.add(key); else classes.delete(key); } },
    get value() { return value; },
    set value(next) { value = next; if (this.options.length) this.options.forEach(option => { option.selected = option.value === next; }); },
    get selectedOptions() { return this.options.filter(option => option.selected); },
    get innerHTML() { return html; },
    set innerHTML(next) { html = next; if (next === '') this.options = []; },
    replaceChildren(...children) { this.options = [...children]; },
    appendChild(child) { this.options.push(child); return child; },
    addEventListener(name, callback) { this.listeners ||= {}; this.listeners[name] = callback; },
    querySelector() { return null; },
  };
}

function harness(page, backing = storage(), embedded) {
  const elements = new Map();
  const get = id => {
    if (embedded && ['reportTitleInput', 'reportCustomerInput', 'reportPreparedInput', 'reportNotesInput', 'reportLogoInput', 'reportSectionChoices'].includes(id)) return null;
    if (!elements.has(id)) elements.set(id, element(id));
    return elements.get(id);
  };
  const sections = ['coverage', 'service-trends'].map(id => ({ dataset: { exportSection: id, collapseTitle: id } }));
  const choices = sections.map(section => ({ dataset: { reportSection: section.dataset.exportSection }, checked: true }));
  const collapse = element(); collapse.dataset.collapsible = 'overview';
  const collapseContent = element(); const collapseLabel = element(); const collapseToggle = element();
  collapse.querySelector = selector => ({ '[data-collapsible-content]': collapseContent, '[data-collapse-label]': collapseLabel, '[data-collapse-toggle]': collapseToggle })[selector] || null;
  const store = createStore({ storage: backing });
  const window = { ITTAnalysisState: store, ITTSections: { initialize() {} }, addEventListener() {}, __ITT_EXECUTIVE_PAYLOAD__: embedded?.payload, __ITT_EXECUTIVE_VIEW_STATE__: embedded?.state };
  const body = { dataset: { theme: 'illumio-light' }, cloneNode: () => ({ dataset: {}, classList: { remove() {}, add() {} }, querySelectorAll: () => [], outerHTML: '<body></body>' }) };
  const document = {
    body, getElementById: get,
    querySelectorAll: selector => selector === '[data-collapsible]' ? [collapse] : selector === '[data-export-section]' ? sections : selector === '#reportSectionChoices input[data-report-section]' ? (embedded ? [] : choices) : [],
    createElement: () => element(), scripts: [{ src: '', textContent: inline(page) }],
  };
  const context = vm.createContext({ window, document, localStorage: storage(), Intl, Date, console, structuredClone, Blob, Option: function(label, value) { return { textContent: label, value, selected: false }; }, fetch: async () => ({ text: async () => '' }) });
  vm.runInContext(inline(page), context);
  if (page === 'executive-summary') {
    vm.runInContext(`
      applyDimensionLabels = renderExecutiveCoverage = renderFindings = renderMonthlyTrendCards = renderRiskyServices = renderPersistentServices = renderLatestMonthChanges = renderCrossTalk = renderExternalSpotlight = renderEnvironmentScorecards = renderTrendCharts = renderPeriodComparison = () => {};
      trendMonthInfo = payload => (payload.testMonths || ['2026-01', '2026-02', '2026-03']).map(month => ({month, covered: true}));
      downloadBlob = blob => { window.downloadedBlob = blob; };
    `, context);
  }
  return { context, window, store, get, choices, collapse, collapseToggle, collapseContent, backing, document };
}

const payload = (revision = 'analysis-1') => ({
  analysisRevision: revision, fileName: 'same-file.csv', summary: [{ protocol: 'TCP', port: 443, flow_count: 10, unique_connections: 1 }],
  report_metadata: { title: 'Server report', customer_name: 'Saved customer' },
  insights: {
    monthly_port_protocol: ['443', '9300', '22'].map((port, index) => ({ month: '2026-03', protocol: 'TCP', port, flow_count: 30 - index, unique_connections: 1 })),
    monthly_relationships: ['Production', 'Development'].map(source => ({ month: '2026-03', source, destination: 'Database', flow_count: 10 })),
  },
});

test('analysis pages load revision-scoped state before collapsible enhancement and contain valid scripts', () => {
  for (const page of ['summary', 'executive-summary']) {
    const html = source(page);
    assert.ok(html.indexOf('/assets/analysis-state.js') < html.indexOf('/assets/collapsible.js'));
    new vm.Script(inline(page));
    assert.match(html, /event\.persisted/);
  }
  assert.doesNotMatch(source('summary'), /localStorage\.(?:get|set)Item\(`ittCollapse/);
});

test('summary pivot selection survives navigation and refresh but resets on a new analysis', () => {
  const first = harness('summary');
  first.context.activateSummaryState(payload());
  first.context.renderPivotSourceEnvOptions(['Production', 'Development']);
  first.get('pivotSourceEnvSelect').options[1].selected = true;
  first.context.saveSummaryState();
  const returned = harness('summary', first.backing);
  returned.context.activateSummaryState(payload());
  returned.context.renderPivotSourceEnvOptions(['Production', 'Development']);
  assert.deepEqual(plain(returned.context.getSelectedPivotSourceEnvs()), ['Development']);
  returned.context.activateSummaryState(payload());
  returned.context.renderPivotSourceEnvOptions(['Production', 'Development']);
  assert.deepEqual(plain(returned.context.getSelectedPivotSourceEnvs()), ['Development']);
  returned.context.activateSummaryState(payload('analysis-2'));
  returned.context.renderPivotSourceEnvOptions(['Production', 'Development']);
  assert.deepEqual(plain(returned.context.getSelectedPivotSourceEnvs()), []);
});

test('summary collapse state belongs to the loaded analysis, not all future datasets', () => {
  const first = harness('summary');
  first.context.activateSummaryState(payload());
  first.collapseToggle.listeners.click();
  assert.equal(first.collapse.classList.contains('is-collapsed'), true);
  const returned = harness('summary', first.backing);
  returned.context.activateSummaryState(payload());
  assert.equal(returned.collapse.classList.contains('is-collapsed'), true);
  returned.context.activateSummaryState(payload('analysis-2'));
  assert.equal(returned.collapse.classList.contains('is-collapsed'), false);
});

test('executive filters, empty service selection, comparison months and drafts survive navigation and refresh', () => {
  const first = harness('executive-summary');
  first.context.renderExecutiveSummary(payload());
  first.get('trendRangeSelect').value = 'all';
  first.get('serviceTrendSelect').options.forEach(option => { option.selected = false; });
  first.get('relationshipTrendSelect').options.forEach(option => { option.selected = option.value.startsWith('Development'); });
  first.get('comparisonStartMonth').value = '2026-01';
  first.get('comparisonEndMonth').value = '2026-02';
  first.get('reportTitleInput').value = '';
  first.get('reportNotesInput').value = ' Work in progress ';
  first.context.setReportSectionSelection([]);
  first.context.saveExecutiveState();
  const returned = harness('executive-summary', first.backing);
  for (let refresh = 0; refresh < 2; refresh++) {
    returned.context.renderExecutiveSummary(payload());
    assert.equal(returned.get('trendRangeSelect').value, 'all');
    assert.equal(returned.get('serviceTrendSelect').selectedOptions.length, 0);
    assert.deepEqual(returned.get('relationshipTrendSelect').selectedOptions.map(option => option.value), ['Development → Database']);
    assert.equal(returned.get('comparisonStartMonth').value, '2026-01');
    assert.equal(returned.get('comparisonEndMonth').value, '2026-02');
    assert.equal(returned.get('reportTitleInput').value, '');
    assert.equal(returned.get('reportNotesInput').value, ' Work in progress ');
    assert.deepEqual(plain(returned.context.selectedReportSectionIDs()), []);
  }
});

test('executive new analysis restores default charts and dataset metadata instead of old drafts', () => {
  const app = harness('executive-summary');
  app.context.renderExecutiveSummary(payload());
  app.get('trendRangeSelect').value = '12';
  app.get('serviceTrendSelect').options.forEach(option => { option.selected = false; });
  app.get('reportTitleInput').value = 'Unsaved edit';
  app.context.setReportSectionSelection([]);
  app.context.saveExecutiveState();
  app.context.renderExecutiveSummary(payload('analysis-2'));
  assert.equal(app.get('trendRangeSelect').value, '6');
  assert.equal(app.get('serviceTrendSelect').selectedOptions.length, 3);
  assert.equal(app.get('reportTitleInput').value, 'Server report');
  assert.equal(app.get('comparisonStartMonth').value, '2026-02');
  assert.equal(app.get('comparisonEndMonth').value, '2026-03');
  assert.deepEqual(plain(app.context.selectedReportSectionIDs()), ['coverage', 'service-trends']);
});

test('executive preserves explicit choices even when saved values are absent from available options', () => {
  const app = harness('executive-summary');
  app.context.renderExecutiveSummary(payload());
  app.context.populateMultiSelect('serviceTrendSelect', [{ value: 'TCP:443', label: 'TLS' }], 5, ['TCP:9999']);
  assert.equal(app.get('serviceTrendSelect').selectedOptions.length, 0);
  app.context.populateMultiSelect('serviceTrendSelect', [{ value: 'TCP:443', label: 'TLS' }], 5);
  assert.equal(app.get('serviceTrendSelect').selectedOptions.length, 1);
});

test('offline executive view restores embedded selections without touching live analysis state', () => {
  const data = payload();
  const app = harness('executive-summary', storage(), { payload: data, state: { trendRange: '12', services: [], relationships: [], comparisonStart: '2026-01', comparisonEnd: '2026-03', reportDraft: { title: 'Exported draft', included_sections: ['coverage'] } } });
  app.window.ITTAnalysisState = { activate: () => assert.fail('Offline reports must not activate live state'), read: () => assert.fail('Offline reports must not read live state'), write: () => assert.fail('Offline reports must not write live state') };
  app.context.renderExecutiveSummary(data);
  assert.equal(app.get('trendRangeSelect').value, '12');
  assert.equal(app.get('serviceTrendSelect').selectedOptions.length, 0);
  assert.equal(app.get('relationshipTrendSelect').selectedOptions.length, 0);
  assert.equal(app.get('reportTitle').textContent, 'Exported draft');
  assert.deepEqual(plain(app.context.selectedReportSectionIDs()), ['coverage']);
});

test('HTML exports embed current filters and unsaved report edits safely', async () => {
  const app = harness('executive-summary');
  app.context.renderExecutiveSummary(payload());
  app.get('trendRangeSelect').value = 'all';
  app.get('serviceTrendSelect').options.forEach(option => { option.selected = false; });
  app.get('reportTitleInput').value = 'Draft </script> title';
  app.get('reportNotesInput').value = 'Unsaved notes';
  app.context.setReportSectionSelection(['coverage']);
  app.context.previewReportMetadata();
  await app.context.downloadExecutiveHTML();
  const html = await app.window.downloadedBlob.text();
  const scripts = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)];
  assert.equal(scripts.length, 3);
  for (const script of scripts) new vm.Script(script[1]);
  const embedded = vm.createContext({ window: {} });
  vm.runInContext(scripts[0][1], embedded);
  assert.equal(embedded.window.__ITT_EXECUTIVE_VIEW_STATE__.trendRange, 'all');
  assert.deepEqual(plain(embedded.window.__ITT_EXECUTIVE_VIEW_STATE__.services), []);
  assert.equal(embedded.window.__ITT_EXECUTIVE_PAYLOAD__.report_metadata.title, 'Draft </script> title');
  assert.equal(embedded.window.__ITT_EXECUTIVE_PAYLOAD__.report_metadata.notes, 'Unsaved notes');
  assert.deepEqual(plain(embedded.window.__ITT_EXECUTIVE_PAYLOAD__.report_metadata.included_sections), ['coverage']);
});

test('malformed page state is ignored without preventing analysis from loading', () => {
  for (const value of [null, 'corrupted state', 7, ['not', 'an', 'object']]) {
    const summary = harness('summary');
    summary.store.activate(payload()); summary.store.write('summary', value);
    summary.context.activateSummaryState(payload());
    summary.context.renderPivotSourceEnvOptions(['Production']);
    summary.context.saveSummaryState();
    assert.deepEqual(plain(summary.context.getSelectedPivotSourceEnvs()), []);
    const executive = harness('executive-summary');
    executive.store.activate(payload()); executive.store.write('executive', value);
    executive.context.renderExecutiveSummary(payload());
    assert.equal(executive.get('trendRangeSelect').value, '6');
    assert.equal(executive.get('reportTitleInput').value, 'Server report');
  }
});

test('overlapping summary and executive refreshes cannot restore an older analysis or its errors', async () => {
  for (const page of ['summary', 'executive-summary']) {
    for (const staleFailure of [false, true]) {
      const app = harness(page);
      if (page === 'summary') vm.runInContext('renderCoverage = renderDashboard = () => {};', app.context);
      const requests = [];
      app.context.fetch = () => new Promise((resolve, reject) => requests.push({ resolve, reject }));
      const load = page === 'summary' ? app.context.loadSummary : app.context.loadExecutiveSummary;
      const older = load();
      const latest = load();
      requests[1].resolve({ ok: true, json: async () => payload('newer-analysis') });
      await latest;
      if (staleFailure) requests[0].reject(new Error('stale network error'));
      else requests[0].resolve({ ok: true, json: async () => payload('older-analysis') });
      await older;
      assert.equal(app.store.currentRevision(), 'newer-analysis');
      assert.doesNotMatch(app.get('fileLabel').innerText, /Failed to load|stale network error/);
    }
  }
});

test('summary and executive port identifiers remain ungrouped while flow totals stay formatted', () => {
  const summary = harness('summary');
  summary.context.renderEnvServicePivot({ env_service_pivot: [{ source_env: 'Production', destination_env: 'Database', protocol: 'TCP', port: 9300, flow_count: 10000 }] });
  assert.match(summary.get('pivotTableWrap').innerHTML, />9300<\/td>/);
  assert.doesNotMatch(summary.get('pivotTableWrap').innerHTML, /9,300/);
  assert.match(summary.get('pivotTableWrap').innerHTML, /10,000/);
  const executive = harness('executive-summary');
  const script = inline('executive-summary');
  const start = script.indexOf('function renderPersistentServices(');
  const end = script.indexOf('function renderCrossTalk(', start);
  vm.runInContext(script.slice(start, end), executive.context);
  executive.context.renderPersistentServices([{ protocol: 'TCP', port: 9300, month_count: 2, active_connections: 1, flow_count: 10000 }]);
  const persistent = executive.get('persistentServices').options[0].innerHTML;
  assert.match(persistent, /TCP 9300/); assert.doesNotMatch(persistent, /9,300/); assert.match(persistent, /10,000/);
  executive.context.renderLatestMonthChanges([{ month: '2026-03', protocol: 'TCP', port: 65535, flow_count: 10000 }]);
  const latest = executive.get('latestMonthChanges').options[0].innerHTML;
  assert.match(latest, /TCP 65535/); assert.doesNotMatch(latest, /65,535/); assert.match(latest, /10,000/);
});
