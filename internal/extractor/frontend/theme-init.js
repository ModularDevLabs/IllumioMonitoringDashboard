(() => {
    const fallback = 'default';
    const allowedThemes = new Set([fallback, 'illumio', 'illumio-light']);
    let savedTheme = fallback;
    try {
        savedTheme = localStorage.getItem('ittTheme') || fallback;
    } catch (_) {
        savedTheme = fallback;
    }
    document.body.dataset.theme = allowedThemes.has(savedTheme) ? savedTheme : fallback;
})();
