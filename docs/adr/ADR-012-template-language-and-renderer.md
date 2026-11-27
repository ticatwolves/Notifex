# ADR-012 — Template language and the sandboxed renderer

- **Status:** Accepted (2026-11-27). Approved with the Phase 2 plan.
- **Decider:** Shobhit

## Context
Tenants write templates that Notifex executes on every send, with payloads they also control (docs/03 §Template language). The threat model lists template injection (SSTI), resource exhaustion and XSS as the main risks of this surface (docs/06 §Template injection, §Resource exhaustion). The renderer runs in the dispatcher on the hot path, and in the API for previews.

## Decision

### 1. An in-house Handlebars subset
`internal/workflow/render` parses templates into an AST and evaluates it with explicit budgets. Supported:

| Construct | Notes |
|---|---|
| `{{path}}` | Paths are dotted (`payload.order.id`), with `this`, `../` (parent scope, at most 8 levels) and `@index`, `@first`, `@last`, `@key` inside `each` |
| `{{#if path}}…{{else}}…{{/if}}`, `{{#unless path}}…{{/unless}}` | Handlebars truthiness: `false`, `null`, missing, `""`, `0` and `[]` are falsy |
| `{{#each path}}…{{else}}…{{/each}}` | Arrays and objects (objects iterate in sorted key order, so output is deterministic) |
| `{{#with path}}…{{else}}…{{/with}}` | |
| `{{! comment }}`, `{{!-- comment --}}` | |

**Rejected at parse time**, with a line and column: triple-stash `{{{raw}}}`, helpers and arguments (`{{foo bar}}`), partials (`{{> x}}`), whitespace control (`~`), decorators, sub-expressions. There is no way to call a function, read a file, or reach anything outside the render context.

**Render context** (plain JSON values only; no Go structs, so no methods are reachable):
`payload` (the trigger payload), `recipient` (the fields given in the trigger), `brand` (the effective brand, §4), `workflow` (`key`, `name`). Missing paths render as empty strings. If the workflow has a `payload_schema`, a reference to a path the schema doesn't define is a **warning** in the preview, not an error. Numbers keep their JSON text (`1000000` stays `1000000`, never `1e+06`).

### 2. Budgets
Every render runs under hard limits. Going over any of them is an error, never truncated output:

| Limit | Value |
|---|---|
| Template source per field | 64 KiB |
| Nesting depth (blocks) | 32 |
| Evaluation steps (nodes visited, including every loop iteration) | 100,000 |
| Output | per channel (§3) |
| Cancellation | the request or dispatch context is checked every 1,024 steps |

The step budget is what stops `{{#each}}` inside `{{#each}}` over a 10k-item array (10⁸ iterations) from running away. Output is written to a bounded buffer, so a size bomb fails as soon as it crosses the limit, before it is fully built.

### 3. Escaping and per-channel output
Escaping depends on where the value lands, not on the tenant:

| Channel / field | Escaping | Output limit | Validation |
|---|---|---|---|
| Email `subject` | Plain text; CR, LF and other control characters removed (header injection) | 998 characters | Non-empty |
| Email `html` | Values HTML-escaped; the rendered HTML is then sanitized (§5) and wrapped in the brand layout unless `layout: none` | 512 KiB | — |
| Email `text` | Plain text. If absent, generated from the sanitized HTML | 512 KiB | — |
| SMS `text` | Plain text | 3 segments | GSM-7 vs UCS-2 segment count (§6) |
| Slack `text`, `blocks` | `text` escaped for Slack mrkdwn (`&`, `<`, `>`). `blocks` is a JSON array: each **string leaf** is rendered separately as text, then the document is re-encoded | 40 KiB, 50 blocks | Block types limited to `section`, `header`, `divider`, `context`, `actions`, `image` |
| Webhook `body` | JSON value; each string leaf rendered separately, then re-encoded | 256 KiB | Valid JSON |

Rendering string leaves of a parsed JSON document (instead of rendering JSON text) means a payload value can never break out of a string and add keys or blocks. Push, WhatsApp and Outlook join in Phase 4 with their own rows here.

### 4. Brand
`brand` in the context is the **effective** brand: the tenant's brand profile with the application's override applied field by field (a field set on the application wins). The email layout puts the logo, colours, footer and legal text around the body. Brand fields are data and are escaped like any other value.

### 5. Email HTML sanitizing
The rendered HTML goes through `bluemonday` with an email policy:
- allowed: common text and table markup, images, links;
- link schemes: `https`, `http`, `mailto`; image sources: `https` only;
- inline `style` only with a property allowlist (colour, background colour, font, text alignment, padding, margin, border, width, height);
- not allowed: `<script>`, `<style>`, `<iframe>`, forms, event handlers, `javascript:` and `data:` URLs.

The plain-text part is generated from the sanitized HTML (links become `text (url)`).

### 6. SMS segments
GSM-7 if every character is in the GSM 03.38 basic or extension table (extension characters count as two); otherwise UCS-2 (characters outside the BMP count as two units). Single segment: 160 GSM-7 / 70 UCS-2; multipart: 153 / 67 per segment. More than 3 segments is a validation error. The preview reports the encoding, segment count and the characters that forced UCS-2.

### 7. Locale selection (used from Phase 3)
Templates are stored per locale for each action. At send time: the recipient's exact locale → its language (`en-IN` → `en`) → the effective brand's `default_locale` → otherwise `template_missing` for that notification.

## Consequences
- About 2 days of work and a security-critical package we own. It gets fuzz tests (parser and evaluator: no panics, budgets always hold, escaping always holds) and is named in scope for the pre-GA penetration test (docs/06).
- Templates are portable to real Handlebars (the subset is strictly smaller), so a later engine swap doesn't break tenant templates.
- No helpers means no date or number formatting in v1. Tenants format values in the payload. Adding a fixed, reviewed helper set later is a minor-version change.
- New dependency: `github.com/microcosm-cc/bluemonday` (HTML sanitizer). `golang.org/x/net/html` (already an indirect dependency) for the text part.

## Alternatives considered
- **`aymerick/raymond`** (Handlebars for Go). It renders the whole output as one string with no step or size budget, supports helpers and partials we'd have to disable, and is largely unmaintained. Rejected.
- **Go `text/template` / `html/template`** with a restricted FuncMap. Different syntax from what docs/03 promises, method calls on values are possible, and there's no step budget. Rejected.
- **Running templates in a separate sandbox process or WASM.** Strong isolation, but heavy for the dispatcher hot path. An in-process interpreter with no function calls and hard budgets gives the same guarantees for this language. Rejected for now; revisit if helpers ever execute tenant code.
