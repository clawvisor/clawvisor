---
description: Fetch the Clawvisor service catalog and show what's connected
---

1. Use the `fetch_catalog` tool to get the current service catalog from Clawvisor.

2. Present the results in a clear summary:

   **Connected services** — list each active service with its available actions, noting which actions are restricted (blocked by the user).

   **Available to connect** — list services that are supported but not yet activated, so the user knows what else they can enable in the Clawvisor dashboard.

3. If any services have restrictions, explain what they mean: restricted actions are hard-blocked by the user and cannot be used regardless of task approval.

4. If the catalog fetch fails with an auth error, let the user know the MCP connection may need to be re-authorized (Settings → Plugins → Clawvisor → Reconnect).
