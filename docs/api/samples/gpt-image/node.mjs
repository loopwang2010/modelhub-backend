// GPT-image-1 edit — image-in → image-out example (Node 20+).
// Two-step upload per ADR-009.

import { readFile } from 'node:fs/promises';
import { CookieJar } from 'tough-cookie';
import { fetch } from 'undici';

const base = process.env.MODELHUB_URL ?? 'https://modelhub.example.com';
const src = process.argv[2] ?? '/path/to/source.png';
const prompt = process.argv[3] ?? 'Make this look like a watercolor painting';

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

const imageBytes = await readFile(src);

// 1. Reserve upload
const upload = await (await f(`${base}/v1/uploads`, {
  method: 'POST', headers: { 'content-type': 'application/json' },
  body: JSON.stringify({
    content_type: 'image/png',
    size_bytes: imageBytes.length,
    filename: src.split(/[\\/]/).pop(),
  }),
})).json();

// 2. PUT image (direct to object storage, NOT through modelhub backend)
await fetch(upload.upload_url, {
  method: 'PUT',
  headers: { 'content-type': 'image/png' },
  body: imageBytes,
});
console.log(`Uploaded as ${upload.upload_id}`);

// 3. Submit (sync)
const r = await (await f(`${base}/v1/generations`, {
  method: 'POST', headers: { 'content-type': 'application/json' },
  body: JSON.stringify({
    model: 'gpt-image-1-edit',
    params: { prompt, image_id: upload.upload_id, size: '1024x1024' },
  }),
})).json();

if (r.status === 'succeeded') {
  console.log(`Done. URL: ${r.output.url}`);
  console.log(`  held=$${(r.credits.held / 1e6).toFixed(4)} settled=$${(r.credits.settled / 1e6).toFixed(4)}`);
} else {
  console.error('Failed:', r.error); process.exit(1);
}
