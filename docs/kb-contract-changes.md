# Proposed changes to the support KB metadata contract

This page proposes changes to `docs/metadata-contract.md` in the `support-agent` repo. It is a reference for the contract owner, who applies the changes by hand. Nothing here is applied yet.

Source of the values: the approved KB publisher spec (2026-10-30). Contract sections are cited as "contract section N".

Approved by the docs-rag owner: numeric bounds in the simple sidecar form, the product value `webhooks`, and a `#anchor` in `source_url`.

Default excludes: `publish-kb` leaves out these files by default:

- `stack.oas.json`, `stack.oas.enriched.json`, `stack.oas.prose.json`, `membership.oas.json` and `api.schemas.json`.
- The dataset `cookbook.numscript.byFingerprint`, which lives inside `numscript.snippets.json`.

`api.operations` and `membership.operations` carry the API in readable form. `ledger.cli-flags` is kept. `--no-default-excludes` restores every default exclude.

## 1. Summary and why

`docs-rag` becomes the producer of the Formance documentation into the support Knowledge Base. The contract was written for one version string and one product per file. The documentation does not fit that model:

- A doc section applies to a range of versions, such as "from Stack v3.1, before v3.2". Bounds are written on component scales (Stack, ledger, payments, numscript interpreter).
- Many pages have no single product. They cover the whole Stack or are shared fragments.

The contract's answer to ranges is one upload per version (contract section 3, "Version format"). That multiplies chunks and cannot express open bounds or component scales. This proposal replaces it with numeric bounds on one common scale, the Stack scale. The scalar `version` key is removed for all document types.

| Change | Contract sections touched |
| --- | --- |
| New keys `version_min`, `version_below`, `version_component`, typed values | 3, 7 |
| Remove the `version` key, no migration | 3, 5, 7, 8 |
| Stack-scale conversion rules | 3 (new subsection) |
| Doc mapping, `webhooks`, `source_url` fragment | 3, 5, 6, 7 |
| One file per chunk, new key pattern | 2, 6, 7 |
| Tickets and fiches carry the bounds | 3, 6, 7 |
| Bot filter, unknown version, `source_url` rewrite | 5 |
| Size budget | 4 |
| Fallbacks, open questions | 5 (operators), 8 |

## 2. New keys and typed values

### Keys

Add three rows to the field table (contract section 3).

| Name | Type | Required | Format | Filterable | Notes |
| --- | --- | --- | --- | --- | --- |
| `version_min` | NUMBER | yes | Integer 0 to 99999. Stack scale: `vX.Y` is `X*100+Y` (v3.1 is 301). `0` means no lower bound. | yes | Inclusive. |
| `version_below` | NUMBER | yes | Integer 1 to 99999. Stack scale, exclusive. `99999` means no upper bound. | yes | Must be greater than `version_min`. |
| `version_component` | STRING | docs: yes. Tickets and fiches: no. | `^[a-z0-9._-]{1,64}$`, for example `stack`, `ledger`, `numscript-interpreter`. | yes | The component the original bound was written on. Informational. The bot does not filter on it. |

Scale limit: the formula only works when the minor version is 99 or less. The current "Version format" pattern allows a three-digit minor. Add this rule: a producer must reject a Stack version with minor above 99.

### Typed values in the simple form

The simple sidecar form accepts JSON numbers. Change the text of contract section 3 as follows.

| Current text | New text |
| --- | --- |
| "The metadata file uses the simple form. Every value is a string." | "The metadata file uses the simple form. Every value is a string, except `version_min` and `version_below`, which are JSON numbers (no quotes)." |

Update the validation checklist (contract section 7):

