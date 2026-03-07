(function () {
  function parseTS(v) { return v ? new Date(v).getTime() : 0; }
  function pad2(n) { return String(n).padStart(2, '0'); }

  function formatStaleness(ts) {
    if (!ts) return 'staleness: unknown';
    var mins = Math.max(0, Math.floor((Date.now() - ts) / 60000));
    if (mins < 1) return 'staleness: <1m';
    if (mins < 60) return 'staleness: ' + mins + 'm';
    var h = Math.floor(mins / 60);
    var m = mins % 60;
    return 'staleness: ' + h + 'h ' + m + 'm';
  }

  function nextRefreshText(nextAt) {
    if (!nextAt) return 'next refresh: --';
    var ms = Math.max(0, nextAt - Date.now());
    var sec = Math.floor(ms / 1000);
    var m = Math.floor(sec / 60);
    var s = sec % 60;
    return 'next refresh: ' + m + 'm ' + pad2(s) + 's';
  }

  function metricTitleMap() {
    return {
      'w-total': 'All workloads returned by /workloads in current org.',
      'm-idle': 'Managed workloads with enforcement_mode=idle.',
      'm-visibility': 'Managed workloads with enforcement_mode=visibility_only.',
      'm-selective': 'Managed workloads with enforcement_mode=selective.',
      'm-full': 'Managed workloads with enforcement_mode=full.',
      'm-unmanaged': 'Workloads without active/managed VEN association.',
      'v-warn': 'Count of VENs from /vens?health=warning.',
      'v-err': 'Count of VENs from /vens?health=error.',
      'tamp': 'Unique deduped VEN/workload names with agent.tampering events in window.'
    };
  }

  function applyMetricTooltips() {
    var map = metricTitleMap();
    Object.keys(map).forEach(function (id) {
      var el = document.getElementById(id);
      if (el) el.title = map[id];
    });
  }

  function applyTheme(themeKey, btnId, rerenderFn) {
    var t = localStorage.getItem(themeKey) || 'light';
    setTheme(t, themeKey, btnId, rerenderFn);
  }

  function setTheme(theme, themeKey, btnId, rerenderFn) {
    var t = theme === 'dark' ? 'dark' : 'light';
    document.body.setAttribute('data-theme', t);
    localStorage.setItem(themeKey, t);
    var btn = document.getElementById(btnId);
    if (btn) btn.innerText = t === 'dark' ? 'Light Mode' : 'Dark Mode';
    if (typeof rerenderFn === 'function') rerenderFn();
  }

  window.UICommon = {
    parseTS: parseTS,
    formatStaleness: formatStaleness,
    nextRefreshText: nextRefreshText,
    applyMetricTooltips: applyMetricTooltips,
    applyTheme: applyTheme,
    setTheme: setTheme
  };
})();
