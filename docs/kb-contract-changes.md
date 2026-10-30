# Proposed changes to the support KB metadata contract

This page proposes changes to `docs/metadata-contract.md` in the `support-agent` repo. It is a reference for the contract owner, who applies the changes by hand. Nothing here is applied yet.

Source of the values: the approved KB publisher spec (2026-10-30). Contract sections are cited as "contract section N".

## 1. Summary and why

`docs-rag` becomes the producer of the Formance documentation into the support Knowledge Base. The contract was written for one version string and one product per file. The documentation does not fit that model:

- A doc section applies to a range of versions, such as "from Stack v3.1, before v3.2". Bounds are written on component scales (Stack, ledger, payments, numscript interpreter).
- Many pages have no single product. They cover the whole Stack or are shared fragments.

The contract's answer to ranges is one upload per version (contract section 3, "Version format"). That multiplies chunks and cannot express open bounds or component scales. This proposal replaces it with numeric bounds on one common scale, the Stack scale.

| Change | Contract sections touched |
| --- | --- |
| New keys `version_min`, `version_below`, `version_component`, typed values | 3, 7, 8 |
| Stack-scale conversion rules | 3 (new subsection) |
| Doc mapping, `webhooks`, `source_url` fragment | 3, 5, 6, 7 |
| One file per chunk, new key pattern | 2, 6, 7 |
| Tickets and fiches carry the bounds | 3, 6, 7 |
| Bot filter and unknown version | 5 |
| Size budget | 4 |
| Fallbacks, open questions | 5 (operators), 8 |

## 2. New keys and typed values

### Keys

Add three rows to the field table (contract section 3).

| Name | Type | Required | Format | Filterable | Notes |
| --- | --- | --- | --- | --- | --- |
| `version_min` | NUMBER | yes | Integer 0 to 99999. Stack scale: `vX.Y` is `X*100+Y` (v3.1 is 301). `0` means no lower bound. | yes | Inclusive. |
| `version_below` | NUMBER | yes | Integer 1 to 99999. Stack scale, exclusive. `99999` means no upper bound. | yes | Must be greater than `version_min`. |
| `version_component` | STRING | docs: yes. Tickets and fiches: no. | `^[a-z0-9-]{1,32}$`, for example `stack`, `ledger`, `numscript-interpreter`. | yes | The component the original bound was written on. Informational. The bot does not filter on it. |

Scale limit: the formula only works when the minor version is 99 or less. The current "Version format" pattern allows a three-digit minor. Add this rule: a producer must reject a Stack version with minor above 99.

### Typed values in the simple form

The simple sidecar form accepts JSON numbers. Change the text of contract section 3 as follows.

| Current text | New text |
| --- | --- |
| "The metadata file uses the simple form. Every value is a string." | "The metadata file uses the simple form. Every value is a string, except `version_min` and `version_below`, which are JSON numbers (no quotes)." |

Update the validation checklist (contract section 7):

| Item | Current | New |
| --- | --- | --- |
| Field count | "All nine fields are present. No extra keys." | "The nine original fields and `version_min` and `version_below` are present. `version_component` is present for `doc`. No keys outside these twelve." |
| Value types | "Every value is a string. No numbers, booleans or lists." | "Every value is a string, except `version_min` and `version_below`, which are integers. No booleans, lists or floats." |
| New | none | "`0 <= version_min < version_below <= 99999`." |
| New | none | "`version_component`, when present, matches `^[a-z0-9-]{1,32}$`." |

### Legacy `version` during migration

`version` stays. Docs write `"any"`. Follow contract section 8 ("Removing or renaming a field"):

1. Producers write both `version` and the new keys. The validation allows both.
2. The consumer switches its filter to `version_min` and `version_below`.
3. Producers drop `version`, and the contract drops the row.

Until step 3, the "no extra keys" rule must name `version` as allowed.

## 3. Stack-scale conversion rules

The docs write bounds on several component scales. `docs-rag` converts every bound to the Stack scale with the `stack` component releases in `apps/docs/content/schema.ts` (v3.0, v3.1, v3.2). Each release lists `requires` per component. A requirement is a point version (`3.0.18`) or a range (`{min, below}`). The "shipped version" of a release for a component is the lower bound of its range, or the point version.

Add these rules as a new subsection of contract section 3, after "Version format".

| Case | Rule |
| --- | --- |
| Stack bound (`v3.2`) | Convert directly with `X*100+Y`. v3.2 is 302. |
| Component lower bound `m` | The first Stack release, in release order, whose shipped version is `>= m`. Its number is `version_min`. No release ships it: not convertible. |
| Component upper bound `b` | The first Stack release whose shipped version is `>= b`. The feature is gone from that release, so this is `version_below`. No such release: `99999`. |
| `numscript-interpreter` bound `m` | Ledger releases require an interpreter range. Find the first ledger release whose interpreter `below` is greater than `m`. Then convert that ledger version through the Stack table. This is a cascade: stack, ledger, numscript-interpreter. |
| Component not in the Stack table | `membership`, `operator`, `fctl-*`, `sdk-*` and similar: not convertible. |
| Version that cannot be parsed | Not convertible. |

