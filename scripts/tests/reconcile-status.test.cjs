'use strict';

// Fix the process timezone before loading the formatter so assertions cover the
// same west-of-UTC behavior that exposed Go's year-0001 zero timestamp.
process.env.TZ = 'America/Chicago';

const assert = require('node:assert/strict');
const test = require('node:test');

const {
  describe,
  timestamp,
  triggerLabel,
} = require('../../static/reconcile-status.js');

const STARTED = '2026-10-02T15:00:00Z';
const COMPLETED = '2026-10-02T15:01:00Z';
const FINISHED = '2026-10-02T15:02:00Z';

test('timestamp rejects empty, non-string, invalid, and Go zero-time values', () => {
  for (const value of [undefined, null, '', '   ', 0, {}, 'not-a-date', '2026-99-99T00:00:00Z']) {
    assert.equal(timestamp(value), null, `expected ${String(value)} to be rejected`);
  }

  assert.equal(timestamp('0001-01-01T00:00:00Z'), null);
  assert.equal(timestamp('0000-01-01T00:00:00Z'), null);

  const valid = timestamp('2026-01-15T12:00:00Z');
  assert.ok(valid instanceof Date);
  assert.equal(valid.getHours(), 6, 'America/Chicago should render the UTC instant in CST');
});

test('year-0001 timestamps do not render a fake prior-day date or zero run counters', () => {
  const message = describe({
    running: false,
    last_started_at: '0001-01-01T00:00:00Z',
    last_finished_at: '0001-01-01T00:00:00Z',
    last_completed_at: '0001-01-01T00:00:00Z',
    last_days: 0,
    last_updated: 0,
    last_failed: 0,
  }, 'blocked');

  assert.equal(message, 'No blocked traffic history reconciliation has run in this session.');
  assert.doesNotMatch(message, /12\/31|Days checked|updates:|failures:/);
});

test('null and invalid status dates never create finished-run details', () => {
  for (const value of [null, '', 'invalid']) {
    const message = describe({
      last_started_at: value,
      last_finished_at: value,
      last_completed_at: value,
      last_days: 17,
      last_updated: 9,
      last_failed: 3,
    }, 'tampering');

    assert.equal(message, 'No tampering history reconciliation has run in this session.');
    assert.doesNotMatch(message, /17|updates|failures|Latest saved/);
  }
});

test('a marker-only startup skip is distinct from a run in the current session', () => {
  const markerOnly = describe({
    running: false,
    startup_skipped: true,
    startup_skip_reason: 'marker exists for stored day set',
    last_completed_at: '2026-10-01T08:30:00Z',
    last_days: 99,
    last_updated: 88,
    last_failed: 77,
  }, 'tampering');

  assert.match(markerOnly, /^Automatic startup reconciliation was skipped:/);
  assert.match(markerOnly, /Latest saved history completion:/);
  assert.match(markerOnly, /Run details are not available in this session\.$/);
  assert.doesNotMatch(markerOnly, /Last tampering history reconciliation finished|Days checked|updates:|failures:/);

  const noPriorDays = describe({
    startup_skipped: true,
    startup_skip_reason: 'no previous day keys present',
  }, 'tampering');
  assert.equal(noPriorDays, 'Automatic startup reconciliation was not needed: no prior-day tampering history is stored.');

  const currentRun = describe({
    running: true,
    startup_skipped: true,
    startup_skip_reason: 'marker exists for stored day set',
    last_trigger_reason: 'startup',
    last_started_at: STARTED,
  }, 'tampering');
  assert.match(currentRun, /^Tampering history reconciliation is running \(automatic at startup\)\./);
  assert.doesNotMatch(currentRun, /skipped|Latest saved/);
});

test('trigger labels distinguish startup, manual, service-change, and queued work', () => {
  assert.equal(triggerLabel('startup'), 'automatic at startup');
  assert.equal(triggerLabel('api'), 'manual request');
  assert.equal(
    triggerLabel('traffic-service-exclusions-changed'),
    'automatic after traffic service exclusion changes',
  );
  assert.equal(
    triggerLabel('traffic-service-exclusions-changed-queued'),
    'automatic after traffic service exclusion changes (queued)',
  );
  assert.equal(triggerLabel('unexpected'), 'trigger not recorded');
  assert.equal(triggerLabel(null), 'trigger not recorded');
});

test('tampering descriptions preserve the trigger distinction for running and finished work', () => {
  const cases = [
    ['startup', 'automatic at startup'],
    ['api', 'manual request'],
    ['traffic-service-exclusions-changed', 'automatic after traffic service exclusion changes'],
    ['traffic-service-exclusions-changed-queued', 'automatic after traffic service exclusion changes (queued)'],
  ];

  for (const [reason, expected] of cases) {
    const running = describe({
      running: true,
      last_trigger_reason: reason,
      last_started_at: STARTED,
    }, 'tampering');
    assert.match(running, /^Tampering history reconciliation is running/);
    assert.ok(running.includes(`(${expected})`), running);

    const finished = describe({
      last_trigger_reason: reason,
      last_started_at: STARTED,
      last_finished_at: FINISHED,
      last_days: 3,
      last_updated: 2,
      last_failed: 0,
    }, 'tampering');
    assert.match(finished, /^Last tampering history reconciliation finished:/);
    assert.ok(finished.includes(`(${expected})`), finished);
  }
});

test('a failed run reports failures and never presents its checkpoint as run success', () => {
  const message = describe({
    running: false,
    last_trigger_reason: 'api',
    last_started_at: STARTED,
    last_finished_at: FINISHED,
    last_completed_at: COMPLETED,
    last_days: 4,
    last_updated: 1,
    last_failed: 3,
  }, 'blocked');

  assert.match(message, /Days checked: 4; updates: 1; failures: 3\./);
  assert.match(message, /Latest saved history completion:/);
  assert.doesNotMatch(message, /success|successful|already reconciled/i);
});

test('the persisted checkpoint is not duplicated for the same successful finished run', () => {
  const message = describe({
    running: false,
    last_trigger_reason: 'api',
    last_started_at: STARTED,
    last_finished_at: FINISHED,
    last_completed_at: COMPLETED,
    last_days: 4,
    last_updated: 4,
    last_failed: 0,
  }, 'blocked');

  assert.match(message, /^Last blocked traffic history reconciliation finished:/);
  assert.match(message, /Days checked: 4; updates: 4; failures: 0\./);
  assert.doesNotMatch(message, /Latest saved history completion|Run details are not available/);
});

test('an older persisted checkpoint remains visible without being attributed to a newer run', () => {
  const message = describe({
    running: false,
    last_trigger_reason: 'api',
    last_started_at: STARTED,
    last_finished_at: FINISHED,
    last_completed_at: '2026-09-01T12:00:00Z',
    last_days: 2,
    last_updated: 0,
    last_failed: 2,
  }, 'tampering');

  assert.match(message, /failures: 2\./);
  assert.match(message, /Latest saved history completion:/);
});
