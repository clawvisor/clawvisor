# Changelog

## [0.8.2](https://github.com/clawvisor/clawvisor/compare/v0.8.1...v0.8.2) (2026-04-04)


### Bug Fixes

* render SKILL.md template instead of serving missing static file ([#108](https://github.com/clawvisor/clawvisor/issues/108)) ([c841da1](https://github.com/clawvisor/clawvisor/commit/c841da1f50a5b1a79d099d9c48a74eacc277a653))
* resolve iMessage truncation for messages over 128 bytes ([#106](https://github.com/clawvisor/clawvisor/issues/106)) ([4c3323a](https://github.com/clawvisor/clawvisor/commit/4c3323a789e8bf5b45e2194dda7ab632c8c4e061))

## [0.8.1](https://github.com/clawvisor/clawvisor/compare/v0.8.0...v0.8.1) (2026-04-02)


### Features

* add parameter docs to skill catalog with compact overview and detail view ([#101](https://github.com/clawvisor/clawvisor/issues/101)) ([ccdf498](https://github.com/clawvisor/clawvisor/commit/ccdf498851c63e9a1a9e37ca82eb0d8897d2d836))
* add Slack OAuth PKCE flow with relay support ([#104](https://github.com/clawvisor/clawvisor/issues/104)) ([97e8fe3](https://github.com/clawvisor/clawvisor/commit/97e8fe3b5af09e91508427b6a907c591e95a9111))
* add SQL adapter for Postgres, MySQL, and SQLite ([#103](https://github.com/clawvisor/clawvisor/issues/103)) ([c7ca037](https://github.com/clawvisor/clawvisor/commit/c7ca037c4e89a2d07bc75c959dbce70d447d1cb0))
* GitHub OAuth device flow ([#100](https://github.com/clawvisor/clawvisor/issues/100)) ([e3024d5](https://github.com/clawvisor/clawvisor/commit/e3024d59909735a6a503d9b910a47417f3a6da9e))


### Bug Fixes

* add USER nonroot to Dockerfile, remove stale openclaw compose ([de1c4d6](https://github.com/clawvisor/clawvisor/commit/de1c4d6ba51efa37afb35cd34bbb320e1beaddab))

## [0.8.0](https://github.com/clawvisor/clawvisor/compare/v0.7.11...v0.8.0) (2026-03-30)


### ⚠ BREAKING CHANGES

* POST /api/approvals/{request_id}/approve no longer executes the request. Agents must call POST /api/gateway/request/{request_id}/execute after approval to get results. Callback payloads now send status "approved" instead of "executed".
* return 404 for unregistered /api/ routes instead of serving SPA

### Features

* add --open flag to server command to auto-open magic link in browser ([#12](https://github.com/clawvisor/clawvisor/issues/12)) ([8ca56f4](https://github.com/clawvisor/clawvisor/commit/8ca56f44d404967cda8e244849018fc1cc383d83))
* add `make docker` to run clawvisor with ~/.clawvisor mounted ([#57](https://github.com/clawvisor/clawvisor/issues/57)) ([b26dcc4](https://github.com/clawvisor/clawvisor/commit/b26dcc409e09d1c4f9ac2e8f9c6206a1041b32da))
* add 84 intent verification eval cases and chain context to setup wizard ([98a1a98](https://github.com/clawvisor/clawvisor/commit/98a1a98ab96eb47a9949b080b9578c0cf72912a3))
* add agent-driven setup guide and env var pass-through ([3c72cde](https://github.com/clawvisor/clawvisor/commit/3c72cdea691354e2cc6afae7fc44a7b70d080d99))
* add build-time staging environment support ([#62](https://github.com/clawvisor/clawvisor/issues/62)) ([5b8d1ad](https://github.com/clawvisor/clawvisor/commit/5b8d1ad452c08b0ee8db42bb127ca7a4b42e354e))
* add chain context verification for multi-step task safety ([51b1fa3](https://github.com/clawvisor/clawvisor/commit/51b1fa380c1af951ab10da12c7970ee925cd39f7))
* add create_draft action to Gmail adapter ([#84](https://github.com/clawvisor/clawvisor/issues/84)) ([9c57827](https://github.com/clawvisor/clawvisor/commit/9c57827228f7b03b500ef2022c2a984e270ef981))
* add curl auto-approve, smoke test, and setup cleanup ([#65](https://github.com/clawvisor/clawvisor/issues/65)) ([bfde881](https://github.com/clawvisor/clawvisor/commit/bfde881c1585fccd4f9d942cf90fd77e9b023158))
* add email allowlist for registration ([20c99a5](https://github.com/clawvisor/clawvisor/commit/20c99a516fd9dc366e64eb65bb10e372ab1535ad))
* add email allowlist for registration ([cd2739b](https://github.com/clawvisor/clawvisor/commit/cd2739befe490f35ef023b47366123120ff0e7c3))
* add end-to-end installer tests with Docker isolation ([#61](https://github.com/clawvisor/clawvisor/issues/61)) ([93eff6a](https://github.com/clawvisor/clawvisor/commit/93eff6a085e2755a7de14be33cec5f8e58787217))
* add extraction eval suite, chain context intent evals, and eval results doc ([461dffd](https://github.com/clawvisor/clawvisor/commit/461dffd8edcb99ec1fab8854d54a992af897e7cd))
* add free Haiku proxy quick-start option to setup wizards ([#63](https://github.com/clawvisor/clawvisor/issues/63)) ([b04a9e1](https://github.com/clawvisor/clawvisor/commit/b04a9e1d4653bc9aae20b9adfa36d2e5b91acd9b))
* add guard check endpoint for Claude Code permission hooks ([c8a6c84](https://github.com/clawvisor/clawvisor/commit/c8a6c841fc4ce06bebf6323bf585ca6fa03c4e06))
* add light mode with CSS variable theming ([9988547](https://github.com/clawvisor/clawvisor/commit/998854772fcd4729fc398ab9d9f57153c57e1665))
* add LLM-powered task risk assessment ([3c352d2](https://github.com/clawvisor/clawvisor/commit/3c352d23968ae7c0c2625ff4a600ecc42f8ca835))
* add long-poll support to get_task endpoint ([#10](https://github.com/clawvisor/clawvisor/issues/10)) ([c06003c](https://github.com/clawvisor/clawvisor/commit/c06003c058b3cf3a0b0bf56f034f2cd2790638d7))
* add MCP endpoint with OAuth 2.1 for Cowork plugin integration ([350e15d](https://github.com/clawvisor/clawvisor/commit/350e15d3dea439132efe2603c6472d1c738bb578))
* add openclaw-setup CLI, healthcheck, and deployment scaffolding ([2d0cb64](https://github.com/clawvisor/clawvisor/commit/2d0cb64a58e76f876537449bdb4354aa9e63e3d3))
* add opt-in anonymous telemetry to track product usage ([#16](https://github.com/clawvisor/clawvisor/issues/16)) ([800c157](https://github.com/clawvisor/clawvisor/commit/800c1576e78dfa36b3e7c5848e306862cc8e6c8b))
* add OWASP ZAP security scanning and fix HSTS on deployed instances ([a84f901](https://github.com/clawvisor/clawvisor/commit/a84f901e577a163332fe3fbb94cee7c987b3d26b))
* add passkey, TOTP, and email verification support for cloud auth ([1a776fe](https://github.com/clawvisor/clawvisor/commit/1a776fec0f2305e89db11a7500a8178da9a66d36))
* add reason tag sanitization, conditional chain context, and injection eval cases ([844f175](https://github.com/clawvisor/clawvisor/commit/844f175793cdae7a6a0798f71cd6a05940c410fa))
* add release binary build workflow with manual trigger ([fba3a62](https://github.com/clawvisor/clawvisor/commit/fba3a62682ff860e6f815e19f2c110dd7ef0bcb2))
* add SSE event stream for instant dashboard updates ([132cad4](https://github.com/clawvisor/clawvisor/commit/132cad403b97c14caa5909ef3690cf84902a066e))
* add task risk UI — badge, panel, and per-level styling ([7ad6f20](https://github.com/clawvisor/clawvisor/commit/7ad6f2078ca0a76d50a855cb3af7001465dd6275))
* add version check and update badge to dashboard and TUI ([#14](https://github.com/clawvisor/clawvisor/issues/14)) ([c3aa26b](https://github.com/clawvisor/clawvisor/commit/c3aa26b6c56ac23d18ca25297f9866f6c946a8ce))
* auto-register daemon with relay on startup ([#64](https://github.com/clawvisor/clawvisor/issues/64)) ([5763617](https://github.com/clawvisor/clawvisor/commit/57636174e3c5aa26b79d2d5529a0eb12e7a37b71))
* automate Claude Desktop MCP setup in installer ([#67](https://github.com/clawvisor/clawvisor/issues/67)) ([4d9b30a](https://github.com/clawvisor/clawvisor/commit/4d9b30aa278839f57f6028b43e88aa83614757e4))
* change default port from 8080 to 25297 (CLAWS on phone keypad) ([9a67e69](https://github.com/clawvisor/clawvisor/commit/9a67e6922b2e83cd715e6aa7b1dcc3e27de05584))
* deduplicate identical task creation requests within a time window ([#85](https://github.com/clawvisor/clawvisor/issues/85)) ([cc5d3d5](https://github.com/clawvisor/clawvisor/commit/cc5d3d5c34b8f6b4fbfd3f08d9ac6f76952dd9ba))
* deduplicate SKILL.md into a shared template and add Cowork plugin ([#88](https://github.com/clawvisor/clawvisor/issues/88)) ([febc9f2](https://github.com/clawvisor/clawvisor/commit/febc9f2e163d06766b19169a841d95427ad9d5e9))
* display task risk assessment in TUI ([e2bfd77](https://github.com/clawvisor/clawvisor/commit/e2bfd771a4f2291e065b9dcbb041ac99d568dcf4))
* expr-lang integration for YAML adapters — eliminate 8 Go overrides ([#93](https://github.com/clawvisor/clawvisor/issues/93)) ([1598ec4](https://github.com/clawvisor/clawvisor/commit/1598ec4d9e37c8349bc6061caf858c9214fb8709))
* implement Phase 1 - Foundation & Infrastructure ([b4d9e40](https://github.com/clawvisor/clawvisor/commit/b4d9e40968545aa89950bd9befbe78846383b98c))
* implement Phase 2 - Policy Engine ([56fdc70](https://github.com/clawvisor/clawvisor/commit/56fdc70c43d9f285d20442c1614295635de6678a))
* implement Phase 3 - Core Gateway, Gmail Adapter & Approval Flow ([0b3a19a](https://github.com/clawvisor/clawvisor/commit/0b3a19ae8e8a80b866f4ccdbe6504d17ac8561af))
* implement Phase 4 - Dashboard (Frontend) + new backend routes ([3cc7ab4](https://github.com/clawvisor/clawvisor/commit/3cc7ab42f9b057d166b07cbcc2d1740d34c2c962))
* Phase 4 addenda — LLM integration, policy authoring, Anthropic support ([f02c8e8](https://github.com/clawvisor/clawvisor/commit/f02c8e89fa6815f9c35752cdd9ebeec1a738be80))
* Phase 5 — Clawvisor OpenClaw skill ([954dcd0](https://github.com/clawvisor/clawvisor/commit/954dcd0cecb347c7bf8fc278b2c9fd9d6ef22527))
* Phase 6 — extended adapters, OAuth popup fix, auth stability ([a98d7e2](https://github.com/clawvisor/clawvisor/commit/a98d7e2a466e6f0a75639226eaca1bd82c8f948a))
* poll-then-execute approval flow for gateway requests ([#83](https://github.com/clawvisor/clawvisor/issues/83)) ([b303343](https://github.com/clawvisor/clawvisor/commit/b30334373eb6a15db760a3bb31bba87da78f028f))
* prompt to open dashboard after install ([#54](https://github.com/clawvisor/clawvisor/issues/54)) ([f6160f7](https://github.com/clawvisor/clawvisor/commit/f6160f73702646228b082b11e78aeec79ddba4a1))
* remove TUI Pending screen and add SSE real-time updates ([d630c96](https://github.com/clawvisor/clawvisor/commit/d630c964fe28e1a37eeb4b7106bcd91100d9cc78))
* request_id dedup, status endpoint, HMAC-signed callbacks ([2f43c29](https://github.com/clawvisor/clawvisor/commit/2f43c2986785b727ed259280740ef1b18b7ece88))
* require confirmation to approve high/critical risk tasks ([3998247](https://github.com/clawvisor/clawvisor/commit/399824714ac75e1138e3623e1f985f3356e61fbc))
* require confirmation to approve high/critical risk tasks in TUI ([cf747e2](https://github.com/clawvisor/clawvisor/commit/cf747e2a303884e359e71d276d791390904a37d9))
* require task_id on gateway requests, pass expansion rationale to intent verification ([bb8e579](https://github.com/clawvisor/clawvisor/commit/bb8e57961b7583733326716038badd288b8ae0f1))
* return 404 for unregistered /api/ routes instead of serving SPA ([bdfbdef](https://github.com/clawvisor/clawvisor/commit/bdfbdef0b75eb24fd41356bae267a4a70f4265fc))
* rework installer flow with welcome, agent detection, and setup links ([#60](https://github.com/clawvisor/clawvisor/issues/60)) ([25919bd](https://github.com/clawvisor/clawvisor/commit/25919bdcc634c090083464401573d777fb82da60))
* run service setup wizard during `clawvisor install` ([#41](https://github.com/clawvisor/clawvisor/issues/41)) ([99df13d](https://github.com/clawvisor/clawvisor/commit/99df13d74da7de8affa1d6bc269d3e4219efa053))
* show completed tasks on dashboard for 60 seconds before removing ([#68](https://github.com/clawvisor/clawvisor/issues/68)) ([0c5707b](https://github.com/clawvisor/clawvisor/commit/0c5707b521e4eef929163bfeabeec26bd64f3fe0))
* split setup into composable subcommands (services, integrate) ([#71](https://github.com/clawvisor/clawvisor/issues/71)) ([fbcdd75](https://github.com/clawvisor/clawvisor/commit/fbcdd75a2c701dcf11a8ce0c702b6856622b03f2))
* support VAULT_KEY env var for vault master key injection ([4775ab8](https://github.com/clawvisor/clawvisor/commit/4775ab824717d3f270b70c473c04f39f755bacbe))
* update setup wizard for shared LLM config and task risk toggle ([6627dfd](https://github.com/clawvisor/clawvisor/commit/6627dfd0dea2b5aa74b3bbeea9f6eaaf62982a93))
* warn when CLI and daemon versions differ ([#40](https://github.com/clawvisor/clawvisor/issues/40)) ([415ff6b](https://github.com/clawvisor/clawvisor/commit/415ff6b05f628842ac42cef9c08502f4eb5e5b0b))
* wire auth_mode config to PasswordAuth feature flag ([c90ebeb](https://github.com/clawvisor/clawvisor/commit/c90ebeba313b6e03a9e4f65e38b3e6bf4a67fc8c))
* YAML-driven adapter definitions with vault-backed OAuth ([#87](https://github.com/clawvisor/clawvisor/issues/87)) ([f5bd31b](https://github.com/clawvisor/clawvisor/commit/f5bd31b6f04db8c17f7399762f23ee6766cb1a24))


### Bug Fixes

* ad-hoc codesign binary on macOS, move PATH hint to end ([#52](https://github.com/clawvisor/clawvisor/issues/52)) ([2ae3baa](https://github.com/clawvisor/clawvisor/commit/2ae3baae9f63ec2a639bd978ff9c70f00b193124))
* add missing CSP directives (frame-ancestors, form-action) ([f223602](https://github.com/clawvisor/clawvisor/commit/f223602710615a04fd36865661b544c1545107f6))
* address security review findings for MCP + OAuth ([9383048](https://github.com/clawvisor/clawvisor/commit/9383048fe72118f1d4ddfd375a7308973ecd8782))
* allow custom app URI schemes (e.g. claude://) in OAuth redirect validation ([43c3b36](https://github.com/clawvisor/clawvisor/commit/43c3b368469f92fc6c0fa8192c0c6a1e2324ae20))
* allow data: URIs in CSP img-src for TOTP QR codes ([0833ad6](https://github.com/clawvisor/clawvisor/commit/0833ad61f3bde08300ebb4955280fa85f08a0352))
* apply docker setup fixes from testing branch ([0d18528](https://github.com/clawvisor/clawvisor/commit/0d1852801d692dd435c55d4e772c3545484f9787))
* build release binaries inline and add make install ([#48](https://github.com/clawvisor/clawvisor/issues/48)) ([c5bf42d](https://github.com/clawvisor/clawvisor/commit/c5bf42d42b2e2b38b507b3dfc7cec97ae46e9c1e))
* build release binaries inline and add make install ([#50](https://github.com/clawvisor/clawvisor/issues/50)) ([69ac925](https://github.com/clawvisor/clawvisor/commit/69ac9254d9eeda39adba97a8dec4aed0f6528b69))
* check current directory first in setup guide repo detection ([9479fc8](https://github.com/clawvisor/clawvisor/commit/9479fc8bcef4d8b2d1f5c66940ae05688bfa64de))
* close SSE channels on unsub, redact bot token from API responses ([bbb30d4](https://github.com/clawvisor/clawvisor/commit/bbb30d47646ac1863e9c19208cd6f2549e315e7b))
* conflict detector should not flag cross-role rule pairs as opposing_decisions ([140188e](https://github.com/clawvisor/clawvisor/commit/140188e8a386d977a33cad681b17f5dc23e88aa4))
* correct cd directory name in skill README ([fedb499](https://github.com/clawvisor/clawvisor/commit/fedb499d9427b5729c556a8e6b474cb0b68f2a3f))
* correct cd directory name in skill README ([0a4940f](https://github.com/clawvisor/clawvisor/commit/0a4940f2ee1caa86756861a32b5ca89f4b4100c4))
* correct OAuth callback URL in Google OAuth setup docs ([#58](https://github.com/clawvisor/clawvisor/issues/58)) ([1f06d8f](https://github.com/clawvisor/clawvisor/commit/1f06d8fa3ecb383abc7b3930f0ea511dc43124a4))
* correct restart command in update message ([#94](https://github.com/clawvisor/clawvisor/issues/94)) ([ffca0ef](https://github.com/clawvisor/clawvisor/commit/ffca0ef6660156085845dd3a015c8588fe64e645))
* correct stale README references to match current codebase ([27b4164](https://github.com/clawvisor/clawvisor/commit/27b4164f7b66abad5622bb277f2e3ffca58f825e))
* create GitHub release before uploading binaries ([#89](https://github.com/clawvisor/clawvisor/issues/89)) ([3ffa925](https://github.com/clawvisor/clawvisor/commit/3ffa9259b37f65fbe8715116640b76b1f186fc44))
* deduplicate paired devices by device_token during pairing ([#78](https://github.com/clawvisor/clawvisor/issues/78)) ([3d8d361](https://github.com/clawvisor/clawvisor/commit/3d8d361bc07eacfbf6a77b2c0b897e5f051b938d))
* derive queue page data from overview cache instead of independent polling ([76af380](https://github.com/clawvisor/clawvisor/commit/76af380b65395b354c9465f681fb47f23b82ba60))
* drop legacy registerHttpHandler fallback ([1cc745d](https://github.com/clawvisor/clawvisor/commit/1cc745dbec89a401f5c74c2253051de965515a6a))
* emit generic agent setup prompt when no agents detected ([#92](https://github.com/clawvisor/clawvisor/issues/92)) ([1050d8c](https://github.com/clawvisor/clawvisor/commit/1050d8c78a3b78351881e878c4451a24537e2520))
* fall back to default relay URL when config omits it ([#73](https://github.com/clawvisor/clawvisor/issues/73)) ([84014c2](https://github.com/clawvisor/clawvisor/commit/84014c2f364116f74c439012e6fba9e2cb202589))
* generate vault.key during make setup for local backend ([6535edd](https://github.com/clawvisor/clawvisor/commit/6535edd46f287ff5f99314aa1f348cfdb0c0683a))
* harden auth, sessions, SSRF, vault key, and SSE token handling ([70c91ca](https://github.com/clawvisor/clawvisor/commit/70c91cacc30a6c54fbf8a06a9d1bdd51fa5b8e2a))
* harden IsLocal, rate-limit keying, callback init, and HMAC replay ([#96](https://github.com/clawvisor/clawvisor/issues/96)) ([276c8da](https://github.com/clawvisor/clawvisor/commit/276c8da3eccf1246e4953c1ef53659d3f2bd08aa))
* hide LLM config editing in cloud deployments ([#75](https://github.com/clawvisor/clawvisor/issues/75)) ([a3fc893](https://github.com/clawvisor/clawvisor/commit/a3fc893645bca855cf8f01bf9d6b36f5717dc6bd))
* hide TUI password input and propagate GCP vault iterator errors ([5fb9605](https://github.com/clawvisor/clawvisor/commit/5fb960572f3dde7aec86104c4a9be76a9e008ef7))
* improve iMessage adapter reliability and contact resolution ([#80](https://github.com/clawvisor/clawvisor/issues/80)) ([6b88290](https://github.com/clawvisor/clawvisor/commit/6b882909ad1ca546a3d7ca5920f18cfaff5ad485))
* improve setup script clarity and add skill.zip endpoint ([#76](https://github.com/clawvisor/clawvisor/issues/76)) ([59fef4f](https://github.com/clawvisor/clawvisor/commit/59fef4f7ba105bb9f4d6d37b8dd7b1a67f34824c))
* include current date in intent verification prompt ([072a4a2](https://github.com/clawvisor/clawvisor/commit/072a4a272441380f858b40095160c57beac83b10))
* inline curl examples in SKILL template to avoid multi-approval ([#95](https://github.com/clawvisor/clawvisor/issues/95)) ([9e6195d](https://github.com/clawvisor/clawvisor/commit/9e6195d8eefbf0cc7fa2a8973133012a133b26fa))
* live-refresh expanded audit entries and structured scope display on Overview ([20c30af](https://github.com/clawvisor/clawvisor/commit/20c30afe7353cceaa9b3ab4f00de0b75d1272670))
* log audit entry for out-of-scope gateway requests ([2e554fa](https://github.com/clawvisor/clawvisor/commit/2e554fa7a7192bc00c5af53e4d96207f812e498d))
* move verification panel above params in ApprovalCard and remove redundant icons ([34f8d33](https://github.com/clawvisor/clawvisor/commit/34f8d332423e7ac6630c259dccd53c78d61b45fa))
* pass bundle_id to push service for correct APNs topic ([#55](https://github.com/clawvisor/clawvisor/issues/55)) ([f5fa243](https://github.com/clawvisor/clawvisor/commit/f5fa2431316ff24f89eb771632994deb805683ff))
* passkey/TOTP auth flows — correct auth_mode and add passkey login ([09d323e](https://github.com/clawvisor/clawvisor/commit/09d323e451194a0c0d5fd9b45779ba48e104f94b))
* persist skill and env vars globally for session restarts ([#97](https://github.com/clawvisor/clawvisor/issues/97)) ([c2812b4](https://github.com/clawvisor/clawvisor/commit/c2812b4f22ad6b8e4c35e222d12e67f09749405b))
* prefer exact service match over base service in CheckTaskScope ([#90](https://github.com/clawvisor/clawvisor/issues/90)) ([510703b](https://github.com/clawvisor/clawvisor/commit/510703bc166b8c5aeb0cb3d854352b6c411816f3))
* prevent double-close panic on SSE channel during shutdown ([f5c52c3](https://github.com/clawvisor/clawvisor/commit/f5c52c381920db8df270e326e2834a5bd7af8342))
* prevent stuck "Loading..." when pressing enter with no audit entries ([81c5048](https://github.com/clawvisor/clawvisor/commit/81c5048787ad418145a49074945dbfe80aa591a6))
* remove backend GET /oauth/authorize to prevent redirect loop ([f08ff3e](https://github.com/clawvisor/clawvisor/commit/f08ff3ea15d95b8ce6a79172861bdcc8588393c6))
* remove hardcoded adapter metadata and fix Drive OAuth execution ([#91](https://github.com/clawvisor/clawvisor/issues/91)) ([2b34294](https://github.com/clawvisor/clawvisor/commit/2b342943b5e8ff2b6e64911338457b5be5872371))
* remove unnecessary polling for queue and LLM status endpoints ([#74](https://github.com/clawvisor/clawvisor/issues/74)) ([dd2752c](https://github.com/clawvisor/clawvisor/commit/dd2752c4c703913059b4e126ada2df1ab11f3642))
* request offline access and force consent for Google OAuth ([a416cec](https://github.com/clawvisor/clawvisor/commit/a416cec336bc686b518fc1e285c415274704e578))
* return JSON content type for E2E middleware error responses ([#79](https://github.com/clawvisor/clawvisor/issues/79)) ([08c3f46](https://github.com/clawvisor/clawvisor/commit/08c3f46b65d4b47282e866bcdfa30fc9fea3ceb4))
* return proper JSON error for dashboard token endpoint ([#38](https://github.com/clawvisor/clawvisor/issues/38)) ([906407f](https://github.com/clawvisor/clawvisor/commit/906407f245398f015712704ab9066fe47d13be8c))
* return WWW-Authenticate header on MCP 401 for OAuth discovery ([a198595](https://github.com/clawvisor/clawvisor/commit/a1985955703237032095384c5254c20c53e2f73b))
* set auth to "plugin" for webhook HTTP route registration ([170c938](https://github.com/clawvisor/clawvisor/commit/170c93816e9dca5d2b4046aa308f146f9cc879b6))
* show "clawvisor update" in update banner instead of raw go install command ([#70](https://github.com/clawvisor/clawvisor/issues/70)) ([38fc580](https://github.com/clawvisor/clawvisor/commit/38fc580c51308ad01f29eeee4cc70979d968e942))
* show collapsible risk panel for all task states including pending ([0d983c0](https://github.com/clawvisor/clawvisor/commit/0d983c02315e0776ac7f66a7c538a9249f8c2c49))
* show completion message after OAuth authorization ([33fae58](https://github.com/clawvisor/clawvisor/commit/33fae5852ac89c327ba6fe9724b8e32160acd071))
* show redirecting state before close message on OAuth consent ([3d56a21](https://github.com/clawvisor/clawvisor/commit/3d56a21acaa8d5654c1a3f1ece15fa5e14d76e87))
* show risk level in badge labels and display risk panel for all levels ([6b1c0ab](https://github.com/clawvisor/clawvisor/commit/6b1c0ab17a662e04999e1ad961e91f7da5b2239a))
* simplify OAuth consent post-redirect UX ([f85a578](https://github.com/clawvisor/clawvisor/commit/f85a578464574fea727ca759ffc574be52bff0d7))
* support Slack thread replies and adapter-scoped verification hints ([b04c807](https://github.com/clawvisor/clawvisor/commit/b04c807425ebe5b5181d146f406b8e18d79a9a9b))
* switch dashboard activity graph from smoothed area chart to stacked bar chart ([ea09e43](https://github.com/clawvisor/clawvisor/commit/ea09e43aa0b17aa76a8938ae485154d55967a0fe))
* thread MagicTokenStore interface through API layer, add magic-link tests ([cdbd9c8](https://github.com/clawvisor/clawvisor/commit/cdbd9c8c90cb295f88db9d8559c62fce42d28289))
* trigger release binaries on tag push instead of release event ([#44](https://github.com/clawvisor/clawvisor/issues/44)) ([2ed64e0](https://github.com/clawvisor/clawvisor/commit/2ed64e075ed8843b884fc36fd4ba2aacd6b68fd2))
* trigger release binaries on tag push instead of release event ([#46](https://github.com/clawvisor/clawvisor/issues/46)) ([00bcbc5](https://github.com/clawvisor/clawvisor/commit/00bcbc5d85fe697641d5c9e945ccfd562c934c5d))
* update policy registry on PUT when YAML id field changes ([ea5547f](https://github.com/clawvisor/clawvisor/commit/ea5547fd8d2cb56fad465c6e11b4fd25b2839aae))
* update repo URL from ericlevine/clawvisor-gatekeeper to clawvisor/clawvisor ([b5d6a16](https://github.com/clawvisor/clawvisor/commit/b5d6a16131bd06204a2ab9c8ed26482662b3a119))
* update repo URLs from old path to clawvisor/clawvisor ([a6160f7](https://github.com/clawvisor/clawvisor/commit/a6160f7b56e6d8490d38dfd5ac478a370b24fbb3))
* update test-phase3.sh with correct API shapes (YAML policies, bare arrays, 204 logout) ([604b995](https://github.com/clawvisor/clawvisor/commit/604b995cbbf270606f87944aa9626d3bd4a0b40e))
* use 5-minute buckets for activity chart with full hour coverage ([ac1594f](https://github.com/clawvisor/clawvisor/commit/ac1594f7fd76188916853a5d5c6fc9b160e95912))
* use human-readable action names in dashboard UI ([71c6fb5](https://github.com/clawvisor/clawvisor/commit/71c6fb5fbd573812c8a103a38d69e35a9863f99c))
* use object signature for registerHttpRoute SDK call ([d761fae](https://github.com/clawvisor/clawvisor/commit/d761faed176570a63bd9e396cace67539d7eda0e))
* use optional chaining on plugin config properties ([2eef749](https://github.com/clawvisor/clawvisor/commit/2eef749e044a98e6e26da71135a21a00f66bc4f7))
* use plain language in risk assessment output and relax eval case ([3865a8d](https://github.com/clawvisor/clawvisor/commit/3865a8df26f72e43ec1bc459126a6b41c86beb8a))
* use role name (not UUID) as AgentRoleID in dry-run evaluate endpoint ([0ef435d](https://github.com/clawvisor/clawvisor/commit/0ef435dacefe572ad81532dca92996bb7773d5e0))
* use standard ClawHub metadata format and update skill files to 0.6.1 ([1c5567d](https://github.com/clawvisor/clawvisor/commit/1c5567d6fd4c60c81b0e7f2f4a4b5f850fe71579))
* use standard ClawHub metadata format for required env vars ([c9908ab](https://github.com/clawvisor/clawvisor/commit/c9908ab66bb8990b9209dfefde3169ab444863fa))
* use VAULT_KEY env var with dev default in docker-compose ([3d4388f](https://github.com/clawvisor/clawvisor/commit/3d4388fede2e878e0eb8a25ff872e2e1cc50dd54))
* use workspace/.env for OpenClaw environment variables ([b530ae8](https://github.com/clawvisor/clawvisor/commit/b530ae86088f8e1009773a58a5b97bff7fb8da26))
* validate OAuth redirect_uri scheme and enforce MCP session IDs ([99f728b](https://github.com/clawvisor/clawvisor/commit/99f728b3bd0fe6e43cf112015877e7a32d107270))
* vault alias bugs, add service validation at task creation ([83c66ff](https://github.com/clawvisor/clawvisor/commit/83c66ff062f34f874e7fbb878560981a57faefef))
* vault key via env var, cleanup orphan users, purpose tokens ([951dd2d](https://github.com/clawvisor/clawvisor/commit/951dd2d48772759138364d0aefa99a2af2029495))
* webhook plugin idempotency, task callback IDs, configurable WS URL, and SDK migration ([ab5f1cc](https://github.com/clawvisor/clawvisor/commit/ab5f1cc9cdc57ed07fef89eaa5bab700f4f7f3c0))
* webhook plugin idempotency, task callbacks, configurable WS URL, and SDK migration ([fea2a7e](https://github.com/clawvisor/clawvisor/commit/fea2a7e1a45273e249d9e4bee8917b2620d2eed2))
* widen activity reason text and add hover tooltip ([d85c73c](https://github.com/clawvisor/clawvisor/commit/d85c73c299c6fd3f9fd8c19c39154e15e2dd0a38))

## [0.7.11](https://github.com/clawvisor/clawvisor/compare/v0.7.10...v0.7.11) (2026-03-29)


### Bug Fixes

* improve iMessage adapter reliability and contact resolution ([#80](https://github.com/clawvisor/clawvisor/issues/80)) ([6b88290](https://github.com/clawvisor/clawvisor/commit/6b882909ad1ca546a3d7ca5920f18cfaff5ad485))

## [0.7.10](https://github.com/clawvisor/clawvisor/compare/v0.7.9...v0.7.10) (2026-03-29)


### Bug Fixes

* deduplicate paired devices by device_token during pairing ([#78](https://github.com/clawvisor/clawvisor/issues/78)) ([3d8d361](https://github.com/clawvisor/clawvisor/commit/3d8d361bc07eacfbf6a77b2c0b897e5f051b938d))
* hide LLM config editing in cloud deployments ([#75](https://github.com/clawvisor/clawvisor/issues/75)) ([a3fc893](https://github.com/clawvisor/clawvisor/commit/a3fc893645bca855cf8f01bf9d6b36f5717dc6bd))
* improve setup script clarity and add skill.zip endpoint ([#76](https://github.com/clawvisor/clawvisor/issues/76)) ([59fef4f](https://github.com/clawvisor/clawvisor/commit/59fef4f7ba105bb9f4d6d37b8dd7b1a67f34824c))
* return JSON content type for E2E middleware error responses ([#79](https://github.com/clawvisor/clawvisor/issues/79)) ([08c3f46](https://github.com/clawvisor/clawvisor/commit/08c3f46b65d4b47282e866bcdfa30fc9fea3ceb4))

## [0.7.9](https://github.com/clawvisor/clawvisor/compare/v0.7.8...v0.7.9) (2026-03-27)


### Features

* split setup into composable subcommands (services, integrate) ([#71](https://github.com/clawvisor/clawvisor/issues/71)) ([fbcdd75](https://github.com/clawvisor/clawvisor/commit/fbcdd75a2c701dcf11a8ce0c702b6856622b03f2))


### Bug Fixes

* fall back to default relay URL when config omits it ([#73](https://github.com/clawvisor/clawvisor/issues/73)) ([84014c2](https://github.com/clawvisor/clawvisor/commit/84014c2f364116f74c439012e6fba9e2cb202589))
* remove unnecessary polling for queue and LLM status endpoints ([#74](https://github.com/clawvisor/clawvisor/issues/74)) ([dd2752c](https://github.com/clawvisor/clawvisor/commit/dd2752c4c703913059b4e126ada2df1ab11f3642))

## [0.7.8](https://github.com/clawvisor/clawvisor/compare/v0.7.7...v0.7.8) (2026-03-27)


### Features

* add OWASP ZAP security scanning and fix HSTS on deployed instances ([a84f901](https://github.com/clawvisor/clawvisor/commit/a84f901e577a163332fe3fbb94cee7c987b3d26b))


### Bug Fixes

* add missing CSP directives (frame-ancestors, form-action) ([f223602](https://github.com/clawvisor/clawvisor/commit/f223602710615a04fd36865661b544c1545107f6))
* show "clawvisor update" in update banner instead of raw go install command ([#70](https://github.com/clawvisor/clawvisor/issues/70)) ([38fc580](https://github.com/clawvisor/clawvisor/commit/38fc580c51308ad01f29eeee4cc70979d968e942))

## [0.7.7](https://github.com/clawvisor/clawvisor/compare/v0.7.6...v0.7.7) (2026-03-27)


### Features

* add curl auto-approve, smoke test, and setup cleanup ([#65](https://github.com/clawvisor/clawvisor/issues/65)) ([bfde881](https://github.com/clawvisor/clawvisor/commit/bfde881c1585fccd4f9d942cf90fd77e9b023158))
* automate Claude Desktop MCP setup in installer ([#67](https://github.com/clawvisor/clawvisor/issues/67)) ([4d9b30a](https://github.com/clawvisor/clawvisor/commit/4d9b30aa278839f57f6028b43e88aa83614757e4))
* show completed tasks on dashboard for 60 seconds before removing ([#68](https://github.com/clawvisor/clawvisor/issues/68)) ([0c5707b](https://github.com/clawvisor/clawvisor/commit/0c5707b521e4eef929163bfeabeec26bd64f3fe0))

## [0.7.6](https://github.com/clawvisor/clawvisor/compare/v0.7.5...v0.7.6) (2026-03-26)


### Features

* add `make docker` to run clawvisor with ~/.clawvisor mounted ([#57](https://github.com/clawvisor/clawvisor/issues/57)) ([b26dcc4](https://github.com/clawvisor/clawvisor/commit/b26dcc409e09d1c4f9ac2e8f9c6206a1041b32da))
* add build-time staging environment support ([#62](https://github.com/clawvisor/clawvisor/issues/62)) ([5b8d1ad](https://github.com/clawvisor/clawvisor/commit/5b8d1ad452c08b0ee8db42bb127ca7a4b42e354e))
* add end-to-end installer tests with Docker isolation ([#61](https://github.com/clawvisor/clawvisor/issues/61)) ([93eff6a](https://github.com/clawvisor/clawvisor/commit/93eff6a085e2755a7de14be33cec5f8e58787217))
* add free Haiku proxy quick-start option to setup wizards ([#63](https://github.com/clawvisor/clawvisor/issues/63)) ([b04a9e1](https://github.com/clawvisor/clawvisor/commit/b04a9e1d4653bc9aae20b9adfa36d2e5b91acd9b))
* auto-register daemon with relay on startup ([#64](https://github.com/clawvisor/clawvisor/issues/64)) ([5763617](https://github.com/clawvisor/clawvisor/commit/57636174e3c5aa26b79d2d5529a0eb12e7a37b71))
* rework installer flow with welcome, agent detection, and setup links ([#60](https://github.com/clawvisor/clawvisor/issues/60)) ([25919bd](https://github.com/clawvisor/clawvisor/commit/25919bdcc634c090083464401573d777fb82da60))


### Bug Fixes

* correct OAuth callback URL in Google OAuth setup docs ([#58](https://github.com/clawvisor/clawvisor/issues/58)) ([1f06d8f](https://github.com/clawvisor/clawvisor/commit/1f06d8fa3ecb383abc7b3930f0ea511dc43124a4))

## [0.7.5](https://github.com/clawvisor/clawvisor/compare/v0.7.4...v0.7.5) (2026-03-26)


### Features

* prompt to open dashboard after install ([#54](https://github.com/clawvisor/clawvisor/issues/54)) ([f6160f7](https://github.com/clawvisor/clawvisor/commit/f6160f73702646228b082b11e78aeec79ddba4a1))


### Bug Fixes

* pass bundle_id to push service for correct APNs topic ([#55](https://github.com/clawvisor/clawvisor/issues/55)) ([f5fa243](https://github.com/clawvisor/clawvisor/commit/f5fa2431316ff24f89eb771632994deb805683ff))

## [0.7.4](https://github.com/clawvisor/clawvisor/compare/v0.7.3...v0.7.4) (2026-03-25)


### Bug Fixes

* ad-hoc codesign binary on macOS, move PATH hint to end ([#52](https://github.com/clawvisor/clawvisor/issues/52)) ([2ae3baa](https://github.com/clawvisor/clawvisor/commit/2ae3baae9f63ec2a639bd978ff9c70f00b193124))

## [0.7.3](https://github.com/clawvisor/clawvisor/compare/v0.7.2...v0.7.3) (2026-03-25)


### Bug Fixes

* build release binaries inline and add make install ([#50](https://github.com/clawvisor/clawvisor/issues/50)) ([69ac925](https://github.com/clawvisor/clawvisor/commit/69ac9254d9eeda39adba97a8dec4aed0f6528b69))

## [0.7.2](https://github.com/clawvisor/clawvisor/compare/v0.7.1...v0.7.2) (2026-03-25)


### Bug Fixes

* build release binaries inline and add make install ([#48](https://github.com/clawvisor/clawvisor/issues/48)) ([c5bf42d](https://github.com/clawvisor/clawvisor/commit/c5bf42d42b2e2b38b507b3dfc7cec97ae46e9c1e))

## [0.7.1](https://github.com/clawvisor/clawvisor/compare/v0.7.0...v0.7.1) (2026-03-25)


### Bug Fixes

* trigger release binaries on tag push instead of release event ([#46](https://github.com/clawvisor/clawvisor/issues/46)) ([00bcbc5](https://github.com/clawvisor/clawvisor/commit/00bcbc5d85fe697641d5c9e945ccfd562c934c5d))

## [0.7.0](https://github.com/clawvisor/clawvisor/compare/v0.6.2...v0.7.0) (2026-03-25)


### ⚠ BREAKING CHANGES

* return 404 for unregistered /api/ routes instead of serving SPA

### Features

* add --open flag to server command to auto-open magic link in browser ([#12](https://github.com/clawvisor/clawvisor/issues/12)) ([8ca56f4](https://github.com/clawvisor/clawvisor/commit/8ca56f44d404967cda8e244849018fc1cc383d83))
* add 84 intent verification eval cases and chain context to setup wizard ([98a1a98](https://github.com/clawvisor/clawvisor/commit/98a1a98ab96eb47a9949b080b9578c0cf72912a3))
* add agent-driven setup guide and env var pass-through ([3c72cde](https://github.com/clawvisor/clawvisor/commit/3c72cdea691354e2cc6afae7fc44a7b70d080d99))
* add chain context verification for multi-step task safety ([51b1fa3](https://github.com/clawvisor/clawvisor/commit/51b1fa380c1af951ab10da12c7970ee925cd39f7))
* add email allowlist for registration ([20c99a5](https://github.com/clawvisor/clawvisor/commit/20c99a516fd9dc366e64eb65bb10e372ab1535ad))
* add email allowlist for registration ([cd2739b](https://github.com/clawvisor/clawvisor/commit/cd2739befe490f35ef023b47366123120ff0e7c3))
* add extraction eval suite, chain context intent evals, and eval results doc ([461dffd](https://github.com/clawvisor/clawvisor/commit/461dffd8edcb99ec1fab8854d54a992af897e7cd))
* add guard check endpoint for Claude Code permission hooks ([c8a6c84](https://github.com/clawvisor/clawvisor/commit/c8a6c841fc4ce06bebf6323bf585ca6fa03c4e06))
* add light mode with CSS variable theming ([9988547](https://github.com/clawvisor/clawvisor/commit/998854772fcd4729fc398ab9d9f57153c57e1665))
* add LLM-powered task risk assessment ([3c352d2](https://github.com/clawvisor/clawvisor/commit/3c352d23968ae7c0c2625ff4a600ecc42f8ca835))
* add long-poll support to get_task endpoint ([#10](https://github.com/clawvisor/clawvisor/issues/10)) ([c06003c](https://github.com/clawvisor/clawvisor/commit/c06003c058b3cf3a0b0bf56f034f2cd2790638d7))
* add MCP endpoint with OAuth 2.1 for Cowork plugin integration ([350e15d](https://github.com/clawvisor/clawvisor/commit/350e15d3dea439132efe2603c6472d1c738bb578))
* add openclaw-setup CLI, healthcheck, and deployment scaffolding ([2d0cb64](https://github.com/clawvisor/clawvisor/commit/2d0cb64a58e76f876537449bdb4354aa9e63e3d3))
* add opt-in anonymous telemetry to track product usage ([#16](https://github.com/clawvisor/clawvisor/issues/16)) ([800c157](https://github.com/clawvisor/clawvisor/commit/800c1576e78dfa36b3e7c5848e306862cc8e6c8b))
* add passkey, TOTP, and email verification support for cloud auth ([1a776fe](https://github.com/clawvisor/clawvisor/commit/1a776fec0f2305e89db11a7500a8178da9a66d36))
* add reason tag sanitization, conditional chain context, and injection eval cases ([844f175](https://github.com/clawvisor/clawvisor/commit/844f175793cdae7a6a0798f71cd6a05940c410fa))
* add release binary build workflow with manual trigger ([fba3a62](https://github.com/clawvisor/clawvisor/commit/fba3a62682ff860e6f815e19f2c110dd7ef0bcb2))
* add SSE event stream for instant dashboard updates ([132cad4](https://github.com/clawvisor/clawvisor/commit/132cad403b97c14caa5909ef3690cf84902a066e))
* add task risk UI — badge, panel, and per-level styling ([7ad6f20](https://github.com/clawvisor/clawvisor/commit/7ad6f2078ca0a76d50a855cb3af7001465dd6275))
* add version check and update badge to dashboard and TUI ([#14](https://github.com/clawvisor/clawvisor/issues/14)) ([c3aa26b](https://github.com/clawvisor/clawvisor/commit/c3aa26b6c56ac23d18ca25297f9866f6c946a8ce))
* change default port from 8080 to 25297 (CLAWS on phone keypad) ([9a67e69](https://github.com/clawvisor/clawvisor/commit/9a67e6922b2e83cd715e6aa7b1dcc3e27de05584))
* display task risk assessment in TUI ([e2bfd77](https://github.com/clawvisor/clawvisor/commit/e2bfd771a4f2291e065b9dcbb041ac99d568dcf4))
* implement Phase 1 - Foundation & Infrastructure ([b4d9e40](https://github.com/clawvisor/clawvisor/commit/b4d9e40968545aa89950bd9befbe78846383b98c))
* implement Phase 2 - Policy Engine ([56fdc70](https://github.com/clawvisor/clawvisor/commit/56fdc70c43d9f285d20442c1614295635de6678a))
* implement Phase 3 - Core Gateway, Gmail Adapter & Approval Flow ([0b3a19a](https://github.com/clawvisor/clawvisor/commit/0b3a19ae8e8a80b866f4ccdbe6504d17ac8561af))
* implement Phase 4 - Dashboard (Frontend) + new backend routes ([3cc7ab4](https://github.com/clawvisor/clawvisor/commit/3cc7ab42f9b057d166b07cbcc2d1740d34c2c962))
* Phase 4 addenda — LLM integration, policy authoring, Anthropic support ([f02c8e8](https://github.com/clawvisor/clawvisor/commit/f02c8e89fa6815f9c35752cdd9ebeec1a738be80))
* Phase 5 — Clawvisor OpenClaw skill ([954dcd0](https://github.com/clawvisor/clawvisor/commit/954dcd0cecb347c7bf8fc278b2c9fd9d6ef22527))
* Phase 6 — extended adapters, OAuth popup fix, auth stability ([a98d7e2](https://github.com/clawvisor/clawvisor/commit/a98d7e2a466e6f0a75639226eaca1bd82c8f948a))
* remove TUI Pending screen and add SSE real-time updates ([d630c96](https://github.com/clawvisor/clawvisor/commit/d630c964fe28e1a37eeb4b7106bcd91100d9cc78))
* request_id dedup, status endpoint, HMAC-signed callbacks ([2f43c29](https://github.com/clawvisor/clawvisor/commit/2f43c2986785b727ed259280740ef1b18b7ece88))
* require confirmation to approve high/critical risk tasks ([3998247](https://github.com/clawvisor/clawvisor/commit/399824714ac75e1138e3623e1f985f3356e61fbc))
* require confirmation to approve high/critical risk tasks in TUI ([cf747e2](https://github.com/clawvisor/clawvisor/commit/cf747e2a303884e359e71d276d791390904a37d9))
* require task_id on gateway requests, pass expansion rationale to intent verification ([bb8e579](https://github.com/clawvisor/clawvisor/commit/bb8e57961b7583733326716038badd288b8ae0f1))
* return 404 for unregistered /api/ routes instead of serving SPA ([bdfbdef](https://github.com/clawvisor/clawvisor/commit/bdfbdef0b75eb24fd41356bae267a4a70f4265fc))
* run service setup wizard during `clawvisor install` ([#41](https://github.com/clawvisor/clawvisor/issues/41)) ([99df13d](https://github.com/clawvisor/clawvisor/commit/99df13d74da7de8affa1d6bc269d3e4219efa053))
* support VAULT_KEY env var for vault master key injection ([4775ab8](https://github.com/clawvisor/clawvisor/commit/4775ab824717d3f270b70c473c04f39f755bacbe))
* update setup wizard for shared LLM config and task risk toggle ([6627dfd](https://github.com/clawvisor/clawvisor/commit/6627dfd0dea2b5aa74b3bbeea9f6eaaf62982a93))
* warn when CLI and daemon versions differ ([#40](https://github.com/clawvisor/clawvisor/issues/40)) ([415ff6b](https://github.com/clawvisor/clawvisor/commit/415ff6b05f628842ac42cef9c08502f4eb5e5b0b))
* wire auth_mode config to PasswordAuth feature flag ([c90ebeb](https://github.com/clawvisor/clawvisor/commit/c90ebeba313b6e03a9e4f65e38b3e6bf4a67fc8c))


### Bug Fixes

* address security review findings for MCP + OAuth ([9383048](https://github.com/clawvisor/clawvisor/commit/9383048fe72118f1d4ddfd375a7308973ecd8782))
* allow custom app URI schemes (e.g. claude://) in OAuth redirect validation ([43c3b36](https://github.com/clawvisor/clawvisor/commit/43c3b368469f92fc6c0fa8192c0c6a1e2324ae20))
* allow data: URIs in CSP img-src for TOTP QR codes ([0833ad6](https://github.com/clawvisor/clawvisor/commit/0833ad61f3bde08300ebb4955280fa85f08a0352))
* apply docker setup fixes from testing branch ([0d18528](https://github.com/clawvisor/clawvisor/commit/0d1852801d692dd435c55d4e772c3545484f9787))
* check current directory first in setup guide repo detection ([9479fc8](https://github.com/clawvisor/clawvisor/commit/9479fc8bcef4d8b2d1f5c66940ae05688bfa64de))
* close SSE channels on unsub, redact bot token from API responses ([bbb30d4](https://github.com/clawvisor/clawvisor/commit/bbb30d47646ac1863e9c19208cd6f2549e315e7b))
* conflict detector should not flag cross-role rule pairs as opposing_decisions ([140188e](https://github.com/clawvisor/clawvisor/commit/140188e8a386d977a33cad681b17f5dc23e88aa4))
* correct cd directory name in skill README ([fedb499](https://github.com/clawvisor/clawvisor/commit/fedb499d9427b5729c556a8e6b474cb0b68f2a3f))
* correct cd directory name in skill README ([0a4940f](https://github.com/clawvisor/clawvisor/commit/0a4940f2ee1caa86756861a32b5ca89f4b4100c4))
* correct stale README references to match current codebase ([27b4164](https://github.com/clawvisor/clawvisor/commit/27b4164f7b66abad5622bb277f2e3ffca58f825e))
* derive queue page data from overview cache instead of independent polling ([76af380](https://github.com/clawvisor/clawvisor/commit/76af380b65395b354c9465f681fb47f23b82ba60))
* drop legacy registerHttpHandler fallback ([1cc745d](https://github.com/clawvisor/clawvisor/commit/1cc745dbec89a401f5c74c2253051de965515a6a))
* generate vault.key during make setup for local backend ([6535edd](https://github.com/clawvisor/clawvisor/commit/6535edd46f287ff5f99314aa1f348cfdb0c0683a))
* harden auth, sessions, SSRF, vault key, and SSE token handling ([70c91ca](https://github.com/clawvisor/clawvisor/commit/70c91cacc30a6c54fbf8a06a9d1bdd51fa5b8e2a))
* hide TUI password input and propagate GCP vault iterator errors ([5fb9605](https://github.com/clawvisor/clawvisor/commit/5fb960572f3dde7aec86104c4a9be76a9e008ef7))
* include current date in intent verification prompt ([072a4a2](https://github.com/clawvisor/clawvisor/commit/072a4a272441380f858b40095160c57beac83b10))
* live-refresh expanded audit entries and structured scope display on Overview ([20c30af](https://github.com/clawvisor/clawvisor/commit/20c30afe7353cceaa9b3ab4f00de0b75d1272670))
* log audit entry for out-of-scope gateway requests ([2e554fa](https://github.com/clawvisor/clawvisor/commit/2e554fa7a7192bc00c5af53e4d96207f812e498d))
* move verification panel above params in ApprovalCard and remove redundant icons ([34f8d33](https://github.com/clawvisor/clawvisor/commit/34f8d332423e7ac6630c259dccd53c78d61b45fa))
* passkey/TOTP auth flows — correct auth_mode and add passkey login ([09d323e](https://github.com/clawvisor/clawvisor/commit/09d323e451194a0c0d5fd9b45779ba48e104f94b))
* prevent double-close panic on SSE channel during shutdown ([f5c52c3](https://github.com/clawvisor/clawvisor/commit/f5c52c381920db8df270e326e2834a5bd7af8342))
* prevent stuck "Loading..." when pressing enter with no audit entries ([81c5048](https://github.com/clawvisor/clawvisor/commit/81c5048787ad418145a49074945dbfe80aa591a6))
* remove backend GET /oauth/authorize to prevent redirect loop ([f08ff3e](https://github.com/clawvisor/clawvisor/commit/f08ff3ea15d95b8ce6a79172861bdcc8588393c6))
* request offline access and force consent for Google OAuth ([a416cec](https://github.com/clawvisor/clawvisor/commit/a416cec336bc686b518fc1e285c415274704e578))
* return proper JSON error for dashboard token endpoint ([#38](https://github.com/clawvisor/clawvisor/issues/38)) ([906407f](https://github.com/clawvisor/clawvisor/commit/906407f245398f015712704ab9066fe47d13be8c))
* return WWW-Authenticate header on MCP 401 for OAuth discovery ([a198595](https://github.com/clawvisor/clawvisor/commit/a1985955703237032095384c5254c20c53e2f73b))
* set auth to "plugin" for webhook HTTP route registration ([170c938](https://github.com/clawvisor/clawvisor/commit/170c93816e9dca5d2b4046aa308f146f9cc879b6))
* show collapsible risk panel for all task states including pending ([0d983c0](https://github.com/clawvisor/clawvisor/commit/0d983c02315e0776ac7f66a7c538a9249f8c2c49))
* show completion message after OAuth authorization ([33fae58](https://github.com/clawvisor/clawvisor/commit/33fae5852ac89c327ba6fe9724b8e32160acd071))
* show redirecting state before close message on OAuth consent ([3d56a21](https://github.com/clawvisor/clawvisor/commit/3d56a21acaa8d5654c1a3f1ece15fa5e14d76e87))
* show risk level in badge labels and display risk panel for all levels ([6b1c0ab](https://github.com/clawvisor/clawvisor/commit/6b1c0ab17a662e04999e1ad961e91f7da5b2239a))
* simplify OAuth consent post-redirect UX ([f85a578](https://github.com/clawvisor/clawvisor/commit/f85a578464574fea727ca759ffc574be52bff0d7))
* support Slack thread replies and adapter-scoped verification hints ([b04c807](https://github.com/clawvisor/clawvisor/commit/b04c807425ebe5b5181d146f406b8e18d79a9a9b))
* switch dashboard activity graph from smoothed area chart to stacked bar chart ([ea09e43](https://github.com/clawvisor/clawvisor/commit/ea09e43aa0b17aa76a8938ae485154d55967a0fe))
* thread MagicTokenStore interface through API layer, add magic-link tests ([cdbd9c8](https://github.com/clawvisor/clawvisor/commit/cdbd9c8c90cb295f88db9d8559c62fce42d28289))
* trigger release binaries on tag push instead of release event ([#44](https://github.com/clawvisor/clawvisor/issues/44)) ([2ed64e0](https://github.com/clawvisor/clawvisor/commit/2ed64e075ed8843b884fc36fd4ba2aacd6b68fd2))
* update policy registry on PUT when YAML id field changes ([ea5547f](https://github.com/clawvisor/clawvisor/commit/ea5547fd8d2cb56fad465c6e11b4fd25b2839aae))
* update repo URL from ericlevine/clawvisor-gatekeeper to clawvisor/clawvisor ([b5d6a16](https://github.com/clawvisor/clawvisor/commit/b5d6a16131bd06204a2ab9c8ed26482662b3a119))
* update repo URLs from old path to clawvisor/clawvisor ([a6160f7](https://github.com/clawvisor/clawvisor/commit/a6160f7b56e6d8490d38dfd5ac478a370b24fbb3))
* update test-phase3.sh with correct API shapes (YAML policies, bare arrays, 204 logout) ([604b995](https://github.com/clawvisor/clawvisor/commit/604b995cbbf270606f87944aa9626d3bd4a0b40e))
* use 5-minute buckets for activity chart with full hour coverage ([ac1594f](https://github.com/clawvisor/clawvisor/commit/ac1594f7fd76188916853a5d5c6fc9b160e95912))
* use human-readable action names in dashboard UI ([71c6fb5](https://github.com/clawvisor/clawvisor/commit/71c6fb5fbd573812c8a103a38d69e35a9863f99c))
* use object signature for registerHttpRoute SDK call ([d761fae](https://github.com/clawvisor/clawvisor/commit/d761faed176570a63bd9e396cace67539d7eda0e))
* use optional chaining on plugin config properties ([2eef749](https://github.com/clawvisor/clawvisor/commit/2eef749e044a98e6e26da71135a21a00f66bc4f7))
* use plain language in risk assessment output and relax eval case ([3865a8d](https://github.com/clawvisor/clawvisor/commit/3865a8df26f72e43ec1bc459126a6b41c86beb8a))
* use role name (not UUID) as AgentRoleID in dry-run evaluate endpoint ([0ef435d](https://github.com/clawvisor/clawvisor/commit/0ef435dacefe572ad81532dca92996bb7773d5e0))
* use standard ClawHub metadata format and update skill files to 0.6.1 ([1c5567d](https://github.com/clawvisor/clawvisor/commit/1c5567d6fd4c60c81b0e7f2f4a4b5f850fe71579))
* use standard ClawHub metadata format for required env vars ([c9908ab](https://github.com/clawvisor/clawvisor/commit/c9908ab66bb8990b9209dfefde3169ab444863fa))
* use VAULT_KEY env var with dev default in docker-compose ([3d4388f](https://github.com/clawvisor/clawvisor/commit/3d4388fede2e878e0eb8a25ff872e2e1cc50dd54))
* use workspace/.env for OpenClaw environment variables ([b530ae8](https://github.com/clawvisor/clawvisor/commit/b530ae86088f8e1009773a58a5b97bff7fb8da26))
* validate OAuth redirect_uri scheme and enforce MCP session IDs ([99f728b](https://github.com/clawvisor/clawvisor/commit/99f728b3bd0fe6e43cf112015877e7a32d107270))
* vault alias bugs, add service validation at task creation ([83c66ff](https://github.com/clawvisor/clawvisor/commit/83c66ff062f34f874e7fbb878560981a57faefef))
* vault key via env var, cleanup orphan users, purpose tokens ([951dd2d](https://github.com/clawvisor/clawvisor/commit/951dd2d48772759138364d0aefa99a2af2029495))
* webhook plugin idempotency, task callback IDs, configurable WS URL, and SDK migration ([ab5f1cc](https://github.com/clawvisor/clawvisor/commit/ab5f1cc9cdc57ed07fef89eaa5bab700f4f7f3c0))
* webhook plugin idempotency, task callbacks, configurable WS URL, and SDK migration ([fea2a7e](https://github.com/clawvisor/clawvisor/commit/fea2a7e1a45273e249d9e4bee8917b2620d2eed2))
* widen activity reason text and add hover tooltip ([d85c73c](https://github.com/clawvisor/clawvisor/commit/d85c73c299c6fd3f9fd8c19c39154e15e2dd0a38))

## [0.6.2](https://github.com/clawvisor/clawvisor/compare/v0.6.1...v0.6.2) (2026-03-25)


### Features

* run service setup wizard during `clawvisor install` ([#41](https://github.com/clawvisor/clawvisor/issues/41)) ([99df13d](https://github.com/clawvisor/clawvisor/commit/99df13d74da7de8affa1d6bc269d3e4219efa053))

## [0.6.1](https://github.com/clawvisor/clawvisor/compare/v0.6.0...v0.6.1) (2026-03-24)


### Features

* warn when CLI and daemon versions differ ([#40](https://github.com/clawvisor/clawvisor/issues/40)) ([415ff6b](https://github.com/clawvisor/clawvisor/commit/415ff6b05f628842ac42cef9c08502f4eb5e5b0b))


### Bug Fixes

* return proper JSON error for dashboard token endpoint ([#38](https://github.com/clawvisor/clawvisor/issues/38)) ([906407f](https://github.com/clawvisor/clawvisor/commit/906407f245398f015712704ab9066fe47d13be8c))

## [0.6.0](https://github.com/clawvisor/clawvisor/compare/v0.5.2...v0.6.0) (2026-03-24)


### ⚠ BREAKING CHANGES

* return 404 for unregistered /api/ routes instead of serving SPA

### Features

* add release binary build workflow with manual trigger ([fba3a62](https://github.com/clawvisor/clawvisor/commit/fba3a62682ff860e6f815e19f2c110dd7ef0bcb2))
* return 404 for unregistered /api/ routes instead of serving SPA ([bdfbdef](https://github.com/clawvisor/clawvisor/commit/bdfbdef0b75eb24fd41356bae267a4a70f4265fc))

## [0.5.2](https://github.com/clawvisor/clawvisor/compare/v0.5.1...v0.5.2) (2026-03-16)


### Features

* add opt-in anonymous telemetry to track product usage ([#16](https://github.com/clawvisor/clawvisor/issues/16)) ([800c157](https://github.com/clawvisor/clawvisor/commit/800c1576e78dfa36b3e7c5848e306862cc8e6c8b))
* add version check and update badge to dashboard and TUI ([#14](https://github.com/clawvisor/clawvisor/issues/14)) ([c3aa26b](https://github.com/clawvisor/clawvisor/commit/c3aa26b6c56ac23d18ca25297f9866f6c946a8ce))

## [0.5.1](https://github.com/clawvisor/clawvisor/compare/v0.5.0...v0.5.1) (2026-03-16)


### Features

* add --open flag to server command to auto-open magic link in browser ([#12](https://github.com/clawvisor/clawvisor/issues/12)) ([8ca56f4](https://github.com/clawvisor/clawvisor/commit/8ca56f44d404967cda8e244849018fc1cc383d83))
* add long-poll support to get_task endpoint ([#10](https://github.com/clawvisor/clawvisor/issues/10)) ([c06003c](https://github.com/clawvisor/clawvisor/commit/c06003c058b3cf3a0b0bf56f034f2cd2790638d7))
* add reason tag sanitization, conditional chain context, and injection eval cases ([844f175](https://github.com/clawvisor/clawvisor/commit/844f175793cdae7a6a0798f71cd6a05940c410fa))


### Bug Fixes

* use standard ClawHub metadata format and update skill files to 0.6.1 ([1c5567d](https://github.com/clawvisor/clawvisor/commit/1c5567d6fd4c60c81b0e7f2f4a4b5f850fe71579))
* use standard ClawHub metadata format for required env vars ([c9908ab](https://github.com/clawvisor/clawvisor/commit/c9908ab66bb8990b9209dfefde3169ab444863fa))
