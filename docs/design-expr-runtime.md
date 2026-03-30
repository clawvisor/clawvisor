# Design Doc: Expression-Driven YAML Runtime

## Problem

The YAML adapter system allows community contributors to define new service integrations without writing Go. However, 13 of 18 Google adapter actions still require Go overrides because the YAML runtime can't express:

1. **Nested response extraction** — Google APIs return deeply nested structures (`names[0].displayName`, `emailAddresses[0].value`) that flat field mapping can't reach.
2. **Parameter transforms** — Date coercion (ISO date to RFC3339), query wrapping (`"fullText contains '...'"` for Drive search), conditional defaults (`timeMin` defaults to `now()`).
3. **Multi-step requests** — Some actions need 2+ API calls where later calls depend on earlier results (Drive `get_file` fetches metadata, then conditionally fetches content based on MIME type).
4. **Request encoding** — Gmail requires RFC822 MIME message construction. Drive requires multipart/related uploads. These are well-specified standards but the runtime only knows JSON.
5. **Conditional fields** — Calendar `update_event` only includes fields that were provided (PATCH semantics). Contacts actions include optional fields only when present.

Every new service that touches email, file storage, calendar, or contacts will hit these same patterns. Without solving them in the runtime, each one requires a custom Go package — defeating the purpose of YAML-driven adapters.

## Proposed Solution

