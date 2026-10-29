'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const heatmapPath = path.resolve(__dirname, '../../internal/extractor/frontend/heatmaps.html');
const html = fs.readFileSync(heatmapPath, 'utf8');
// Extract controlled test fixtures, not untrusted HTML. This is not a sanitizer
// or general HTML parser; tag casing and closing attributes must still work.
const scriptBodies = markup => [...markup.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script\b[^>]*>/gi)].map(match => match[1]);
const script = scriptBodies(html).join('\n');

class Element {
  constructor() {
    this.children = [];
    this.nodes = new Map();
    this.handlers = new Map();
    this.classes = new Set();
    this.classList = {
      add: (...names) => names.forEach((name) => this.classes.add(name)),
      remove: (...names) => names.forEach((name) => this.classes.delete(name)),
      contains: (name) => this.classes.has(name),
      toggle: (name, force = !this.classes.has(name)) => force ? this.classes.add(name) : this.classes.delete(name),
    };
    this.checked = false;
    this.value = '';
    this.dataset = {};
    this.innerText = '';
    this.innerHTML = '';
  }

  set innerHTML(value) {
    this.html = value;
    this.children = [];
    this.nodes.clear();
  }

  get innerHTML() { return this.html; }
  appendChild(child) { this.children.push(child); }
  addEventListener(name, fn) { this.handlers.set(name, fn); }
  querySelector(selector) {
    if (!this.nodes.has(selector)) this.nodes.set(selector, new Element());
    return this.nodes.get(selector);
  }
  querySelectorAll() { return []; }
  focus() {}
  setSelectionRange() {}
}

function stateStore() {
  let revision = '';
  let values = {};
  return {
    activate(payload) {
      const next = payload.analysisRevision || '';
      const changed = next !== revision;
      if (changed) values = {};
      revision = next;
      return changed;
    },
    read(key, fallback = {}) { return JSON.parse(JSON.stringify(values[key] ?? fallback)); },
    write(key, value) { values[key] = JSON.parse(JSON.stringify(value)); },
    currentRevision() { return revision; },
  };
}

function setup(store = stateStore()) {
  const elements = new Map();
  const events = new Map();
  const document = {
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, new Element());
      return elements.get(id);
    },
    createElement() { return new Element(); },
    querySelectorAll() { return []; },
    addEventListener() {},
    body: { dataset: {} },
  };
  const context = vm.createContext({
    window: { ITTAnalysisState: store, addEventListener: (name, fn) => events.set(name, fn) },
    document,
    localStorage: { getItem() { return null; }, setItem() {} },
    Intl: { NumberFormat: class extends Intl.NumberFormat { constructor() { super('en-US'); } } },
  });
  vm.runInContext(script, context, { filename: heatmapPath });
  return {
    store, context, events, elements,
    run: (code) => vm.runInContext(code, context),
    view: () => JSON.parse(vm.runInContext('JSON.stringify({ mode: currentMode, hideEmpty: document.getElementById("hideEmptyToggle").checked, ...captureModeState() })', context)),
    load(payload) {
      context.payload = payload;
      vm.runInContext('renderPage(payload)', context);
    },
  };
}

function payload(revision = 'analysis-1') {
  return {
    analysisRevision: revision,
    fileName: 'traffic.csv',
    summary: [{ protocol: 'TCP', port: 9300 }],
    insights: {
      env_service_pivot: [
        { source_env: 'Production', destination_env: 'Database', protocol: 'TCP', port: 9300, flow_count: 12345, unique_connections: 1234 },
        { source_env: 'Production', destination_env: 'Database', protocol: 'UDP', port: 5353, flow_count: 10, unique_connections: 2 },
        { source_env: 'Test', destination_env: 'Database', protocol: 'TCP', port: 443, flow_count: 100, unique_connections: 3 },
      ],
      app_service_pivot: [
        { source_app: 'Payments', destination_app: 'SQL', protocol: 'TCP', port: 1433, flow_count: 23456, unique_connections: 1 },
      ],
      combined_service_pivot: [],
    },
  };
}

