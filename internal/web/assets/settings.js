(() => {
  let importTimer;
  let pingController;
  const refreshImport = async () => {
    const status = document.getElementById('public-proxy-import-status');
    if (!status) {
      clearInterval(importTimer);
      importTimer = undefined;
      return;
    }
    try {
      const response = await fetch('/settings/public/import/status', {cache: 'no-store'});
      if (!response.ok) throw new Error();
      const state = await response.json();
      status.textContent = state.message;
      status.dataset.running = state.running;
      status.dataset.error = 'false';
      status.setAttribute('role', 'status');
      const button = document.querySelector('#public-proxy-import-form button');
      if (button) button.disabled = state.running;
      const count = document.getElementById('public-proxy-count');
      if (count) count.textContent = state.count;
      if (!state.running) {
        clearInterval(importTimer);
        importTimer = undefined;
      }
    } catch (_) {
      status.textContent = 'Could not check import status. Reopen Settings to retry.';
      status.dataset.error = 'true';
      status.setAttribute('role', 'alert');
      clearInterval(importTimer);
      importTimer = undefined;
    }
  };
  const watchImport = () => {
    const status = document.getElementById('public-proxy-import-status');
    if (!status || status.dataset.running !== 'true' || importTimer) return;
    refreshImport();
    importTimer = setInterval(refreshImport, 2000);
  };
  document.addEventListener('DOMContentLoaded', watchImport);
  document.addEventListener('htmx:afterSwap', watchImport);
  document.addEventListener('submit', async event => {
    const form = event.target;
    if (form.id === 'public-proxy-import-form') {
      event.preventDefault();
      if (!window.confirm('Download the saved source files and check public proxies? New proxies will be saved only if they reach YouTube.')) return;
      const button = form.querySelector('button[type="submit"]');
      const status = document.getElementById('public-proxy-import-status');
      button.disabled = true;
      status.textContent = 'Starting public proxy import…';
      status.dataset.error = 'false';
      status.setAttribute('role', 'status');
      try {
        const response = await fetch(form.action, {
          method: 'POST',
          headers: {'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json'},
          body: new URLSearchParams({confirm: 'yes'})
        });
        if (!response.ok) {
          status.textContent = (await response.text()).trim() || 'Could not start public proxy import.';
          status.dataset.error = 'true';
          status.setAttribute('role', 'alert');
          button.disabled = false;
          return;
        }
        status.dataset.running = 'true';
        watchImport();
      } catch (_) {
        status.textContent = 'Could not start public proxy import. Try again.';
        status.dataset.error = 'true';
        status.setAttribute('role', 'alert');
        button.disabled = false;
      }
      return;
    }
    if (form.id !== 'proxy-sources-form' && form.id !== 'personal-proxy-form' && form.id !== 'download-settings-form' && !form.classList.contains('proxy-delete')) return;
    event.preventDefault();
    const removing = form.classList.contains('proxy-delete');
    if (removing && !window.confirm(`Remove proxy ${form.closest('[data-proxy-id]').dataset.proxyHost}?`)) return;
    const savingSources = form.id === 'proxy-sources-form';
    const savingDownloads = form.id === 'download-settings-form';
    const feedback = document.getElementById(savingDownloads ? 'download-settings-feedback' : savingSources ? 'proxy-sources-feedback' : 'personal-proxy-feedback');
    const button = form.querySelector('button[type="submit"]') || document.querySelector(`button[form="${form.id}"]`);
    button.disabled = true;
    feedback.textContent = savingDownloads ? 'Saving…' : form.id === 'personal-proxy-form' ? 'Checking YouTube through proxy…' : savingSources ? 'Saving sources…' : 'Removing proxy…';
    feedback.dataset.error = 'false';
    feedback.setAttribute('role', 'status');
    if (savingSources) form.elements.proxy_sources.removeAttribute('aria-invalid');
    if (savingDownloads) form.elements.download_retries.removeAttribute('aria-invalid');
    if (form.id === 'personal-proxy-form') form.elements.address.removeAttribute('aria-invalid');
    try {
      const data = new URLSearchParams(new FormData(form));
      if (removing) data.set('confirm', 'yes');
      const response = await fetch(form.action, {
        method: 'POST',
        headers: {'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json'},
        body: data
      });
      if (!response.ok) {
        feedback.textContent = (await response.text()).trim() || 'Could not update settings.';
        feedback.dataset.error = 'true';
        feedback.setAttribute('role', 'alert');
        if (savingSources && response.status === 400) form.elements.proxy_sources.setAttribute('aria-invalid', 'true');
        if (savingDownloads && response.status === 400) form.elements.download_retries.setAttribute('aria-invalid', 'true');
        if (form.id === 'personal-proxy-form' && response.status === 400) form.elements.address.setAttribute('aria-invalid', 'true');
        return;
      }
      if (savingDownloads) {
        feedback.textContent = 'Download settings saved.';
      } else {
        htmx.ajax('GET', savingSources ? '/?source=settings&sources_saved=1' : '/?source=settings', {target: '#content', select: '#content', swap: 'outerHTML'});
      }
    } catch (_) {
      feedback.textContent = 'Could not save settings. Try again.';
      feedback.dataset.error = 'true';
      feedback.setAttribute('role', 'alert');
    } finally {
      if (button.isConnected) button.disabled = false;
    }
  });
  document.addEventListener('click', async event => {
    const ping = event.target.closest('[data-proxy-ping]');
    if (ping) {
      if (!ping.disabled && !pingController) await pingProxies(ping.dataset.proxyPing);
      return;
    }
    const button = event.target.closest('.proxy-reveal');
    if (!button) return;
    const row = button.closest('[data-proxy-id]');
    const host = row.dataset.proxyHost;
    const addressLabel = row.querySelector('.proxy-address');
    const label = button.querySelector('.proxy-reveal-label');
    const icon = button.querySelector('use');
    if (button.getAttribute('aria-expanded') === 'true') {
      addressLabel.textContent = addressLabel.dataset.masked;
      button.setAttribute('aria-expanded', 'false');
      label.textContent = 'Show URL';
      icon.setAttribute('href', '/assets/icons.svg#hg-view');
      button.setAttribute('aria-label', `Show proxy URL for ${host}`);
      button.title = 'Show URL';
      return;
    }
    button.disabled = true;
    try {
      const response = await fetch(`/settings/personal/${row.dataset.proxyId}/address`, {cache: 'no-store'});
      if (!response.ok) throw new Error();
      const {address} = await response.json();
      if (!row.isConnected) return;
      addressLabel.textContent = address;
      button.setAttribute('aria-expanded', 'true');
      label.textContent = 'Hide URL';
      icon.setAttribute('href', '/assets/icons.svg#hg-view-off');
      button.setAttribute('aria-label', `Hide proxy URL for ${host}`);
      button.title = 'Hide URL';
    } catch (_) {
      const feedback = document.getElementById('personal-proxy-feedback');
      if (feedback) {
        feedback.textContent = 'Could not show proxy URL.';
        feedback.dataset.error = 'true';
        feedback.setAttribute('role', 'alert');
      }
    } finally {
      if (button.isConnected) button.disabled = false;
    }
  });

  function proxyResult(row, message, state) {
    const field = row.querySelector('.proxy-result');
    field.querySelector('span').textContent = message;
    field.dataset.state = state;
    field.querySelector('use').setAttribute('href', `/assets/icons.svg#hg-${state === 'ready' ? 'checkmark-circle-02' : state === 'pending' ? 'reload' : 'circle'}`);
  }

  async function pingProxies(target) {
    const rows = [...document.querySelectorAll('[data-proxy-id]')].filter(row => target === 'all' || row.dataset.proxyId === target);
    if (!rows.length) return;
    const controller = new AbortController();
    pingController = controller;
    const controls = [...document.querySelectorAll('[data-proxy-ping]')];
    const feedback = document.getElementById('proxy-ping-feedback');
    controls.forEach(button => { button.disabled = true; });
    rows.forEach(row => proxyResult(row, 'Waiting', ''));
    let checked = 0;
    let failed = 0;
    const progress = () => {
      if (!feedback.isConnected || controller.signal.aborted) return;
      feedback.textContent = `${checked} / ${rows.length} checked${failed ? ` · ${failed} failed` : ''}`;
      feedback.dataset.error = String(failed > 0);
      feedback.setAttribute('role', failed ? 'alert' : 'status');
    };
    progress();
    const check = async row => {
      proxyResult(row, 'Checking…', 'pending');
      try {
        const response = await fetch(`/settings/personal/${row.dataset.proxyId}/ping`, {
          method: 'POST', headers: {Accept: 'application/json'}, signal: controller.signal
        });
        if (!response.ok) throw new Error((await response.text()).trim() || 'Could not check proxy. Try again.');
        const result = await response.json();
        if (!result.reachable) failed++;
        if (row.isConnected) {
          proxyResult(row, result.reachable ? 'YouTube reachable' : 'YouTube unavailable', result.reachable ? 'ready' : 'failed');
          row.querySelector('.proxy-result').title = `Checked ${new Date(result.checked_at * 1000).toLocaleString()}`;
        }
      } catch (error) {
        if (controller.signal.aborted) return;
        failed++;
        if (row.isConnected) {
          proxyResult(row, 'Check failed', 'failed');
          row.querySelector('.proxy-result').title = error.message;
        }
      } finally {
        checked++;
        progress();
      }
    };
    try {
      // Each request has its own server-side timeout and result. A failed proxy
      // does not prevent the rest of the personal list from being checked.
      for (let start = 0; start < rows.length && !controller.signal.aborted; start += 4) {
        await Promise.all(rows.slice(start, start + 4).map(check));
      }
    } finally {
      if (controller.signal.aborted) {
        for (const row of rows) {
          const field = row.querySelector('.proxy-result');
          if (row.isConnected && ['Checking…', 'Waiting'].includes(field.textContent.trim())) proxyResult(row, 'Check cancelled', '');
        }
        if (feedback.isConnected) {
          feedback.textContent = 'Proxy checks cancelled. Ping again to retry.';
          feedback.dataset.error = 'false';
          feedback.setAttribute('role', 'status');
        }
      }
      controls.forEach(button => { if (button.isConnected) button.disabled = false; });
      if (pingController === controller) pingController = undefined;
    }
  }

  document.addEventListener('htmx:beforeSwap', event => {
    if (event.detail.target.id === 'content' && event.detail.shouldSwap && !event.defaultPrevented) pingController?.abort();
  });
  document.addEventListener('auth:expired', () => pingController?.abort());
})();
