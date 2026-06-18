// Package web serves the local dashboard for browsing the discount archive.
package web

import (
	"context"
	"embed"
	"encoding/csv"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OptimumMeans/ShopifyDiscount/internal/ingest"
	"github.com/OptimumMeans/ShopifyDiscount/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server renders dashboard pages from the store.
type Server struct {
	st      *store.Store
	tpl     *template.Template
	dataDir string
}

// New builds a Server backed by st. dataDir is used to pull new snapshots from
// the UI and to serve an optional logo at data/logo.* (git-ignored).
func New(st *store.Store, dataDir string) (*Server, error) {
	tpl, err := template.New("").Funcs(funcMap()).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{st: st, tpl: tpl, dataDir: dataDir}, nil
}

// Handler returns the configured HTTP router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /snapshots", s.handleSnapshots)
	mux.HandleFunc("GET /archive", s.handleArchive)
	mux.HandleFunc("GET /backup/db", s.handleBackupDB)
	mux.HandleFunc("GET /backup/csv", s.handleBackupCSV)
	mux.HandleFunc("GET /code/{name}", s.handleCode)
	mux.HandleFunc("POST /code/{name}/note", s.handleSetNote)
	mux.HandleFunc("GET /owners", s.handleOwners)
	mux.HandleFunc("GET /trends", s.handleTrends)
	mux.HandleFunc("GET /attention", s.handleAttention)
	mux.HandleFunc("GET /compare", s.handleCompare)
	mux.HandleFunc("POST /pull", s.handlePull)
	mux.HandleFunc("GET /logo", s.handleLogo)
	if static, err := fs.Sub(assets, "static"); err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	}
	return mux
}

type dashboardData struct {
	Latest      *store.SnapshotMeta
	Snapshots   []store.SnapshotMeta
	Discounts   []store.DiscountView
	Disappeared []string
	Totals      totals
	Flash       string
	FlashErr    bool
	ByStatus    []Segment
	ByClass     []Segment
	ByValueType []Segment
	Redemption  []Segment
	TopCodes    []Segment
}

// Segment is one slice of a chart (donut wedge or bar).
type Segment struct {
	Label string
	Value int
	Color string
}

// chartPalette cycles through theme colors for categorical breakdowns.
var chartPalette = []string{"#16A3BE", "#BDFDC0", "#0f7e94", "#0f8a5f", "#cfcfcf", "#7fd4e3", "#000000", "#e6e6e6"}

type totals struct {
	Codes       int
	TimesUsed   int
	TotalDelta  int
	Active      int
	Expired     int
	NewCodes    int
	WithUsage   int
	Disappeared int
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	latest, err := s.st.LatestSnapshot()
	if err != nil {
		httpError(w, err)
		return
	}
	data := dashboardData{Latest: latest}
	if m := r.URL.Query().Get("msg"); m != "" {
		data.Flash = m
	}
	if e := r.URL.Query().Get("err"); e != "" {
		data.Flash, data.FlashErr = e, true
	}
	snaps, err := s.st.Snapshots()
	if err != nil {
		httpError(w, err)
		return
	}
	data.Snapshots = snaps
	if latest != nil {
		discs, err := s.st.SnapshotDiscounts(latest.ID)
		if err != nil {
			httpError(w, err)
			return
		}
		data.Discounts = discs
		gone, err := s.st.DisappearedCodes(latest.ID)
		if err != nil {
			httpError(w, err)
			return
		}
		data.Disappeared = gone
		data.Totals = computeTotals(discs, gone)
		data.ByStatus = breakdown(discs, func(d store.DiscountView) string { return orDash(d.Status) })
		data.ByClass = breakdown(discs, func(d store.DiscountView) string { return titleCase(orDash(d.DiscountClass)) })
		data.ByValueType = breakdown(discs, func(d store.DiscountView) string { return valueTypeLabel(d.ValueType) })
		data.Redemption = redemption(discs)
		data.TopCodes = topCodes(discs, 10)
	}
	s.render(w, "dashboard.html", data)
}