Integrate [expr-lang/expr](https://github.com/expr-lang/expr) as an expression evaluator in the YAML runtime. Expr is a Go expression language that is:

- **Sandboxed** — expressions can only access data and functions explicitly provided in the environment. No filesystem, network, or import access.
- **Always-terminating** — no general-purpose loops, only bounded operations (map, filter, reduce over finite collections).
- **Type-checked at compile time** — expressions are compiled once when the YAML is loaded and reused across requests.
- **Battle-tested** — used by Google, Uber, Argo, OpenTelemetry, CrowdSec, KEDA.

Expr handles the data wiring (field access, conditionals, array transforms). A small set of custom functions (plugins) handles protocol-specific encoding (MIME, multipart). Adapter authors never write Go — they compose expressions from the available functions.

## YAML Syntax Extensions

### 1. Nested field paths via expressions

**Before (Go override required):**
```go
func (a *ContactsAdapter) listContacts(...) {
    for _, c := range resp.Connections {
        name := ""
        if len(c.Names) > 0 {
            name = c.Names[0].DisplayName
        }
        email := ""
        if len(c.EmailAddresses) > 0 {
            email = c.EmailAddresses[0].Value
        }
        // ...
    }
}
```

**After (pure YAML):**
```yaml
list_contacts:
  method: GET
  path: "/people/me/connections"
  params:
    max_results: { type: int, default: 100, max: 1000, map_to: pageSize, location: query }
    person_fields:
      type: string
      default: "names,emailAddresses,phoneNumbers"
      location: query
      map_to: personFields
  response:
    data_path: "connections"
    fields:
      - name: display_name
        expr: "names[0]?.displayName ?? ''"
      - name: email
        expr: "emailAddresses[0]?.value ?? ''"
      - name: phone
        expr: "phoneNumbers[0]?.value ?? ''"
        optional: true
    summary: "{{len .Data}} contact(s)"
```

Expr's optional chaining (`?.`) and nil coalescing (`??`) handle missing fields without conditionals.

**Eliminates:** All 3 Contacts overrides.

### 2. Parameter transforms

**Before (Go override required):**
```go
func (a *CalendarAdapter) listEvents(...) {
    timeMin := time.Now().Format(time.RFC3339)
    if v, ok := params["from"].(string); ok {
        if !strings.Contains(v, "T") {
            v += "T00:00:00Z"
        }
        timeMin = v
    }
    timeMax := ""
    if v, ok := params["to"].(string); ok {
        if !strings.Contains(v, "T") {
            v += "T23:59:59Z"
        }
        timeMax = v
    }
}
```

**After (pure YAML):**
```yaml
list_events:
  method: GET
  path: "/calendars/{{calendar_id ?? 'primary'}}/events"
  params:
    from:
      type: string
      default_expr: "rfc3339(now())"
      transform: "rfc3339(from)"
      map_to: timeMin
      location: query
    to:
      type: string
      transform: "endOfDay(to)"
      map_to: timeMax
      location: query
    max_results:
      type: int
      default: 10
      max: 50
      map_to: maxResults
      location: query
```

**Eliminates:** Calendar `list_events`. Combined with conditional body building (below), also eliminates `create_event` and `update_event`.

### 3. Request pipelines (chained API calls)

**Before (Go override required):**
```go
func (a *DriveAdapter) getFile(...) {
    // Step 1: fetch metadata
    var meta fileMetadata
    apiGET(ctx, client, metadataURL, &meta)

    // Step 2: conditionally fetch content
    if textMimeTypes[meta.MimeType] {
        content := fetchContent(ctx, client, contentURL)
        // merge into result
    }
}
```

**After (pure YAML):**
```yaml
get_file:
  steps:
    - id: metadata
      method: GET
      path: "/files/{{file_id}}"
      params:
        file_id: { type: string, required: true, location: path }
        fields: { type: string, default: "id,name,mimeType,modifiedTime,size,webViewLink", location: query }

    - id: content
      method: GET
      path: "/files/{{file_id}}?alt=media"
      condition: "metadata.mimeType in textMimeTypes"
      response:
        max_bytes: 50000

  response:
    fields:
      - name: id
        expr: "metadata.id"
      - name: name
        expr: "sanitize(metadata.name)"
      - name: mime_type
        expr: "metadata.mimeType"
      - name: content
        expr: "content?.body ?? ''"
        optional: true
    summary: "File: {{.name}}"
```

Each step's response is available to subsequent steps by its `id`. The `condition` field is an expr expression — the step is skipped when it evaluates to false.

**Eliminates:** Drive `get_file`, Drive `update_file`.

### 4. Request encodings

**Before (Go override required):**
```go
func (a *GmailAdapter) sendMessage(...) {
    var buf bytes.Buffer
    fmt.Fprintf(&buf, "To: %s\r\n", to)
    fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
    fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
    fmt.Fprintf(&buf, "Content-Type: text/plain; charset=\"utf-8\"\r\n\r\n")
    buf.WriteString(body)
    raw := base64.URLEncoding.EncodeToString(buf.Bytes())
    // POST {"raw": raw}
}
```

**After (pure YAML):**
```yaml
send_message:
  method: POST
  path: "/users/me/messages/send"
  encoding: mime/rfc822
  params:
    to: { type: string, required: true, role: recipient }
    subject: { type: string, required: true, role: subject }
    body: { type: string, required: true, role: body }
    cc: { type: string, role: cc }
  response:
    fields:
      - { name: id }
      - { name: threadId }
    summary: "Sent message {{.id}}"
```

The `encoding` field selects a built-in encoder. The runtime maps param `role` annotations to the encoder's expected inputs. Supported encodings:

| Encoding | Use case | Wrapping |
|---|---|---|
| `json` (default) | Standard REST APIs | `Content-Type: application/json` |
| `form` | URL-encoded forms | `Content-Type: application/x-www-form-urlencoded` |
| `multipart/form-data` | File uploads (Slack, Discord) | Multipart with boundary |
| `multipart/related` | Metadata + binary (Google Drive) | JSON part + content part |
| `mime/rfc822` | Email (Gmail, Outlook, etc.) | RFC822 headers + base64 body, wrapped in `{"raw": "..."}` |

**Eliminates:** Gmail `send_message`, Gmail `create_draft`, Drive `create_file`.

### 5. Conditional body building

**Before (Go override required):**
```go
func (a *CalendarAdapter) updateEvent(...) {
    body := map[string]any{}
    if summary, ok := params["summary"]; ok {
        body["summary"] = summary
    }
    if location, ok := params["location"]; ok {
        body["location"] = location
    }
    if start, ok := params["start"]; ok {
        body["start"] = calendarDtField(start)
    }
    // PATCH with only provided fields
}
```

**After (pure YAML):**
```yaml
update_event:
  method: PATCH
  path: "/calendars/{{calendar_id ?? 'primary'}}/events/{{event_id}}"
  body_mode: sparse    # only include params that were provided
  params:
    event_id: { type: string, required: true, location: path }
    calendar_id: { type: string, default: "primary", location: path }
    summary: { type: string, location: body }
    location: { type: string, location: body }
    start:
      type: string
      location: body
      transform: "isAllDay(start) ? {date: start} : {dateTime: rfc3339(start)}"
    end:
      type: string
      location: body
      transform: "isAllDay(end) ? {date: end} : {dateTime: rfc3339(end)}"
    attendees:
      type: list
      location: body
      transform: "map(attendees, {email: #})"
```

`body_mode: sparse` tells the runtime to omit fields from the JSON body when the corresponding param was not provided by the caller. Combined with `transform`, this handles Calendar's all-day vs timed event detection and attendee object construction.

**Eliminates:** Calendar `create_event`, Calendar `update_event`.

### 6. Response post-processing

**Before (Go override required):**
```go
func (a *GmailAdapter) getMessage(...) {
    // Recursive MIME walk to find text/plain or text/html part
    body := extractBody(payload)
    decoded, _ := base64.URLEncoding.DecodeString(body)
    text := stripHTML(string(decoded))
}
```

**After (pure YAML):**
```yaml
get_message:
  steps:
    - id: raw
      method: GET
      path: "/users/me/messages/{{message_id}}"
      params:
        message_id: { type: string, required: true, location: path }
        format: { type: string, default: "full", location: query }

  response:
    fields:
      - name: id
        expr: "raw.id"
      - name: subject
        expr: "findHeader(raw.payload.headers, 'Subject')"
      - name: from
        expr: "findHeader(raw.payload.headers, 'From')"
      - name: date
        expr: "findHeader(raw.payload.headers, 'Date')"
      - name: body
        expr: "stripHTML(base64Decode(extractMimeBody(raw.payload)))"
      - name: snippet
        expr: "sanitize(raw.snippet, 200)"
    summary: "Message from {{.from}}: {{.subject}}"
```

The heavy lifting is in three custom functions: `extractMimeBody` (recursive MIME walk), `base64Decode`, and `stripHTML`. These are generic enough to be reusable by any email adapter.

**Eliminates:** Gmail `list_messages`, Gmail `get_message`.

## Custom Functions (Plugins)

All registered in the expr environment at YAML load time. Community adapters can use any of them.

### Date/Time
| Function | Description |
|---|---|
| `rfc3339(value)` | Coerce date string to RFC3339. Handles `YYYY-MM-DD`, `YYYY-MM-DDTHH:MM:SS`, epoch seconds. |
| `endOfDay(value)` | Like `rfc3339` but appends `T23:59:59Z` for date-only inputs. |
| `isAllDay(value)` | Returns true if value is date-only (no time component). |

### String/Encoding
| Function | Description |
|---|---|
| `sanitize(s, maxLen?)` | Truncate + strip control characters. |
| `base64Decode(s)` | URL-safe base64 decode. |
| `stripHTML(s)` | Remove HTML tags and decode entities. |

### Email
| Function | Description |
|---|---|
| `extractMimeBody(payload)` | Recursive MIME walk, returns text/plain or text/html body part. |
| `findHeader(headers, name)` | Find a header value by name in a `[{name, value}]` array. |

### Collections
| Function | Description |
|---|---|
| `textMimeTypes` | Set of MIME types considered "text" for content preview. |

Expr also provides built-in `map`, `filter`, `reduce`, `find`, `all`, `any`, `sort`, `len`, `contains`, `startsWith`, `endsWith`, `upper`, `lower`, `trim`, `split`, `join`.

## Override Elimination Summary

| Service | Action | Current | After | Enabling Feature |
|---|---|---|---|---|
| Contacts | `list_contacts` | Go | YAML | Field path expressions |
| Contacts | `get_contact` | Go | YAML | Field path expressions |
| Contacts | `search_contacts` | Go | YAML | Field path expressions |
| Calendar | `list_events` | Go | YAML | Parameter transforms |
| Calendar | `create_event` | Go | YAML | Transforms + sparse body |
| Calendar | `update_event` | Go | YAML | Transforms + sparse body |
| Drive | `get_file` | Go | YAML | Request pipelines |
| Drive | `create_file` | Go | YAML | multipart/related encoding |
| Drive | `search_files` | Go | YAML | Parameter transforms |
| Gmail | `list_messages` | Go | YAML | Pipelines + field expressions |
| Gmail | `get_message` | Go | YAML | Pipelines + post-processing |
| Gmail | `send_message` | Go | YAML | mime/rfc822 encoding |
| Gmail | `create_draft` | Go | YAML | mime/rfc822 encoding |

**Result: 13/13 Go overrides eliminated. All Google adapters become pure YAML.**

## Implementation Order

Each phase is independently shippable and immediately useful:

### Phase 1: Field path expressions
- Add `expr` field to response field definitions
- Compile expressions at YAML load time
- Apply during response mapping
- **Unblocks:** Contacts (3 overrides), any community adapter hitting nested APIs

### Phase 2: Parameter transforms + sparse body
- Add `transform`, `map_to`, `default_expr` to param definitions
- Add `body_mode: sparse` to action definitions
- **Unblocks:** Calendar (3 overrides), Drive `search_files` (1 override)

### Phase 3: Request pipelines
- Add `steps` with `id`, `condition`, and cross-step references
- **Unblocks:** Drive `get_file` (1 override), Gmail `list_messages` and `get_message` (2 overrides)

### Phase 4: Request encodings
- Add `encoding` field with `multipart/related` and `mime/rfc822` encoders
- Add `role` annotation to params for encoder input mapping
- **Unblocks:** Gmail `send_message` and `create_draft` (2 overrides), Drive `create_file` (1 override)

## Security

- **Expr is sandboxed by design.** Expressions cannot access the filesystem, network, or any Go package not explicitly injected. The environment contains only the request params, step results, and registered functions.
- **Always-terminating.** No unbounded loops. `map`/`filter`/`reduce` operate on finite collections from API responses.
- **Compiled once, run many.** Expressions are compiled and type-checked at YAML load time. A malformed expression in `~/.clawvisor/adapters/` fails fast at startup, not at request time.
- **Custom functions are read-only.** `sanitize`, `base64Decode`, `stripHTML`, etc. are pure functions with no side effects.
- **No escalation path.** A community adapter's expressions can only transform data within its own request/response cycle. They cannot access other adapters' credentials, vault entries, or internal state.

## Open Questions

1. **Expression error handling.** If a field expression returns nil or errors (e.g., API response missing expected structure), should the field be omitted, set to empty string, or should the action fail? Proposal: omit optional fields, fail on required fields.
2. **Expression debugging.** When a community adapter's expression doesn't produce expected results, how do authors debug? Proposal: `clawvisor adapter test <file.yaml>` CLI command that loads a YAML definition, runs a mock request, and prints intermediate expression results.
3. **Plugin extensibility.** Should community adapters be able to register their own custom functions, or is the built-in set sufficient? Proposal: start with built-in only; add plugin registration if concrete demand emerges.
