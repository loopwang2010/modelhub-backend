// flux-pro-1.1 — sync image generation via Node 18+ fetch.
//
// Sync model: a single POST blocks ~5-10s and returns the finished
// image inline. Cost: ~$0.04 per 1MP image.
//
// Usage:
//     node example.mjs "a forest at dawn"
//
// Environment:
//     MODELHUB_BASE      base URL (default http://localhost:6666)
//     MODELHUB_EMAIL     login email
//     MODELHUB_PASSWORD  login password
//
// Cookies in plain Node fetch require manual handling — fetch does not
// keep a cookie jar by default. We extract the Set-Cookie from the
// login response and replay it as Cookie on subsequent requests. For
// real applications, prefer `node-fetch-cookies` or `axios` with a
// cookie-jar adapter.

const BASE = process.env.MODELHUB_BASE ?? 'http://localhost:6666';
const EMAIL = process.env.MODELHUB_EMAIL ?? 'alice@example.com';
const PASSWORD = process.env.MODELHUB_PASSWORD ?? 'correct horse battery staple';

/**
 * Log in and return the raw cookie header value to replay on later requests.
 */
async function login() {
    const res = await fetch(`${BASE}/v1/auth/login`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: EMAIL, password: PASSWORD }),
    });
    if (!res.ok) {
        throw new Error(`login failed: ${res.status} ${await res.text()}`);
    }
    const setCookie = res.headers.get('set-cookie');
    if (!setCookie) {
        throw new Error('login returned 200 but no Set-Cookie — backend misconfigured');
    }
    // Set-Cookie can carry multiple cookies; for modelhub we only need the
    // first name=value pair.
    return setCookie.split(';')[0];
}

async function generate(cookie, prompt) {
    const res = await fetch(`${BASE}/v1/generations`, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json',
            'Cookie': cookie,
        },
        body: JSON.stringify({
            model: 'flux-pro-1.1',
            params: {
                prompt,
                width: 1024,
                height: 1024,
            },
        }),
    });
    if (res.status === 401) {
        throw new Error('session expired — re-login (see docs/api/auth.md)');
    }
    if (res.status === 402) {
        const body = await res.json();
        throw new Error(`payment required: ${body.error.message}`);
    }
    if (!res.ok) {
        throw new Error(`HTTP ${res.status}: ${await res.text()}`);
    }
    return res.json();
}

async function main() {
    const prompt = process.argv[2] ?? 'a forest at dawn, painterly';

    const cookie = await login();
    const env = await generate(cookie, prompt);

    if (env.status !== 'succeeded') {
        const err = env.error ?? {};
        console.error(
            `generation ${env.id} did not succeed: status=${env.status} ` +
            `code=${err.code} message=${err.message}`,
        );
        process.exit(2);
    }

    const output = env.output ?? {};
    if (output.type !== 'image_url') {
        console.error(`unexpected output type: ${output.type}`);
        process.exit(2);
    }

    console.log(`id:      ${env.id}`);
    console.log(`status:  ${env.status}`);
    console.log(`image:   ${output.url}`);
    console.log(`settled: ${env.credits.settled} micro-USD`);
}

main().catch((err) => {
    console.error(err.message ?? err);
    process.exit(1);
});
