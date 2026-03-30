---
description: Morning briefing across email, calendar, and messages
---

1. Use `fetch_catalog` to check which services are connected. This command works best with Gmail, Calendar, and iMessage, but adapt to whatever is available.

2. Use `create_task` with:
   - **purpose**: "Daily briefing — summarize today's calendar, recent emails, and messages needing attention"
   - **authorized_actions** (include only services that are connected and not restricted):
     - `google.calendar` / `list_events` — `auto_execute: true` — "List today's calendar events for the briefing"
     - `google.gmail` / `list_messages` — `auto_execute: true` — "List recent inbox emails for the briefing"
     - `google.gmail` / `get_message` — `auto_execute: true` — "Read emails to summarize key items"
     - `apple.imessage` / `list_threads` — `auto_execute: true` — "List recent iMessage threads"
     - `apple.imessage` / `get_thread` — `auto_execute: true` — "Read threads to check reply status"
   - **expires_in_seconds**: 1800

3. Tell the user: "I've requested access for your daily briefing. Please approve the task in Clawvisor."

4. Use `get_task` with `wait: true` to long-poll until approved. If denied, stop.

5. Once approved, gather data in parallel where possible:

   **Calendar** — Fetch today's events. Note start times, meeting titles, and attendees.

   **Email** — List recent inbox messages (since yesterday). Read the important ones and classify: urgent, follow-up, or FYI.

   **Messages** — List recent iMessage threads. Identify any that need a reply.

6. Present the briefing:
   ```
   ☀️ Daily Briefing — <date>

   📅 Today's Schedule
   - <time> — <event title> (with <attendees>)
   - <time> — <event title>
   - <N> meetings total, <free hours> hours free

   📬 Email Highlights
   - 🔴 <count> urgent — <brief descriptions>
   - 🟡 <count> follow-ups
   - <total> new emails since yesterday

   💬 Messages
   - <count> threads need your reply
   - <contact>: "<preview>" — <time ago>
   ```

7. Ask if the user wants to drill into anything — read a specific email, reply to a message, or check a calendar event's details.

8. Use `complete_task` when done.
