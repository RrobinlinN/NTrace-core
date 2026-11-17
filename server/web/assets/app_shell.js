// App-shell mode for the mobile shell apps (mobile/android, mobile/ios).
//
// The shells load the console as /?shell=app. This script then rearranges the
// existing form into an app layout: search row, segmented mode/protocol
// controls, recent targets, and an options sheet. It only moves and decorates
// elements; app.js keeps owning every control, so tracing logic is untouched.
// Styling lives in app_shell.css.
(function () {
  'use strict';

  if (!/(^|[?&])shell=app(&|$)/.test(window.location.search)) {
    return;
  }
  const root = document.documentElement;
  // Set before first paint so the browser layout never flashes.
  root.classList.add('app-shell');

  const RECENTS_KEY = 'nexttrace.shell.recentTargets';
  const RECENTS_MAX = 8;
  const MAP_KEY = 'nexttrace.shell.disableMap';
  const THEME_KEY = 'nexttrace.shell.theme';
  const THEME_CHOICES = ['system', 'light', 'dark'];
  const TEXT = {
    cn: {
      options: '选项', done: '完成', appearance: '外观',
      system: '跟随系统', light: '日间', dark: '夜间',
      toLight: '切换到日间模式', toDark: '切换到夜间模式',
    },
    en: {
      options: 'Options', done: 'Done', appearance: 'Appearance',
      system: 'System', light: 'Light', dark: 'Dark',
      toLight: 'Switch to light mode', toDark: 'Switch to dark mode',
    },
  };
  const ICON_OPEN =
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" ' +
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">';
  const GEAR_ICON = ICON_OPEN +
    '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/></svg>';
  const SUN_ICON = ICON_OPEN +
    '<circle cx="12" cy="12" r="4"/><path d="M12 2.5v2M12 19.5v2M4.6 4.6 6 6M18 18l1.4 1.4' +
    'M2.5 12h2M19.5 12h2M4.6 19.4 6 18M18 6l1.4-1.4"/></svg>';
  const MOON_ICON = ICON_OPEN + '<path d="M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z"/></svg>';

  // ── Theme ──────────────────────────────────────────────────────────────
  // The choice is "system", "light" or "dark"; <html data-theme> always holds
  // the resolved light/dark value. This runs before first paint.
  const darkQuery = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  // The Android WebView does not report the system setting through
  // prefers-color-scheme, so the Android shell passes it in the URL (for the
  // first paint) and through nextTraceShellSystemTheme() when it changes.
  let nativeSystemTheme = (/(?:^|[?&])system=(light|dark)(?:&|$)/.exec(window.location.search) || [])[1] || null;
  let themeChoice = 'system';
  try {
    const stored = window.localStorage.getItem(THEME_KEY);
    if (THEME_CHOICES.indexOf(stored) >= 0) {
      themeChoice = stored;
    }
  } catch (err) {
    // Storage unavailable: follow the system.
  }
  const themeListeners = [];

  function systemTheme() {
    if (nativeSystemTheme) {
      return nativeSystemTheme;
    }
    // Without a media query answer keep the original dark look.
    return darkQuery && !darkQuery.matches ? 'light' : 'dark';
  }

  function resolvedTheme() {
    return themeChoice === 'system' ? systemTheme() : themeChoice;
  }

  // Lets the shells match window and system-bar colours to the page.
  function notifyNativeTheme(theme) {
    try {
      if (window.NextTraceShell && typeof window.NextTraceShell.setTheme === 'function') {
        window.NextTraceShell.setTheme(theme, themeChoice === 'system');
      } else if (window.webkit && window.webkit.messageHandlers &&
                 window.webkit.messageHandlers.nextTraceTheme) {
        window.webkit.messageHandlers.nextTraceTheme.postMessage({
          theme: theme,
          followsSystem: themeChoice === 'system',
        });
      }
    } catch (err) {
      // Not running inside a shell, or the bridge is missing.
    }
  }

  function applyTheme() {
    const theme = resolvedTheme();
    root.setAttribute('data-theme', theme);
    root.style.colorScheme = theme;
    notifyNativeTheme(theme);
    themeListeners.forEach((listener) => listener());
  }

  function setThemeChoice(choice) {
    if (THEME_CHOICES.indexOf(choice) < 0) {
      return;
    }
    themeChoice = choice;
    try {
      window.localStorage.setItem(THEME_KEY, choice);
    } catch (err) {
      // The choice then only lasts for this session.
    }
    applyTheme();
  }

  // Called by the Android shell when the system switches light/dark.
  window.nextTraceShellSystemTheme = function (theme) {
    if (theme !== 'light' && theme !== 'dark') {
      return;
    }
    nativeSystemTheme = theme;
    applyTheme();
  };

  if (darkQuery) {
    const onSystemChange = () => {
      if (!nativeSystemTheme) {
        applyTheme();
      }
    };
    if (darkQuery.addEventListener) {
      darkQuery.addEventListener('change', onSystemChange);
    } else if (darkQuery.addListener) {
      darkQuery.addListener(onSystemChange);
    }
  }
  applyTheme();

  function el(tag, className) {
    const node = document.createElement(tag);
    if (className) {
      node.className = className;
    }
    return node;
  }

  function loadRecents() {
    try {
      const list = JSON.parse(window.localStorage.getItem(RECENTS_KEY) || '[]');
      return Array.isArray(list) ? list.filter((item) => typeof item === 'string') : [];
    } catch (err) {
      return [];
    }
  }

  function saveRecent(target) {
    const list = [target].concat(loadRecents().filter((item) => item !== target)).slice(0, RECENTS_MAX);
    try {
      window.localStorage.setItem(RECENTS_KEY, JSON.stringify(list));
    } catch (err) {
      // Storage may be unavailable; recents are a convenience only.
    }
  }

  // Mirrors a <select> as a row of toggle buttons and keeps both in sync.
  function buildSegment(select, formatLabel) {
    const segment = el('div', 'shell-segment');
    segment.setAttribute('role', 'group');

    function render() {
      segment.textContent = '';
      Array.from(select.options).forEach((option) => {
        const button = el('button');
        button.type = 'button';
        button.textContent = formatLabel ? formatLabel(option) : option.textContent;
        button.setAttribute('aria-pressed', String(option.value === select.value));
        button.addEventListener('click', () => {
          if (select.value === option.value) {
            return;
          }
          select.value = option.value;
          select.dispatchEvent(new Event('change', { bubbles: true }));
        });
        segment.appendChild(button);
      });
      segment.hidden = select.options.length < 2;
    }

    // Options arrive asynchronously (protocols) and are relabelled on a
    // language switch (modes).
    new MutationObserver(render).observe(select, { childList: true, subtree: true, characterData: true });
    select.addEventListener('change', render);
    render();
    return segment;
  }

  function init() {
    const form = document.getElementById('trace-form');
    const target = document.getElementById('target');
    const modeSelect = document.getElementById('mode');
    const protocolSelect = document.getElementById('protocol');
    const providerSelect = document.getElementById('data-provider');
    const submitBtn = document.getElementById('submit-btn');
    const actions = form && form.querySelector('.form__actions');
    const headerActions = document.querySelector('.header__actions');
    const cacheBtn = document.getElementById('cache-btn');
    const langBtn = document.getElementById('lang-toggle');
    const tosInput = document.getElementById('tos');
    if (!form || !target || !modeSelect || !protocolSelect || !providerSelect || !submitBtn ||
        !actions || !headerActions || !cacheBtn || !langBtn) {
      // The console markup changed; fall back to the plain layout.
      root.classList.remove('app-shell');
      return;
    }

    const viewport = document.querySelector('meta[name="viewport"]');
    if (viewport) {
      viewport.setAttribute('content',
        'width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no, viewport-fit=cover');
    }

    const modeGroup = modeSelect.closest('.form__group');
    const targetGroup = target.closest('.form__group');
    const protocolCell = protocolSelect.parentElement;
    const providerGroup = providerSelect.closest('.form__group');
    const tosGroup = tosInput ? tosInput.closest('.form__group') : null;

    // Search row: target + start/stop.
    target.setAttribute('enterkeyhint', 'go');
    target.setAttribute('inputmode', 'url');
    target.setAttribute('autocapitalize', 'off');
    target.setAttribute('autocorrect', 'off');
    target.setAttribute('autocomplete', 'off');
    target.spellcheck = false;
    target.setAttribute('aria-labelledby', 'label-target');
    const search = el('div', 'shell-search');
    search.appendChild(target);
    search.appendChild(actions);

    // Segmented controls drive the original selects.
    const segments = el('div', 'shell-segments');
    segments.appendChild(buildSegment(modeSelect));
    segments.appendChild(buildSegment(protocolSelect, (option) => option.textContent.toUpperCase()));
    modeGroup.classList.add('shell-offscreen');
    targetGroup.classList.add('shell-offscreen');
    protocolCell.classList.add('shell-offscreen');
    providerSelect.parentElement.classList.add('shell-wide');

    const recents = el('div', 'shell-recents');
    function renderRecents() {
      recents.textContent = '';
      loadRecents().forEach((item) => {
        const chip = el('button');
        chip.type = 'button';
        chip.textContent = item;
        chip.addEventListener('click', () => {
          target.value = item;
          if (!submitBtn.disabled) {
            form.requestSubmit(submitBtn);
          }
        });
        recents.appendChild(chip);
      });
    }

    // Options sheet: everything that is not needed for a quick trace.
    const backdrop = el('div', 'shell-backdrop');
    const sheet = el('div', 'shell-sheet');
    sheet.setAttribute('role', 'dialog');
    sheet.setAttribute('aria-modal', 'true');
    const sheetBar = el('div', 'shell-sheet__bar');
    const sheetTitle = el('div', 'shell-sheet__title');
    const sheetDone = el('button', 'shell-sheet__done');
    sheetDone.type = 'button';
    sheetBar.appendChild(sheetTitle);
    sheetBar.appendChild(sheetDone);
    const sheetBody = el('div', 'shell-sheet__body');
    [providerGroup,
      document.getElementById('group-basic-params'),
      document.getElementById('group-advanced-params'),
      tosGroup,
      document.getElementById('group-disable-map')].forEach((group) => {
      if (group) {
        sheetBody.appendChild(group);
      }
    });
    // Appearance: a detached select drives a segmented control like the
    // mode/protocol ones; it has no name, so nothing is submitted.
    const themeSelect = document.createElement('select');
    THEME_CHOICES.forEach((choice) => {
      const option = document.createElement('option');
      option.value = choice;
      themeSelect.appendChild(option);
    });
    themeSelect.value = themeChoice;
    themeSelect.addEventListener('change', () => {
      if (themeSelect.value !== themeChoice) {
        setThemeChoice(themeSelect.value);
      }
    });
    const themeGroup = el('div', 'form__group shell-appearance');
    const themeLabel = el('label');
    themeGroup.appendChild(themeLabel);
    themeGroup.appendChild(buildSegment(themeSelect));
    sheetBody.appendChild(themeGroup);

    const sheetActions = el('div', 'shell-sheet__actions');
    sheetActions.appendChild(cacheBtn);
    sheetActions.appendChild(langBtn);
    sheetBody.appendChild(sheetActions);
    sheet.appendChild(sheetBar);
    sheet.appendChild(sheetBody);

    form.insertBefore(search, form.firstChild);
    form.insertBefore(segments, search.nextSibling);
    form.insertBefore(recents, segments.nextSibling);
    // The sheet stays inside the form so its fields are still submitted.
    form.appendChild(backdrop);
    form.appendChild(sheet);

    // Quick light/dark toggle; "follow the system" is in the options sheet.
    const themeBtn = el('button', 'shell-icon-btn');
    themeBtn.type = 'button';
    themeBtn.addEventListener('click', () => {
      setThemeChoice(resolvedTheme() === 'dark' ? 'light' : 'dark');
    });
    headerActions.appendChild(themeBtn);

    const optionsBtn = el('button', 'shell-icon-btn');
    optionsBtn.type = 'button';
    optionsBtn.innerHTML = GEAR_ICON;
    headerActions.appendChild(optionsBtn);

    function setSheetOpen(open) {
      root.classList.toggle('shell-sheet-open', open);
      if (open && document.activeElement && document.activeElement.blur) {
        document.activeElement.blur();
      }
    }
    // Called by the Android shell on the Back key; true means "handled".
    window.nextTraceShellBack = function () {
      if (root.classList.contains('shell-sheet-open')) {
        setSheetOpen(false);
        return true;
      }
      return false;
    };
    optionsBtn.addEventListener('click', () => setSheetOpen(true));
    sheetDone.addEventListener('click', () => setSheetOpen(false));
    backdrop.addEventListener('click', () => setSheetOpen(false));

    // The language button's own label names the *other* language.
    function currentText() {
      return langBtn.textContent.trim() === 'English' ? TEXT.cn : TEXT.en;
    }

    // The toggle shows the mode it switches to.
    function syncThemeControls() {
      const text = currentText();
      const dark = resolvedTheme() === 'dark';
      themeBtn.innerHTML = dark ? SUN_ICON : MOON_ICON;
      themeBtn.setAttribute('aria-label', dark ? text.toLight : text.toDark);
      if (themeSelect.value !== themeChoice) {
        themeSelect.value = themeChoice;
        // Re-renders the segmented control; the change handler sees no change.
        themeSelect.dispatchEvent(new Event('change'));
      }
    }
    themeListeners.push(syncThemeControls);

    function applyText() {
      const text = currentText();
      sheetTitle.textContent = text.options;
      sheetDone.textContent = text.done;
      optionsBtn.setAttribute('aria-label', text.options);
      themeLabel.textContent = text.appearance;
      Array.from(themeSelect.options).forEach((option) => {
        if (option.textContent !== text[option.value]) {
          option.textContent = text[option.value];
        }
      });
      syncThemeControls();
    }
    new MutationObserver(applyText).observe(langBtn, { childList: true, subtree: true, characterData: true });
    applyText();

    // app.js disables the start button for the lifetime of a trace.
    function syncRunning() {
      root.classList.toggle('shell-running', submitBtn.disabled);
    }
    new MutationObserver(syncRunning).observe(submitBtn, { attributes: true, attributeFilter: ['disabled'] });
    syncRunning();

    // Map generation is off by default in the app (the map service often
    // answers "request url not found"). app.js resets the checkbox from the
    // server defaults once /api/options has loaded, which is also when the
    // protocol list gets filled, so re-apply the app's choice then.
    const mapToggle = document.getElementById('disable-maptrace');
    if (mapToggle) {
      const applyMapChoice = () => {
        let stored = null;
        try {
          stored = window.localStorage.getItem(MAP_KEY);
        } catch (err) {
          // Fall through to the default.
        }
        mapToggle.checked = stored === null ? true : stored === '1';
      };
      mapToggle.addEventListener('change', () => {
        try {
          window.localStorage.setItem(MAP_KEY, mapToggle.checked ? '1' : '0');
        } catch (err) {
          // The choice then only lasts for this session.
        }
      });
      new MutationObserver(applyMapChoice).observe(protocolSelect, { childList: true });
      applyMapChoice();
    }

    form.addEventListener('submit', () => {
      const value = target.value.trim();
      if (value) {
        saveRecent(value);
        renderRecents();
      }
      setSheetOpen(false);
      target.blur();
    });

    renderRecents();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
