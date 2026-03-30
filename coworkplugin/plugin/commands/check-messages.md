---
description: Review recent iMessage threads and identify ones needing replies
---

1. Use `fetch_catalog` to confirm iMessage (`apple.imessage`) is connected and `list_threads` / `get_thread` are not restricted.

2. Use `create_task` with:
   - **purpose**: "Review recent iMessage threads and identify ones needing replies"
   - **authorized_actions**:
     - `apple.imessage` / `list_threads` — `auto_execute: true` — "List recent iMessage threads to find ones needing attention"
     - `apple.imessage` / `get_thread` — `auto_execute: true` — "Read individual thread messages to check reply status"
   - **expires_in_seconds**: 1800

3. Tell the user: "I've requested access to read your iMessage threads. Please approve the task in Clawvisor."

4. Use `get_task` with `wait: true` to long-poll until approved. If denied, stop.

5. Once approved, use `gateway_request` to list recent threads (last 24-48 hours).

6. For each thread, read it and classify:
   - **Needs reply** — the last message is from the other person and seems to expect a response
   - **Waiting** — the last message is from the user; awaiting a reply from the other person
   - **Resolved** — conversation appears complete

7. Present the results:
   ```
   💬 iMessage Check — <date>

   📩 Needs your reply
   - <contact> — "<last message preview>" — <time ago>

   ⏳ Waiting for reply
   - <contact> — you said: "<last message preview>" — <time ago>

   ✅ Resolved
   - <count> threads with no pending action
   ```

8. Ask if the user wants to reply to any thread. If so, use `expand_task` to add `send_message` with `auto_execute: false`, then draft and send via `gateway_request`.

9. Use `complete_task` when done.
