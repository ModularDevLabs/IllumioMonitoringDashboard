'use strict';

const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const source = readFileSync(path.join(__dirname, '../../static/estate-email-copy.js'), 'utf8');

function runtime(options = {}) {
    const events = new Map();
    const writes = [];
    const created = [];
    const clipboardData = new Map();
    const selectedRanges = [{ id: 'original-selection', cloneRange() { return { id: this.id }; } }];
    const restored = { focus: 0, input: null, prevented: false, commands: 0 };
    const focus = {
        selectionStart: 2, selectionEnd: 8, selectionDirection: 'backward',
        focus() { restored.focus++; },
        setSelectionRange(...args) { restored.input = args; }
    };
    const selection = {
        rangeCount: 1,
        getRangeAt: index => selectedRanges[index],
        removeAllRanges() { selectedRanges.length = 0; },
        addRange(range) { selectedRanges.push(range); }
    };
    const body = {
        children: [],
        appendChild(node) { this.children.push(node); node.parentNode = this; },
        removeChild(node) { this.children = this.children.filter(child => child !== node); node.parentNode = null; }
    };
    const document = {
        body, activeElement: focus,
        createElement(tag) {
            assert.equal(tag, 'textarea');
            const element = { style: {}, attributes: {},
                setAttribute(name, value) { this.attributes[name] = value; },
                focus() { document.activeElement = this; },
                select() { this.selected = true; }
            };
            created.push(element);
            return element;
        },
        addEventListener(type, callback, capture) { assert.equal(capture, true); events.set(type, callback); },
        removeEventListener(type, callback, capture) { assert.equal(events.get(type), callback); assert.equal(capture, true); events.delete(type); },
        execCommand(command) {
            restored.commands++;
            assert.equal(command, 'copy');
            if (options.commandThrows) throw new Error('copy prohibited');
            if (!options.noCopyEvent) {
                events.get('copy')?.({
                    preventDefault() { restored.prevented = true; },
                    clipboardData: options.noEventData ? undefined : { setData(type, value) {
                        if (options.rejectHTML && type === 'text/html') throw new Error('unsupported');
                        clipboardData.set(type, value);
                    } }
                });
            }
            return options.commandResult !== false;
        }
    };
    if (options.noLegacy) delete document.execCommand;
    class ClipboardItem {
        constructor(data) { this.data = data; }
    }
    if (options.supportsHTML !== undefined) ClipboardItem.supports = () => options.supportsHTML;
    const window = { document, Blob, ClipboardItem, navigator: {}, getSelection: () => selection };
    if (options.clipboard) window.navigator.clipboard = options.clipboard;
    if (options.noClipboardItem) delete window.ClipboardItem;
    const context = vm.createContext({ window });
    vm.runInContext(source, context);
    return { copy: window.EstateEmailCopy.copy, window, document, body, created, events, clipboardData, restored, selectedRanges, writes };
}

test('modern rich copy dispatches one item synchronously with HTML and plain text', async () => {
    let finish;
    const writes = [];
    const app = runtime({ clipboard: { write(items) { writes.push(items); return new Promise(resolve => { finish = resolve; }); } } });
    const pending = app.copy({ html: '<h1>Estate &amp; VENs</h1>', text: 'Estate & VENs' });
    assert.equal(writes.length, 1, 'write must occur before the first await');
    assert.equal(writes[0].length, 1);
    const item = writes[0][0];
    assert.deepEqual(Object.keys(item.data).sort(), ['text/html', 'text/plain']);
    assert.equal(item.data['text/html'].type, 'text/html');
    assert.equal(await item.data['text/html'].text(), '<h1>Estate &amp; VENs</h1>');
    assert.equal(await item.data['text/plain'].text(), 'Estate & VENs');
    finish();
    assert.equal(await pending, true);
    assert.equal(app.created.length, 0);
});

test('plain subject copy uses only writeText and dispatches before awaiting', async () => {
    let finish;
    const calls = [];
    const app = runtime({ clipboard: {
        write() { throw new Error('rich write must not be called'); },
        writeText(text) { calls.push(text); return new Promise(resolve => { finish = resolve; }); }
    } });
    const pending = app.copy({ text: 'Weekly Estate Report — October 26' });
    assert.deepEqual(calls, ['Weekly Estate Report — October 26']);
    finish();
    assert.equal(await pending, true);
    assert.equal(app.restored.commands, 0);
});

