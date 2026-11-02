(function () {
    'use strict';
    const features = {
        inventory_enabled: 'estate-inventory-enabled', policy_enabled: 'estate-policy-enabled',
        issue_aging_enabled: 'estate-aging-enabled', label_hygiene_enabled: 'estate-labels-enabled',
        ven_versions_enabled: 'estate-versions-enabled', change_log_enabled: 'estate-changes-enabled',
        weekly_report_enabled: 'estate-report-enabled', weekly_schedule_enabled: 'estate-schedule-enabled',
        weekly_webhook_enabled: 'estate-webhook-enabled'
    };
    const byID = id => document.getElementById(id);
    function labelTypes(raw) {
        return [...new Set(String(raw || '').split(/[,\n]/).map(value => value.trim()).filter(Boolean))];
    }
    function populate(options) {
        Object.entries(features).forEach(([key, id]) => { byID(id).checked = options[key] === true; });
        byID('estate-required-labels').value = (options.required_label_types || []).join(', ');
        byID('estate-history-days').value = options.history_days || 90;
        byID('estate-report-weekday').value = options.weekly_day || 'monday';
        byID('estate-report-time').value = options.weekly_time || '09:00';
        byID('estate-report-timezone').textContent = 'Dashboard timezone: ' + (options.effective_timezone || 'system timezone');
        byID('estate-webhook-provider').value = options.weekly_webhook_provider || 'generic';
        byID('estate-webhook-url').value = '';
        byID('estate-clear-webhook').checked = false;
        byID('estate-webhook-help').textContent = options.weekly_webhook_configured || options.weekly_webhook_url
            ? 'A webhook URL is saved. Leave blank to keep it, enter a replacement, or select Clear below.'
            : 'No webhook URL is saved. Webhook URLs are secrets and are not displayed.';
        const sections = options.weekly_sections || ['estate', 'health', 'policy', 'traffic', 'changes', 'quality'];
        document.querySelectorAll('#estate-scheduled-sections input').forEach(input => { input.checked = sections.includes(input.value); });
    }
    function payload() {
        const options = {};
        Object.entries(features).forEach(([key, id]) => { options[key] = byID(id).checked; });
        options.required_label_types = labelTypes(byID('estate-required-labels').value);
        options.history_days = Number(byID('estate-history-days').value);
        options.weekly_day = byID('estate-report-weekday').value;
        options.weekly_time = byID('estate-report-time').value;
        options.weekly_webhook_provider = byID('estate-webhook-provider').value;
        options.weekly_sections = [...document.querySelectorAll('#estate-scheduled-sections input:checked')].map(input => input.value);
        const url = byID('estate-webhook-url').value.trim();
        if (byID('estate-clear-webhook').checked) options.weekly_webhook_url = '';
        else if (url) options.weekly_webhook_url = url;
        if (options.weekly_schedule_enabled && !options.weekly_report_enabled) throw new Error('Enable weekly reports before enabling scheduled delivery.');
        if (options.weekly_webhook_enabled && !options.weekly_schedule_enabled) throw new Error('Enable the weekly schedule before enabling webhook delivery.');
        if (options.label_hygiene_enabled && !options.required_label_types.length) throw new Error('Select at least one required label type for label hygiene.');
        if (options.weekly_report_enabled && !options.weekly_sections.length) throw new Error('Select at least one weekly report section.');
        if (!Number.isInteger(options.history_days) || options.history_days < 1 || options.history_days > 3650) throw new Error('Estate history retention must be between 1 and 3650 days.');
        return options;
    }
    async function responseJSON(response) {
        if (!response.ok) {
            const detail = (await response.text()).trim();
            throw new Error(detail || 'Estate settings request failed (' + response.status + ').');
        }
        return response.json();
    }
    function message(text, failed) {
        byID('estate-settings-message').textContent = text;
        byID('estate-settings-message').className = 'small mt-2' + (failed ? ' text-danger' : '');
    }
    async function load() {
        try {
            const options = await responseJSON(await fetch('/api/config/estate', { cache: 'no-store' }));
            populate(options);
            byID('estate-settings-fields').disabled = false;
            message('Choose the features you want, then save. All new features are opt-in.', false);
        } catch (error) { message('Unable to load estate settings: ' + error.message, true); }
    }
    async function save(event) {
        event.preventDefault();
        let options;
        try { options = payload(); } catch (error) { message(error.message, true); return; }
        byID('estate-settings-fields').disabled = true;
        message('Saving estate settings…', false);
        try {
            const saved = await responseJSON(await fetch('/api/config/estate', {
                method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(options)
            }));
            populate(saved);
            message('Estate settings saved. Enabled insights are collected on the next dashboard collection. Weekly coverage builds from that point onward.', false);
        } catch (error) { message('Unable to save estate settings: ' + error.message, true); }
        finally { byID('estate-settings-fields').disabled = false; }
    }
    window.EstateSettings = { labelTypes, populate, payload, load, save };
    function init() {
        const form = byID('estate-settings-form');
        if (!form) return;
        form.addEventListener('submit', save);
        load();
    }
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init, { once: true });
    else init();
})();
