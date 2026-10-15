'use strict';

process.env.TZ = 'America/Chicago';

const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const repo = path.join(__dirname, '../..');
const details = readFileSync(path.join(repo, 'details.html'), 'utf8');
// Controlled repository fixture extraction, not HTML sanitization.
const scriptBodies = markup => [...markup.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script\b[^>]*>/gi)].map(match => match[1]);

function node(tag = 'div') {
  const classes = new Set();
  const attributes = new Map();
  let text = '';
  return {
    tagName: tag.toUpperCase(), children: [], className: '', style: {}, listeners: {},
    classList: {
      add: value => classes.add(value), remove: value => classes.delete(value), contains: value => classes.has(value),
      toggle: (value, enabled) => { if (enabled) classes.add(value); else classes.delete(value); },
    },
    setAttribute: (key, value) => attributes.set(key, value),
    removeAttribute: key => attributes.delete(key),
    getAttribute: key => attributes.get(key),
    appendChild(child) { this.children.push(child); return child; },
    append(...children) { this.children.push(...children); },
    addEventListener(name, callback) { this.listeners[name] = callback; },
    set innerHTML(value) { assert.equal(value, '', 'workload names must never be inserted as HTML'); text = ''; this.children = []; },
    set innerText(value) { text = String(value); this.children = []; },
    get innerText() { return text + this.children.map(child => child.textContent).join(''); },
    set textContent(value) { text = String(value); this.children = []; },
    get textContent() { return text + this.children.map(child => child.textContent).join(''); },
  };
}

function renderer() {
  const elements = new Map([['items', node('ul')], ['tampering-list-window', node('p')]]);
  const context = vm.createContext({
    document: { getElementById: id => elements.get(id), createElement: node },
    window: { location: { href: '' } },
  });
  const start = details.indexOf('function tamperingEventTime(');
  const end = details.indexOf('async function load()', start);
  assert.ok(start >= 0 && end > start);
  vm.runInContext(details.slice(start, end), context);
  return { context, list: elements.get('items'), caption: elements.get('tampering-list-window') };
}

test('tampering fixture scripts handle mixed-case tags and closing whitespace or attributes', () => {
  assert.deepEqual(scriptBodies('<ScRiPt>const one = 1;</ScRiPt > <script>const two = 2;</script data-fixture>'), ['const one = 1;', 'const two = 2;']);
});

test('tampering timestamps reject missing, non-RFC3339, zero-time, and invalid calendar dates', () => {
  const { context } = renderer();
  for (const value of [undefined, null, '', ' ', 0, 1, {}, [], 'not a date', '2026-10-15', '2026-10-15T13:00:00', '0001-01-01T00:00:00Z', '0000-01-01T00:00:00Z', '2026-02-30T12:00:00Z', '2025-02-29T12:00:00Z', '2026-13-01T12:00:00Z', '2026-10-15T24:00:00Z', '2026-10-15T12:60:00Z', '2026-10-15T12:00:60Z', '2026-10-15T12:00:00+24:00']) {
    assert.equal(context.tamperingEventTime(value), null, `expected ${String(value)} to be unavailable`);
  }
});

test('timestamps preserve the event instant and render localized date, time, and timezone', () => {
  const { context } = renderer();
  const summer = context.tamperingEventTime('2026-10-15T18:04:05Z');
  assert.equal(summer.datetime, '2026-10-15T18:04:05.000Z');
  assert.match(summer.label, /2026/);
  assert.match(summer.label, /1:04:05\s?PM|13:04:05/);
  assert.match(summer.label, /CDT|GMT-5/);
  const winter = context.tamperingEventTime('2026-01-15T18:04:05Z');
  assert.match(winter.label, /12:04:05/);
  assert.match(winter.label, /CST|GMT-6/);
  assert.equal(context.tamperingEventTime('2026-10-15T13:04:05-05:00').datetime, summer.datetime);
  assert.equal(context.tamperingEventTime(' 2024-02-29T18:04:05.123456789Z ').datetime, '2024-02-29T18:04:05.123Z');
});

