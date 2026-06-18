// Command shopifydiscount archives Shopify discount exports and tracks how each
// code's usage changes over time.
//
// Usage:
//
//	shopifydiscount import <export.csv> [-data DIR] [-as-of TIME] [-no-archive]
//	shopifydiscount serve [-data DIR] [-addr HOST:PORT]
//	shopifydiscount report [-data DIR]
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OptimumMeans/ShopifyDiscount/internal/csvimport"
	"github.com/OptimumMeans/ShopifyDiscount/internal/shopify"
	"github.com/OptimumMeans/ShopifyDiscount/internal/store"
	"github.com/OptimumMeans/ShopifyDiscount/internal/web"

	"net/http"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "import":
		err = cmdImport(os.Args[2:])
	case "auth":
		err = cmdAuth(os.Args[2:])
	case "pull":
		err = cmdPull(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `shopifydiscount — archive Shopify discount exports and track usage over time

Commands:
  import <export.csv>   Import a Shopify discounts export as a timestamped snapshot
  auth                  One-time OAuth: mint an Admin API token from app credentials
  pull                  Pull discounts directly from the Shopify Admin API as a snapshot
  serve                 Start the local web dashboard
  report                Print a text summary of the latest snapshot

Common flags:
  -data DIR             Data directory for the database + archived CSVs (default "data")

import flags:
  -as-of TIME           Override the snapshot time (RFC3339 or "2006-01-02"); defaults to the file's modified time
  -no-archive           Do not copy the raw CSV into the archive folder

auth flags:
  -shop DOMAIN          Shop domain, e.g. your-store.myshopify.com
  -client-id ID         App client ID (API key)
  -client-secret SECRET App client secret
  -scopes LIST          Comma-separated scopes (default "read_discounts")
  -port N               Local OAuth callback port (default 3456)
  The app must allow redirect URL http://localhost:<port>/auth/callback.
  Saves the minted token to data/shopify.json.

pull flags:
  -shop DOMAIN          Shop domain, e.g. your-store.myshopify.com
  -token TOKEN          Admin API access token (needs read_discounts)
  -api-version VER      Admin API version (default "2025-10")
  Credentials resolve from these flags, then env vars
  (SHOPIFY_SHOP / SHOPIFY_ADMIN_TOKEN / SHOPIFY_API_VERSION),
  then a git-ignored data/shopify.json {"shop","token","apiVersion"}.

serve flags:
  -addr HOST:PORT       Listen address (default "127.0.0.1:8080")
`)
}

// openStore opens the database under dataDir, creating the directory if needed.
func openStore(dataDir string) (*store.Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dataDir, "discounts.db"))
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	asOf := fs.String("as-of", "", "override snapshot time (RFC3339 or 2006-01-02)")
	noArchive := fs.Bool("no-archive", false, "do not copy the raw CSV into the archive")
	fs.Parse(args)

	if fs.NArg() != 1 {
		return fmt.Errorf("import requires exactly one CSV path")
	}
	csvPath := fs.Arg(0)

	parsed, err := csvimport.ParseFile(csvPath)
	if err != nil {
		return err
	}

	hash, err := hashFile(csvPath)
	if err != nil {
		return err
	}

	takenAt, err := resolveTakenAt(*asOf, csvPath)
	if err != nil {
		return err
	}

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	if existing, err := st.FindByHash(hash); err != nil {
		return err
	} else if existing != nil {
		fmt.Printf("Already imported (snapshot #%d, taken %s). Nothing to do.\n",
			existing.ID, existing.TakenAt.Local().Format("Jan 2, 2006 15:04"))
		return nil
	}

	meta := store.SnapshotMeta{
		TakenAt:    takenAt,
		ImportedAt: time.Now(),
		SourceFile: filepath.Base(csvPath),
		FileHash:   hash,
	}
	id, err := st.Import(meta, parsed.Rows)
	if err != nil {
		return err
	}

	if !*noArchive {
		if err := archiveCSV(csvPath, *dataDir, takenAt); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not archive CSV:", err)
		}
	}

	fmt.Printf("Imported snapshot #%d — %d codes, taken %s.\n",
		id, len(parsed.Rows), takenAt.Local().Format("Jan 2, 2006 15:04"))
	printDelta(st, id)
	return nil
}

