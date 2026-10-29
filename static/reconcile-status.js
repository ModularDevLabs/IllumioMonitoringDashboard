(function (root) {
  'use strict';

  function timestamp(value) {
    if (typeof value !== 'string' || !value.trim()) return null;
    const date = new Date(value);
    // Older servers emitted Go's unset time as 0001-01-01T00:00:00Z.
    // Reject it before local-time formatting can shift it into the previous year.
    if (!Number.isFinite(date.getTime()) || date.getUTCFullYear() <= 1) return null;
    return date;
  }

  function triggerLabel(reason) {
    if (reason === 'api') return 'manual request';
    if (reason === 'startup') return 'automatic at startup';
    if (typeof reason === 'string' && reason.startsWith('traffic-service-exclusions-')) {
      return reason.endsWith('-queued')
        ? 'automatic after traffic service exclusion changes (queued)'
        : 'automatic after traffic service exclusion changes';
    }
    return 'trigger not recorded';
  }

  function describe(status, kind) {
    const s = status || {};
    const label = kind === 'tampering' ? 'tampering' : 'blocked traffic';
    const started = timestamp(s.last_started_at);
    const finished = timestamp(s.last_finished_at);
    const completed = timestamp(s.last_completed_at);
    const trigger = triggerLabel(s.last_trigger_reason);
    let message;

    if (s.running) {
      message = `${label === 'tampering' ? 'Tampering' : 'Blocked traffic'} history reconciliation is running (${trigger}).`;
      if (started) message += ` Started: ${started.toLocaleString()}.`;
    } else if (finished) {
      message = `Last ${label} history reconciliation finished: ${finished.toLocaleString()} (${trigger}).`;
      message += ` Days checked: ${s.last_days || 0}; updates: ${s.last_updated || 0}; failures: ${s.last_failed || 0}.`;
    } else if (s.startup_skipped) {
      message = s.startup_skip_reason === 'no previous day keys present'
        ? `Automatic startup reconciliation was not needed: no prior-day ${label} history is stored.`
        : `Automatic startup reconciliation was skipped: ${label} history was already reconciled.`;
    } else if (s.startup_skip_reason) {
      message = `Automatic ${label} history reconciliation is deferred. ${s.startup_skip_reason}`;
    } else {
      message = `No ${label} history reconciliation has run in this session.`;
    }

    // A persisted checkpoint can survive an app restart. It does not supply
    // the finish time, counters, or trigger of a run in the current session.
    const completionBelongsToFinishedRun = !s.running && started && finished && completed &&
      completed >= started && completed <= finished && !(s.last_failed > 0);
    if (completed && !completionBelongsToFinishedRun) {
      message += ` Latest saved history completion: ${completed.toLocaleString()}.`;
      if (!finished && !s.running) message += ' Run details are not available in this session.';
    }
    return message;
  }

  const api = { describe, timestamp, triggerLabel };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.ReconcileStatus = api;
})(typeof window !== 'undefined' ? window : this);
