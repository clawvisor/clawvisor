# Changelog

## Unreleased

### Features

* **admin-visibility:** an instance `admin` gets a single-instance fleet view and an admin approval queue so a flat team can actually operate under Govern. New admin-only, `RequireAdminOrToken`-gated routes: `GET /api/admin/agents` (every user's agents incl. `_instance` Terraform/CI rows, with owner attribution), `GET /api/admin/approvals` + `POST /api/admin/approvals/{id}/resolve` (list and approve/deny ANY user's pending hold — the governed member is no longer the sole approver), `GET /api/admin/audit` (cross-user audit; departed users render via `actor_email`), and `GET /api/admin/costs?window=daily|monthly` (per-user/per-agent spend rollup incl. automation). Members keep their exact self-scoped views — no user-scoped store signature changed (new `ListAll*` / `InstanceCostSummary` siblings). Two config knobs land: `approval.admin_notify` (default true — also alert admins on hold creation) and `approval.allow_self_approve` (default true; the `govern`/`contain` posture presets flip it false, so no user — **including an admin (F7)** — may resolve a hold raised by their own agent; the one exception is a solo-admin instance, permitted but logged to the audit trail as a self-approval). The dashboard gains an admin-only "Fleet" section (gated on role). **Cloud (multi-org) note:** these `ListAll*` reads are org-blind by construction, so the `/api/admin/*` routes are **OSS-only** — they are not mounted when the cloud composition wires `WithOrgGov`; cloud must provide org-scoped equivalents at submodule bump.
* **flat-team:** admin/member user roles, an invite + user-management API (`/api/users`, `/api/users/invites`), and instance-shared vault entries (`/api/vault/shared`) so a flat team can share one Anthropic key. Invites are single-use, expire in 48h, delivered as a one-time `cvinv_` token/URL, and only ever produce a member over the enrollment channel; invite-claimed accounts are `pending_verification` until a magic-link confirm. The API-token scope split lands too: `config-read` / `config-write` / `instance-admin`, with token / user / shared-vault management gated on `instance-admin`. **BREAKING (OSS→cloud seam):** `store.Store.CreateUser` gains a `role` parameter (`CreateUser(ctx, email, passwordHash, role)`); the cloud submodule bump must update its call sites. A new `store.CreateInvitedUser` creates pending accounts. Deleting a user now SET-NULLs (no longer cascade-deletes) `audit_log.user_id`, and both `audit_log` and `llm_request_cost` carry a server-derived `actor_email`, so a departed user's audit/cost history survives (PRD §15.4). Provider follow-up (wave 5): the `clawvisor_user` resource creates via `POST /api/users/invites` and should expose a computed, sensitive `invite_url` (`<base>/join?token=cvinv_...`), returned once at mint.
* **installer:** wire invite delivery into the shell installers. The generated `claude-code`/`codex` scripts (default, skill-only, and claude-code's subscription variant) accept an enrollment invite via `--invite-stdin` or `--invite-file=PATH` (with `CLAWVISOR_INVITE` as a documented, argv-exposed CI fallback), read it off stdin/file — never argv, so it stays out of shell history and the process table — and POST it to the new unauthenticated `POST /api/agents/enroll`. The endpoint claims the invite (single-use, member-only, expiry preserved), confirms email possession inline (magic-link mode's "claim IS the magic-link" carve-out), registers a per-user agent under the new member, and returns its `cvis_` token in one round-trip. A subscription-seat install carrying an invite still never touches a provider key. Enrollment is only mounted in magic-link mode. Note: the invite must be delivered on a free stdin (`printf '%s' "<invite-url>" | sh -c "$(curl -fsSL …/skill/install/claude-code.sh)" -- --invite-stdin`), since `curl … | sh` already occupies stdin with the script body.
* **api-tokens:** long-lived, scoped, revocable API tokens (`cvat_` prefix) at `POST/GET/DELETE /api/tokens`, plus a first-boot bootstrap path via the new `CLAWVISOR_BOOTSTRAP_TOKEN` env var (single-use, 24h expiry, burned on first mint). `GET /api/features` now advertises `api_tokens: true`. **Cloud submodule-bump note:** a `cvat_` bearer passes through `proxy_lite_gate.go` untouched (it sniffs only `cvis_`/`cv-nonce-`) and falls through the per-user `proxy_lite_users` gate on `/api/vault/items`; instance-admin tokens are intended to bypass per-user proxy-lite gating, so this is acceptable but must be an explicit, reviewed decision at the bump.

## [0.9.10](https://github.com/clawvisor/clawvisor/compare/v0.9.9...v0.9.10) (2026-06-22)


### Features