### Widened chunks

A chunk with a bound that is not convertible is widened to `version_min` `0` and `version_below` `99999`. It stays visible for every Stack version. This is a safe error: the bot may show a chunk that does not apply, but it never hides one that does.

`docs-rag` lists every widened chunk by file in its dry-run output. The inline note in the body keeps the component bound, for example `(Applies from payments 3.3.0)`. See section 5 of this page.

## 4. Doc mapping

Add a "Docs" column or a subsection to contract section 3. This table states how `docs-rag` derives each key.

| Key | Value for a doc | Derivation |
| --- | --- | --- |
| `doc_type` | `doc` | Constant. |
| `product` | See "Product rules" below. | From frontmatter `path`, or the file name for generated data. |
| `version` | `any` | Legacy key, constant during migration. |
| `version_min` | Number | From the page frontmatter and `<When>` blocks, converted as in section 3. |
| `version_below` | Number | Same. |
| `version_component` | String | The component of the original bound. `stack` when the bound is on the Stack scale or there is no bound. |
| `deployment` | `cloud`, `self-hosted`, `any` | From visibility. `docs-rag` value `both` becomes `any`. |
| `license` | `ee`, `oss`, `any` | `ee` when the badge is `enterprise` or frontmatter `visibility.license` is `ee`. `oss` when `visibility.license` is `oss`. Otherwise `any`. |
| `client_id` | `none` | Constant for docs. |
| `language` | `en` | Constant. |
| `source_url` | `https://docs.formance.com` + frontmatter `path` + optional `#<anchor>` | The anchor is the leaf heading of the chunk. 200 characters or fewer. |
| `updated_at` | `YYYY-MM-DD` | Date of the last git commit of the source file in the docs repo. |

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

- Add `webhooks` to the allowed values (contract section 3 and the validation checklist).
- Keep `product` as metadata. The bot no longer filters on it (section 7 of this page). Pages that mix products make a product filter drop valid content.

### `source_url` rule change

Contract section 3, "source_url rules", and the checklist forbid `?` and `#`. Change the rule for `doc`:

- `doc`: a `#fragment` is allowed. A query string (`?`) is still forbidden.
- `ticket` and `fiche`: unchanged.

For fragments and generated data, the URL is the page that includes them. `api.*` and `stack.oas*` use `https://docs.formance.com/stack-api-reference`. `membership.*` uses `https://docs.formance.com/membership-api`. The fallback is `https://docs.formance.com/`.

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
| No re-split | Chunks are at most about 1800 bytes (about 500 tokens). The KB semantic chunker (512 tokens max) does not cut them again, so the breadcrumb and the metadata stay with the text. A larger chunk is split on paragraph or line boundaries, with the breadcrumb repeated in each part. |
| Stable keys | A key depends on the section name, not its position. Inserting a section does not rename the others. A rename is a delete plus a create, so stable keys keep syncs small. |
| Safe deletion | `docs-rag` lists everything under `docs/`, compares a `sha256` of body plus sidecar (stored as S3 user metadata), and deletes stale keys and orphan sidecars. It never deletes outside `docs/`. It refuses when deletes exceed 20% of existing objects, unless `--allow-mass-delete` is set. |
| No version in the key | The version is metadata now. The same chunk is not uploaded once per version. |

### File body

The body starts with the breadcrumb (`Title > H2 > H3`). Add this to the contract as a body convention for docs:

- An `Applies to:` line follows the breadcrumb when the page is gated. It lists Stack versions, Enterprise Edition, Cloud or self-hosted, the new Numscript interpreter, and Experimental.
- Inline conditional blocks (`<When>`) are not flattened. The paragraph stays with an inline note: `(Applies from Stack v3.2)`, `(Applies before Stack v3.2)`, `(Applies from Stack v3.1, before v3.2)`, `(Applies to self-hosted)`, `(Applies to Enterprise Edition)`.
- A bound that is not convertible keeps a component note, for example `(Applies from payments 3.3.0)`.
- `<DocCase value="ee">` gets `(Enterprise Edition)`. `<DocCase value="oss">` gets `(Open source edition)`.

The notes put the condition in the text the model reads, because the chunk-level bounds only describe the whole chunk.

Update the examples in contract section 2 ("Sidecar file") and section 6 ("doc") to the new key pattern.

## 6. Tickets and fiches carry the bounds

A numeric condition excludes vectors that lack the key. This is the same behavior as `in` (contract section 3, `client_id`). A ticket or fiche without `version_min` and `version_below` would be invisible once the bot uses the numeric filter.

Add to the field table and the checklist: tickets and fiches MUST carry `version_min` and `version_below`.

