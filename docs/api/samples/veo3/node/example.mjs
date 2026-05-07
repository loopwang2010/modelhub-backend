// veo-3.0-pro — async video generation via Node 18+ fetch.
//
// Async model: submit returns 202 with status=queued. We poll
// GET /v1/generations/{id} until terminal. Webhook delivery is
// covered separately in docs/api/webhooks.md.
//
// Usage:
//     node example.mjs "a slow zoom-out from a city skyline"
//
// Environment:
//     MODELHUB_BASE        base URL (default http://localhost:6666)
//     MODELHUB_EMAIL       login email
//     MODELHUB_PASSWORD    login password
//     MODELHUB_POLL_MAX_S  max wait in seconds (default 600)
//
// As in the Flux example, we manage cookies manually for portability.

const BASE = process.env.MODELHUB_BASE ?? 'http://localhost:6666';
const EMAIL = process.env.MODELHUB_EMAIL ?? 'alice@example.com';
const PASSWORD = process.env.MODELHUB_PASSWORD ?? 'correct horse battery staple';
const POLL_MAX_S = Number(process.env.MODELHUB_POLL_MAX_S ?? 600);

const TERMINAL = new Set(['succeeded', 'failed', 'cancelled']);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function login() {
    const res = await fetch(`${BASE}/v1/auth/login`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: EMAIL, password: PASSWORD }),
    });
    if (!res.ok) throw new Error(`login failed: ${res.status}`);
    const setCookie = res.headers.get('set-cookie');
    if (!setCookie) throw new Error('no Set-Cookie returned by /v1/auth/login');
    return setCookie.split(';')[0];
}

async function submit(cookie, prompt, durationS = 5) {
    const res = await fetch(`${BASE}/v1/generations`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Cookie': cookie },
        body: JSON.stringify({
            model: 'veo-3.0-pro',
            params: { prompt, duration_seconds: durationS },
        }),
    });
    if (res.status === 401) throw new Error('session expired — re-login');
    if (res.status === 402) {
        const body = await res.json();
        // See docs/api/limits.md for cost cap vs balance disambiguation.
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
    const prompt = process.argv[2] ?? 'a slow zoom-out from a city skyline at golden hour';

    const cookie = await login();
    const initial = await submit(cookie, prompt, 5);
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
    if (output.type !== 'video_url') {
        console.error(`unexpected output type: ${output.type}`);
        process.exit(2);
    }

    console.log(`id:      ${env.id}`);
    console.log(`status:  ${env.status}`);
    console.log(`video:   ${output.url}`);
    console.log(`settled: ${env.credits.settled} micro-USD`);
}

main().catch((err) => {
    console.error(err.message ?? err);
    process.exit(1);
});
