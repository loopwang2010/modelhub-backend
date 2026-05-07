"""Veo 3.0 Pro — text-to-video example.

Veo3 typically takes 1-5 minutes. This sample waits up to 15 min (SLA per design).

Usage:
    export MODELHUB_URL=...; export MH_EMAIL=...; export MH_PASSWORD=...
    python python.py
"""

import os, time, requests

BASE = os.environ.get("MODELHUB_URL", "https://modelhub.example.com")
session = requests.Session()
session.post(f"{BASE}/v1/auth/login", json={
    "email": os.environ["MH_EMAIL"],
    "password": os.environ["MH_PASSWORD"],
}).raise_for_status()

resp = session.post(f"{BASE}/v1/generations", json={
    "model": "veo-3.0-pro",
    "params": {
        "prompt": "A drone shot orbiting a snow-covered cabin, golden hour, cinematic",
        "duration_seconds": 5,
        "aspect_ratio": "16:9",
    },
}).json()
task_id = resp["id"]
print(f"Submitted: {task_id} (held ${resp['credits']['held']/1_000_000:.2f})")

deadline = time.time() + 15 * 60
poll = 0
while time.time() < deadline:
    poll += 1
    r = session.get(f"{BASE}/v1/generations/{task_id}").json()
    status = r["status"]
    if status == "succeeded":
        print(f"Done after {poll} polls. URL: {r['output']['url']}")
        held = r["credits"]["held"] / 1_000_000
        settled = r["credits"]["settled"] / 1_000_000
        print(f"  held=${held:.2f} settled=${settled:.2f} refunded=${held-settled:.2f}")
        break
    if status == "failed":
        raise SystemExit(f"Failed: {r['error']}")
    if poll % 3 == 0:
        print(f"  ...still {status} (poll {poll})")
    time.sleep(10)
else:
    raise SystemExit(f"Timeout: task {task_id} still in flight after 15 min")
