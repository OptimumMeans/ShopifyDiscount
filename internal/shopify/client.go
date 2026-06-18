// Package shopify pulls discounts directly from the Shopify Admin GraphQL API
// and maps them to the same row shape produced by the CSV importer, so a "pull"
// snapshot is interchangeable with an "import" snapshot.
package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OptimumMeans/ShopifyDiscount/internal/csvimport"
)

// Client talks to one shop's Admin GraphQL endpoint.
type Client struct {
	Shop       string // e.g. "your-store.myshopify.com"
	Token      string // Admin API access token (needs read_discounts)
	APIVersion string // e.g. "2025-10"
	HTTP       *http.Client
}

// New builds a Client with sensible defaults.
func New(shop, token, apiVersion string) *Client {
	if apiVersion == "" {
		apiVersion = "2025-10"
	}
	return &Client{
		Shop:       shop,
		Token:      token,
		APIVersion: apiVersion,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
}

// endpoint returns the GraphQL URL for the shop.
func (c *Client) endpoint() string {
	return fmt.Sprintf("https://%s/admin/api/%s/graphql.json", c.Shop, c.APIVersion)
}

// FetchDiscounts pages through every discount and returns them as importer rows.
func (c *Client) FetchDiscounts(ctx context.Context) ([]csvimport.Row, error) {
	if c.Shop == "" || c.Token == "" {
		return nil, fmt.Errorf("shop and token are required")
	}
	var (
		all    []discountNode
		cursor string
	)
	for {
		page, err := c.fetchPage(ctx, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			break
		}
		cursor = page.PageInfo.EndCursor
	}
	// Bulk discounts can hold more codes than one page (250). Fetch the rest so
	// per-code rows are never silently truncated.
	for i := range all {
		d := &all[i].Discount
		if d.Codes == nil || !d.Codes.PageInfo.HasNextPage {
			continue
		}
		more, err := c.fetchAllCodes(ctx, all[i].ID, d.Codes.PageInfo.EndCursor)
		if err != nil {
			return nil, fmt.Errorf("paging codes for %q: %w", d.Title, err)
		}
		d.Codes.Nodes = append(d.Codes.Nodes, more...)
	}
	return nodesToRows(all), nil
}

// fetchAllCodes pages through a single discount's remaining redeem codes,
// starting after the given cursor.
func (c *Client) fetchAllCodes(ctx context.Context, discountID, after string) ([]codeNode, error) {
	var out []codeNode
	cursor := after
	for {
		body, err := json.Marshal(map[string]any{
			"query":     moreCodesQuery,
			"variables": map[string]any{"id": discountID, "cursor": cursor},
		})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Shopify-Access-Token", c.Token)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(raw))
		}
		var out2 struct {
			Data struct {
				DiscountNode struct {
					Discount struct {
						Codes codesConn `json:"codes"`
					} `json:"discount"`
				} `json:"discountNode"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(raw, &out2); err != nil {
			return nil, err
		}
		if len(out2.Errors) > 0 {
			return nil, fmt.Errorf("graphql error: %s", out2.Errors[0].Message)
		}
		conn := out2.Data.DiscountNode.Discount.Codes
		out = append(out, conn.Nodes...)
		if !conn.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = conn.PageInfo.EndCursor
	}
}

func (c *Client) fetchPage(ctx context.Context, cursor string) (*discountConnection, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		conn, throttled, err := c.doRequest(ctx, cursor)
		switch {
		case err == nil:
			return conn, nil
		case throttled:
			lastErr = err
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
		default:
			return nil, err
		}
	}
	return nil, fmt.Errorf("giving up after throttling: %w", lastErr)
}

func (c *Client) doRequest(ctx context.Context, cursor string) (*discountConnection, bool, error) {
	var cur *string
	if cursor != "" {
		cur = &cursor
	}
	body, err := json.Marshal(map[string]any{
		"query":     discountsQuery,
		"variables": map[string]any{"cursor": cur},
	})
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Access-Token", c.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("shopify HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("shopify HTTP %d: %s", resp.StatusCode, snippet(raw))
	}

	var out gqlResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("decoding response: %w", err)
	}
	if len(out.Errors) > 0 {
		msg := out.Errors[0].Message
		throttled := strings.Contains(strings.ToUpper(msg), "THROTTLED")
		return nil, throttled, fmt.Errorf("graphql error: %s", msg)
	}
	return &out.Data.DiscountNodes, false, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// ---- GraphQL response shapes -------------------------------------------------

type gqlResponse struct {
	Data struct {
		DiscountNodes discountConnection `json:"discountNodes"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type discountConnection struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []discountNode `json:"nodes"`
}

type discountNode struct {
	ID       string   `json:"id"`
	Discount discount `json:"discount"`
}

// codeNode is one redeem code within a discount, with its own usage count.
type codeNode struct {
	Code            string `json:"code"`
	AsyncUsageCount int    `json:"asyncUsageCount"`
}

// codesConn is a (possibly paginated) connection of redeem codes.
type codesConn struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []codeNode `json:"nodes"`
}

// discount flattens every concrete discount type. Because the query uses inline
// fragments that share field names, unset fields simply decode to their zero
// value (pointers stay nil).
type discount struct {
	Typename               string  `json:"__typename"`
	Title                  string  `json:"title"`
	Status                 string  `json:"status"`
	StartsAt               string  `json:"startsAt"`
	EndsAt                 string  `json:"endsAt"`
	AsyncUsageCount        int     `json:"asyncUsageCount"`
	UsageLimit             *int    `json:"usageLimit"`
	AppliesOncePerCustomer *bool      `json:"appliesOncePerCustomer"`
	CodesCount             *struct {
		Count int `json:"count"`
	} `json:"codesCount"`
	Codes        *codesConn `json:"codes"`
	CombinesWith *struct {
		OrderDiscounts    bool `json:"orderDiscounts"`
		ProductDiscounts  bool `json:"productDiscounts"`
		ShippingDiscounts bool `json:"shippingDiscounts"`
	} `json:"combinesWith"`
	CustomerGets *struct {
		Value struct {
			Typename   string  `json:"__typename"`
			Percentage float64 `json:"percentage"`
			Amount     *struct {
				Amount string `json:"amount"`
			} `json:"amount"`
		} `json:"value"`
	} `json:"customerGets"`
}

// nodesToRows maps decoded GraphQL nodes to importer rows. Exported indirectly
// via FetchDiscounts; kept separate so it can be unit-tested without a network.
//
// A discount with a single code (or none) yields one row keyed by its title
// (matching Shopify's CSV). A bulk discount with many codes (hundreds of unique
// codes under one discount) yields one row per code, keyed by the code with that
// code's own usage count, so each can be tracked individually.
func nodesToRows(nodes []discountNode) []csvimport.Row {
	var rows []csvimport.Row
	for _, n := range nodes {
		d := n.Discount
		base := baseRow(d)
		if d.Codes != nil && len(d.Codes.Nodes) > 1 {
			for _, c := range d.Codes.Nodes {
				r := base
				r.Name = c.Code
				r.TimesUsed = c.AsyncUsageCount
				rows = append(rows, r)
			}
			continue
		}
		base.Name = discountName(d)
		base.TimesUsed = d.AsyncUsageCount
		rows = append(rows, base)
	}
	return rows
}

// baseRow fills the discount-level attributes shared by every row a discount
// produces (everything except Name and TimesUsed).
func baseRow(d discount) csvimport.Row {
	row := csvimport.Row{
		Status:  titleStatus(d.Status),
		StartAt: normTime(d.StartsAt),
		EndAt:   normTime(d.EndsAt),
	}
	if d.UsageLimit != nil {
		row.UsageLimit = strconv.Itoa(*d.UsageLimit)
	}
	if d.AppliesOncePerCustomer != nil && *d.AppliesOncePerCustomer {
		row.AppliesOnce = "Yes"
	}
	if d.CombinesWith != nil {
		row.CombinesOrder = yesNo(d.CombinesWith.OrderDiscounts)
		row.CombinesProduct = yesNo(d.CombinesWith.ProductDiscounts)
		row.CombinesShipping = yesNo(d.CombinesWith.ShippingDiscounts)
	}
	row.Value, row.ValueType = discountValue(d)
	row.Type, row.DiscountClass = discountTypeClass(d)
	return row
}

// discountName matches Shopify's CSV "Name" column, which is the discount's
// title — not its code. They're usually identical for hand-made code discounts,
// but app-generated discounts often have a descriptive title and one or more
// separate codes, so keying on the code would mis-identify them.
func discountName(d discount) string {
	if d.Title != "" {
		return d.Title
	}
	if d.Codes != nil && len(d.Codes.Nodes) > 0 {
		return d.Codes.Nodes[0].Code
	}
	return ""
}

// discountValue returns the CSV-style value (percentages negative, like Shopify's
// export) and value type.
func discountValue(d discount) (float64, string) {
	if isApp(d.Typename) {
		return 0, "app"
	}
	if d.CustomerGets == nil {
		return 0, "percentage" // free shipping rows export as percentage in the CSV
	}
	v := d.CustomerGets.Value
	switch v.Typename {
	case "DiscountPercentage":
		p := v.Percentage
		if p > 0 && p <= 1 { // API returns a fraction (0.15 == 15%)
			p *= 100
		}
		return -p, "percentage"
	case "DiscountAmount":
		amt := 0.0
		if v.Amount != nil {
			amt, _ = strconv.ParseFloat(v.Amount.Amount, 64)
		}
		return -amt, "fixed_amount"
	default:
		return 0, "percentage"
	}
}

// discountTypeClass mirrors the CSV's "Type" and "Discount Class" columns,
// derived from the GraphQL __typename (best-effort; Shopify does not expose a
// single class field stable across versions).
func discountTypeClass(d discount) (typ, class string) {
	switch {
	case isApp(d.Typename):
		return "App Discount", "multiple"
	case strings.Contains(d.Typename, "FreeShipping"):
		return "Free Shipping", "shipping"
	default:
		return "Amount Off", "product"
	}
}

func isApp(typename string) bool { return strings.HasSuffix(typename, "App") }

func titleStatus(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// normTime converts an RFC3339 timestamp to the same RFC3339 the CSV path emits,
// passing through "" and unparseable values unchanged.
func normTime(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format(time.RFC3339)
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

// moreCodesQuery pages through a single discount's redeem codes beyond the
// first page fetched by discountsQuery.
const moreCodesQuery = `
query MoreCodes($id: ID!, $cursor: String) {
  discountNode(id: $id) {
    discount {
      __typename
      ... on DiscountCodeBasic { codes(first: 250, after: $cursor) { pageInfo { hasNextPage endCursor } nodes { code asyncUsageCount } } }
      ... on DiscountCodeBxgy { codes(first: 250, after: $cursor) { pageInfo { hasNextPage endCursor } nodes { code asyncUsageCount } } }
      ... on DiscountCodeFreeShipping { codes(first: 250, after: $cursor) { pageInfo { hasNextPage endCursor } nodes { code asyncUsageCount } } }
      ... on DiscountCodeApp { codes(first: 250, after: $cursor) { pageInfo { hasNextPage endCursor } nodes { code asyncUsageCount } } }
    }
  }
}`

// discountsQuery pages through all discounts. Fields are limited to ones that
// are stable across recent Admin API versions to avoid query-validation errors.
const discountsQuery = `
query PullDiscounts($cursor: String) {
  discountNodes(first: 100, after: $cursor) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      discount {
        __typename
        ... on DiscountCodeBasic {
          title status startsAt endsAt asyncUsageCount usageLimit appliesOncePerCustomer
          codesCount { count }
          codes(first: 250) { pageInfo { hasNextPage endCursor } nodes { code asyncUsageCount } }
          combinesWith { orderDiscounts productDiscounts shippingDiscounts }
          customerGets { value {
            __typename
            ... on DiscountPercentage { percentage }
            ... on DiscountAmount { amount { amount } }
          } }
        }
        ... on DiscountCodeBxgy {
          title status startsAt endsAt asyncUsageCount usageLimit appliesOncePerCustomer
          codesCount { count }
          codes(first: 250) { pageInfo { hasNextPage endCursor } nodes { code asyncUsageCount } }
          combinesWith { orderDiscounts productDiscounts shippingDiscounts }
        }
        ... on DiscountCodeFreeShipping {
          title status startsAt endsAt asyncUsageCount usageLimit appliesOncePerCustomer
          codesCount { count }
          codes(first: 250) { pageInfo { hasNextPage endCursor } nodes { code asyncUsageCount } }
          combinesWith { orderDiscounts productDiscounts shippingDiscounts }
        }
        ... on DiscountCodeApp {
          title status startsAt endsAt asyncUsageCount usageLimit appliesOncePerCustomer
          codesCount { count }
          codes(first: 250) { pageInfo { hasNextPage endCursor } nodes { code asyncUsageCount } }
          combinesWith { orderDiscounts productDiscounts shippingDiscounts }
        }
        ... on DiscountAutomaticBasic {
          title status startsAt endsAt asyncUsageCount
          combinesWith { orderDiscounts productDiscounts shippingDiscounts }
          customerGets { value {
            __typename
            ... on DiscountPercentage { percentage }
            ... on DiscountAmount { amount { amount } }
          } }
        }
        ... on DiscountAutomaticBxgy {
          title status startsAt endsAt asyncUsageCount
          combinesWith { orderDiscounts productDiscounts shippingDiscounts }
        }
        ... on DiscountAutomaticFreeShipping {
          title status startsAt endsAt asyncUsageCount
          combinesWith { orderDiscounts productDiscounts shippingDiscounts }
        }
        ... on DiscountAutomaticApp {
          title status startsAt endsAt asyncUsageCount
        }
      }
    }
  }
}`
