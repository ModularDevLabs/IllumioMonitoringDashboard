'use strict';

const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const repo = path.join(__dirname, '../..');
const source = name => readFileSync(path.join(repo, `${name}.html`), 'utf8');
// Controlled repository fixture extraction only, not HTML sanitization.
const scripts = markup => [...markup.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script\b[^>]*>/gi)].map(match => match[1]);
const NOW = '2026-10-22T18:00:00Z';
class FixedDate extends Date {
  constructor(...args) { super(...(args.length ? args : [NOW])); }
  static now() { return new Date(NOW).getTime(); }
}

function node(id = '') {
  const classes = new Set(); const attributes = new Map(); let text = '';
  return {
    id, children: [], style: {}, listeners: {}, className: '', dataset: {},
    classList: { add: value => classes.add(value), remove: value => classes.delete(value), contains: value => classes.has(value), toggle: (value, enabled) => { if (enabled) classes.add(value); else classes.delete(value); } },
    getAttribute: key => attributes.get(key), setAttribute: (key, value) => attributes.set(key, value), removeAttribute: key => attributes.delete(key),
    set innerHTML(value) { text = value; this.children = []; }, get innerHTML() { return text; },
    set innerText(value) { text = String(value); this.children = []; }, get innerText() { return text + this.children.map(child => child.textContent).join(''); },
    set textContent(value) { text = String(value); this.children = []; }, get textContent() { return text + this.children.map(child => child.textContent).join(''); },
    appendChild(child) { this.children.push(child); }, append(...children) { this.children.push(...children); },
    addEventListener(event, callback) { this.listeners[event] = callback; }, getContext() { return this; },
    querySelector(selector) { this.parts ||= {}; return this.parts[selector] ||= node(selector); },
  };
}

function runtime(page) {
  const elements = new Map(); const charts = [];
  const get = id => { if (!elements.has(id)) elements.set(id, node(id)); return elements.get(id); };
  const document = { body: node('body'), getElementById: get, createElement: node, querySelectorAll: () => [] };
  const window = { location: { search: '?metric=tampering', href: '' }, print() {} };
  const context = vm.createContext({ document, window, URLSearchParams, Date: FixedDate, console, setTimeout() {}, clearTimeout() {}, Chart: function(_, config) { this.data = config.data; this.destroy = () => {}; charts.push(this); } });
  vm.runInContext(readFileSync(path.join(repo, 'static/ui-common.js'), 'utf8'), context);
  context.UICommon = { ...window.UICommon, makeChartZoomable() {} };
  if (page) {
    const script = scripts(source(page)).find(body => body.includes('function load()'));
    const marker = page === 'executive' ? '\n    boot();' : page === 'report' ? '\n    UICommon.applyTheme(THEME_KEY' : '\n        initTheme();';
    const end = script.lastIndexOf(marker); assert.ok(end > 0);
    vm.runInContext(script.slice(0, end), context);
  }
  return { context, get, charts };
}

function tampering(count, complete = true, partial = false) {
  return { workloads: Array.from({ length: count }, (_, index) => `tampered-${index + 1}`), count, partial, status: { success: complete, error: complete ? '' : partial ? 'PCE repeated page 2' : 'PCE unavailable' } };
}

function snapshot(data) {
  return { timestamp: NOW, collection: { window_start: NOW, window_end: NOW }, tampering: data, workloads: { total: 1, status: { success: true }, enforcement_modes: {} }, ven_status: { warning: [], error: [], stopped: [], stopped_status: { success: true }, status: { success: true } }, blocked: { targets: [], status: { success: true } } };
}

function api(app, data, history = []) {
  app.context.fetch = async raw => {
    const url = new URL(raw, 'http://localhost'); let response = {};
    if (url.pathname === '/api/stats') response = snapshot(data);
    if (url.pathname === '/api/drilldown') {
      const metric = url.searchParams.get('metric');
      response = metric === 'tampering'
        ? { metric, title: 'Tampered VENs / Workloads', count: data.count, items: data.workloads, status: data.status, partial: data.partial, trend_24h: history, trend_daily: history, trend_ma_24h: [], trend_ma_daily: [], anomalous: !data.status.success, anomaly_reason: !data.status.success ? 'Earlier anomaly context' : '' }
        : { metric, count: 0, items: [], status: { success: true } };
    }
    return { ok: true, json: async () => response };
  };
}

