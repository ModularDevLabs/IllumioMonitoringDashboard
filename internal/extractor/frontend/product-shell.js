(function () {
    'use strict';

    var themeKey = 'illumio_product_theme';
    var compactKey = 'illumio_product_nav_compact';
    var groupKey = 'illumio_product_nav_groups';
    var isExtractor = window.location.pathname.indexOf('/blocked-traffic') === 0;

    var icons = {
        overview: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 13h6V4H4v9Zm0 7h6v-4H4v4Zm10 0h6v-9h-6v9Zm0-16v4h6V4h-6Z"/></svg>',
        executive: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 19V9m5 10V5m5 14v-7m5 7V3M2 21h20"/></svg>',
        trends: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m3 17 5-5 4 3 7-9m0 0v5m0-5h-5"/></svg>',
        extract: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v12m0 0 4-4m-4 4-4-4M4 19h16"/></svg>',
        analytics: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m16 16 5 5M8 12l2-2 2 2 3-4"/></svg>',
        heatmaps: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/></svg>',
        trafficExecutive: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 20V10m5 10V4m5 16v-7m5 7V7"/></svg>',
        automation: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v3m0 12v3M3 12h3m12 0h3M5.6 5.6l2.1 2.1m8.6 8.6 2.1 2.1m0-12.8-2.1 2.1m-8.6 8.6-2.1 2.1"/><circle cx="12" cy="12" r="4"/></svg>',
        settings: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1-2.8 2.8-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.6v.2h-4V21a1.7 1.7 0 0 0-1-1.6 1.7 1.7 0 0 0-1.9.3l-.1.1L4.2 17l.1-.1a1.7 1.7 0 0 0 .3-1.9A1.7 1.7 0 0 0 3 14H2.8v-4H3a1.7 1.7 0 0 0 1.6-1 1.7 1.7 0 0 0-.3-1.9L4.2 7 7 4.2l.1.1A1.7 1.7 0 0 0 9 4.6 1.7 1.7 0 0 0 10 3V2.8h4V3a1.7 1.7 0 0 0 1 1.6 1.7 1.7 0 0 0 1.9-.3l.1-.1L19.8 7l-.1.1a1.7 1.7 0 0 0-.3 1.9 1.7 1.7 0 0 0 1.6 1h.2v4H21a1.7 1.7 0 0 0-1.6 1Z"/></svg>'
    };

    var groups = [
        { id: 'monitoring', label: 'Monitoring', items: [
            { href: '/', label: 'Dashboard', title: 'Monitoring Dashboard', icon: 'overview', match: function (path) { return path === '/' || path === '/details'; } },
            { href: '/executive', label: 'Executive View', title: 'Executive View', icon: 'executive', match: function (path) { return path === '/executive'; } },
            { href: '/trends', label: 'Trends & Reports', title: 'Trends & Reports', icon: 'trends', match: function (path) { return path === '/trends' || path === '/report'; } }
        ]},
		{ id: 'blocked-traffic', label: 'Traffic', items: [
			{ href: '/blocked-traffic/', label: 'Extract & Import', title: 'Traffic Extractor', icon: 'extract', match: function (path) { return path === '/blocked-traffic' || path === '/blocked-traffic/'; } },
            { href: '/blocked-traffic/summary', label: 'Analytics', title: 'Traffic Analytics', icon: 'analytics', match: function (path) { return path === '/blocked-traffic/summary'; } },
            { href: '/blocked-traffic/heatmaps', label: 'Heatmap Explorer', title: 'Heatmap Explorer', icon: 'heatmaps', match: function (path) { return path === '/blocked-traffic/heatmaps'; } },
			{ href: '/blocked-traffic/executive-summary', label: 'Executive Summary', title: 'Traffic Executive Summary', icon: 'trafficExecutive', match: function (path) { return path === '/blocked-traffic/executive-summary'; } }
        ]},
        { id: 'automation', label: 'Automation', items: [
            { href: '/blocked-traffic/automation', label: 'Templates & Delivery', title: 'Templates & Delivery', icon: 'automation', match: function (path) { return path === '/blocked-traffic/automation'; } }
        ]},
        { id: 'administration', label: 'Administration', items: [
            { href: '/settings', label: 'Settings', title: 'Settings', icon: 'settings', match: function (path) { return path === '/settings'; } }
        ]}
    ];

    function normalizedPath() {
        var path = window.location.pathname || '/';
        return path.length > 1 && path.endsWith('/') && path !== '/blocked-traffic/' ? path.slice(0, -1) : path;
    }

    function routeInfo() {
        var path = normalizedPath();
        if (path === '/details') return { group: groups[0], item: { title: 'Metric Details' } };
        if (path === '/report') {
            return { group: groups[0], item: { title: new URLSearchParams(window.location.search).get('live') === '1' ? 'Trend Analysis' : 'Monitoring Report' } };
        }
        for (var g = 0; g < groups.length; g += 1) {
            for (var i = 0; i < groups[g].items.length; i += 1) {
                if (groups[g].items[i].match(path)) return { group: groups[g], item: groups[g].items[i] };
            }
        }
		return { group: { label: isExtractor ? 'Traffic' : 'Monitoring' }, item: { title: document.title || 'Illumio Operations Hub' } };
    }

    function savedTheme() {
        var unified = localStorage.getItem(themeKey);
        if (unified === 'light' || unified === 'dark') return unified;
        if (isExtractor) return localStorage.getItem('ittTheme') === 'illumio-light' ? 'light' : 'dark';
        return localStorage.getItem('illumio_theme') === 'dark' ? 'dark' : 'light';
    }

    function pageThemeValue(theme) {
        return isExtractor ? (theme === 'dark' ? 'illumio' : 'illumio-light') : theme;
    }

    function synchronizeTheme(theme, triggerPageBehavior) {
        var value = theme === 'dark' ? 'dark' : 'light';
        document.documentElement.dataset.unifiedTheme = value;
        localStorage.setItem(themeKey, value);
        localStorage.setItem('illumio_theme', value);
        localStorage.setItem('ittTheme', value === 'dark' ? 'illumio' : 'illumio-light');

        if (triggerPageBehavior) {
            if (isExtractor) {
                var extractorButton = document.querySelector('[data-theme-option="' + pageThemeValue(value) + '"]');
                document.body.dataset.theme = pageThemeValue(value);
                if (extractorButton) extractorButton.click();
            } else {
                var current = document.body.getAttribute('data-theme') === 'dark' ? 'dark' : 'light';
                var dashboardButton = document.getElementById('btn-theme');
                if (dashboardButton && current !== value) dashboardButton.click();
                document.body.setAttribute('data-theme', value);
            }
        } else {
            document.body.dataset.theme = pageThemeValue(value);
        }

        var label = document.querySelector('.product-shell-theme-label');
        if (label) label.textContent = value === 'dark' ? 'Light' : 'Dark';
        var themeButton = document.getElementById('product-shell-theme');
        if (themeButton) themeButton.setAttribute('aria-label', value === 'dark' ? 'Use light theme' : 'Use dark theme');
    }

    function groupState() {
        try { return JSON.parse(localStorage.getItem(groupKey) || '{}') || {}; } catch (_) { return {}; }
    }

    function setGroupCollapsed(group, collapsed) {
        group.classList.toggle('is-collapsed', collapsed);
        var button = group.querySelector('.product-shell-group-toggle');
        if (button) button.setAttribute('aria-expanded', String(!collapsed));
        var state = groupState();
        state[group.dataset.group] = collapsed;
        localStorage.setItem(groupKey, JSON.stringify(state));
    }

    function navMarkup(active) {
        var state = groupState();
        return groups.map(function (group) {
            var containsActive = group.items.some(function (item) { return item.match(normalizedPath()); });
            var collapsed = containsActive ? false : state[group.id] === true;
            var links = group.items.map(function (item) {
                var current = item.match(normalizedPath());
                return '<a class="product-shell-nav-link" href="' + item.href + '" title="' + item.label + '"' + (current ? ' aria-current="page"' : '') + '>' +
                    '<span class="product-shell-icon">' + icons[item.icon] + '</span>' +
                    '<span class="product-shell-nav-label">' + item.label + '</span></a>';
            }).join('');
            return '<section class="product-shell-group' + (collapsed ? ' is-collapsed' : '') + '" data-group="' + group.id + '">' +
                '<button type="button" class="product-shell-group-toggle" aria-expanded="' + String(!collapsed) + '"><span>' + group.label + '</span>' +
                '<svg class="product-shell-chevron" viewBox="0 0 20 20" aria-hidden="true"><path d="m5 7.5 5 5 5-5" fill="none" stroke="currentColor" stroke-width="1.8"/></svg></button>' +
                '<div class="product-shell-group-items">' + links + '</div></section>';
        }).join('');
    }

    function shellButton(id, className, label, svg) {
        return '<button id="' + id + '" type="button" class="product-shell-control ' + className + '" aria-label="' + label + '">' + svg + '</button>';
    }

    function menuIcon() { return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M4 12h16M4 17h16"/></svg>'; }
    function collapseIcon() { return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m14 6-6 6 6 6M20 4v16"/></svg>'; }
    function themeIcon() { return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v2m0 14v2M3 12h2m14 0h2M5.6 5.6 7 7m10 10 1.4 1.4m0-12.8L17 7M7 17l-1.4 1.4"/><circle cx="12" cy="12" r="4"/></svg>'; }

    function isLegacyNavigationLink(link) {
        var text = (link.textContent || '').trim().toLowerCase();
        return text === 'settings' || text === 'executive view' || text === 'trend view' || text === 'traffic extractor' ||
            text === 'back to dashboard' || text === 'monitoring dashboard' || text === 'extractor' || text === 'analytics' ||
            text === 'heatmaps' || text === 'executive' || text === 'automation';
    }

    function collectPageActions(actionContainer) {
        document.querySelectorAll('.app-nav, .theme-switcher, #btn-theme').forEach(function (element) {
            element.classList.add('product-shell-obsolete');
        });
        document.querySelectorAll('.header a').forEach(function (link) {
            if (isLegacyNavigationLink(link)) link.classList.add('product-shell-obsolete');
        });

        var candidates = [];
        document.querySelectorAll('.app-header-actions > .app-header-action').forEach(function (element) { candidates.push(element); });
        document.querySelectorAll('.header .status-actions > *').forEach(function (element) {
            if (!element.classList.contains('product-shell-obsolete')) candidates.push(element);
        });
        candidates.forEach(function (element) { actionContainer.appendChild(element); });

        document.querySelectorAll('.app-header-actions, .header .status-actions').forEach(function (container) {
            var visible = Array.prototype.some.call(container.children, function (child) { return !child.classList.contains('product-shell-obsolete'); });
            if (!visible) container.classList.add('product-shell-context-empty');
        });
        document.querySelectorAll('.app-header').forEach(function (header) {
            if (header.firstElementChild && header.firstElementChild.tagName === 'H1') {
                header.classList.add('product-shell-context-empty');
            }
        });
    }

    function closeMobileMenu() {
        document.body.classList.remove('product-shell-menu-open');
        var toggle = document.getElementById('product-shell-menu');
        if (toggle) toggle.setAttribute('aria-expanded', 'false');
    }

    function setCompactNavigation(compact) {
        document.body.classList.toggle('product-shell-compact', compact);
        localStorage.setItem(compactKey, String(compact));
        var button = document.getElementById('product-shell-collapse');
        if (!button) return;
        var action = compact ? 'Expand' : 'Collapse';
        button.setAttribute('aria-label', action + ' navigation');
        button.setAttribute('title', action + ' navigation');
        var label = button.querySelector('.product-shell-sidebar-tool-label');
        if (label) label.textContent = action + ' menu';
    }

    function installShell() {
        if (document.body.classList.contains('product-shell-enabled')) return;
        var info = routeInfo();
        var initialTheme = savedTheme();
        document.documentElement.dataset.unifiedTheme = initialTheme;

        var originalNodes = Array.prototype.slice.call(document.body.childNodes);
        var sidebar = document.createElement('aside');
        sidebar.id = 'product-shell-sidebar';
        sidebar.className = 'product-shell-sidebar';
        sidebar.setAttribute('aria-label', 'Product navigation');
        sidebar.setAttribute('data-offline-remove', 'true');
        sidebar.innerHTML = '<a class="product-shell-brand" href="/" aria-label="Illumio Operations Hub home">' +
            '<span class="product-shell-mark" aria-hidden="true"></span><span class="product-shell-brand-copy">' +
            '<span class="product-shell-brand-name">ILLUMIO</span><span class="product-shell-brand-subtitle">Operations Hub</span></span></a>' +
            '<div class="product-shell-sidebar-tools">' +
            '<button id="product-shell-collapse" type="button" class="product-shell-sidebar-tool product-shell-desktop-toggle" aria-label="Collapse navigation" title="Collapse navigation">' +
            collapseIcon() + '<span class="product-shell-sidebar-tool-label">Collapse menu</span></button></div>' +
            '<nav class="product-shell-nav">' + navMarkup(info) + '</nav>' +
            '<div class="product-shell-sidebar-footer"><div class="product-shell-runtime"><span class="product-shell-runtime-dot"></span>' +
            '<span class="product-shell-runtime-copy">Local application<span id="product-shell-version" class="product-shell-version">Version loading…</span></span></div></div>';

        var main = document.createElement('div');
        main.className = 'product-shell-main';
        var topbar = document.createElement('header');
        topbar.className = 'product-shell-topbar';
        topbar.setAttribute('data-offline-remove', 'true');
        topbar.innerHTML = '<div class="product-shell-topbar-start">' +
            shellButton('product-shell-menu', 'product-shell-mobile-toggle', 'Open navigation', menuIcon()) +
            '<div class="product-shell-page-heading"><div class="product-shell-breadcrumb">' + info.group.label + '</div>' +
            '<h1 class="product-shell-page-title">' + info.item.title + '</h1></div></div>' +
            '<div class="product-shell-topbar-actions" id="product-shell-actions"></div>';

        var page = document.createElement('div');
        page.className = 'product-shell-page';
        var pageToolbar = document.createElement('div');
        pageToolbar.id = 'product-shell-page-actions';
        pageToolbar.className = 'product-shell-page-toolbar';
        pageToolbar.setAttribute('data-offline-remove', 'true');
        page.appendChild(pageToolbar);
        originalNodes.forEach(function (node) { page.appendChild(node); });
        main.appendChild(topbar);
        main.appendChild(page);

        var overlay = document.createElement('button');
        overlay.type = 'button';
        overlay.className = 'product-shell-overlay';
        overlay.setAttribute('aria-label', 'Close navigation');
        overlay.setAttribute('data-offline-remove', 'true');

        document.body.appendChild(sidebar);
        document.body.appendChild(main);
        document.body.appendChild(overlay);
        document.body.classList.add('product-shell-enabled');

        var actionContainer = document.getElementById('product-shell-actions');
        collectPageActions(pageToolbar);
        actionContainer.insertAdjacentHTML('beforeend', '<button id="product-shell-theme" type="button" class="product-shell-control" aria-label="Change color theme">' +
            themeIcon() + '<span class="product-shell-theme-label"></span></button>');

        synchronizeTheme(initialTheme, false);
        setCompactNavigation(localStorage.getItem(compactKey) === 'true');

        document.querySelectorAll('.product-shell-group-toggle').forEach(function (button) {
            button.addEventListener('click', function () {
                var group = button.closest('.product-shell-group');
                setGroupCollapsed(group, !group.classList.contains('is-collapsed'));
            });
        });
        document.getElementById('product-shell-theme').addEventListener('click', function () {
            synchronizeTheme(document.documentElement.dataset.unifiedTheme === 'dark' ? 'light' : 'dark', true);
        });
        document.getElementById('product-shell-collapse').addEventListener('click', function () {
            setCompactNavigation(!document.body.classList.contains('product-shell-compact'));
        });
        document.getElementById('product-shell-menu').addEventListener('click', function () {
            var open = !document.body.classList.contains('product-shell-menu-open');
            document.body.classList.toggle('product-shell-menu-open', open);
            this.setAttribute('aria-expanded', String(open));
        });
        overlay.addEventListener('click', closeMobileMenu);
        document.querySelectorAll('.product-shell-nav-link').forEach(function (link) { link.addEventListener('click', closeMobileMenu); });
        document.addEventListener('keydown', function (event) { if (event.key === 'Escape') closeMobileMenu(); });

        fetch('/api/version', { headers: { Accept: 'application/json' } })
            .then(function (response) { if (!response.ok) throw new Error('version unavailable'); return response.json(); })
            .then(function (payload) {
                var label = document.getElementById('product-shell-version');
                if (label) label.textContent = payload.version || 'Development build';
            })
            .catch(function () {
                var label = document.getElementById('product-shell-version');
                if (label) label.textContent = 'Development build';
            });
    }

    document.documentElement.dataset.unifiedTheme = savedTheme();
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', installShell, { once: true });
    else installShell();
})();
