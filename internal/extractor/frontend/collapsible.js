(() => {
    function storageKey(section) {
        const scope = document.body.dataset.collapseScope || window.location.pathname || 'page';
        return `ittCollapse:${scope}:${section.dataset.autoCollapsible}`;
    }

    function apply(section, collapsed, persist = false) {
        const content = section.querySelector(':scope > .auto-collapse-content');
        const button = section.querySelector(':scope > .auto-collapse-toolbar .auto-collapse-toggle');
        const label = button?.querySelector('[data-auto-collapse-label]');
        if (!content || !button || !label) return;
        section.classList.toggle('is-auto-collapsed', collapsed);
        content.hidden = collapsed;
        button.setAttribute('aria-expanded', String(!collapsed));
        label.textContent = collapsed ? 'Expand' : 'Collapse';
        if (persist) {
            try { localStorage.setItem(storageKey(section), collapsed ? 'true' : 'false'); } catch (_) {}
        }
        document.dispatchEvent(new CustomEvent('itt:section-toggle', { detail: { section, collapsed } }));
    }

    function enhance(section) {
        let toolbar = section.querySelector(':scope > .auto-collapse-toolbar');
        let content = section.querySelector(':scope > .auto-collapse-content');
        if (!toolbar || !content) {
            toolbar = document.createElement('div');
            toolbar.className = 'auto-collapse-toolbar';
            const summary = document.createElement('span');
            summary.className = 'auto-collapse-summary';
            summary.textContent = section.dataset.collapseTitle || 'Section';
            const button = document.createElement('button');
            button.type = 'button';
            button.className = 'auto-collapse-toggle';
            button.innerHTML = '<span data-auto-collapse-label>Collapse</span><span class="auto-collapse-icon" aria-hidden="true">▾</span>';
            toolbar.append(summary, button);

            content = document.createElement('div');
            content.className = 'auto-collapse-content';
            while (section.firstChild) content.appendChild(section.firstChild);
            section.append(toolbar, content);
        }

        const button = toolbar.querySelector('.auto-collapse-toggle');
        if (button && !button.__ittCollapseBound) {
            button.__ittCollapseBound = true;
            button.addEventListener('click', () => apply(section, !section.classList.contains('is-auto-collapsed'), true));
        }
        let collapsed = section.classList.contains('is-auto-collapsed');
        try {
            const saved = localStorage.getItem(storageKey(section));
            if (saved !== null) collapsed = saved === 'true';
        } catch (_) {}
        apply(section, collapsed, false);
    }

    function initialize(root = document) {
        root.querySelectorAll('[data-auto-collapsible]').forEach(enhance);
    }

    window.ITTSections = { initialize, apply };
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => initialize());
    else initialize();
})();
