"""Flux Pro 1.1 — text-to-image example.

Usage:
    export MODELHUB_URL=https://modelhub.example.com
    export MH_EMAIL=you@example.com
    export MH_PASSWORD=...
    python python.py
"""

import os
import time
import requests

BASE = os.environ.get("MODELHUB_URL", "https://modelhub.example.com")
session = requests.Session()
session.post(f"{BASE}/v1/auth/login", json={
    "email": os.environ["MH_EMAIL"],
    "password": os.environ["MH_PASSWORD"],
}).raise_for_status()

# Submit
resp = session.post(f"{BASE}/v1/generations", json={
    "model": "flux-pro-1.1",
    "params": {
        "prompt": "An origami fox in a foggy forest at dawn, cinematic",
        "aspect_ratio": "16:9",
        "num_images": 1,
    },
}).json()
task_id = resp["id"]
print(f"Submitted: {task_id}")

# Poll
deadline = time.time() + 300   # 5 minutes
while time.time() < deadline:
    resp = session.get(f"{BASE}/v1/generations/{task_id}").json()
    status = resp["status"]
    if status == "succeeded":
        print(f"Done. URL: {resp['output']['url']}")
        print(f"Credits: held={resp['credits']['held']}, "
              f"settled={resp['credits']['settled']}")
        break
    if status == "failed":
        raise SystemExit(f"Failed: {resp['error']}")
    time.sleep(5)
else:
    raise SystemExit(f"Timeout waiting for {task_id}")
