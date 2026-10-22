'use strict';

const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const repo = path.join(__dirname, '../..');
const html = name => readFileSync(path.join(repo, `${name}.html`), 'utf8');
// Controlled repository fixtures only; this is not HTML sanitization.
const scripts = markup => [...markup.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script\b[^>]*>/gi)].map(match => match[1]);
const NOW = '2026-10-22T18:00:00Z';
class FixedDate extends Date {
  constructor(...args) { super(...(args.length ? args : [NOW])); }
  static now() { return new Date(NOW).getTime(); }
}

function element(id = '') {
  const classes = new Set();
  const attributes = new Map();
  let text = '';
  return {
    id, children: [], className: '', dataset: {}, style: {}, listeners: {},
    classList: { add: value => classes.add(value), remove: value => classes.delete(value), contains: value => classes.has(value), toggle: (value, enabled) => { if (enabled) classes.add(value); else classes.delete(value); } },
    getAttribute: key => attributes.get(key), setAttribute: (key, value) => attributes.set(key, value), removeAttribute: key => attributes.delete(key),
    set innerHTML(value) { text = value; this.children = []; },
    get innerHTML() { return text; },
    set innerText(value) { text = String(value); this.children = []; },
    get innerText() { return text + this.children.map(child => child.textContent).join(''); },
    set textContent(value) { text = String(value); this.children = []; },
    get textContent() { return text + this.children.map(child => child.textContent).join(''); },
    appendChild(child) { this.children.push(child); }, append(...children) { this.children.push(...children); },
    addEventListener(event, callback) { this.listeners[event] = callback; },
    getContext() { return this; },
  };
}

function runtime(page) {
  const elements = new Map();
  const get = id => { if (!elements.has(id)) elements.set(id, element(id)); return elements.get(id); };
  const charts = [];
  const document = { body: element('body'), getElementById: get, createElement: element, querySelectorAll: () => [] };
  const window = { location: { search: '?metric=ven_stopped', href: '' }, print() {} };
  const context = vm.createContext({ document, window, URLSearchParams, Date: FixedDate, console, setTimeout() {}, clearTimeout() {}, Chart: function(_, config) { this.data = config.data; this.options = config.options; this.destroy = () => {}; charts.push(this); } });
  vm.runInContext(readFileSync(path.join(repo, 'static/ui-common.js'), 'utf8'), context);
  context.UICommon = { ...window.UICommon, makeChartZoomable() {} };
  if (page) {
    const script = scripts(html(page)).find(body => body.includes('function load()'));
    const marker = page === 'executive' ? '\n    boot();' : page === 'report' ? '\n    UICommon.applyTheme(THEME_KEY' : '\n        initTheme();';
    const end = script.lastIndexOf(marker);
    assert.ok(end > 0, `startup boundary missing for ${page}`);
    vm.runInContext(script.slice(0, end), context);
  }
  return { context, get, document, window, charts };
}

function stats(stopped = ['app-01'], complete = true) {
  return { timestamp: NOW, ven_status: { warning: [], error: [], suspended: [], stopped, stopped_status: { success: complete, error: complete ? '' : 'Collection failed' } }, workloads: { total: 1, enforcement_modes: {} }, blocked: { targets: [] }, tampering: { workloads: [] }, collection: { window_start: NOW, window_end: NOW } };
}

function drilldown(overrides = {}) {
  return { metric: 'ven_stopped', title: 'Stopped VENs', items: ['app-01'], count: 1, status: { success: true }, trend_24h: [{ timestamp: NOW, value: 1 }], trend_daily: [{ timestamp: NOW, value: 2 }], trend_ma_24h: [{ timestamp: NOW, value: 0.5 }], trend_ma_daily: [{ timestamp: NOW, value: 0.75 }], anomaly_window: 7, anomaly_pct: 50, anomalous: true, anomaly_reason: 'Above baseline', ...overrides };
}

function api(app, snapshot, stopped = drilldown()) {
  const calls = [];
  app.context.fetch = async raw => {
    const url = new URL(raw, 'http://localhost'); calls.push(url.pathname + url.search);
    let data = {};
    if (url.pathname === '/api/stats') data = snapshot;
    else if (url.pathname === '/api/drilldown') data = url.searchParams.get('metric') === 'ven_stopped' ? stopped : { items: [], count: 0, status: { success: true } };
    return { ok: true, json: async () => data };
  };
  return calls;
}

