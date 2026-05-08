"""flux-pro-1.1 — sync image generation via the requests library.

Sync model: a single POST blocks ~5-10s and returns the finished
image inline. Cost: ~$0.04 per 1MP image.

Usage:
    python example.py "a forest at dawn"

Environment:
    MODELHUB_BASE      base URL (default http://localhost:6666)
    MODELHUB_EMAIL     login email
    MODELHUB_PASSWORD  login password

The script logs in once, persists cookies in `requests.Session`, then
submits a single generation. Re-run the script and it will log in again
on every invocation; for repeated calls inside one process, reuse the
session.
"""
from __future__ import annotations

import os
import sys

import requests


BASE = os.environ.get("MODELHUB_BASE", "http://localhost:6666")
EMAIL = os.environ.get("MODELHUB_EMAIL", "alice@example.com")
PASSWORD = os.environ.get("MODELHUB_PASSWORD", "correct horse battery staple")


def login(session: requests.Session) -> None:
    """Authenticate and let the session capture the modelhub_session cookie."""
    res = session.post(
        f"{BASE}/v1/auth/login",
        json={"email": EMAIL, "password": PASSWORD},
        timeout=15,
    )
    res.raise_for_status()


def generate(session: requests.Session, prompt: str) -> dict:
    """Submit a Flux Pro 1.1 generation and return the response envelope."""
    res = session.post(
        f"{BASE}/v1/generations",
        json={
            "model": "flux-pro-1.1",
            "params": {
                "prompt": prompt,
                "width": 1024,
                "height": 1024,
            },
        },
        # Generous timeout — Flux is sync but upstream variance is real.
        timeout=60,
    )
    if res.status_code == 401:
        raise RuntimeError("session expired — re-login (see docs/api/auth.md)")
    if res.status_code == 402:
        # Insufficient credits OR per-request cost cap hit.
        # docs/api/limits.md explains the difference.
        body = res.json()
        raise RuntimeError(f"payment required: {body['error']['message']}")
    res.raise_for_status()
    return res.json()


def main() -> int:
    prompt = sys.argv[1] if len(sys.argv) > 1 else "a forest at dawn, painterly"

    with requests.Session() as session:
        login(session)
        env = generate(session, prompt)

        if env["status"] != "succeeded":
            err = env.get("error") or {}
            sys.stderr.write(
                f"generation {env['id']} did not succeed: "
                f"status={env['status']} code={err.get('code')} "
                f"message={err.get('message')}\n"
            )
            return 2

        output = env.get("output") or {}
        if output.get("type") != "image_url":
            sys.stderr.write(f"unexpected output type: {output.get('type')}\n")
            return 2

        print(f"id:       {env['id']}")
        print(f"status:   {env['status']}")
        print(f"image:    {output['url']}")
        print(f"settled:  {env['credits']['settled']} micro-USD")
        return 0


if __name__ == "__main__":
    raise SystemExit(main())