// printDelta prints a short summary of what changed versus the previous snapshot.
func printDelta(st *store.Store, snapshotID int64) {
	discs, err := st.SnapshotDiscounts(snapshotID)
	if err != nil {
		return
	}
	gone, _ := st.DisappearedCodes(snapshotID)
	var totalDelta, newCount, totalUses int
	for _, d := range discs {
		totalUses += d.TimesUsed
		totalDelta += d.Delta
		if d.IsNew {
			newCount++
		}
	}
	fmt.Printf("  Total uses: %d  (%+d since previous snapshot)\n", totalUses, totalDelta)
	if newCount > 0 {
		fmt.Printf("  New codes: %d\n", newCount)
	}
	if len(gone) > 0 {
		fmt.Printf("  Removed since previous: %d  (%s)\n", len(gone), strings.Join(gone, ", "))
	}
}

// shopifyCreds holds resolved Admin API credentials. ClientID/ClientSecret are
// only needed for the `auth` OAuth flow; `pull` uses Token alone.
type shopifyCreds struct {
	Shop         string `json:"shop"`
	Token        string `json:"token,omitempty"`
	APIVersion   string `json:"apiVersion,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// resolveCreds layers credentials: flags win, then env vars, then data/shopify.json.
func resolveCreds(dataDir, shop, token, apiVersion string) (shopifyCreds, error) {
	c := shopifyCreds{Shop: shop, Token: token, APIVersion: apiVersion}

	// Config file (lowest precedence).
	if raw, err := os.ReadFile(filepath.Join(dataDir, "shopify.json")); err == nil {
		var fc shopifyCreds
		if err := json.Unmarshal(raw, &fc); err != nil {
			return c, fmt.Errorf("parsing data/shopify.json: %w", err)
		}
		c = firstNonEmpty(c, fc)
	}
	// Env vars (middle precedence).
	c = firstNonEmpty(c, shopifyCreds{
		Shop:       os.Getenv("SHOPIFY_SHOP"),
		Token:      os.Getenv("SHOPIFY_ADMIN_TOKEN"),
		APIVersion: os.Getenv("SHOPIFY_API_VERSION"),
	})
	if c.Shop == "" {
		return c, fmt.Errorf("no shop domain found. Provide -shop, set SHOPIFY_SHOP, or add data/shopify.json")
	}
	if c.Token == "" {
		return c, fmt.Errorf("no Admin API token found. Provide -token, set SHOPIFY_ADMIN_TOKEN, or add data/shopify.json")
	}
	return c, nil
}

// firstNonEmpty keeps base's fields, filling any empty ones from fallback.
func firstNonEmpty(base, fallback shopifyCreds) shopifyCreds {
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

// loadConfig reads data/shopify.json if present (empty creds if absent).
func loadConfig(dataDir string) (shopifyCreds, error) {
	var c shopifyCreds
	raw, err := os.ReadFile(filepath.Join(dataDir, "shopify.json"))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parsing data/shopify.json: %w", err)
	}
	return c, nil
}

// saveConfig writes creds to data/shopify.json with owner-only permissions,
// merging over any existing file so unrelated fields are preserved.
func saveConfig(dataDir string, c shopifyCreds) error {
	existing, _ := loadConfig(dataDir)
	merged := firstNonEmpty(c, existing)
	raw, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, "shopify.json"), raw, 0o600)
}

func cmdAuth(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	shop := fs.String("shop", "", "shop domain (e.g. your-store.myshopify.com)")
	clientID := fs.String("client-id", "", "app client ID (API key)")
	clientSecret := fs.String("client-secret", "", "app client secret")
	scopes := fs.String("scopes", "read_discounts", "comma-separated OAuth scopes")
	port := fs.Int("port", 3456, "local callback port")
	fs.Parse(args)

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return err
	}
	cfg, err := loadConfig(*dataDir)
	if err != nil {
		return err
	}
	c := firstNonEmpty(shopifyCreds{
		Shop: *shop, ClientID: *clientID, ClientSecret: *clientSecret,
	}, cfg)
	if c.Shop == "" {
		return fmt.Errorf("need -shop (or set shop in data/shopify.json)")
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf("need -client-id and -client-secret (or set clientId/clientSecret in data/shopify.json)")
	}

	fmt.Printf("Authorizing app %s on %s (scopes: %s)\n", c.ClientID, c.Shop, *scopes)
	fmt.Printf("Make sure the app allows this redirect URL: http://localhost:%d/auth/callback\n\n", *port)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := shopify.RunOAuth(ctx, c.Shop, c.ClientID, c.ClientSecret, *scopes, *port)
	if err != nil {
		return err
	}

	c.Token = res.AccessToken
	if err := saveConfig(*dataDir, c); err != nil {
		return err
	}
	fmt.Printf("\n✅ Access token saved to %s\n", filepath.Join(*dataDir, "shopify.json"))
	fmt.Printf("   Granted scopes: %s\n", res.Scope)
	fmt.Println("   Now run: shopifydiscount pull")
	return nil
}

func cmdPull(args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	shop := fs.String("shop", "", "shop domain (e.g. your-store.myshopify.com)")
	token := fs.String("token", "", "Admin API access token")
	apiVersion := fs.String("api-version", "", "Admin API version")
	fs.Parse(args)

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	creds, err := resolveCreds(*dataDir, *shop, *token, *apiVersion)
	if err != nil {
		return err
	}

	client := shopify.New(creds.Shop, creds.Token, creds.APIVersion)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fmt.Printf("Pulling discounts from %s …\n", creds.Shop)
	rows, err := client.FetchDiscounts(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no discounts returned (check token scope: read_discounts)")
	}

	hash := hashRows(rows)
	if existing, err := st.FindByHash(hash); err != nil {
		return err
	} else if existing != nil {
		fmt.Printf("No changes since snapshot #%d (taken %s). Nothing to do.\n",
			existing.ID, existing.TakenAt.Local().Format("Jan 2, 2006 15:04"))
		return nil
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
		return err
	}
	fmt.Printf("Pulled snapshot #%d — %d codes, taken %s.\n",
		id, len(rows), now.Local().Format("Jan 2, 2006 15:04"))
	printDelta(st, id)
	return nil
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

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	fs.Parse(args)

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	srv, err := web.New(st)
	if err != nil {
		return err
	}
	fmt.Printf("Dashboard running at http://%s  (Ctrl+C to stop)\n", *addr)
	return http.ListenAndServe(*addr, srv.Handler())
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	fs.Parse(args)

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	latest, err := st.LatestSnapshot()
	if err != nil {
		return err
	}
	if latest == nil {
		fmt.Println("No snapshots imported yet. Run: shopifydiscount import <export.csv>")
		return nil
	}
	discs, err := st.SnapshotDiscounts(latest.ID)
	if err != nil {
		return err
	}
	gone, _ := st.DisappearedCodes(latest.ID)

	var totalUses, totalDelta, active, expired int
	for _, d := range discs {
		totalUses += d.TimesUsed
		totalDelta += d.Delta
		switch strings.ToLower(d.Status) {
		case "active":
			active++
		case "expired":
			expired++
		}
	}

	fmt.Printf("Latest snapshot #%d taken %s\n", latest.ID, latest.TakenAt.Local().Format("Jan 2, 2006 15:04"))
	fmt.Printf("%d codes · %d total uses (%+d since previous) · %d active · %d expired\n\n",
		len(discs), totalUses, totalDelta, active, expired)

	// Top movers since the previous snapshot.
	movers := append([]store.DiscountView(nil), discs...)
	sort.SliceStable(movers, func(i, j int) bool { return movers[i].Delta > movers[j].Delta })
	fmt.Println("Top movers since previous snapshot:")
	shown := 0
	for _, d := range movers {
		if d.Delta <= 0 {
			break
		}
		fmt.Printf("  %-24s +%-4d (now %d)\n", d.Name, d.Delta, d.TimesUsed)
		if shown++; shown >= 10 {
			break
		}
	}
	if shown == 0 {
		fmt.Println("  (none)")
	}
	if len(gone) > 0 {
		fmt.Printf("\nRemoved since previous snapshot: %s\n", strings.Join(gone, ", "))
	}
	return nil
}

// resolveTakenAt determines the snapshot time: an explicit -as-of wins, then the
// file's modified time, then now.
func resolveTakenAt(asOf, csvPath string) (time.Time, error) {
	if asOf != "" {
		if t, err := time.Parse(time.RFC3339, asOf); err == nil {
			return t, nil
		}
		if t, err := time.ParseInLocation("2006-01-02", asOf, time.Local); err == nil {
			return t, nil
		}
		return time.Time{}, fmt.Errorf("could not parse -as-of %q (use RFC3339 or 2006-01-02)", asOf)
	}
	if fi, err := os.Stat(csvPath); err == nil {
		return fi.ModTime(), nil
	}
	return time.Now(), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// archiveCSV copies the raw export into data/archive with a timestamped name.
func archiveCSV(csvPath, dataDir string, takenAt time.Time) error {
	archiveDir := filepath.Join(dataDir, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(archiveDir, takenAt.Format("2006-01-02_150405")+"_"+filepath.Base(csvPath))
	in, err := os.Open(csvPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