test('tampering availability distinguishes complete zero, known partial counts, failure, and legacy success-with-warning', () => {
  const state = runtime().context.UICommon.tamperingStatus;
  assert.equal(state(tampering(0)).display, '0');
  assert.equal(state(tampering(75, false, true)).display, '75 (incomplete)');
  assert.equal(state(tampering(0, false)).display, '—');
  assert.equal(state({}).display, '—');
  const legacy = tampering(50); legacy.status.error = 'Events partial: PCE repeated page 2';
  assert.equal(state(legacy).complete, false);
  assert.equal(state(legacy).display, '50 (incomplete)');
  assert.equal(state(tampering(0, false, true)).display, '—');
});

test('pipeline dots are amber for partial or warnings, red for failure, green only for complete success', () => {
  const app = runtime('index');
  const indicator = app.get('stat-tampering');
  app.context.updateStatusIndicator('stat-tampering', { success: false, error: 'PCE repeated page 2' }, 'PCE repeated page 2', true);
  assert.equal(indicator.querySelector('.status-dot').className, 'status-dot dot-yellow');
  assert.equal(indicator.classList.contains('text-warning'), true);
  assert.match(indicator.title, /repeated page 2/);
  app.context.updateStatusIndicator('stat-tampering', { success: true, error: 'Legacy partial result' });
  assert.equal(indicator.querySelector('.status-dot').className, 'status-dot dot-yellow');
  app.context.updateStatusIndicator('stat-tampering', { success: false, error: 'Network failure' });
  assert.equal(indicator.querySelector('.status-dot').className, 'status-dot dot-red');
  app.context.updateStatusIndicator('stat-tampering', { success: true });
  assert.equal(indicator.querySelector('.status-dot').className, 'status-dot dot-green');
  assert.equal(indicator.classList.contains('text-warning'), false);
});

