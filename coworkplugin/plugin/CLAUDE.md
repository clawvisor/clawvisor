# Clawvisor Plugin

Clawvisor is a gatekeeper between you and external services (Gmail, Calendar, Drive, GitHub, Slack, Notion, Linear, iMessage, etc.). Every action goes through Clawvisor, which checks restrictions, validates task scopes, injects credentials, optionally routes to the user for approval, and returns a clean result. You never hold API keys.

This plugin provides MCP tools for interacting with Clawvisor and commands for common workflows.

## MCP Tools

The `clawvisor-local` MCP server provides six tools:
- `fetch_catalog` — See available services, actions, and restrictions
- `create_task` — Declare your purpose and the actions you need
- `get_task` — Check task status (supports long-polling with `wait: true`)
- `complete_task` — Mark a task as completed
- `expand_task` — Add new actions to an existing task scope
- `gateway_request` — Execute service actions under an approved task

## Commands

- `/clawvisor:check-services` — Fetch the service catalog and show what's connected
- `/clawvisor:triage-inbox` — Triage recent emails, classify by urgency and action needed
- `/clawvisor:check-messages` — Review recent iMessage threads and identify ones needing replies
- `/clawvisor:daily-briefing` — Morning briefing across email, calendar, and messages

## Skills

- **clawvisor** — Complete workflow guide for task-scoped authorization, gateway requests, and response handling