* **autovault:** script sessions for credentialed fan-out workflows ([#506](https://github.com/clawvisor/clawvisor/issues/506)) ([04953fe](https://github.com/clawvisor/clawvisor/commit/04953feb00b2220de40de24f469b837c70e40e0d))
* **installer:** collapse Hermes/OpenClaw to one-paste auto-approve flow ([#554](https://github.com/clawvisor/clawvisor/issues/554)) ([8c2bf4a](https://github.com/clawvisor/clawvisor/commit/8c2bf4a60ea748bac967cef6b2cb3484325d2b19))
* **installer:** env-namespace agent token paths + fix rc-block idempotency ([#581](https://github.com/clawvisor/clawvisor/issues/581)) ([1c0981b](https://github.com/clawvisor/clawvisor/commit/1c0981b6b336cd63356531441afec4cc02efe034))
* **installer:** replace LLM-driven markdown installer with shell one-liner for claude-code/codex ([#580](https://github.com/clawvisor/clawvisor/issues/580)) ([845877b](https://github.com/clawvisor/clawvisor/commit/845877b15748f0a657fd4b951b767037181d1d4d))
* **lite-proxy:** one-paste install flow with passthrough-first smoke test ([#525](https://github.com/clawvisor/clawvisor/issues/525)) ([32db87d](https://github.com/clawvisor/clawvisor/commit/32db87d88d2d707d05ac1d95a483df3c20f694fe))
* **llmproxy:** batch intent verification across parallel tool_uses ([#535](https://github.com/clawvisor/clawvisor/issues/535)) ([90ec7ff](https://github.com/clawvisor/clawvisor/commit/90ec7ffcbac829f5c27dc7cb582be89decd406dd))
* **llmproxy:** discover existing tasks before creating new ones ([#538](https://github.com/clawvisor/clawvisor/issues/538)) ([efc63d5](https://github.com/clawvisor/clawvisor/commit/efc63d554353d00a19b22adeac7bd52644e5c33f))
* **llmproxy:** handle script-session llm judge timeouts gracefully ([#543](https://github.com/clawvisor/clawvisor/issues/543)) ([b453907](https://github.com/clawvisor/clawvisor/commit/b453907be6766b05dfd55f41fba7f7117ba6de83))
* **llmproxy:** inline approval via AskUserQuestion when available ([#545](https://github.com/clawvisor/clawvisor/issues/545)) ([9eb7f02](https://github.com/clawvisor/clawvisor/commit/9eb7f02d65bc7e10cfc642b5ccab7d3003806980))
* **llmproxy:** LLM judge for variable-ized script-session calls ([#529](https://github.com/clawvisor/clawvisor/issues/529)) ([4251d81](https://github.com/clawvisor/clawvisor/commit/4251d8149e30685354ceef8e3692522d5a6a616b))
* **llmproxy:** log upstream 4xx/5xx response bodies ([#519](https://github.com/clawvisor/clawvisor/issues/519)) ([ea83dfa](https://github.com/clawvisor/clawvisor/commit/ea83dfa55d2133d5aaee4824634ffda9d2ec4ee0))
* **llmproxy:** per-call cvreason on tool_use for intent verification ([#541](https://github.com/clawvisor/clawvisor/issues/541)) ([d9d203a](https://github.com/clawvisor/clawvisor/commit/d9d203aebbd044ca80115ea28fe6a93e07891c27))
* **llmproxy:** recoverable Deny verdicts give agents one retry per turn ([#544](https://github.com/clawvisor/clawvisor/issues/544)) ([e76f34e](https://github.com/clawvisor/clawvisor/commit/e76f34eb8e365f88cce60aab5b135faf10057797))
* **llmproxy:** scope-drift continuation menu ([#564](https://github.com/clawvisor/clawvisor/issues/564)) ([11cb05f](https://github.com/clawvisor/clawvisor/commit/11cb05f839b7829911beeff74104657f4e747a5e))
* **llmproxy:** slide session task expires_at on each authorized tool_use ([#505](https://github.com/clawvisor/clawvisor/issues/505)) ([ad5f307](https://github.com/clawvisor/clawvisor/commit/ad5f30791c962a7140b7240d0eece43a07fa7629))
* **llmproxy:** split control-plane notices by role — backticked [Clawvisor] for humans, &lt;clawvisor-notice&gt; tag for the LLM ([#504](https://github.com/clawvisor/clawvisor/issues/504)) ([d70e1c8](https://github.com/clawvisor/clawvisor/commit/d70e1c8988a2868d321cd007075a04be98a14443))
* **llmproxy:** transient-deny verdicts + recoverable parser shape refusals ([#579](https://github.com/clawvisor/clawvisor/issues/579)) ([0223b4d](https://github.com/clawvisor/clawvisor/commit/0223b4d139082c2af26a7b3e0278f84015301c51))
* **pricing:** add claude-opus-4-8 row ([#498](https://github.com/clawvisor/clawvisor/issues/498)) ([c986aa6](https://github.com/clawvisor/clawvisor/commit/c986aa6f5fa8cb4f316dde868548f4b4d38b7b68))
* **taskrisk:** envelope evals, strict-match mode, prompt anchors ([#546](https://github.com/clawvisor/clawvisor/issues/546)) ([132fe6b](https://github.com/clawvisor/clawvisor/commit/132fe6bf4ecf70900c4181bf23ac836438813eaa))
* **tasks:** envelope-shape scope expansion (foundation) ([#540](https://github.com/clawvisor/clawvisor/issues/540)) ([ffaa642](https://github.com/clawvisor/clawvisor/commit/ffaa6427932248f9095fd4d3eb5feae4d37e004a))
* **tasks:** expose POST /api/control/tasks/{id}/complete to LLM-proxy agents ([#575](https://github.com/clawvisor/clawvisor/issues/575)) ([a6317dd](https://github.com/clawvisor/clawvisor/commit/a6317ddf9bea2870b36cdec360344acc11d77a41))
* **web:** tell user to approve connection request in bootstrap curl ([#518](https://github.com/clawvisor/clawvisor/issues/518)) ([bc111ac](https://github.com/clawvisor/clawvisor/commit/bc111ac95f664d7fcb8f828300bd627ff0b8d5b8))


### Bug Fixes

* **autovault:** surface concrete example curl in script-session mint response ([#514](https://github.com/clawvisor/clawvisor/issues/514)) ([34c0da4](https://github.com/clawvisor/clawvisor/commit/34c0da4d01481207dbb08d8a22aef0c21f1408d7))
* **calendar:** accept location on create/update_event, q + larger pages + time_min/time_max aliases on list_events ([#547](https://github.com/clawvisor/clawvisor/issues/547)) ([485d56c](https://github.com/clawvisor/clawvisor/commit/485d56c6d0b4b1c13efcfadd86fbff33b702910a))
* **dev:** honor pre-set PUBLIC_URL / SERVER_HOST in dev.sh ([#555](https://github.com/clawvisor/clawvisor/issues/555)) ([eed9568](https://github.com/clawvisor/clawvisor/commit/eed9568d1f4ed228b67a0eb5f64a4ee863077fb0))
* **installer:** macOS-portable smoke tests + env-aware Codex provider slug ([#549](https://github.com/clawvisor/clawvisor/issues/549)) ([384445b](https://github.com/clawvisor/clawvisor/commit/384445b531097b5a94b8d1e288bac95fab6dbbae))
* **installer:** route registration to app host, not LLM proxy ([#531](https://github.com/clawvisor/clawvisor/issues/531)) ([10159c4](https://github.com/clawvisor/clawvisor/commit/10159c4fc37a1c24936d16000381fc2890f3f106))
* **intent:** stop logging parse failure on empty chain-context responses ([#571](https://github.com/clawvisor/clawvisor/issues/571)) ([e6bbca1](https://github.com/clawvisor/clawvisor/commit/e6bbca18fb987094834de3c463dbe83facdcd440))
* **llmproxy:** byte-faithful Anthropic request and response rewriting ([#511](https://github.com/clawvisor/clawvisor/issues/511)) ([d6f7d7f](https://github.com/clawvisor/clawvisor/commit/d6f7d7f0b54b567284ce80015cdf31bf9afb802f))
* **llmproxy:** consolidate drift claim rollback logic and handle auto-approve outcome failure ([#569](https://github.com/clawvisor/clawvisor/issues/569)) ([c21fee3](https://github.com/clawvisor/clawvisor/commit/c21fee3791e11064d0b9a74a8e7f6c149e8db919))
* **llmproxy:** cvreason Rewritten contract + abstraction cleanup ([#542](https://github.com/clawvisor/clawvisor/issues/542)) ([8b9c591](https://github.com/clawvisor/clawvisor/commit/8b9c591659e483e96b00940d236b063ffeff0206))
* **llmproxy:** don't hijack bare yes/no replies as approval_not_found ([#539](https://github.com/clawvisor/clawvisor/issues/539)) ([00e1486](https://github.com/clawvisor/clawvisor/commit/00e148600f0fc65cbe7397d433a9a0b784170bd1))
* **llmproxy:** embed approval marker in coalesced per-turn prompt ([#517](https://github.com/clawvisor/clawvisor/issues/517)) ([e4d20b4](https://github.com/clawvisor/clawvisor/commit/e4d20b40bab7c7305a932d7424fdb08e3a59335c))
* **llmproxy:** fire sliding task slide on EvaluateAuthorization VerdictAllow ([#520](https://github.com/clawvisor/clawvisor/issues/520)) ([fc2e3e6](https://github.com/clawvisor/clawvisor/commit/fc2e3e6b1988794091234c39d3dafd617972e623))
* **llmproxy:** isolate choice rewrites and support inline blocked tool substitutions ([#522](https://github.com/clawvisor/clawvisor/issues/522)) ([a3ed02a](https://github.com/clawvisor/clawvisor/commit/a3ed02ad75253c69718d45a7592809db414b034f))
* **llmproxy:** keep ?surface=inline when drift_id is unclaimable ([#582](https://github.com/clawvisor/clawvisor/issues/582)) ([0788caa](https://github.com/clawvisor/clawvisor/commit/0788caa9da6cee9d188be81ecdaa6e578cf47fd9))
* **llmproxy:** open trace log at 0600 to match rawlog sibling ([#561](https://github.com/clawvisor/clawvisor/issues/561)) ([bd347cf](https://github.com/clawvisor/clawvisor/commit/bd347cf25fb0aee79303f02fd5ed1952bbc2b092))
* **llmproxy:** pair text-shape inline-task notice with reconstructed tool_use + rewrite↔sanitize round-trip test ([#553](https://github.com/clawvisor/clawvisor/issues/553)) ([9ea0c81](https://github.com/clawvisor/clawvisor/commit/9ea0c81e43a75fc38c76083c1cf7905317586a2d))
* **llmproxy:** parallelize metadata fetching in listMessages to prevent timeouts ([#516](https://github.com/clawvisor/clawvisor/issues/516)) ([5928407](https://github.com/clawvisor/clawvisor/commit/5928407c3a474897031400e715f8fec5d7a52814))
* **llmproxy:** pass preferred task id during approval release ([#502](https://github.com/clawvisor/clawvisor/issues/502)) ([8319c2e](https://github.com/clawvisor/clawvisor/commit/8319c2e2a6945b6c2fde31accce27e4482ed65f4))
* **llmproxy:** persist script sessions and log resolver IO ([#527](https://github.com/clawvisor/clawvisor/issues/527)) ([5cab897](https://github.com/clawvisor/clawvisor/commit/5cab897e8f28d7d83266b33f47f7695c3bb0f95c))
* **llmproxy:** preserve empty `thinking` field on thinking_delta re-emit ([#510](https://github.com/clawvisor/clawvisor/issues/510)) ([2dde8f1](https://github.com/clawvisor/clawvisor/commit/2dde8f13551ffe3d3287e1d6cecc6a2eb4ce3817))
* **llmproxy:** preserve index 0 for thinking blocks in Anthropic SSE stream ([#507](https://github.com/clawvisor/clawvisor/issues/507)) ([9cd0b10](https://github.com/clawvisor/clawvisor/commit/9cd0b10542e45694036c02f9fbb1ee778b809287))
* **llmproxy:** preserve thinking block bytes in secret-rewrite path ([#534](https://github.com/clawvisor/clawvisor/issues/534)) ([352f33d](https://github.com/clawvisor/clawvisor/commit/352f33d539fc4df86d5366c46a5a33fec76c2344))
* **llmproxy:** require vault-item check before harness auth tools ([#532](https://github.com/clawvisor/clawvisor/issues/532)) ([ca6d38e](https://github.com/clawvisor/clawvisor/commit/ca6d38e96d362f34ec0cc1f519dc3678f59f43f9))
* **llmproxy:** treat deterministic parser refusals as agent-recoverable ([#557](https://github.com/clawvisor/clawvisor/issues/557)) ([4676735](https://github.com/clawvisor/clawvisor/commit/4676735e5d91e4d86dd1bb579155ee1dcf4269cb))
* **llmproxy:** treat tool_search and reasoning items as approval-reply staleness signals ([#515](https://github.com/clawvisor/clawvisor/issues/515)) ([44ac826](https://github.com/clawvisor/clawvisor/commit/44ac826d0297a5f962f0fcfd4f6cf97acceeb580))
* **raw-log-viewer:** align transcript diff via LCS instead of positional pairing ([#509](https://github.com/clawvisor/clawvisor/issues/509)) ([d5d1ced](https://github.com/clawvisor/clawvisor/commit/d5d1cedeed7293e19e7b05c7928a8be4b37d88e5))
* **runtime:** handle stream write errors and sanitize fallback SSE comments ([#523](https://github.com/clawvisor/clawvisor/issues/523)) ([f02f6d5](https://github.com/clawvisor/clawvisor/commit/f02f6d51c5d4004c5af9d207ed809f4014001d32))
* **tasks:** validate vault item handles at submission time ([#558](https://github.com/clawvisor/clawvisor/issues/558)) ([a8a1c11](https://github.com/clawvisor/clawvisor/commit/a8a1c1193eff3521ddd7cf5a3bbef136b608ce98))


### Performance Improvements

* **llmproxy:** kill prompt-cache busts around inline task creation ([#567](https://github.com/clawvisor/clawvisor/issues/567)) ([b80011a](https://github.com/clawvisor/clawvisor/commit/b80011ae76a1d5b458634714864e1f7f3f72daa7))

## [0.9.9](https://github.com/clawvisor/clawvisor/compare/v0.9.8...v0.9.9) (2026-06-02)


### Features

* add Google Sheets adapter with OAuth integration  ([#431](https://github.com/clawvisor/clawvisor/issues/431)) ([fa9a80f](https://github.com/clawvisor/clawvisor/commit/fa9a80f4b3e9767e12ae281b0e72276436fcaf27))
* **agents:** add GBrain and Cloud agent connect tiles ([#485](https://github.com/clawvisor/clawvisor/issues/485)) ([059a040](https://github.com/clawvisor/clawvisor/commit/059a040747dc60302badf4d358dae05bc1c07fe9))
* **agents:** improve connect agent wizard ([#476](https://github.com/clawvisor/clawvisor/issues/476)) ([45808e5](https://github.com/clawvisor/clawvisor/commit/45808e524d6d17eeda9a212b2fca88642eb56dc5))
* **cloudlogging:** coalesce log lines under request entries in GCP Logs Explorer ([#456](https://github.com/clawvisor/clawvisor/issues/456)) ([bdce746](https://github.com/clawvisor/clawvisor/commit/bdce746781fa96f0232cede77896c039fb1e148b))
* **drive:** export specific sheet tabs ([#452](https://github.com/clawvisor/clawvisor/issues/452)) ([6be2a8c](https://github.com/clawvisor/clawvisor/commit/6be2a8cd836f23558bcd7be04f83e53b111b2e4d))
* **installer:** enable lite-proxy by default in generated config ([#496](https://github.com/clawvisor/clawvisor/issues/496)) ([9457851](https://github.com/clawvisor/clawvisor/commit/9457851d54e4e0b7ec67b884ae362e1a5ddc91aa))
* **intent:** scale verification strictness with action blast radius ([#437](https://github.com/clawvisor/clawvisor/issues/437)) ([cc66401](https://github.com/clawvisor/clawvisor/commit/cc6640115dce1ee07906e9267cad019c90beb431))
* **lite-proxy:** selectively stream SSE responses while preserving tool mediation ([#482](https://github.com/clawvisor/clawvisor/issues/482)) ([b554d71](https://github.com/clawvisor/clawvisor/commit/b554d711162a1eec3a7499d2b82e7cd1cb48d227))
* **llmproxy:** clarify inline task approval prompt ([#466](https://github.com/clawvisor/clawvisor/issues/466)) ([c2e8b1a](https://github.com/clawvisor/clawvisor/commit/c2e8b1ac5be9b357bfb3721637ae042f0c3582a1))
* **llmproxy:** coalesce multi-tool turns into one approval ([#439](https://github.com/clawvisor/clawvisor/issues/439)) ([10d505a](https://github.com/clawvisor/clawvisor/commit/10d505acacffbe2fc466f176c4a22ab01dd1c09b))
* **llmproxy:** conversation-based auto-approval for inline tasks ([#442](https://github.com/clawvisor/clawvisor/issues/442)) ([513ffcd](https://github.com/clawvisor/clawvisor/commit/513ffcd23ac1f36612408faee3840b9363ee1d1b))
* **llmproxy:** distinguish passthrough-bearer-missing from vault miss ([#461](https://github.com/clawvisor/clawvisor/issues/461)) ([7dd806c](https://github.com/clawvisor/clawvisor/commit/7dd806c4635934025d28337a57c4aa112a5ba2a6))
* **llmproxy:** inject Clawvisor routing notice on first assistant turn ([#443](https://github.com/clawvisor/clawvisor/issues/443)) ([cb54a6e](https://github.com/clawvisor/clawvisor/commit/cb54a6ec5be8dcdee960dd717accfffabd5817dd))
* **llmproxy:** mint and inject Clawvisor conversation ID on Chat Completions turn 1 ([#444](https://github.com/clawvisor/clawvisor/issues/444)) ([55a041f](https://github.com/clawvisor/clawvisor/commit/55a041f4bce1e17bb6a5ac740c0d637e3f759cc5))
* **llmproxy:** per-task LLM cost tracking + dashboard ([#467](https://github.com/clawvisor/clawvisor/issues/467)) ([c0e73a8](https://github.com/clawvisor/clawvisor/commit/c0e73a87c4b8a15128067d050edac8763fdead4a))
* **llmproxy:** strengthen task-first prompt + add scenario eval harness ([#459](https://github.com/clawvisor/clawvisor/issues/459)) ([8dc9887](https://github.com/clawvisor/clawvisor/commit/8dc9887eb67c82d9438eb336573c394c434dc04c))
* **resolver:** refresh expired OAuth tokens before forwarding ([#492](https://github.com/clawvisor/clawvisor/issues/492)) ([9bc946d](https://github.com/clawvisor/clawvisor/commit/9bc946dff95c318e0a530b0841b511cacb625c96))
* **runtime:** guard sensitive file reads ([#475](https://github.com/clawvisor/clawvisor/issues/475)) ([85ea7c5](https://github.com/clawvisor/clawvisor/commit/85ea7c5090f547f08c99dc4d4cbfd649db467be0))
* **runtime:** surface inline-chat pending tasks via store.Task ([#472](https://github.com/clawvisor/clawvisor/issues/472)) ([9defcab](https://github.com/clawvisor/clawvisor/commit/9defcabe9cd794a426826162cdc7daf2dfe3c6dd))
* **taskrisk:** cache risk assessor prompt ([#450](https://github.com/clawvisor/clawvisor/issues/450)) ([5f9d79a](https://github.com/clawvisor/clawvisor/commit/5f9d79ae77f6fcf5b2d510b3424622ed5a6b42b7))
* **taskrisk:** scope risk to intent-allowed capability ([#438](https://github.com/clawvisor/clawvisor/issues/438)) ([6382f3f](https://github.com/clawvisor/clawvisor/commit/6382f3fc5adf5c3c837423049d505d0252a9ac9f))
* **web:** add How it works walkthrough and proxy-aware onboarding ([#490](https://github.com/clawvisor/clawvisor/issues/490)) ([01aa393](https://github.com/clawvisor/clawvisor/commit/01aa393bcec655dc821fd93a9f96c8355977d926))


### Bug Fixes

* **adaptergen:** isolate generated adapters per user ([#432](https://github.com/clawvisor/clawvisor/issues/432)) ([b46d77f](https://github.com/clawvisor/clawvisor/commit/b46d77f19ed4d0720d3a568b3332763ede624e1f))
* **api:** respect caller-supplied *slog.Logger via WithLogger option ([#460](https://github.com/clawvisor/clawvisor/issues/460)) ([ab4270d](https://github.com/clawvisor/clawvisor/commit/ab4270d5589779122c3a7b905e621da8cf1b718b))
* **autovault:** allowlist placeholder prefix chars to match inspector ([#489](https://github.com/clawvisor/clawvisor/issues/489)) ([bb64694](https://github.com/clawvisor/clawvisor/commit/bb64694e8c6c4f3b6104b57e04f48691d00b49de))
* **haikuproxy:** revert base URL prefix to /v1 ([#473](https://github.com/clawvisor/clawvisor/issues/473)) ([1f227ff](https://github.com/clawvisor/clawvisor/commit/1f227ff72a8d55b19cff1f7bd04dd264e9d05e20))
* **intent:** cache proxy-lite verifier prompt separately ([#447](https://github.com/clawvisor/clawvisor/issues/447)) ([0d32eeb](https://github.com/clawvisor/clawvisor/commit/0d32eeb03d860a288d28f5e26d0537eafbf444fb))
* **llmproxy:** accept `head -N` / `tail -N` bare-digit shorthand in read-only bash inspector ([#449](https://github.com/clawvisor/clawvisor/issues/449)) ([de197fa](https://github.com/clawvisor/clawvisor/commit/de197fa3046a242d9013fa6cf91418d492a95570))
* **llmproxy:** accept echo with any dashed argument ([#458](https://github.com/clawvisor/clawvisor/issues/458)) ([5a23dae](https://github.com/clawvisor/clawvisor/commit/5a23dae91b7db2b348872e357f2db7cabc4cabda))
* **llmproxy:** clarify task creation timing ([#448](https://github.com/clawvisor/clawvisor/issues/448)) ([47008a1](https://github.com/clawvisor/clawvisor/commit/47008a1fe8e5e8046e3c05bab687abb38c46187b))
* **llmproxy:** disambiguate one-off approvals via transcript marker ([#436](https://github.com/clawvisor/clawvisor/issues/436)) ([79889dc](https://github.com/clawvisor/clawvisor/commit/79889dc55d101ea69fe475eb99941d00bdff6f12))
* **llmproxy:** expire pending inline tasks on rollback ([#470](https://github.com/clawvisor/clawvisor/issues/470)) ([19c2063](https://github.com/clawvisor/clawvisor/commit/19c2063731b4a899488e5fa2d376770c3eb4842a))
* **llmproxy:** extend inline task approval hold lifetime ([#453](https://github.com/clawvisor/clawvisor/issues/453)) ([4806942](https://github.com/clawvisor/clawvisor/commit/4806942802de5b61dd399619880778e546e70929))
* **llmproxy:** extend sanitization to identifier fields and broaden unicode Cf blocklist ([#484](https://github.com/clawvisor/clawvisor/issues/484)) ([f56baae](https://github.com/clawvisor/clawvisor/commit/f56baae3e31210891948ba0d474fa06f295d0ff6))
* **llmproxy:** ignore stub-length autovault placeholders ([#474](https://github.com/clawvisor/clawvisor/issues/474)) ([5b423bb](https://github.com/clawvisor/clawvisor/commit/5b423bb581387358e0dbd76076828079013f1f98))
* **llmproxy:** preserve lite-proxy audit child rows ([#465](https://github.com/clawvisor/clawvisor/issues/465)) ([e99c92c](https://github.com/clawvisor/clawvisor/commit/e99c92c70b11bd5c401a798ac703b7f9411c969b))
* **llmproxy:** restore first-turn routing notice on streaming SSE ([#491](https://github.com/clawvisor/clawvisor/issues/491)) ([f3068f3](https://github.com/clawvisor/clawvisor/commit/f3068f3f2f7c25aa566abcebce14d7ebc2847059))
* **llmproxy:** return harness-shaped errors so the user can recover ([#451](https://github.com/clawvisor/clawvisor/issues/451)) ([ec5b9f1](https://github.com/clawvisor/clawvisor/commit/ec5b9f1598d97f474f0267654449d6951507c43a))
* **llmproxy:** reuse inline risk assessment on manual approval ([#468](https://github.com/clawvisor/clawvisor/issues/468)) ([0385694](https://github.com/clawvisor/clawvisor/commit/0385694f4d08d29325d2e5748f3bfcf606f5f525))
* **llmproxy:** sanitize user-supplied fields in task approval prompt ([#481](https://github.com/clawvisor/clawvisor/issues/481)) ([63f8bd0](https://github.com/clawvisor/clawvisor/commit/63f8bd0859d970f3c9329ed397ffbcf78d0a1e00))
* **llmproxy:** scope coalesced approval hold to conversation ([#441](https://github.com/clawvisor/clawvisor/issues/441)) ([f07322c](https://github.com/clawvisor/clawvisor/commit/f07322cf4442e3697503481802b7770fab09c829))
* **llmproxy:** stop re-firing approval release on stale OpenAI history ([#457](https://github.com/clawvisor/clawvisor/issues/457)) ([f93dc0d](https://github.com/clawvisor/clawvisor/commit/f93dc0dc8abac651b94b677743662d5e9de4e5db))
* **llmproxy:** stop treating Bash `description` as a per-call rationale ([#445](https://github.com/clawvisor/clawvisor/issues/445)) ([712d150](https://github.com/clawvisor/clawvisor/commit/712d15029a54d0ff0298118baa936c34a6bda1eb))
* **llmproxy:** surface recovery guidance for script rewrites ([#493](https://github.com/clawvisor/clawvisor/issues/493)) ([97878f3](https://github.com/clawvisor/clawvisor/commit/97878f3352b7dd6798774371397aa5cbf4d28a58))
* **llmproxy:** thread matched task_id into tool_use audit rows ([#463](https://github.com/clawvisor/clawvisor/issues/463)) ([a46712b](https://github.com/clawvisor/clawvisor/commit/a46712b834b255b4ed186485d243229d3821f9d4))
* **mcp:** support numeric JSON timeout in tools exec proxy ([#477](https://github.com/clawvisor/clawvisor/issues/477)) ([f7d7ea4](https://github.com/clawvisor/clawvisor/commit/f7d7ea41cc103b361bbe3b039b37333f935cb1ad))
* redact internal error from risk assessor and log Redis outcome failures ([#434](https://github.com/clawvisor/clawvisor/issues/434)) ([9886eb5](https://github.com/clawvisor/clawvisor/commit/9886eb55f7ba17d4876be5f00d4bccbcad382dfd))
* **web:** clarify inline-chat deep-link approvals ([#480](https://github.com/clawvisor/clawvisor/issues/480)) ([a4e4c6e](https://github.com/clawvisor/clawvisor/commit/a4e4c6e48abeaa269781b176dc4cb65a24683f2d))
* **web:** count proxy/runtime success outcomes as executed on dashboard ([#471](https://github.com/clawvisor/clawvisor/issues/471)) ([cd73540](https://github.com/clawvisor/clawvisor/commit/cd73540ebbfb08f32780ad54940d60a296775ccd))


### Reverts

* **llmproxy:** restore 10m TTL for inline approvals ([#454](https://github.com/clawvisor/clawvisor/issues/454)) ([e1983c8](https://github.com/clawvisor/clawvisor/commit/e1983c8fc89c6eb4c04925db8e73d403ca134605))

## [0.9.8](https://github.com/clawvisor/clawvisor/compare/v0.9.7...v0.9.8) (2026-05-21)


### Features

* **lite-proxy:** add experimental secret detection toggle ([#423](https://github.com/clawvisor/clawvisor/issues/423)) ([2289914](https://github.com/clawvisor/clawvisor/commit/228991486f9f119458b4ab2a97aa6ef7b79d12ae))
* **lite-proxy:** add task checkout focus ([#426](https://github.com/clawvisor/clawvisor/issues/426)) ([b056249](https://github.com/clawvisor/clawvisor/commit/b056249a9d264f50a8df58fd41b960c13b66e53a))
* **runtime:** speed up proxy-lite read-only tools ([#424](https://github.com/clawvisor/clawvisor/issues/424)) ([fb30e22](https://github.com/clawvisor/clawvisor/commit/fb30e22da03fc3b1247dec885e11209339b01963))
* **taskrisk:** run LLM risk assessor on v2 envelopes and inline tasks ([#427](https://github.com/clawvisor/clawvisor/issues/427)) ([135b458](https://github.com/clawvisor/clawvisor/commit/135b4589c5ce4c0fc34f0d172deececcee8ac28e))


### Bug Fixes

* **auth:** preserve session on transient refresh failures ([#410](https://github.com/clawvisor/clawvisor/issues/410)) ([bfa49a9](https://github.com/clawvisor/clawvisor/commit/bfa49a9797a0c8771e5cf2d5cfb669067715ed78))
* **lite-proxy:** move routes under api ([#420](https://github.com/clawvisor/clawvisor/issues/420)) ([15390ea](https://github.com/clawvisor/clawvisor/commit/15390eaf3181cd2f169ff3bed4ed02f89a9c2e33))
* **llmproxy:** default allow codex internal tools ([#422](https://github.com/clawvisor/clawvisor/issues/422)) ([fe39ca7](https://github.com/clawvisor/clawvisor/commit/fe39ca749d1b7311378338961fca7eaa5ce1af08))
* **llmproxy:** handle lite OpenAI chat approval routes ([#425](https://github.com/clawvisor/clawvisor/issues/425)) ([6f619de](https://github.com/clawvisor/clawvisor/commit/6f619de6e58d77d1a293dd536623d52860f90e5b))
* **llmproxy:** raise inbound body cap to 34 MiB ([#428](https://github.com/clawvisor/clawvisor/issues/428)) ([271a5c6](https://github.com/clawvisor/clawvisor/commit/271a5c65ca61224c2c3a80de03f344a4248a85a0))
* **runtime:** allow read-only agent tools ([#415](https://github.com/clawvisor/clawvisor/issues/415)) ([beaa94f](https://github.com/clawvisor/clawvisor/commit/beaa94fab6b461a0290703e4c5762ed50a8a6fc5))
* **runtime:** guide agents to aliased vault items ([#429](https://github.com/clawvisor/clawvisor/issues/429)) ([4501a85](https://github.com/clawvisor/clawvisor/commit/4501a8590c640bdab274039fe76e8f8c5a5c1cca))
* **security:** address code scanning findings ([#421](https://github.com/clawvisor/clawvisor/issues/421)) ([416d836](https://github.com/clawvisor/clawvisor/commit/416d8362d9c33848d2d8e9ec509ae13889275d3b))
* **yamlruntime:** propagate io.ReadAll and credentialFields errors ([#412](https://github.com/clawvisor/clawvisor/issues/412)) ([ffeee40](https://github.com/clawvisor/clawvisor/commit/ffeee40c9090a7a2ac038689d6277368bde353c2))

## [0.9.7](https://github.com/clawvisor/clawvisor/compare/v0.9.6...v0.9.7) (2026-05-18)


### Bug Fixes

* align install e2e mock with release assets ([#400](https://github.com/clawvisor/clawvisor/issues/400)) ([7896263](https://github.com/clawvisor/clawvisor/commit/7896263b89672be5fd7ec976156bc1a4d5cd35f8))
* **conversation:** accept yes/y/no/n as approval aliases ([#392](https://github.com/clawvisor/clawvisor/issues/392)) ([55660dc](https://github.com/clawvisor/clawvisor/commit/55660dc75c495aaa1e7a523a686955a9bf2d72a7))
* **intent:** enhance tag stripping to handle case variations ([#377](https://github.com/clawvisor/clawvisor/issues/377)) ([7001419](https://github.com/clawvisor/clawvisor/commit/7001419b98a2d32006a315c7f1537479b03239b3))
* **intent:** strip role tags from all agent-supplied fields ([#375](https://github.com/clawvisor/clawvisor/issues/375)) ([d3fd175](https://github.com/clawvisor/clawvisor/commit/d3fd1750dfbd26a161c024752201e334a5b88418))
* **server:** use PublicURL for magic link when set ([#380](https://github.com/clawvisor/clawvisor/issues/380)) ([9d728dd](https://github.com/clawvisor/clawvisor/commit/9d728dd3a668867ab32fca7f25444c988f08506c))
* **tui:** auto-detect local server from .local-session ([#383](https://github.com/clawvisor/clawvisor/issues/383)) ([2b67845](https://github.com/clawvisor/clawvisor/commit/2b678458d7ca2bdbcf4e02334a9f9df717f58eca))

## [0.9.6](https://github.com/clawvisor/clawvisor/compare/v0.9.5...v0.9.6) (2026-05-12)


### Features

* **mcp:** spec-driven MCP adapters with OAuth discovery + registration ([#373](https://github.com/clawvisor/clawvisor/issues/373)) ([2fe599e](https://github.com/clawvisor/clawvisor/commit/2fe599efeea53ab6fad18a57a2319ab5ea12e34d))
* symmetric (user, request_id, task_id) dedup scope ([#365](https://github.com/clawvisor/clawvisor/issues/365)) ([ec08db7](https://github.com/clawvisor/clawvisor/commit/ec08db73b441c7fdbee60733ef7349638c80a063))


### Bug Fixes

* **approvals:** skip canonical resolve on stranded-executor recovery ([#370](https://github.com/clawvisor/clawvisor/issues/370)) ([da0ee49](https://github.com/clawvisor/clawvisor/commit/da0ee4988d6c0073ab01988f4df607036c36735b))
* **intent:** always run builtin chain extraction so created IDs reach chain_facts ([#371](https://github.com/clawvisor/clawvisor/issues/371)) ([2f67095](https://github.com/clawvisor/clawvisor/commit/2f67095b6bfac9b82e3eac07aa95f4c43990e735))
* **intent:** clean builtin email extraction and add chain_extraction opt-in ([#369](https://github.com/clawvisor/clawvisor/issues/369)) ([20795bb](https://github.com/clawvisor/clawvisor/commit/20795bba195f3883f95241e2000f213ba94297d6))
* **mcp:** url-encode timeout param and add injection tests ([#366](https://github.com/clawvisor/clawvisor/issues/366)) ([b76d9f4](https://github.com/clawvisor/clawvisor/commit/b76d9f45772057769af929c4272a7c16345f879c))
* **services:** same-origin postMessage fallback restores dashboard auto-refresh ([#372](https://github.com/clawvisor/clawvisor/issues/372)) ([afb2763](https://github.com/clawvisor/clawvisor/commit/afb27630668689e1e6517a4a6e5a73585ee64f8f))

## [0.9.5](https://github.com/clawvisor/clawvisor/compare/v0.9.4...v0.9.5) (2026-05-11)


### Features

* Microsoft 365 integration (Outlook, OneDrive) with full OAuth2 support ([#344](https://github.com/clawvisor/clawvisor/issues/344)) ([87a4adf](https://github.com/clawvisor/clawvisor/commit/87a4adf678cd0263efe5bb6a1d6c49bcfb5f8343))


### Bug Fixes

* **linear:** map issue_id to  and add strict assignee filters ([#358](https://github.com/clawvisor/clawvisor/issues/358)) ([3b5616b](https://github.com/clawvisor/clawvisor/commit/3b5616b7120c3900cd8c2a2919496a5438011a87))
* **llm:** retry on Vertex Gemini cache-expired (400 INVALID_ARGUMENT) ([#360](https://github.com/clawvisor/clawvisor/issues/360)) ([ccf506e](https://github.com/clawvisor/clawvisor/commit/ccf506ebf7d830edc80c24059a845c0d994c9023))
* **mcp:** url-encode service param to prevent query string injection ([#363](https://github.com/clawvisor/clawvisor/issues/363)) ([d637dc0](https://github.com/clawvisor/clawvisor/commit/d637dc0667f2304da8489c21fc43dfd94bcee62a))

## [0.9.4](https://github.com/clawvisor/clawvisor/compare/v0.9.3...v0.9.4) (2026-05-07)


### Features

* **adapters:** SQL connect via pasted DSN + richer credential prompt ([#354](https://github.com/clawvisor/clawvisor/issues/354)) ([d057196](https://github.com/clawvisor/clawvisor/commit/d0571962440e7f818042bdb3238f74d3963a2c08))
* **api:** per-user FeaturesHook for plan-based feature gating ([#351](https://github.com/clawvisor/clawvisor/issues/351)) ([79c9fba](https://github.com/clawvisor/clawvisor/commit/79c9fba818651533622a1b6f2806ffc8c5414316))
* **imessage:** pin helper SHA in source so go install can install it ([#356](https://github.com/clawvisor/clawvisor/issues/356)) ([e795881](https://github.com/clawvisor/clawvisor/commit/e79588138b3af791cb01dc026a6b883586a97b69))
* **llm,config:** per-sub-block timeout + opt-in request hedging ([#355](https://github.com/clawvisor/clawvisor/issues/355)) ([a4bed58](https://github.com/clawvisor/clawvisor/commit/a4bed584b1c114d348a175c03032f02745270af1))

## [0.9.3](https://github.com/clawvisor/clawvisor/compare/v0.9.2...v0.9.3) (2026-05-07)


### Features

* **intent:** two-phase chain context extraction + chain_facts provenance ([#343](https://github.com/clawvisor/clawvisor/issues/343)) ([1c254e0](https://github.com/clawvisor/clawvisor/commit/1c254e0612e52a1365f1853cddf7b2dd8db5cc4b))
* **llm:** Anthropic prompt caching for verification, risk, and chain context ([#342](https://github.com/clawvisor/clawvisor/issues/342)) ([5d7dc5e](https://github.com/clawvisor/clawvisor/commit/5d7dc5e08b0ea49a771486e02f99e31e2519b1ae))
* **llm:** Gemini provider with explicit context caching ([#345](https://github.com/clawvisor/clawvisor/issues/345)) ([46bba30](https://github.com/clawvisor/clawvisor/commit/46bba30f79c3f950d4b11a6075e9d64d5679b1b5))


### Bug Fixes

* **mcp:** drop top-level anyOf from create_task schema ([#340](https://github.com/clawvisor/clawvisor/issues/340)) ([a6f42da](https://github.com/clawvisor/clawvisor/commit/a6f42daa3422b2265967ba1a6005ee4d7b29b6ac))
* **proxy:** snapshot edge cert stat baseline before reload goroutine starts ([#349](https://github.com/clawvisor/clawvisor/issues/349)) ([8a2e753](https://github.com/clawvisor/clawvisor/commit/8a2e75389a44522e5071380a1a03d6013137ca78))
* **settings:** hide local daemon section for non-paid plans ([#347](https://github.com/clawvisor/clawvisor/issues/347)) ([7362c8c](https://github.com/clawvisor/clawvisor/commit/7362c8cfe2e85ee9192e80c8ea7b1adc5bd2f3e7))

## [0.9.2](https://github.com/clawvisor/clawvisor/compare/v0.9.1...v0.9.2) (2026-05-05)


### Features

* **e2e:** add LLM-driven harness with 15 scenarios for runtime + tasks + gateway ([#325](https://github.com/clawvisor/clawvisor/issues/325)) ([fe3dd48](https://github.com/clawvisor/clawvisor/commit/fe3dd4841be9721c34ac71348d9b544d1a8538a3))
* **runtime:** edge TLS provider, multi-tenant leaf cert cache, log redaction handler ([#329](https://github.com/clawvisor/clawvisor/issues/329)) ([98a0f8e](https://github.com/clawvisor/clawvisor/commit/98a0f8e9d7fceed4c19efc35c64588512ac3f97c))
* **runtime:** per-session overrides for InlineApproval, lease TTL, harness allowlist ([#328](https://github.com/clawvisor/clawvisor/issues/328)) ([736de18](https://github.com/clawvisor/clawvisor/commit/736de18425ccb34e4576b9cd607709d2e08048a5))


### Bug Fixes

* **api:** return empty array for agents list when none exist ([#320](https://github.com/clawvisor/clawvisor/issues/320)) ([c9e1061](https://github.com/clawvisor/clawvisor/commit/c9e1061cb53c1f7bb1374ed9ef995ab83f8d91a4))
* **api:** return empty arrays for remaining list endpoints ([#322](https://github.com/clawvisor/clawvisor/issues/322)) ([345bc86](https://github.com/clawvisor/clawvisor/commit/345bc86f335fa307b50fe074def7d4f68725f471))
* **dev:** pin Vite to 25297 and randomize backend so OAuth works ([#323](https://github.com/clawvisor/clawvisor/issues/323)) ([47bdf88](https://github.com/clawvisor/clawvisor/commit/47bdf88864724b8bf4be48724d7efc7b12908eac))
* **e2e:** update harness to new CreateRuntimeSession signature ([#334](https://github.com/clawvisor/clawvisor/issues/334)) ([65f7bda](https://github.com/clawvisor/clawvisor/commit/65f7bdace562c0235afc2a36847173bbbe4aec51))
* **e2e:** update proxy imports to pkg/ after package move ([#333](https://github.com/clawvisor/clawvisor/issues/333)) ([a0edd3f](https://github.com/clawvisor/clawvisor/commit/a0edd3f9644f346afb7861166f341dddc9f6a01b))
* **gmail:** thread draft replies and add cc/bcc support ([#324](https://github.com/clawvisor/clawvisor/issues/324)) ([0b27b7f](https://github.com/clawvisor/clawvisor/commit/0b27b7fa916e04874c90eabc47692d039626f6e9))

## [0.9.1](https://github.com/clawvisor/clawvisor/compare/v0.9.0...v0.9.1) (2026-05-04)


### Features

* **dev:** allow pinning the Vite port via arg or ([#317](https://github.com/clawvisor/clawvisor/issues/317)) ([f033e8f](https://github.com/clawvisor/clawvisor/commit/f033e8fbbbae0d8fcef294ae6e5c0c64998bc469))
* **gmail:** add archive_message action and gate write scopes ([#316](https://github.com/clawvisor/clawvisor/issues/316)) ([db0a92d](https://github.com/clawvisor/clawvisor/commit/db0a92dd6cbf05b8e9a08ac030e52faef37900e9))


### Bug Fixes

* **web:** make per-request approval prompt binary ([#319](https://github.com/clawvisor/clawvisor/issues/319)) ([bb7a075](https://github.com/clawvisor/clawvisor/commit/bb7a075e6596c4098722c539a20a71caa3d9a43a))

## [0.9.0](https://github.com/clawvisor/clawvisor/compare/v0.8.16...v0.9.0) (2026-05-03)


### ⚠ BREAKING CHANGES

* add runtime proxy, controls, expose, and compose isolation ([#315](https://github.com/clawvisor/clawvisor/issues/315))

### Features

* add runtime proxy, controls, expose, and compose isolation ([#315](https://github.com/clawvisor/clawvisor/issues/315)) ([145c95b](https://github.com/clawvisor/clawvisor/commit/145c95b27f9f81ede384c606d2d1fd585c4586f3))
* **gateway:** add batch endpoint and structured error codes ([#291](https://github.com/clawvisor/clawvisor/issues/291)) ([39e2705](https://github.com/clawvisor/clawvisor/commit/39e270509c2f698326056f98275e3efbf83e384c))
* **intent:** per-scope verification modes with approval overrides ([#293](https://github.com/clawvisor/clawvisor/issues/293)) ([cef347d](https://github.com/clawvisor/clawvisor/commit/cef347d5a1f752dc9ea32dcd489c11677ff06de9))
* **scripts:** add dev.sh for hot-reloading local daemon ([#296](https://github.com/clawvisor/clawvisor/issues/296)) ([fd1db5c](https://github.com/clawvisor/clawvisor/commit/fd1db5c637c217e9eb18403cd4d878ca67fe6916))
* **web:** ScopePill tooltips + stacked mobile layout ([#294](https://github.com/clawvisor/clawvisor/issues/294)) ([cdae30a](https://github.com/clawvisor/clawvisor/commit/cdae30ae3af1f08c68efcb949be35a682d03644d))


### Bug Fixes

* **api:** eliminate spurious 503s on long-poll and MCP endpoints ([#299](https://github.com/clawvisor/clawvisor/issues/299)) ([eb7d7f0](https://github.com/clawvisor/clawvisor/commit/eb7d7f098aec8fccc35e8472a8d71ca80a699e65))
* **api:** scope concurrent connection-poll limit per user ([#298](https://github.com/clawvisor/clawvisor/issues/298)) ([81a77c6](https://github.com/clawvisor/clawvisor/commit/81a77c6bf870ef105cbdfc9d1e1ed5491be6fc2e))
* **api:** scope pending connection request limit per user ([#297](https://github.com/clawvisor/clawvisor/issues/297)) ([e30f885](https://github.com/clawvisor/clawvisor/commit/e30f885b65b7c167edb093939622a18d9f0eb11a))
* **calendar:** surface attachments and hangoutLink in event responses ([#308](https://github.com/clawvisor/clawvisor/issues/308)) ([9f83db6](https://github.com/clawvisor/clawvisor/commit/9f83db67d24c05bb7f4ad878c0ba9419360033cd))
* **daemon:** make `clawvisor stop` actually stop the daemon ([#303](https://github.com/clawvisor/clawvisor/issues/303)) ([bd33370](https://github.com/clawvisor/clawvisor/commit/bd3337017c244aeb06f1d44af09bd90365dd06b0))
* separate tui config from server config ([#305](https://github.com/clawvisor/clawvisor/issues/305)) ([daf333d](https://github.com/clawvisor/clawvisor/commit/daf333deabdf24e8504bd735c6b2e59cf58eba5c))
* **tui:** render gateway log timestamps in local time ([#309](https://github.com/clawvisor/clawvisor/issues/309)) ([3b35afa](https://github.com/clawvisor/clawvisor/commit/3b35afabf7251360e20f3e792fa154be0cd34cd4))

## [0.8.16](https://github.com/clawvisor/clawvisor/compare/v0.8.15...v0.8.16) (2026-04-19)


### Features

* **welcome:** Vertex-aware gating, get-started CTA, rocket nav icon ([#289](https://github.com/clawvisor/clawvisor/issues/289)) ([3ddc37f](https://github.com/clawvisor/clawvisor/commit/3ddc37f1a9c25e9754a11c0c9c95bc58560f9cb6))


### Bug Fixes

* **gateway:** prevent spurious chain rejects from cross-instance extraction race ([#287](https://github.com/clawvisor/clawvisor/issues/287)) ([7038117](https://github.com/clawvisor/clawvisor/commit/703811733dd8da001de1cfc2cb84548308279d5c))
* **telemetry:** strip connection aliases from service usage counts ([#290](https://github.com/clawvisor/clawvisor/issues/290)) ([8bc0802](https://github.com/clawvisor/clawvisor/commit/8bc08024810edc8903324d521ea65f04cb39b242))

## [0.8.15](https://github.com/clawvisor/clawvisor/compare/v0.8.14...v0.8.15) (2026-04-18)


### Features

* add Get Started page with LLM-generated task suggestions ([#284](https://github.com/clawvisor/clawvisor/issues/284)) ([ce9b219](https://github.com/clawvisor/clawvisor/commit/ce9b219577e88dd1b24bf744eb2aaa9518168d3c))
* add Perplexity adapter (chat + search actions) ([#283](https://github.com/clawvisor/clawvisor/issues/283)) ([73d96ce](https://github.com/clawvisor/clawvisor/commit/73d96ce3e97f10356553ac2ccc769f745e79b572))
* **gmail:** include resolved label names on list_messages and get_message ([#285](https://github.com/clawvisor/clawvisor/issues/285)) ([fdcd0cf](https://github.com/clawvisor/clawvisor/commit/fdcd0cf70fe0403755d7333785aef41f8f9f066e))
* **granola:** add Granola adapter for meeting notes and transcripts ([#280](https://github.com/clawvisor/clawvisor/issues/280)) ([e25a472](https://github.com/clawvisor/clawvisor/commit/e25a472ece47627dd0adc51a86fb35d90266b918))


### Bug Fixes

* **dashboard:** always show error_msg in task activity row ([#282](https://github.com/clawvisor/clawvisor/issues/282)) ([d721165](https://github.com/clawvisor/clawvisor/commit/d72116506c523b864669e334fa3d14bf2c9ab119))

## [0.8.14](https://github.com/clawvisor/clawvisor/compare/v0.8.13...v0.8.14) (2026-04-17)


### Features

* add icon_url field and swap adapters to official full-color logos ([#268](https://github.com/clawvisor/clawvisor/issues/268)) ([b52f2a6](https://github.com/clawvisor/clawvisor/commit/b52f2a6d98efc393d57665ded8b28e9797042ada))


### Bug Fixes

* keep web/dist embed working without a frontend build ([#269](https://github.com/clawvisor/clawvisor/issues/269)) ([74ee805](https://github.com/clawvisor/clawvisor/commit/74ee8056d61d448128e83a2219c49e33e622cbb9))
* **skill:** use correct list_events param names (from/to) ([#273](https://github.com/clawvisor/clawvisor/issues/273)) ([342ac23](https://github.com/clawvisor/clawvisor/commit/342ac23eebe3e821202dd1d2a7fd81ed66a89b13))
* split OAuth scope string on both comma and space ([#275](https://github.com/clawvisor/clawvisor/issues/275)) ([d0769a7](https://github.com/clawvisor/clawvisor/commit/d0769a7f333bf3a40e9fd403e7ffde6d97d98689))
* **twilio:** interpolate credentials in base_url and map params to Twilio's PascalCase ([#278](https://github.com/clawvisor/clawvisor/issues/278)) ([4d8696e](https://github.com/clawvisor/clawvisor/commit/4d8696e98ea58f45b6b60fd94a3b37946153e7bf))
* use real activated alias in catalog's service:account note ([#271](https://github.com/clawvisor/clawvisor/issues/271)) ([67787ea](https://github.com/clawvisor/clawvisor/commit/67787eac6da5fca8b90f3ac794b6455655ad7938))

## [0.8.13](https://github.com/clawvisor/clawvisor/compare/v0.8.12...v0.8.13) (2026-04-16)


### Bug Fixes

* security & code-quality audit pass (F1–F12) ([#265](https://github.com/clawvisor/clawvisor/issues/265)) ([7a11cf3](https://github.com/clawvisor/clawvisor/commit/7a11cf3d6140cfec0c5099534d1ad066a6ab5f0b))

## [0.8.12](https://github.com/clawvisor/clawvisor/compare/v0.8.11...v0.8.12) (2026-04-16)


### Features

* add forgot-password UI pages and API client methods ([#260](https://github.com/clawvisor/clawvisor/issues/260)) ([7894d2a](https://github.com/clawvisor/clawvisor/commit/7894d2a928b2b3ce223d5ebcae0659e123664366))


### Bug Fixes

* add audit logging to early gateway rejections and scope request_id uniqueness per user ([#262](https://github.com/clawvisor/clawvisor/issues/262)) ([8a9ddee](https://github.com/clawvisor/clawvisor/commit/8a9ddee904f0a1cc9a6f755f3c0f3306dda6e1bc))
* use optimistic update for daemon unpair to prevent stale UI ([#263](https://github.com/clawvisor/clawvisor/issues/263)) ([5195766](https://github.com/clawvisor/clawvisor/commit/5195766a513238e3483ea25e7eed43cdd1856bf9))

## [0.8.11](https://github.com/clawvisor/clawvisor/compare/v0.8.10...v0.8.11) (2026-04-15)


### Features

* redesign daemon card and local services settings ([#256](https://github.com/clawvisor/clawvisor/issues/256)) ([f970d35](https://github.com/clawvisor/clawvisor/commit/f970d3578a4b39fe9b3cf066dfdf1fb9906f4cd9))


### Bug Fixes

* make local services installer idempotent ([#259](https://github.com/clawvisor/clawvisor/issues/259)) ([29b044b](https://github.com/clawvisor/clawvisor/commit/29b044b17feff8ec8082be25565a9db6d270569c))

## [0.8.10](https://github.com/clawvisor/clawvisor/compare/v0.8.9...v0.8.10) (2026-04-15)


### Features

* actionable API error responses for task and gateway endpoints ([#220](https://github.com/clawvisor/clawvisor/issues/220)) ([001a394](https://github.com/clawvisor/clawvisor/commit/001a394475b1fa93b22a4eee8e4658f38dda9e0b))
* add agent feedback system with bug reports and NPS surveys ([#201](https://github.com/clawvisor/clawvisor/issues/201)) ([fcf420c](https://github.com/clawvisor/clawvisor/commit/fcf420c41e0fce7045852728592c23b84e9fa701))
* add connect-src CSP directive for local daemon pairing ([#247](https://github.com/clawvisor/clawvisor/issues/247)) ([25daacc](https://github.com/clawvisor/clawvisor/commit/25daacc3bb58e7749ab2eacd8e6f5d945a77f013))
* add copy buttons to Claude Desktop setup commands ([#195](https://github.com/clawvisor/clawvisor/issues/195)) ([11c0434](https://github.com/clawvisor/clawvisor/commit/11c04345cdb6aa900c1eaa2c8525d8d3ad229d58))
* add Discord and self-hosted CTAs to waitlist success screen ([#199](https://github.com/clawvisor/clawvisor/issues/199)) ([f2e731e](https://github.com/clawvisor/clawvisor/commit/f2e731ef3116bd2637463c0d4f367f7cddcad38f))
* add Dropbox adapter ([#217](https://github.com/clawvisor/clawvisor/issues/217)) ([3b187e9](https://github.com/clawvisor/clawvisor/commit/3b187e98481c752bd7e7dfcc31ddfb3b5262a3ac))
* add feedback hooks for cloud-layer event callbacks ([#250](https://github.com/clawvisor/clawvisor/issues/250)) ([fd13060](https://github.com/clawvisor/clawvisor/commit/fd1306082045ee6534978de6ab1c9cd07a5325d5))
* add Hermes to OpenClaw tab label ([#232](https://github.com/clawvisor/clawvisor/issues/232)) ([e1a534b](https://github.com/clawvisor/clawvisor/commit/e1a534ba5cbe34493d58b0c18244c10f996444b9))
* add key_hint field for service-specific token placeholder text ([#235](https://github.com/clawvisor/clawvisor/issues/235)) ([739f432](https://github.com/clawvisor/clawvisor/commit/739f432d082290d631f58aa775f69e75f62414ff))
* add local_daemon feature flag and daemon pairing UI ([#203](https://github.com/clawvisor/clawvisor/issues/203)) ([b92c384](https://github.com/clawvisor/clawvisor/commit/b92c38447acd514b02c4d9ca2e849e8b8503bab6))
* add mobile_pairing feature flag, disabled by default ([#208](https://github.com/clawvisor/clawvisor/issues/208)) ([8e91879](https://github.com/clawvisor/clawvisor/commit/8e91879dc4da4957a5ecee4a9e8096a52f83df42))
* add Overrides() getter to YAMLAdapter ([#223](https://github.com/clawvisor/clawvisor/issues/223)) ([3364831](https://github.com/clawvisor/clawvisor/commit/3364831a2056df98ed77ab042272b77a6e6c8544))
* add pagination support to all adapters ([#230](https://github.com/clawvisor/clawvisor/issues/230)) ([c7b6164](https://github.com/clawvisor/clawvisor/commit/c7b61643e88a9515969e625fa797d91d050da269))
* add pagination support to Gmail list_messages ([#226](https://github.com/clawvisor/clawvisor/issues/226)) ([cee3348](https://github.com/clawvisor/clawvisor/commit/cee3348be0e4e9988fd000dcfa8a746f960605c0))
* add scope_param and token_path to OAuthDef for non-standard OAuth flows ([#210](https://github.com/clawvisor/clawvisor/issues/210)) ([c9e4979](https://github.com/clawvisor/clawvisor/commit/c9e49795ef3324467be0f6ee10297bc58d126705))
* add TOS acceptance step to onboarding flow ([#239](https://github.com/clawvisor/clawvisor/issues/239)) ([9d4712f](https://github.com/clawvisor/clawvisor/commit/9d4712fce00ba5dfcd9b4bbb5ee466bb3a303a3a))
* add Vertex AI region fallback for LLM requests ([#243](https://github.com/clawvisor/clawvisor/issues/243)) ([2515ab3](https://github.com/clawvisor/clawvisor/commit/2515ab3201019299ec382699084c9069ae9e8f3b))
* add waitlist signup flow to frontend ([#197](https://github.com/clawvisor/clawvisor/issues/197)) ([a2cb023](https://github.com/clawvisor/clawvisor/commit/a2cb0233f94d4e71a9e964ffe433de53bc742728))
* detect partial Google OAuth scopes at task creation ([#211](https://github.com/clawvisor/clawvisor/issues/211)) ([ed1db9e](https://github.com/clawvisor/clawvisor/commit/ed1db9eab77a9440114e770b473dd5a57cb719d5))
* Gmail thread support, reply threading, and sender display name ([#216](https://github.com/clawvisor/clawvisor/issues/216)) ([14a11bd](https://github.com/clawvisor/clawvisor/commit/14a11bde7e7c5b581d9106598f5621c24e2e1c3a))
* local service integration for cloud-hosted agents ([#215](https://github.com/clawvisor/clawvisor/issues/215)) ([735296a](https://github.com/clawvisor/clawvisor/commit/735296a92ce90a20fd0a2bedc9663eabd129842d))
* make webapp responsive for mobile devices ([#209](https://github.com/clawvisor/clawvisor/issues/209)) ([27b6169](https://github.com/clawvisor/clawvisor/commit/27b61699e4d1b042ebc8c17069918501f449fd0f))
* open-source the local services connector ([#206](https://github.com/clawvisor/clawvisor/issues/206)) ([c46b276](https://github.com/clawvisor/clawvisor/commit/c46b276f3282aa5b55380d442c31136f9ddebcba))
* replace free trial with permanent free tier ([#240](https://github.com/clawvisor/clawvisor/issues/240)) ([51a0de5](https://github.com/clawvisor/clawvisor/commit/51a0de52da8af63aeaecf2053ccd0268d4540b18))
* support multiple Telegram groups with independent monitoring ([#200](https://github.com/clawvisor/clawvisor/issues/200)) ([25151e8](https://github.com/clawvisor/clawvisor/commit/25151e8b683e447e10dca1d7df556b85eaf19c87))
* warn on unknown gateway request params with typo detection ([#224](https://github.com/clawvisor/clawvisor/issues/224)) ([2b00ba1](https://github.com/clawvisor/clawvisor/commit/2b00ba13d2e939b601b3eb1a7d8941c71f08507f))


### Bug Fixes

* add missing field support for Google Calendar create/update events ([#251](https://github.com/clawvisor/clawvisor/issues/251)) ([e0444a3](https://github.com/clawvisor/clawvisor/commit/e0444a339d4b8b4932db14bc2cc9f938038458fe))
* add retry with jittered backoff for LLM verification overload ([#242](https://github.com/clawvisor/clawvisor/issues/242)) ([e210705](https://github.com/clawvisor/clawvisor/commit/e2107058f5ec07148606fdc0f4f69938f3be30c7))
* allow subscribed calendar access in intent verification ([#225](https://github.com/clawvisor/clawvisor/issues/225)) ([b409b02](https://github.com/clawvisor/clawvisor/commit/b409b0229e7da6622e4e7a14f63007186b756ba0))
* auto-join waitlist from Google OAuth flow ([#198](https://github.com/clawvisor/clawvisor/issues/198)) ([38ddb26](https://github.com/clawvisor/clawvisor/commit/38ddb261683b4e646469ada7ef03afeaa3b39cbb))
* clarify that Clawvisor needs a separate Telegram bot from agent ([#238](https://github.com/clawvisor/clawvisor/issues/238)) ([f8279cc](https://github.com/clawvisor/clawvisor/commit/f8279ccf7e72331cb2b088db6d6cc745af99f7fa))
* correct OAuth mobile redirect path to /dashboard/services ([#213](https://github.com/clawvisor/clawvisor/issues/213)) ([7612848](https://github.com/clawvisor/clawvisor/commit/761284818118bdbc37c532a6904b054f3af21931))
* exclude standing tasks from group chat auto-approval ([#202](https://github.com/clawvisor/clawvisor/issues/202)) ([639f9ca](https://github.com/clawvisor/clawvisor/commit/639f9ca46b11e916b74bc21b838a9785fa682dd3))
* guard verify-email effect against StrictMode double-invoke ([#236](https://github.com/clawvisor/clawvisor/issues/236)) ([c81592f](https://github.com/clawvisor/clawvisor/commit/c81592f709f65559eec4e3c3ee56b8e7a09d986e))
* include TokenPath in Redis OAuth state serialization ([#214](https://github.com/clawvisor/clawvisor/issues/214)) ([ae80d04](https://github.com/clawvisor/clawvisor/commit/ae80d0477f7278137fcefb5776c08f38770f0f00))
* only send Google-specific OAuth params for Google endpoints ([#233](https://github.com/clawvisor/clawvisor/issues/233)) ([ee0fbdd](https://github.com/clawvisor/clawvisor/commit/ee0fbdd784d0e22d16ee02a122512ad961a76b61))
* pass LocalDaemon feature flag through to API server ([#205](https://github.com/clawvisor/clawvisor/issues/205)) ([a9e08af](https://github.com/clawvisor/clawvisor/commit/a9e08aff57297e3e7b6369efacb3dc4e76dfdc8c))
* remove cancelled flag that blocks verify in StrictMode ([#237](https://github.com/clawvisor/clawvisor/issues/237)) ([299b3c2](https://github.com/clawvisor/clawvisor/commit/299b3c25652f04ce775310c51b52784cf2164db6))
* return UNKNOWN_ACTION error for non-existent adapter actions ([#228](https://github.com/clawvisor/clawvisor/issues/228)) ([228588e](https://github.com/clawvisor/clawvisor/commit/228588e6deaa559639ec52bb47a9d7e9031610d0))
* rotate agent token on --replace instead of delete+recreate ([#244](https://github.com/clawvisor/clawvisor/issues/244)) ([2d5483b](https://github.com/clawvisor/clawvisor/commit/2d5483b81c249a0ee7c30c565b68031bd2473012))
* serve /skill/setup route without requiring daemon ID ([#204](https://github.com/clawvisor/clawvisor/issues/204)) ([187aa5f](https://github.com/clawvisor/clawvisor/commit/187aa5f072b70c84365a09219771659481747ecf))
* skip E2E encryption instructions in /skill/setup for direct connections ([#207](https://github.com/clawvisor/clawvisor/issues/207)) ([69cdbc8](https://github.com/clawvisor/clawvisor/commit/69cdbc85100c0baa8b221c41c0435583be02076c))
* skip localhost daemon probe when no daemons are paired ([#248](https://github.com/clawvisor/clawvisor/issues/248)) ([dcddb04](https://github.com/clawvisor/clawvisor/commit/dcddb041c7fc798d5dd0e784fd0a022d753fcd62))
* support PKCE-sourced credentials in api_key adapters ([#227](https://github.com/clawvisor/clawvisor/issues/227)) ([d7f2d6f](https://github.com/clawvisor/clawvisor/commit/d7f2d6f9a507be8198acd73359c3a8303f63939b))
* use context-appropriate language in setup endpoints for cloud vs local ([#241](https://github.com/clawvisor/clawvisor/issues/241)) ([c5b51ba](https://github.com/clawvisor/clawvisor/commit/c5b51babebdb5aebc08684ccd0a03301fde871bc))
* use generic 'agent' label in OpenClaw connect guide ([#234](https://github.com/clawvisor/clawvisor/issues/234)) ([9c99923](https://github.com/clawvisor/clawvisor/commit/9c999236aa1b8c10e8a57c9180a76e8e65e7f083))
* use same-tab navigation for OAuth on mobile instead of popups ([#212](https://github.com/clawvisor/clawvisor/issues/212)) ([3b013f5](https://github.com/clawvisor/clawvisor/commit/3b013f522a780c836beb70dfbed24dbc406d09f6))

## [0.8.9](https://github.com/clawvisor/clawvisor/compare/v0.8.8...v0.8.9) (2026-04-10)


### Features

* add billing UI pages with promo code support ([#192](https://github.com/clawvisor/clawvisor/issues/192)) ([90ed958](https://github.com/clawvisor/clawvisor/commit/90ed9589f2102ee51e171ac03e9f064093fd1362))
* dev-only skip onboarding UI ([#193](https://github.com/clawvisor/clawvisor/issues/193)) ([c631d6f](https://github.com/clawvisor/clawvisor/commit/c631d6f379326608b668ad9c803070f30cbc5d42))


### Bug Fixes

* extract publish-skill into reusable workflow with manual trigger ([#189](https://github.com/clawvisor/clawvisor/issues/189)) ([babfa59](https://github.com/clawvisor/clawvisor/commit/babfa59b25fddeffd45e658ee87ddc73c3900ecb))
* move inline dark-mode script to external file to fix CSP violation ([#191](https://github.com/clawvisor/clawvisor/issues/191)) ([c05d819](https://github.com/clawvisor/clawvisor/commit/c05d819c61d92665feadce1a0bc7f96454837b4d))

## [0.8.8](https://github.com/clawvisor/clawvisor/compare/v0.8.7...v0.8.8) (2026-04-10)


### Features

* add auto-approve permission rule step for cloud users in setup ([#158](https://github.com/clawvisor/clawvisor/issues/158)) ([5c45abf](https://github.com/clawvisor/clawvisor/commit/5c45abf0106000a4aa2b858e8d08fe041a8a01bb))
* add backup gateway_request_log table for audit durability ([#183](https://github.com/clawvisor/clawvisor/issues/183)) ([4e96bb1](https://github.com/clawvisor/clawvisor/commit/4e96bb17914bfdbb257a765a5d56e0e5aacac779))
* add OpenClaw tab to agent setup guide ([#160](https://github.com/clawvisor/clawvisor/issues/160)) ([be75763](https://github.com/clawvisor/clawvisor/commit/be757636abc3713beb56807cc3dc66992907fc64))
* add PKCE OAuth flow support for Linear integration ([#170](https://github.com/clawvisor/clawvisor/issues/170)) ([f0c8a4d](https://github.com/clawvisor/clawvisor/commit/f0c8a4d8c7b451cf26ce249ac651ac6cf8885d70))
* add skill version endpoint and self-update check in SKILL.md ([#184](https://github.com/clawvisor/clawvisor/issues/184)) ([9d74336](https://github.com/clawvisor/clawvisor/commit/9d74336041606fbb13d2950df491d3771035d3ce))
* add user_id to agent connection requests for multi-tenant support ([#150](https://github.com/clawvisor/clawvisor/issues/150)) ([2625bb0](https://github.com/clawvisor/clawvisor/commit/2625bb0d57a5090677a4823ca4b5266b418cb671))
* consolidate org pages into shared components with org-awareness ([#151](https://github.com/clawvisor/clawvisor/issues/151)) ([3ce2285](https://github.com/clawvisor/clawvisor/commit/3ce22858da81ffa6f4c90371d931fae5ca03598a))
* extract iMessage into separate FDA-holding helper binary ([#185](https://github.com/clawvisor/clawvisor/issues/185)) ([d0e360e](https://github.com/clawvisor/clawvisor/commit/d0e360ea73200b6f7d3522b2a264cbdaedfb32cb))
* extract in-memory stores behind interfaces with Redis implementations ([#176](https://github.com/clawvisor/clawvisor/issues/176)) ([c996f39](https://github.com/clawvisor/clawvisor/commit/c996f39cf69d4740ea488399270a8b3970bbf295))
* gate generate-integration behind adapter_gen feature flag ([#163](https://github.com/clawvisor/clawvisor/issues/163)) ([eb5da75](https://github.com/clawvisor/clawvisor/commit/eb5da75d69dad94105fea4b224e30f5af1a9efa1))
* load clawvisor skill before smoke test in setup flow ([#165](https://github.com/clawvisor/clawvisor/issues/165)) ([ba6d6f8](https://github.com/clawvisor/clawvisor/commit/ba6d6f84f8a87a2f1d8f20f514b6b38b1040b26a))
* opt-in auto-update for self-hosted deployments ([#182](https://github.com/clawvisor/clawvisor/issues/182)) ([7ebae63](https://github.com/clawvisor/clawvisor/commit/7ebae632fd22d4c798af4200fb6b3ecf5375af92))
* package iMessage helper as .app bundle for proper macOS FDA attribution ([#188](https://github.com/clawvisor/clawvisor/issues/188)) ([a77cbd5](https://github.com/clawvisor/clawvisor/commit/a77cbd5ba88ea421dd599e367d433aba9cf25a01))
* replace inline onboarding with persistent setup banner ([#161](https://github.com/clawvisor/clawvisor/issues/161)) ([2dd4ef0](https://github.com/clawvisor/clawvisor/commit/2dd4ef0d814a256a43835e4d55a1e6988a0236e9))
* revoke active tasks when a service is disconnected ([#171](https://github.com/clawvisor/clawvisor/issues/171)) ([cdb7bbc](https://github.com/clawvisor/clawvisor/commit/cdb7bbc833d0748b1dcb292db8773672f10aefd6))
* show success banner when service connection is established ([#180](https://github.com/clawvisor/clawvisor/issues/180)) ([a9013cf](https://github.com/clawvisor/clawvisor/commit/a9013cfc36a7d3aad002f73048b8ccdf6b78114c))
* soft-delete agents and revoke their tasks on deletion ([#178](https://github.com/clawvisor/clawvisor/issues/178)) ([8bd3b5d](https://github.com/clawvisor/clawvisor/commit/8bd3b5dfd17e3a403fb6dc504fa2130d91a9ab9f))
* unify onboarding and MFA login flow in frontend ([#181](https://github.com/clawvisor/clawvisor/issues/181)) ([6439204](https://github.com/clawvisor/clawvisor/commit/6439204fb14f6acba907d2e8239fcb5d466809c6))
* update Claude Desktop setup to use plugin-based flow ([#164](https://github.com/clawvisor/clawvisor/issues/164)) ([6e8cc6c](https://github.com/clawvisor/clawvisor/commit/6e8cc6c62714793dd2fef6325f1a8f2173c57789))
* warn users about affected tasks before disconnecting a service ([#173](https://github.com/clawvisor/clawvisor/issues/173)) ([2ef17c5](https://github.com/clawvisor/clawvisor/commit/2ef17c5d6377be3fdab7b391c66218e363277338))


### Bug Fixes

* data race on Version variable in Apply() ([#186](https://github.com/clawvisor/clawvisor/issues/186)) ([87b18b2](https://github.com/clawvisor/clawvisor/commit/87b18b2b18d3af43fae6819b3e4a055151cbdc07))
* harden security, reliability, and error handling across backend and frontend ([#175](https://github.com/clawvisor/clawvisor/issues/175)) ([25371f4](https://github.com/clawvisor/clawvisor/commit/25371f46a6d81a582fede5b4765c7174660b2f8b))
* log swallowed errors instead of silently discarding them ([#174](https://github.com/clawvisor/clawvisor/issues/174)) ([3b91159](https://github.com/clawvisor/clawvisor/commit/3b911592377e6591db5da7280791f9636b208804))
* make audit log insert idempotent to prevent duplicate key errors ([#167](https://github.com/clawvisor/clawvisor/issues/167)) ([ee0f64f](https://github.com/clawvisor/clawvisor/commit/ee0f64fc8e36511d54b260ca1d87e968a00a5e63))
* prevent Vertex provider from using Anthropic API endpoint ([#168](https://github.com/clawvisor/clawvisor/issues/168)) ([f5201bb](https://github.com/clawvisor/clawvisor/commit/f5201bb09dd85d21b711c700665ee03ffc32054d))
* remove model field from Vertex completion request body ([#166](https://github.com/clawvisor/clawvisor/issues/166)) ([aabcbaa](https://github.com/clawvisor/clawvisor/commit/aabcbaacc2c4d4ebb00bb4cdf695b871a7d6a158))
* resolve service alias fallback and web/dist embed for tests ([#153](https://github.com/clawvisor/clawvisor/issues/153)) ([9389d86](https://github.com/clawvisor/clawvisor/commit/9389d8662861a10983f8a214d235edc7a366985e))
* restore approve/deny mutual exclusion in Redis token store and sweep all OAuth maps ([#177](https://github.com/clawvisor/clawvisor/issues/177)) ([0d8ae9a](https://github.com/clawvisor/clawvisor/commit/0d8ae9aa2c80ea510778bb54818263ae4b73b9c4))
* return available connections when gateway request has no matching account ([#157](https://github.com/clawvisor/clawvisor/issues/157)) ([e117c0b](https://github.com/clawvisor/clawvisor/commit/e117c0b93633d9a361e5c4b14b4b41bae3b35bf0))
* run check_permissions after helper install to register for FDA ([#187](https://github.com/clawvisor/clawvisor/issues/187)) ([8cc0dfe](https://github.com/clawvisor/clawvisor/commit/8cc0dfe3b7ec0fd52e62c655fd34784eb0ec5916))
* serve /skill/clawvisor-setup.md without daemon ID for cloud deployments ([#155](https://github.com/clawvisor/clawvisor/issues/155)) ([6df398f](https://github.com/clawvisor/clawvisor/commit/6df398fcebce32db3f340873d459842243ca1eec))
* skip smoke tests using local config unless CLAWVISOR_LOCAL_CONFIG is set ([#169](https://github.com/clawvisor/clawvisor/issues/169)) ([339cd3e](https://github.com/clawvisor/clawvisor/commit/339cd3efb5d84b78e1f61983da2011573ee89cf1))
* update vite and picomatch to patch high-severity vulnerabilities ([#179](https://github.com/clawvisor/clawvisor/issues/179)) ([d6e5f4f](https://github.com/clawvisor/clawvisor/commit/d6e5f4fc6c74c365c7a3cbfe67398523b599874d))
* use correct anthropic_version for Vertex AI rawPredict ([#156](https://github.com/clawvisor/clawvisor/issues/156)) ([217068d](https://github.com/clawvisor/clawvisor/commit/217068d12144e5a6df4f8dab370198813f37ab80))
* use IsLocal() instead of ViaRelay for cloud detection ([#159](https://github.com/clawvisor/clawvisor/issues/159)) ([80eb6e4](https://github.com/clawvisor/clawvisor/commit/80eb6e4c4bc3f171938388e4b86bad4188fad2bf))
* use port 8080 and host 127.0.0.1 for web-dev, add web-install prereq ([#162](https://github.com/clawvisor/clawvisor/issues/162)) ([db2d2af](https://github.com/clawvisor/clawvisor/commit/db2d2af8c4c8de93ad3414e6b876ab4edccfed2b))

## [0.8.7](https://github.com/clawvisor/clawvisor/compare/v0.8.6...v0.8.7) (2026-04-09)


### Features

* add agent setup guide to dashboard with local/relay-aware URLs ([#148](https://github.com/clawvisor/clawvisor/issues/148)) ([d6ffbbe](https://github.com/clawvisor/clawvisor/commit/d6ffbbe544852c3d46af6e2061c4448316e8840e))
* add export_file action to Google Drive adapter ([#149](https://github.com/clawvisor/clawvisor/issues/149)) ([23e90ce](https://github.com/clawvisor/clawvisor/commit/23e90ce39db47ae061038873be9562bdd551e194))
* add Redis-backed event hub, magic tokens, and decision bus for multi-instance deployments ([#147](https://github.com/clawvisor/clawvisor/issues/147)) ([3ba9883](https://github.com/clawvisor/clawvisor/commit/3ba988382cebfad1a295a568287a2df71184d3b9))
* LLM-powered adapter generation from OpenAPI specs ([#139](https://github.com/clawvisor/clawvisor/issues/139)) ([800e40a](https://github.com/clawvisor/clawvisor/commit/800e40a22866a206a9fc565246f2decad4e770a0))
* org support extension points for multi-tenancy ([#120](https://github.com/clawvisor/clawvisor/issues/120)) ([4699c4c](https://github.com/clawvisor/clawvisor/commit/4699c4cc89d805b6d80de29e074cde917d270829))
* pluggable AdapterStore with per-user DB persistence for cloud ([#145](https://github.com/clawvisor/clawvisor/issues/145)) ([05d10ab](https://github.com/clawvisor/clawvisor/commit/05d10ab9555f7b4a15a69e73ad1c5398d414ffde))
* Telegram group chat auto-approval ([#135](https://github.com/clawvisor/clawvisor/issues/135)) ([8ccf891](https://github.com/clawvisor/clawvisor/commit/8ccf8918d408937da541b6f441bd0a25fd70de65))
* user-configurable variables in adapter YAML specs ([#146](https://github.com/clawvisor/clawvisor/issues/146)) ([5b0f01b](https://github.com/clawvisor/clawvisor/commit/5b0f01b1d413725dfcfd006384fc9188194cda7e))


### Bug Fixes

* add service:account usage hint to catalog overview ([#144](https://github.com/clawvisor/clawvisor/issues/144)) ([4ab08d5](https://github.com/clawvisor/clawvisor/commit/4ab08d51ad3858372546adbae81cb470351a3164))
* authenticate clawhub CLI before publishing skill ([#138](https://github.com/clawvisor/clawvisor/issues/138)) ([e0a4e0c](https://github.com/clawvisor/clawvisor/commit/e0a4e0c4701cb40f6f4e56ad296230cbe3325acf))
* auto-restart daemon after clawvisor update ([#141](https://github.com/clawvisor/clawvisor/issues/141)) ([c8b372f](https://github.com/clawvisor/clawvisor/commit/c8b372f195aef6acc2e8237eb7440c01c6b814a8))
* sync YAML integration spec with implementation and remove dead code ([#143](https://github.com/clawvisor/clawvisor/issues/143)) ([b5a4645](https://github.com/clawvisor/clawvisor/commit/b5a4645ec6f49cb387eb27f6b083803e43ffe7d4))
* use absolute URL for YAML spec in generate-integration skill ([#142](https://github.com/clawvisor/clawvisor/issues/142)) ([5f8dc63](https://github.com/clawvisor/clawvisor/commit/5f8dc63fb40a5eac915438a90f42b037adb49db7))

## [0.8.6](https://github.com/clawvisor/clawvisor/compare/v0.8.5...v0.8.6) (2026-04-08)


### Features

* add scope guidance, reason docs, and fetchable task presets ([#134](https://github.com/clawvisor/clawvisor/issues/134)) ([e9d3636](https://github.com/clawvisor/clawvisor/commit/e9d3636139aa73ee473f92f217412c1098bfd4ce))
* services dashboard overhaul with OAuth, auto-identity, and alias rename ([#133](https://github.com/clawvisor/clawvisor/issues/133)) ([2c50178](https://github.com/clawvisor/clawvisor/commit/2c501787aa6a8818d47be96bad16f1a4971e381d))


### Bug Fixes

* allow subset requests in intent verification for paginated pulls ([#136](https://github.com/clawvisor/clawvisor/issues/136)) ([7a71dd3](https://github.com/clawvisor/clawvisor/commit/7a71dd344d2a9e37a3ded42ea0ef65b9f21dfd5f))
* diff against previous tag to detect skill changes across releases ([#130](https://github.com/clawvisor/clawvisor/issues/130)) ([5b4827e](https://github.com/clawvisor/clawvisor/commit/5b4827ec5e1f2ca7c10ba3de23883b444acbdfd1))
* reduce intent verification false positives for triage workflows ([#131](https://github.com/clawvisor/clawvisor/issues/131)) ([5c2e1f6](https://github.com/clawvisor/clawvisor/commit/5c2e1f60df8c203df3c9e9b86c7de96870e8176f))

## [0.8.5](https://github.com/clawvisor/clawvisor/compare/v0.8.4...v0.8.5) (2026-04-07)


### Features

* add pagination support to Google Contacts list_contacts ([#128](https://github.com/clawvisor/clawvisor/issues/128)) ([a1716ba](https://github.com/clawvisor/clawvisor/commit/a1716ba8191de48f2493434996b1da872d75e19a))
* add permalink and channel fields to Slack adapter responses ([#124](https://github.com/clawvisor/clawvisor/issues/124)) ([8e0374b](https://github.com/clawvisor/clawvisor/commit/8e0374b98d4c9b7505089f1c4ae22d0210eafbb2))
* chain context fallback with regex-based extraction ([#129](https://github.com/clawvisor/clawvisor/issues/129)) ([d331179](https://github.com/clawvisor/clawvisor/commit/d331179e285b9215fcf45a52c1ea19dae5bd3359))


### Bug Fixes

* return full email body instead of preview-only text/plain ([#127](https://github.com/clawvisor/clawvisor/issues/127)) ([65f56ef](https://github.com/clawvisor/clawvisor/commit/65f56efb2f5e2d91db9207a25edc13f3c921d63e))
* teach clawvisor skill to scope tasks broadly and verbosely ([#126](https://github.com/clawvisor/clawvisor/issues/126)) ([be50e4e](https://github.com/clawvisor/clawvisor/commit/be50e4e0b446facaafef405e92cb687f30da6a24))

## [0.8.4](https://github.com/clawvisor/clawvisor/compare/v0.8.3...v0.8.4) (2026-04-07)


### Features

* add CSV export button to gateway log page ([#123](https://github.com/clawvisor/clawvisor/issues/123)) ([fbf083b](https://github.com/clawvisor/clawvisor/commit/fbf083b452d77891a7c5bc65bc51cfc1148ecda0))
* Gmail adapter attachment support ([#122](https://github.com/clawvisor/clawvisor/issues/122)) ([bc9f3d1](https://github.com/clawvisor/clawvisor/commit/bc9f3d182cfe5e77a86605190fad939623bd7d29))
* pre-registered planned calls to skip intent verification ([#116](https://github.com/clawvisor/clawvisor/issues/116)) ([c4503cb](https://github.com/clawvisor/clawvisor/commit/c4503cbfc0d7aac4cd0da8013fba43749a7df9b9))
* publish skill to ClawHub on release ([#113](https://github.com/clawvisor/clawvisor/issues/113)) ([130381c](https://github.com/clawvisor/clawvisor/commit/130381c65b5f8ec91b9cc5ed25da9b7be9329e85))


### Bug Fixes

* enable chain context extraction for planned call matches ([#117](https://github.com/clawvisor/clawvisor/issues/117)) ([1b45df1](https://github.com/clawvisor/clawvisor/commit/1b45df1868ce8ad88bc041490c7dcfb6b8007d83))
* hide iMessage on non-macOS and check Full Disk Access on activation ([#119](https://github.com/clawvisor/clawvisor/issues/119)) ([85e998d](https://github.com/clawvisor/clawvisor/commit/85e998de23df84fd3aa6cd071cd3dfb0988edfbd))
* planned call UI visibility and Telegram risk level ([#118](https://github.com/clawvisor/clawvisor/issues/118)) ([c00d0f3](https://github.com/clawvisor/clawvisor/commit/c00d0f385a4f0484c355e1881fbd7ef9456975a8))
* reduce false positives for broad list actions and imperative reasons ([#121](https://github.com/clawvisor/clawvisor/issues/121)) ([f57c3dd](https://github.com/clawvisor/clawvisor/commit/f57c3ddc8aa95a4cf6743f48e104207addbce3b8))

## [0.8.3](https://github.com/clawvisor/clawvisor/compare/v0.8.2...v0.8.3) (2026-04-05)


### Features

* add CI-compatible e2e tests with mock test adapters ([#110](https://github.com/clawvisor/clawvisor/issues/110)) ([34abfad](https://github.com/clawvisor/clawvisor/commit/34abfadc54b4ee06d69cc683a6f2980d83ef9b87))


### Bug Fixes

* include private channels and support pagination in Slack adapter ([#112](https://github.com/clawvisor/clawvisor/issues/112)) ([c149212](https://github.com/clawvisor/clawvisor/commit/c1492124d8b88f75d15d6a0fbf29b082734ab313))

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
