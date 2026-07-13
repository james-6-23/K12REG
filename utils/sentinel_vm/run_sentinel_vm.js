'use strict';

// Subprocess entry point for executing sentinel turnstile.dx JSVMP bytecode.
// Reads JSON from stdin: { secret, encodedPayload, userAgent?, language?, locationSearch?, timeoutMs? }
// Writes JSON to stdout:  { ok: true,  result: <normalized VM result> }
//                    or:  { ok: false, error: <string> }
//
// Usage:
//   echo '{"secret":"...","encodedPayload":"..."}' | node run_sentinel_vm.js

const { runFromInputs } = require('./sdkvm');

function makeStorage() {
  const store = new Map();
  return {
    length: 0,
    getItem(key) {
      return store.has(String(key)) ? store.get(String(key)) : null;
    },
    setItem(key, value) {
      store.set(String(key), String(value));
      this.length = store.size;
    },
    removeItem(key) {
      store.delete(String(key));
      this.length = store.size;
    },
    clear() {
      store.clear();
      this.length = 0;
    }
  };
}

function makeElement(tag) {
  return {
    tagName: String(tag).toUpperCase(),
    style: {},
    children: [],
    hidden: false,
    visibility: 'visible',
    ariaHidden: 'false',
    innerText: '',
    textContent: '',
    appendChild(child) {
      this.children.push(child);
      return child;
    },
    removeChild(child) {
      this.children = this.children.filter(item => item !== child);
    },
    getBoundingClientRect() {
      return {
        x: 0, y: 0, top: 0, left: 0,
        right: 84, bottom: 16,
        width: 84, height: 16
      };
    }
  };
}

function createRuntimeEnvironment(opts = {}) {
  const ua = opts.userAgent || (
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) '
    + 'AppleWebKit/537.36 (KHTML, like Gecko) '
    + 'Chrome/146.0.0.0 Safari/537.36'
  );
  const language = opts.language || 'en-US';
  const languages = opts.languages || [language, 'en'];
  const locationSearch = opts.locationSearch || '';
  // Default to auth.openai.com for registration flows (caller can override).
  const locationHref = opts.locationHref
    || `https://auth.openai.com/${locationSearch ? '?' + locationSearch : ''}`;
  let locationOrigin = 'https://auth.openai.com';
  let locationPathname = '/';
  try {
    const u = new URL(locationHref);
    locationOrigin = u.origin;
    locationPathname = u.pathname || '/';
  } catch (_) { /* keep defaults */ }

  const body = makeElement('body');
  body.clientWidth = 1280;
  body.clientHeight = 720;

  const documentRef = {
    body,
    head: makeElement('head'),
    documentElement: Object.assign(makeElement('html'), {
      clientWidth: 1280,
      clientHeight: 720,
      getAttribute(_name) { return null; }
    }),
    referrer: '',
    cookie: '',
    visibilityState: 'visible',
    scripts: [],
    createElement(tag) { return makeElement(tag); },
    addEventListener() {},
    removeEventListener() {}
  };

  const windowRef = {
    Reflect, Object, Math, Date, JSON,
    document: documentRef,
    history: {
      length: 2, state: null,
      pushState() {}, replaceState() {}
    },
    navigator: {
      userAgent: ua,
      language,
      languages,
      platform: 'Win32',
      vendor: 'Google Inc.',
      deviceMemory: 8,
      maxTouchPoints: 0,
      webdriver: false,
      hardwareConcurrency: 8
    },
    screen: {
      width: 1920, height: 1080,
      availWidth: 1920, availHeight: 1040,
      availLeft: 0, availTop: 0,
      colorDepth: 24, pixelDepth: 24
    },
    performance: {
      now() { return performance.now(); },
      timeOrigin: Date.now() - 12345,
      memory: { jsHeapSizeLimit: 4294705152 }
    },
    location: {
      href: locationHref,
      origin: locationOrigin,
      pathname: locationPathname,
      search: locationSearch ? ('?' + locationSearch) : '',
      hash: ''
    },
    localStorage: makeStorage(),
    sessionStorage: makeStorage(),
    setTimeout, clearTimeout, setInterval, clearInterval,
    atob: globalThis.atob || (v => Buffer.from(String(v), 'base64').toString('binary')),
    btoa: globalThis.btoa || (v => Buffer.from(String(v), 'binary').toString('base64')),
    URL, URLSearchParams, TextEncoder, TextDecoder,
    __reactRouterContext: {
      state: {
        loaderData: {
          'routes/layouts/client-auth-session-layout/layout': {
            session: {
              session_id: '',
              auth_session_logging_id: '',
              openai_client_id: '',
              app_name_enum: '',
              promo: '',
              signup_source: '',
              country_code_hint: 'SG',
              is_missing_session: false
            },
            seedCacheEntry: null
          }
        },
        actionData: null,
        errors: null
      }
    }
  };

  return { windowRef, documentRef };
}

async function readAllStdin() {
  let raw = '';
  process.stdin.setEncoding('utf8');
  for await (const chunk of process.stdin) {
    raw += chunk;
  }
  return raw;
}

(async () => {
  let input;
  try {
    const raw = await readAllStdin();
    input = JSON.parse(raw);
  } catch (e) {
    process.stdout.write(JSON.stringify({ ok: false, error: 'stdin parse failed: ' + (e && e.message || e) }));
    process.exit(2);
    return;
  }

  const secret = input.secret;
  const encodedPayload = input.encodedPayload;
  if (typeof encodedPayload !== 'string' || encodedPayload.length === 0) {
    process.stdout.write(JSON.stringify({ ok: false, error: 'encodedPayload missing or empty' }));
    process.exit(2);
    return;
  }

  const { windowRef, documentRef } = createRuntimeEnvironment(input);
  const timeoutMs = Number(input.timeoutMs) > 0 ? Number(input.timeoutMs) : 2000;

  try {
    const result = await runFromInputs(secret, encodedPayload, {
      windowRef,
      documentRef,
      timeoutMs
    });
    process.stdout.write(JSON.stringify({ ok: true, result }));
    process.exit(0);
  } catch (e) {
    process.stdout.write(JSON.stringify({
      ok: false,
      error: String(e && e.stack || e)
    }));
    process.exit(1);
  }
})();
