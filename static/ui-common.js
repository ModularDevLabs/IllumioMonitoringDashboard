(function () {
  var zoomModalEl = null;
  var zoomChart = null;

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

  function ensureChartZoomModal() {
    if (zoomModalEl) return zoomModalEl;
    var styleId = 'chart-zoom-style';
    if (!document.getElementById(styleId)) {
      var style = document.createElement('style');
      style.id = styleId;
      style.textContent = [
        '.chart-zoom-overlay{position:fixed;inset:0;z-index:9999;background:rgba(0,0,0,0.65);display:flex;align-items:center;justify-content:center;padding:16px;}',
        '.chart-zoom-modal{width:min(1200px,96vw);height:min(760px,92vh);background:var(--panel-bg,var(--surface-1,#fff));border:1px solid var(--border,#d6dde5);border-radius:12px;box-shadow:0 10px 30px rgba(0,0,0,0.35);display:flex;flex-direction:column;}',
        '.chart-zoom-head{display:flex;align-items:center;justify-content:space-between;padding:10px 12px;border-bottom:1px solid var(--border,#d6dde5);}',
        '.chart-zoom-title{font-size:0.95rem;font-weight:700;color:var(--text,var(--text-1,#111));}',
        '.chart-zoom-close{border:1px solid var(--border,#d6dde5);background:var(--panel-bg,var(--surface-1,#fff));color:var(--text,var(--text-1,#111));border-radius:6px;padding:4px 10px;cursor:pointer;}',
        '.chart-zoom-body{flex:1;min-height:0;padding:10px;}',
        '.chart-zoom-canvas{width:100%!important;height:100%!important;}'
      ].join('');
      document.head.appendChild(style);
    }

    zoomModalEl = document.createElement('div');
    zoomModalEl.className = 'chart-zoom-overlay';
    zoomModalEl.style.display = 'none';
    zoomModalEl.innerHTML = '' +
      '<div class="chart-zoom-modal" role="dialog" aria-modal="true" aria-label="Expanded chart">' +
      '  <div class="chart-zoom-head">' +
      '    <div id="chart-zoom-title" class="chart-zoom-title">Chart</div>' +
      '    <button id="chart-zoom-close" type="button" class="chart-zoom-close">Close</button>' +
      '  </div>' +
      '  <div class="chart-zoom-body">' +
      '    <canvas id="chart-zoom-canvas" class="chart-zoom-canvas"></canvas>' +
      '  </div>' +
      '</div>';
    document.body.appendChild(zoomModalEl);

    var closeBtn = zoomModalEl.querySelector('#chart-zoom-close');
    closeBtn.addEventListener('click', closeChartZoom);
    zoomModalEl.addEventListener('click', function (e) {
      if (e.target === zoomModalEl) closeChartZoom();
    });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && zoomModalEl && zoomModalEl.style.display !== 'none') closeChartZoom();
    });
    return zoomModalEl;
  }

  function closeChartZoom() {
    if (zoomChart) {
      zoomChart.destroy();
      zoomChart = null;
    }
    if (zoomModalEl) zoomModalEl.style.display = 'none';
  }

  function baseZoomOptions() {
    return {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      plugins: {
        legend: { display: true }
      },
      scales: {
        x: {
          ticks: { maxRotation: 45, minRotation: 0 }
        },
        y: {
          beginAtZero: true
        }
      }
    };
  }

  function openChartZoomFromChart(chart, title) {
    if (!chart || typeof Chart === 'undefined') return;
    var modal = ensureChartZoomModal();
    var titleEl = modal.querySelector('#chart-zoom-title');
    titleEl.textContent = title || (chart.data && chart.data.datasets && chart.data.datasets[0] ? chart.data.datasets[0].label : 'Chart');
    var canvas = modal.querySelector('#chart-zoom-canvas');
    if (typeof Chart.getChart === 'function') {
      var existing = Chart.getChart(canvas);
      if (existing) existing.destroy();
    }
    if (zoomChart) {
      zoomChart.destroy();
      zoomChart = null;
    }
    var config = {
      type: chart.config.type || 'line',
      data: {
        labels: (chart.data && chart.data.labels) ? chart.data.labels.slice() : [],
        datasets: (chart.data && chart.data.datasets) ? chart.data.datasets.map(function (ds) {
          return Object.assign({}, ds, { data: Array.isArray(ds.data) ? ds.data.slice() : ds.data });
        }) : []
      },
      options: baseZoomOptions()
    };
    zoomChart = new Chart(canvas.getContext('2d'), config);
    modal.style.display = 'flex';
  }

  function makeChartZoomable(chart, title) {
    if (!chart || !chart.canvas) return;
    chart.canvas.style.cursor = 'zoom-in';
    chart.canvas.title = 'Click to expand chart';
    chart.canvas.addEventListener('click', function () {
      openChartZoomFromChart(chart, title);
    });
  }

  window.UICommon = {
    parseTS: parseTS,
    formatStaleness: formatStaleness,
    nextRefreshText: nextRefreshText,
    applyMetricTooltips: applyMetricTooltips,
    applyTheme: applyTheme,
    setTheme: setTheme,
    makeChartZoomable: makeChartZoomable
  };
})();
