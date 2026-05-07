"""GPT-image-1 edit — image-in → image-out example.

Sync model. Two-step upload (ADR-009): reserve PUT URL, upload, then submit.

Usage:
    python python.py /path/to/source.png "Make this look like a watercolor"
"""

import os, sys, requests

BASE = os.environ.get("MODELHUB_URL", "https://modelhub.example.com")
session = requests.Session()
session.post(f"{BASE}/v1/auth/login", json={
    "email": os.environ["MH_EMAIL"],
    "password": os.environ["MH_PASSWORD"],
}).raise_for_status()

src = sys.argv[1] if len(sys.argv) > 1 else "/path/to/source.png"
prompt = sys.argv[2] if len(sys.argv) > 2 else "Make this look like a watercolor painting"

with open(src, "rb") as f:
    image_bytes = f.read()

# 1. Reserve upload
upload = session.post(f"{BASE}/v1/uploads", json={
    "content_type": "image/png",
    "size_bytes": len(image_bytes),
    "filename": os.path.basename(src),
}).json()

# 2. PUT to pre-signed URL (this goes directly to object storage, NOT through modelhub backend)
requests.put(upload["upload_url"],
             data=image_bytes,
             headers={"Content-Type": "image/png"}).raise_for_status()

print(f"Uploaded as {upload['upload_id']}")

# 3. Submit (sync — returns inline)
r = session.post(f"{BASE}/v1/generations", json={
    "model": "gpt-image-1-edit",
    "params": {
        "prompt": prompt,
        "image_id": upload["upload_id"],
        "size": "1024x1024",
    },
}).json()

if r["status"] == "succeeded":
    print(f"Done. URL: {r['output']['url']}")
    held = r["credits"]["held"] / 1_000_000
    settled = r["credits"]["settled"] / 1_000_000
    print(f"  held=${held:.4f} settled=${settled:.4f}")
else:
    raise SystemExit(f"Failed: {r['error']}")
