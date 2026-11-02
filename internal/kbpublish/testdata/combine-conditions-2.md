Filter and Paginate Results > Build expressions > Combine conditions
Applies to: Stack v4.0 and later (ledger 3.0.0 and later)

`curl --get --data-urlencode` produces this encoding for you.

#### Either of two conditions, excluding a third

Gold or silver accounts that are not blocked:

```text
(metadata[tier] == "gold" or metadata[tier] == "silver") and not metadata[status] == "blocked"
```

```json
{"$and":[{"$or":[{"$match":{"metadata[tier]":"gold"}},{"$match":{"metadata[tier]":"silver"}}]},{"$not":{"$match":{"metadata[status]":"blocked"}}}]}
```

#### Transactions in a date range, by metadata

Card transactions with an effective date in March 2026. Date bounds accept RFC 3339 timestamps. Use an exclusive upper bound at the start of the next month so that timestamps with fractional seconds late on March 31 are included:

```text
timestamp >= "2026-03-01T00:00:00Z" and timestamp < "2026-04-01T00:00:00Z" and metadata[channel] == "card"
```

```json
{"$and":[{"$gte":{"timestamp":"2026-03-01T00:00:00Z"}},{"$lt":{"timestamp":"2026-04-01T00:00:00Z"}},{"$match":{"metadata[channel]":"card"}}]}
```

#### Source and destination

Transactions funded from `world` that credit an account under `users:`:

```text
source == "world" and destination ^= "users:"
```

```json
{"$and":[{"$match":{"source":"world"}},{"$match":{"destination":"users:"}}]}
```