| Item | Current | New |
| --- | --- | --- |
| Field count | "All nine fields are present. No extra keys." | "The eight original fields (all but `version`) and `version_min` and `version_below` are present. `version_component` is present for `doc`. No keys outside these eleven." |
| Value types | "Every value is a string. No numbers, booleans or lists." | "Every value is a string, except `version_min` and `version_below`, which are integers. No booleans, lists or floats." |
| New | none | "`0 <= version_min < version_below <= 99999`." |
| New | none | "`version_component`, when present, matches `^[a-z0-9._-]{1,64}$`." |

### Removing `version`

The `version` key is removed from the contract for all document types. There is no migration:

- No consumer reads the new keys yet, and the KB holds no ticket or fiche.
- `version` is replaced by `version_min` and `version_below`. Producers never write both.
- Contract section 8 ("Removing or renaming a field") has no steps to follow. Drop the `version` row from the field table and from every example (contract sections 2, 5 and 6), and remove `version` from the "no extra keys" rule.

A doc sidecar has 11 keys. A ticket or fiche sidecar has 10, because `version_component` is optional for them.

## 3. Stack-scale conversion rules

The docs write bounds on several component scales. `docs-rag` converts every bound to the Stack scale with the `stack` component releases in `apps/docs/content/schema.ts`. The table is read from that file at publish time, so it holds every Stack release the file lists, upcoming ones included (v4.0 is upcoming today). Each release lists `requires` per component. A requirement is a point version (`3.0.18`) or a range (`{min, below}`). A release allows a range of versions of each component: `{min, below}` is `[min, below)`, `{min}` alone is `[min, +inf)`, and a point version `p` is the single version `[p, p]`.

`schema.ts` is the source of truth. Revisit at the v4.0 release: the v4.0 entry is marked upcoming and can change before it ships.

Add these rules as a new subsection of contract section 3, after "Version format".

| Case | Rule |
| --- | --- |
| Stack bound (`v3.2`) | Convert directly with `X*100+Y`. v3.2 is 302. |
| Component range `[m, b)` (either bound may be absent: `-inf` / `+inf`) | A Stack release is included iff the range it allows for the component **intersects** `[m, b)` (non-empty overlap). `below` is exclusive on both sides, so `[2.3.0, 2.4.0)` and `[2.4.0, 2.5.0)` do not intersect. A point version `p` intersects iff `m <= p < b`. `version_min` is the number of the first release that intersects (`0` when `m` is absent). `version_below` is the number of the release after the last one that intersects (`99999` when there is none). No release intersects: empty, the chunk is excluded. `m` above every version the table lists: not convertible (the content is for an unreleased version). Example: v3.1 allows ledger `[2.3.0, 2.4.0)` and v3.2 starts at 2.4.11, so content for ledger `[2.3.1, 2.4.0)` gets `[301, 302)`. |
| `numscript-interpreter` range | Same rule at each level of the cascade stack, ledger, numscript-interpreter. The interpreter range is intersected with the interpreter range each ledger release bundles. Each matching ledger release applies from its version up to the next ledger entry. Their spans are merged into one ledger range (first start to last end), and that range is intersected with the `ledger` ranges of the Stack releases as above. Every ledger release in `schema.ts` declares an interpreter range starting at 0.0.10, so an interpreter bound with only `below` (for example below 0.0.20) intersects every ledger release and gives `version_below` 99999. This over-includes, which is the conservative direction. |
| Component `orchestration` (written `flows` in docs) | The docs name `flows` is looked up as the schema id `orchestration`. The product of the chunk is `flows`. |
| Component not in the Stack table | `membership`, `operator`, `fctl-*`, `sdk-*` and similar: not convertible. This holds for an upper bound too: a component that no Stack release lists has no `99999` fallback. |
| Version that cannot be parsed | Not convertible. |

### Why any overlap keeps the release

The customer knows their Stack version, not the patch version of each component. A Stack release that allows ledger `[2.3.0, 2.4.0)` can run 2.3.0 or 2.3.9, so content valid for part of that range may apply, and the release is kept. This can show a chunk that does not apply to one customer. It never hides one that does.

