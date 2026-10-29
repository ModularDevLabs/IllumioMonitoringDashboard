(function (root, factory) {
    const api = factory();
    if (typeof module === 'object' && module.exports) module.exports = api;
    if (root) root.ITTCSVImport = api;
})(typeof window !== 'undefined' ? window : globalThis, function () {
    'use strict';

    const endpoint = '/blocked-traffic/api/results/import-csv';
    const activeButtons = new WeakSet();

    function formatBytes(value) {
        const bytes = Number.isFinite(Number(value)) && Number(value) > 0 ? Number(value) : 0;
        if (bytes < 1024) return `${Math.round(bytes)} B`;
        const units = ['KiB', 'MiB', 'GiB', 'TiB'];
        let amount = bytes / 1024;
        let unit = units[0];
        for (let index = 1; index < units.length && amount >= 1024; index += 1) {
            amount /= 1024;
            unit = units[index];
        }
        return `${amount.toFixed(1)} ${unit}`;
    }

    function selectionDetails(files) {
        const selected = Array.from(files || []);
        const totalBytes = selected.reduce((sum, file) => {
            const size = Number(file && file.size);
            return sum + (Number.isFinite(size) && size > 0 ? size : 0);
        }, 0);
        const fileLabel = `${selected.length} CSV file${selected.length === 1 ? '' : 's'}`;
        return {
            files: selected,
            fileCount: selected.length,
            totalBytes,
            label: `${fileLabel} (${formatBytes(totalBytes)} total)`,
        };
    }

    function importError(message, code) {
        const error = new Error(message);
        error.code = code;
        return error;
    }

    function responsePayload(xhr) {
        const raw = typeof xhr.responseText === 'string' ? xhr.responseText.trim() : '';
        if (!raw) return { payload: null, parseError: true };
        try {
            return { payload: JSON.parse(raw), parseError: false };
        } catch (_) {
            return { payload: null, parseError: true };
        }
    }

    function responseError(xhr, parsed) {
        const status = Number(xhr.status) || 0;
        const serverDetail = parsed.payload && typeof parsed.payload.error === 'string'
            ? parsed.payload.error.trim()
            : '';
        if (status === httpStatusRequestEntityTooLarge) {
            const detail = serverDetail ? ` Server message: ${serverDetail}` : '';
            return importError(
                `CSV import failed (HTTP 413): the server rejected the upload as too large. ` +
                `Confirm you are running the updated build and check any reverse-proxy upload limits.${detail}`,
                'HTTP_413',
            );
        }
        if (status < 200 || status >= 300) {
            const detail = serverDetail ? `: ${serverDetail}` : '.';
            return importError(`CSV import failed (HTTP ${status || 'unknown'})${detail}`, 'HTTP_ERROR');
        }
        if (parsed.parseError || !parsed.payload || typeof parsed.payload !== 'object') {
            return importError(
                `CSV import failed: the server returned an unreadable response (HTTP ${status}). ` +
                'Check the application logs and try again.',
                'INVALID_JSON',
            );
        }
        if (!parsed.payload.success) {
            return importError(
                `CSV import failed: ${serverDetail || 'the server did not provide an error message.'}`,
                'IMPORT_REJECTED',
            );
        }
        return null;
    }

    const httpStatusRequestEntityTooLarge = 413;

    function upload(options) {
        const settings = options || {};
        const selection = selectionDetails(settings.files);
        if (selection.fileCount === 0) {
            return Promise.reject(importError('Choose one or more CSV files first.', 'NO_FILES'));
        }

        const button = settings.button || null;
        if (button && activeButtons.has(button)) {
            return Promise.reject(importError('A CSV import is already in progress.', 'IMPORT_IN_PROGRESS'));
        }

        const xhrFactory = settings.xhrFactory || (() => new XMLHttpRequest());
        const formDataFactory = settings.formDataFactory || (() => new FormData());
        const onStatus = typeof settings.onStatus === 'function' ? settings.onStatus : () => {};
        const uploadEndpoint = settings.endpoint || endpoint;
        const originalButtonText = button ? button.textContent : '';

        function report(message, details) {
            try {
                onStatus(message, Object.assign({
                    fileCount: selection.fileCount,
                    totalBytes: selection.totalBytes,
                }, details || {}));
            } catch (_) {
                // A page-level status renderer must not strand an active upload.
            }
        }

        if (button) {
            activeButtons.add(button);
            button.disabled = true;
            button.textContent = 'Importing...';
        }
        report(`Preparing ${selection.label}...`, { stage: 'preparing', percent: 0 });

        return new Promise((resolve, reject) => {
            let settled = false;
            let xhr;

            function finish(error, payload) {
                if (settled) return;
                settled = true;
                if (button) {
                    activeButtons.delete(button);
                    button.disabled = false;
                    button.textContent = originalButtonText;
                }
                if (error) reject(error);
                else resolve(payload);
            }

            try {
                const formData = formDataFactory();
                selection.files.forEach((file) => formData.append('files', file));
                Object.entries(settings.fields || {}).forEach(([key, value]) => {
                    formData.append(key, value == null ? '' : String(value));
                });

                xhr = xhrFactory();
                xhr.open('POST', uploadEndpoint, true);
                // Large local imports can legitimately take several minutes. The
                // server owns request cancellation; the browser adds no timeout.
                xhr.timeout = 0;

                xhr.upload.onprogress = (event) => {
                    if (settled) return;
                    let percent = null;
                    let transferred = '';
                    if (event.lengthComputable && event.total > 0) {
                        const ratio = Math.max(0, Math.min(1, event.loaded / event.total));
                        percent = Math.round(ratio * 100);
                        transferred = ` (${formatBytes(selection.totalBytes * ratio)} of ${formatBytes(selection.totalBytes)})`;
                    }
                    const progress = percent == null ? '' : `: ${percent}%${transferred}`;
                    report(`Uploading ${selection.label}${progress}...`, { stage: 'uploading', percent });
                };
                xhr.upload.onload = () => {
                    if (!settled) {
                        report(`Upload complete for ${selection.label}; the server is analyzing the CSV data...`, {
                            stage: 'analyzing',
                            percent: 100,
                        });
                    }
                };
                xhr.onload = () => {
                    const parsed = responsePayload(xhr);
                    const error = responseError(xhr, parsed);
                    if (error) {
                        report(error.message, { stage: 'error', percent: null });
                        finish(error);
                        return;
                    }
                    report(`CSV import complete for ${selection.label}.`, { stage: 'complete', percent: 100 });
                    finish(null, parsed.payload);
                };
                xhr.onerror = () => {
                    const error = importError(
                        'CSV import failed: the connection to the local application was lost before the import finished. ' +
                        'Confirm it is still running, check its logs and available memory/disk space, then try again.',
                        'NETWORK_ERROR',
                    );
                    report(error.message, { stage: 'error', percent: null });
                    finish(error);
                };
                xhr.onabort = () => {
                    const error = importError('CSV import was cancelled before it completed.', 'ABORTED');
                    report(error.message, { stage: 'error', percent: null });
                    finish(error);
                };
                xhr.ontimeout = () => {
                    const error = importError(
                        'CSV import timed out before the server responded. Confirm the application is still running and try again.',
                        'TIMEOUT',
                    );
                    report(error.message, { stage: 'error', percent: null });
                    finish(error);
                };
                xhr.send(formData);
            } catch (error) {
                const wrapped = importError(`CSV import could not start: ${error.message || String(error)}`, 'START_ERROR');
                report(wrapped.message, { stage: 'error', percent: null });
                finish(wrapped);
            }
        });
    }

    return {
        endpoint,
        formatBytes,
        selectionDetails,
        upload,
    };
});
