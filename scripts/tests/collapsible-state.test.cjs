'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const { createStore } = require('../../internal/extractor/frontend/analysis-state.js');

const script = fs.readFileSync(path.resolve(__dirname, '../../internal/extractor/frontend/collapsible.js'), 'utf8');

function storage() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    values,
  };
}

class Element {
  constructor(classes = []) {
    this.classes = new Set(classes);
    this.children = [];
    this.dataset = {};
    this.handlers = new Map();
    this.attributes = {};
    this.hidden = false;
    this.classList = {
      add: (name) => this.classes.add(name),
      remove: (name) => this.classes.delete(name),
      contains: (name) => this.classes.has(name),
      toggle: (name, enabled) => enabled ? this.classes.add(name) : this.classes.delete(name),
    };
  }
  set className(value) { this.classes = new Set(value.split(/\s+/)); }
  get className() { return [...this.classes].join(' '); }
  set innerHTML(value) {
    this.html = value;
    if (value.includes('data-auto-collapse-label')) {
      const label = new Element();
      label.dataset.autoCollapseLabel = '';
      label.textContent = 'Collapse';
      this.append(label);
    }
  }
  get firstChild() { return this.children[0] || null; }
  append(...items) { items.forEach((item) => this.appendChild(item)); }
  appendChild(item) {
    if (item.parent) item.parent.children = item.parent.children.filter((child) => child !== item);
    this.children.push(item);
    item.parent = this;
    return item;
  }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, fn) {
    if (!this.handlers.has(name)) this.handlers.set(name, []);
    this.handlers.get(name).push(fn);
  }
  click() { (this.handlers.get('click') || []).forEach((fn) => fn()); }
  matches(selector) {
    return selector.startsWith('.')
      ? this.classes.has(selector.slice(1))
      : selector === '[data-auto-collapse-label]' && Object.hasOwn(this.dataset, 'autoCollapseLabel');
  }
  querySelector(selector) {
    if (selector.startsWith(':scope > ')) {
      const [first, ...rest] = selector.slice(9).split(' ');
      const child = this.children.find((entry) => entry.matches(first));
      return rest.length ? child?.querySelector(rest.join(' ')) || null : child || null;
    }
    for (const child of this.children) {
      if (child.matches(selector)) return child;
      const nested = child.querySelector(selector);
      if (nested) return nested;
    }
    return null;
  }
}

function setup(options = {}) {
  const session = options.session || storage();
  const local = options.local || storage();
  const sections = (options.defaults || [false, true]).map((collapsed, index) => {
    const section = new Element(collapsed ? ['is-auto-collapsed'] : []);
    section.dataset.autoCollapsible = 'section-' + index;
    section.dataset.collapseTitle = 'Section ' + index;
    section.append(new Element());
    return section;
  });
  const handlers = new Map();
  const events = [];
  const document = {
    body: { dataset: options.scope ? { collapseScope: options.scope } : {} },
    readyState: options.readyState || 'complete',
    querySelectorAll(selector) { return selector === '[data-auto-collapsible]' ? sections : []; },
    createElement() { return new Element(); },
    addEventListener(name, fn) {
      if (!handlers.has(name)) handlers.set(name, []);
      handlers.get(name).push(fn);
    },
    dispatchEvent(event) {
      events.push(event);
      (handlers.get(event.type) || []).forEach((fn) => fn(event));
    },
  };
  class CustomEvent {
    constructor(type, options = {}) { this.type = type; this.detail = options.detail; }
  }
  const store = createStore({
    storage: session,
    scope: 'test-app',
    onActivate(detail) { document.dispatchEvent(new CustomEvent('itt:analysis-activated', { detail })); },
  });
  const window = {
    location: { pathname: options.pathname || '/heatmaps' },
    ...(options.analysis === false ? {} : { ITTAnalysisState: store }),
    ...(options.exported ? { __ITT_EXECUTIVE_PAYLOAD__: { analysisRevision: 'offline' } } : {}),
  };
  vm.runInNewContext(script, { window, document, localStorage: local, CustomEvent }, { filename: 'collapsible.js' });
  return {
    store, session, local, sections, document, window, events,
    activate(revision = 'analysis-1') { store.activate({ analysisRevision: revision }); },
    state() { return sections.map((section) => section.classList.contains('is-auto-collapsed')); },
    click(index) { sections[index].querySelector('.auto-collapse-toggle').click(); },
  };
}

test('same-analysis section choices survive navigation and refresh, independently by page', () => {
  const original = setup();
  original.activate();
  original.click(0);
  original.click(1);
  assert.deepEqual(original.state(), [true, false]);

  const revisited = setup({ session: original.session });
  assert.deepEqual(revisited.state(), [false, true], 'Wait for analysis identity before restoring saved choices');
  revisited.activate();
  assert.deepEqual(revisited.state(), [true, false]);
  revisited.activate();
  assert.deepEqual(revisited.state(), [true, false]);

  const otherPage = setup({ session: original.session, pathname: '/summary' });
  otherPage.activate();
  assert.deepEqual(otherPage.state(), [false, true]);
  assert.equal(original.local.values.size, 0, 'Analysis choices must not leak to legacy global localStorage');
});

test('new analysis restores original per-section defaults rather than prior UI state', () => {
  const page = setup();
  page.activate();
  page.click(0);
  page.click(1);
  assert.deepEqual(page.state(), [true, false]);
  page.activate('analysis-2');
  assert.deepEqual(page.state(), [false, true]);
  const content = page.sections[1].querySelector(':scope > .auto-collapse-content');
  const button = page.sections[1].querySelector('.auto-collapse-toggle');
  assert.equal(content.hidden, true);
  assert.equal(button.attributes['aria-expanded'], 'false');
  assert.equal(button.querySelector('[data-auto-collapse-label]').textContent, 'Expand');
});

