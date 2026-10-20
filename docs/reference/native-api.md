---
description: "The native API behind Hangar's web UI and CLI, at /api/hangar/v1: conventions, authentication, pagination and the OpenAPI description."
---

# Native API

The native API serves Hangar's web UI, the `hangar` CLI and automation, at `/api/hangar/v1`. It is separate from the Hugging Face Hub API that Hangar also serves.

- JSON in and out, `snake_case` names, RFC 3339 UTC times and sizes in bytes.
- Errors are RFC 9457 problem details (`application/problem+json`).
- Lists use cursor pagination: `?cursor=&limit=` (default 50, maximum 500), answered with `{"items": [...], "next_cursor": "..."}`.
- Authenticate with `Authorization: Bearer hgr_...`, or the browser session cookie plus the `X-CSRF-Token` header.

The OpenAPI description is [`openapi.yaml`](openapi.yaml), also in the repository at [`api/openapi.yaml`](https://github.com/hangar-hub/hangar/blob/main/api/openapi.yaml).

<div id="redoc-container"></div>
<script src="https://cdn.jsdelivr.net/npm/redoc@2.5.4/bundles/redoc.standalone.js"></script>
<script>
  Redoc.init("../openapi.yaml", {hideDownloadButton: false, expandResponses: "200,201"}, document.getElementById("redoc-container"));
</script>
