// Package trueup is the official client for the TrueUp API.
//
// Send TrueUp two ledgers (a supplier statement and your receiving log, your books and the bank feed, invoices and
// payments) and it pairs every row, then tells you what's only on one side, what was counted twice and where the
// numbers disagree.
//
//	client, err := trueup.NewClient() // reads TRUEUP_API_KEY
//	result, err := client.Reconcile(ctx, trueup.File("statement.csv"), trueup.File("receiving.csv"), nil)
//	for _, f := range result.Findings {
//		fmt.Println(f.Kind, f.Subject, f.Detail)
//	}
package trueup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Version of this SDK.
const Version = "0.2.0"

// DefaultBaseURL is the hosted TrueUp API.
const DefaultBaseURL = "https://trueup-cloud.merchantprotocol.workers.dev"

// Row is one row of a table: column name -> value (string, number, bool or nil).
type Row map[string]any

// Table is one table to reconcile: a file, file contents, or rows. Name is how findings refer to its rows
// ("statement.csv:row 5"). Build one with File, Content or Rows.
type Table struct {
	Name    string
	path    string
	content []byte
	rows    []Row
	isRows  bool
}

// File is a table read from a file on disk when the request is sent.
func File(path string) Table { return Table{Name: filepath.Base(path), path: path} }

// Content is a table from file contents; name's extension says the format (.csv, .tsv, .json, .jsonl).
func Content(name string, content []byte) Table { return Table{Name: name, content: content} }

// Rows is a table from rows.
func Rows(name string, rows []Row) Table { return Table{Name: name, rows: rows, isRows: true} }

func (t Table) file() (string, []byte, error) {
	if t.isRows {
		b, err := json.Marshal(t.rows)
		stem := strings.TrimSuffix(t.Name, filepath.Ext(t.Name))
		return stem + ".json", b, err
	}
	if t.path != "" {
		b, err := os.ReadFile(t.path)
		return t.Name, b, err
	}
	return t.Name, t.content, nil
}

// Answers are decisions a person made about pairs: [left row, right row], e.g.
// {"statement.csv:row 3", "receiving.csv:row 4"}.
type Answers struct {
	Same      [][2]string `json:"same,omitempty"`
	Different [][2]string `json:"different,omitempty"`
}

// ReconcileOptions are optional settings for a reconcile call.
type ReconcileOptions struct {
	// Weights is Details.Weights from an earlier result: apply what was learned then instead of learning again.
	Weights json.RawMessage
	Answers *Answers
}

// Finding is one thing TrueUp found.
type Finding struct {
	// Kind: phantom, unbilled, duplicate, received_duplicate, qty_mismatch, price_change, amount_mismatch, unsure_pair.
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
	// Confidence is how likely the pairing is right; nil for rows with no pair.
	Confidence *float64       `json:"confidence"`
	Status     string         `json:"status"`
	Amount     *float64       `json:"amount"`
	Provenance []string       `json:"provenance"`
	Data       map[string]any `json:"data"`
}

// Pair is one pairing of a left row with a right row.
type Pair struct {
	Left       string  `json:"left"`
	Right      string  `json:"right"`
	Confidence float64 `json:"confidence"`
}

// Details are what the run learned and every pair it made.
type Details struct {
	Model   map[string]any  `json:"model"`
	Weights json.RawMessage `json:"weights"`
	Pairs   []Pair          `json:"pairs"`
}

// ReconcileResult is the answer to a reconcile call.
type ReconcileResult struct {
	Analysis string             `json:"analysis"`
	Title    string             `json:"title"`
	Headline string             `json:"headline"`
	Stats    map[string]float64 `json:"stats"`
	Findings []Finding          `json:"findings"`
	Details  Details            `json:"details"`
	Inputs   []string           `json:"inputs"`
	Engine   string             `json:"engine"`
}

