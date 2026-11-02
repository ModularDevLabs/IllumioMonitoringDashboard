(function (global) {
    'use strict';

    // The caller supplies the escaped email renderer's HTML and its plain-text
    // equivalent. Neither path reads the clipboard or installs persistent DOM.
    function legacyCopy(html, text) {
        const document = global.document;
        if (!document || !document.body || typeof document.execCommand !== 'function') return false;

        const focused = document.activeElement;
        let inputSelection = null;
        let selection = null;
        const ranges = [];
        let temporary = null;
        let listening = false;
        let copyEventSucceeded = false;

        try {
            if (focused && typeof focused.selectionStart === 'number' && typeof focused.selectionEnd === 'number') {
                inputSelection = { start: focused.selectionStart, end: focused.selectionEnd, direction: focused.selectionDirection };
            }
            if (typeof global.getSelection === 'function') selection = global.getSelection();
            else if (typeof document.getSelection === 'function') selection = document.getSelection();
            if (selection) {
                for (let index = 0; index < selection.rangeCount; index++) ranges.push(selection.getRangeAt(index).cloneRange());
            }
        } catch (_) {
            // Do not disturb a selection that cannot be preserved.
            return false;
        }

        function onCopy(event) {
            if (!event.clipboardData || typeof event.clipboardData.setData !== 'function') return;
            try {
                event.preventDefault();
                event.clipboardData.setData('text/plain', text);
                if (html !== null) event.clipboardData.setData('text/html', html);
                copyEventSucceeded = true;
            } catch (_) { copyEventSucceeded = false; }
        }

        try {
            temporary = document.createElement('textarea');
            temporary.value = text;
            temporary.readOnly = true;
            temporary.tabIndex = -1;
            temporary.setAttribute('aria-hidden', 'true');
            temporary.style.cssText = 'position:fixed;left:-10000px;top:0;width:1px;height:1px;opacity:0;pointer-events:none;';
            document.body.appendChild(temporary);
            document.addEventListener('copy', onCopy, true);
            listening = true;
            temporary.focus({ preventScroll: true });
            temporary.select();
            const commandSucceeded = document.execCommand('copy');
            return commandSucceeded === true && copyEventSucceeded;
        } catch (_) {
            return false;
        } finally {
            if (listening) {
                try { document.removeEventListener('copy', onCopy, true); } catch (_) {}
            }
            if (temporary) {
                try { if (temporary.parentNode) temporary.parentNode.removeChild(temporary); } catch (_) {}
            }
            try {
                if (focused && typeof focused.focus === 'function') focused.focus({ preventScroll: true });
            } catch (_) {}
            try {
                if (selection) {
                    selection.removeAllRanges();
                    ranges.forEach(range => selection.addRange(range));
                }
            } catch (_) {}
            try {
                if (inputSelection && typeof focused.setSelectionRange === 'function') {
                    focused.setSelectionRange(inputSelection.start, inputSelection.end, inputSelection.direction);
                }
            } catch (_) {}
        }
    }

    async function copy(payload) {
        const text = String(payload?.text ?? '');
        const html = typeof payload?.html === 'string' && payload.html.length > 0 ? payload.html : null;
        try {
            const clipboard = global.navigator?.clipboard;
            if (html === null && clipboard && typeof clipboard.writeText === 'function') {
                // Invoke the browser synchronously while user activation is
                // available; never await anything before dispatching the write.
                const pending = clipboard.writeText(text);
                await pending;
                return true;
            }
            if (html !== null && clipboard && typeof clipboard.write === 'function' &&
                typeof global.ClipboardItem === 'function' && typeof global.Blob === 'function') {
                const supports = global.ClipboardItem.supports;
                if (typeof supports !== 'function' || supports.call(global.ClipboardItem, 'text/html')) {
                    const item = new global.ClipboardItem({
                        'text/html': new global.Blob([html], { type: 'text/html' }),
                        'text/plain': new global.Blob([text], { type: 'text/plain' })
                    });
                    const pending = clipboard.write([item]);
                    await pending;
                    return true;
                }
            }
        } catch (_) {
            // Permission rejection, insecure contexts, and unsupported HTML can
            // still allow a user-initiated legacy copy. Failure remains explicit.
        }
        return legacyCopy(html, text);
    }

    global.EstateEmailCopy = { copy };
})(window);
