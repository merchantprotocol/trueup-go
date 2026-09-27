# TrueUp for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/merchantprotocol/trueup-go.svg)](https://pkg.go.dev/github.com/merchantprotocol/trueup-go)

The official client for the [TrueUp API](https://trueup-cloud.merchantprotocol.workers.dev/docs). Send TrueUp two ledgers (a supplier statement and your receiving log, your books and the bank feed, invoices and payments) and it pairs every row, then tells you what's only on one side, what was counted twice and where the numbers disagree.

Go 1.21+. No dependencies beyond the standard library.

## Install

```bash
go get github.com/merchantprotocol/trueup-go
```

## Quickstart

Create an API key in the TrueUp dashboard (**API keys**), then set `TRUEUP_API_KEY`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/merchantprotocol/trueup-go"
)

func main() {
	client, err := trueup.NewClient() // reads TRUEUP_API_KEY
	if err != nil {
		log.Fatal(err)
	}
	// left: the side that bills or claims; right: the other side
	result, err := client.Reconcile(context.Background(), trueup.File("statement.csv"), trueup.File("receiving.csv"), nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Headline)
	// 7 of 8 rows of statement.csv paired with receiving.csv; 1 only in statement.csv, ...
	for _, f := range result.Findings {
		fmt.Println(f.Kind, f.Subject, f.Detail)
	}
	// qty_mismatch statement.csv:row 5 Qty 24 vs qty_received 20; ...
	// phantom statement.csv:row 6 no match on the other side
}
```

## Reconcile

A table is a file, file contents, or rows:

```go
trueup.File("books.csv")
trueup.Content("books.csv", csvBytes)
trueup.Rows("invoices", []trueup.Row{{"Invoice #": "INV-10101", "Date": "2026-06-09", "Total": "$2,999.31"}})
```

CSV, TSV, JSON and JSON Lines are read, and date and number formats are detected. Nothing about the columns is configured.

Not sure which file is which? `client.ReconcileFiles(ctx, []trueup.Table{trueup.File("a.csv"), trueup.File("b.csv")}, nil)` picks the pair and the sides.

**Reuse what was learned** by passing an earlier result's weights, and **answer the questions** TrueUp wasn't sure about:

```go
march, _ := client.Reconcile(ctx, trueup.File("march-statement.csv"), trueup.File("march-receiving.csv"), nil)
april, _ := client.Reconcile(ctx, trueup.File("april-statement.csv"), trueup.File("april-receiving.csv"),
	&trueup.ReconcileOptions{Weights: march.Details.Weights})

client.Reconcile(ctx, trueup.File("statement.csv"), trueup.File("receiving.csv"), &trueup.ReconcileOptions{
	Answers: &trueup.Answers{
		Same:      [][2]string{{"statement.csv:row 12", "receiving.csv:row 11"}},
		Different: [][2]string{{"statement.csv:row 3", "receiving.csv:row 9"}},
	},
})
```

Each call to `Reconcile` or `ReconcileFiles` counts as one analysis on your plan.

## Stored files, runs and saved models

Files uploaded to your team stay there (you'll also see them in the dashboard). Runs on stored files are kept, and what a run learned can be saved as a model:

```go
files, err := client.UploadFiles(ctx, trueup.File("statement.csv"), trueup.File("receiving.csv"))
statement, receiving := files[0], files[1]   // statement.Rows, .Columns, .Roles ("Qty": "number", ...)

res, err := client.ReconcileStored(ctx, trueup.StoredInput{LeftFileID: statement.ID, RightFileID: receiving.ID}, nil)
modelID, err := client.CreateModel(ctx, res.RunID, "Acme statements")

// Next month: apply what was learned.
client.ReconcileStored(ctx, trueup.StoredInput{FileIDs: []string{aprilStatement.ID, aprilReceiving.ID}},
	&trueup.StoredOptions{Model: modelID})
```

| Method | Returns |
|---|---|
| `UploadFiles(ctx, tables...)`, `ListFiles(ctx)`, `GetFile(ctx, id)` | `StoredFile`: `ID`, `Name`, `Rows`, `Columns`, `Roles` |
| `FileContent(ctx, id)` | the bytes, exactly as uploaded |
| `DeleteFile(ctx, id)` | |
| `ReconcileStored(ctx, StoredInput, *StoredOptions)` | `StoredResult`: a result plus `RunID` (one analysis) |
| `ListRuns(ctx, limit, before)` | `RunPage`: `Runs`, `HasMore`, newest first |
| `AllRuns(ctx, func(Run) bool)` | calls you for every run, paging for you |
| `GetRun(ctx, id)` | `RunDetail`: `Run`, `Result` |
| `CreateModel(ctx, runID, name)`, `ListModels(ctx)`, `GetModel(ctx, id)`, `DeleteModel(ctx, id)` | `GetModel` includes the `Weights` |

## Findings

| `Kind` | Meaning |
|---|---|
| `phantom` | Only on the left: billed or recorded, never matched |
| `unbilled` | Only on the right: received or paid, never billed |
| `duplicate`, `received_duplicate` | A copy of a row that's already paired |
| `qty_mismatch`, `price_change`, `amount_mismatch` | Paired rows whose numbers disagree |
| `unsure_pair` | A likely pair a person should confirm |

## Account and usage

```go
acct, _ := client.Account(ctx)
usage, _ := client.Usage(ctx)
plans, _ := client.Plans(ctx)
```

## Errors

API errors are `*trueup.Error` with `Status`, `Code` (the API's error code) and `Message`. Match kinds with `errors.Is`:

| Sentinel | When |
|---|---|
| `trueup.ErrAuthentication` | 401: missing, unknown or revoked key |
| `trueup.ErrInvalidRequest` | 400, 413, 415, 422: the request or the files need fixing (`unsupported_file`, `not_reconcilable`, ...) |
| `trueup.ErrRateLimited` | 429 `rate_limited`: retried automatically; `RetryAfter` |
| `trueup.ErrQuotaExceeded` | 429 `quota_exceeded`: the plan's monthly allowance is used up |
| `trueup.ErrServer` | 5xx: retried automatically |
| `trueup.ErrConnection` | the API couldn't be reached |

```go
if errors.Is(err, trueup.ErrQuotaExceeded) {
	fmt.Println("upgrade the plan")
}
```

## Configuration

```go
trueup.NewClient(
	trueup.WithAPIKey("tu_live_..."),       // default: TRUEUP_API_KEY
	trueup.WithBaseURL("https://..."),      // default: TRUEUP_BASE_URL, then the hosted API
	trueup.WithHTTPClient(&http.Client{Timeout: 5 * time.Minute}),
	trueup.WithMaxRetries(2),               // rate limits, 5xx and dropped connections
)
```

## Development

The tests run in Docker against the live API:

```bash
export TRUEUP_API_KEY=tu_live_...   # a key for a test team (each run uses 4 analyses)
just test                            # or: docker compose run --rm test
```

## License

MIT
