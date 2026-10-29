'use strict';
const assert = require('node:assert/strict');
const test = require('node:test');
const { createStore } = require('../../internal/extractor/frontend/analysis-state.js');

function storage() {
  const values = new Map();
  return { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), values };
}
function activate(store, revision = 'analysis-a') { return store.activate({ analysisRevision: revision }); }

test('navigation and refresh preserve each page independently for the same analysis', () => {
  const disk = storage();
  const first = createStore({storage: disk});
  assert.equal(activate(first), true);
  const heatmap = { mode: 'app', source: ['Payments'], ports: ['1000', '9300'], hideEmpty: false };
  assert.equal(first.write('heatmaps', heatmap), true);
  first.write('executive', {range: 'all', services: [], includedSections: ['overview']});
  const nextPage = createStore({storage: disk});
  assert.equal(activate(nextPage), false);
  assert.deepEqual(nextPage.read('heatmaps'), heatmap);
  assert.deepEqual(nextPage.read('executive').services, []);
  assert.equal(activate(nextPage), false);
  assert.deepEqual(nextPage.read('heatmaps'), heatmap);
});

test('new revision resets all analysis pages even with the same filename or dataset ID', () => {
  const view = createStore({storage: storage()});
  view.activate({analysisRevision: 'one', fileName: 'traffic.csv', datasetId: 'saved'});
  for (const page of ['heatmaps', 'summary', 'executive', 'sections:/heatmaps']) view.write(page, {selected: true});
  assert.equal(view.activate({analysisRevision: 'two', fileName: 'traffic.csv', datasetId: 'saved'}), true);
  for (const page of ['heatmaps', 'summary', 'executive', 'sections:/heatmaps']) assert.deepEqual(view.read(page), {});
  assert.equal(view.currentRevision(), 'two');
});

test('saving metadata or unchanged refresh does not rotate view state', () => {
  const view = createStore({storage: storage()});
  activate(view);
  view.write('executive', {range: '12'});
  view.activate({analysisRevision: 'analysis-a', report_metadata: {title: 'New title'}, datasetId: 'newly-saved'});
  assert.deepEqual(view.read('executive'), {range: '12'});
});

test('stale pages cannot overwrite a new analysis after a navigation or back-forward restore', () => {
  const disk = storage();
  const stale = createStore({storage: disk});
  const current = createStore({storage: disk});
  activate(stale, 'old');
  activate(current, 'new');
  current.write('heatmaps', {mode: 'env'});
  assert.equal(stale.write('heatmaps', {mode: 'combined'}), false);
  assert.deepEqual(stale.read('heatmaps'), {});
  assert.deepEqual(current.read('heatmaps'), {mode: 'env'});
});

test('views are isolated by application scope and browser session', () => {
  const disk = storage();
  const standalone = createStore({storage: disk, scope: 'standalone'});
  const combined = createStore({storage: disk, scope: '/blocked-traffic'});
  const anotherTab = createStore({storage: storage(), scope: 'standalone'});
  for (const view of [standalone, combined, anotherTab]) activate(view);
  standalone.write('summary', {source: ['prod']});
  assert.deepEqual(combined.read('summary'), {});
  assert.deepEqual(anotherTab.read('summary'), {});
});

test('storage failures and malformed saved data do not break analysis rendering', () => {
  for (const disk of [
    {getItem() {throw Error('blocked');}, setItem() {throw Error('blocked');}},
    {getItem() {return null;}, setItem() {throw Error('quota');}},
  ]) {
    const view = createStore({storage: disk});
    assert.doesNotThrow(() => activate(view));
    assert.equal(view.write('heatmaps', {mode: 'app'}), true);
    assert.deepEqual(view.read('heatmaps'), {mode: 'app'});
    activate(view, 'next');
    assert.deepEqual(view.read('heatmaps'), {});
  }
});

test('malformed stored JSON is repaired so later navigation still remembers choices', () => {
  const disk = storage();
  disk.values.set('ittAnalysisView:v1:standalone', '{');
  const first = createStore({storage: disk});
  activate(first);
  first.write('heatmaps', {mode: 'combined'});
  const returned = createStore({storage: disk});
  activate(returned);
  assert.deepEqual(returned.read('heatmaps'), {mode: 'combined'});
});

test('missing analysis identity never falls back to a filename and cannot save stale filters', () => {
  const view = createStore({storage: storage()});
  activate(view);
  view.write('heatmaps', {mode: 'app'});
  view.activate({fileName: 'traffic.csv'});
  assert.equal(view.currentRevision(), '');
  assert.deepEqual(view.read('heatmaps'), {});
  assert.equal(view.write('heatmaps', {mode: 'combined'}), false);
});

test('state is copied, special keys are rejected, and activation notifies on every load', () => {
  const events = [];
  const view = createStore({storage: storage(), onActivate: event => events.push(event)});
  activate(view);
  const source = {ports: ['8080']};
  view.write('heatmaps', source);
  source.ports.push('443');
  const saved = view.read('heatmaps');
  saved.ports.push('22');
  assert.deepEqual(view.read('heatmaps'), {ports: ['8080']});
  assert.equal(view.write('__proto__', {polluted: true}), false);
  activate(view);
  assert.deepEqual(events, [{revision:'analysis-a',changed:true}, {revision:'analysis-a',changed:false}]);
});