// Account is the team, plan and key behind an API key.
type Account struct {
	Team struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	Plan *struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"plan"`
	Key struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Prefix string `json:"prefix"`
	} `json:"key"`
}

// UsageMetric is this month's use of one metric.
type UsageMetric struct {
	Metric    string `json:"metric"`
	Label     string `json:"label"`
	Used      int    `json:"used"`
	Included  int    `json:"included"`
	Remaining int    `json:"remaining"`
	HardCap   bool   `json:"hard_cap"`
	Overage   int    `json:"overage"`
}

// Usage is this month's usage for a team.
type Usage struct {
	Period   string        `json:"period"`
	ResetsAt string        `json:"resets_at"`
	Metrics  []UsageMetric `json:"metrics"`
}

// Plan is a plan a team can be on.
type Plan struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceCents  int    `json:"price_cents"`
	Interval    string `json:"interval"`
	Purchasable bool   `json:"purchasable"`
	Limits      []struct {
		Metric               string `json:"metric"`
		Included             int    `json:"included"`
		HardCap              bool   `json:"hard_cap"`
		OverageMicrosPerUnit *int   `json:"overage_micros_per_unit"`
	} `json:"limits"`
}

// StoredFile is a file stored in the team (uploaded through the API or the dashboard).
type StoredFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Kind is "table" or "document".
	Kind    string   `json:"kind"`
	Rows    *int     `json:"rows"`
	Columns []string `json:"columns"`
	// Roles is what TrueUp read each column as: "date", "number", "text", ...
	Roles     map[string]string `json:"roles"`
	CreatedAt string            `json:"created_at"`
}

// Run is a run on stored files, from the API or the dashboard.
type Run struct {
	ID       string `json:"id"`
	Analysis string `json:"analysis"`
	// Status is "done" or "failed".
	Status string `json:"status"`
	// Via is "api" or "portal".
	Via    string   `json:"via"`
	Inputs []string `json:"inputs"`
	Model  *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"model"`
	Headline *string            `json:"headline"`
	Stats    map[string]float64 `json:"stats"`
	// Findings is how many findings the run has.
	Findings  *int    `json:"findings"`
	Error     *string `json:"error"`
	CreatedAt string  `json:"created_at"`
}

// RunPage is one page of runs, newest first.
type RunPage struct {
	Runs    []Run `json:"runs"`
	HasMore bool  `json:"has_more"`
}

// RunDetail is one run and its full result (nil if the run failed).
type RunDetail struct {
	Run    Run              `json:"run"`
	Result *ReconcileResult `json:"result"`
}

// Model is a saved model: what a run learned, reusable on next month's files.
type Model struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Analysis    string  `json:"analysis"`
	SourceRunID *string `json:"source_run_id"`
	CreatedAt   string  `json:"created_at"`
	// Weights is set only by GetModel.
	Weights json.RawMessage `json:"weights,omitempty"`
}

// StoredInput names stored files by id: LeftFileID and RightFileID, or FileIDs for TrueUp to pick the pair.
type StoredInput struct {
	LeftFileID  string
	RightFileID string
	FileIDs     []string
}

// StoredOptions are optional settings for ReconcileStored.
type StoredOptions struct {
	// Model is a saved model id: apply what it learned instead of learning again.
	Model   string
	Answers *Answers
}

// StoredResult is a reconcile result on stored files, with the id of the run that was kept.
type StoredResult struct {
	ReconcileResult
	RunID string `json:"run_id"`
}

// ---------------------------------------------------------------- errors

// Error is any error the API returned, or a failure to reach it. Code is the API's error code; branch on it (or
// use errors.Is with the sentinel kinds below).
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	Body       []byte
	kind       error
}

func (e *Error) Error() string { return fmt.Sprintf("trueup: %s (%d %s)", e.Message, e.Status, e.Code) }

// Unwrap lets errors.Is(err, trueup.ErrQuotaExceeded) and friends work.
func (e *Error) Unwrap() error { return e.kind }

