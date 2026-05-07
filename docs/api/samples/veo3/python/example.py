"""veo-3.0-pro — async video generation via the requests library.

Async model: submit returns 202 with status=queued. Result is fetched
either by polling GET /v1/generations/{id} or by supplying a webhook
URL on submit.

This example polls with exponential-ish back-off (cap 30s) and a
10-minute budget — Veo 3 typical latency is 60-180s but tail latency
can stretch when the upstream is busy.

Usage:
    python example.py "a slow zoom-out from a city skyline"

Environment:
    MODELHUB_BASE        base URL (default http://localhost:6666)
    MODELHUB_EMAIL       login email
    MODELHUB_PASSWORD    login password
    MODELHUB_POLL_MAX_S  max wait in seconds (default 600)
"""
from __future__ import annotations

import os
import sys
import time

import requests


BASE = os.environ.get("MODELHUB_BASE", "http://localhost:6666")
EMAIL = os.environ.get("MODELHUB_EMAIL", "alice@example.com")
PASSWORD = os.environ.get("MODELHUB_PASSWORD", "correct horse battery staple")
POLL_MAX_S = int(os.environ.get("MODELHUB_POLL_MAX_S", "600"))

TERMINAL = {"succeeded", "failed", "cancelled"}


def login(session: requests.Session) -> None:
    res = session.post(
        f"{BASE}/v1/auth/login",
        json={"email": EMAIL, "password": PASSWORD},
        timeout=15,
    )
    res.raise_for_status()


def submit(session: requests.Session, prompt: str, duration_s: int = 5) -> dict:
    """POST /v1/generations for Veo 3 — returns the initial envelope."""
    res = session.post(
        f"{BASE}/v1/generations",
        json={
            "model": "veo-3.0-pro",
            "params": {
                "prompt": prompt,
                "duration_seconds": duration_s,
            },
        },
        timeout=30,
    )
    if res.status_code == 401:
        raise RuntimeError("session expired — re-login (see docs/api/auth.md)")
    if res.status_code == 402:
        body = res.json()
        # See docs/api/limits.md: 402 with code=payment is either insufficient
        # balance or per-request cost cap (AP-16).
        raise RuntimeError(f"payment required: {body['error']['message']}")
    if res.status_code == 429:
        body = res.json()
        # Per-user concurrent ceiling (AP-15) or upstream throttle.
        raise RuntimeError(f"rate limited: {body['error']['message']}")
    res.raise_for_status()
    # Submit is 202 for async, but body is still GenerationResponse.
    return res.json()


def poll_until_terminal(session: requests.Session, gen_id: str) -> dict:
    """Poll /v1/generations/{id} until status is terminal or budget elapses."""
    deadline = time.time() + POLL_MAX_S
    delay = 5.0
    while True:
        if time.time() > deadline:
            raise TimeoutError(
                f"task {gen_id} still in flight after {POLL_MAX_S}s; "
                f"poll again later"
            )
        time.sleep(delay)
        delay = min(delay * 1.3, 30.0)  # gentle back-off, cap 30s

        res = session.get(f"{BASE}/v1/generations/{gen_id}", timeout=15)
        res.raise_for_status()
        env = res.json()
        sys.stderr.write(f"  status={env['status']}\n")
        if env["status"] in TERMINAL:
            return env


def main() -> int:
    prompt = (
        sys.argv[1]
        if len(sys.argv) > 1
        else "a slow zoom-out from a city skyline at golden hour"
    )

    with requests.Session() as session:
        login(session)
        initial = submit(session, prompt, duration_s=5)
        sys.stderr.write(
            f"submitted: {initial['id']} "
            f"(held={initial['credits']['held']} micro-USD)\n"
        )

        env = poll_until_terminal(session, initial["id"])

        if env["status"] != "succeeded":
            err = env.get("error") or {}
            sys.stderr.write(
                f"task {env['status']}: "
                f"{err.get('code')}: {err.get('message')}\n"
            )
            return 2

        output = env.get("output") or {}
        if output.get("type") != "video_url":
            sys.stderr.write(f"unexpected output type: {output.get('type')}\n")
            return 2

        print(f"id:      {env['id']}")
        print(f"status:  {env['status']}")
        print(f"video:   {output['url']}")
        print(f"settled: {env['credits']['settled']} micro-USD")
        return 0


if __name__ == "__main__":
    raise SystemExit(main())