test('stopped status distinguishes confirmed zero, failures, partial results, and old responses', () => {
  const { context } = runtime();
  const state = context.UICommon.stoppedVENStatus;
  assert.equal(state(stats([], true).ven_status).display, '0');
  assert.equal(state(stats([], true).ven_status).complete, true);
  assert.equal(state(stats([], false).ven_status).display, '—');
  assert.equal(state({}).display, '—');
  assert.equal(state(null).complete, false);
  assert.equal(state(stats(['app-01'], false).ven_status).display, '1 (incomplete)');
  assert.equal(state(stats(['app-01', 'app-02'], true).ven_status).display, '2');
});

test('dashboard stopped card updates count and availability and uses ordinary drilldown navigation', () => {
  const app = runtime('index');
  app.context.renderStoppedVENStatus(stats(['app-01'], true).ven_status);
  assert.equal(app.get('v-stopped').innerText, '1');
  assert.equal(app.get('v-stopped-list').innerText, 'Click to view workload names');
  app.context.renderStoppedVENStatus(stats([], false).ven_status);
  assert.equal(app.get('v-stopped').innerText, '—');
  assert.match(app.get('v-stopped-list').innerText, /unavailable/);
  app.context.renderStoppedVENStatus(stats([], true).ven_status);
  assert.equal(app.get('v-stopped').innerText, '0');
  const card = element(); card.dataset.metric = 'ven_stopped'; app.document.querySelectorAll = () => [card];
  app.context.bindDrilldowns(); card.listeners.click();
  assert.equal(app.window.location.href, '/details?metric=ven_stopped');
  assert.match(html('index'), /row-cols-1 row-cols-sm-2 row-cols-lg-5/);
  assert.match(html('index'), /data-metric="ven_stopped"/);
});

test('stopped drilldown offers names, moving averages, anomaly context, chart ranges, and CSV export', async () => {
  const app = runtime('details');
  api(app, stats(), drilldown());
  await app.context.load();
  assert.equal(app.get('items').children[0].innerText, 'app-01');
  assert.match(app.get('count').innerText, /1 workload\(s\).*Above baseline/);
  assert.equal(app.get('trend-controls').classList.contains('d-none'), false);
  assert.equal(app.charts.length, 1);
  assert.equal(app.charts[0].data.datasets[0].label, 'Stopped VENs (24h, 5m)');
  assert.deepEqual(Array.from(app.charts[0].data.datasets[0].data), [1]);
  assert.match(app.charts[0].data.datasets[1].label, /7 points, 50% threshold/);
  app.context.bindTrendControls(); app.get('trend-sum').listeners.click();
  assert.equal(app.charts.at(-1).data.datasets[0].label, 'Stopped VENs (daily)');
  assert.deepEqual(Array.from(app.charts.at(-1).data.datasets[0].data), [2]);
  assert.equal(app.get('range-controls').classList.contains('d-none'), false);
  app.get('range-90').listeners.click();
  assert.equal(app.get('range-90').className, 'btn btn-sm btn-primary');
  app.context.setExportLinks();
  assert.equal(app.get('btn-csv').href, '/api/export/drilldown.csv?metric=ven_stopped');
  assert.equal(typeof app.get('btn-pdf').listeners.click, 'function');
});

test('confirmed stopped zero has zero-count chart parity while failed collection never fabricates zero', async () => {
  for (const complete of [true, false]) {
    const app = runtime('details');
    api(app, stats([], complete), drilldown({ items: [], count: 0, status: { success: complete, error: complete ? '' : 'API unavailable' }, trend_24h: [], trend_daily: [], trend_ma_24h: [], trend_ma_daily: [], anomalous: false, anomaly_reason: '' }));
    await app.context.load();
    if (complete) {
      assert.match(app.get('count').innerText, /^0 workload/);
      assert.deepEqual(Array.from(app.charts[0].data.datasets[0].data), [0]);
    } else {
      assert.match(app.get('count').innerText, /unavailable.*API unavailable/);
      assert.match(app.get('items').children[0].innerText, /successful collection is needed before reporting zero/);
      assert.equal(app.charts.length, 0);
    }
  }
});

