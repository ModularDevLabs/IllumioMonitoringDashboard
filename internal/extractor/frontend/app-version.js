(() => {
    const footer = document.createElement('footer');
    footer.className = 'app-version-footer';
    footer.setAttribute('aria-label', 'Application version');

    const product = document.createElement('span');
    product.className = 'app-version-product';
    product.textContent = 'Illumio Blocked Traffic Extractor';

    const separator = document.createElement('span');
    separator.className = 'app-version-separator';
    separator.setAttribute('aria-hidden', 'true');
    separator.textContent = '·';

    const version = document.createElement('span');
    version.className = 'app-version-value';
    version.dataset.appVersion = '';
    version.setAttribute('aria-live', 'polite');
    version.textContent = 'Development build';

    footer.append(product, separator, version);
    document.body.appendChild(footer);

    fetch('/blocked-traffic/api/version', { cache: 'no-store' })
        .then(response => {
            if (!response.ok) throw new Error('version unavailable');
            return response.json();
        })
        .then(payload => {
            const value = String(payload.version || '').trim();
            version.textContent = !value || value === 'development' ? 'Development build' : value;
        })
        .catch(() => {
            version.textContent = 'Version unavailable';
        });
})();
