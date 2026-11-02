(function (root) {
    'use strict';

    // Deliberately independent of the DOM, clipboard, theme and live dashboard.
    // Outlook's Word renderer understands these tables and inline styles.
    const unavailable = 'Unavailable';
    const sectionOrder = ['estate', 'health', 'policy', 'traffic', 'changes', 'quality'];
    const font = 'font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:21px;color:#272b2d;';
    const pStyle = font + 'margin:0 0 12px 0;';
    const cellStyle = font + 'border-bottom:1px solid #d7d1c5;padding:9px 10px;vertical-align:top;text-align:left;';
    const muted = 'color:#59615f;';
    const counts = new Intl.NumberFormat('en-US', { maximumFractionDigits: 0 });
    const own = (value, key) => Object.prototype.hasOwnProperty.call(value || {}, key);
    const object = value => value && typeof value === 'object' && !Array.isArray(value) ? value : {};
    const list = value => Array.isArray(value) ? value : [];
    const finite = value => typeof value === 'number' && Number.isFinite(value);
    const validCount = value => finite(value) && value >= 0 && Number.isInteger(value);
    const count = value => validCount(value) ? counts.format(value) : unavailable;
    const signed = value => finite(value) && Number.isInteger(value) ? (value > 0 ? '+' : '') + counts.format(value) : unavailable;
    const days = value => Number.isInteger(value) && value >= 0 && value <= 7 ? value : null;
    const coverage = value => days(value) === null ? unavailable : String(value) + '/7';
    const valueText = value => typeof value === 'string' || typeof value === 'number' ? String(value) : '';
    const display = value => valueText(value).trim() || unavailable;

    function escape(value) {
        return valueText(value).replace(/[&<>"']/g, character => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[character]));
    }

    function calendarDay(value) {
        if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value)) return unavailable;
        const parsed = new Date(value + 'T00:00:00Z');
        return Number.isFinite(parsed.getTime()) && parsed.toISOString().slice(0, 10) === value ? value : unavailable;
    }

    function timestamp(value, timezone) {
        if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) return unavailable;
        const parsed = new Date(value);
        if (!Number.isFinite(parsed.getTime()) || parsed.getUTCFullYear() < 1000) return unavailable;
        const options = { timeZone: timezone, year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' };
        try {
            return new Intl.DateTimeFormat('en-US', options).format(parsed);
        } catch (_) {
            return new Intl.DateTimeFormat('en-US', { ...options, timeZone: 'UTC' }).format(parsed) + ' (report timezone unavailable)';
        }
    }

    function render(input) {
        const report = object(input);
        const selected = new Set(list(report.sections).filter(section => sectionOrder.includes(section)));
        const period = object(report.window);
        const comparison = object(report.previous_window);
        const quality = object(report.quality);
        const timezone = typeof report.timezone === 'string' && report.timezone.trim() ? report.timezone : 'UTC';
        const firstDay = calendarDay(period.first_day);
        const lastDay = calendarDay(period.last_day);
        const subject = 'Illumio Weekly Estate Report: ' + firstDay + ' through ' + lastDay;
        const html = [];
        const text = [];

        function paragraph(value, style) {
            html.push('<p style="' + pStyle + (style || '') + '">' + escape(value).replace(/\r?\n/g, '<br>') + '</p>');
            text.push(valueText(value), '');
        }

        function heading(value, level) {
            const tag = level === 3 ? 'h3' : 'h2';
            html.push('<' + tag + ' style="' + font + 'font-size:' + (level === 3 ? '16' : '20') + 'px;font-weight:bold;margin:22px 0 12px 0;">' + escape(value) + '</' + tag + '>');
            text.push(valueText(value), '');
        }

        function table(headers, rows) {
            html.push('<table width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%;border-collapse:collapse;margin:0 0 16px 0;"><thead><tr>');
            for (const header of headers) html.push('<th scope="col" bgcolor="#eeebe4" style="' + cellStyle + 'background-color:#eeebe4;font-weight:bold;">' + escape(header) + '</th>');
            html.push('</tr></thead><tbody>');
            text.push(headers.join(' | '));
            for (const row of rows) {
                html.push('<tr>');
                for (const cell of row) html.push('<td bgcolor="#ffffff" style="' + cellStyle + 'background-color:#ffffff;">' + escape(cell).replace(/\r?\n/g, '<br>') + '</td>');
                html.push('</tr>');
                text.push(row.map(valueText).join(' | '));
            }
            html.push('</tbody></table>');
            text.push('');
        }

        function notes(values) {
            const seen = new Set();
            for (const raw of list(values)) {
                if (typeof raw !== 'string' || !raw.trim() || seen.has(raw)) continue;
                seen.add(raw);
                paragraph('\u2022 ' + raw, muted);
            }
        }

        html.push('<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#f5f3ee" style="width:100%;border-collapse:collapse;background-color:#f5f3ee;"><tr><td align="center" bgcolor="#f5f3ee" style="padding:16px;background-color:#f5f3ee;">');
        html.push('<table role="presentation" width="720" cellpadding="0" cellspacing="0" border="0" bgcolor="#ffffff" style="width:720px;max-width:100%;border-collapse:collapse;background-color:#ffffff;"><tr><td bgcolor="#ffffff" style="padding:24px;border-top:5px solid #f26322;background-color:#ffffff;' + font + '">');
        paragraph('ILLUMIO MONITORING DASHBOARD', 'font-size:11px;font-weight:bold;letter-spacing:1px;color:#b74610;');
        html.push('<h1 style="' + font + 'font-size:26px;line-height:32px;font-weight:bold;margin:0 0 12px 0;">Weekly Estate Report</h1>');
        text.push('Weekly Estate Report', '');
        paragraph(firstDay + ' through ' + lastDay + ' | ' + display(report.timezone));
        paragraph('Compared with ' + calendarDay(comparison.first_day) + ' through ' + calendarDay(comparison.last_day) + '. Generated ' + timestamp(report.generated_at, timezone) + '.', muted);
        paragraph('Observation coverage: ' + coverage(quality.covered_days) + ' days this week; ' + coverage(quality.previous_covered_days) + ' in the comparison week. Missing periods are unknown, not zero activity. A day with samples does not guarantee continuous coverage.', 'border-left:3px solid #f26322;padding-left:12px;');
        if (list(quality.missing_days).length) paragraph('Missing observation days: ' + quality.missing_days.map(calendarDay).join(', ') + '.', muted);

        if (selected.has('estate')) {
            heading('Estate overview');
            if (!report.estate) {
                paragraph('No complete inventory observation is available for this week. Estate totals and VEN versions are unavailable, not zero.');
            } else {
                const estate = object(report.estate);
                paragraph('Latest retained inventory observation: ' + timestamp(estate.observed_at, timezone) + '. These are observed snapshot values, not exact week-end totals. Management does not imply enforcement.', muted);
                if (estate.previous_observed_at) paragraph('Comparison inventory observation: ' + timestamp(estate.previous_observed_at, timezone) + '.', muted);
                table(['Workloads', 'This week', 'Previous week', 'Change'], [['Total', 'total'], ['Managed', 'managed'], ['Unmanaged', 'unmanaged']].map(([label, key]) => {
                    const metric = object(estate[key]);
                    const hasBaseline = validCount(metric.previous);
                    return [label, count(metric.current), count(metric.previous), hasBaseline && validCount(metric.current) ? signed(metric.change) : unavailable];
                }));
                heading('VEN version distribution', 3);
                if (estate.versions_available !== true) {
                    paragraph('Version distribution was not collected for this observation. It is unavailable, not zero deployed VENs.');
                } else {
                    paragraph('Distinct deployed VENs: ' + count(estate.ven_count) + '. A VEN shared by multiple workload records is counted once. Unknown means the deployed VEN\u2019s version was not returned.', muted);
                    const versions = list(estate.versions);
                    if (versions.length) table(['VEN version', 'Total deployed VENs'], versions.map(raw => {
                        const row = object(raw);
                        return [valueText(row.version).trim() || 'Unknown', count(row.total)];
                    }));
                    else paragraph(estate.ven_count === 0 ? 'No deployed VENs were observed in this inventory snapshot.' : 'Per-version counts are unavailable for this observation.');
                }
                const enforcement = object(estate.enforcement);
                if (Object.keys(enforcement).length) {
                    heading('Managed workload enforcement', 3);
                    table(['Enforcement mode', 'Workloads'], Object.keys(enforcement).sort().map(mode => [display(mode), count(enforcement[mode])]));
                }
            }
        }

        if (selected.has('health')) {
            heading('VEN health');
            paragraph('Counts are distinct workload identities observed in each status during the week, not sums of daily peaks or rolling 24-hour counts. A workload may appear in more than one status. Tampering uses event timestamps. Incomplete collection means observed counts are minimums.', muted);
            const health = object(report.health);
            const current = object(health.unique_affected);
            const previous = object(health.previous_unique_affected);
            const names = { warning: 'Warning', error: 'Error', stopped: 'Stopped', suspended: 'Suspended', tampering: 'Tampering' };
            if (Object.keys(current).length) table(['Status', 'Distinct observed workloads', 'Previous week'], Object.keys(current).sort().map(kind => [own(names, kind) ? names[kind] : display(kind), count(current[kind]), own(previous, kind) ? count(previous[kind]) : unavailable]));
            else paragraph('No successfully collected health history is available for this week. Missing statuses are unavailable, not zero affected workloads.');
        }

        if (selected.has('policy')) {
            heading('Policy overview');
            paragraph('Active and draft are versions of policy objects and must not be added into a single unique total. Counts and pending changes reflect the latest retained observation, not all changes made during the week.', muted);
            if (!report.policy) paragraph('No active/draft policy observation is available for this week.');
            else {
                const policy = object(report.policy);
                const previous = object(report.previous_policy);
                paragraph('Observed ' + timestamp(policy.at, timezone) + '.', muted);
                table(['Policy version', 'Rulesets', 'Previous rulesets', 'Rules', 'Previous rules'], ['active', 'draft'].map(kind => {
                    const currentCount = object(policy[kind]);
                    const previousCount = object(previous[kind]);
                    return [kind === 'active' ? 'Active' : 'Draft', currentCount.complete === true ? count(currentCount.rulesets) : unavailable, previousCount.complete === true ? count(previousCount.rulesets) : unavailable, currentCount.complete === true ? count(currentCount.rules) : unavailable, previousCount.complete === true ? count(previousCount.rules) : unavailable];
                }));
                paragraph('Last observed provisioning time: ' + timestamp(policy.last_provisioned_at, timezone) + '.');
                heading('Pending changes at this observation', 3);
                if (policy.pending_complete !== true) paragraph('Pending changes are unavailable or incomplete.');
                else if (!list(policy.pending).length) paragraph('No pending changes were observed.');
                else table(['Object', 'Kind', 'Change'], policy.pending.map(raw => { const row = object(raw); return [display(row.name || row.id), display(row.kind), display(row.operation)]; }));
                notes(policy.warnings);
            }
        }

        if (selected.has('traffic')) {
            heading('Monitored blocked traffic');
            paragraph('Targets may overlap. Counts are per target after configured exclusions and must not be added into an estate-wide total. Partial weeks cannot be compared with complete weeks.', muted);
            paragraph('Only verified complete-day queries for this PCE, organization, timezone and exclusion settings are included. Legacy history without this provenance is excluded; a successful daily or manual reconciliation can verify missing days.', muted);
            const targets = list(report.traffic);
            if (!targets.length) paragraph('No verified monitored target history is available.');
            else {
                table(['Target', 'Observed total', 'Days', 'Previous complete week', 'Change'], targets.map(raw => {
                    const row = object(raw);
                    const observedDays = days(row.covered_days);
                    const completePrevious = row.previous_covered_days === 7;
                    const total = observedDays !== null && observedDays > 0 ? count(row.total) : unavailable;
                    return [display(row.target), total + (observedDays > 0 && observedDays < 7 ? ' (partial)' : ''), coverage(row.covered_days), completePrevious ? count(row.previous) : unavailable, observedDays === 7 && completePrevious && validCount(row.previous) && validCount(row.total) ? signed(row.change) : unavailable];
                }));
                for (const raw of targets) { const row = object(raw); if (list(row.missing_days).length) paragraph('Missing verified days for ' + display(row.target) + ': ' + row.missing_days.map(calendarDay).join(', ') + '.', muted); }
            }
        }

        if (selected.has('changes')) {
            heading('Estate change log');
            paragraph('Observed changes since a retained baseline. Initial inventory is not classified as newly added workloads; changes between collection samples may be missed.', muted);
            if (!list(report.changes).length) paragraph('No retained changes are available for this week. A disabled or incomplete change log does not establish that no changes occurred.');
            else table(['Observed', 'Workload / object', 'Change', 'Before', 'After'], report.changes.map(raw => {
                const row = object(raw);
                return [timestamp(row.at, timezone), display(row.name || row.workload_id), display(row.kind), display(row.before), display(row.after)];
            }));
        }

        if (selected.has('quality')) {
            heading('Data quality and scope');
            table(['Observation window', 'Timestamp'], [['First observation', timestamp(quality.first_observation, timezone)], ['Last observation', timestamp(quality.last_observation, timezone)]]);
            notes(quality.warnings);
        } else if (list(quality.warnings).some(value => typeof value === 'string' && value.trim())) {
            // Section selection must not suppress warnings that qualify totals.
            paragraph('Important data notes', 'font-weight:bold;');
            notes(quality.warnings);
        }
        paragraph('Illumio Monitoring Dashboard ' + display(report.version) + ' | Frozen report generated ' + timestamp(report.generated_at, timezone) + '.', 'border-top:1px solid #d7d1c5;padding-top:14px;font-size:11px;color:#59615f;');
        html.push('</td></tr></table></td></tr></table>');
        return { subject, html: html.join(''), text: text.join('\n').replace(/\n{3,}/g, '\n\n').trim() };
    }

    root.EstateEmail = Object.freeze({ render });
})(window);