test('a rejected modern write falls back to a confirmed rich copy event', async () => {
    const app = runtime({ clipboard: { write() { return Promise.reject(new Error('permission denied')); } } });
    assert.equal(await app.copy({ html: '<p>Versions</p>', text: 'Versions' }), true);
    assert.equal(app.clipboardData.get('text/html'), '<p>Versions</p>');
    assert.equal(app.clipboardData.get('text/plain'), 'Versions');
    assert.equal(app.restored.prevented, true);
    assert.equal(app.body.children.length, 0);
    assert.equal(app.events.size, 0);
    assert.equal(app.restored.focus, 1);
    assert.deepEqual(app.restored.input, [2, 8, 'backward']);
    assert.equal(app.selectedRanges[0].id, 'original-selection');
    assert.equal(app.created[0].value, 'Versions');
    assert.equal(app.created[0].selected, true);
});

test('unsupported modern HTML goes directly to legacy rich copy without silently copying plain text', async () => {
    let modernCalls = 0;
    const app = runtime({ supportsHTML: false, clipboard: {
        write() { modernCalls++; }, writeText() { modernCalls++; }
    } });
    assert.equal(await app.copy({ html: '<p>Report</p>', text: 'Report' }), true);
    assert.equal(modernCalls, 0);
    assert.equal(app.clipboardData.get('text/html'), '<p>Report</p>');
});

test('HTTP or missing ClipboardItem can use legacy copy without Clipboard API access', async () => {
    for (const options of [{}, { noClipboardItem: true, clipboard: { write() { throw new Error('must not call'); } } }]) {
        const app = runtime(options);
        assert.equal(await app.copy({ html: '<p>Report</p>', text: 'Report' }), true);
        assert.equal(app.body.children.length, 0);
        assert.equal(app.events.size, 0);
    }
});

test('plain legacy copy includes text/plain only and restores focus and selection', async () => {
    const app = runtime();
    assert.equal(await app.copy({ text: 'Weekly subject' }), true);
    assert.deepEqual([...app.clipboardData], [['text/plain', 'Weekly subject']]);
    assert.equal(app.events.size, 0);
    assert.equal(app.body.children.length, 0);
    assert.equal(app.restored.focus, 1);
    assert.deepEqual(app.restored.input, [2, 8, 'backward']);
});

test('legacy success requires both the copy event data write and command success', async () => {
    for (const options of [{ noCopyEvent: true }, { noEventData: true }, { commandResult: false }, { rejectHTML: true }, { commandThrows: true }]) {
        const app = runtime(options);
        assert.equal(await app.copy({ html: '<p>Report</p>', text: 'Report' }), false, JSON.stringify(options));
        assert.equal(app.events.size, 0);
        assert.equal(app.body.children.length, 0);
        assert.equal(app.restored.focus, 1);
        assert.deepEqual(app.restored.input, [2, 8, 'backward']);
        assert.equal(app.selectedRanges[0].id, 'original-selection');
    }
});

test('denied writes and absent fallback APIs return false for manual preview selection', async () => {
    const denied = runtime({ noLegacy: true, clipboard: { writeText() { return Promise.reject(new Error('denied')); } } });
    assert.equal(await denied.copy({ text: 'Subject' }), false);
    const unavailable = runtime({ noLegacy: true });
    assert.equal(await unavailable.copy({ html: '<p>Report</p>', text: 'Report' }), false);
    assert.equal(unavailable.created.length, 0);
    assert.equal(unavailable.events.size, 0);
});

test('clipboard getter failures are contained and no clipboard reads occur', async () => {
    const app = runtime();
    Object.defineProperty(app.window.navigator, 'clipboard', { get() { throw new Error('insecure context'); } });
    assert.equal(await app.copy({ text: 'Subject' }), true);
    assert.equal(app.restored.commands, 1);
    assert.doesNotMatch(source, /clipboard\.(?:read|readText)\s*\(/);
});

test('an unavailable existing selection is left untouched and reported as failure', async () => {
    const app = runtime();
    app.window.getSelection = () => { throw new Error('selection is inaccessible'); };
    assert.equal(await app.copy({ text: 'Subject' }), false);
    assert.equal(app.created.length, 0);
    assert.equal(app.restored.focus, 0);
});
