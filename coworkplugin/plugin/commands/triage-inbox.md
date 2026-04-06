---
description: Triage recent emails — classify by urgency and action needed
---

1. Use `fetch_catalog` to confirm Gmail (`google.gmail`) is connected and `list_messages` / `get_message` are not restricted.

2. Use `create_task` with:
   - **purpose**: "Triage recent inbox emails — classify by urgency and identify action items"
   - **authorized_actions**:
     - `google.gmail` / `list_messages` — `auto_execute: true` — "List recent inbox emails to identify ones needing triage"
     - `google.gmail` / `get_message` — `auto_execute: true` — "Read individual emails to classify urgency and extract action items"
   - **planned_calls**:
     - `google.gmail` / `list_messages` — params: `{"query": "newer_than:2d", "max_results": 20}` — "List recent inbox emails for triage"
     - `google.gmail` / `get_message` — params: `{"message_id": "$chain"}` — "Read each email from the listing to classify urgency"
   - **expires_in_seconds**: 1800

3. Tell the user: "I've requested access to read your recent emails. Please approve the task in Clawvisor."

4. Use `get_task` with `wait: true` to long-poll until the task is approved. If denied, let the user know and stop.

5. Once approved, use `gateway_request` to list recent inbox messages (last 24-48 hours or ~20 messages, whichever is more useful).

6. For each message, read it with `gateway_request` and classify:
   - **Urgent / action needed** — requires a response or action today
   - **Follow-up** — needs attention but not time-sensitive
   - **FYI** — informational, no action needed
   - **Low priority** — newsletters, notifications, etc.

7. Present the triage as a structured summary:
   ```
   📬 Inbox Triage — <date>

   🔴 Urgent (action needed)
   - <sender> — <subject> — <one-line summary of what's needed>

   🟡 Follow-up
   - <sender> — <subject> — <one-line summary>

   🟢 FYI
   - <sender> — <subject>

   ⚪ Low priority
   - <count> newsletters/notifications skipped
   ```

8. Ask if the user wants to drill into any email or draft a reply. If they want to reply, use `expand_task` to add `create_draft` with `auto_execute: false`.

9. Use `complete_task` when done.
