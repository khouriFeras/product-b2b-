# Test script to verify Arabic text is correct in JSON
$response = Invoke-WebRequest -Uri "http://localhost:3000/v1/catalog/products?limit=1" `
  -Headers @{Authorization="Bearer dev-key-123"} `
  -UseBasicParsing

$json = $response.Content | ConvertFrom-Json
$product = $json.data[0]

# Save to file to see proper encoding
$json.Content | Out-File -FilePath "product-test.json" -Encoding UTF8

Write-Host "`nProduct data saved to product-test.json"
Write-Host "Open the file in a text editor (VS Code, Notepad++) to see Arabic text correctly"
Write-Host "`nOr view in browser:"
Write-Host "http://localhost:3000/debug/product/$($product.handle)"

# Try to display with proper encoding
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Write-Host "`nProduct Title: $($product.title)"
Write-Host "Product Handle: $($product.handle)"
