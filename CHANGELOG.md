# Changelog

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