test('tampering rows safely pair each workload name with its own semantic last-event time', () => {
  const { context, list, caption } = renderer();
  const hostileName = '<img src=x onerror=alert(1)>';
  context.renderDrilldownItems({ metric: 'tampering', items: ['app-01', hostileName], item_last_seen: { 'app-01': '2026-10-15T18:04:05Z', [hostileName]: '2026-10-15T19:01:00Z' } });
  assert.equal(caption.classList.contains('d-none'), false);
  assert.equal(list.getAttribute('aria-describedby'), 'tampering-list-window');
  assert.equal(list.children.length, 2);
  for (const [index, name] of ['app-01', hostileName].entries()) {
    const row = list.children[index];
    assert.equal(row.className, 'list-group-item tampering-workload-item');
    assert.equal(row.children[0].textContent, name);
    assert.equal(row.children[0].children.length, 0);
    assert.equal(row.children[1].children[0].tagName, 'TIME');
    assert.match(row.children[1].textContent, /^Last tamper: /);
    assert.match(row.children[1].textContent, /CDT|GMT-5/);
  }
  assert.equal(list.children[0].children[1].children[0].getAttribute('datetime'), '2026-10-15T18:04:05.000Z');
  assert.equal(list.children[1].children[1].children[0].getAttribute('datetime'), '2026-10-15T19:01:00.000Z');
});

test('missing, invalid, inherited and legacy timestamps remain unavailable instead of creating fake dates', () => {
  const { context, list } = renderer();
  const times = Object.create({ inherited: '2026-10-15T18:04:05Z' });
  Object.assign(times, { invalid: '2026-02-30T00:00:00Z', zero: '0001-01-01T00:00:00Z' });
  context.renderDrilldownItems({ metric: 'tampering', items: ['missing', 'invalid', 'zero', 'inherited'], item_last_seen: times });
  for (const row of list.children) {
    assert.equal(row.children[1].textContent, 'Last tamper: unavailable');
    assert.equal(row.children[1].children.length, 0);
  }
  context.renderDrilldownItems({ metric: 'tampering', items: ['legacy-host'] });
  assert.equal(list.children[0].children[1].textContent, 'Last tamper: unavailable');
});

test('tampering empty state states the last-24-hour window, while other metrics retain existing copy and names', () => {
  const { context, list, caption } = renderer();
  context.renderDrilldownItems({ metric: 'tampering', items: [] });
  assert.match(list.children[0].textContent, /last 24 hours as of the latest collection/);
  assert.doesNotMatch(list.children[0].textContent, /latest collection cycle/);
  context.renderDrilldownItems({ metric: 'ven_warning', items: ['warning-01'], item_last_seen: { 'warning-01': '2026-10-15T18:04:05Z' } });
  assert.equal(caption.classList.contains('d-none'), true);
  assert.equal(list.getAttribute('aria-describedby'), undefined);
  assert.equal(list.children[0].className, 'list-group-item mono');
  assert.equal(list.children[0].textContent, 'warning-01');
  assert.equal(list.children[0].children.length, 0);
  context.renderDrilldownItems({ metric: 'ven_error', items: [] });
  assert.equal(list.children[0].textContent, 'No results for this metric in the latest collection cycle.');
});

test('policy metric list visibility and ruleset navigation are unchanged', () => {
  const { context, list } = renderer();
  for (const metric of ['policy_rules', 'policy_ruleset']) {
    context.renderDrilldownItems({ metric, items: ['hidden'] });
    assert.equal(list.classList.contains('d-none'), true);
    assert.equal(list.children.length, 0);
  }
  context.renderDrilldownItems({ metric: 'policy_rulesets', items: ['Payments'], item_targets: ['/orgs/1/sec_policy/draft/rule_sets/9'] });
  assert.equal(list.classList.contains('d-none'), false);
  assert.equal(list.children[0].textContent, 'Payments');
  assert.equal(list.children[0].style.cursor, 'pointer');
  list.children[0].listeners.click();
  assert.equal(context.window.location.href, '/details?metric=policy_ruleset&target=%2Forgs%2F1%2Fsec_policy%2Fdraft%2Frule_sets%2F9');
});

test('card, tooltip and drilldown explain the fixed list window independently of chart range', () => {
  const index = readFileSync(path.join(repo, 'index.html'), 'utf8');
  const shared = readFileSync(path.join(repo, 'static/ui-common.js'), 'utf8');
  assert.match(index, /Last 24h · Click for impacted workloads and last tamper time/);
  assert.match(shared, /agent\.tampering events in the last 24 hours as of the latest collection/);
  assert.match(details, /Changing the chart range does not change this list\./);
  assert.match(details, /\$\{d\.count\} workload\(s\) with tampering events in the last 24 hours/);
  assert.match(details, /\.tampering-workload-item \{[^}]*flex-wrap: wrap/);
  assert.match(details, /detail\.className = 'tampering-last-seen text-muted'/);
  for (const script of scriptBodies(details)) {
    if (script.trim()) new vm.Script(script);
  }
});
