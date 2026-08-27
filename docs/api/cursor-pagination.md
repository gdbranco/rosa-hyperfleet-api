# Cursor-Based Pagination

Migration from limit/offset to keyset cursor pagination for the three K8s/Postgres-backed list endpoints: `/clusters`, `/nodepools`, `/oidc_configs`.

## Why

Offset pagination fetches all rows up to the requested position and then discards them. As account sizes grow this is expensive, and items shift when records are added or deleted between pages — causing duplicates or skips. Cursor (keyset) pagination fixes both: only the needed rows are fetched, and the cursor marks a stable position in the ordering that isn't affected by concurrent writes.

ZOA/DynamoDB endpoints are out of scope — they require a separate DynamoDB `ExclusiveStartKey` approach.

## API Changes

### Request

| Parameter | Before | After |
|---|---|---|
| Page size | `?limit=N` (default 50, max 100) | `?limit=N` (unchanged) |
| Page position | `?offset=N` | `?continue=<token>` |

The `continue` token is an opaque base64 string returned by the previous response. Omit it to start from the beginning. The old `?offset=` parameter is removed.

### Response

```json
{
  "items": [...],
  "limit": 50,
  "has_more": true,
  "continue": "eyJ0eGlkX3N0YW1wIjo0MiwiYWNjb3VudF9pZCI6IjEyMzQ1Njc4OTAxMiJ9"
}
```

| Field | Before | After |
|---|---|---|
| `items` | ✓ | ✓ (unchanged) |
| `total` | ✓ count of all records | removed — unreliable across pages |
| `offset` | ✓ | removed |
| `limit` | ✓ | ✓ (unchanged) |
| `has_more` | — | ✓ true when a next page exists |
| `continue` | — | ✓ token to pass on the next request |

`has_more` is true when and only when `continue` is non-empty.

## How It Works

The sort key for all list queries is `txid_stamp` (a PostgreSQL `xid8` transaction ID) — monotonically increasing per transaction, never reused. The cursor encodes the `txid_stamp` of the last item on the current page.

On the next request, the SQL changes from:

```sql
ORDER BY txid_stamp LIMIT $N OFFSET $M
```

to:

```sql
WHERE ... AND txid_stamp > $cursor
ORDER BY txid_stamp LIMIT $N
```

This means the DB only touches the rows that need to be returned.

## Account Filtering

Previously `ListClusters` and `ListNodePools` (without clusterID) fetched all rows of that GVK and filtered by account label in Go memory. With this change, the account filter moves to SQL via a field selector on `metadata.labels`:

```sql
AND metadata->'labels'->>'hyperfleet.io/account-id' = $accountID
```

All three resource types now scope at the DB level — no rows for other accounts are read.

## Security

### Account ID validation

The `Identity` middleware validates the `X-Amz-Account-Id` header against the AWS 12-digit format (`^[0-9]{12}$`) before storing it in context. Malformed account IDs are rejected with HTTP 400 before reaching any handler.

### Cursor validation

The continue token embeds both the `txid_stamp` cursor position and the account ID that generated it:

```json
{"txid_stamp": 42, "account_id": "123456789012"}
```

On decode, the token's `account_id` is compared against the current request's account ID. A mismatch returns HTTP 400. This prevents accidental cross-account cursor reuse and gives a clear error rather than silent skip behavior.

A structurally malformed token (bad base64, invalid JSON) also returns HTTP 400 — distinct from the HTTP 500 returned for genuine server/DB errors.

## Clientset Changes

The `platform.ListOptions` struct in the `clientset` module changes:

```go
// Before
type ListOptions struct {
    Limit  int64
    Offset int64
}

// After
type ListOptions struct {
    Limit    int64
    Continue string // opaque token from a prior list response; empty = first page
}
```

The transport adapter (`clientset/transport/bridge.go`) previously rewrote numeric `?continue=N` to `?offset=N`. That rewrite is removed — the opaque token is passed through as-is.

## Paginating Through Results

```go
var allClusters []*v1alpha1.Cluster
opts := platform.ListOptions{Limit: 50}

for {
    list, err := cs.HyperfleetV1alpha1().Clusters().List(ctx, opts)
    if err != nil {
        return err
    }
    allClusters = append(allClusters, list.Items...)
    if !list.HasMore {
        break
    }
    opts.Continue = list.Continue
}
```
