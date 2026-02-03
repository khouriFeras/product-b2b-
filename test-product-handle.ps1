# Test script to verify product handle and test the debug endpoint

Write-Host "Step 1: Get a product handle from the list..."
$response = Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=5" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing

$json = $response.Content | ConvertFrom-Json

Write-Host "`nFound $($json.data.Count) products:"
foreach ($product in $json.data) {
    Write-Host "  - Handle: $($product.handle)"
    Write-Host "    Title: $($product.title)"
    Write-Host ""
}

# Test with first product
if ($json.data.Count -gt 0) {
    $testHandle = $json.data[0].handle
    Write-Host "`nStep 2: Testing debug endpoint with handle: $testHandle"
    Write-Host "URL: http://localhost:3000/debug/product/$testHandle"
    Write-Host "`nOpening in browser..."
    Start-Process "http://localhost:3000/debug/product/$testHandle"
    
    Write-Host "`nOr test JSON format:"
    Write-Host "http://localhost:3000/debug/product/$testHandle?format=json"
}