test('enhancement is idempotent and binds one toggle listener per section', () => {
  const page = setup();
  page.activate();
  for (let index = 0; index < 5; index++) page.window.ITTSections.initialize();
  assert.equal(page.sections[0].children.length, 2);
  assert.equal(page.sections[0].querySelector(':scope > .auto-collapse-content').children.length, 1);
  assert.equal(page.sections[0].querySelector('.auto-collapse-toggle').handlers.get('click').length, 1);
  page.click(0);
  assert.deepEqual(page.state(), [true, true]);
  assert.equal(page.events.at(-1).type, 'itt:section-toggle');
  assert.equal(page.events.at(-1).detail.collapsed, true);
});

test('non-analysis pages preserve their existing localStorage choices and custom scope', () => {
  const local = storage();
  local.setItem('ittCollapse:automation:section-0', 'true');
  const page = setup({ analysis: false, pathname: '/automation', scope: 'automation', local });
  assert.deepEqual(page.state(), [true, true]);
  page.click(0);
  assert.equal(local.getItem('ittCollapse:automation:section-0'), 'false');
  const revisit = setup({ analysis: false, pathname: '/automation', scope: 'automation', local });
  assert.deepEqual(revisit.state(), [false, true]);
});

test('standalone executive HTML exports ignore live analysis state and keep working offline', () => {
  const session = storage();
  session.setItem('ittAnalysisView:v1:test-app', JSON.stringify({
    revision: 'live-analysis',
    pages: { 'sections:/executive-summary': { 'section-0': false } },
  }));
  const local = storage();
  local.setItem('ittCollapse:/executive-summary:section-0', 'true');
  const page = setup({ session, local, exported: true, pathname: '/executive-summary' });
  assert.deepEqual(page.state(), [true, true]);
  page.click(0);
  assert.equal(local.getItem('ittCollapse:/executive-summary:section-0'), 'false');
  assert.equal(JSON.parse(session.getItem('ittAnalysisView:v1:test-app')).revision, 'live-analysis');
  assert.equal(page.store.currentRevision(), '');
});

test('blocked browser storage does not stop section controls from working', () => {
  const blocked = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); } };
  const page = setup({ session: blocked, local: blocked });
  assert.doesNotThrow(() => page.activate());
  page.click(0);
  page.activate();
  assert.deepEqual(page.state(), [true, true]);
  const legacy = setup({ analysis: false, local: blocked });
  assert.doesNotThrow(() => legacy.click(0));
  assert.deepEqual(legacy.state(), [true, true]);
});

test('malformed storage records and malformed section page values fall back safely', () => {
  for (const invalid of ['{', 'null', '[]', '{"revision":"analysis-1","pages":null}']) {
    const session = storage();
    session.setItem('ittAnalysisView:v1:test-app', invalid);
    const page = setup({ session });
    assert.doesNotThrow(() => page.activate());
    assert.deepEqual(page.state(), [false, true]);
    assert.doesNotThrow(() => page.click(0));
    assert.deepEqual(page.state(), [true, true]);
  }
  for (const invalid of [null, 'bad', 42, true, []]) {
    const session = storage();
    session.setItem('ittAnalysisView:v1:test-app', JSON.stringify({
      revision: 'analysis-1', pages: { 'sections:/heatmaps': invalid },
    }));
    const page = setup({ session });
    assert.doesNotThrow(() => page.activate());
    assert.deepEqual(page.state(), [false, true]);
    assert.doesNotThrow(() => page.click(0));
    page.activate();
    assert.deepEqual(page.state(), [true, true], 'A valid object replaces malformed page state');
  }
});

test('only Boolean section values override defaults; new analyses do not inherit legacy collapse keys', () => {
  const session = storage();
  session.setItem('ittAnalysisView:v1:test-app', JSON.stringify({
    revision: 'analysis-1', pages: { 'sections:/heatmaps': { 'section-0': 'true', 'section-1': 0 } },
  }));
  const local = storage();
  local.setItem('ittCollapse:/heatmaps:section-0', 'true');
  local.setItem('ittCollapse:/heatmaps:section-1', 'false');
  const page = setup({ session, local });
  page.activate();
  assert.deepEqual(page.state(), [false, true]);
  page.activate('analysis-2');
  assert.deepEqual(page.state(), [false, true]);
});

test('recoverable malformed JSON is repaired so section choices survive the next navigation', () => {
  const session = storage();
  session.setItem('ittAnalysisView:v1:test-app', '{');
  const page = setup({ session });
  page.activate();
  page.click(0);
  const revisited = setup({ session });
  revisited.activate();
  assert.deepEqual(revisited.state(), [true, true]);
});

test('DOMContentLoaded initialization and activation before it do not duplicate controls', () => {
  const page = setup({ readyState: 'loading' });
  assert.equal(page.sections[0].querySelector('.auto-collapse-toggle'), null);
  page.activate();
  page.document.dispatchEvent({ type: 'DOMContentLoaded' });
  assert.equal(page.sections[0].children.length, 2);
  assert.equal(page.sections[0].querySelector('.auto-collapse-toggle').handlers.get('click').length, 1);
  page.click(0);
  assert.deepEqual(page.state(), [true, true]);
});

test('a stale analysis page cannot overwrite newer section selections', () => {
  const stale = setup();
  stale.activate('old-analysis');
  const fresh = setup({ session: stale.session });
  fresh.activate('new-analysis');
  fresh.click(1);
  stale.click(0);
  fresh.activate('new-analysis');
  assert.deepEqual(fresh.state(), [false, false]);
});
