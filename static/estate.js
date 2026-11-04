(function () {
    'use strict';
    const featureNames = ['inventory_enabled', 'policy_enabled', 'issue_aging_enabled', 'label_hygiene_enabled', 'ven_versions_enabled', 'change_log_enabled', 'weekly_report_enabled'];
    const byID = id => document.getElementById(id);
    let lastView = null;
    let loading = false;
    let loadGeneration = 0;
    let reportRunsGeneration = 0;
    let reportGeneration = 0;
    let reportHTML = '';
    let reportWeek = '';
    let reportRunID = '';
    let reportKind = '';
    let reportObservation = '';
    let savingReport = false;
    let reportLoaded = false;
    let reportEmail = null;
    let reportSelectionsInitialized = false;
    const sectionChoices = new Map();
    let inventoryFilter = null;
    function node(tag, text, className) {
        const element = document.createElement(tag);
        if (text !== undefined) element.textContent = String(text);
        if (className) element.className = className;
        return element;
    }
    function count(value) { return Number.isFinite(value) ? value.toLocaleString() : '—'; }
    function timestamp(value) {
        const date = new Date(value || '');
        return Number.isFinite(date.getTime()) && date.getUTCFullYear() >= 1970 ? date.toLocaleString() : 'Not available';
    }
    function note(parent, text, warning) { parent.appendChild(node('p', text, warning ? 'estate-warning' : 'estate-muted')); }
    function kpi(parent, label, value, description) {
        const card = node('div', undefined, 'estate-kpi');
        card.appendChild(node('div', label, 'estate-kpi-label'));
        card.appendChild(node('strong', value));
        if (description) card.appendChild(node('div', description, 'estate-muted'));
        parent.appendChild(card);
        return card;
    }
    function panel(parent, title, id, options = {}) {
        const section = node('details', undefined, options.className || 'estate-panel estate-section');
        const key = options.key || 'illumio_estate_section_' + title;
        let saved = sectionChoices.get(key);
        if (saved === undefined) { try { saved = sessionStorage.getItem(key); } catch (_) {} }
        section.open = saved === 'open' ? true : saved === 'closed' ? false : options.defaultOpen !== false;
        let previousOpen = section.open;
        section.addEventListener('toggle', () => {
            // Initial/programmatic toggle notifications must not create a fake
            // remembered choice. Persist only a change from the rendered state.
            if (section.open === previousOpen) return;
            previousOpen = section.open;
            const value = section.open ? 'open' : 'closed';
            sectionChoices.set(key, value);
            try { sessionStorage.setItem(key, value); } catch (_) {}
        });
        if (id) section.id = id;
        section.appendChild(node('summary', title));
        parent.appendChild(section);
        return section;
    }
    function exportCSVControl(section, view, kind) {
        const controls = node('div', undefined, 'estate-intro');
        const button = node('button', 'Export CSV (Excel)');
        button.type = 'button';
        button.dataset.estateExport = kind;
        const status = node('span', undefined, 'estate-muted');
        status.setAttribute('role', 'status');
        const api = window.EstateCSV;
        const describe = api && (kind === 'labels' ? api.labelHygiene : api.issues);
        const result = describe ? describe(view, { includeContent: false }) : { available: false, reason: 'CSV export is unavailable. Reload this page to try again.' };
        button.disabled = !result.available;
        status.textContent = result.available ? 'Exports all ' + count(result.rowCount) + ' observed rows, including other pages.' + (result.partial ? ' Partial collection: the file identifies incomplete observations.' : '') : result.reason;
        button.addEventListener('click', () => {
            if (button.disabled) return;
            // Use this rendered view, not a later request or a table's current page.
            const exported = describe(view);
            if (!api.download(exported)) status.textContent = 'Unable to download the CSV. The displayed observations have not changed.';
            else status.textContent = 'Exported ' + count(exported.rowCount) + ' observed rows.' + (exported.partial ? ' This is a partial collection; see the assessment columns.' : '');
        });
        controls.appendChild(button); controls.appendChild(status); section.appendChild(controls);
    }
    function observedDuration(first, last) {
        const start = new Date(first || '').getTime();
        const end = new Date(last || '').getTime();
        if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return '—';
        const hours = Math.floor((end - start) / 3600000);
        return hours >= 24 ? Math.floor(hours / 24) + 'd ' + hours % 24 + 'h' : hours > 0 ? hours + 'h' : '<1h';
    }
    // Every cell is text, including PCE names, labels, versions, and error messages.
    function table(parent, headers, rows, emptyMessage) {
        if (!rows.length) { note(parent, emptyMessage || 'No matching observations.'); return; }
        const wrap = node('div', undefined, 'estate-table-wrap');
        const element = node('table', undefined, 'estate-table');
        const head = node('thead');
        const headerRow = node('tr');
        headers.forEach(label => { const cell = node('th', label); cell.scope = 'col'; headerRow.appendChild(cell); });
        head.appendChild(headerRow);
        element.appendChild(head);
        const body = node('tbody');
        element.appendChild(body);
        wrap.appendChild(element);
        parent.appendChild(wrap);
        let page = 0;
        const pageSize = 100;
        const controls = node('div', undefined, 'estate-intro');
        const status = node('span', undefined, 'estate-muted');
        const previous = node('button', 'Previous'); previous.type = 'button';
        const next = node('button', 'Next'); next.type = 'button';
        function render() {
            body.replaceChildren();
            rows.slice(page * pageSize, (page + 1) * pageSize).forEach(row => {
                const tr = node('tr');
                row.forEach(value => tr.appendChild(node('td', value === undefined || value === null ? '—' : value)));
                body.appendChild(tr);
            });
            status.textContent = 'Showing ' + count(page * pageSize + 1) + '–' + count(Math.min((page + 1) * pageSize, rows.length)) + ' of ' + count(rows.length);
            previous.disabled = page === 0;
            next.disabled = (page + 1) * pageSize >= rows.length;
        }
        if (rows.length > pageSize) {
            previous.addEventListener('click', () => { if (page > 0) { page--; render(); } });
            next.addEventListener('click', () => { if ((page + 1) * pageSize < rows.length) { page++; render(); } });
            controls.appendChild(status); controls.appendChild(previous); controls.appendChild(next);
            parent.appendChild(controls);
        }
        render();
    }
    function inventorySummary(snapshot) {
        if (!snapshot) return 'Waiting for the first estate observation.';
        if (!snapshot.inventory_complete) return 'Inventory unavailable or incomplete; counts are not confirmed.';
        const coverage = snapshot.total > 0 ? (100 * snapshot.managed / snapshot.total).toFixed(1) + '% management coverage' : 'No workloads observed';
        return 'Managed: ' + count(snapshot.managed) + ' · Unmanaged: ' + count(snapshot.unmanaged) + ' · ' + coverage;
    }
    function pendingPolicySummary(policy, kind) {
        const summary = policy?.pending_counts?.[kind];
        const valid = summary && ['added', 'modified', 'deleted', 'total'].every(key => Number.isInteger(summary[key]) && summary[key] >= 0);
        if (!valid || summary.complete !== true) return { value: 'Unavailable / incomplete', detail: 'Awaiting a complete pending-change assessment; draft inventory is not the pending total.' };
        return { value: count(summary.total), detail: 'Added: ' + count(summary.added) + ' · Modified: ' + count(summary.modified) + ' · Deleted: ' + count(summary.deleted) };
    }
    function pendingPolicyCards(parent, policy) {
        for (const [kind, label] of [['rules', 'Pending rules'], ['rulesets', 'Pending rulesets']]) {
            const summary = pendingPolicySummary(policy, kind);
            kpi(parent, label, summary.value, summary.detail);
        }
    }
    function renderDashboard(view) {
        const inventory = byID('estate-inventory-breakdown');
        const row = byID('estate-dashboard-row');
        if (!inventory || !row) return;
        const options = view.options || {};
        const snapshot = view.snapshot;
        inventory.hidden = !options.inventory_enabled;
        inventory.replaceChildren();
        if (options.inventory_enabled) {
            if (snapshot?.inventory_complete) {
                const managed = node('a', 'Managed: ' + count(snapshot.managed)); managed.href = '/estate?inventory=managed';
                const unmanaged = node('a', 'Unmanaged: ' + count(snapshot.unmanaged)); unmanaged.href = '/estate?inventory=unmanaged';
                inventory.appendChild(managed); inventory.appendChild(node('span', ' · ')); inventory.appendChild(unmanaged);
            } else inventory.textContent = inventorySummary(snapshot);
        }
        const visible = featureNames.some(key => options[key]);
        row.hidden = !visible;
        if (!visible) return;
        const content = byID('estate-dashboard-content');
        content.replaceChildren();
        if (!snapshot) { note(content, 'Enabled estate insights will appear after the next dashboard collection.'); return; }
        if (options.policy_enabled) {
            const policy = snapshot.policy;
            kpi(content, 'Active policy', policy?.active?.complete ? count(policy.active.rules) + ' rules' : 'Unavailable', policy?.active?.complete ? count(policy.active.rulesets) + ' rulesets' : 'Awaiting a complete policy collection');
            kpi(content, 'Draft policy inventory', policy?.draft?.complete ? count(policy.draft.rules) + ' rules' : 'Unavailable', (policy?.draft?.complete ? count(policy.draft.rulesets) + ' rulesets · ' : '') + 'Full draft inventory, not pending changes; not added to active totals');
            pendingPolicyCards(content, policy);
        }
        if (options.ven_versions_enabled) {
            const available = snapshot.inventory_complete && snapshot.versions_collected === true;
            kpi(content, 'Installed VENs', available ? count(snapshot.ven_count) : 'Unavailable', available ? 'Distinct installed VENs; per-version totals in estate details' : 'Awaiting a complete VEN version observation');
        }
        if (options.issue_aging_enabled) kpi(content, 'Observed active issues', count((view.issues || []).filter(issue => issue.active).length), 'First/last observed and recurring issue history');
        if (options.label_hygiene_enabled) {
            const requirements = options.required_label_types || [];
            const complete = snapshot.inventory_complete && snapshot.labels_complete === true;
            const observed = snapshot.labels_assessed > 0;
            const value = !requirements.length ? 'Not configured' : complete ? count((snapshot.label_gaps || []).length) : observed ? count((snapshot.label_gaps || []).length) + ' observed (incomplete)' : 'Unavailable';
            kpi(content, 'Missing required labels', value, !requirements.length ? 'Select required label types in Settings' : complete ? requirements.join(', ') : 'Incomplete assessment; ' + count(snapshot.labels_assessed) + ' workloads assessed');
        }
        if (options.change_log_enabled) {
            const changes = view.changes_since_report;
            const onDemand = changes?.baseline_kind === 'on_demand';
            const baselineName = onDemand ? 'on-demand report baseline' : 'last weekly report';
            let value = 'Awaiting report';
            let description = options.weekly_report_enabled ? 'Save a completed-week snapshot, or choose Run report now with the change-baseline checkbox selected, to establish a baseline.' : 'No saved report baseline. Enable Weekly Estate Report in Settings, then save a weekly snapshot or explicitly select an on-demand change baseline.';
            if (changes?.status === 'available' && Number.isFinite(changes.count) && changes.count >= 0) {
                value = count(changes.count) + (changes.partial ? ' observed (incomplete)' : '');
                description = changes.since ? 'Since ' + timestamp(changes.since) : 'Changes observed after the ' + (onDemand ? 'on-demand report baseline' : 'last saved weekly report');
                if (onDemand) description += ' · Explicitly selected on-demand baseline' + (changes.observation_at ? ' · Observation: ' + timestamp(changes.observation_at) : '');
                else if (changes.report_week) description += ' · Report week: ' + changes.report_week;
            } else if (changes && changes.status !== 'awaiting_report') {
                value = 'Unavailable';
                description = 'Unable to determine changes since the ' + baselineName + '.';
            }
            const card = kpi(content, 'Changes since ' + baselineName, value, description);
            if (changes?.warning) note(card, changes.warning, true);
        }
        if (options.weekly_report_enabled) kpi(content, 'Weekly Estate Report', 'Enabled', 'Includes deployed VEN version totals');
        if (options.inventory_enabled && content.children.length === 0) note(content, inventorySummary(snapshot));
    }
    function renderInventory(parent, snapshot) {
        const section = panel(parent, 'Workload Inventory', 'estate-inventory', { defaultOpen: false, key: 'illumio_estate_inventory_section_v2' });
        note(section, inventorySummary(snapshot));
        if (!snapshot?.inventory_complete) return;
        const grid = node('div', undefined, 'estate-grid');
        section.appendChild(grid);
        kpi(grid, 'Total workloads', count(snapshot.total));
        kpi(grid, 'Managed', count(snapshot.managed), 'Management is separate from enforcement');
        kpi(grid, 'Unmanaged', count(snapshot.unmanaged));
        table(section, ['Enforcement mode', 'Workloads'], Object.entries(snapshot.modes || {}).sort().map(([mode, total]) => [mode, count(total)]));
        const list = panel(section, 'Workload list', 'estate-workload-list', { defaultOpen: false, key: 'illumio_estate_workload_list_v1', className: 'estate-section' });
        const controls = node('label', 'Show workloads: ', 'estate-intro');
        const filter = node('select');
        filter.setAttribute('aria-label', 'Workload management filter');
        ['all', 'managed', 'unmanaged'].forEach(value => { const option = node('option', value[0].toUpperCase() + value.slice(1)); option.value = value; filter.appendChild(option); });
        if (inventoryFilter === null) {
            const requested = new URLSearchParams(window.location.search).get('inventory');
            let saved;
            try { saved = sessionStorage.getItem('illumio_estate_inventory_filter'); } catch (_) {}
            inventoryFilter = ['managed', 'unmanaged'].includes(requested) ? requested : ['managed', 'unmanaged'].includes(saved) ? saved : 'all';
        }
        filter.value = inventoryFilter;
        controls.appendChild(filter); list.appendChild(controls);
        const results = node('div'); list.appendChild(results);
        let listRendered = false;
        function renderWorkloads() {
            listRendered = true;
            results.replaceChildren();
            const workloads = (snapshot.workloads || []).filter(workload => filter.value === 'all' || (filter.value === 'managed' ? workload.managed : !workload.managed));
            table(results, ['Workload', 'Management', 'Enforcement', 'VEN version'], workloads.map(workload => [workload.name || workload.id, workload.managed ? 'Managed' : 'Unmanaged', workload.mode || 'Unknown', workload.managed ? workload.ven_version || 'Unknown' : 'Not applicable']), 'No matching workloads in this observation.');
        }
        filter.addEventListener('change', () => {
            inventoryFilter = filter.value;
            try { sessionStorage.setItem('illumio_estate_inventory_filter', inventoryFilter); } catch (_) {}
            renderWorkloads();
        });
        list.addEventListener('toggle', () => { if (list.open && !listRendered) renderWorkloads(); });
        if (list.open) renderWorkloads();
    }
    function renderPolicy(parent, snapshot) {
        const section = panel(parent, 'Active & Draft Policy');
        const policy = snapshot?.policy;
        if (!policy) { note(section, 'Policy data is not yet available.'); return; }
        note(section, 'Active and draft are separate versions of policy objects. They are not added together as a unique policy total. Draft inventory includes unchanged objects and is not the pending-change count.');
        table(section, ['Policy version', 'Rulesets', 'Rules', 'Collection'], ['active', 'draft'].map(key => {
            const value = policy[key] || {};
            return [key === 'active' ? 'Active' : 'Draft', value.complete ? count(value.rulesets) : '—', value.complete ? count(value.rules) : '—', value.complete ? 'Complete' : 'Unavailable / incomplete'];
        }));
        note(section, 'Last observed provisioning: ' + timestamp(policy.last_provisioned_at) + ' · Policy observation: ' + timestamp(policy.at));
        new Set([...(policy.warnings || []), ...(policy.pending_counts?.warnings || [])]).forEach(warning => note(section, warning, true));
        section.appendChild(node('h3', 'Pending changes'));
        const pendingGrid = node('div', undefined, 'estate-grid');
        section.appendChild(pendingGrid);
        pendingPolicyCards(pendingGrid, policy);
        if (!policy.pending_complete) note(section, 'Pending changes are unavailable or incomplete. The list below is not a confirmed total.', true);
        table(section, ['Object', 'Kind', 'Change', 'Updated'], (policy.pending || []).map(change => [change.name || change.id, change.kind, change.operation, timestamp(change.updated_at)]), policy.pending_complete ? 'No pending changes observed.' : 'Pending changes unavailable.');
    }
    function renderPage(view) {
        const parent = byID('estate-live-sections');
        if (!parent) return;
        const options = view.options || {};
        const snapshot = view.snapshot;
        const enabled = featureNames.some(key => options[key]);
        byID('estate-disabled').hidden = enabled;
        byID('estate-report-section').hidden = !options.weekly_report_enabled;
        if (!options.weekly_report_enabled) invalidateReport();
        if (!reportSelectionsInitialized && options.weekly_report_enabled) {
            const selected = options.weekly_sections || ['estate', 'health', 'policy', 'traffic', 'changes', 'quality'];
            document.querySelectorAll('#estate-report-sections input').forEach(input => { input.checked = selected.includes(input.value); });
            reportSelectionsInitialized = true;
        }
        parent.replaceChildren();
        byID('estate-status').textContent = !enabled ? 'All optional estate features are disabled.' : snapshot ? 'Latest estate observation: ' + timestamp(snapshot.at) + ' · Tracking since: ' + timestamp(view.started_at) : 'Waiting for the first estate collection. Your existing dashboard monitoring continues normally.';
        (view.warnings || []).forEach(warning => note(parent, warning, true));
        (snapshot?.warnings || []).forEach(warning => note(parent, warning, true));
        if (options.inventory_enabled) renderInventory(parent, snapshot);
        if (options.policy_enabled) renderPolicy(parent, snapshot);
        if (options.ven_versions_enabled) {
            const section = panel(parent, 'VEN Version Distribution');
            note(section, 'Totals count distinct installed VENs per reported version. A VEN associated with multiple workloads is counted once. Unknown versions are included explicitly; unmanaged workloads are not assigned a VEN version.');
            if (!snapshot?.inventory_complete) note(section, 'Version distribution is unavailable until a complete workload collection is available.', true);
            else if (snapshot.versions_collected !== true) note(section, 'Awaiting VEN version data. The retained inventory predates version collection; it does not establish a zero VEN count.');
            else {
                note(section, 'Distinct installed VENs: ' + count(snapshot.ven_count));
                table(section, ['VEN version', 'Distinct installed VENs'], (snapshot.versions || []).map(version => [version.version || 'Unknown', count(version.count)]), snapshot.ven_count === 0 ? 'No installed VENs observed.' : 'Version totals are unavailable for the observed VENs.');
            }
        }
        if (options.issue_aging_enabled) {
            const section = panel(parent, 'Issue Aging & Recurrence');
            note(section, 'Times are first and last observations, not the exact onset of an incident. Collection gaps do not prove recovery. Occurrences represent observed issue episodes.');
            exportCSVControl(section, view, 'issues');
            table(section, ['Workload', 'Issue', 'State', 'Observed age', 'First observed', 'Last observed', 'Occurrences', 'Observed days'], (view.issues || []).map(issue => [issue.name || issue.workload_id, issue.kind, issue.active ? 'Active' : 'Resolved: ' + timestamp(issue.resolved_at), observedDuration(issue.first_seen, issue.last_seen), timestamp(issue.first_seen), timestamp(issue.last_seen), count(issue.occurrences), count((issue.observed_days || []).length)]), snapshot ? 'No issues have been observed since tracking was enabled.' : 'Waiting for the first issue observation.');
        }
        if (options.label_hygiene_enabled) {
            const section = panel(parent, 'Label Hygiene');
            const required = options.required_label_types || [];
            note(section, 'Required label types: ' + (required.join(', ') || 'None configured. Select required types in Settings.'));
            exportCSVControl(section, view, 'labels');
            if (required.length) {
                const complete = snapshot?.inventory_complete && snapshot.labels_complete === true;
                const assessed = snapshot?.labels_assessed;
                if (!complete) note(section, assessed > 0 ? 'Incomplete label assessment: ' + count(assessed) + ' of ' + count(snapshot.total) + ' workloads assessed. Unresolved label references or collection gaps are unknown, not evidence of missing or present labels.' : 'Awaiting a complete label assessment. Unresolved label references or uncollected label data cannot establish that workloads have all required labels.', true);
                if (complete || assessed > 0) table(section, ['Workload', 'Missing label types'], (snapshot.label_gaps || []).map(gap => [gap.name || gap.id, (gap.missing || []).join(', ')]), complete ? 'All observed workloads have the configured required label types.' : 'No missing labels found among the ' + count(assessed) + ' assessed workloads. Unassessed workloads remain unknown.');
            }
        }
        if (options.change_log_enabled) {
            const section = panel(parent, 'Estate Change Log');
            note(section, 'Observed differences between successful collections. The initial inventory establishes a baseline; it is not treated as a wave of new workloads.');
            table(section, ['Observed', 'Workload / object', 'Change', 'Before', 'After'], (view.changes || []).map(change => [timestamp(change.observed_at || change.at), change.name || change.workload_id, change.kind, change.before || '—', change.after || '—']), snapshot ? 'No estate changes observed yet.' : 'Waiting for the initial baseline.');
        }
    }
    async function checked(response) {
        if (!response.ok) throw new Error((await response.text()).trim() || 'Request failed (' + response.status + ').');
        return response;
    }
    async function load(force = false) {
        if (loading && force !== true) return;
        const generation = ++loadGeneration;
        // A saved report may have advanced the change baseline. Its refresh
        // must not wait for, or be overwritten by, a pre-save response.
        if (force === true) ++reportRunsGeneration;
        loading = true;
        try {
            const view = await (await checked(await fetch('/api/estate', { cache: 'no-store' }))).json();
            if (generation !== loadGeneration) return;
            lastView = view;
            renderDashboard(lastView); renderPage(lastView);
            if (byID('estate-page') && lastView.options?.weekly_report_enabled) loadReportRuns();
        } catch (error) {
            if (generation !== loadGeneration) return;
            const status = byID('estate-status');
            if (status) status.textContent = 'Unable to refresh estate insights: ' + error.message + (lastView ? ' Previous observations remain displayed.' : '');
            const inventory = byID('estate-inventory-breakdown');
            if (inventory && lastView?.options?.inventory_enabled) inventory.textContent = 'Estate refresh unavailable; open Estate for details.';
        } finally { if (generation === loadGeneration) loading = false; }
    }
    function selectedSections() { return [...document.querySelectorAll('#estate-report-sections input:checked')].map(input => input.value); }
    function theme() { return document.documentElement.dataset.unifiedTheme === 'dark' ? 'dark' : 'light'; }
    async function loadReportRuns() {
        const target = byID('estate-report-runs');
        if (!target) return;
        const generation = ++reportRunsGeneration;
        try {
            const result = await (await checked(await fetch('/api/estate/report/runs', { cache: 'no-store' }))).json();
            if (generation !== reportRunsGeneration) return;
            target.replaceChildren();
            if (!(result.runs || []).length) { note(target, 'No estate reports have been saved. Use Run report now, Save weekly snapshot, or enable scheduled reports in Settings.'); return; }
            (result.runs || []).forEach(run => {
                const row = node('div', undefined, 'estate-kpi');
                const onDemand = run.kind === 'on_demand';
                const link = node('a', onDemand ? 'Download on-demand report · ' + timestamp(run.observation_at || run.created_at) : 'Download week of ' + run.week);
                link.href = '/api/estate/report/runs?' + new URLSearchParams({ id: run.id, format: 'html', theme: theme() }).toString();
                link.download = reportFilename(run.kind, run.observation_at || run.created_at, run.week, run.id);
                row.appendChild(link);
                note(row, timestamp(run.created_at) + ' · ' + run.state);
                if (onDemand) note(row, run.sets_change_baseline ? 'Explicitly selected as a change baseline.' : 'Change baseline unchanged.');
                note(row, run.message || '');
                target.appendChild(row);
            });
        } catch (error) {
            if (generation !== reportRunsGeneration) return;
            target.replaceChildren(); note(target, 'Report history unavailable: ' + error.message, true);
        }
    }
    function reportQuery(format, onDemand = false) {
        const sections = selectedSections();
        if (!sections.length) throw new Error('Choose at least one report section.');
        const query = new URLSearchParams({ sections: sections.join(','), theme: theme() });
        const week = byID('estate-week-start').value;
        if (!onDemand && week) query.set('week', week);
        if (format) query.set('format', format);
        if (onDemand) query.set('set_baseline', byID('estate-set-baseline').checked === true ? 'true' : 'false');
        return (onDemand ? '/api/estate/report/now?' : '/api/estate/report?') + query.toString();
    }
    function reportFilename(kind, observation, week, id) {
        if (kind !== 'on_demand') return 'Illumio_Weekly_Estate_' + (week || 'report') + '.html';
        const safe = value => String(value || '').replace(/[^a-zA-Z0-9_-]/g, '-');
        return 'Illumio_Estate_On_Demand_' + safe(observation || 'report') + (id ? '_' + safe(id) : '') + '.html';
    }
    function previewReport(saveSnapshot = false) {
        // Click events and theme changes are never permission to save a report.
        return requestReport(saveSnapshot === true ? 'save' : 'weekly');
    }
    function runReportNow() { return requestReport('now'); }
    function refreshReportTheme() {
        if (!reportLoaded) return;
        // A theme change must reopen this frozen run, not create another run or
        // silently substitute the historical report for the same calendar week.
        return requestReport(reportRunID ? 'frozen' : 'weekly', reportRunID);
    }
    async function requestReport(mode, frozenID = '') {
        const saveSnapshot = mode === 'save';
        const onDemand = mode === 'now';
        const mutating = saveSnapshot || onDemand;
        if (mutating && savingReport) return;
        if (mutating) savingReport = true;
        const generation = ++reportGeneration;
        const requestedTheme = theme();
        reportHTML = ''; reportLoaded = false;
        resetEmail();
        byID('estate-download').disabled = true;
        byID('estate-print').disabled = true;
        byID('estate-save-snapshot').disabled = savingReport;
        byID('estate-run-now').disabled = savingReport;
        byID('estate-report-preview').hidden = true;
        byID('estate-report-status').textContent = onDemand ? 'Saving on-demand report from the latest retained observation…' : saveSnapshot ? 'Saving weekly snapshot…' : mode === 'frozen' ? 'Updating the saved report theme…' : 'Preparing weekly report…';
        try {
            const url = mode === 'frozen' ? '/api/estate/report/runs?' + new URLSearchParams({ id: frozenID, format: 'preview', theme: requestedTheme }).toString() : reportQuery('preview', onDemand);
            // Consume this explicit choice for this request, even if its UI is
            // later invalidated or the response fails. A later run opts in anew.
            if (onDemand) byID('estate-set-baseline').checked = false;
            const preview = await (await checked(await fetch(saveSnapshot ? url.replace('/api/estate/report?', '/api/estate/report/snapshot?') : url, { cache: 'no-store', method: mutating ? 'POST' : 'GET' }))).json();
            if (generation !== reportGeneration) { if (mutating) await load(true); return; }
            if (!preview || !preview.report || typeof preview.report !== 'object' || typeof preview.html !== 'string' || !preview.html) throw new Error('The report preview response is incomplete. Refresh and try again.');
            // One frozen response backs the displayed HTML and email payload,
            // even if collection or settings change while the preview loads.
            const metadata = preview.report;
            if ((mutating || metadata.kind === 'on_demand') && (typeof metadata.id !== 'string' || !metadata.id)) throw new Error('The saved report response is incomplete. Check saved report history before trying again.');
            if (onDemand && metadata.kind !== 'on_demand') throw new Error('The on-demand report response is incomplete. Check saved report history before trying again.');
            if (mode === 'frozen' && metadata.id !== frozenID) throw new Error('The saved report response did not match the selected report.');
            const html = preview.html;
            const week = metadata.window?.first_day || byID('estate-week-start').value;
            if (week && metadata.kind !== 'on_demand') byID('estate-week-start').value = week;
            reportHTML = html; reportWeek = week; reportLoaded = true;
            reportRunID = typeof metadata.id === 'string' ? metadata.id : frozenID;
            reportKind = metadata.kind || '';
            reportObservation = metadata.observation_at || metadata.generated_at || '';
            if ((mutating || mode === 'frozen') && Array.isArray(metadata.sections)) {
                // Reopening a frozen week may return its original selection.
                // Keep the checkboxes aligned with the report being exported.
                document.querySelectorAll('#estate-report-sections input').forEach(input => { input.checked = metadata.sections.includes(input.value); });
                reportSelectionsInitialized = true;
            }
            if (window.EstateEmail) {
                try {
                    reportEmail = window.EstateEmail.render(metadata);
                    byID('estate-email-copy').disabled = false;
                    byID('estate-email-preview').disabled = false;
                    byID('estate-email-status').textContent = 'Copy for Email prepares these sections for pasting into Outlook. Nothing is sent automatically.';
                } catch (_) {
                    byID('estate-email-status').textContent = 'The email layout could not be prepared. HTML download and Print / PDF are still available.';
                }
            }
            const frame = byID('estate-report-preview');
            frame.srcdoc = html; frame.hidden = false;
            byID('estate-download').disabled = false;
            byID('estate-print').disabled = false;
            const warnings = metadata.quality?.warnings || [];
            byID('estate-report-status').textContent = 'Report timezone: ' + (metadata.timezone || 'dashboard timezone') + '. ' + (warnings.length ? warnings.join(' ') : 'Report ready. Coverage details are included in the report.');
            if (reportKind === 'on_demand') {
                const expected = metadata.quality?.expected_days;
                byID('estate-report-status').textContent += ' Saved on-demand report · As of ' + timestamp(reportObservation) + '. Uses the latest retained observation, not a fresh PCE query.';
                if (metadata.partial_week) byID('estate-report-status').textContent += ' Current partial week' + (Number.isInteger(expected) ? ' · ' + count(expected) + ' elapsed reporting days' : '') + '; future days are not missing data.';
                if (metadata.quality?.comparison_available === false) byID('estate-report-status').textContent += ' Previous-period comparison is unavailable.';
                byID('estate-report-status').textContent += metadata.sets_change_baseline ? ' Explicitly selected as a change baseline for this run; an existing newer baseline is kept.' : ' Change baseline unchanged.';
            }
            if (saveSnapshot) {
                byID('estate-report-status').textContent += ' Saved weekly snapshot ready. The dashboard counts changes after the newest saved reporting period.';
            }
            if (mutating) await load(true);
            if (generation === reportGeneration && requestedTheme !== theme()) await refreshReportTheme();
        } catch (error) {
            if (generation !== reportGeneration) return;
            byID('estate-report-preview').hidden = true;
            byID('estate-report-status').textContent = (onDemand ? 'Unable to run report now: ' : saveSnapshot ? 'Unable to save weekly snapshot: ' : 'Unable to prepare report: ') + error.message;
        } finally {
            if (mutating) savingReport = false;
            byID('estate-save-snapshot').disabled = savingReport;
            byID('estate-run-now').disabled = savingReport;
        }
    }
    function downloadReport() {
        if (!reportHTML) return;
        const url = URL.createObjectURL(new Blob([reportHTML], { type: 'text/html;charset=utf-8' }));
        const link = node('a'); link.href = url; link.download = reportFilename(reportKind, reportObservation, reportWeek, reportRunID);
        document.body.appendChild(link); link.click(); link.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
    }
    function resetEmail() {
        reportEmail = null;
        byID('estate-email-copy').disabled = true;
        byID('estate-email-preview').disabled = true;
        byID('estate-email-copy-subject').disabled = false;
        byID('estate-email-panel').hidden = true;
        byID('estate-email-panel').open = false;
        byID('estate-email-content').replaceChildren();
        byID('estate-email-subject').value = '';
        byID('estate-email-status').textContent = '';
    }
    function previewEmail() {
        if (!reportEmail) return false;
        // Only our escaping, fixed-markup renderer can supply this fragment.
        // Never insert raw report/PCE fields as HTML here.
        byID('estate-email-content').innerHTML = reportEmail.html;
        byID('estate-email-subject').value = reportEmail.subject;
        const panel = byID('estate-email-panel');
        panel.hidden = false;
        panel.open = true;
        return true;
    }
    function selectEmail() {
        if (!previewEmail()) return;
        const content = byID('estate-email-content');
        try {
            content.focus();
            const selection = window.getSelection();
            const range = document.createRange();
            range.selectNodeContents(content);
            selection.removeAllRanges();
            selection.addRange(range);
            byID('estate-email-status').textContent = 'Report selected. Press Ctrl+C (Command+C on Mac), then paste into your email using Keep Source Formatting.';
        } catch (_) {
            byID('estate-email-status').textContent = 'Select the email-ready report below manually, copy it, and paste it into your email.';
        }
    }
    async function copyEmail(subjectOnly = false) {
        const email = reportEmail;
        if (!email) return;
        const generation = reportGeneration;
        const button = byID(subjectOnly ? 'estate-email-copy-subject' : 'estate-email-copy');
        button.disabled = true;
        let copied = false;
        try {
            if (window.EstateEmailCopy) copied = await window.EstateEmailCopy.copy(subjectOnly ? { text: email.subject } : email);
        } catch (_) { /* Fall back to visible manual copy; never claim success. */ }
        if (generation !== reportGeneration || email !== reportEmail) return;
        button.disabled = false;
        if (copied) {
            byID('estate-email-status').textContent = subjectOnly ? 'Subject copied. Paste it into your email subject line.' : 'Report copied. Paste into an HTML-format Outlook message using Keep Source Formatting. Use Email Preview for the suggested subject.';
        } else if (subjectOnly) {
            previewEmail();
            byID('estate-email-subject').focus();
            byID('estate-email-subject').select();
            byID('estate-email-status').textContent = 'Automatic copying is blocked. Subject selected: press Ctrl+C (Command+C on Mac) to copy it.';
        } else {
            selectEmail();
        }
    }
    function invalidateReport() {
        ++reportGeneration;
        reportHTML = ''; reportLoaded = false; reportRunID = ''; reportKind = ''; reportObservation = '';
        resetEmail();
        byID('estate-download').disabled = true; byID('estate-print').disabled = true;
        byID('estate-save-snapshot').disabled = savingReport;
        byID('estate-run-now').disabled = savingReport;
        byID('estate-report-preview').hidden = true;
        byID('estate-report-status').textContent = 'Selection changed. Preview the report again before exporting.';
    }
    window.EstateUI = { count, timestamp, observedDuration, inventorySummary, renderDashboard, renderPage, selectedSections, reportQuery, reportFilename, load, previewReport, runReportNow, refreshReportTheme, invalidateReport, loadReportRuns, previewEmail, selectEmail, copyEmail };
    function init() {
        if (!byID('estate-page') && !byID('estate-dashboard-row')) return;
        if (byID('estate-page')) {
            byID('estate-refresh').addEventListener('click', load);
            byID('estate-preview').addEventListener('click', () => previewReport());
            byID('estate-save-snapshot').addEventListener('click', () => previewReport(true));
            byID('estate-run-now').addEventListener('click', () => runReportNow());
            byID('estate-download').addEventListener('click', downloadReport);
            byID('estate-email-preview').addEventListener('click', previewEmail);
            byID('estate-email-copy').addEventListener('click', () => copyEmail());
            byID('estate-email-copy-subject').addEventListener('click', () => copyEmail(true));
            byID('estate-email-select').addEventListener('click', selectEmail);
            byID('estate-print').addEventListener('click', () => {
                if (reportHTML) { const target = byID('estate-report-preview').contentWindow; target.focus(); target.print(); }
            });
            byID('estate-week-start').addEventListener('change', invalidateReport);
            document.querySelectorAll('#estate-report-sections input').forEach(input => input.addEventListener('change', invalidateReport));
            let previousTheme = theme();
            new MutationObserver(() => { const current = theme(); if (current !== previousTheme) { previousTheme = current; refreshReportTheme(); if (lastView?.options?.weekly_report_enabled) loadReportRuns(); } }).observe(document.documentElement, { attributes: true, attributeFilter: ['data-unified-theme'] });
        }
        load();
        setInterval(() => { if (document.visibilityState !== 'hidden') load(); }, 300000);
        document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') load(); });
    }
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init, { once: true });
    else init();
})();
