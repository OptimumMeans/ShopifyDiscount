// Package csvimport parses Shopify "Discounts export" CSV files into typed rows.
//
// Shopify's export uses these columns (order is not guaranteed, so we map by
// header name):
//
//	Name, Value, Value Type, Type, Discount Class,
//	Minimum Purchase Requirements, Combines with Order Discounts,
//	Combines with Product Discounts, Combines with Shipping Discounts,
//	Customer Selection, Context, Times Used In Total,
//	Applies Once Per Customer, Usage Limit Per Code, Status, Start, End
package csvimport

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// shopifyTimeLayout matches values like "2025-12-08 11:28:18 -0600".
const shopifyTimeLayout = "2006-01-02 15:04:05 -0700"

// Row is one discount as captured in a single export.
type Row struct {
	Name              string
	Value             float64
	ValueType         string // percentage, fixed_amount, app
	Type              string // Amount Off, Free Shipping, App Discount
	DiscountClass     string // product, order, shipping, multiple
	MinPurchase       string
	CombinesOrder     string
	CombinesProduct   string
	CombinesShipping  string
	CustomerSelection string
	Context           string
	TimesUsed         int
	AppliesOnce       string
	UsageLimit        string
	Status            string // Active, Expired, ...
	StartAt           string // RFC3339, "" if absent
	EndAt             string // RFC3339, "" if absent
}

// File is the parsed contents of one export.
type File struct {
	Rows []Row
}

// ParseFile reads and parses a Shopify discounts export at path.
func ParseFile(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads a Shopify discounts export from r.
func Parse(r io.Reader) (*File, error) {
	cr := csv.NewReader(skipBOM(r))
	cr.FieldsPerRecord = -1 // tolerate ragged rows
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	idx := indexHeader(header)
	if _, ok := idx["name"]; !ok {
		return nil, fmt.Errorf("not a Shopify discounts export: missing 'Name' column (got %v)", header)
	}

	var out File
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		get := func(key string) string {
			if i, ok := idx[key]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		name := get("name")
		if name == "" {
			continue // skip blank lines
		}
		row := Row{
			Name:              name,
			Value:             parseFloat(get("value")),
			ValueType:         get("value type"),
			Type:              get("type"),
			DiscountClass:     get("discount class"),
			MinPurchase:       get("minimum purchase requirements"),
			CombinesOrder:     get("combines with order discounts"),
			CombinesProduct:   get("combines with product discounts"),
			CombinesShipping:  get("combines with shipping discounts"),
			CustomerSelection: get("customer selection"),
			Context:           get("context"),
			TimesUsed:         parseInt(get("times used in total")),
			AppliesOnce:       get("applies once per customer"),
			UsageLimit:        get("usage limit per code"),
			Status:            get("status"),
			StartAt:           parseTime(get("start")),
			EndAt:             parseTime(get("end")),
		}
		out.Rows = append(out.Rows, row)
	}
	return &out, nil
}

// skipBOM returns a reader that discards a leading UTF-8 byte-order mark, which
// some exports (and spreadsheet round-trips) prepend. encoding/csv otherwise
// treats the BOM bytes as part of the first field and errors out.
func skipBOM(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	if b, err := br.Peek(3); err == nil && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		br.Discard(3)
	}
	return br
}

// indexHeader maps normalized (lower-cased, trimmed) header names to column index.
func indexHeader(header []string) map[string]int {
	bom := string(rune(0xFEFF)) // Shopify may prepend a UTF-8 BOM to the first header
	m := make(map[string]int, len(header))
	for i, h := range header {
		key := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, bom)))
		m[key] = i
	}
	return m
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	v, _ := strconv.Atoi(s)
	return v
}

// parseTime converts a Shopify timestamp to RFC3339, returning "" when blank or
// unparseable (so downstream storage stays simple text).
func parseTime(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(shopifyTimeLayout, s)
	if err != nil {
		return s // keep the raw value rather than lose information
	}
	return t.Format(time.RFC3339)
}
