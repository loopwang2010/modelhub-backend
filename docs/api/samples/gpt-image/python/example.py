"""gpt-image-1-edit — image edit via the requests library.

Three steps:
    1. POST /v1/uploads to mint a pre-signed PUT URL.
    2. PUT the source image bytes directly to that URL.
    3. POST /v1/generations referencing the upload_id.
       The model is async — we poll for completion.

Usage:
    python example.py ./cat.png "add a small wizard hat"

Environment:
    MODELHUB_BASE        base URL (default http://localhost:6666)
    MODELHUB_EMAIL       login email
    MODELHUB_PASSWORD    login password
    MODELHUB_POLL_MAX_S  max wait in seconds (default 300)

Anti-pattern note (AP-17):
    The backend validates the source image after upload. Files larger
    than 50 MiB are rejected at /v1/uploads time; MIME mismatches and
    forbidden content types (SVG, executables, polyglots) are rejected
    after the PUT lands. Plan the size cap accordingly.
"""
from __future__ import annotations

import mimetypes
import os
import pathlib
import sys
import time

import requests


BASE = os.environ.get("MODELHUB_BASE", "http://localhost:6666")
EMAIL = os.environ.get("MODELHUB_EMAIL", "alice@example.com")
PASSWORD = os.environ.get("MODELHUB_PASSWORD", "correct horse battery staple")
POLL_MAX_S = int(os.environ.get("MODELHUB_POLL_MAX_S", "300"))

ALLOWED_CT = {"image/jpeg", "image/png", "image/webp", "image/gif"}
TERMINAL = {"succeeded", "failed", "cancelled"}


def login(session: requests.Session) -> None:
    res = session.post(
        f"{BASE}/v1/auth/login",
        json={"email": EMAIL, "password": PASSWORD},
        timeout=15,
    )
    res.raise_for_status()


def mint_upload(session: requests.Session, src: pathlib.Path) -> dict:
    """POST /v1/uploads to get a pre-signed PUT URL."""
    ct, _ = mimetypes.guess_type(src.name)
    if ct not in ALLOWED_CT:
        raise ValueError(
            f"unsupported source content type: {ct!r}; "
            f"allowed = {sorted(ALLOWED_CT)}"
        )
    size = src.stat().st_size
    res = session.post(
        f"{BASE}/v1/uploads",
        json={
            "content_type": ct,
            "size_bytes": size,
            "filename": src.name,
        },
        timeout=15,
    )
    if res.status_code == 400:
        raise RuntimeError(
            f"upload pre-flight rejected: {res.json().get('error', {}).get('message')}"
        )
    res.raise_for_status()
    return res.json()


def put_bytes(upload: dict, src: pathlib.Path) -> None:
    """Stream the source bytes to the pre-signed PUT URL."""
    method = upload["method"]
    if method != "PUT":
        raise RuntimeError(f"unexpected upload method: {method}")
    headers = dict(upload.get("headers") or {})
    # The backend echoes any required headers; falling back to the
    # Content-Type we declared is safe.
    headers.setdefault("Content-Type", "application/octet-stream")
    with src.open("rb") as fh:
        res = requests.put(upload["upload_url"], data=fh, headers=headers, timeout=120)
    res.raise_for_status()


def submit(session: requests.Session, upload_id: str, prompt: str) -> dict:
    """POST /v1/generations for gpt-image-1-edit. Returns the initial envelope."""
    res = session.post(
        f"{BASE}/v1/generations",
        json={
            "model": "gpt-image-1-edit",
            "params": {
                "prompt": prompt,
                "image": {"upload_id": upload_id},
            },
        },
        timeout=30,
    )
    if res.status_code == 401:
        raise RuntimeError("session expired — re-login")
    if res.status_code == 402:
        raise RuntimeError(f"payment required: {res.json()['error']['message']}")
    if res.status_code == 429:
        raise RuntimeError(f"rate limited: {res.json()['error']['message']}")
    res.raise_for_status()
    return res.json()


def poll_until_terminal(session: requests.Session, gen_id: str) -> dict:
    deadline = time.time() + POLL_MAX_S
    delay = 5.0
    while True:
        if time.time() > deadline:
            raise TimeoutError(f"task {gen_id} still in flight after {POLL_MAX_S}s")
        time.sleep(delay)
        delay = min(delay * 1.3, 30.0)

        res = session.get(f"{BASE}/v1/generations/{gen_id}", timeout=15)
        res.raise_for_status()
        env = res.json()
        sys.stderr.write(f"  status={env['status']}\n")
        if env["status"] in TERMINAL:
            return env


def main() -> int:
    if len(sys.argv) < 3:
        sys.stderr.write("usage: example.py <source-image> <prompt>\n")
        return 1
    src = pathlib.Path(sys.argv[1])
    prompt = sys.argv[2]
    if not src.exists():
        sys.stderr.write(f"source not found: {src}\n")
        return 1

    with requests.Session() as session:
        login(session)
        upload = mint_upload(session, src)
        sys.stderr.write(f"upload_id: {upload['upload_id']}\n")
        put_bytes(upload, src)
        sys.stderr.write(f"uploaded:  {src.stat().st_size} bytes\n")

        initial = submit(session, upload["upload_id"], prompt)
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
        if output.get("type") != "image_url":
            sys.stderr.write(f"unexpected output type: {output.get('type')}\n")
            return 2

        print(f"id:      {env['id']}")
        print(f"status:  {env['status']}")
        print(f"edited:  {output['url']}")
        print(f"settled: {env['credits']['settled']} micro-USD")
        return 0


if __name__ == "__main__":
    raise SystemExit(main())