// Kinds of error, for errors.Is.
var (
	ErrAuthentication = errors.New("authentication")  // 401: missing, unknown or revoked API key
	ErrInvalidRequest = errors.New("invalid request") // 400, 413, 415, 422: the request or the files need fixing
	ErrNotFound       = errors.New("not found")       // 404, 405
	ErrRateLimited    = errors.New("rate limited")    // 429 rate_limited
	ErrQuotaExceeded  = errors.New("quota exceeded")  // 429 quota_exceeded: retrying won't help
	ErrServer         = errors.New("server error")    // 5xx
	ErrConnection     = errors.New("connection")      // the API couldn't be reached
)

func errorFor(status int, code, message string, body []byte, retryAfter string) *Error {
	e := &Error{Status: status, Code: code, Message: message, Body: body}
	switch {
	case status == 401:
		e.kind = ErrAuthentication
	case status == 429 && code == "quota_exceeded":
		e.kind = ErrQuotaExceeded
	case status == 429:
		e.kind = ErrRateLimited
		if s, err := strconv.ParseFloat(retryAfter, 64); err == nil {
			e.RetryAfter = time.Duration(s * float64(time.Second))
		}
	case status == 404 || status == 405:
		e.kind = ErrNotFound
	case status >= 500:
		e.kind = ErrServer
	default:
		e.kind = ErrInvalidRequest
	}
	return e
}

// ---------------------------------------------------------------- client