[min, below) must be one interval. If the releases that intersect are not adjacent (v3.0 and v3.2 but not v3.1), the conversion takes the superset from the first intersecting release up to the first release after the last one, and the dry-run lists the chunk under "WARNING: ranges overlapping non-adjacent Stack releases". With today's schema this does not happen: each component's versions only grow from one release to the next, so the intersecting releases are always adjacent. A component that leaves the Stack and comes back would be the exception, and there the cap below wins.

### Components that leave the Stack

A component can disappear from a later Stack release. A release that no longer lists the component does not intersect anything, so the range is capped at the first Stack release, after one that lists the component, that no longer lists it. For example, `payments` is absent from the upcoming v4.0, so a payments bound with no upper limit gets `version_below` 400, not `99999`. A range is empty, and the chunk is excluded (see "Empty ranges are excluded"), only when no Stack release intersects it. A lower bound above every listed version is widened, not emptied. The cap deliberately wins: if a component leaves the Stack and is listed again in a later release, `version_below` stops at the first release that no longer lists it, even if a later release would intersect again. The dry-run prints the non-contiguous warning in that case. This does not happen with today's `schema.ts`.

### Widened chunks

Widening is per bound. Only the bound that is not convertible becomes `0` (lower) or `99999` (upper). A bound that converts keeps its value. For example, a lower bound that converts to 302 with an upper bound that does not gives `[302, 99999)`. The chunk is not widened as a whole.

Widening is a safe error: the bot may show a chunk that does not apply, but it never hides one that does.

`docs-rag` lists every widened chunk by file in its dry-run output, with the reason "not convertible". The inline note in the body keeps the component bound, for example `(Applies from payments 3.3.0)`. See section 5 of this page.

### Empty ranges are excluded

A converted range that is empty (`version_min >= version_below`) means that no Stack release ships the range. The chunk is excluded, not widened. An inline `<When>` block with an empty range is excluded in the same way: its paragraph is left out of the body. `docs-rag` lists every excluded chunk and block in its dry-run output under the heading "EXCLUDED: no Stack release ships these ranges", one line per entry (level page or inline, file, component, bounds).

Excluding is safe here: no supported Stack release ships the content, so no customer needs it.

## 4. Doc mapping

Add a "Docs" column or a subsection to contract section 3. This table states how `docs-rag` derives each key.

| Key | Value for a doc | Derivation |
| --- | --- | --- |
| `doc_type` | `doc` | Constant. |
| `product` | See "Product rules" below. | From frontmatter `path`, or the file name for generated data. |
| `version_min` | Number | From the page frontmatter and `<When>` blocks, converted as in section 3. |
| `version_below` | Number | Same. |
| `version_component` | String | The component of the original bound. `stack` when the bound is on the Stack scale or there is no bound. |
| `deployment` | `cloud`, `self-hosted`, `any` | From visibility. `docs-rag` value `both` becomes `any`. |
| `license` | `ee`, `oss`, `any` | `ee` when the badge is `enterprise` or frontmatter `visibility.license` is `ee`. `oss` when `visibility.license` is `oss`. Otherwise `any`. |
| `client_id` | `none` | Constant for docs. |
| `language` | `en` | Constant. |
| `source_url` | `https://docs.formance.com` + frontmatter `path` + optional `#<anchor>` | The anchor is the leaf heading of the chunk. 200 bytes or fewer. |
| `updated_at` | `YYYY-MM-DD` | Date of the last git commit of the source file in the docs repo. Docs use this date form only. This is a restriction for docs: the contract also allows the timestamp form. |

### Product rules