// breakdown counts discounts by a key and returns palette-colored segments,
// largest first.
func breakdown(ds []store.DiscountView, key func(store.DiscountView) string) []Segment {
	counts := map[string]int{}
	var order []string
	for _, d := range ds {
		k := key(d)
		if _, ok := counts[k]; !ok {
			order = append(order, k)
		}
		counts[k]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	segs := make([]Segment, len(order))
	for i, k := range order {
		segs[i] = Segment{Label: k, Value: counts[k], Color: chartPalette[i%len(chartPalette)]}
	}
	return segs
}

// redemption splits codes into redeemed (any usage) vs unused.
func redemption(ds []store.DiscountView) []Segment {
	used := 0
	for _, d := range ds {
		if d.TimesUsed > 0 {
			used++
		}
	}
	return []Segment{
		{Label: "Redeemed", Value: used, Color: "#16A3BE"},
		{Label: "Unused", Value: len(ds) - used, Color: "#e6e6e6"},
	}
}

// topCodes returns the n most-used codes (usage > 0), descending.
func topCodes(ds []store.DiscountView, n int) []Segment {
	sorted := append([]store.DiscountView(nil), ds...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TimesUsed > sorted[j].TimesUsed })
	var segs []Segment
	for _, d := range sorted {
		if d.TimesUsed <= 0 || len(segs) >= n {
			break
		}
		segs = append(segs, Segment{Label: d.Name, Value: d.TimesUsed, Color: "#16A3BE"})
	}
	return segs
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func titleCase(s string) string {
	if s == "" || s == "—" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func valueTypeLabel(vt string) string {
	switch vt {
	case "percentage":
		return "Percentage"
	case "fixed_amount":
		return "Fixed amount"
	case "app":
		return "App"
	case "":
		return "—"
	default:
		return vt
	}
}

func computeTotals(discs []store.DiscountView, gone []string) totals {
	t := totals{Codes: len(discs), Disappeared: len(gone)}
	for _, d := range discs {
		t.TimesUsed += d.TimesUsed
		t.TotalDelta += d.Delta
		if d.IsNew {
			t.NewCodes++
		}
		if d.TimesUsed > 0 {
			t.WithUsage++
		}
		switch strings.ToLower(d.Status) {
		case "active":
			t.Active++
		case "expired":
			t.Expired++
		}
	}
	return t
}

// handlePull pulls a fresh snapshot from Shopify, then redirects back to the
// dashboard with a status message.
func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	res, err := ingest.Pull(ctx, s.st, s.dataDir)
	if err != nil {
		http.Redirect(w, r, "/?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	var msg string
	if res.Deduped {
		msg = fmt.Sprintf("No changes since snapshot #%d — nothing new to record.", res.SnapshotID)
	} else {
		msg = fmt.Sprintf("Pulled snapshot #%d · %d codes · %+d uses since last.", res.SnapshotID, res.RowCount, res.TotalDelta)
	}
	http.Redirect(w, r, "/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleLogo serves an optional logo from the (git-ignored) data dir, so brand
// assets never enter the repo or the binary.
func (s *Server) handleLogo(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{"logo.svg", "logo.png", "logo.jpg", "logo.jpeg", "logo.webp", "logo.gif"} {
		p := filepath.Join(s.dataDir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, p)
			return
		}
	}
	http.NotFound(w, r)
}

type archiveData struct {
	Codes      []store.ArchivedCode
	Total      int
	Live       int
	Retired    int
	HasRevenue bool
	Revenue    map[string]store.Revenue
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	codes, err := s.st.AllCodes()
	if err != nil {
		httpError(w, err)
		return
	}
	d := archiveData{Codes: codes, Total: len(codes)}
	for _, c := range codes {
		if c.Live {
			d.Live++
		} else {
			d.Retired++
		}
	}
	rev, err := s.st.AllRevenue()
	if err != nil {
		httpError(w, err)
		return
	}
	if len(rev) > 0 {
		d.HasRevenue = true
		d.Revenue = rev
	}
	s.render(w, "archive.html", d)
}

// revLabel formats a code's revenue from the map, "—" when absent.
func revLabel(m map[string]store.Revenue, name string) string {
	r, ok := m[name]
	if !ok {
		return "—"
	}
	if r.Currency != "" {
		return fmt.Sprintf("%s %.2f", r.Currency, r.TotalDiscount)
	}
	return fmt.Sprintf("%.2f", r.TotalDiscount)
}

// handleBackupDB streams a consistent copy of the SQLite database as a download.
func (s *Server) handleBackupDB(w http.ResponseWriter, r *http.Request) {
	f, err := os.CreateTemp("", "discounts-*.db")
	if err != nil {
		httpError(w, err)
		return
	}
	name := f.Name()
	f.Close()
	os.Remove(name) // VACUUM INTO requires the destination not to exist
	if err := s.st.BackupTo(name); err != nil {
		httpError(w, err)
		return
	}
	defer os.Remove(name)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="discounts-backup.db"`)
	http.ServeFile(w, r, name)
}

// handleBackupCSV exports the full archive (every code ever seen) as a CSV.
func (s *Server) handleBackupCSV(w http.ResponseWriter, r *http.Request) {
	codes, err := s.st.AllCodes()
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="discount-archive.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	cw.Write([]string{"Code", "Value", "Value Type", "Status", "Times Used",
		"Ends", "Usage Limit", "First Seen", "Last Seen", "Snapshots", "State"})
	for _, c := range codes {
		state := "Retired"
		if c.Live {
			state = "Live"
		}
		ends := ""
		if t, err := time.Parse(time.RFC3339, c.EndAt); err == nil {
			ends = t.Format("2006-01-02")
		}
		cw.Write([]string{
			c.Name, fmt.Sprintf("%g", c.Value), c.ValueType, c.Status, fmt.Sprintf("%d", c.TimesUsed),
			ends, c.UsageLimit, c.FirstSeen.Format("2006-01-02"), c.LastSeen.Format("2006-01-02"),
			fmt.Sprintf("%d", c.Snapshots), state,
		})
	}
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.st.Snapshots()
	if err != nil {
		httpError(w, err)
		return
	}
	s.render(w, "snapshots.html", snaps)
}

type codeData struct {
	Name    string
	History []store.HistoryPoint
	Latest  store.HistoryPoint
	Min     int
	Max     int
	First   store.HistoryPoint
	Note    store.Note
}

func (s *Server) handleCode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	hist, err := s.st.CodeHistory(name)
	if err != nil {
		httpError(w, err)
		return
	}
	if len(hist) == 0 {
		http.NotFound(w, r)
		return
	}
	d := codeData{Name: name, History: hist, First: hist[0], Latest: hist[len(hist)-1]}
	d.Min, d.Max = hist[0].TimesUsed, hist[0].TimesUsed
	for _, p := range hist {
		if p.TimesUsed < d.Min {
			d.Min = p.TimesUsed
		}
		if p.TimesUsed > d.Max {
			d.Max = p.TimesUsed
		}
	}
	note, err := s.st.GetNote(name)
	if err != nil {
		httpError(w, err)
		return
	}
	d.Note = note
	s.render(w, "code.html", d)
}

// handleSetNote saves a free-form note + tags for a code and redirects back.
func (s *Server) handleSetNote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if err := s.st.SetNote(name, strings.TrimSpace(r.FormValue("note")), strings.TrimSpace(r.FormValue("tags"))); err != nil {
		httpError(w, err)
		return
	}
	http.Redirect(w, r, "/code/"+url.PathEscape(name), http.StatusSeeOther)
}

// inferOwner derives an owner/group label from a code by trimming trailing
// digits and trailing non-alphanumeric characters (e.g. "Elisa15" -> "Elisa").
// It does no brand-specific stripping — digits and punctuation only.
func inferOwner(code string) string {
	r := []rune(code)
	i := len(r)
	for i > 0 {
		c := r[i-1]
		if (c >= '0' && c <= '9') ||
			!(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			i--
			continue
		}
		break
	}
	owner := string(r[:i])
	if owner == "" {
		return code
	}
	return owner
}

type ownerRow struct {
	Owner     string
	Codes     int
	TimesUsed int
}

type ownersData struct {
	Owners []ownerRow
	Total  int
}

func (s *Server) handleOwners(w http.ResponseWriter, r *http.Request) {
	latest, err := s.st.LatestSnapshot()
	if err != nil {
		httpError(w, err)
		return
	}
	if latest == nil {
		s.render(w, "owners.html", ownersData{})
		return
	}
	discs, err := s.st.SnapshotDiscounts(latest.ID)
	if err != nil {
		httpError(w, err)
		return
	}
	idx := map[string]int{}
	var rows []ownerRow
	for _, d := range discs {
		o := inferOwner(d.Name)
		if i, ok := idx[o]; ok {
			rows[i].Codes++
			rows[i].TimesUsed += d.TimesUsed
		} else {
			idx[o] = len(rows)
			rows = append(rows, ownerRow{Owner: o, Codes: 1, TimesUsed: d.TimesUsed})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TimesUsed > rows[j].TimesUsed })
	s.render(w, "owners.html", ownersData{Owners: rows, Total: len(discs)})
}

type trendRow struct {
	store.SnapshotTotal
	Delta   int
	HadPrev bool
}

type trendsData struct {
	Points []store.SnapshotTotal
	Rows   []trendRow
}

func (s *Server) handleTrends(w http.ResponseWriter, r *http.Request) {
	totals, err := s.st.SnapshotTotals()
	if err != nil {
		httpError(w, err)
		return
	}
	rows := make([]trendRow, len(totals))
	for i, t := range totals {
		rows[i] = trendRow{SnapshotTotal: t}
		if i > 0 {
			rows[i].HadPrev = true
			rows[i].Delta = t.TotalUses - totals[i-1].TotalUses
		}
	}
	// Newest first for the table.
	rev := make([]trendRow, len(rows))
	for i := range rows {
		rev[len(rows)-1-i] = rows[i]
	}
	s.render(w, "trends.html", trendsData{Points: totals, Rows: rev})
}

type attnRow struct {
	store.DiscountView
	Limit int
	Ratio float64
}

type attentionData struct {
	Expiring []attnRow
	NearCap  []attnRow
}

func (s *Server) handleAttention(w http.ResponseWriter, r *http.Request) {
	latest, err := s.st.LatestSnapshot()
	if err != nil {
		httpError(w, err)
		return
	}
	var d attentionData
	if latest == nil {
		s.render(w, "attention.html", d)
		return
	}
	discs, err := s.st.SnapshotDiscounts(latest.ID)
	if err != nil {
		httpError(w, err)
		return
	}
	now := time.Now()
	cutoff := now.AddDate(0, 0, 30)
	type expiring struct {
		row attnRow
		end time.Time
	}
	var exp []expiring
	for _, d2 := range discs {
		if !strings.EqualFold(d2.Status, "active") {
			continue
		}
		if d2.EndAt == "" {
			continue
		}
		end, err := time.Parse(time.RFC3339, d2.EndAt)
		if err != nil {
			continue
		}
		if end.After(now) && end.Before(cutoff) {
			exp = append(exp, expiring{row: attnRow{DiscountView: d2}, end: end})
		}
	}
	sort.SliceStable(exp, func(i, j int) bool { return exp[i].end.Before(exp[j].end) })
	for _, e := range exp {
		d.Expiring = append(d.Expiring, e.row)
	}

	for _, d2 := range discs {
		limit, err := strconv.Atoi(strings.TrimSpace(d2.UsageLimit))
		if err != nil || limit <= 0 {
			continue
		}
		ratio := float64(d2.TimesUsed) / float64(limit)
		if ratio >= 0.8 && d2.TimesUsed <= limit {
			d.NearCap = append(d.NearCap, attnRow{DiscountView: d2, Limit: limit, Ratio: ratio})
		}
	}
	sort.SliceStable(d.NearCap, func(i, j int) bool { return d.NearCap[i].Ratio > d.NearCap[j].Ratio })
	s.render(w, "attention.html", d)
}

type compareChange struct {
	Name  string
	A     int
	B     int
	Delta int
}

type compareData struct {
	Snapshots []store.SnapshotMeta
	A         int64
	B         int64
	HasA      bool
	HasB      bool
	OnlyA     []string // removed
	OnlyB     []string // added
	Changed   []compareChange
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.st.Snapshots()
	if err != nil {
		httpError(w, err)
		return
	}
	d := compareData{Snapshots: snaps}
	aStr := r.URL.Query().Get("a")
	bStr := r.URL.Query().Get("b")
	if aStr == "" || bStr == "" {
		s.render(w, "compare.html", d)
		return
	}
	a, errA := strconv.ParseInt(aStr, 10, 64)
	b, errB := strconv.ParseInt(bStr, 10, 64)
	if errA != nil || errB != nil {
		s.render(w, "compare.html", d)
		return
	}
	d.A, d.B, d.HasA, d.HasB = a, b, true, true
	rowsA, err := s.st.SnapshotRowsByName(a)
	if err != nil {
		httpError(w, err)
		return
	}
	rowsB, err := s.st.SnapshotRowsByName(b)
	if err != nil {
		httpError(w, err)
		return
	}
	for name, ra := range rowsA {
		if rb, ok := rowsB[name]; ok {
			if ra.TimesUsed != rb.TimesUsed {
				d.Changed = append(d.Changed, compareChange{
					Name: name, A: ra.TimesUsed, B: rb.TimesUsed, Delta: rb.TimesUsed - ra.TimesUsed,
				})
			}
		} else {
			d.OnlyA = append(d.OnlyA, name)
		}
	}
	for name := range rowsB {
		if _, ok := rowsA[name]; !ok {
			d.OnlyB = append(d.OnlyB, name)
		}
	}
	sort.Strings(d.OnlyA)
	sort.Strings(d.OnlyB)
	sort.SliceStable(d.Changed, func(i, j int) bool { return d.Changed[i].Name < d.Changed[j].Name })
	s.render(w, "compare.html", d)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		httpError(w, err)
	}
}

func httpError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("Jan 2, 2006 15:04")
		},
		"day": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("Jan 2, 2006")
		},
		"pct": func(v float64) string {
			// Shopify stores percentages as negatives like -15.0.
			return fmt.Sprintf("%g%%", -v)
		},
		"delta": func(n int) template.HTML {
			switch {
			case n > 0:
				return template.HTML(fmt.Sprintf(`<span class="up">+%d</span>`, n))
			case n < 0:
				return template.HTML(fmt.Sprintf(`<span class="down">%d</span>`, n))
			default:
				return template.HTML(`<span class="flat">0</span>`)
			}
		},
		"sparkline": sparkline,
		"linechart": linechart,
		"donut":     donut,
		"bars":      bars,
		"revLabel":  revLabel,
		"splitTags": func(s string) []string {
			var out []string
			for _, t := range strings.Split(s, ",") {
				if t = strings.TrimSpace(t); t != "" {
					out = append(out, t)
				}
			}
			return out
		},
		"pctOf": func(part, total int) string {
			if total == 0 {
				return "0%"
			}
			return fmt.Sprintf("%.0f%%", float64(part)/float64(total)*100)
		},
		"lower":   strings.ToLower,
		"sub":     func(a, b int) int { return a - b },
		"codeURL": func(name string) string { return "/code/" + url.PathEscape(name) },
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d is not a string", i)
				}
				m[k] = pairs[i+1]
			}
			return m, nil
		},
		"rfcday": func(s string) string {
			if s == "" {
				return "—"
			}
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t.Local().Format("Jan 2, 2006")
			}
			return s
		},
	}
}

// sparkline renders usage history as a small inline SVG polyline.
func sparkline(history []store.HistoryPoint) template.HTML {
	const w, h, pad = 220, 44, 4
	if len(history) == 0 {
		return ""
	}
	if len(history) == 1 {
		return template.HTML(fmt.Sprintf(
			`<svg class="spark" width="%d" height="%d" viewBox="0 0 %d %d"><circle cx="%d" cy="%d" r="3" fill="#16A3BE"/></svg>`,
			w, h, w, h, w/2, h/2))
	}
	minV, maxV := history[0].TimesUsed, history[0].TimesUsed
	for _, p := range history {
		if p.TimesUsed < minV {
			minV = p.TimesUsed
		}
		if p.TimesUsed > maxV {
			maxV = p.TimesUsed
		}
	}
	span := float64(maxV - minV)
	if span == 0 {
		span = 1
	}
	var b strings.Builder
	for i, p := range history {
		x := pad + float64(i)*float64(w-2*pad)/float64(len(history)-1)
		y := float64(h-pad) - float64(p.TimesUsed-minV)/span*float64(h-2*pad)
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="spark" width="%d" height="%d" viewBox="0 0 %d %d"><polyline fill="none" stroke="#16A3BE" stroke-width="2" points="%s"/></svg>`,
		w, h, w, h, b.String()))
}

// linechart renders total uses over time as an inline SVG line chart: teal
// stroke, dots at each point, and light gridlines at the min and max values.
// Axis-free, ~640x220. Handles 0 and 1 points gracefully.
func linechart(points []store.SnapshotTotal) template.HTML {
	const w, h = 640, 220
	const padL, padR, padT, padB = 16, 16, 16, 16
	if len(points) == 0 {
		return template.HTML(fmt.Sprintf(
			`<svg class="line" width="%d" height="%d" viewBox="0 0 %d %d"><text x="%d" y="%d" text-anchor="middle" fill="#6b6b6b" font-size="13">No snapshots yet.</text></svg>`,
			w, h, w, h, w/2, h/2))
	}
	if len(points) == 1 {
		return template.HTML(fmt.Sprintf(
			`<svg class="line" width="%d" height="%d" viewBox="0 0 %d %d"><circle cx="%d" cy="%d" r="4" fill="#16A3BE"/><text x="%d" y="%d" text-anchor="middle" fill="#6b6b6b" font-size="12">%d uses</text></svg>`,
			w, h, w, h, w/2, h/2, w/2, h/2-12, points[0].TotalUses))
	}
	minV, maxV := points[0].TotalUses, points[0].TotalUses
	for _, p := range points {
		if p.TotalUses < minV {
			minV = p.TotalUses
		}
		if p.TotalUses > maxV {
			maxV = p.TotalUses
		}
	}
	span := float64(maxV - minV)
	if span == 0 {
		span = 1
	}
	plotW := float64(w - padL - padR)
	plotH := float64(h - padT - padB)
	xAt := func(i int) float64 { return float64(padL) + float64(i)*plotW/float64(len(points)-1) }
	yAt := func(v int) float64 { return float64(padT) + plotH - float64(v-minV)/span*plotH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="line" width="%d" height="%d" viewBox="0 0 %d %d">`, w, h, w, h)
	// Gridlines at min and max.
	yMax := yAt(maxV)
	yMin := yAt(minV)
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#e6e6e6" stroke-width="1"/>`, padL, yMax, w-padR, yMax)
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#e6e6e6" stroke-width="1"/>`, padL, yMin, w-padR, yMin)
	fmt.Fprintf(&b, `<text x="%d" y="%.1f" fill="#6b6b6b" font-size="11">%d</text>`, padL, yMax-4, maxV)
	fmt.Fprintf(&b, `<text x="%d" y="%.1f" fill="#6b6b6b" font-size="11">%d</text>`, padL, yMin-4, minV)
	// Polyline.
	var pts strings.Builder
	for i, p := range points {
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%.1f,%.1f", xAt(i), yAt(p.TotalUses))
	}
	fmt.Fprintf(&b, `<polyline fill="none" stroke="#16A3BE" stroke-width="2" points="%s"/>`, pts.String())
	// Dots.
	for i, p := range points {
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="#16A3BE"><title>%s · %d</title></circle>`,
			xAt(i), yAt(p.TotalUses), html.EscapeString(p.TakenAt.Local().Format("Jan 2, 2006")), p.TotalUses)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// donut renders a stacked-segment donut chart as inline SVG, with the total in
// the center. Segments are drawn as arcs via stroke-dasharray on a circle.
func donut(segs []Segment) template.HTML {
	const cx, cy, r, sw = 80.0, 80.0, 60.0, 26.0
	total := 0
	for _, s := range segs {
		total += s.Value
	}
	var b strings.Builder
	fmt.Fprint(&b, `<svg class="donut" width="160" height="160" viewBox="0 0 160 160">`)
	fmt.Fprintf(&b, `<circle cx="%g" cy="%g" r="%g" fill="none" stroke="#f0f0f0" stroke-width="%g"/>`, cx, cy, r, sw)
	if total > 0 {
		c := 2 * math.Pi * r
		offset := 0.0
		for _, s := range segs {
			if s.Value == 0 {
				continue
			}
			dash := float64(s.Value) / float64(total) * c
			fmt.Fprintf(&b, `<circle cx="%g" cy="%g" r="%g" fill="none" stroke="%s" stroke-width="%g" stroke-dasharray="%.2f %.2f" stroke-dashoffset="%.2f" transform="rotate(-90 %g %g)"/>`,
				cx, cy, r, s.Color, sw, dash, c-dash, -offset, cx, cy)
			offset += dash
		}
	}
	fmt.Fprintf(&b, `<text x="%g" y="%g" text-anchor="middle" font-size="28" font-weight="700" fill="#000">%d</text>`, cx, cy+6, total)
	fmt.Fprint(&b, `</svg>`)
	return template.HTML(b.String())
}

// bars renders a horizontal bar chart for ranked values (e.g. top codes).
func bars(segs []Segment) template.HTML {
	maxV := 0
	for _, s := range segs {
		if s.Value > maxV {
			maxV = s.Value
		}
	}
	if maxV == 0 {
		return template.HTML(`<p class="muted">No redemptions yet.</p>`)
	}
	var b strings.Builder
	b.WriteString(`<div class="bars">`)
	for _, s := range segs {
		w := float64(s.Value) / float64(maxV) * 100
		fmt.Fprintf(&b, `<div class="bar-row"><a class="bar-label" href="/code/%s" title="%s">%s</a><span class="bar-track"><span class="bar-fill" style="width:%.1f%%"></span></span><span class="bar-val mono">%d</span></div>`,
			url.PathEscape(s.Label), html.EscapeString(s.Label), html.EscapeString(s.Label), w, s.Value)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}
