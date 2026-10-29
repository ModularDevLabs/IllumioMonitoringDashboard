(function (root, factory) {
    'use strict';
    const createStore = factory();
    if (typeof module === 'object' && module.exports) module.exports = { createStore };
    if (!root || !root.document) return;

    let storage;
    try { storage = root.sessionStorage; } catch (_) {}
    // Keep standalone and embedded extractor views separate, even on one origin.
    const scriptPath = new URL(root.document.currentScript.src, root.location.href).pathname;
    const scope = scriptPath.replace(/\/assets\/analysis-state\.js$/, '') || 'standalone';
    root.ITTAnalysisState = createStore({
        storage,
        scope,
        onActivate(detail) {
            root.document.dispatchEvent(new root.CustomEvent('itt:analysis-activated', { detail }));
        },
    });
})(typeof window !== 'undefined' ? window : undefined, function () {
    'use strict';

    function object(value) {
        return value !== null && typeof value === 'object' && !Array.isArray(value);
    }
    function copy(value) {
        return JSON.parse(JSON.stringify(value));
    }

    return function createStore(options = {}) {
        const key = 'ittAnalysisView:v1:' + (options.scope || 'standalone');
        let revision = '';
        let memory = null;
        let storageFailed = false;

        function readRecord() {
            if (!storageFailed && options.storage) {
                let raw;
                try {
                    raw = options.storage.getItem(key);
                } catch (_) {
                    storageFailed = true;
                    return memory;
                }
                try {
                    const parsed = raw ? JSON.parse(raw) : null;
                    if (object(parsed) && typeof parsed.revision === 'string' && object(parsed.pages)) {
                        memory = parsed;
                    } else {
                        memory = null;
                    }
                } catch (_) {
                    // Repair malformed data on the next activation; it does not
                    // mean the browser has blocked storage access.
                    memory = null;
                }
            }
            return memory;
        }
        function saveRecord(record) {
            memory = record;
            if (!storageFailed && options.storage) {
                try { options.storage.setItem(key, JSON.stringify(record)); }
                catch (_) { storageFailed = true; }
            }
        }
        function activate(payload) {
            const next = typeof payload?.analysisRevision === 'string' ? payload.analysisRevision : '';
            const previous = readRecord();
            const changed = previous?.revision !== next;
            revision = next;
            if (next && changed) saveRecord({ revision: next, pages: {} });
            if (typeof options.onActivate === 'function') {
                options.onActivate({ revision, changed });
            }
            return changed;
        }
        function read(page, fallback = {}) {
            const record = readRecord();
            if (!revision || record?.revision !== revision || !Object.hasOwn(record.pages, page)) return copy(fallback);
            return copy(record.pages[page]);
        }
        function write(page, value) {
            if (!revision || typeof page !== 'string' || !page || ['__proto__', 'constructor', 'prototype'].includes(page)) return false;
            const record = readRecord();
            // A stale page (for example, a back/forward cache entry) cannot put
            // old selections back after another page has loaded new analysis.
            if (record?.revision !== revision) return false;
            try {
                record.pages[page] = copy(value);
                saveRecord(record);
                return true;
            } catch (_) { return false; }
        }
        return { activate, read, write, currentRevision: () => revision };
    };
});
