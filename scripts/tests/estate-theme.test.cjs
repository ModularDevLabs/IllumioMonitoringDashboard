'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const repo = path.join(__dirname, '../..');
const estateCSS = fs.readFileSync(path.join(repo, 'static/estate.css'), 'utf8');
const shellCSS = fs.readFileSync(path.join(repo, 'internal/extractor/frontend/product-shell.css'), 'utf8');
const estateHTML = fs.readFileSync(path.join(repo, 'estate.html'), 'utf8');
const shellJS = fs.readFileSync(path.join(repo, 'internal/extractor/frontend/product-shell.js'), 'utf8');

// This deliberately bounded parser handles the stylesheet's ordinary leaf
// rules, including those inside media blocks. It does not replace browser QA.
function rules(css) {
    const result = [];
    const clean = css.replace(/\/\*[\s\S]*?\*\//g, '');
    for (const match of clean.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
        const selectorList = match[1].trim();
        if (selectorList.startsWith('@')) continue;
        const declarations = {};
        for (const declaration of match[2].split(';')) {
            const colon = declaration.indexOf(':');
            if (colon < 0) continue;
            declarations[declaration.slice(0, colon).trim()] = declaration.slice(colon + 1).trim();
        }
        for (const selector of selectorList.split(',').map(value => value.trim()).filter(Boolean)) result.push({ selector, declarations });
    }
    return result;
}

function node(tag, classes, parent, attributes, states) {
    return { tag, classes: new Set((classes || '').split(/\s+/).filter(Boolean)), parent, attributes: attributes || {}, states: new Set(states || []) };
}

function simpleMatches(selector, element) {
    if (!element) return false;
    let allowed = true;
    selector = selector.replace(/:not\(([^()]*)\)/g, (_, inner) => { if (simpleMatches(inner, element)) allowed = false; return ''; });
    selector = selector.replace(/\[([\w-]+)(?:\s*=\s*['"]?([^'"\]]+)['"]?)?\]/g, (_, name, value) => {
        if (!Object.prototype.hasOwnProperty.call(element.attributes, name) || (value !== undefined && element.attributes[name] !== value)) allowed = false;
        return '';
    });
    selector = selector.replace(/:([\w-]+)/g, (_, state) => {
        if (state === 'disabled') { if (!element.attributes.disabled) allowed = false; }
        else if (!element.states.has(state)) allowed = false;
        return '';
    });
    selector = selector.replace(/#([\w-]+)/g, (_, id) => { if (element.attributes.id !== id) allowed = false; return ''; });
    selector = selector.replace(/\.([\w-]+)/g, (_, className) => { if (!element.classes.has(className)) allowed = false; return ''; });
    return allowed && (!selector || selector === '*' || selector === element.tag);
}

function matches(selector, element) {
    const parts = selector.replace(/\s*>\s*/g, ' > ').trim().split(/\s+/);
    function at(index, candidate) {
        if (!candidate || !simpleMatches(parts[index], candidate)) return false;
        if (index === 0) return true;
        if (parts[index - 1] === '>') return at(index - 2, candidate.parent);
        for (let ancestor = candidate.parent; ancestor; ancestor = ancestor.parent) if (at(index - 1, ancestor)) return true;
        return false;
    }
    return at(parts.length - 1, element);
}

function specificity(selector) {
    const unwrapped = selector.replace(/:not\(([^()]*)\)/g, '$1');
    const ids = (unwrapped.match(/#[\w-]+/g) || []).length;
    const classes = (unwrapped.match(/\.[\w-]+|\[[^\]]*\]|:(?!:)[\w-]+/g) || []).length;
    const tags = unwrapped.replace(/#[\w-]+|\.[\w-]+|\[[^\]]*\]|::?[\w-]+/g, '').split(/[\s>+~]+/).filter(value => /^[a-z][a-z0-9-]*$/i.test(value)).length;
    return ids * 10000 + classes * 100 + tags;
}

function computedDeclarations(allRules, element) {
    const result = {};
    const ranks = {};
    allRules.forEach((rule, order) => {
        if (!matches(rule.selector, element)) return;
        for (const [property, rawValue] of Object.entries(rule.declarations)) {
            const important = /!important\s*$/.test(rawValue);
            const rank = [important ? 1 : 0, specificity(rule.selector), order];
            const previous = ranks[property];
            if (!previous || rank[0] > previous[0] || (rank[0] === previous[0] && (rank[1] > previous[1] || (rank[1] === previous[1] && rank[2] > previous[2])))) {
                ranks[property] = rank;
                result[property] = rawValue.replace(/\s*!important\s*$/, '');
            }
        }
    });
    return result;
}

function layout(theme, state) {
    const html = node('html', '', null, { 'data-unified-theme': theme });
    const body = node('body', 'estate-page product-shell-enabled', html);
    const sidebar = node('aside', 'product-shell-sidebar', body);
    const shellMain = node('div', 'product-shell-main', body);
    const topbar = node('header', 'product-shell-topbar', shellMain);
    const page = node('div', 'product-shell-page', shellMain);
    const main = node('main', 'estate-wrap', page, { id: 'estate-page' });
    const attributes = state === 'active' ? { 'aria-current': 'page' } : {};
    const buttonAttributes = state === 'disabled' ? { disabled: true } : {};
    const states = state === 'hover' || state === 'focus-visible' ? [state] : [];
    return {
        navigation: node('a', 'product-shell-nav-link', sidebar, attributes, states),
        brand: node('a', 'product-shell-brand', sidebar, {}, states),
        theme: node('button', 'product-shell-control', topbar, { ...buttonAttributes, id: 'product-shell-theme' }, states),
        collapse: node('button', 'product-shell-sidebar-tool product-shell-desktop-toggle', sidebar, { ...buttonAttributes, id: 'product-shell-collapse' }, states),
        group: node('button', 'product-shell-group-toggle', sidebar, buttonAttributes, states),
        refresh: node('button', '', main, { ...buttonAttributes, id: 'estate-refresh' }, states),
        input: node('input', '', main, { type: 'date' }, states),
        link: node('a', '', main, {}, states)
    };
}

test('estate body class has only its base rule, never generic descendant control overrides', () => {
    const parsed = rules(estateCSS);
    const bodyRules = parsed.filter(rule => rule.selector.includes('.estate-page'));
    assert.equal(bodyRules.length, 1, 'keep the body base rule, but scope page descendants to content classes');
    assert.equal(bodyRules[0].selector, '.estate-page');
    assert.ok(parsed.some(rule => rule.selector === '.estate-wrap a'));
    for (const selector of ['.estate-wrap input', '.estate-wrap select', '.estate-wrap button']) {
        const matching = parsed.filter(rule => rule.selector === selector);
        assert.ok(matching.some(rule => Object.prototype.hasOwnProperty.call(rule.declarations, 'background')), 'content control styling missing for ' + selector);
    }
    for (const rule of parsed) {
        assert.doesNotMatch(rule.selector, /[.#]product-shell-/, 'page CSS must not restyle the shared shell directly: ' + rule.selector);
        if (/(?:^|[\s>+~])(?:a|input|select|button)(?=[\s.#[:>+~]|$)/.test(rule.selector)) {
            assert.match(rule.selector, /(?:\.estate-(?!page\b)[\w-]+|#estate-[\w-]+)/, 'generic control selector needs a content-owned scope: ' + rule.selector);
        }
    }
});

test('global hidden rule only preserves native hidden semantics', () => {
    const globals = rules(estateCSS).filter(rule => !/(?:[.#]estate-[\w-]+)/.test(rule.selector));
    assert.ok(globals.length <= 1, 'estate stylesheet added an unscoped global rule');
    for (const rule of globals) {
        assert.equal(rule.selector, '[hidden]');
        assert.deepEqual(rule.declarations, { display: 'none !important' });
    }
});

test('estate content stays inside main and shared shell navigation is installed outside it', () => {
    assert.match(estateHTML, /<body\s+class="estate-page"[^>]*>/);
    const main = estateHTML.match(/<main\b[^>]*class="estate-wrap"[^>]*>([\s\S]*?)<\/main>/);
    assert.ok(main, 'estate content must retain its own main wrapper');
    assert.match(main[1], /id="estate-refresh"/);
    assert.match(main[1], /id="estate-report-section"/);
    assert.doesNotMatch(main[1], /class="[^"]*product-shell-(?:sidebar|topbar|nav-link|control)/);
    assert.match(shellJS, /document\.body\.appendChild\(sidebar\)/);
    assert.match(shellJS, /document\.body\.appendChild\(main\)/);
    assert.match(shellJS, /main\.appendChild\(topbar\)/);
    assert.match(shellJS, /main\.appendChild\(page\)/);
    assert.match(shellJS, /originalNodes\.forEach\(function\s*\(node\)\s*\{\s*page\.appendChild\(node\);\s*\}\)/);
    assert.doesNotMatch(shellJS, /\.className\s*=\s*['"]estate-wrap['"]/);
});

test('adding estate styles leaves navigation and shell buttons unchanged in both themes and interaction states', () => {
    const shared = rules(shellCSS);
    const combined = shared.concat(rules(estateCSS));
    for (const theme of ['light', 'dark']) for (const state of ['default', 'active', 'hover', 'focus-visible', 'disabled']) {
        const elements = layout(theme, state);
        for (const name of ['navigation', 'brand', 'theme', 'collapse', 'group']) {
            const before = computedDeclarations(shared, elements[name]);
            const after = computedDeclarations(combined, elements[name]);
            assert.ok(Object.keys(before).length > 0, 'shared CSS fixture failed to match ' + name);
            assert.deepEqual(after, before, theme + '/' + state + ': estate CSS changed shared ' + name + ' styling');
        }
    }
});

test('estate controls retain their own colors, backgrounds and dark date input without reaching shell controls', () => {
    const estate = rules(estateCSS);
    for (const theme of ['light', 'dark']) {
        const elements = layout(theme, 'default');
        const refresh = computedDeclarations(estate, elements.refresh);
        assert.equal(refresh.background, 'var(--product-shell-panel-soft, #eeebe4)');
        assert.equal(refresh.color, 'var(--product-shell-text, #272b2d)');
        assert.equal(refresh.padding, '8px 12px');
        assert.equal(computedDeclarations(estate, elements.link).color, 'var(--product-shell-accent, #c94e14)');
        assert.deepEqual(computedDeclarations(estate, elements.theme), {});
        assert.equal(computedDeclarations(estate, elements.input)['color-scheme'], theme === 'dark' ? 'dark' : undefined);
    }
});

test('cascade guard detects the original body-wide link and button regression', () => {
    const elements = layout('dark', 'default');
    const shared = rules(shellCSS);
    const broken = shared.concat(rules('.estate-page a { color: orange; } .estate-page button { border: 1px solid red; background: white; padding: 8px 12px; }'));
    assert.notDeepEqual(computedDeclarations(broken, elements.navigation), computedDeclarations(shared, elements.navigation));
    assert.equal(computedDeclarations(broken, elements.navigation).color, 'orange');
    assert.notDeepEqual(computedDeclarations(broken, elements.theme), computedDeclarations(shared, elements.theme));
    assert.equal(computedDeclarations(broken, elements.theme).background, 'white');
});
