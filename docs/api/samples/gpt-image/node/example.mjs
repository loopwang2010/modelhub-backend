// gpt-image-1-edit — image edit via Node 18+ fetch.
//
// Three steps:
//   1. POST /v1/uploads to mint a pre-signed PUT URL.
//   2. PUT the source image bytes directly to that URL.
//   3. POST /v1/generations referencing the upload_id; poll until terminal.
//
// Usage:
//     node example.mjs ./cat.png "add a small wizard hat"
//
// Environment:
//     MODELHUB_BASE        base URL (default http://localhost:6666)
//     MODELHUB_EMAIL       login email
//     MODELHUB_PASSWORD    login password
//     MODELHUB_POLL_MAX_S  max wait in seconds (default 300)

import { readFile, stat } from 'node:fs/promises';
import { basename, extname } from 'node:path';

const BASE = process.env.MODELHUB_BASE ?? 'http://localhost:6666';
const EMAIL = process.env.MODELHUB_EMAIL ?? 'alice@example.com';
const PASSWORD = process.env.MODELHUB_PASSWORD ?? 'correct horse battery staple';
const POLL_MAX_S = Number(process.env.MODELHUB_POLL_MAX_S ?? 300);

const TERMINAL = new Set(['succeeded', 'failed', 'cancelled']);
const EXT_TO_CT = {
    '.png': 'image/png',
    '.jpg': 'image/jpeg',
    '.jpeg': 'image/jpeg',
    '.webp': 'image/webp',
    '.gif': 'image/gif',
};

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function login() {
    const res = await fetch(`${BASE}/v1/auth/login`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: EMAIL, password: PASSWORD }),
    });
    if (!res.ok) throw new Error(`login failed: ${res.status}`);
    return res.headers.get('set-cookie').split(';')[0];
}

async function mintUpload(cookie, src) {
    const ct = EXT_TO_CT[extname(src).toLowerCase()];
    if (!ct) throw new Error(`unsupported source extension: ${src}`);
    const { size } = await stat(src);

    const res = await fetch(`${BASE}/v1/uploads`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Cookie': cookie },
        body: JSON.stringify({
            content_type: ct,
            size_bytes: size,
            filename: basename(src),
        }),
    });
    if (res.status === 400) {
        const body = await res.json();
        throw new Error(`upload pre-flight rejected: ${body.error?.message}`);
    }
    if (!res.ok) throw new Error(`HTTP ${res.status}: ${await res.text()}`);
    return { ...(await res.json()), declaredContentType: ct };
}

async function putBytes(upload, src) {
    if (upload.method !== 'PUT') throw new Error(`unexpected method: ${upload.method}`);
    const headers = { ...(upload.headers ?? {}) };
    if (!headers['Content-Type']) headers['Content-Type'] = upload.declaredContentType;
    const body = await readFile(src);
    const res = await fetch(upload.upload_url, { method: 'PUT', headers, body });
    if (!res.ok) throw new Error(`PUT failed: ${res.status} ${await res.text()}`);
}

async function submit(cookie, uploadId, prompt) {
    const res = await fetch(`${BASE}/v1/generations`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Cookie': cookie },
        body: JSON.stringify({
            model: 'gpt-image-1-edit',
            params: { prompt, image: { upload_id: uploadId } },
        }),
    });
    if (res.status === 401) throw new Error('session expired — re-login');
    if (res.status === 402) {
        const body = await res.json();
        throw new Error(`payment required: ${body.error.message}`);
    }
    if (res.status === 429) {
        const body = await res.json();
        throw new Error(`rate limited: ${body.error.message}`);
    }
    if (!res.ok) throw new Error(`HTTP ${res.status}: ${await res.text()}`);
    return res.json();
}

async function pollUntilTerminal(cookie, genId) {
    const deadline = Date.now() + POLL_MAX_S * 1000;
    let delayMs = 5000;
    while (true) {
        if (Date.now() > deadline) {
            throw new Error(`task ${genId} still in flight after ${POLL_MAX_S}s`);
        }
        await sleep(delayMs);
        delayMs = Math.min(delayMs * 1.3, 30_000);

        const res = await fetch(`${BASE}/v1/generations/${genId}`, {
            headers: { 'Cookie': cookie },
        });
        if (!res.ok) throw new Error(`poll HTTP ${res.status}`);
        const env = await res.json();
        process.stderr.write(`  status=${env.status}\n`);
        if (TERMINAL.has(env.status)) return env;
    }
}

async function main() {
    const [, , src, prompt] = process.argv;
    if (!src || !prompt) {
        console.error('usage: node example.mjs <source-image> <prompt>');
        process.exit(1);
    }

    const cookie = await login();
    const upload = await mintUpload(cookie, src);
    process.stderr.write(`upload_id: ${upload.upload_id}\n`);
    await putBytes(upload, src);
    process.stderr.write(`uploaded:  ${(await stat(src)).size} bytes\n`);

    const initial = await submit(cookie, upload.upload_id, prompt);
    process.stderr.write(
        `submitted: ${initial.id} (held=${initial.credits.held} micro-USD)\n`,
    );

    const env = await pollUntilTerminal(cookie, initial.id);

    if (env.status !== 'succeeded') {
        const err = env.error ?? {};
        console.error(`task ${env.status}: ${err.code}: ${err.message}`);
        process.exit(2);
    }

    const output = env.output ?? {};
    if (output.type !== 'image_url') {
        console.error(`unexpected output type: ${output.type}`);
        process.exit(2);
    }

    console.log(`id:      ${env.id}`);
    console.log(`status:  ${env.status}`);
    console.log(`edited:  ${output.url}`);
    console.log(`settled: ${env.credits.settled} micro-USD`);
}

main().catch((err) => {
    console.error(err.message ?? err);
    process.exit(1);
});