| Source `version` | `version_min` | `version_below` |
| --- | --- | --- |
| Exact `vX.Y` | `X*100+Y` | `X*100+Y+1` |
| `any` | `0` | `99999` |

Example: `v2.3` gives `[203, 204)`. For tickets and fiches, `version` is the version of the Stack, as the customer profile reports it. See the open questions.

`version` stays during migration, as in section 2 of this page.

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

- If the Stack version is unknown, the bot drops both version conditions. The filter has 3 conditions.
- The bot fails closed on `client_id` only. It never fails closed on version. A missing version must not hide the docs.

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
| 9 original keys | | | | | 449 (from contract section 4) |
| `version_min` | 13 | 1 | `99999` | 5 | 19 |
| `version_below` | 15 | 1 | `99999` | 5 | 21 |
| `version_component` | 19 | 1 | 32 characters | 34 | 54 |

- Entries: 449 + 19 + 21 + 54 = 543 bytes.
- Commas between 12 entries: 11 bytes. Total 554 bytes.
- Wrapper `{"metadataAttributes":{` (23 bytes) and `}}` (2 bytes): 25 bytes. Total 579 bytes.

The worst case is 579 bytes, which is under the 900-byte producer limit and the 1 KB KB limit. The key count is 12 of 35. A realistic doc (`doc_type` `doc`, `version` `any`, `client_id` `none`, a 10-character date, a 21-character component) is about 500 bytes.

Index impact:

- `version_min`, `version_below` and `version_component` are not in the non-filterable list (`AMAZON_BEDROCK_TEXT`, `AMAZON_BEDROCK_METADATA`, `source_url`, `updated_at`). They need no index recreation.
- Existing objects need a re-upload of their sidecars and a sync (contract section 8, "Adding a filterable field").

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

| Option | Idea | Pros | Cons | Test needed |
| --- | --- | --- | --- | --- |
| A. `stack_versions` list | Key `stack_versions`, type STRING_LIST. It lists every compatible Stack version, for example `["v3.1","v3.2"]`. The bot filters with `listContains`. | One copy per chunk. Keeps the range model. | `listContains` is untested. Open ranges must list every supported version, so each Stack release forces a re-upload of open-ended chunks. Lists use bytes. | A `listContains` check, inside `andAll`, in `kb-verify.sh`. |
| B. One key per version | A key such as `v3_1: true` per version. | Uses `equals`. | Key explosion. 35 keys per vector. Every release adds a key and the producers must set all of them. | None. Rejected. |
| C. One copy per version | Scalar `version` string. Upload one copy of each versioned chunk per supported Stack version. The bot filters with `in ["v3.1","any"]`. | Uses only `in`, which is tested. This is the current contract's approach. | About N copies of every versioned chunk, where N is the number of supported Stack versions. The key must carry the version again (`docs/<product>/<version>/...`). Duplicate hits in results. | None beyond the existing smoke test. |

`in` with a STRING_LIST key does not help. `in` compares a scalar value to a list given in the filter. It does not test whether a stored list contains a value.

### Recommendation

1. Try A first (`listContains`).
2. If `listContains` also fails, use C. It costs about N copies of versioned chunks. Chunks with no version bound use `any` and stay single.

### Size of option A

Key `"stack_versions"` is 16 bytes with quotes, plus 1 colon. A list of 10 versions:

| Case | Element | Bytes per element | List bytes | Entry bytes |
| --- | --- | --- | --- | --- |
| Realistic | `"v3.1"` | 6 | 10 x 6 + 9 commas + 2 brackets = 71 | 16 + 1 + 71 = 88 |
| Worst case | `"v999.999"` | 10 | 10 x 10 + 9 commas + 2 brackets = 111 | 16 + 1 + 111 = 128 |

The worst case sidecar has the 9 original entries (449) plus 128, so 577 bytes. With 9 commas (10 entries) and the 25-byte wrapper, the total is 611 bytes. It fits in 900 bytes. Assumption: list values count as their serialized bytes in the 1 KB budget. Confirm this with the test.

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

Option C (4 conditions, with room for `product` as a fifth):

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

Both stay within the 5-condition limit. For an unknown Stack version, drop the version condition (3 conditions).

## 11. Open questions for the contract owner

1. Which version does the customer profile API return: the Stack version or a module version? The new scale is the Stack scale, and the bot needs the Stack version. The contract does not say.
2. Where do the ticket and fiche producers get the bound? Is `version` on a ticket the Stack version?
3. Is the minor-above-99 limit acceptable? The old pattern allows `v999.999`.
4. Is `version_component` required for tickets and fiches, or optional as proposed here?
5. When should producers drop `version` (step 3 of the migration)? Name an owner and a date.
6. Is the `source_url` fragment limit still 200 characters with the anchor included?
7. Should the contract name `webhooks` in the README section 8 text, which lists "nine string fields"? The README needs the same edit.