// Client talks to the TrueUp API. It is safe for concurrent use.
type Client struct {
	apiKey     string
	BaseURL    string
	HTTPClient *http.Client
	MaxRetries int
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the API key (default: TRUEUP_API_KEY).
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithBaseURL sets the API address (default: TRUEUP_BASE_URL, then the hosted API).
func WithBaseURL(url string) Option { return func(c *Client) { c.BaseURL = url } }

// WithHTTPClient sets the HTTP client (default: 300 s timeout; big ledgers take a while).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.HTTPClient = h } }

// WithMaxRetries sets retries for rate limits, server errors and dropped connections (default 2).
func WithMaxRetries(n int) Option { return func(c *Client) { c.MaxRetries = n } }

// NewClient returns a client. It fails if there is no API key.
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{
		apiKey:     os.Getenv("TRUEUP_API_KEY"),
		BaseURL:    os.Getenv("TRUEUP_BASE_URL"),
		HTTPClient: &http.Client{Timeout: 300 * time.Second},
		MaxRetries: 2,
	}
	for _, o := range opts {
		o(c)
	}
	if c.apiKey == "" {
		return nil, &Error{Code: "missing_api_key", kind: ErrAuthentication,
			Message: "no API key: use WithAPIKey or set TRUEUP_API_KEY (create one in the TrueUp dashboard under API keys)"}
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c, nil
}

// Account returns the team, plan and key behind the client's API key.
func (c *Client) Account(ctx context.Context) (*Account, error) {
	var out Account
	return &out, c.do(ctx, "GET", "/v1/account", nil, "", &out)
}

// Usage returns this month's usage for the key's team.
func (c *Client) Usage(ctx context.Context) (*Usage, error) {
	var out Usage
	return &out, c.do(ctx, "GET", "/v1/usage", nil, "", &out)
}

// Plans returns the plans a team can be on.
func (c *Client) Plans(ctx context.Context) ([]Plan, error) {
	var out struct {
		Plans []Plan `json:"plans"`
	}
	err := c.do(ctx, "GET", "/v1/plans", nil, "", &out)
	return out.Plans, err
}

// Reconcile reconciles two tables. left is the side that bills or claims (a statement, your books), right the
// other side (receiving log, bank feed). opts may be nil. Counts as one analysis.
func (c *Client) Reconcile(ctx context.Context, left, right Table, opts *ReconcileOptions) (*ReconcileResult, error) {
	if opts == nil {
		opts = &ReconcileOptions{}
	}
	if left.isRows && right.isRows {
		body := map[string]any{
			"left":  map[string]any{"name": left.Name, "rows": left.rows},
			"right": map[string]any{"name": right.Name, "rows": right.rows},
		}
		if len(opts.Weights) > 0 {
			body["weights"] = opts.Weights
		}
		if opts.Answers != nil {
			body["answers"] = opts.Answers
		}
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		var out ReconcileResult
		return &out, c.do(ctx, "POST", "/v1/reconcile", b, "application/json", &out)
	}
	return c.upload(ctx, []string{"left", "right"}, []Table{left, right}, opts)
}

// ReconcileFiles sends two or more files; TrueUp picks the pair to reconcile and which side is which. opts may be
// nil. One analysis.
func (c *Client) ReconcileFiles(ctx context.Context, files []Table, opts *ReconcileOptions) (*ReconcileResult, error) {
	if opts == nil {
		opts = &ReconcileOptions{}
	}
	fields := make([]string, len(files))
	for i := range files {
		fields[i] = "files"
	}
	return c.upload(ctx, fields, files, opts)
}

// UploadFiles stores one or more files in the team. Each comes back with its ID, Rows, Columns and Roles.
func (c *Client) UploadFiles(ctx context.Context, files ...Table) ([]StoredFile, error) {
	if len(files) == 0 {
		return nil, &Error{Code: "invalid_request", Message: "pass at least one file to upload", kind: ErrInvalidRequest}
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, t := range files {
		name, content, err := t.file()
		if err != nil {
			return nil, err
		}
		part, err := w.CreateFormFile("file", name)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(content); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	var out struct {
		Files []StoredFile `json:"files"`
	}
	err := c.do(ctx, "POST", "/v1/files", buf.Bytes(), w.FormDataContentType(), &out)
	return out.Files, err
}

// ListFiles returns the team's stored files.
func (c *Client) ListFiles(ctx context.Context) ([]StoredFile, error) {
	var out struct {
		Files []StoredFile `json:"files"`
	}
	err := c.do(ctx, "GET", "/v1/files", nil, "", &out)
	return out.Files, err
}

// GetFile returns one stored file.
func (c *Client) GetFile(ctx context.Context, id string) (*StoredFile, error) {
	var out struct {
		File StoredFile `json:"file"`
	}
	if err := c.do(ctx, "GET", "/v1/files/"+url.PathEscape(id), nil, "", &out); err != nil {
		return nil, err
	}
	return &out.File, nil
}

// FileContent returns a stored file's bytes, exactly as uploaded.
func (c *Client) FileContent(ctx context.Context, id string) ([]byte, error) {
	var out []byte
	err := c.do(ctx, "GET", "/v1/files/"+url.PathEscape(id)+"/content", nil, "", &out)
	return out, err
}

// DeleteFile deletes a stored file.
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/files/"+url.PathEscape(id), nil, "", nil)
}

// ReconcileStored reconciles files already stored in the team, by id. The run is kept: its id is RunID.
// opts may be nil. One analysis.
func (c *Client) ReconcileStored(ctx context.Context, in StoredInput, opts *StoredOptions) (*StoredResult, error) {
	body := map[string]any{}
	switch {
	case len(in.FileIDs) > 0:
		body["file_ids"] = in.FileIDs
	case in.LeftFileID != "" && in.RightFileID != "":
		body["left_file_id"], body["right_file_id"] = in.LeftFileID, in.RightFileID
	default:
		return nil, &Error{Code: "invalid_request", Message: "set LeftFileID and RightFileID, or FileIDs", kind: ErrInvalidRequest}
	}
	if opts != nil && opts.Model != "" {
		body["model"] = opts.Model
	}
	if opts != nil && opts.Answers != nil {
		body["answers"] = opts.Answers
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out StoredResult
	if err := c.do(ctx, "POST", "/v1/reconcile", b, "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRuns returns one page of runs on stored files, newest first. limit is 1-100 (0 for the default of 100);
// before is a run id ("" for the newest).
func (c *Client) ListRuns(ctx context.Context, limit int, before string) (*RunPage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before != "" {
		q.Set("before", before)
	}
	path := "/v1/runs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out RunPage
	if err := c.do(ctx, "GET", path, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AllRuns calls fn for every run, newest first, fetching page after page. Return false from fn to stop.
func (c *Client) AllRuns(ctx context.Context, fn func(Run) bool) error {
	before := ""
	for {
		page, err := c.ListRuns(ctx, 100, before)
		if err != nil {
			return err
		}
		for _, r := range page.Runs {
			if !fn(r) {
				return nil
			}
		}
		if !page.HasMore || len(page.Runs) == 0 {
			return nil
		}
		before = page.Runs[len(page.Runs)-1].ID
	}
}

// GetRun returns one run and its full result.
func (c *Client) GetRun(ctx context.Context, id string) (*RunDetail, error) {
	var out RunDetail
	if err := c.do(ctx, "GET", "/v1/runs/"+url.PathEscape(id), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateModel saves what a run learned as a model and returns the model's id.
func (c *Client) CreateModel(ctx context.Context, runID, name string) (string, error) {
	body := map[string]string{"run_id": runID}
	if name != "" {
		body["name"] = name
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	err = c.do(ctx, "POST", "/v1/models", b, "application/json", &out)
	return out.ID, err
}

// ListModels returns the team's saved models.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	var out struct {
		Models []Model `json:"models"`
	}
	err := c.do(ctx, "GET", "/v1/models", nil, "", &out)
	return out.Models, err
}

// GetModel returns one saved model, including its Weights.
func (c *Client) GetModel(ctx context.Context, id string) (*Model, error) {
	var out struct {
		Model Model `json:"model"`
	}
	if err := c.do(ctx, "GET", "/v1/models/"+url.PathEscape(id), nil, "", &out); err != nil {
		return nil, err
	}
	return &out.Model, nil
}

// DeleteModel deletes a saved model.
func (c *Client) DeleteModel(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/models/"+url.PathEscape(id), nil, "", nil)
}

func (c *Client) upload(ctx context.Context, fields []string, tables []Table, opts *ReconcileOptions) (*ReconcileResult, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for i, t := range tables {
		name, content, err := t.file()
		if err != nil {
			return nil, err
		}
		part, err := w.CreateFormFile(fields[i], name)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(content); err != nil {
			return nil, err
		}
	}
	if len(opts.Weights) > 0 {
		_ = w.WriteField("weights", string(opts.Weights))
	}
	if opts.Answers != nil {
		b, err := json.Marshal(opts.Answers)
		if err != nil {
			return nil, err
		}
		_ = w.WriteField("answers", string(b))
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	var out ReconcileResult
	return &out, c.do(ctx, "POST", "/v1/reconcile", buf.Bytes(), w.FormDataContentType(), &out)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, contentType string, out any) error {
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "trueup-go/"+Version)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res, err := c.HTTPClient.Do(req)
		if err != nil {
			if ctx.Err() == nil && attempt < c.MaxRetries {
				sleep(ctx, backoff(attempt))
				continue
			}
			return &Error{Code: "connection_error", kind: ErrConnection,
				Message: fmt.Sprintf("couldn't reach TrueUp at %s: %v", c.BaseURL, err)}
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return &Error{Code: "connection_error", kind: ErrConnection, Message: err.Error()}
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if raw, ok := out.(*[]byte); ok {
				*raw = data
				return nil
			}
			if out == nil {
				return nil
			}
			return json.Unmarshal(data, out)
		}
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		code, msg := envelope.Error.Code, envelope.Error.Message
		if code == "" {
			code = fmt.Sprintf("http_%d", res.StatusCode)
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		e := errorFor(res.StatusCode, code, msg, data, res.Header.Get("Retry-After"))
		if (e.kind == ErrRateLimited || e.kind == ErrServer) && attempt < c.MaxRetries {
			wait := backoff(attempt)
			if e.RetryAfter > 0 {
				wait = e.RetryAfter
			}
			sleep(ctx, wait)
			continue
		}
		return e
	}
}

func backoff(attempt int) time.Duration {
	s := math.Min(30, math.Pow(2, float64(attempt))) * (0.5 + rand.Float64()/2)
	return time.Duration(s * float64(time.Second))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
