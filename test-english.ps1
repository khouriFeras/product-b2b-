# Test script to fetch products in English (lang=en) and show title + description
# Also invokes debug/translations to see what Shopify returns for locale=en
$baseUrl = "http://localhost:3000"
$apiKey = "dev-key-123"

# --- Step 1: Check what Shopify returns for English translations (raw) ---
Write-Host "Step 1: Checking Shopify translations for locale=en for product 9049439961300..." -ForegroundColor Cyan
$debugUrl = "$baseUrl/debug/translations?product_id=gid://shopify/Product/9049439961300&locale=en"
$debugResp = Invoke-WebRequest -Uri $debugUrl -UseBasicParsing
$debugJson = $debugResp.Content | ConvertFrom-Json
Write-Host "  Translation keys returned by Shopify for locale=en: $($debugJson.count)" -ForegroundColor $(if ($debugJson.count -gt 0) { "Green" } else { "Yellow" })
if ($debugJson.count -eq 0) {
    Write-Host "  -> No English translations in Shopify for this product. Add English in Settings > Languages and translate the product." -ForegroundColor Yellow
} else {
    $keys = if ($debugJson.translations -is [System.Collections.IDictionary]) { $debugJson.translations.Keys } else { ($debugJson.translations | Get-Member -MemberType NoteProperty).Name }
    Write-Host "  Keys: $($keys -join ', ')" -ForegroundColor Gray
    if ($debugJson.translations.title) { Write-Host "  title (sample): $($debugJson.translations.title)" -ForegroundColor Gray }
}
Write-Host ""

# --- Step 2: Fetch products with lang=en ---
Write-Host "Step 2: Fetching products with lang=en and limit=1..." -ForegroundColor Cyan
$response = Invoke-WebRequest -Uri "$baseUrl/v1/catalog/products?lang=en&limit=1" `
  -Headers @{ Authorization = "Bearer $apiKey" } `
  -UseBasicParsing

$json = $response.Content | ConvertFrom-Json

if ($json.data.Count -eq 0) {
    Write-Host "No products returned." -ForegroundColor Yellow
    exit 1
}

$product = $json.data[0]

# Ensure UTF-8 for console output
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

Write-Host "`n--- Product (lang=en) ---" -ForegroundColor Green
Write-Host "ID:     $($product.id)"
Write-Host "Handle: $($product.handle)"
Write-Host "Title:  $($product.title)"
Write-Host "Vendor: $($product.vendor)"
Write-Host "Type:   $($product.productType)"
Write-Host "`nDescription (descriptionHtml):"
Write-Host $product.descriptionHtml
Write-Host "`n---" -ForegroundColor Green

# Save full response to file for inspection
$response.Content | Out-File -FilePath "product-english.json" -Encoding UTF8
Write-Host "Full response saved to product-english.json" -ForegroundColor Gray
