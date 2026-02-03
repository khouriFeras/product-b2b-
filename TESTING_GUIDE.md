# Testing Guide - Step by Step

## Prerequisites
- Server running: `go run main.go`
- API key: `dev-key-123` (from your `.env`)

---

## Test 1: Product Listing Endpoint ✅

### Test basic listing:
```powershell
Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=5" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing
```

**Expected:** Status 200, JSON with products array

### Test pagination:
```powershell
# First page
$response1 = Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=2" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing

$json1 = $response1.Content | ConvertFrom-Json
Write-Host "First page - Has next: $($json1.pagination.hasNextPage)"
Write-Host "End cursor: $($json1.pagination.endCursor)"

# Second page (if hasNextPage is true)
if ($json1.pagination.hasNextPage) {
    $cursor = $json1.pagination.endCursor
    Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=2&cursor=$cursor" `
      -Headers @{Authorization="Bearer dev-key-123"} `
      -UseBasicParsing
}
```

**Expected:** Different products on second page

---

## Test 2: Single Product Endpoint ✅

### Get a product handle first:
```powershell
$response = Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=1" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing

$json = $response.Content | ConvertFrom-Json
$handle = $json.data[0].handle
$gid = $json.data[0].id

Write-Host "Testing with handle: $handle"
Write-Host "Testing with GID: $gid"
```

### Test by handle:
```powershell
Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products/$handle" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing
```

**Expected:** Status 200, single product JSON

### Test by GID:
```powershell
Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?id=$gid" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing
```

**Expected:** Status 200, same product JSON

### Test invalid product:
```powershell
Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products/nonexistent-product" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing
```

**Expected:** Status 404 or 403 (if product exists but not in collection)

---

## Test 3: Authentication ✅

### Test valid key:
```powershell
Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=1" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing
```

**Expected:** Status 200

### Test invalid key:
```powershell
Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=1" `
  -Headers @{Authorization="Bearer wrong-key"} `
  -UseBasicParsing
```

**Expected:** Status 401 Unauthorized

### Test missing key:
```powershell
Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=1" `
  -UseBasicParsing
```

**Expected:** Status 401 Unauthorized

---

## Test 4: Webhook Notifications ✅

### First, register webhooks:
```powershell
$BASE="http://localhost:3000"
$KEY="setup-please-change-me"

Invoke-WebRequest -Uri "$BASE/admin/setup/webhooks" `
  -Method POST `
  -Headers @{ "X-Setup-Key" = $KEY } `
  -UseBasicParsing
```

**Expected:** Status 200, JSON with `ok: true` for each webhook

### Then test webhooks:
1. **Update a product** in Shopify Admin (change title, description, or tags)
2. **Change inventory** for a product variant
3. **Watch your server logs** - you should see:
   ```
   [PARTNER NOTIFICATION] Event=update, Product=..., Changes=[...]
   ```

**Expected:** Detailed change messages in logs

---

## Test 5: Inventory Status Endpoint ✅

```powershell
Invoke-WebRequest -Uri "http://localhost:3000/debug/inventory-status" `
  -UseBasicParsing
```

**Expected:** Status 200, JSON with:
- `summary` (totalProducts, outOfStockCount, etc.)
- `products` array with inventory details and image URLs

---

## Test 6: Product Viewer Endpoint ✅

### HTML format (browser):
Open in browser:
```
http://localhost:3000/debug/product/shrb-mwktyl-mwhytw-lytly-lmmyz-mn-drynkmt-500-ml-khly-mn-lkhwl-wmnkhfd-ls-rt
```

**Expected:** Formatted HTML page with product details

### JSON format:
```powershell
Invoke-WebRequest -Uri "http://localhost:3000/debug/product/shrb-mwktyl-mwhytw-lytly-lmmyz-mn-drynkmt-500-ml-khly-mn-lkhwl-wmnkhfd-ls-rt?format=json" `
  -UseBasicParsing
```

**Expected:** Status 200, JSON product data

---

## Test 7: Health Check ✅

```powershell
Invoke-WebRequest -Uri "http://localhost:3000/health" -UseBasicParsing
```

**Expected:** Status 200, "ok"

---

## Test Results Checklist

- [ ] Product listing works
- [ ] Pagination works (cursor-based)
- [ ] Single product by handle works
- [ ] Single product by GID works
- [ ] Authentication rejects invalid keys
- [ ] Webhooks registered successfully
- [ ] Webhook notifications show detailed changes
- [ ] Inventory status endpoint works
- [ ] Product viewer HTML works
- [ ] Product viewer JSON works

---

## Common Issues

### 404 on endpoints
- **Fix:** Restart server after code changes

### 401 Unauthorized
- **Fix:** Check API key matches `PARTNER_API_KEYS` in `.env`

### Webhooks not triggering
- **Fix:** Verify webhooks are registered in Shopify Admin
- **Fix:** Check `PUBLIC_BASE_URL` is accessible (ngrok/public domain)

### "Product not in Partner Catalog"
- **Fix:** Ensure product is in the collection specified by `PARTNER_COLLECTION_HANDLE`
