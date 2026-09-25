// Fetch shim, injected via NODE_OPTIONS=--require. Redirects fetch() to the
// interceptor with the real destination in X-Atomic-Host, before TLS.
// HTTP_PROXY is no alternative: undici ignores it for plain http.
"use strict";

const PROXY = process.env.ATOMIC_PROXY || "http://127.0.0.1:8082";
const realFetch = globalThis.fetch;

// Classic clients (axios etc.) would bypass the effect log silently, so they throw.
for (const mod of ["http", "https"]) {
  const m = require(`node:${mod}`);
  for (const fn of ["request", "get"]) {
    m[fn] = () => {
      throw new Error(`atomic: direct ${mod}.${fn}() is disabled; use fetch() so the effect is logged`);
    };
  }
}

globalThis.fetch = (input, init = {}) => {
  let url;
  try {
    url = new URL(input instanceof URL ? input : String(input));
  } catch {
    return realFetch(input, init);
  }
  if (!/^https?:$/.test(url.protocol) || url.hostname === "localhost" || url.hostname === "127.0.0.1") {
    return realFetch(input, init);
  }
  const headers = new Headers(init.headers || {});
  headers.set("X-Atomic-Host", url.host);
  headers.set("X-Atomic-Scheme", url.protocol.slice(0, -1));
  return realFetch(PROXY + url.pathname + url.search, { ...init, headers });
};
