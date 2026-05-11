# Adapters

## OVERVIEW
Service adapters translate gateway requests into external API calls. Each adapter implements the `Adapter` interface from `pkg/adapters/` and handles credential injection, request execution, and response formatting for one service. Adapters never hold credentials themselves; Clawvisor injects decrypted credentials at execution time.

## STRUCTURE
```
internal/adapters/
├── apple/imessage/       # iMessage (macOS only, delegates to imessage-helper binary)
├── definitions/          # YAML adapter definitions (17 files, embedded via go:embed)
├── dropbox/              # Dropbox (Go adapter)
├── format/               # Response sanitization helpers (StripSecrets, SanitizeText, etc.)
├── google/
│   ├── calendar/         # Google Calendar
│   ├── contacts/         # Google Contacts
│   ├── credential/       # Shared Google OAuth credential handling
│   ├── drive/            # Google Drive
│   └── gmail/            # Gmail
├── microsoft/
│   ├── credential/       # Shared Microsoft OAuth credential handling
│   ├── onedrive/         # OneDrive
│   ├── outlook/          # Outlook
│   └── teams/            # Microsoft Teams (Go adapter)
├── perplexity/           # Perplexity AI (Go adapter)
└── sql/                  # SQL database adapter (Go adapter)
```

Services defined purely in YAML (no Go package): `github`, `slack`, `notion`, `linear`, `stripe`, `twilio`. These run through `pkg/adapters/yamlruntime/`.

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Add a Go adapter | Create package under `internal/adapters/<service>/`, implement `pkg/adapters.Adapter` |
| Add a YAML adapter | Add definition in `internal/adapters/definitions/`, runtime lives in `pkg/adapters/yamlruntime/` |
| Change adapter interface | `pkg/adapters/adapters.go` |
| Change response formatting | `internal/adapters/format/format.go` |
| Generate adapters from API specs | `internal/adaptergen/` (LLM-powered YAML generation + risk classification) |
| Add optional adapter capability | `pkg/adapters/adapters.go` (ContactsChecker, IdentityFetcher, VerificationHinter, etc.) |
| Shared OAuth credential logic | `internal/adapters/google/credential/`, `internal/adapters/microsoft/credential/` |

## CONVENTIONS
- Two adapter flavors: **Go adapters** (full control, complex APIs) and **YAML adapters** (declarative, REST/GraphQL, no Go code needed).
- Go adapters register via `adapters.Registry.Register()` at server startup.
- YAML adapters are embedded from `definitions/` and loaded by `yamlruntime` at startup.
- Google services share a single OAuth connection and vault key (`google`). Microsoft services share `microsoft`.
- Each adapter declares a `serviceID` constant (e.g. `"google.gmail"`, `"github"`).
- Optional interfaces (`MetadataProvider`, `ContactsChecker`, `AvailabilityChecker`, `ActivationChecker`, `IdentityFetcher`, `VerificationHinter`, `ActionScoper`, `ActionParamDescriber`, `APIKeyCredentialBuilder`) are discovered via type assertion at runtime.
- iMessage adapter delegates to a separate `imessage-helper` binary for Full Disk Access isolation on macOS.

## ANTI-PATTERNS
- NEVER log credentials, tokens, or full request/response bodies.
- NEVER store credentials in adapter code; they are injected via `Request.Credential`.
- NEVER return raw API responses to the gateway; always route through `format` package helpers.
- NEVER skip `format.StripSecrets` on map data before returning results.
- NEVER skip `format.SanitizeText` on free-text fields (strips HTML, dangerous Unicode).
- Respect all chain-context anti-patterns from root AGENTS.md when formatting adapter responses.