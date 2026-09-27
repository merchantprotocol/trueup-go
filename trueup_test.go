// Integration tests against the live TrueUp API. Need TRUEUP_API_KEY (and optionally TRUEUP_BASE_URL).
// Each full run uses 6 analyses. Run in Docker: `just test` (or `docker compose run --rm test`).
package trueup_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/merchantprotocol/trueup-go"
)

func live(t *testing.T) *trueup.Client {
	t.Helper()
	if os.Getenv("TRUEUP_API_KEY") == "" {
		t.Skip("needs TRUEUP_API_KEY")
	}
	c, err := trueup.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func rows(t *testing.T, path string) []trueup.Row {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	out := []trueup.Row{}
	for _, r := range recs[1:] {
		row := trueup.Row{}
		for i, col := range recs[0] {
			row[col] = r[i]
		}
		out = append(out, row)
	}
	return out
}

func TestMissingAPIKeyFailsBeforeAnyRequest(t *testing.T) {
	t.Setenv("TRUEUP_API_KEY", "")
	_, err := trueup.NewClient()
	var e *trueup.Error
	if !errors.As(err, &e) || e.Code != "missing_api_key" || !errors.Is(err, trueup.ErrAuthentication) {
		t.Fatalf("want missing_api_key, got %v", err)
	}
}

func TestAccountUsagePlans(t *testing.T) {
	c := live(t)
	ctx := context.Background()
	acct, err := c.Account(ctx)
	if err != nil || !strings.HasPrefix(acct.Key.Prefix, "tu_live_") {
		t.Fatalf("account: %v %+v", err, acct)
	}
	usage, err := c.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range usage.Metrics {
		found = found || m.Metric == "analyses"
	}
	plans, err := c.Plans(ctx)
	if err != nil || !found || len(plans) == 0 {
		t.Fatalf("usage/plans: %v %v %d", err, found, len(plans))
	}
}

func TestReconcileFilesThenRowsWithSavedWeights(t *testing.T) {
	c := live(t)
	ctx := context.Background()
	res, err := c.Reconcile(ctx, trueup.File("testdata/statement.csv"), trueup.File("testdata/receiving.csv"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Analysis != "reconcile" || res.Stats["paired"] != 7 || len(res.Findings) != 2 {
		t.Fatalf("unexpected result: %s", res.Headline)
	}
	got := [][2]string{{res.Findings[0].Kind, res.Findings[0].Subject}, {res.Findings[1].Kind, res.Findings[1].Subject}}
	want := [][2]string{{"qty_mismatch", "statement.csv:row 5"}, {"phantom", "statement.csv:row 6"}}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("findings: got %v want %v", got, want)
	}
	if res.Findings[1].Amount == nil || *res.Findings[1].Amount != 43.2 {
		t.Fatalf("phantom amount: %v", res.Findings[1].Amount)
	}
	if !strings.Contains(string(res.Details.Weights), `"trueup.match-weights"`) {
		t.Fatal("no weights")
	}

	again, err := c.Reconcile(ctx, trueup.Rows("statement.csv", rows(t, "testdata/statement.csv")),
		trueup.Rows("receiving.csv", rows(t, "testdata/receiving.csv")), &trueup.ReconcileOptions{Weights: res.Details.Weights})
	if err != nil {
		t.Fatal(err)
	}
	if again.Stats["paired"] != 7 || again.Details.Model["learned"] != false {
		t.Fatalf("with weights: %s learned=%v", again.Headline, again.Details.Model["learned"])
	}
}

func TestErrorsAreTyped(t *testing.T) {
	c := live(t)
	ctx := context.Background()
	bad, _ := trueup.NewClient(trueup.WithAPIKey("tu_live_" + strings.Repeat("x", 40)))
	_, err := bad.Account(ctx)
	var e *trueup.Error
	if !errors.As(err, &e) || e.Status != 401 || e.Code != "invalid_api_key" || !errors.Is(err, trueup.ErrAuthentication) {
		t.Fatalf("want 401 invalid_api_key, got %v", err)
	}
	_, err = c.Reconcile(ctx, trueup.File("testdata/statement.csv"), trueup.Content("scan.pdf", []byte("%PDF-1.4")), nil)
	if !errors.As(err, &e) || e.Status != 422 || e.Code != "unsupported_file" || !errors.Is(err, trueup.ErrInvalidRequest) {
		t.Fatalf("want 422 unsupported_file, got %v", err)
	}
}

func TestStoredFilesRunsAndModels(t *testing.T) {
	c := live(t)
	ctx := context.Background()
	files, err := c.UploadFiles(ctx, trueup.File("testdata/statement.csv"), trueup.File("testdata/receiving.csv"))
	if err != nil {
		t.Fatal(err)
	}
	statement, receiving := files[0], files[1]
	defer func() {
		_ = c.DeleteFile(ctx, statement.ID)
		_ = c.DeleteFile(ctx, receiving.ID)
		if _, err := c.GetFile(ctx, statement.ID); !errors.Is(err, trueup.ErrNotFound) {
			t.Errorf("deleted file: want not found, got %v", err)
		}
	}()
	if statement.Rows == nil || *statement.Rows != 8 || statement.Roles["Qty"] != "number" {
		t.Fatalf("statement: %+v", statement)
	}
	if f, err := c.GetFile(ctx, receiving.ID); err != nil || f.Name != "receiving.csv" {
		t.Fatalf("get file: %v %v", f, err)
	}
	content, err := c.FileContent(ctx, statement.ID)
	want, _ := os.ReadFile("testdata/statement.csv")
	if err != nil || !bytes.Equal(content, want) {
		t.Fatalf("content differs: %v", err)
	}

	res, err := c.ReconcileStored(ctx, trueup.StoredInput{LeftFileID: statement.ID, RightFileID: receiving.ID}, nil)
	if err != nil || res.Stats["paired"] != 7 || !strings.HasPrefix(res.RunID, "run_") {
		t.Fatalf("reconcile stored: %v %v", res, err)
	}
	run, err := c.GetRun(ctx, res.RunID)
	if err != nil || run.Run.Status != "done" || run.Result.Stats["paired"] != 7 {
		t.Fatalf("get run: %v %v", run, err)
	}
	page, err := c.ListRuns(ctx, 1, "")
	if err != nil || len(page.Runs) != 1 || !page.HasMore {
		t.Fatalf("runs page: %v %v", page, err)
	}
	next, err := c.ListRuns(ctx, 1, page.Runs[0].ID)
	if err != nil || next.Runs[0].ID == page.Runs[0].ID {
		t.Fatalf("next page: %v %v", next, err)
	}

	modelID, err := c.CreateModel(ctx, res.RunID, "sdk test")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.GetModel(ctx, modelID)
	if err != nil || !strings.Contains(string(m.Weights), `"trueup.match-weights"`) {
		t.Fatalf("get model: %v", err)
	}
	again, err := c.ReconcileStored(ctx, trueup.StoredInput{FileIDs: []string{statement.ID, receiving.ID}}, &trueup.StoredOptions{Model: modelID})
	if err != nil || again.Details.Model["learned"] != false {
		t.Fatalf("with model: %v", err)
	}
	if err := c.DeleteModel(ctx, modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetModel(ctx, modelID); !errors.Is(err, trueup.ErrNotFound) {
		t.Fatalf("deleted model: want not found, got %v", err)
	}
}

func TestMatchTwoListsThenReuseTheLearning(t *testing.T) {
	c := live(t)
	ctx := context.Background()
	want := [][2]string{{"1", "1"}, {"2", "2"}, {"3", "3"}, {"4", "5"}}
	pairs := func(r *trueup.MatchResult) [][2]string {
		out := [][2]string{}
		for _, p := range r.Details.Pairs {
			out = append(out, [2]string{p.LeftID, p.RightID})
		}
		return out
	}
	res, err := c.Match(ctx, trueup.File("testdata/invoice.csv"), trueup.File("testdata/catalog.csv"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Analysis != "match" || fmt.Sprint(pairs(res)) != fmt.Sprint(want) {
		t.Fatalf("match: %s %v", res.Headline, pairs(res))
	}
	var only []string
	for _, f := range res.Findings {
		if f.Kind == "only_left" {
			only = append(only, f.Subject)
		}
	}
	if fmt.Sprint(only) != "[5]" {
		t.Fatalf("only_left: %v", only)
	}
	again, err := c.Match(ctx, trueup.Rows("invoice.csv", rows(t, "testdata/invoice.csv")), trueup.Rows("catalog.csv", rows(t, "testdata/catalog.csv")),
		&trueup.MatchOptions{Weights: res.Details.Weights})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pairs(again)) != fmt.Sprint(want) || again.Details.Model.Learned {
		t.Fatalf("with weights: %v learned=%v", pairs(again), again.Details.Model.Learned)
	}
}
