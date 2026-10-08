/*
 * TLB suite bar — the shared top navigation for hub, cTab, Tac Map and OCAP.
 *
 * Canonical copy lives in hub/suite/. Apps get a copy via hub/sync-suite.ps1
 * (Tac Map's Content-Security-Policy only allows its own scripts).
 *
 * Include it as the FIRST element inside <body>, not deferred, so the bar is
 * in place before the page paints:
 *
 *   <link rel="stylesheet" href="/tlb/tlb-suite-bar.css">
 *   <body>
 *     <script src="/tlb/tlb-suite-bar.js" data-app="ctab"></script>
 *
 * Attributes on the script tag:
 *   data-app          which app this page belongs to: hub | ctab | tacmap | ocap
 *   data-theme-owner  "app" when the app applies its own theme on load (Tac Map);
 *                     otherwise the bar applies the saved theme itself.
 *   data-theme-toggle "off" hides the dark/light button, for an app that has
 *                     no light theme.
 *
 * Events: dispatches `tlb:themechange` on window (detail: "dark" | "light")
 * when the user toggles the theme from the bar.
 */
(function () {
  'use strict';

  var script = document.currentScript;
  var currentApp = (script && script.dataset.app) || '';
  var appOwnsTheme = script && script.dataset.themeOwner === 'app';
  var showThemeToggle = !(script && script.dataset.themeToggle === 'off');

  // Live sites run under *.tlbmilsim.com; anything else (localhost, LAN IP) is local.
  var isLive = /(^|\.)tlbmilsim\.com$/i.test(location.hostname);

  var APPS = [
    { id: 'hub', label: 'Hub', live: 'https://hub.tlbmilsim.com/', local: 'http://localhost:8000/' },
    { id: 'ctab', label: 'cTab', live: 'https://ctab.tlbmilsim.com/', local: 'http://localhost:5010/' },
    { id: 'tacmap', label: 'Tac Map', live: 'https://tacmap.tlbmilsim.com/', local: 'http://localhost:8080/' },
    { id: 'ocap', label: 'OCAP', live: 'https://ocap.tlbmilsim.com/', local: 'http://localhost:5000/' }
  ];
  var SITE = { label: 'tlbmilsim.com', href: 'https://tlbmilsim.com/' };
  // Shared Discord sign-in service (auth/).
  var AUTH = { live: 'https://auth.tlbmilsim.com/', local: 'http://localhost:5100/' };

  function urlFor(app) { return isLive ? app.live : app.local; }

  // ---- Signed-in user ------------------------------------------------------
  // tlb_user is a display-only cookie (name, avatar, Steam linked) written by the
  // auth service next to its HttpOnly session cookie. Apps never trust it for access.
  function signedInUser() {
    var match = document.cookie.match(/(?:^|;\s*)tlb_user=([A-Za-z0-9_-]+)/);
    if (!match) return null;
    try {
      var b64 = match[1].replace(/-/g, '+').replace(/_/g, '/');
      var bytes = Uint8Array.from(atob(b64 + '==='.slice((b64.length + 3) % 4)), function (c) { return c.charCodeAt(0); });
      var user = JSON.parse(new TextDecoder().decode(bytes));
      return user && typeof user.name === 'string' ? user : null;
    } catch (e) {
      return null;
    }
  }

  function authUrl(path, returnTo) {
    var url = (isLive ? AUTH.live : AUTH.local) + path;
    return returnTo ? url + '?returnUrl=' + encodeURIComponent(returnTo) : url;
  }

  // ---- Theme ---------------------------------------------------------------
  var COOKIE = 'tlb-theme';

  function storedTheme() {
    var match = document.cookie.match(/(?:^|;\s*)tlb-theme=(dark|light)/);
    return match ? match[1] : null;
  }

  function saveTheme(theme) {
    // Cookies ignore ports, so this is shared by every localhost app; live it is
    // shared across all *.tlbmilsim.com subdomains.
    var cookie = COOKIE + '=' + theme + '; path=/; max-age=31536000; SameSite=Lax';
    if (isLive) cookie += '; domain=.tlbmilsim.com';
    if (location.protocol === 'https:') cookie += '; Secure';
    document.cookie = cookie;
  }

  function currentTheme() {
    return document.documentElement.dataset.theme === 'light' ? 'light' : 'dark';
  }

  function applyTheme(theme) {
    var root = document.documentElement;
    root.dataset.theme = theme;
    root.style.colorScheme = theme;
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute('content', theme === 'light' ? '#F7F4EF' : '#100F0D');
  }

  if (!appOwnsTheme) applyTheme(storedTheme() || 'dark');

  // ---- Markup --------------------------------------------------------------
  var ICON_MOON = '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5Z"/>';
  var ICON_SUN = '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>';
  var ICON_MENU = '<path d="M4 7h16M4 12h16M4 17h16"/>';
  var ICON_EXT = '<path d="M14 5h5v5M19 5l-8 8M17 14v5H5V7h5"/>';

  function svg(paths, size) {
    return '<svg aria-hidden="true" viewBox="0 0 24 24" width="' + size + '" height="' + size +
      '" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' + paths + '</svg>';
  }

  function escape(text) {
    return String(text).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  var ICON_DISCORD = '<path d="M8.5 15.5c2.3 1 4.7 1 7 0M7 7.5c3.3-1.3 6.7-1.3 10 0 1.6 2.4 2.4 5 2.3 8-1.2 1-2.5 1.6-3.8 2l-1-1.6M7 7.5c-1.6 2.4-2.4 5-2.3 8 1.2 1 2.5 1.6 3.8 2l1-1.6"/><circle cx="9.5" cy="12" r=".8"/><circle cx="14.5" cy="12" r=".8"/>';

  var user = signedInUser();
  var account;
  if (user) {
    var avatar = typeof user.avatar === 'string' && user.avatar.indexOf('https://cdn.discordapp.com/') === 0
      ? '<img class="tlb-sb-avatar" src="' + escape(user.avatar) + '" alt="">'
      : '<span class="tlb-sb-avatar">' + escape(user.name.charAt(0).toUpperCase()) + '</span>';
    account =
      '<div class="tlb-sb-account">' +
        '<button type="button" class="tlb-sb-user" aria-haspopup="menu" aria-expanded="false" title="' + escape(user.name) + '">' +
          avatar + '<span class="tlb-sb-user-name">' + escape(user.name) + '</span>' +
        '</button>' +
        '<div class="tlb-sb-account-menu" role="menu" hidden>' +
          '<div class="tlb-sb-account-head"><strong>' + escape(user.name) + '</strong>' +
            '<small>' + (user.steam ? 'Discord · Steam linked' : 'Discord · Steam not linked') + '</small></div>' +
          '<a role="menuitem" href="' + escape(authUrl('')) + '">' + (user.steam ? 'Account' : 'Account · link Steam') + '</a>' +
          '<a role="menuitem" href="' + escape(authUrl('logout', location.href)) + '">Sign out</a>' +
        '</div>' +
      '</div>';
  } else {
    account = '<a class="tlb-sb-signin" href="' + escape(authUrl('login', location.href)) + '">' + svg(ICON_DISCORD, 16) + '<span>Sign in</span></a>';
  }

  var hub = APPS[0];
  var links = APPS.map(function (app) {
    var current = app.id === currentApp ? ' aria-current="page"' : '';
    return '<a class="tlb-sb-link" href="' + escape(urlFor(app)) + '"' + current + '>' + escape(app.label) + '</a>';
  }).join('');

  var bar = document.createElement('header');
  bar.className = 'tlb-sb';
  bar.setAttribute('data-tlb-suite-bar', '');
  bar.innerHTML =
    '<a class="tlb-sb-brand" href="' + escape(urlFor(hub)) + '" aria-label="TLB MilSim hub">' +
      '<span class="tlb-sb-logo" aria-hidden="true"></span>' +
      '<span class="tlb-sb-brand-text">TLB MilSim</span>' +
    '</a>' +
    '<nav class="tlb-sb-nav" id="tlb-sb-nav" aria-label="TLB services">' + links +
      '<a class="tlb-sb-link tlb-sb-link--site" href="' + SITE.href + '">' + SITE.label + svg(ICON_EXT, 12) + '</a>' +
    '</nav>' +
    '<div class="tlb-sb-tools">' +
      '<a class="tlb-sb-site" href="' + SITE.href + '">' + SITE.label + svg(ICON_EXT, 12) + '</a>' +
      (showThemeToggle ? '<button type="button" class="tlb-sb-icon" data-tlb-theme></button>' : '') +
      account +
      '<button type="button" class="tlb-sb-icon tlb-sb-menu" aria-label="Open navigation" aria-controls="tlb-sb-nav" aria-expanded="false">' + svg(ICON_MENU, 18) + '</button>' +
    '</div>';

  var themeButton = bar.querySelector('[data-tlb-theme]');
  var menuButton = bar.querySelector('.tlb-sb-menu');

  function renderThemeButton() {
    if (!themeButton) return;
    var theme = currentTheme();
    var next = theme === 'dark' ? 'light' : 'dark';
    themeButton.innerHTML = svg(theme === 'dark' ? ICON_MOON : ICON_SUN, 17);
    themeButton.setAttribute('aria-label', 'Switch to ' + next + ' theme');
    themeButton.title = 'Switch to ' + next + ' theme';
  }

  if (themeButton) themeButton.addEventListener('click', function () {
    var next = currentTheme() === 'dark' ? 'light' : 'dark';
    applyTheme(next);
    saveTheme(next);
    window.dispatchEvent(new CustomEvent('tlb:themechange', { detail: next }));
  });

  // Follow theme changes made by the app itself (e.g. Tac Map loading the
  // account's saved theme) and remember them for the other apps.
  new MutationObserver(function () {
    renderThemeButton();
    saveTheme(currentTheme());
  }).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

  function setMenu(open) {
    bar.classList.toggle('tlb-sb--open', open);
    menuButton.setAttribute('aria-expanded', String(open));
    menuButton.setAttribute('aria-label', open ? 'Close navigation' : 'Open navigation');
  }
  menuButton.addEventListener('click', function () { setMenu(!bar.classList.contains('tlb-sb--open')); });

  var userButton = bar.querySelector('.tlb-sb-user');
  var accountMenu = bar.querySelector('.tlb-sb-account-menu');
  function setAccountMenu(open) {
    if (!userButton) return;
    accountMenu.hidden = !open;
    userButton.setAttribute('aria-expanded', String(open));
  }
  if (userButton) userButton.addEventListener('click', function () { setAccountMenu(accountMenu.hidden); });

  document.addEventListener('click', function (event) {
    if (!bar.contains(event.target)) setMenu(false);
    if (userButton && !userButton.parentNode.contains(event.target)) setAccountMenu(false);
  });
  document.addEventListener('keydown', function (event) {
    if (event.key === 'Escape') { setMenu(false); setAccountMenu(false); }
  });

  renderThemeButton();

  function mount() { document.body.insertBefore(bar, document.body.firstChild); }
  if (document.body) mount();
  else document.addEventListener('DOMContentLoaded', mount);

  window.TLBSuite = { isLive: isLive, apps: APPS, urlFor: urlFor, storedTheme: storedTheme, user: user, authUrl: authUrl };
})();