function chooseEnvironmentPair(page) {
  page.run(
    'filterState.source.add("Production"); filterState.destination.add("Database"); ' +
    'selectedDrilldownPair = { source: "Production", destination: "Database" }; ' +
    'selectedCellKey = heatmapPairKey("Production", "Database"); ' +
    'filterState.drillProtocol.add("TCP"); filterState.drillPort.add("9300"); ' +
    'filterSearch.source = "Prod"; filterSearch.drillPort = "93"; ' +
    'document.getElementById("hideEmptyToggle").checked = false; ' +
    'renderHeatmap(); persistHeatmapState();'
  );
}

test('heatmap fixture scripts support mixed-case tags and closing-tag whitespace or attributes', () => {
  assert.deepEqual(scriptBodies('<SCRIPT>let first = 1;</SCRIPT >\n<ScRiPt type="text/javascript">let second = 2;</sCrIpT ignored="fixture">'), ['let first = 1;', 'let second = 2;']);
});

test('heatmap state asset is loaded before section and page scripts', () => {
  assert.match(html, /src="\/(?:blocked-traffic\/)?assets\/analysis-state\.js"/);
  assert.ok(html.indexOf('assets/analysis-state.js') < html.indexOf('assets/collapsible.js'));
});

test('navigation and same-analysis refresh preserve filters, pair, searches, and hide-empty', () => {
  const first = setup();
  first.load(payload());
  chooseEnvironmentPair(first);
  const expected = first.view();

  first.load(payload());
  assert.deepEqual(first.view(), expected, 'Refresh must not call the old resetting mode path');
  const revisited = setup(first.store);
  revisited.load(payload());
  assert.deepEqual(revisited.view(), expected, 'A new page should restore the same analysis view');
  assert.equal(revisited.elements.get('drilldownLabel').innerText, 'Production -> Database');
  assert.match(revisited.elements.get('heatmapWrap').innerHTML, /is-selected/);
});

test('dimension modes retain their own filters and selected cells', () => {
  const page = setup();
  page.load(payload());
  chooseEnvironmentPair(page);
  const environment = page.view();
  page.run('applyMode("app");');
  assert.deepEqual(page.view().filters.source, []);
  assert.equal(page.view().pair, null);
  page.run(
    'filterState.source.add("Payments"); selectedDrilldownPair = { source: "Payments", destination: "SQL" }; ' +
    'selectedCellKey = heatmapPairKey("Payments", "SQL"); filterState.drillPort.add("1433"); renderHeatmap(); persistHeatmapState();'
  );
  const application = page.view();
  page.run('applyMode("env")');
  assert.deepEqual(page.view(), environment);
  page.run('applyMode("app")');
  assert.deepEqual(page.view(), application);
  const revisit = setup(page.store);
  revisit.load(payload());
  assert.deepEqual(revisit.view(), application);
});

test('a new analysis resets filters and defaults even when its filename is unchanged', () => {
  const page = setup();
  page.load(payload());
  chooseEnvironmentPair(page);
  page.run('applyMode("app")');
  page.load(payload('analysis-2'));
  assert.equal(page.view().mode, 'env');
  assert.equal(page.view().hideEmpty, true);
  assert.equal(page.view().pair, null);
  assert.deepEqual(page.view().filters, { source: [], destination: [], drillProtocol: [], drillPort: [] });
  assert.deepEqual(page.view().search, { source: '', destination: '', drillProtocol: '', drillPort: '' });
  page.run('applyMode("app")');
  assert.equal(page.view().pair, null);
});

test('stale and malformed saved filter values are pruned without hiding the matrix', () => {
  const page = setup();
  page.store.activate(payload());
  page.store.write('heatmaps', {
    mode: 'env', hideEmpty: 'bad',
    modes: {
      env: {
        filters: { source: ['Production', 'removed', 5], destination: null, drillProtocol: ['TCP', 'removed'], drillPort: ['9300', '9999'] },
        search: { source: 1 },
        pair: { source: 'Production', destination: 'Database' },
      },
    },
  });
  page.load(payload());
  assert.deepEqual(page.view().filters.source, ['Production']);
  assert.deepEqual(page.view().filters.drillProtocol, ['TCP']);
  assert.deepEqual(page.view().filters.drillPort, ['9300']);
  assert.equal(page.view().hideEmpty, true);
  assert.equal(page.view().search.source, '');

  const changed = payload();
  changed.insights.env_service_pivot = [changed.insights.env_service_pivot[2]];
  page.load(changed);
  assert.equal(page.view().pair, null);
  assert.deepEqual(page.view().filters.source, []);
  assert.deepEqual(page.view().filters.drillPort, []);
});

