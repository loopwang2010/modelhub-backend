// Veo 3.0 Pro — text-to-video example (Node 20+).
//
// Same cookie-jar pattern as the Flux Node sample. SLA = 15 min.

import { CookieJar } from 'tough-cookie';
import { fetch } from 'undici';

const base = process.env.MODELHUB_URL ?? 'https://modelhub.example.com';
const jar = new CookieJar();

async function f(url, opts = {}) {
  const cookieHeader = await jar.getCookieString(url);
  const headers = { ...opts.headers, ...(cookieHeader ? { cookie: cookieHeader } : {}) };
  const r = await fetch(url, { ...opts, headers });
  for (const c of r.headers.getSetCookie?.() ?? []) await jar.setCookie(c, url);
  return r;
}

await (await f(`${base}/v1/auth/login`, {
  method: 'POST', headers: { 'content-type': 'application/json' },
  body: JSON.stringify({ email: process.env.MH_EMAIL, password: process.env.MH_PASSWORD }),
})).json();

const submit = await (await f(`${base}/v1/generations`, {
  method: 'POST', headers: { 'content-type': 'application/json' },
  body: JSON.stringify({
    model: 'veo-3.0-pro',
    params: {
      prompt: 'A drone shot orbiting a snow-covered cabin, golden hour, cinematic',
      duration_seconds: 5,
      aspect_ratio: '16:9',
    },
  }),
})).json();
console.log(`Submitted: ${submit.id} (held $${(submit.credits.held / 1e6).toFixed(2)})`);

const deadline = Date.now() + 15 * 60 * 1000;
let poll = 0;
while (Date.now() < deadline) {
  poll++;
  const r = await (await f(`${base}/v1/generations/${submit.id}`)).json();
  if (r.status === 'succeeded') {
    console.log(`Done after ${poll} polls. URL: ${r.output.url}`);
    process.exit(0);
  }
  if (r.status === 'failed') {
    console.error('Failed:', r.error); process.exit(1);
  }
  if (poll % 3 === 0) console.log(`  ...still ${r.status} (poll ${poll})`);
  await new Promise(res => setTimeout(res, 10000));
}
console.error(`Timeout: ${submit.id}`); process.exit(1);