| Source | `product` |
| --- | --- |
| Path `/modules/<m>/...` with `m` in `ledger`, `payments`, `wallets`, `flows`, `reconciliation`, `numscript`, `webhooks` | `<m>` |
| Path `/deploy`, `/manage`, `/getting-started`, `/stack-api-reference` | `stack` |
| Any other path | `other` |
| Generated data `ledger.*`, `payments.*`, `numscript.*` | That product |
| Generated data `stack.*`, `api.*`, `platform.*`, `operator.*`, `infra.*`, `otel.*`, `sdks`, `cloud.*`, `audit.*` | `stack` |
| Generated data `membership.*` | `other` |
| Fragments | The module name in the path if there is one, otherwise `other` |
| `packages/numscript-engine` | `numscript` |

Contract changes for `product`:

- Add `webhooks` to the allowed values (contract section 3 and the validation checklist). Approved by the docs-rag owner.
- Keep `product` as metadata. The bot no longer filters on it (section 7 of this page). Pages that mix products make a product filter drop valid content.

The contract README (section 8) lists "nine string fields". It needs the same edit for `webhooks` and for the removal of `version`.

### `source_url` rule change

Contract section 3, "source_url rules", and the checklist forbid `?` and `#`. Change the rule for `doc`:

- `doc`: a `#fragment` is allowed. Approved by the docs-rag owner. A query string (`?`) is still forbidden.
- `ticket` and `fiche`: unchanged.

For fragments and generated data, the URL is the page that includes them. `api.*` and `stack.oas*` use `https://docs.formance.com/stack-api-reference`. `membership.*` uses `https://docs.formance.com/membership-api`. The fallback is `https://docs.formance.com/`, which is acceptable.

The publisher keeps bare paths (`https://docs.formance.com/<path>`). Section 7 of this page gives the bot rule that adds the version.

## 5. Object layout change

Replace the `doc` row of the prefix table (contract section 2).

| doc_type | Current key pattern | New key pattern |
| --- | --- | --- |
| `doc` | `docs/<product>/<version>/<slug>.md` | `docs/<product>/<page path>/<section-slug>.md` |

Examples:

```text
docs/ledger/modules/ledger/accounts/_intro.md
docs/ledger/modules/ledger/accounts/metadata-3f9a1c20.md
docs/stack/_fragments/<fragment>/<slug>.md
docs/payments/_data/<dataset>/<id-slug>.md
```

Key rules:

- The page path has no leading slash. Text before the first H2 uses `_intro`.
- The slug comes from the section name, not its position. Duplicate names in one page get `-2`, `-3` in document order. Parts of a split chunk get `-p2`, `-p3`.
- A slug over 60 characters is cut to 60 plus 8 hex characters of a hash.
- Fragments live under `docs/<product>/_fragments/`. Generated data lives under `docs/<product>/_data/<dataset>/`.
- Characters: `[a-z0-9._/-]` only. This matches the existing key rules.

### Why one file per chunk

| Reason | Detail |
| --- | --- |
| Metadata per section | Each section has its own bounds, license and `source_url` anchor. One file per page cannot carry that. |
| Small files | The KB keeps one data source with semantic chunking (512 tokens max) for the whole KB. `docs-rag` re-splits files to about 1500 bytes and to at most 400 estimated tokens, on paragraph or line boundaries, with the breadcrumb repeated in each part. Code blocks over the budget are split at line boundaries, with each part closing and reopening its code fence. The token estimate is conservative: text inside code fences and lines with more than 12% symbol characters count 2 characters per token, prose 4, and each non-ASCII character 1 token. Validation refuses any file over the token budget. A file usually stays under 512 tokens. There is no guarantee: the KB may still re-split code-heavy files. |
| Stable keys | A key depends on the section name, not its position. Inserting a section does not rename the others. A rename is a delete plus a create, so stable keys keep syncs small. |
| Safe deletion | `docs-rag` lists everything under `docs/`, compares a `sha256` of body plus sidecar (stored as S3 user metadata), and deletes stale keys and orphan sidecars. It never deletes outside `docs/`. It refuses when deletes exceed 20% of existing objects, unless `--allow-mass-delete` is set. |
| No version in the key | The version is metadata now. The same chunk is not uploaded once per version. |