test('zero-match drilldown combinations retain visible controls and can be cleared', () => {
  const page = setup();
  page.load(payload());
  chooseEnvironmentPair(page);
  page.run('filterState.drillPort = new Set(["5353"]); runFiltersForState("drillPort");');
  assert.equal(page.elements.get('drilldownPanel').classList.contains('hidden'), false);
  assert.match(page.elements.get('drilldownBody').innerHTML, /No services match/);
  assert.deepEqual(page.view().filters.drillProtocol, ['TCP']);
  assert.deepEqual(page.view().filters.drillPort, ['5353']);
  page.load(payload());
  assert.match(page.elements.get('drilldownBody').innerHTML, /No services match/);
  page.run('filterState.drillProtocol.clear(); filterState.drillPort.clear(); runFiltersForState("drillPort");');
  assert.equal(page.elements.get('drilldownBody').children.length, 2);
});

test('port identifiers have no thousands separators but volume counts retain grouping', () => {
  const page = setup();
  page.load(payload());
  chooseEnvironmentPair(page);
  const row = page.elements.get('drilldownBody').children[0].innerHTML;
  assert.match(row, />9300<\/td>/);
  assert.doesNotMatch(row, /9,300/);
  assert.match(row, />12,345<\/td>/);
  assert.match(row, />1,234<\/td>/);
});

test('source and destination text containing the former key delimiter remains distinct', () => {
  const page = setup();
  const data = payload();
  data.insights.env_service_pivot = [
    { source_env: 'A|||B', destination_env: 'C', protocol: 'TCP', port: 1234, flow_count: 100, unique_connections: 1 },
    { source_env: 'A', destination_env: 'B|||C', protocol: 'TCP', port: 5678, flow_count: 200, unique_connections: 1 },
  ];
  page.load(data);
  page.run('selectedDrilldownPair = { source: "A|||B", destination: "C" }; selectedCellKey = heatmapPairKey("A|||B", "C"); renderHeatmap(); persistHeatmapState();');
  assert.equal(page.elements.get('drilldownLabel').innerText, 'A|||B -> C');
  assert.equal(page.elements.get('drilldownFlows').innerText, '100');
  page.load(data);
  assert.equal(page.elements.get('drilldownFlows').innerText, '100');
});

test('dropdown search text is persisted on input', () => {
  const page = setup();
  page.load(payload());
  const root = page.elements.get('sourceFilterWrap').querySelector('[data-filter-dropdown]');
  const search = root.querySelector('[data-filter-search]');
  search.value = 'Production';
  search.handlers.get('input')();
  const revisited = setup(page.store);
  revisited.load(payload());
  assert.equal(revisited.view().search.source, 'Production');
});

test('back-forward cache restoration checks the currently loaded analysis again', () => {
  const page = setup();
  page.run('let reloads = 0; loadHeatmaps = () => { reloads++; };');
  page.events.get('pageshow')({ persisted: false });
  assert.equal(page.run('reloads'), 0);
  page.events.get('pageshow')({ persisted: true });
  assert.equal(page.run('reloads'), 1);
});

test('overlapping refresh responses cannot restore an older analysis', async () => {
  const page = setup();
  const pending = [];
  page.context.fetch = () => new Promise((resolve) => pending.push(resolve));
  const older = page.run('loadHeatmaps()');
  const newer = page.run('loadHeatmaps()');
  pending[1]({ ok: true, json: async () => payload('newer-analysis') });
  await newer;
  pending[0]({ ok: true, json: async () => payload('older-analysis') });
  await older;
  assert.equal(page.store.currentRevision(), 'newer-analysis');
});
