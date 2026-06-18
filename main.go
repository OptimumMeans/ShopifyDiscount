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
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OptimumMeans/ShopifyDiscount/internal/csvimport"
	"github.com/OptimumMeans/ShopifyDiscount/internal/ingest"
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
	cfg, err := ingest.LoadConfig(*dataDir)
	if err != nil {
		return err
	}
	c := ingest.Merge(ingest.Creds{
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
	if err := ingest.SaveConfig(*dataDir, c); err != nil {
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

	creds, err := ingest.ResolveCreds(*dataDir, *shop, *token, *apiVersion)
	if err != nil {
		return err
	}
	fmt.Printf("Pulling discounts from %s …\n", creds.Shop)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := ingest.PullWithCreds(ctx, st, creds)
	if err != nil {
		return err
	}
	if res.Deduped {
		fmt.Printf("No changes since snapshot #%d (taken %s). Nothing to do.\n",
			res.SnapshotID, res.TakenAt.Local().Format("Jan 2, 2006 15:04"))
		return nil
	}
	fmt.Printf("Pulled snapshot #%d — %d codes, taken %s.\n",
		res.SnapshotID, res.RowCount, res.TakenAt.Local().Format("Jan 2, 2006 15:04"))
	fmt.Printf("  Total uses: %d  (%+d since previous snapshot)\n", res.TotalUses, res.TotalDelta)
	if res.NewCodes > 0 {
		fmt.Printf("  New codes: %d\n", res.NewCodes)
	}
	if len(res.Removed) > 0 {
		fmt.Printf("  Removed since previous: %d  (%s)\n", len(res.Removed), strings.Join(res.Removed, ", "))
	}
	return nil
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

	srv, err := web.New(st, *dataDir)
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
