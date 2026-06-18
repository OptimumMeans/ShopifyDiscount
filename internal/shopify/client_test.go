package shopify

import (
	"encoding/json"
	"testing"
)

// sampleResponse mimics a Shopify discountNodes payload covering the discount
// shapes we map: a code percentage discount, a code fixed-amount discount, a
// code free-shipping discount, an automatic percentage discount, an app
// discount, an app-generated discount whose title differs from its code, and a
// bulk discount holding several codes.
const sampleResponse = `{
  "data": { "discountNodes": {
    "pageInfo": { "hasNextPage": false, "endCursor": "x" },
    "nodes": [
      { "id": "gid://shopify/DiscountNode/1", "discount": {
        "__typename": "DiscountCodeBasic",
        "title": "SUMMER15", "status": "ACTIVE",
        "startsAt": "2025-12-08T11:28:18-06:00", "endsAt": "2026-06-30T23:59:59-05:00",
        "asyncUsageCount": 6, "usageLimit": null, "appliesOncePerCustomer": false,
        "codes": { "nodes": [ { "code": "SUMMER15" } ] },
        "combinesWith": { "orderDiscounts": false, "productDiscounts": false, "shippingDiscounts": true },
        "customerGets": { "value": { "__typename": "DiscountPercentage", "percentage": 0.15 } }
      }},
      { "id": "gid://shopify/DiscountNode/2", "discount": {
        "__typename": "DiscountCodeBasic",
        "title": "TENOFF", "status": "EXPIRED",
        "asyncUsageCount": 3, "usageLimit": 100, "appliesOncePerCustomer": true,
        "codes": { "nodes": [ { "code": "TENOFF" } ] },
        "customerGets": { "value": { "__typename": "DiscountAmount", "amount": { "amount": "10.00" } } }
      }},
      { "id": "gid://shopify/DiscountNode/3", "discount": {
        "__typename": "DiscountCodeFreeShipping",
        "title": "FREESHIP", "status": "ACTIVE", "asyncUsageCount": 1,
        "codes": { "nodes": [ { "code": "FREESHIP" } ] }
      }},
      { "id": "gid://shopify/DiscountNode/4", "discount": {
        "__typename": "DiscountAutomaticBasic",
        "title": "Spend $50 save 20%", "status": "ACTIVE", "asyncUsageCount": 42,
        "customerGets": { "value": { "__typename": "DiscountPercentage", "percentage": 0.2 } }
      }},
      { "id": "gid://shopify/DiscountNode/5", "discount": {
        "__typename": "DiscountCodeApp",
        "title": "PARTNER100", "status": "ACTIVE", "asyncUsageCount": 9,
        "codes": { "nodes": [ { "code": "PARTNER100" } ] }
      }},
      { "id": "gid://shopify/DiscountNode/6", "discount": {
        "__typename": "DiscountCodeBasic",
        "title": "[Survey] AB12CD34", "status": "ACTIVE", "asyncUsageCount": 2,
        "codes": { "nodes": [ { "code": "AB12CD34", "asyncUsageCount": 2 } ] },
        "customerGets": { "value": { "__typename": "DiscountPercentage", "percentage": 0.1 } }
      }},
      { "id": "gid://shopify/DiscountNode/7", "discount": {
        "__typename": "DiscountCodeBasic",
        "title": "BULK-PLACEHOLDER", "status": "ACTIVE", "asyncUsageCount": 5,
        "codes": { "pageInfo": { "hasNextPage": false },
          "nodes": [ { "code": "BULK-AAA", "asyncUsageCount": 3 },
                     { "code": "BULK-BBB", "asyncUsageCount": 2 },
                     { "code": "BULK-CCC", "asyncUsageCount": 0 } ] },
        "customerGets": { "value": { "__typename": "DiscountPercentage", "percentage": 0.25 } }
      }}
    ]
  }}
}`

func TestNodesToRows(t *testing.T) {
	var resp gqlResponse
	if err := json.Unmarshal([]byte(sampleResponse), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rows := nodesToRows(resp.Data.DiscountNodes.Nodes)
	// 5 single/automatic + 1 app-generated + 3 bulk codes = 9 rows.
	if len(rows) != 9 {
		t.Fatalf("got %d rows, want 9", len(rows))
	}
	by := map[string]int{}
	for i, r := range rows {
		by[r.Name] = i
	}

	pctOff := rows[by["SUMMER15"]]
	if pctOff.Value != -15 || pctOff.ValueType != "percentage" {
		t.Errorf("SUMMER15 value=%v type=%q, want -15 percentage", pctOff.Value, pctOff.ValueType)
	}
	if pctOff.TimesUsed != 6 || pctOff.Status != "Active" {
		t.Errorf("SUMMER15 used=%d status=%q, want 6 Active", pctOff.TimesUsed, pctOff.Status)
	}
	if pctOff.Type != "Amount Off" || pctOff.DiscountClass != "product" {
		t.Errorf("SUMMER15 type=%q class=%q", pctOff.Type, pctOff.DiscountClass)
	}
	if pctOff.CombinesShipping != "Yes" || pctOff.CombinesOrder != "No" {
		t.Errorf("SUMMER15 combines wrong: %+v", pctOff)
	}

	ten := rows[by["TENOFF"]]
	if ten.Value != -10 || ten.ValueType != "fixed_amount" {
		t.Errorf("TENOFF value=%v type=%q, want -10 fixed_amount", ten.Value, ten.ValueType)
	}
	if ten.UsageLimit != "100" || ten.AppliesOnce != "Yes" || ten.Status != "Expired" {
		t.Errorf("TENOFF limit=%q once=%q status=%q", ten.UsageLimit, ten.AppliesOnce, ten.Status)
	}

	ship := rows[by["FREESHIP"]]
	if ship.Type != "Free Shipping" || ship.DiscountClass != "shipping" {
		t.Errorf("FREESHIP type=%q class=%q", ship.Type, ship.DiscountClass)
	}

	auto := rows[by["Spend $50 save 20%"]]
	if auto.Value != -20 || auto.TimesUsed != 42 {
		t.Errorf("automatic value=%v used=%d, want -20 42", auto.Value, auto.TimesUsed)
	}

	app := rows[by["PARTNER100"]]
	if app.ValueType != "app" || app.Type != "App Discount" {
		t.Errorf("app valueType=%q type=%q", app.ValueType, app.Type)
	}

	// App-generated discounts must be named by their title (matching the CSV),
	// not by their differing code.
	if _, ok := by["[Survey] AB12CD34"]; !ok {
		t.Errorf("app-generated discount should be keyed by title, got names: %v", by)
	}
	if _, ok := by["AB12CD34"]; ok {
		t.Errorf("app-generated discount was keyed by code, want title")
	}

	// Multi-code (bulk) discount emits one row per code, keyed by code, with each
	// code's own usage — not a single aggregate row.
	if _, ok := by["BULK-PLACEHOLDER"]; ok {
		t.Errorf("bulk discount should not emit an aggregate title row")
	}
	for code, want := range map[string]int{"BULK-AAA": 3, "BULK-BBB": 2, "BULK-CCC": 0} {
		i, ok := by[code]
		if !ok {
			t.Errorf("missing per-code row %q", code)
			continue
		}
		if rows[i].TimesUsed != want {
			t.Errorf("%s TimesUsed=%d, want %d", code, rows[i].TimesUsed, want)
		}
		if rows[i].Value != -25 || rows[i].Status != "Active" {
			t.Errorf("%s did not inherit discount attrs: value=%v status=%q", code, rows[i].Value, rows[i].Status)
		}
	}
}
