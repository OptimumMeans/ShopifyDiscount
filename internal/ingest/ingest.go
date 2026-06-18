// Package ingest resolves Shopify credentials and pulls discounts from the
// Admin API into the local store as a snapshot. It is shared by the CLI and the
// web dashboard so both create snapshots the same way.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/OptimumMeans/ShopifyDiscount/internal/csvimport"
	"github.com/OptimumMeans/ShopifyDiscount/internal/shopify"
	"github.com/OptimumMeans/ShopifyDiscount/internal/store"
)

// Creds holds Admin API credentials. ClientID/ClientSecret are only needed for
// the OAuth flow; pulling uses Token alone.
type Creds struct {
	Shop         string `json:"shop"`
	Token        string `json:"token,omitempty"`
	APIVersion   string `json:"apiVersion,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

const configName = "shopify.json"

// LoadConfig reads data/shopify.json if present (zero Creds if absent).
func LoadConfig(dataDir string) (Creds, error) {
	var c Creds
	raw, err := os.ReadFile(filepath.Join(dataDir, configName))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parsing %s: %w", configName, err)
	}
	return c, nil
}

// SaveConfig writes Creds to data/shopify.json (owner-only), merging over any
// existing file so unrelated fields are preserved.
func SaveConfig(dataDir string, c Creds) error {
	existing, _ := LoadConfig(dataDir)
	merged := Merge(c, existing)
	raw, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, configName), raw, 0o600)
}

// Merge keeps base's set fields, filling any empty ones from fallback.
func Merge(base, fallback Creds) Creds {
	if base.Shop == "" {
		base.Shop = fallback.Shop
	}
	if base.Token == "" {
		base.Token = fallback.Token
	}
	if base.APIVersion == "" {
		base.APIVersion = fallback.APIVersion
	}
	if base.ClientID == "" {
		base.ClientID = fallback.ClientID
	}
	if base.ClientSecret == "" {
		base.ClientSecret = fallback.ClientSecret
	}
	return base
}

// ResolveCreds layers credentials: explicit args win, then env vars, then
// data/shopify.json. Shop and Token must end up set.
func ResolveCreds(dataDir, shop, token, apiVersion string) (Creds, error) {
	c := Creds{Shop: shop, Token: token, APIVersion: apiVersion}
	if fc, err := LoadConfig(dataDir); err != nil {
		return c, err
	} else {
		c = Merge(c, fc)
	}
	c = Merge(c, Creds{
		Shop:       os.Getenv("SHOPIFY_SHOP"),
		Token:      os.Getenv("SHOPIFY_ADMIN_TOKEN"),
		APIVersion: os.Getenv("SHOPIFY_API_VERSION"),
	})
	if c.Shop == "" {
		return c, fmt.Errorf("no shop domain found. Provide -shop, set SHOPIFY_SHOP, or add data/shopify.json")
	}
	if c.Token == "" {
		return c, fmt.Errorf("no Admin API token found. Run `auth`, set SHOPIFY_ADMIN_TOKEN, or add a token to data/shopify.json")
	}
	return c, nil
}

// Result summarizes a pull.
type Result struct {
	Deduped    bool // true when the pull matched the latest snapshot (no-op)
	SnapshotID int64
	RowCount   int
	TakenAt    time.Time
	TotalUses  int
	TotalDelta int
	NewCodes   int
	Removed    []string
}

// Pull resolves credentials from dataDir/env and pulls into the store.
func Pull(ctx context.Context, st *store.Store, dataDir string) (*Result, error) {
	creds, err := ResolveCreds(dataDir, "", "", "")
	if err != nil {
		return nil, err
	}
	return PullWithCreds(ctx, st, creds)
}

// PullWithCreds fetches all discounts with the given credentials and stores them
// as a new snapshot. An unchanged pull (same content hash) is a no-op.
func PullWithCreds(ctx context.Context, st *store.Store, creds Creds) (*Result, error) {
	client := shopify.New(creds.Shop, creds.Token, creds.APIVersion)
	rows, err := client.FetchDiscounts(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no discounts returned (check the token has the read_discounts scope)")
	}

	hash := hashRows(rows)
	if existing, err := st.FindByHash(hash); err != nil {
		return nil, err
	} else if existing != nil {
		return &Result{Deduped: true, SnapshotID: existing.ID, RowCount: existing.RowCount, TakenAt: existing.TakenAt}, nil
	}

	now := time.Now()
	meta := store.SnapshotMeta{
		TakenAt:    now,
		ImportedAt: now,
		SourceFile: "shopify-api:" + creds.Shop,
		FileHash:   hash,
	}
	id, err := st.Import(meta, rows)
	if err != nil {
		return nil, err
	}

	res := &Result{SnapshotID: id, RowCount: len(rows), TakenAt: now}
	if discs, err := st.SnapshotDiscounts(id); err == nil {
		for _, d := range discs {
			res.TotalUses += d.TimesUsed
			res.TotalDelta += d.Delta
			if d.IsNew {
				res.NewCodes++
			}
		}
	}
	res.Removed, _ = st.DisappearedCodes(id)
	return res, nil
}

// PullRevenue fetches order discounts since the last sinceDays days and stores
// per-code revenue totals. Requires the token to have the read_orders scope.
func PullRevenue(ctx context.Context, st *store.Store, dataDir string, sinceDays int) (int, error) {
	creds, err := ResolveCreds(dataDir, "", "", "")
	if err != nil {
		return 0, err
	}
	var sinceISO string
	if sinceDays > 0 {
		sinceISO = time.Now().AddDate(0, 0, -sinceDays).UTC().Format(time.RFC3339)
	}
	client := shopify.New(creds.Shop, creds.Token, creds.APIVersion)
	discounts, err := client.FetchOrderDiscounts(ctx, sinceISO)
	if err != nil {
		return 0, err
	}
	rev := make([]store.Revenue, 0, len(discounts))
	for _, d := range discounts {
		rev = append(rev, store.Revenue{
			Name:          d.Code,
			TotalDiscount: d.TotalDiscount,
			OrderCount:    d.OrderCount,
			Currency:      d.Currency,
		})
	}
	if err := st.SetRevenue(rev, time.Now()); err != nil {
		return 0, err
	}
	return len(rev), nil
}

// hashRows produces a content hash so an unchanged pull dedupes against the
// previous snapshot (mirroring import's file-hash idempotency).
func hashRows(rows []csvimport.Row) string {
	sorted := append([]csvimport.Row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	for _, r := range sorted {
		fmt.Fprintf(h, "%s|%g|%s|%s|%d|%s|%s|%s\n",
			r.Name, r.Value, r.ValueType, r.Type, r.TimesUsed, r.Status, r.StartAt, r.EndAt)
	}
	return hex.EncodeToString(h.Sum(nil))
}