test('incomplete stopped collection retains returned names and historical daily trends', async () => {
  const app = runtime('details');
  api(app, stats(['partial-host'], false), drilldown({ items: ['partial-host'], status: { success: false, error: 'Partial fetch' }, trend_24h: [] }));
  await app.context.load();
  assert.match(app.get('count').innerText, /1 \(incomplete\).*Partial fetch/);
  assert.equal(app.get('items').children[0].innerText, 'partial-host');
  assert.equal(app.charts.at(-1).data.datasets[0].label, 'Stopped VENs (daily)');
  assert.deepEqual(Array.from(app.charts.at(-1).data.datasets[0].data), [2]);
});

test('reports include stopped summaries, both trend frequencies and confirmed stopped anomalies', async () => {
  const app = runtime('report');
  const calls = api(app, stats(), drilldown());
  const items = []; const charts = [];
  app.context.addSummaryItem = (root, label, value) => items.push({ id: root.id, label, value });
  app.context.renderLineChart = (...args) => charts.push(args);
  app.context.finalizeSectionSummariesAndExpansion = () => {};
  await app.context.load();
  assert.ok(calls.some(url => url.includes('metric=ven_stopped')));
  assert.ok(items.some(item => item.label === 'Stopped VENs' && item.value === '1'));
  assert.ok(items.some(item => item.label === 'Active Metric Anomalies' && item.value === 1));
  const stoppedCharts = charts.filter(args => args[0].startsWith('Stopped VENs'));
  assert.deepEqual(stoppedCharts.map(args => args[0]), ['Stopped VENs (24h, 5m)', 'Stopped VENs (Daily)']);
  assert.deepEqual(Array.from(stoppedCharts[0][4][0].points).map(point => point.value), [0.5]);
  assert.deepEqual(Array.from(stoppedCharts[1][4][0].points).map(point => point.value), [0.75]);
  items.length = 0;
  app.context.renderAnomalyOutcomes(stats([], false), {}, {}, drilldown({ status: { success: false } }), {}, {});
  assert.ok(items.some(item => item.label === 'Active Metric Anomalies' && item.value === '0 confirmed (incomplete coverage)'));
});

test('executive actions, board risks, narrative and KPI distinguish stopped risks from unavailable collection', async () => {
  for (const complete of [true, false]) {
    const app = runtime('executive');
    const snapshot = stats(complete ? ['app-01'] : [], complete);
    api(app, snapshot);
    const kpis = [];
    app.context.addKPI = (_, label, value, sub, cls) => kpis.push({ label, value, sub, cls });
    app.context.renderChart = app.context.renderPolicyChart = app.context.renderValueRanking = () => {};
    await app.context.load();
    const actions = app.get('actions-list').children.map(node => node.textContent).join(' ');
    const risks = app.get('board-risks').children.map(node => node.textContent).join(' ');
    const narrative = app.get('narrative-text').value;
    const stoppedKPI = kpis.find(item => item.label === 'Stopped VENs');
    if (complete) {
      assert.match(actions, /Review 1 stopped VEN workload/);
      assert.match(risks, /1 stopped VEN workload/);
      assert.match(narrative, /stopped VENs: 1/);
      assert.equal(stoppedKPI.cls, 'risk-bad');
    } else {
      assert.match(actions, /Check stopped VEN collection/);
      assert.match(risks, /Stopped VEN status unavailable/);
      assert.match(narrative, /stopped VENs: unavailable/);
      assert.equal(stoppedKPI.value, '—');
      assert.equal(stoppedKPI.cls, 'risk-warn');
      assert.doesNotMatch(actions, /No immediate risk actions/);
    }
  }
});

test('shared settings, tooltip, and all changed inline scripts include stopped support without syntax regressions', () => {
  assert.match(html('settings'), /VEN Warning\/Error\/Stopped Anomaly Settings/);
  assert.match(html('settings'), /warning, error, and stopped VEN counts, including anomaly alerts/);
  const app = runtime(); app.context.UICommon.applyMetricTooltips();
  assert.match(app.get('v-stopped').title, /currently in the stopped state/);
  for (const page of ['index', 'details', 'report', 'executive', 'settings']) {
    for (const body of scripts(html(page))) if (body.trim()) new vm.Script(body, { filename: `${page}.html` });
  }
  assert.deepEqual(scripts('<ScRiPt>one</ScRiPt > <script>two</script data-fixture>'), ['one', 'two']);
});
