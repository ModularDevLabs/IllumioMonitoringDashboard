(function (global) {
    'use strict';

    function textCell(value) {
        let text = String(value);
        // Excel can evaluate formulas even when a CSV field is quoted, and can
        // ignore leading whitespace/control characters before the operator.
        if (/^[=+\-@\s\u0000-\u001f\u007f\uFEFF]/u.test(text)) text = "'" + text;
        return text;
    }
    function cell(value) {
        if (value === null || value === undefined) return '';
        const raw = typeof value === 'number' ? (Number.isFinite(value) ? String(value) : '') :
            typeof value === 'boolean' ? String(value) : textCell(value);
        return /[",\r\n]/.test(raw) ? '"' + raw.replaceAll('"', '""') + '"' : raw;
    }
    function serialize(headers, rows) {
        return '\uFEFF' + [headers, ...rows].map(row => row.map(cell).join(',')).join('\r\n') + '\r\n';
    }
    function utc(value) {
        if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T.*(?:Z|[+-]\d{2}:\d{2})$/i.test(value)) return '';
        const parsed = new Date(value);
        return Number.isFinite(parsed.getTime()) && parsed.getUTCFullYear() >= 1970 ? parsed.toISOString() : '';
    }
    function numeric(value) { return Number.isFinite(value) ? value : null; }
    function flag(value) { return typeof value === 'boolean' ? value : null; }
    function ageHours(first, last) {
        const start = utc(first), end = utc(last);
        if (!start || !end) return null;
        const elapsed = new Date(end).getTime() - new Date(start).getTime();
        return elapsed >= 0 ? elapsed / 3600000 : null;
    }
    function disabled(reason) { return { available: false, reason, partial: false, rowCount: 0, filename: '', content: '' }; }
    function descriptor(name, snapshot, length, partial) {
        const date = utc(snapshot.at).slice(0, 10) || 'undated';
        return {
            available: true, reason: '', partial, rowCount: length,
            filename: 'Illumio_' + name + '_' + date + (partial ? '_partial' : '') + '.csv', content: ''
        };
    }
    function labelHygiene(view, options) {
        const snapshot = view?.snapshot;
        const required = view?.options?.required_label_types;
        if (view?.options?.label_hygiene_enabled !== true) return disabled('Enable label hygiene to export its observed results.');
        if (!Array.isArray(required) || !required.length) return disabled('Choose required label types before exporting a label assessment.');
        if (!snapshot || (snapshot.labels_complete !== true && !(snapshot.labels_assessed > 0))) return disabled('Label assessment is unavailable. Wait for observed results before exporting.');
        const rows = snapshot.label_gaps;
        if (!Array.isArray(rows) || !rows.length) return disabled(snapshot.labels_complete === true && snapshot.inventory_complete === true ? 'There are no missing-label rows to export.' : 'There are no observed missing-label rows to export; unassessed workloads remain unknown.');
        const partial = snapshot.inventory_complete !== true || snapshot.labels_complete !== true || !utc(snapshot.at);
        const result = descriptor('Label_Hygiene', snapshot, rows.length, partial);
        if (options?.includeContent === false) return result;
        const status = partial ? 'Partial observed results; unassessed workloads remain unknown' : 'Complete assessment';
        result.content = serialize([
            'Workload', 'Workload ID', 'Missing label types', 'Required label types', 'Assessment',
            'Assessed workloads', 'Total workloads', 'Inventory complete', 'Labels complete', 'Observed at (UTC)'
        ], rows.map(gap => [
            gap.name, gap.id, Array.isArray(gap.missing) ? gap.missing.join('; ') : null, required.join('; '), status,
            numeric(snapshot.labels_assessed), numeric(snapshot.total), flag(snapshot.inventory_complete), flag(snapshot.labels_complete), utc(snapshot.at)
        ]));
        return result;
    }
    function issues(view, options) {
        const snapshot = view?.snapshot;
        if (view?.options?.issue_aging_enabled !== true) return disabled('Enable issue aging to export its observed results.');
        if (!snapshot) return disabled('Issue observations are unavailable. Wait for the first collection before exporting.');
        const rows = view.issues;
        if (!Array.isArray(rows) || !rows.length) return disabled('There are no observed issue rows to export.');
        const complete = snapshot.health_complete || {};
        const partial = !utc(snapshot.at) || ['warning', 'error', 'stopped', 'suspended'].some(kind => complete[kind] !== true);
        const result = descriptor('Issue_Aging', snapshot, rows.length, partial);
        if (options?.includeContent === false) return result;
        result.content = serialize([
            'Workload', 'Workload ID', 'Issue ID', 'Status', 'Current observed state',
            'First observed (UTC)', 'Last observed (UTC)', 'Resolved (UTC)', 'Observed age (hours)',
            'Occurrences', 'Observed days (count)', 'Observed days (dates)', 'Status collection complete',
            'Assessment', 'Snapshot observed at (UTC)'
        ], rows.map(issue => [
            issue.name, issue.workload_id, issue.id, issue.kind,
            issue.active === true ? 'Active' : issue.active === false ? 'Resolved' : 'Unknown',
            utc(issue.first_seen), utc(issue.last_seen), utc(issue.resolved_at), ageHours(issue.first_seen, issue.last_seen),
            numeric(issue.occurrences), Array.isArray(issue.observed_days) ? issue.observed_days.length : null,
            Array.isArray(issue.observed_days) ? issue.observed_days.join('; ') : null,
            flag(complete[issue.kind]), partial ? 'Partial collection; retained observations only' : 'Complete current collection; retained observations', utc(snapshot.at)
        ]));
        return result;
    }
    function download(result) {
        if (!result?.available || !result.content || !global.document?.body || typeof global.URL?.createObjectURL !== 'function') return false;
        let url, link;
        try {
            url = global.URL.createObjectURL(new global.Blob([result.content], { type: 'text/csv;charset=utf-8' }));
            link = global.document.createElement('a');
            link.href = url; link.download = result.filename;
            global.document.body.appendChild(link); link.click();
            global.setTimeout(() => global.URL.revokeObjectURL(url), 1000);
            return true;
        } catch (_) {
            if (url) { try { global.URL.revokeObjectURL(url); } catch (_) {} }
            return false;
        } finally {
            if (link) link.remove();
        }
    }

    global.EstateCSV = { serialize, utc, ageHours, labelHygiene, issues, download };
})(window);
