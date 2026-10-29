'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const CSVImport = require('../../internal/extractor/frontend/csv-import.js');

class MockFormData {
  constructor() {
    this.entries = [];
  }

  append(name, value) {
    this.entries.push([name, value]);
  }
}

class MockXHR {
  constructor() {
    this.upload = {};
    this.status = 0;
    this.responseText = '';
    this.timeout = undefined;
    this.sent = false;
  }

  open(method, url, async) {
    this.method = method;
    this.url = url;
    this.async = async;
  }

  send(body) {
    this.sent = true;
    this.body = body;
  }

  uploadProgress(loaded, total) {
    this.upload.onprogress({ lengthComputable: true, loaded, total });
  }

  finishUpload() {
    this.upload.onload();
  }

  respond(status, body) {
    this.status = status;
    this.responseText = body;
    this.onload();
  }
}

function setup(options = {}) {
  const xhrs = [];
  const forms = [];
  const statuses = [];
  const button = options.button || { disabled: false, textContent: 'Import CSVs' };
  const upload = CSVImport.upload({
    files: options.files || [{ name: 'traffic.csv', size: 1024 }],
    fields: options.fields || {
      primary_label_key: 'env',
      secondary_label_key: 'app',
      dataset_name: 'Quarterly traffic',
    },
    button,
    onStatus: (message, details) => statuses.push({ message, ...details }),
    xhrFactory: () => {
      const xhr = new MockXHR();
      xhrs.push(xhr);
      return xhr;
    },
    formDataFactory: () => {
      const form = new MockFormData();
      forms.push(form);
      return form;
    },
  });
  return { upload, xhr: xhrs[0], xhrs, form: forms[0], statuses, button };
}

test('three CSVs including a 91 MiB file have no frontend size cutoff and report aggregate progress', async () => {
  const largeSize = 91 * 1024 * 1024;
  const totalSize = 93 * 1024 * 1024;
  const run = setup({ files: [
    { name: 'ninety-one-megabytes.csv', size: largeSize },
    { name: 'previous-month.csv', size: 1024 * 1024 },
    { name: 'current-month.csv', size: 1024 * 1024 },
  ] });

  assert.equal(run.xhr.method, 'POST');
  assert.equal(run.xhr.url, '/blocked-traffic/api/results/import-csv');
  assert.equal(run.xhr.async, true);
  assert.equal(run.xhr.timeout, 0, 'large imports should not receive a browser timeout');
  assert.equal(run.xhr.sent, true);
  assert.equal(run.button.disabled, true);
  assert.equal(run.button.textContent, 'Importing...');
  assert.match(run.statuses[0].message, /3 CSV files \(93\.0 MiB total\)/);

  run.xhr.uploadProgress(totalSize / 2, totalSize);
  run.xhr.finishUpload();
  run.xhr.respond(200, JSON.stringify({ success: true, fileName: 'Imported CSV set: 1 file' }));
  const payload = await run.upload;

  assert.equal(payload.success, true);
  assert.deepEqual(run.statuses.map((status) => status.stage), [
    'preparing',
    'uploading',
    'analyzing',
    'complete',
  ]);
  assert.equal(run.statuses[1].percent, 50);
  assert.match(run.statuses[1].message, /46\.5 MiB of 93\.0 MiB/);
  assert.match(run.statuses[2].message, /server is analyzing/i);
  assert.equal(run.button.disabled, false);
  assert.equal(run.button.textContent, 'Import CSVs');
  assert.equal(run.form.entries.filter(([name]) => name === 'files').length, 3);
  assert.deepEqual(
    run.form.entries.filter(([name]) => name !== 'files'),
    [
      ['primary_label_key', 'env'],
      ['secondary_label_key', 'app'],
      ['dataset_name', 'Quarterly traffic'],
    ],
  );
});

test('network failures are actionable and always restore the import button', async () => {
  const run = setup();
  const rejected = assert.rejects(run.upload, (error) => {
    assert.equal(error.code, 'NETWORK_ERROR');
    assert.match(error.message, /connection to the local application|local application could not be reached/i);
    assert.match(error.message, /still running/i);
    return true;
  });

  run.xhr.onerror();
  await rejected;
  assert.equal(run.button.disabled, false);
  assert.equal(run.button.textContent, 'Import CSVs');
  assert.equal(run.statuses.at(-1).stage, 'error');
});

test('HTTP 413 explains the server-side rejection without claiming a browser limit', async () => {
  const run = setup();
  const rejected = assert.rejects(run.upload, (error) => {
    assert.equal(error.code, 'HTTP_413');
    assert.match(error.message, /HTTP 413/);
    assert.match(error.message, /server rejected the upload as too large/i);
    assert.match(error.message, /updated build|reverse-proxy|request-size settings|fewer files/i);
    assert.doesNotMatch(error.message, /64 MiB/i);
    return true;
  });

  run.xhr.finishUpload();
  run.xhr.respond(413, JSON.stringify({ error: 'request body rejected' }));
  await rejected;
  assert.equal(run.button.disabled, false);
});

test('successful HTTP responses with non-JSON bodies identify an unreadable server response', async () => {
  const run = setup();
  const rejected = assert.rejects(run.upload, (error) => {
    assert.equal(error.code, 'INVALID_JSON');
    assert.match(error.message, /unreadable response/i);
    assert.match(error.message, /HTTP 200/);
    assert.match(error.message, /application logs/i);
    return true;
  });

  run.xhr.finishUpload();
  run.xhr.respond(200, '<html>not JSON</html>');
  await rejected;
  assert.equal(run.button.disabled, false);
});

test('non-success HTTP and JSON application errors preserve useful server details', async (t) => {
  await t.test('HTTP failure', async () => {
    const run = setup();
    const rejected = assert.rejects(run.upload, (error) => {
      assert.equal(error.code, 'HTTP_ERROR');
      assert.match(error.message, /HTTP 503/);
      assert.match(error.message, /temporarily unavailable/);
      return true;
    });
    run.xhr.respond(503, JSON.stringify({ error: 'temporarily unavailable' }));
    await rejected;
  });

  await t.test('application rejection', async () => {
    const run = setup();
    const rejected = assert.rejects(run.upload, (error) => {
      assert.equal(error.code, 'IMPORT_REJECTED');
      assert.match(error.message, /CSV header is missing/);
      return true;
    });
    run.xhr.respond(200, JSON.stringify({ success: false, error: 'CSV header is missing' }));
    await rejected;
  });
});

test('duplicate submissions using the same button are rejected without disturbing the active upload', async () => {
  const button = { disabled: false, textContent: 'Import CSVs' };
  const first = setup({ button });
  let duplicateXHRCalls = 0;

  await assert.rejects(CSVImport.upload({
    files: [{ name: 'again.csv', size: 2048 }],
    button,
    xhrFactory: () => {
      duplicateXHRCalls += 1;
      return new MockXHR();
    },
    formDataFactory: () => new MockFormData(),
  }), (error) => {
    assert.equal(error.code, 'IMPORT_IN_PROGRESS');
    assert.match(error.message, /already in progress/i);
    return true;
  });

  assert.equal(duplicateXHRCalls, 0);
  assert.equal(button.disabled, true);
  assert.equal(button.textContent, 'Importing...');

  first.xhr.respond(200, JSON.stringify({ success: true }));
  await first.upload;
  assert.equal(button.disabled, false);
  assert.equal(button.textContent, 'Import CSVs');
});
