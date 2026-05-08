// Flux Pro 1.1 — text-to-image example (Node 20+ with native fetch).
//
// Usage:
//   export MODELHUB_URL=https://modelhub.example.com
//   export MH_EMAIL=you@example.com
//   export MH_PASSWORD=...
//   node node.mjs
//
// Note: Node's fetch needs a CookieJar to persist the session cookie.
// We use the experimental 'undici' helper that ships with Node 20+.

import { CookieJar } from 'tough-cookie';
import { fetch } from 'undici';

const base = process.env.MODELHUB_URL ?? 'https://modelhub.example.com';
const jar = new CookieJar();

async function fetchWithCookies(url, opts = {}) {
  const cookieHeader = await jar.getCookieString(url);
  const headers = { ...opts.headers, ...(cookieHeader ? { cookie: cookieHeader } : {}) };
  const resp = await fetch(url, { ...opts, headers });
  for (const c of resp.headers.getSetCookie?.() ?? []) {
    await jar.setCookie(c, url);
  }
  return resp;
}

// Login
await (await fetchWithCookies(`${base}/v1/auth/login`, {
  method: 'POST',
  headers: { 'content-type': 'application/json' },
  body: JSON.stringify({
    email: process.env.MH_EMAIL,
    password: process.env.MH_PASSWORD,
  }),
})).json();

// Submit
const submit = await (await fetchWithCookies(`${base}/v1/generations`, {
  method: 'POST',
  headers: { 'content-type': 'application/json' },
  body: JSON.stringify({
    model: 'flux-pro-1.1',
    params: {
      prompt: 'An origami fox in a foggy forest at dawn, cinematic',
      aspect_ratio: '16:9',
      num_images: 1,
    },
  }),
})).json();
const taskId = submit.id;
console.log(`Submitted: ${taskId}`);

// Poll
const deadline = Date.now() + 5 * 60 * 1000;
while (Date.now() < deadline) {
  const r = await (await fetchWithCookies(`${base}/v1/generations/${taskId}`)).json();
  if (r.status === 'succeeded') {
    console.log(`Done. URL: ${r.output.url}`);
    console.log(`Credits: held=${r.credits.held}, settled=${r.credits.settled}`);
    process.exit(0);
  }
  if (r.status === 'failed') {
    console.error(`Failed:`, r.error);
    process.exit(1);
  }
  await new Promise(res => setTimeout(res, 5000));
}
console.error(`Timeout waiting for ${taskId}`);
process.exit(1);
