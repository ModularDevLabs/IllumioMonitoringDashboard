const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { test } = require('node:test');
const vm = require('node:vm');

function renderer() {
  const html = readFileSync(path.join(__dirname, '../../internal/extractor/frontend/index.html'), 'utf8');
  const start = html.indexOf('function renderExtractionStatus(data) {');
  const end = html.indexOf('async function refreshExtractionStatus()', start);
  assert.ok(start >= 0 && end > start);
  const elements = new Map();
  const logs = [];
  let resetCount = 0;
  const element = (id) => {
    if (!elements.has(id)) {
      const classes = new Set(['hidden']);
      elements.set(id, { innerText: '', style: {}, disabled: false, classList: {
        add: (name) => classes.add(name), remove: (name) => classes.delete(name), contains: (name) => classes.has(name),
      } });
    }
    return elements.get(id);
  };
  const context = vm.createContext({ document: { getElementById: element }, log: (line) => logs.push(line), resetButtons: () => resetCount++ });
  vm.runInContext(html.slice(start, end), context);
  return { render: context.renderExtractionStatus, element, logs, resets: () => resetCount };
}

test('cancel request keeps polling until the salvage file is finished', () => {
  const ui = renderer();
  assert.equal(ui.render({ done: false, cancelled: true, completedChunks: 1, requestedChunks: 24 }), false);
  assert.equal(ui.resets(), 0);
  assert.equal(ui.element('cancelBtn').disabled, true);
  assert.match(ui.element('statusLabel').innerText, /saving completed data/);
  assert.equal(ui.logs.some((line) => line.includes('NO OUTPUT SAVED')), false);
  assert.equal(ui.render({ done: true, cancelled: true, partial: true, fileName: 'saved_PARTIAL.csv', completedChunks: 1, requestedChunks: 24, failedChunks: 23 }), true);
  assert.equal(ui.element('partialOutputWarning').classList.contains('hidden'), false);
  assert.equal(ui.element('summaryLinkWrap').classList.contains('hidden'), false);
  assert.match(ui.element('partialOutputPath').innerText, /saved_PARTIAL.csv/);
});

test('partial results are not labelled complete or double-prefixed in the log', () => {
  const ui = renderer();
  ui.render({ done: true, partial: true, fileName: 'partial.csv', completedChunks: 23, requestedChunks: 24, failedChunks: 1, error: 'INCOMPLETE EXTRACTION: missing window' });
  assert.match(ui.element('statusLabel').innerText, /Partial extraction saved/);
  assert.match(ui.element('partialOutputMessage').innerText, /1 original chunk did not complete in full/);
  assert.equal(ui.logs.some((line) => line.includes('INCOMPLETE EXTRACTION: INCOMPLETE')), false);
  assert.equal(ui.logs.some((line) => line.startsWith('COMPLETED:')), false);
});

test('complete result and no-output failure have distinct outcomes', () => {
  const success = renderer();
  success.render({ done: true, fileName: 'full.csv', completedChunks: 24, requestedChunks: 24 });
  assert.match(success.element('statusLabel').innerText, /completed successfully/);
  assert.equal(success.element('partialOutputWarning').classList.contains('hidden'), true);
  const failure = renderer();
  failure.render({ done: true, error: 'No query window completed', requestedChunks: 24 });
  assert.match(failure.element('statusLabel').innerText, /failed without output/);
  assert.equal(failure.element('summaryLinkWrap').classList.contains('hidden'), true);
});