### File body

The body starts with the breadcrumb (`Title > H2 > H3`). Add this to the contract as a body convention for docs:

- An `Applies to:` line follows the breadcrumb when the page is gated. It lists Stack versions, Enterprise Edition, Cloud or self-hosted, the new Numscript interpreter, and Experimental.
- Inline conditional blocks (`<When>`) are not flattened. The paragraph stays with an inline note: `(Applies from Stack v3.2)`, `(Applies before Stack v3.2)`, `(Applies from Stack v3.1, before v3.2)`, `(Applies to self-hosted)`, `(Applies to Enterprise Edition)`. A block whose range no Stack release ships is excluded (section 3 of this page).
- A bound that is not convertible keeps a component note, for example `(Applies from payments 3.3.0)`.
- `<DocCase value="ee">` gets `(Enterprise Edition)`. `<DocCase value="oss">` gets `(Open source edition)`.

The notes put the condition in the text the model reads, because the chunk-level bounds only describe the whole chunk.

Update the examples in contract section 2 ("Sidecar file") and section 6 ("doc") to the new key pattern.

## 6. Tickets and fiches carry the bounds

A numeric condition excludes vectors that lack the key. This is the same behavior as `in` (contract section 3, `client_id`). A ticket or fiche without `version_min` and `version_below` would be invisible once the bot uses the numeric filter.

Add to the field table and the checklist: tickets and fiches MUST carry `version_min` and `version_below`. They no longer carry `version`. The producer reads the version of the ticket or fiche and writes the bounds from it.

| Ticket or fiche version | `version_min` | `version_below` |
| --- | --- | --- |
| Exact `vX.Y` | `X*100+Y` | `X*100+Y+1` |
| `any` | `0` | `99999` |

Example: `v2.3` gives `[203, 204)`. For tickets and fiches, the version is the version of the Stack, as the customer profile reports it. See the open questions.

The minor is 99 or less (section 2 of this page). So `vX.99` gives `[X*100+99, (X+1)*100)`, which touches `v(X+1).0` and never overlaps it. An overlap would need a minor of 100 or more, such as `v2.100` landing on 300, and producers reject that.

## 7. Bot filter

### Example filter

Replace the example in contract section 5, "Example filter". This is the filter for a customer on Stack v3.1, cloud, Enterprise Edition. Stack v3.1 is 301.

```json
{
  "andAll": [
    { "in": { "key": "client_id", "value": ["none", "<client>"] } },
    { "lessThanOrEquals": { "key": "version_min", "value": 301 } },
    { "greaterThan": { "key": "version_below", "value": 301 } },
    { "in": { "key": "deployment", "value": ["cloud", "any"] } },
    { "in": { "key": "license", "value": ["ee", "any"] } }
  ]
}
```

A chunk matches when `version_min <= 301 < version_below`.

### Condition count

The filter has 5 conditions. AWS allows up to 5 groups in an `andAll` (contract section 5, "Operators and limits"). The filter is at the limit. `product` is dropped from the filter to make room for the second version condition. Do not add a sixth condition. `client_id` is always present.

### Unknown customer Stack version

The customer profile does not expose the Stack version yet. Until it does, the bot always applies this rule:

- If the Stack version is unknown, the bot drops both version conditions. The filter has 3 conditions.
- The bot fails closed on `client_id` only. It never fails closed on version. A missing version must not hide the docs.

### `source_url` and the customer version

When the customer version is known, the bot rewrites `source_url` to the customer's version path, for example `https://docs.formance.com/v3.1/<path>`. The publisher does not do this: it keeps the bare path. Add the rewrite to the bot rules in contract section 5.

### Text changes in contract section 5