test('main tampering count is honest and partial collection downgrades SLO confidence', () => {
  const app = runtime('index');
  app.context.renderTamperingStatus(tampering(75, false, true));
  assert.equal(app.get('tamp').innerText, '75 (incomplete)');
  assert.equal(app.get('tamp').classList.contains('tampering-incomplete'), true);
  assert.match(source('index'), /#tamp\.tampering-incomplete\s*\{[^}]*font-size:\s*1\.1rem;[^}]*overflow-wrap:\s*normal;[^}]*word-break:\s*normal;/);
  assert.match(app.get('tamp-list').innerText, /known workloads only/);
  app.context.updateConfidenceBadge(snapshot(tampering(75, false, true)));
  assert.equal(app.get('confidence-pill').textContent, 'MEDIUM');
  assert.match(app.get('confidence-pill').title, /partial stages=1/);
  const legacy = tampering(50); legacy.status.error = 'Partial results';
  app.context.updateConfidenceBadge(snapshot(legacy));
  assert.notEqual(app.get('confidence-pill').textContent, 'HIGH');
  app.context.renderTamperingStatus(tampering(0, false));
  assert.equal(app.get('tamp').innerText, '—');
  assert.equal(app.get('tamp').classList.contains('tampering-incomplete'), false);
  app.context.renderTamperingStatus(tampering(0));
  assert.equal(app.get('tamp').innerText, '0');
  app.context.updateConfidenceBadge(snapshot(tampering(0)));
  assert.equal(app.get('confidence-pill').textContent, 'HIGH');
});

test('drilldown renders every known tampering workload beyond50 and explicitly labels incomplete collection', async () => {
  for (const count of [75, 250]) {
    const app = runtime('details'); api(app, tampering(count, false, true));
    await app.context.load();
    assert.equal(app.get('items').children.length, count);
    assert.match(app.get('count').innerText, new RegExp(`${count} \\(incomplete\\)`));
    assert.match(app.get('count').innerText, /PCE repeated page 2/);
    assert.match(app.get('count').innerText, /not a current all-clear/);
    assert.equal(app.charts.length, 0, 'incomplete empty history must not synthesize a zero point');
  }
});

test('failed collection does not imply zero while successful zero remains a real zero', async () => {
  for (const complete of [true, false]) {
    const app = runtime('details'); api(app, tampering(0, complete)); await app.context.load();
    if (complete) {
      assert.match(app.get('count').innerText, /^0 workload/);
      assert.deepEqual(Array.from(app.charts[0].data.datasets[0].data), [0]);
      assert.match(app.get('items').children[0].innerText, /No workloads with tampering events/);
    } else {
      assert.match(app.get('count').innerText, /^—.*unavailable/);
      assert.match(app.get('items').children[0].innerText, /does not confirm zero/);
      assert.equal(app.charts.length, 0);
    }
  }
});

test('incomplete tampering refresh preserves historical chart and anomaly context without inventing values', async () => {
  const app = runtime('details');
  api(app, tampering(75, false, true), [{ timestamp: '2026-10-22T17:55:00Z', value: 3 }]);
  await app.context.load();
  assert.deepEqual(Array.from(app.charts[0].data.datasets[0].data), [3]);
  assert.match(app.get('count').innerText, /Earlier anomaly context/);
});

test('reports mark tampering and anomaly coverage incomplete instead of claiming an all-clear', async () => {
  const app = runtime('report'); api(app, tampering(75, false, true)); const items = [];
  app.context.addSummaryItem = (_, label, value) => items.push({ label, value });
  app.context.renderLineChart = app.context.finalizeSectionSummariesAndExpansion = () => {};
  await app.context.load();
  assert.ok(items.some(item => item.label === 'Tampered VENs (Deduped)' && item.value === '75 (incomplete)'));
  assert.ok(items.some(item => item.label === 'Tampering Collection' && /incomplete/.test(item.value)));
  assert.ok(items.some(item => item.label === 'Active Metric Anomalies' && item.value === '0 confirmed (incomplete coverage)'));
  assert.match(app.get('anomaly-notes').textContent, /do not establish a current all-clear/);
});

test('executive summaries never label incomplete tampering collection healthy or risk-free', async () => {
  for (const count of [0, 75]) {
    const app = runtime('executive'); api(app, tampering(count, false, count > 0)); const kpis = [];
    app.context.addKPI = (_, label, value, sub, cls) => kpis.push({ label, value, sub, cls });
    app.context.renderChart = app.context.renderPolicyChart = app.context.renderValueRanking = () => {};
    await app.context.load();
    const tamperKPI = kpis.find(item => item.label === 'Tampered VENs');
    assert.equal(tamperKPI.value, count ? '75 (incomplete)' : '—');
    assert.notEqual(tamperKPI.cls, 'risk-good');
    assert.notEqual(kpis.find(item => item.label === 'Pipeline Confidence').value, 'Healthy');
    assert.match(app.get('board-risks').children.map(child => child.textContent).join(' '), /no current all-clear/);
    assert.match(app.get('actions-list').children[0].textContent, /Restore complete tampering collection/);
    assert.doesNotMatch(app.get('narrative-text').value, /Pipeline confidence: healthy/);
    assert.match(app.get('narrative-text').value, /Historical alert transitions do not establish a current all-clear/);
  }
});

test('modified inline scripts stay valid and fixture extraction tolerates mixed-case closing tags', () => {
  for (const page of ['index', 'details', 'report', 'executive']) {
    for (const script of scripts(source(page))) if (script.trim()) new vm.Script(script, { filename: `${page}.html` });
  }
  assert.deepEqual(scripts('<ScRiPt>one</ScRiPt > <script>two</script data-fixture>'), ['one', 'two']);
});