| Current | New |
| --- | --- |
| "The bot filters on `product`, `version`, `deployment`, `license` and `client_id`." | "The bot filters on `version_min`, `version_below`, `deployment`, `license` and `client_id`. `product` is metadata only." |
| "Each of the five filters is an `equals` or `in` condition." | "Each of the five conditions is an `in` condition, or a numeric comparison for the version (`lessThanOrEquals` on `version_min`, `greaterThan` on `version_below`). With an unknown Stack version, the version conditions are dropped." |
| "Do not add a sixth condition ... drop `product` when the question is not product-specific." | "Do not add a sixth condition. `product` is not in the filter. Never drop `client_id`." |

Also add `lessThanOrEquals` and `greaterThan` to the tested-status line in "Operators and limits" once the test passes (section 9).

## 8. Size budget

Update contract section 4. Same method: compact JSON, ASCII, entry = key with quotes + 1 colon + value. Numbers have no quotes.

| Key | Key bytes (with quotes) | Colon | Worst value | Value bytes | Entry bytes |
| --- | --- | --- | --- | --- | --- |
| 8 original keys (all but `version`) | | | | | 429 (449 from contract section 4, minus 20 for `version`) |
| `version_min` | 13 | 1 | `99999` | 5 | 19 |
| `version_below` | 15 | 1 | `99999` | 5 | 21 |
| `version_component` | 19 | 1 | 64 characters | 66 | 86 |

- Entries: 429 + 19 + 21 + 86 = 555 bytes.
- Commas between 11 entries: 10 bytes. Total 565 bytes. This is the serialized `metadataAttributes` object.
- Wrapper `{"metadataAttributes":{` (23 bytes) and `}}` (2 bytes): 25 bytes. Total 590 bytes with the wrapper. Treat this as the conservative figure.

The producer's 900-byte limit counts only the serialized `metadataAttributes` object (`internal/kbpublish/sidecar.go`, `attributesSize`), so the worst case is 565 bytes. With the wrapper it is 590 bytes. Both are under the 900-byte limit and the 1 KB KB limit. The key count is 11 of 35. A realistic doc (`doc_type` `doc`, `client_id` `none`, a 10-character date, a 21-character component) is about 485 bytes without the wrapper.

Index impact:

- `version_min`, `version_below` and `version_component` are not in the non-filterable list (`AMAZON_BEDROCK_TEXT`, `AMAZON_BEDROCK_METADATA`, `source_url`, `updated_at`). They need no index recreation.
- The KB holds no ticket or fiche yet, so there are no existing sidecars to migrate. Documents are published with the new keys from the first sync.

## 9. Status of the numeric operators

| Operator | AWS documents it for S3 Vectors | Tested on our deployment |
| --- | --- | --- |
| `in` | yes | yes (2026-10-30) |
| `lessThanOrEquals` | yes | no |
| `greaterThan` | yes | no |
| `listContains` | yes | no |

`docs-rag` ships `scripts/kb-verify.sh`. It tests the numeric operators first, on the dev KB (`support-kb-test`). Do not switch the bot to the numeric filter until it passes. When it passes, update "Operators and limits" in contract section 5 and the Known limitations in the KB README.

## 10. Fallbacks if numeric filters fail

Why this needs care: with `in`, the metadata value must be a scalar and the filter lists the candidate values. A per-chunk scalar cannot express a range. A range needs either a comparison, a list value, or one copy per version.

The primary design drops the `version` key. Option C below is a fallback that brings back a scalar `version` key, for that option only. Use it only if the numeric filters and `listContains` both fail.

| Option | Idea | Pros | Cons | Test needed |
| --- | --- | --- | --- | --- |
| A. `stack_versions` list | Key `stack_versions`, type STRING_LIST. It lists every compatible Stack version, for example `["v3.1","v3.2"]`. The bot filters with `listContains`. | One copy per chunk. Keeps the range model. | `listContains` is untested. Open ranges must list every supported version, so each Stack release forces a re-upload of open-ended chunks. Lists use bytes. | A `listContains` check, inside `andAll`. `kb-verify.sh` does not test it, because the docs publish no STRING_LIST key. It needs dedicated fixtures, for example an extension of the support-agent smoke test. |
| B. One key per version | A key such as `v3_1: true` per version. | Uses `equals`. | Key explosion. 35 keys per vector. Every release adds a key and the producers must set all of them. | None. Rejected. |
| C. One copy per version (fallback: reintroduces scalar `version`) | Scalar `version` string, written per copy. Upload one copy of each versioned chunk per supported Stack version. The bot filters with `in ["v3.1","any"]`. | Uses only `in`, which is tested. This is the current contract's approach. | About N copies of every versioned chunk, where N is the number of supported Stack versions. The key must carry the version again (`docs/<product>/<version>/...`). Duplicate hits in results. | None beyond the existing smoke test. |

`in` with a STRING_LIST key does not help. `in` compares a scalar value to a list given in the filter. It does not test whether a stored list contains a value.

### Recommendation

1. Try A first (`listContains`).
2. If `listContains` also fails, use C. It costs about N copies of versioned chunks and brings `version` back as a scalar key on those copies. Chunks with no version bound use `any` and stay single.

### Size of option A

Key `"stack_versions"` is 16 bytes with quotes, plus 1 colon. A list of 10 versions:

| Case | Element | Bytes per element | List bytes | Entry bytes |
| --- | --- | --- | --- | --- |
| Realistic | `"v3.1"` | 6 | 10 x 6 + 9 commas + 2 brackets = 71 | 16 + 1 + 71 = 88 |
| Worst case | `"v999.999"` | 10 | 10 x 10 + 9 commas + 2 brackets = 111 | 16 + 1 + 111 = 128 |

The worst case sidecar has the 8 original entries without `version` (429) plus 128, so 557 bytes. With 8 commas (9 entries) the `metadataAttributes` object is 565 bytes, the figure the 900-byte limit counts. With the 25-byte wrapper the conservative total is 590 bytes. Both fit in 900 bytes. Assumption: list values count as their serialized bytes in the 1 KB budget. Confirm this with the test.

### Filter for v3.1, cloud, EE under each fallback

Option A (4 conditions, with room for `product` as a fifth):

```json
{
  "andAll": [
    { "in": { "key": "client_id", "value": ["none", "<client>"] } },
    { "listContains": { "key": "stack_versions", "value": "v3.1" } },
    { "in": { "key": "deployment", "value": ["cloud", "any"] } },
    { "in": { "key": "license", "value": ["ee", "any"] } }
  ]
}
```

A chunk valid for every version must list every supported version, because `listContains` has no `any`.

Filling `stack_versions`: docs list every supported Stack version inside `[version_min, version_below)`. Tickets and fiches have an exact `vX.Y`, so the list is `["vX.Y"]`. A ticket or fiche with version `any` lists every supported Stack version.

Option C (4 conditions, with room for `product` as a fifth). This is the only place the scalar `version` key is used:

```json
{
  "andAll": [
    { "in": { "key": "client_id", "value": ["none", "<client>"] } },
    { "in": { "key": "version", "value": ["v3.1", "any"] } },
    { "in": { "key": "deployment", "value": ["cloud", "any"] } },
    { "in": { "key": "license", "value": ["ee", "any"] } }
  ]
}
```

Both stay within the 5-condition limit. For an unknown Stack version, drop the version condition (3 conditions). With option C this returns duplicate hits: every versioned chunk exists in N copies and all N match.

## 11. Open questions for the contract owner

1. Where do the ticket and fiche producers get the bound? Is the version on a ticket the Stack version?
2. When will the customer profile API return the Stack version, and not a module version? The new scale is the Stack scale. Until then the bot uses the unknown-version rule (section 7 of this page), which needs no answer.
3. Is the minor-above-99 limit acceptable? The old pattern allows `v999.999`.
4. Is `version_component` required for tickets and fiches, or optional as proposed here?
