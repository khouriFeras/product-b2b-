package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Types and cache are now in models.go and cache.go

// syncCollectionMembership periodically checks the Partner Catalog collection
// and detects products that were added or removed, without requiring product edits
func syncCollectionMembership(shop, token, ver, collHandle, collTitle string) {
	if collHandle == "" && collTitle == "" {
		log.Printf("[SYNC] Skipping collection sync: no collection handle or title configured")
		return
	}

	log.Printf("[SYNC] Starting collection membership sync...")

	// Fetch all products currently in the Partner Catalog collection
	var allProductGIDs []string
	cursor := ""
	limit := 250

	for {
		var data []byte
		var err error

		if collHandle != "" {
			data, err = fetchProductsByCollectionHandlePaginated(shop, token, ver, collHandle, cursor, limit)
		} else {
			collectionID, findErr := findCollectionIDByTitle(shop, token, ver, collTitle)
			if findErr != nil {
				log.Printf("[SYNC] Failed to find collection: %v", findErr)
				return
			}
			if collectionID == "" {
				log.Printf("[SYNC] Collection not found")
				return
			}
			data, err = fetchCollectionProductsPaginated(shop, token, ver, collectionID, cursor, limit)
		}

		if err != nil {
			log.Printf("[SYNC] Failed to fetch products: %v", err)
			return
		}

		// Parse response to extract product GIDs
		var apiResp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Pagination struct {
				HasNextPage bool   `json:"hasNextPage"`
				NextCursor  string `json:"nextCursor"`
			} `json:"pagination"`
		}

		if err := json.Unmarshal(data, &apiResp); err != nil {
			log.Printf("[SYNC] Failed to parse response: %v", err)
			return
		}

		// Collect all product GIDs
		for _, product := range apiResp.Data {
			if product.ID != "" {
				allProductGIDs = append(allProductGIDs, product.ID)
			}
		}

		// Check if there are more pages
		if !apiResp.Pagination.HasNextPage {
			break
		}
		cursor = apiResp.Pagination.NextCursor
	}

	log.Printf("[SYNC] Found %d products in Partner Catalog collection", len(allProductGIDs))

	// Create a map of current product GIDs for fast lookup
	currentProducts := make(map[string]bool)
	for _, gid := range allProductGIDs {
		currentProducts[gid] = true
	}

	// Find products that were ADDED to collection
	var addedProducts []string
	for _, gid := range allProductGIDs {
		if cached, ok := productStateCache.Load(gid); ok {
			previousState := cached.(*ProductState)
			if !previousState.InPartnerCatalog {
				// Product was in cache but marked as NOT in collection, now it IS
				addedProducts = append(addedProducts, gid)
			}
		} else {
			// Product not in cache at all - might be newly added
			// We'll treat it as added if it's the first time we see it
			addedProducts = append(addedProducts, gid)
		}
	}

	// Find products that were REMOVED from collection
	var removedProducts []string
	productStateCache.Range(func(key, value interface{}) bool {
		productGID := key.(string)
		previousState := value.(*ProductState)

		// Only check products that were previously in the collection
		if previousState.InPartnerCatalog {
			if !currentProducts[productGID] {
				// Product was in collection before, but not anymore
				removedProducts = append(removedProducts, productGID)
			}
		}
		return true
	})

	// Notify about additions
	for _, productGID := range addedProducts {
		log.Printf("[COLLECTION CHANGE] Product %s ADDED to Partner Catalog (detected via sync)", productGID)

		// Fetch product details for notification
		productState, err := fetchProductState(shop, token, ver, productGID)
		if err != nil {
			log.Printf("[SYNC] Failed to fetch product state for %s: %v", productGID, err)
			continue
		}

		productState.InPartnerCatalog = true
		productStateCache.Store(productGID, productState)

		// Create a mock payload for notification
		var payload struct {
			ID          int64  `json:"id"`
			Title       string `json:"title"`
			Handle      string `json:"handle"`
			Vendor      string `json:"vendor"`
			ProductType string `json:"product_type"`
			Status      string `json:"status"`
			UpdatedAt   string `json:"updated_at"`
		}

		// Extract numeric ID from GID
		if strings.HasPrefix(productGID, "gid://shopify/Product/") {
			var productID int64
			fmt.Sscanf(productGID, "gid://shopify/Product/%d", &productID)
			payload.ID = productID
		}
		payload.Title = productState.Title
		payload.Status = productState.Status
		payload.UpdatedAt = productState.UpdatedAt.Format(time.RFC3339)

		notifyPartners(productGID, "collection_added", payload, []string{"Product added to Partner Catalog collection (detected via background sync)"})
	}

	// Notify about removals
	for _, productGID := range removedProducts {
		log.Printf("[COLLECTION CHANGE] Product %s REMOVED from Partner Catalog (detected via sync)", productGID)

		// Get previous state for notification
		if cached, ok := productStateCache.Load(productGID); ok {
			previousState := cached.(*ProductState)

			var payload struct {
				ID          int64  `json:"id"`
				Title       string `json:"title"`
				Handle      string `json:"handle"`
				Vendor      string `json:"vendor"`
				ProductType string `json:"product_type"`
				Status      string `json:"status"`
				UpdatedAt   string `json:"updated_at"`
			}

			// Extract numeric ID from GID
			if strings.HasPrefix(productGID, "gid://shopify/Product/") {
				var productID int64
				fmt.Sscanf(productGID, "gid://shopify/Product/%d", &productID)
				payload.ID = productID
			}
			payload.Title = previousState.Title
			payload.Status = previousState.Status
			payload.UpdatedAt = previousState.UpdatedAt.Format(time.RFC3339)

			notifyPartners(productGID, "collection_removed", payload, []string{"Product removed from Partner Catalog collection (detected via background sync)"})

			// Update cache to mark as not in collection
			previousState.InPartnerCatalog = false
			productStateCache.Store(productGID, previousState)
		} else {
			// Remove from cache if we can't find previous state
			productStateCache.Delete(productGID)
		}
	}

	// Update cache for all current products to mark them as in collection
	for _, productGID := range allProductGIDs {
		if cached, ok := productStateCache.Load(productGID); ok {
			state := cached.(*ProductState)
			state.InPartnerCatalog = true
			productStateCache.Store(productGID, state)
		} else {
			// Product not in cache - fetch and store it
			productState, err := fetchProductState(shop, token, ver, productGID)
			if err == nil {
				productState.InPartnerCatalog = true
				productStateCache.Store(productGID, productState)
			}
		}
	}

	if len(addedProducts) > 0 || len(removedProducts) > 0 {
		log.Printf("[SYNC] Sync complete: %d added, %d removed", len(addedProducts), len(removedProducts))
	} else {
		log.Printf("[SYNC] Sync complete: no changes detected")
	}
}

func main() {
	_ = godotenv.Load() // loads .env if present

	clientID := os.Getenv("SHOPIFY_CLIENT_ID")
	clientSecret := os.Getenv("SHOPIFY_CLIENT_SECRET")
	redirectURI := os.Getenv("APP_REDIRECT_URI")

	shop := os.Getenv("SHOPIFY_SHOP")
	token := os.Getenv("SHOPIFY_ADMIN_TOKEN")
	ver := os.Getenv("SHOPIFY_API_VERSION")
	collHandle := os.Getenv("PARTNER_COLLECTION_HANDLE")
	collTitle := os.Getenv("PARTNER_COLLECTION_TITLE")
	partnerAPIKeysStr := os.Getenv("PARTNER_API_KEYS") // Format: "partnerA:KEY1,partnerB:KEY2"
	if collTitle == "" {
		collTitle = "Partner Catalog"
	}

	// Parse partner API keys
	partnerAPIKeys := parsePartnerAPIKeys(partnerAPIKeysStr)
	if len(partnerAPIKeys) == 0 {
		log.Fatal("PARTNER_API_KEYS env var is required. Format: partnerA:KEY1,partnerB:KEY2")
	}

	// Override token if .shopify_token file exists (from OAuth flow)
	if b, err := os.ReadFile(".shopify_token"); err == nil {
		token = strings.TrimSpace(string(b))
		log.Println("Loaded token from .shopify_token file")
	}

	if shop == "" || token == "" || ver == "" {
		log.Fatal("Missing env vars. Need SHOPIFY_SHOP, SHOPIFY_ADMIN_TOKEN, SHOPIFY_API_VERSION")
	}

	// Register specific routes first (before root route)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	// Start OAuth: /auth?shop=xxxx.myshopify.com
	http.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		shopParam := r.URL.Query().Get("shop")
		if shopParam == "" {
			http.Error(w, "missing shop query param, e.g. /auth?shop=xxxx.myshopify.com", 400)
			return
		}

		if clientID == "" || clientSecret == "" || redirectURI == "" {
			http.Error(w, "missing OAuth env vars: SHOPIFY_CLIENT_ID, SHOPIFY_CLIENT_SECRET, APP_REDIRECT_URI", 500)
			return
		}

		// For MVP: fixed state (you should store per-session in production)
		state := "devstate123"

		// Requested scopes (must match what you set in Dev Dashboard)
		scope := "read_products,read_inventory,read_locations,read_translations,read_locales"

		authURL := fmt.Sprintf(
			"https://%s/admin/oauth/authorize?client_id=%s&scope=%s&redirect_uri=%s&state=%s",
			shopParam,
			url.QueryEscape(clientID),
			url.QueryEscape(scope),
			url.QueryEscape(redirectURI),
			url.QueryEscape(state),
		)
		http.Redirect(w, r, authURL, http.StatusFound)
	})

	// OAuth callback
	http.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		shopParam := q.Get("shop")
		code := q.Get("code")
		h := q.Get("hmac")
		state := q.Get("state")

		if shopParam == "" || code == "" || h == "" {
			http.Error(w, "missing shop/code/hmac", 400)
			return
		}

		if clientSecret == "" {
			http.Error(w, "missing SHOPIFY_CLIENT_SECRET env var", 500)
			return
		}

		// Verify state to prevent CSRF attacks
		if state != "devstate123" {
			http.Error(w, "invalid state parameter", 400)
			return
		}

		// Basic shop domain validation
		if !strings.HasSuffix(shopParam, ".myshopify.com") {
			http.Error(w, "invalid shop domain", 400)
			return
		}

		// Verify HMAC (recommended even in dev)
		if !verifyShopifyHMAC(q, clientSecret) {
			http.Error(w, "invalid hmac", 401)
			return
		}

		token, err := exchangeCodeForToken(shopParam, code, clientID, clientSecret)
		if err != nil {
			http.Error(w, "token exchange failed: "+err.Error(), 500)
			return
		}

		// Persist token to file automatically
		if err := os.WriteFile(".shopify_token", []byte(token), 0600); err != nil {
			log.Printf("Warning: Failed to save token to file: %v", err)
		} else {
			log.Println("Token saved to .shopify_token file")
		}

		// Print token for reference
		log.Println("ACCESS TOKEN:", token)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("Installed OK. Token saved to .shopify_token file.\n"))
	})

	// Debug endpoint to check what scopes this token actually has
	http.HandleFunc("/debug/access-scopes", func(w http.ResponseWriter, r *http.Request) {
		endpoint := fmt.Sprintf("https://%s/admin/oauth/access_scopes.json", shop)

		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		req.Header.Set("X-Shopify-Access-Token", token)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()

		raw, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(raw)
	})

	// Helper endpoint to list all collections and their handles
	http.HandleFunc("/debug/list-collections", func(w http.ResponseWriter, r *http.Request) {
		data, err := listAllCollections(shop, token, ver)
		if err != nil {
			http.Error(w, "failed to list collections: "+err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	// Debug endpoint: GET /debug/translations?product_id=GID&locale=en
	// Returns raw Shopify translatableResource translations for that product and locale.
	// Use this to verify whether Shopify has English (or other) translations for a product.
	handleDebugTranslations := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/translations" && r.URL.Path != "/debug/translations/" {
			http.NotFound(w, r)
			return
		}
		productID := r.URL.Query().Get("product_id")
		locale := r.URL.Query().Get("locale")
		if productID == "" || locale == "" {
			http.Error(w, "query params required: product_id (e.g. gid://shopify/Product/9049439961300) and locale (e.g. en)", 400)
			return
		}
		trans, err := fetchProductTranslations(shop, token, ver, productID, locale)
		if err != nil {
			http.Error(w, "fetch translations failed: "+err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"product_id":   productID,
			"locale":       locale,
			"translations": trans,
			"count":        len(trans),
		})
	}
	http.HandleFunc("/debug/translations", handleDebugTranslations)
	http.HandleFunc("/debug/translations/", handleDebugTranslations)

	// Production API endpoint: GET /v1/catalog/products
	http.HandleFunc("/v1/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		// Only handle exact path (no trailing path segments)
		if r.URL.Path != "/v1/catalog/products" {
			http.NotFound(w, r)
			return
		}

		// Authenticate partner
		if !authenticatePartner(r, partnerAPIKeys) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized: Invalid or missing API key", 401)
			return
		}

		// Parse pagination parameters
		cursor := r.URL.Query().Get("cursor")
		limit := 25 // Default page size
		if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
			if parsed, err := fmt.Sscanf(limitStr, "%d", &limit); err != nil || parsed != 1 || limit < 1 || limit > 100 {
				http.Error(w, "Invalid limit parameter (must be 1-100)", 400)
				return
			}
		}

		// Parse language: default is ar (Arabic); use lang=en for English
		lang := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("lang")))
		if lang == "" {
			lang = "ar"
		}
		if lang != "en" && lang != "ar" {
			http.Error(w, "Invalid lang parameter (use en or ar)", 400)
			return
		}

		// Fetch products with pagination
		var data []byte
		var err error
		if collHandle != "" {
			data, err = fetchProductsByCollectionHandlePaginated(shop, token, ver, collHandle, cursor, limit)
		} else {
			// Fallback to collection ID lookup
			collectionID, findErr := findCollectionIDByTitle(shop, token, ver, collTitle)
			if findErr != nil {
				http.Error(w, "find collection failed: "+findErr.Error(), 500)
				return
			}
			if collectionID == "" {
				http.Error(w, "collection not found", 404)
				return
			}
			data, err = fetchCollectionProductsPaginated(shop, token, ver, collectionID, cursor, limit)
		}

		if err != nil {
			http.Error(w, "fetch products failed: "+err.Error(), 500)
			return
		}

		// If a language is requested (en or ar), fetch translations and merge into product fields
		if lang == "en" || lang == "ar" {
			var payload struct {
				Data       []map[string]interface{} `json:"data"`
				Pagination map[string]interface{}   `json:"pagination"`
				Meta       map[string]interface{}   `json:"meta"`
			}
			if err := json.Unmarshal(data, &payload); err == nil && len(payload.Data) > 0 {
				var gids []string
				for _, p := range payload.Data {
					if id, _ := p["id"].(string); id != "" {
						gids = append(gids, id)
					}
				}
				transByProduct, transErr := fetchProductTranslationsBatchWithFallback(shop, token, ver, gids, lang)
				if transErr != nil {
					transByProduct = make(map[string]map[string]string)
				}
				// Fallback: for any product with no translations from batch, fetch one-by-one (same path as debug/translations)
				for _, p := range payload.Data {
					id, _ := p["id"].(string)
					if id == "" {
						continue
					}
					trans := transByProduct[id]
					if len(trans) == 0 {
						singleTrans, err := fetchProductTranslationsWithFallback(shop, token, ver, id, lang)
						if err == nil && len(singleTrans) > 0 {
							transByProduct[id] = singleTrans
						}
					}
				}
				for i, p := range payload.Data {
					if id, _ := p["id"].(string); id != "" {
						if trans, ok := transByProduct[id]; ok && len(trans) > 0 {
							applyTranslationsToProductMap(p, trans)
						}
					}
					payload.Data[i] = p
				}
				if data, err = json.Marshal(payload); err != nil {
					http.Error(w, "failed to encode response", 500)
					return
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Language", lang) // ar = default (Arabic), en = English
		w.Write(data)
	})

	// Single product endpoint: GET /v1/catalog/products/{id}
	http.HandleFunc("/v1/catalog/products/", func(w http.ResponseWriter, r *http.Request) {
		// Authenticate partner
		if !authenticatePartner(r, partnerAPIKeys) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized: Invalid or missing API key", 401)
			return
		}

		// Extract product identifier: sku, id (GID), or path (handle)
		// Support: /v1/catalog/products?sku=XXX, ?id=gid://..., or /v1/catalog/products/{handle}
		var productID string

		if skuParam := strings.TrimSpace(r.URL.Query().Get("sku")); skuParam != "" {
			// Resolve SKU to product GID
			productGIDFromSKU, err := getProductGIDBySKU(shop, token, ver, skuParam)
			if err != nil {
				http.Error(w, "lookup by SKU failed: "+err.Error(), 500)
				return
			}
			if productGIDFromSKU == "" {
				http.Error(w, "Product not found for SKU: "+skuParam, 404)
				return
			}
			productID = productGIDFromSKU
		} else if idParam := r.URL.Query().Get("id"); idParam != "" {
			productID = idParam
		} else {
			// Extract from path (for handles)
			pathSuffix := strings.TrimPrefix(r.URL.Path, "/v1/catalog/products/")
			if pathSuffix == "" {
				http.Error(w, "Product identifier is required (use ?sku= for SKU, ?id= for GID, or /handle for handle)", 400)
				return
			}
			productID = pathSuffix
		}

		// URL decode the product ID in case it has special characters
		if decoded, err := url.QueryUnescape(productID); err == nil {
			productID = decoded
		}

		// Parse language: default is ar (Arabic); use lang=en for English
		lang := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("lang")))
		if lang == "" {
			lang = "ar"
		}
		if lang != "en" && lang != "ar" {
			http.Error(w, "Invalid lang parameter (use en or ar)", 400)
			return
		}

		// Fetch single product
		data, err := fetchSingleProduct(shop, token, ver, productID)
		if err != nil {
			// Check for "not found" errors
			errMsg := err.Error()
			if strings.Contains(errMsg, "not found") ||
				strings.Contains(errMsg, "404") ||
				strings.Contains(errMsg, "null") ||
				strings.Contains(errMsg, "Cannot return null") {
				http.Error(w, "Product not found: "+productID, 404)
				return
			}
			// Return detailed error for debugging
			http.Error(w, "fetch product failed: "+errMsg, 500)
			return
		}

		// Enforce: product must be in Partner Catalog collection
		//
		// PERFORMANCE NOTE: This requires 2 GraphQL API calls:
		//   1. fetchSingleProduct() - fetches the product data
		//   2. isProductInCollection() - checks if product is in Partner Catalog
		//
		// This is acceptable for MVP but has limitations:
		//   - Slow: 2 API calls per request adds latency
		//   - Throttling risk: More API calls = higher chance of hitting Shopify rate limits
		//   - Reliability: If collection check fails, entire request fails
		//
		// TODO (Production optimization):
		//   - Cache collection membership in memory (TTL-based)
		//   - Maintain DB table of "allowed product GIDs" updated via webhooks
		//   - Include collections in initial product query (if permissions allow)
		//   - Pre-populate allowed products list on startup from collection query
		//
		// Extract product GID from response to check collection membership
		// Note: fetchSingleProduct returns {"data": productObject}, not {"data": {"product": ...}}
		var productResp struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &productResp); err != nil {
			// If we can't parse the response, deny access (security: fail closed)
			http.Error(w, "Failed to verify product access: invalid response format", 500)
			return
		}

		productGID := productResp.Data.ID
		if productGID == "" {
			// If we can't extract product GID, deny access (security: fail closed)
			http.Error(w, "Failed to verify product access: product ID not found in response", 500)
			return
		}

		// Check if product is in Partner Catalog collection
		inCollection, err := isProductInCollection(shop, token, ver, productGID, collHandle)
		if err != nil {
			http.Error(w, "Failed to verify product access: "+err.Error(), 500)
			return
		}
		if !inCollection {
			http.Error(w, "Forbidden: Product is not in Partner Catalog collection", 403)
			return
		}

		// If a language is requested (en or ar), fetch translations and merge into product fields
		if (lang == "en" || lang == "ar") && productGID != "" {
			var singlePayload struct {
				Data map[string]interface{} `json:"data"`
			}
			if err := json.Unmarshal(data, &singlePayload); err == nil && singlePayload.Data != nil {
				trans, transErr := fetchProductTranslationsWithFallback(shop, token, ver, productGID, lang)
				if transErr == nil {
					applyTranslationsToProductMap(singlePayload.Data, trans)
					if data, err = json.Marshal(singlePayload); err != nil {
						http.Error(w, "failed to encode response", 500)
						return
					}
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Language", lang) // ar = default (Arabic), en = English
		w.Write(data)
	})

	// Webhook endpoints for Shopify product and inventory updates
	// POST /webhooks/products/update
	http.HandleFunc("/webhooks/products/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		handleProductWebhook(w, r, clientSecret, shop, token, ver, collHandle, "update")
	})

	// POST /webhooks/products/delete
	http.HandleFunc("/webhooks/products/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		handleProductWebhook(w, r, clientSecret, shop, token, ver, collHandle, "delete")
	})

	// POST /webhooks/inventory_levels/update
	http.HandleFunc("/webhooks/inventory_levels/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		handleInventoryWebhook(w, r, clientSecret, shop, token, ver, collHandle)
	})

	// Admin endpoint to register webhooks with Shopify
	// POST /admin/setup/webhooks
	http.HandleFunc("/admin/setup/webhooks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}

		setupKey := os.Getenv("ADMIN_SETUP_KEY")
		if setupKey == "" {
			http.Error(w, "ADMIN_SETUP_KEY missing", 500)
			return
		}

		// Simple admin auth header: X-Setup-Key: <ADMIN_SETUP_KEY>
		if r.Header.Get("X-Setup-Key") != setupKey {
			http.Error(w, "Unauthorized", 401)
			return
		}

		// Allow optional JSON body to override PUBLIC_BASE_URL and endpoints.
		// Body schema (optional):
		// {
		//   "public_base": "https://example.com",
		//   "endpoints": {
		//     "PRODUCTS_UPDATE": "https://.../webhooks/products/update",
		//     "PRODUCTS_DELETE": "https://.../webhooks/products/delete"
		//   }
		// }

		type setupBody struct {
			PublicBase string            `json:"public_base"`
			Endpoints  map[string]string `json:"endpoints"`
		}

		var sb setupBody
		// Read body but don't require it; if empty, fall back to env
		bodyBytes, _ := io.ReadAll(r.Body)
		if len(bodyBytes) > 0 {
			if err := json.Unmarshal(bodyBytes, &sb); err != nil {
				http.Error(w, "Invalid JSON body: "+err.Error(), 400)
				return
			}
		}

		publicBase := strings.TrimRight(sb.PublicBase, "/")
		if publicBase == "" {
			publicBase = strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/")
		}
		if publicBase == "" {
			http.Error(w, "PUBLIC_BASE_URL missing", 500)
			return
		}

		topics := []string{
			"PRODUCTS_UPDATE",
			"PRODUCTS_DELETE",
			"INVENTORY_LEVELS_UPDATE",
		}

		endpoints := map[string]string{
			"PRODUCTS_UPDATE":         publicBase + "/webhooks/products/update",
			"PRODUCTS_DELETE":         publicBase + "/webhooks/products/delete",
			"INVENTORY_LEVELS_UPDATE": publicBase + "/webhooks/inventory_levels/update",
		}
		// Merge/override with provided endpoints from body if present
		for k, v := range sb.Endpoints {
			if strings.TrimSpace(v) != "" {
				endpoints[k] = v
			}
		}

		results := make([]map[string]interface{}, 0, len(topics))
		for _, topic := range topics {
			cb := endpoints[topic]
			id, err := ensureWebhook(shop, token, ver, topic, cb)
			if err != nil {
				results = append(results, map[string]interface{}{
					"topic": topic, "callback": cb, "ok": false, "error": err.Error(),
				})
				continue
			}
			results = append(results, map[string]interface{}{
				"topic": topic, "callback": cb, "ok": true, "id": id,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": results,
		})
	})

	// Debug endpoint to check inventory status for all products in Partner Catalog
	http.HandleFunc("/debug/inventory-status", func(w http.ResponseWriter, r *http.Request) {
		// Fetch all products from Partner Catalog
		var data []byte
		var err error
		if collHandle != "" {
			// Fetch with a large limit to get all products
			data, err = fetchProductsByCollectionHandlePaginated(shop, token, ver, collHandle, "", 250)
		} else {
			collectionID, findErr := findCollectionIDByTitle(shop, token, ver, collTitle)
			if findErr != nil {
				http.Error(w, "find collection failed: "+findErr.Error(), 500)
				return
			}
			if collectionID == "" {
				http.Error(w, "collection not found", 404)
				return
			}
			data, err = fetchCollectionProductsPaginated(shop, token, ver, collectionID, "", 250)
		}

		if err != nil {
			http.Error(w, "fetch products failed: "+err.Error(), 500)
			return
		}

		// Parse the response from our paginated helper:
		// { "data": [ { ...product... } ], "pagination": { ... }, "meta": { ... } }
		var apiResp struct {
			Data []struct {
				ID            string `json:"id"`
				Title         string `json:"title"`
				Handle        string `json:"handle"`
				Status        string `json:"status"`
				FeaturedImage *struct {
					URL string `json:"url"`
				} `json:"featuredImage"`
				Images struct {
					Nodes []struct {
						URL string `json:"url"`
					} `json:"nodes"`
				} `json:"images"`
				Variants struct {
					Nodes []struct {
						ID                string `json:"id"`
						SKU               string `json:"sku"`
						Price             string `json:"price"`
						InventoryQuantity int    `json:"inventoryQuantity"`
					} `json:"nodes"`
				} `json:"variants"`
			} `json:"data"`
		}

		if err := json.Unmarshal(data, &apiResp); err != nil {
			http.Error(w, "failed to parse response: "+err.Error(), 500)
			return
		}

		// Build inventory report
		type VariantInfo struct {
			SKU               string `json:"sku"`
			Price             string `json:"price"`
			InventoryQuantity int    `json:"inventoryQuantity"`
			OutOfStock        bool   `json:"outOfStock"`
		}

		type InventoryReport struct {
			ProductID  string        `json:"productId"`
			Title      string        `json:"title"`
			Handle     string        `json:"handle"`
			Status     string        `json:"status"`
			ImageURLs  string        `json:"imageUrls"` // Semicolon-separated image URLs
			TotalStock int           `json:"totalStock"`
			OutOfStock bool          `json:"outOfStock"`
			Variants   []VariantInfo `json:"variants"`
		}

		var report []InventoryReport
		for _, p := range apiResp.Data {
			totalStock := 0
			allOutOfStock := true
			var variantInfos []VariantInfo

			for _, v := range p.Variants.Nodes {
				totalStock += v.InventoryQuantity
				if v.InventoryQuantity > 0 {
					allOutOfStock = false
				}
				variantInfos = append(variantInfos, VariantInfo{
					SKU:               v.SKU,
					Price:             v.Price,
					InventoryQuantity: v.InventoryQuantity,
					OutOfStock:        v.InventoryQuantity == 0,
				})
			}

			// Collect all image URLs
			var imageURLs []string
			// Add featured image first if available
			if p.FeaturedImage != nil && p.FeaturedImage.URL != "" {
				imageURLs = append(imageURLs, p.FeaturedImage.URL)
			}
			// Add all other images
			for _, img := range p.Images.Nodes {
				if img.URL != "" {
					// Avoid duplicates (featured image might be in images list too)
					isDuplicate := false
					for _, existing := range imageURLs {
						if existing == img.URL {
							isDuplicate = true
							break
						}
					}
					if !isDuplicate {
						imageURLs = append(imageURLs, img.URL)
					}
				}
			}
			// Join with semicolon
			imageURLsStr := strings.Join(imageURLs, ";")

			report = append(report, InventoryReport{
				ProductID:  p.ID,
				Title:      p.Title,
				Handle:     p.Handle,
				Status:     p.Status,
				ImageURLs:  imageURLsStr,
				TotalStock: totalStock,
				OutOfStock: allOutOfStock && len(p.Variants.Nodes) > 0,
				Variants:   variantInfos,
			})
		}

		// Calculate summary
		totalProducts := len(report)
		outOfStockCount := 0
		totalInventory := 0
		for _, r := range report {
			if r.OutOfStock {
				outOfStockCount++
			}
			totalInventory += r.TotalStock
		}

		response := map[string]interface{}{
			"summary": map[string]interface{}{
				"totalProducts":   totalProducts,
				"outOfStockCount": outOfStockCount,
				"inStockCount":    totalProducts - outOfStockCount,
				"totalInventory":  totalInventory,
			},
			"products": report,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// Debug endpoint to view a product by handle in browser (no auth required)
	http.HandleFunc("/debug/product/", func(w http.ResponseWriter, r *http.Request) {
		// Extract handle from path: /debug/product/{handle}
		pathSuffix := strings.TrimPrefix(r.URL.Path, "/debug/product/")
		if pathSuffix == "" {
			http.Error(w, "Product handle is required. Usage: /debug/product/{handle}", 400)
			return
		}

		// Clean up the path - remove any query parameters or fragments
		handle := strings.Split(pathSuffix, "?")[0]
		handle = strings.Split(handle, "#")[0]
		handle = strings.TrimSpace(handle)

		// URL decode in case of special characters
		if decoded, err := url.QueryUnescape(handle); err == nil {
			handle = decoded
		}

		// Log for debugging
		log.Printf("Debug product endpoint - extracted handle: %q", handle)

		// Fetch product
		data, err := fetchSingleProduct(shop, token, ver, handle)
		if err != nil {
			errMsg := err.Error()
			if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "Cannot return null") {
				http.Error(w, fmt.Sprintf("Product not found with handle: %q. Error: %s", handle, errMsg), 404)
				return
			}
			http.Error(w, fmt.Sprintf("Failed to fetch product: %s (handle: %q)", errMsg, handle), 500)
			return
		}

		// Parse product data
		var productResp struct {
			Data struct {
				ID              string   `json:"id"`
				Title           string   `json:"title"`
				Handle          string   `json:"handle"`
				Status          string   `json:"status"`
				Vendor          string   `json:"vendor"`
				ProductType     string   `json:"productType"`
				Tags            []string `json:"tags"`
				DescriptionHTML string   `json:"descriptionHtml"`
				FeaturedImage   *struct {
					URL     string `json:"url"`
					AltText string `json:"altText"`
				} `json:"featuredImage"`
				Images struct {
					Nodes []struct {
						URL     string `json:"url"`
						AltText string `json:"altText"`
					} `json:"nodes"`
				} `json:"images"`
				Variants struct {
					Nodes []struct {
						ID                string `json:"id"`
						SKU               string `json:"sku"`
						Barcode           string `json:"barcode"`
						Price             string `json:"price"`
						CompareAtPrice    string `json:"compareAtPrice"`
						InventoryQuantity int    `json:"inventoryQuantity"`
					} `json:"nodes"`
				} `json:"variants"`
			} `json:"data"`
		}

		if err := json.Unmarshal(data, &productResp); err != nil {
			http.Error(w, "Failed to parse product data: "+err.Error(), 500)
			return
		}

		// Check if product is in Partner Catalog
		inCollection, err := isProductInCollection(shop, token, ver, productResp.Data.ID, collHandle)
		if err != nil {
			http.Error(w, "Failed to verify collection: "+err.Error(), 500)
			return
		}
		if !inCollection {
			http.Error(w, "Product is not in Partner Catalog collection", 403)
			return
		}

		// Check if JSON format is requested
		format := r.URL.Query().Get("format")
		if format == "json" {
			// Return JSON format
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(productResp.Data)
			return
		}

		// Render as HTML for browser viewing (default)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
	<title>%s</title>
	<style>
		body { font-family: Arial, sans-serif; max-width: 1200px; margin: 20px auto; padding: 20px; }
		.product-header { border-bottom: 2px solid #333; padding-bottom: 10px; margin-bottom: 20px; }
		.product-title { font-size: 24px; font-weight: bold; margin-bottom: 10px; }
		.product-info { margin: 10px 0; }
		.product-info strong { display: inline-block; width: 150px; }
		.images { display: flex; flex-wrap: wrap; gap: 10px; margin: 20px 0; }
		.images img { max-width: 200px; max-height: 200px; border: 1px solid #ddd; }
		.variants { margin-top: 20px; }
		.variant { border: 1px solid #ddd; padding: 10px; margin: 10px 0; }
		.out-of-stock { color: red; font-weight: bold; }
		.in-stock { color: green; }
		.description { margin: 20px 0; padding: 10px; background: #f5f5f5; }
		.tags { margin: 10px 0; }
		.tag { display: inline-block; background: #e0e0e0; padding: 5px 10px; margin: 5px; border-radius: 3px; }
	</style>
</head>
<body>
	<div class="product-header">
		<div class="product-title">%s</div>
		<div class="product-info"><strong>ID:</strong> %s</div>
		<div class="product-info"><strong>Handle:</strong> %s</div>
		<div class="product-info"><strong>Status:</strong> %s</div>
		<div class="product-info"><strong>Vendor:</strong> %s</div>
		<div class="product-info"><strong>Type:</strong> %s</div>
	</div>
	
	<div class="tags">
		<strong>Tags:</strong>`,
			productResp.Data.Title,
			productResp.Data.Title,
			productResp.Data.ID,
			productResp.Data.Handle,
			productResp.Data.Status,
			productResp.Data.Vendor,
			productResp.Data.ProductType)

		// Add tags
		if len(productResp.Data.Tags) > 0 {
			for _, tag := range productResp.Data.Tags {
				html += fmt.Sprintf(`<span class="tag">%s</span>`, tag)
			}
		} else {
			html += " <em>No tags</em>"
		}

		html += `</div>`

		// Add images
		if productResp.Data.FeaturedImage != nil || len(productResp.Data.Images.Nodes) > 0 {
			html += `<div class="images"><strong>Images:</strong><br>`
			if productResp.Data.FeaturedImage != nil {
				html += fmt.Sprintf(`<img src="%s" alt="%s" title="Featured Image">`,
					productResp.Data.FeaturedImage.URL,
					productResp.Data.FeaturedImage.AltText)
			}
			for _, img := range productResp.Data.Images.Nodes {
				// Skip if it's the same as featured image
				if productResp.Data.FeaturedImage == nil || img.URL != productResp.Data.FeaturedImage.URL {
					html += fmt.Sprintf(`<img src="%s" alt="%s">`, img.URL, img.AltText)
				}
			}
			html += `</div>`
		}

		// Add description
		if productResp.Data.DescriptionHTML != "" {
			html += fmt.Sprintf(`<div class="description"><strong>Description:</strong><br>%s</div>`, productResp.Data.DescriptionHTML)
		}

		// Add variants
		html += `<div class="variants"><strong>Variants:</strong>`
		for _, v := range productResp.Data.Variants.Nodes {
			stockClass := "in-stock"
			stockText := fmt.Sprintf("%d in stock", v.InventoryQuantity)
			if v.InventoryQuantity == 0 {
				stockClass = "out-of-stock"
				stockText = "Out of stock"
			}

			html += fmt.Sprintf(`<div class="variant">
				<strong>SKU:</strong> %s<br>
				<strong>Price:</strong> $%s`,
				v.SKU, v.Price)

			if v.CompareAtPrice != "" && v.CompareAtPrice != v.Price {
				html += fmt.Sprintf(` <span style="text-decoration: line-through; color: #999;">$%s</span>`, v.CompareAtPrice)
			}

			html += fmt.Sprintf(`<br><strong>Inventory:</strong> <span class="%s">%s</span>`, stockClass, stockText)

			if v.Barcode != "" {
				html += fmt.Sprintf(`<br><strong>Barcode:</strong> %s`, v.Barcode)
			}

			html += `</div>`
		}
		html += `</div>`

		html += `</body></html>`

		w.Write([]byte(html))
	})

	// Debug endpoint to verify you can read curated products
	http.HandleFunc("/debug/partner-products", func(w http.ResponseWriter, r *http.Request) {
		var data []byte
		var err error

		// If collection handle is provided, use it directly (most reliable)
		if collHandle != "" {
			data, err = fetchProductsByCollectionHandle(shop, token, ver, collHandle)
		} else {
			// Otherwise, try to find collection and fetch products
			collectionID, findErr := findCollectionIDByTitle(shop, token, ver, collTitle)
			if findErr != nil {
				http.Error(w, "find collection failed: "+findErr.Error()+". Hint: Set PARTNER_COLLECTION_HANDLE in .env", 500)
				return
			}
			if collectionID == "" {
				http.Error(w, "collection not found by title: "+collTitle+". Hint: Set PARTNER_COLLECTION_HANDLE in .env", 404)
				return
			}
			data, err = fetchCollectionProducts(shop, token, ver, collectionID)
		}

		if err != nil {
			http.Error(w, "fetch products failed: "+err.Error(), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	// Admin endpoint to manually trigger collection membership sync
	http.HandleFunc("/admin/sync/collection", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}

		setupKey := os.Getenv("ADMIN_SETUP_KEY")
		if setupKey == "" {
			http.Error(w, "ADMIN_SETUP_KEY missing", 500)
			return
		}

		if r.Header.Get("X-Setup-Key") != setupKey {
			http.Error(w, "Unauthorized", 401)
			return
		}

		// Trigger sync immediately
		go syncCollectionMembership(shop, token, ver, collHandle, collTitle)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "sync_triggered",
			"message": "Collection membership sync started in background",
		})
	})

	// Root route - register last so it doesn't catch other routes
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only match exact root path
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("OK. App server is running.\n\nAPI Endpoints:\n  GET /v1/catalog/products?cursor=&limit=25&lang=en|ar\n  GET /health\n  GET /debug/partner-products\n  GET /debug/translations?product_id=GID&locale=en\n\nOAuth:\n  /auth?shop=YOURSTORE.myshopify.com\n"))
	})

	// Start background sync job to detect collection membership changes
	// Runs every 10 minutes
	if collHandle != "" || collTitle != "" {
		log.Println("Starting background collection membership sync (runs every 5 minutes)")
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()

			// Run immediately on startup
			syncCollectionMembership(shop, token, ver, collHandle, collTitle)

			// Then run every 5 minutes
			for range ticker.C {
				syncCollectionMembership(shop, token, ver, collHandle, collTitle)
			}
		}()
	}

	log.Println("Listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}

func shopifyGraphQL(shop, token, ver string, req gqlReq) ([]byte, error) {
	endpoint := fmt.Sprintf("https://%s/admin/api/%s/graphql.json", shop, ver)

	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Shopify-Access-Token", token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	return raw, nil
}

func findCollectionIDByTitle(shop, token, ver, title string) (string, error) {
	// Use REST API instead of GraphQL to avoid permission issues
	// Use custom_collections.json for manual collections
	endpoint := fmt.Sprintf("https://%s/admin/api/%s/custom_collections.json", shop, ver)

	httpReq, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("X-Shopify-Access-Token", token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}

	var result struct {
		CustomCollections []struct {
			ID     int64  `json:"id"`
			Title  string `json:"title"`
			Handle string `json:"handle"`
		} `json:"custom_collections"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	// Search for collection by title (case-insensitive, trimmed)
	titleNormalized := strings.TrimSpace(title)
	for _, coll := range result.CustomCollections {
		if strings.EqualFold(strings.TrimSpace(coll.Title), titleNormalized) {
			// Convert numeric ID to GraphQL GID format
			return fmt.Sprintf("gid://shopify/Collection/%d", coll.ID), nil
		}
	}
	return "", nil // Collection not found
}

func fetchProductsByCollectionHandle(shop, token, ver, handle string) ([]byte, error) {
	// Query products filtered by collection handle - more reliable than collection ID
	q := `query($handle:String!){
		collectionByHandle(handle: $handle){
			id
			title
			products(first: 25){
				nodes{
					id
					title
					handle
					status
					updatedAt
					vendor
					productType
					tags
					descriptionHtml
					featuredImage{ url altText }
					images(first: 10){ nodes{ url altText } }
					variants(first: 50){
						nodes{
							id
							sku
							barcode
							price
							compareAtPrice
							inventoryQuantity
							inventoryPolicy
							taxable
							inventoryItem {
								id
								requiresShipping
								measurement {
									weight {
										value
										unit
									}
								}
							}
						}
					}
				}
			}
		}
	}`
	req := gqlReq{
		Query: q,
		Variables: map[string]interface{}{
			"handle": handle,
		},
	}
	return shopifyGraphQL(shop, token, ver, req)
}

func listAllCollections(shop, token, ver string) ([]byte, error) {
	// List all collections with their handles - helps user find the right handle
	q := `{
		collections(first: 50){
			nodes{
				id
				title
				handle
			}
		}
	}`
	req := gqlReq{
		Query: q,
	}
	return shopifyGraphQL(shop, token, ver, req)
}

func fetchCollectionProducts(shop, token, ver, collectionID string) ([]byte, error) {
	q := `query($id:ID!){
		collection(id:$id){
			id
			title
			products(first: 25){
				nodes{
					id
					title
					handle
					status
					updatedAt
					vendor
					productType
					tags
					descriptionHtml
					featuredImage{ url altText }
					images(first: 10){ nodes{ url altText } }
					variants(first: 50){
						nodes{
							id
							sku
							barcode
							price
							compareAtPrice
							inventoryQuantity
							inventoryPolicy
							taxable
							inventoryItem {
								id
								requiresShipping
								measurement {
									weight {
										value
										unit
									}
								}
							}
						}
					}
				}
			}
		}
	}`
	req := gqlReq{
		Query: q,
		Variables: map[string]interface{}{
			"id": collectionID,
		},
	}
	return shopifyGraphQL(shop, token, ver, req)
}

func exchangeCodeForToken(shop, code, clientID, clientSecret string) (string, error) {
	body := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"code":          code,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	resp, err := http.Post("https://"+shop+"/admin/oauth/access_token", "application/json", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

func verifyShopifyHMAC(q url.Values, secret string) bool {
	// Shopify HMAC is computed on the sorted query string excluding hmac and signature
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "hmac" || k == "signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, k+"="+v)
		}
	}
	msg := strings.Join(parts, "&")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	expected := mac.Sum(nil)

	got, err := hex.DecodeString(q.Get("hmac"))
	if err != nil {
		return false
	}
	return hmac.Equal(expected, got)
}

// parsePartnerAPIKeys parses PARTNER_API_KEYS env var format: "partnerA:KEY1,partnerB:KEY2"
// Returns a map of valid API keys for quick lookup
func parsePartnerAPIKeys(keysStr string) map[string]bool {
	validKeys := make(map[string]bool)
	if keysStr == "" {
		return validKeys
	}

	// Split by comma: "partnerA:KEY1,partnerB:KEY2" -> ["partnerA:KEY1", "partnerB:KEY2"]
	pairs := strings.Split(keysStr, ",")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		// Split by colon: "partnerA:KEY1" -> ["partnerA", "KEY1"]
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[1])
			if key != "" {
				validKeys[key] = true
			}
		} else {
			// If no colon, treat entire string as key (backward compatibility)
			key := strings.TrimSpace(pair)
			if key != "" {
				validKeys[key] = true
			}
		}
	}
	return validKeys
}

// authenticatePartner validates the API key from Authorization header against valid keys
func authenticatePartner(r *http.Request, validKeys map[string]bool) bool {
	if len(validKeys) == 0 {
		return false
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}

	// Support both "Bearer <key>" and just "<key>" formats
	key := strings.TrimPrefix(authHeader, "Bearer ")
	key = strings.TrimSpace(key)

	// Check if key exists in valid keys map
	return validKeys[key]
}

// fetchProductsByCollectionHandlePaginated fetches products with pagination support
func fetchProductsByCollectionHandlePaginated(shop, token, ver, handle, cursor string, limit int) ([]byte, error) {
	var q string
	var variables map[string]interface{}

	if cursor != "" {
		q = `query($handle:String!, $first:Int!, $after:String!){
			collectionByHandle(handle: $handle){
				id
				title
				products(first: $first, after: $after){
					pageInfo {
						hasNextPage
						endCursor
					}
					nodes{
						id
						title
						handle
						status
						updatedAt
						vendor
						productType
						tags
						descriptionHtml
						featuredImage{ url altText }
						images(first: 10){ nodes{ url altText } }
						variants(first: 50){
							nodes{
								id
								sku
								barcode
								price
								compareAtPrice
								inventoryQuantity
								inventoryPolicy
								taxable
								inventoryItem {
									id
									requiresShipping
									measurement {
										weight {
											value
											unit
										}
									}
								}
							}
						}
					}
				}
			}
		}`
		variables = map[string]interface{}{
			"handle": handle,
			"after":  cursor,
			"first":  limit,
		}
	} else {
		q = `query($handle:String!, $first:Int!){
			collectionByHandle(handle: $handle){
				id
				title
				products(first: $first){
					pageInfo {
						hasNextPage
						endCursor
					}
					nodes{
						id
						title
						handle
						status
						updatedAt
						vendor
						productType
						tags
						descriptionHtml
						featuredImage{ url altText }
						images(first: 10){ nodes{ url altText } }
						variants(first: 50){
							nodes{
								id
								sku
								barcode
								price
								compareAtPrice
								inventoryQuantity
								inventoryPolicy
								taxable
								inventoryItem {
									id
									requiresShipping
									measurement {
										weight {
											value
											unit
										}
									}
								}
							}
						}
					}
				}
			}
		}`
		variables = map[string]interface{}{
			"handle": handle,
			"first":  limit,
		}
	}

	req := gqlReq{
		Query:     q,
		Variables: variables,
	}

	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return nil, err
	}

	// Transform response to include pagination metadata
	return transformPaginatedResponse(raw)
}

// fetchCollectionProductsPaginated fetches products by collection ID with pagination
func fetchCollectionProductsPaginated(shop, token, ver, collectionID, cursor string, limit int) ([]byte, error) {
	var q string
	var variables map[string]interface{}

	if cursor != "" {
		q = `query($id:ID!, $first:Int!, $after:String!){
			collection(id:$id){
				id
				title
				products(first: $first, after: $after){
					pageInfo {
						hasNextPage
						endCursor   
					}
					nodes{
						id
						title
						handle
						status
						updatedAt
						vendor
						productType
						tags
						descriptionHtml
						featuredImage{ url altText }
						images(first: 10){ nodes{ url altText } }
						variants(first: 50){
							nodes{
								id
								sku
								barcode
								price
								compareAtPrice
								inventoryQuantity
								inventoryPolicy
								taxable
								inventoryItem {
									id
									requiresShipping
									measurement {
										weight {
											value
											unit
										}
									}
								}
							}
						}
					}
				}
			}
		}`
		variables = map[string]interface{}{
			"id":    collectionID,
			"after": cursor,
			"first": limit,
		}
	} else {
		q = `query($id:ID!, $first:Int!){
			collection(id:$id){
				id
				title
				products(first: $first){
					pageInfo {
						hasNextPage
						endCursor
					}
					nodes{
						id
						title
						handle
						status
						updatedAt
						vendor
						productType
						tags
						descriptionHtml
						featuredImage{ url altText }
						images(first: 10){ nodes{ url altText } }
						variants(first: 50){
							nodes{
								id
								sku
								barcode
								price
								compareAtPrice
								inventoryQuantity
								inventoryPolicy
								taxable
								inventoryItem {
									id
									requiresShipping
									measurement {
										weight {
											value
											unit
										}
									}
								}
							}
						}
					}
				}
			}
		}`
		variables = map[string]interface{}{
			"id":    collectionID,
			"first": limit,
		}
	}

	req := gqlReq{
		Query:     q,
		Variables: variables,
	}

	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return nil, err
	}

	// Transform response to include pagination metadata
	return transformPaginatedResponse(raw)
}

// getProductGIDBySKU looks up a product GID by variant SKU via productVariants query.
// Returns the product GID or empty string if not found.
func getProductGIDBySKU(shop, token, ver, sku string) (string, error) {
	if sku == "" {
		return "", nil
	}
	// Shopify query syntax: sku:VALUE for exact match (escape special chars if needed)
	q := `query($query: String!){
		productVariants(first: 1, query: $query){
			nodes{
				product{
					id
				}
			}
		}
	}`
	req := gqlReq{
		Query: q,
		Variables: map[string]interface{}{
			"query": "sku:" + sku,
		},
	}
	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			ProductVariants struct {
				Nodes []struct {
					Product struct {
						ID string `json:"id"`
					} `json:"product"`
				} `json:"nodes"`
			} `json:"productVariants"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if len(resp.Errors) > 0 {
		return "", fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}
	if len(resp.Data.ProductVariants.Nodes) == 0 {
		return "", nil
	}
	return resp.Data.ProductVariants.Nodes[0].Product.ID, nil
}

// fetchSingleProduct fetches a single product by ID (GID) or handle
func fetchSingleProduct(shop, token, ver, productID string) ([]byte, error) {
	var q string
	var variables map[string]interface{}

	// Check if it's a GID or handle
	if strings.HasPrefix(productID, "gid://") {
		// Query by GID
		q = `query($id:ID!){
			product(id:$id){
				id
				title
				handle
				status
				updatedAt
				vendor
				productType
				tags
				descriptionHtml
				featuredImage{ url altText }
				images(first: 10){ nodes{ url altText } }
				variants(first: 50){
					nodes{
						id
						sku
						barcode
						price
						compareAtPrice
						inventoryQuantity
						inventoryPolicy
						taxable
						inventoryItem {
							id
							requiresShipping
							measurement {
								weight {
									value
									unit
								}
							}
						}
					}
				}
			}
		}`
		variables = map[string]interface{}{
			"id": productID,
		}
	} else {
		// Query by handle
		q = `query($handle:String!){
			productByHandle(handle: $handle){
				id
				title
				handle
				status
				updatedAt
				vendor
				productType
				tags
				descriptionHtml
				featuredImage{ url altText }
				images(first: 10){ nodes{ url altText } }
				variants(first: 50){
					nodes{
						id
						sku
						barcode
						price
						compareAtPrice
						inventoryQuantity
						inventoryPolicy
						taxable
						inventoryItem {
							id
							requiresShipping
							measurement {
								weight {
									value
									unit
								}
							}
						}
					}
				}
			}
		}`
		variables = map[string]interface{}{
			"handle": productID,
		}
	}

	req := gqlReq{
		Query:     q,
		Variables: variables,
	}

	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return nil, err
	}

	// Parse and transform response
	var shopifyResp struct {
		Data struct {
			Product struct {
				ID              string   `json:"id"`
				Title           string   `json:"title"`
				Handle          string   `json:"handle"`
				Status          string   `json:"status"`
				UpdatedAt       string   `json:"updatedAt"`
				Vendor          string   `json:"vendor"`
				ProductType     string   `json:"productType"`
				Tags            []string `json:"tags"`
				DescriptionHTML string   `json:"descriptionHtml"`
				FeaturedImage   *struct {
					URL     string `json:"url"`
					AltText string `json:"altText"`
				} `json:"featuredImage"`
				Images struct {
					Nodes []struct {
						URL     string `json:"url"`
						AltText string `json:"altText"`
					} `json:"nodes"`
				} `json:"images"`
				Variants struct {
					Nodes []interface{} `json:"nodes"`
				} `json:"variants"`
			} `json:"product"`
			ProductByHandle struct {
				ID              string   `json:"id"`
				Title           string   `json:"title"`
				Handle          string   `json:"handle"`
				Status          string   `json:"status"`
				UpdatedAt       string   `json:"updatedAt"`
				Vendor          string   `json:"vendor"`
				ProductType     string   `json:"productType"`
				Tags            []string `json:"tags"`
				DescriptionHTML string   `json:"descriptionHtml"`
				FeaturedImage   *struct {
					URL     string `json:"url"`
					AltText string `json:"altText"`
				} `json:"featuredImage"`
				Images struct {
					Nodes []struct {
						URL     string `json:"url"`
						AltText string `json:"altText"`
					} `json:"nodes"`
				} `json:"images"`
				Variants struct {
					Nodes []interface{} `json:"nodes"`
				} `json:"variants"`
			} `json:"productByHandle"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(raw, &shopifyResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(shopifyResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", shopifyResp.Errors[0].Message)
	}

	// Determine which product structure was used
	// Check if we got a product by ID first
	var product interface{}
	hasProductByID := shopifyResp.Data.Product.ID != ""
	hasProductByHandle := shopifyResp.Data.ProductByHandle.ID != ""

	if hasProductByID {
		product = shopifyResp.Data.Product
	} else if hasProductByHandle {
		product = shopifyResp.Data.ProductByHandle
	} else {
		// Neither query returned a product - it doesn't exist
		return nil, fmt.Errorf("product not found: %s", productID)
	}

	// Build API response
	apiResp := map[string]interface{}{
		"data": product,
	}

	return json.Marshal(apiResp)
}

// isProductInCollection checks if a product (by GID) is in a specific collection (by handle)
// Returns true if the product is in the collection, false otherwise
//
// PERFORMANCE NOTE: This makes a GraphQL API call to Shopify for every check.
// For production, consider:
//   - Caching results in memory with TTL
//   - Maintaining a DB table of allowed product GIDs updated via webhooks
//   - Batch checking multiple products in a single query
func isProductInCollection(shop, token, ver, productGID, collectionHandle string) (bool, error) {
	if collectionHandle == "" {
		// If no collection handle is configured, deny access (security: require explicit collection)
		return false, nil
	}

	// Query the product's collections to see if it includes the Partner Catalog
	q := `query($id:ID!){
		product(id:$id){
			id
			collections(first: 50){
				nodes{
					handle
				}
			}
		}
	}`

	req := gqlReq{
		Query: q,
		Variables: map[string]interface{}{
			"id": productGID,
		},
	}

	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return false, fmt.Errorf("failed to query product collections: %w", err)
	}

	var resp struct {
		Data struct {
			Product struct {
				ID          string `json:"id"`
				Collections struct {
					Nodes []struct {
						Handle string `json:"handle"`
					} `json:"nodes"`
				} `json:"collections"`
			} `json:"product"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil {
		return false, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(resp.Errors) > 0 {
		return false, fmt.Errorf("graphql error: %s", resp.Errors[0].Message)
	}

	// Check if the product is in the Partner Catalog collection
	for _, coll := range resp.Data.Product.Collections.Nodes {
		if coll.Handle == collectionHandle {
			return true, nil
		}
	}

	return false, nil
}

// fetchProductTranslations fetches translations for a product in the given locale via translatableResource.
// Returns a map of field key -> translated value (e.g. "title" -> "...", "body_html" -> "...").
// Requires read_translations scope.
func fetchProductTranslations(shop, token, ver, productGID, locale string) (map[string]string, error) {
	q := `query($resourceId:ID!, $locale:String!){
		translatableResource(resourceId: $resourceId){
			resourceId
			translations(locale: $locale){
				key
				value
			}
		}
	}`
	req := gqlReq{
		Query: q,
		Variables: map[string]interface{}{
			"resourceId": productGID,
			"locale":     locale,
		},
	}
	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			TranslatableResource *struct {
				ResourceId   string `json:"resourceId"`
				Translations []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"translations"`
			} `json:"translatableResource"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", resp.Errors[0].Message)
	}
	out := make(map[string]string)
	if resp.Data.TranslatableResource == nil {
		return out, nil
	}
	for _, t := range resp.Data.TranslatableResource.Translations {
		if t.Key != "" {
			out[t.Key] = t.Value
		}
	}
	return out, nil
}

// getShopLocales returns the shop's enabled locales (locale code and primary flag).
// Requires read_locales scope. Returns nil on error.
func getShopLocales(shop, token, ver string) ([]struct {
	Locale  string
	Primary bool
}, error) {
	q := `query{ shopLocales{ locale primary } }`
	req := gqlReq{Query: q}
	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			ShopLocales []struct {
				Locale  string `json:"locale"`
				Primary bool   `json:"primary"`
			} `json:"shopLocales"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}
	out := make([]struct {
		Locale  string
		Primary bool
	}, len(resp.Data.ShopLocales))
	for i, l := range resp.Data.ShopLocales {
		out[i].Locale = l.Locale
		out[i].Primary = l.Primary
	}
	return out, nil
}

// translationLocaleForLang returns the Shopify locale code(s) to try for the given lang.
// If shop locales are provided and primary matches the requested lang, returns nil (product already in that language).
// Otherwise returns locales to try: for "en" tries shop's English locale first, then "en", "en-US", "en-GB".
func translationLocaleForLang(lang string, shopLocales []struct {
	Locale  string
	Primary bool
}) []string {
	// If primary locale matches requested language, caller can skip translation fetch
	if len(shopLocales) > 0 {
		for _, l := range shopLocales {
			if l.Primary {
				if lang == "en" && (l.Locale == "en" || strings.HasPrefix(l.Locale, "en-")) {
					return nil // product is already in English
				}
				if lang == "ar" && (l.Locale == "ar" || strings.HasPrefix(l.Locale, "ar-")) {
					return nil // product is already in Arabic
				}
				break
			}
		}
	}
	// Build list: for "en", prefer shop's English locale(s) first, then fallbacks
	if lang == "en" {
		try := []string{"en", "en-US", "en-GB"}
		if len(shopLocales) > 0 {
			var enLocales []string
			for _, l := range shopLocales {
				if l.Locale == "en" || strings.HasPrefix(l.Locale, "en-") {
					enLocales = append(enLocales, l.Locale)
				}
			}
			if len(enLocales) > 0 {
				try = append(enLocales, try...)
				// Dedupe while preserving order
				seen := make(map[string]bool)
				var deduped []string
				for _, t := range try {
					if !seen[t] {
						seen[t] = true
						deduped = append(deduped, t)
					}
				}
				return deduped
			}
		}
		return try
	}
	return []string{lang}
}

// fetchProductTranslationsWithFallback fetches product translations, using shop locales when available and trying fallback locale codes for "en".
// When the shop's primary locale already matches the requested language, returns empty map (product is already in that language).
func fetchProductTranslationsWithFallback(shop, token, ver, productGID, lang string) (map[string]string, error) {
	shopLocales, _ := getShopLocales(shop, token, ver)
	locales := translationLocaleForLang(lang, shopLocales)
	if locales == nil {
		return make(map[string]string), nil // product already in requested language
	}
	var lastErr error
	for _, locale := range locales {
		trans, err := fetchProductTranslations(shop, token, ver, productGID, locale)
		if err != nil {
			lastErr = err
			continue
		}
		if len(trans) > 0 {
			return trans, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return make(map[string]string), nil
}

// fetchProductTranslationsBatchWithFallback fetches translations for multiple products, using shop locales when available.
// When the shop's primary locale already matches the requested language, returns empty result (products already in that language).
func fetchProductTranslationsBatchWithFallback(shop, token, ver string, productGIDs []string, lang string) (map[string]map[string]string, error) {
	shopLocales, _ := getShopLocales(shop, token, ver)
	locales := translationLocaleForLang(lang, shopLocales)
	if locales == nil {
		return make(map[string]map[string]string), nil // products already in requested language
	}
	var lastResult map[string]map[string]string
	var lastErr error
	for _, locale := range locales {
		result, err := fetchProductTranslationsBatch(shop, token, ver, productGIDs, locale)
		if err != nil {
			lastErr = err
			continue
		}
		lastResult = result
		// If at least one product has at least one translation, use this locale
		for _, trans := range result {
			if len(trans) > 0 {
				return result, nil
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if lastResult == nil {
		lastResult = make(map[string]map[string]string)
	}
	return lastResult, nil
}

// fetchProductTranslationsBatch fetches translations for multiple products in one GraphQL request (aliased queries).
// Returns map[productGID]map[key]value. Missing or failed products are omitted from the result.
func fetchProductTranslationsBatch(shop, token, ver string, productGIDs []string, locale string) (map[string]map[string]string, error) {
	if len(productGIDs) == 0 {
		return make(map[string]map[string]string), nil
	}
	// Build query with aliases: p0: translatableResource(...), p1: ...
	var b strings.Builder
	b.WriteString("query($locale:String!){")
	for i, gid := range productGIDs {
		// Escape the GID for use in GraphQL (it's a string literal)
		escaped := strings.ReplaceAll(gid, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		fmt.Fprintf(&b, "p%d:translatableResource(resourceId:%q){resourceId translations(locale:$locale){key value}}", i, escaped)
	}
	b.WriteString("}")
	q := b.String()
	req := gqlReq{
		Query: q,
		Variables: map[string]interface{}{
			"locale": locale,
		},
	}
	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return nil, err
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if errs, ok := resp["errors"].([]interface{}); ok && len(errs) > 0 {
		if m, ok := errs[0].(map[string]interface{}); ok {
			if msg, _ := m["message"].(string); msg != "" {
				return nil, fmt.Errorf("graphql error: %s", msg)
			}
		}
		return nil, fmt.Errorf("graphql error")
	}
	data, _ := resp["data"].(map[string]interface{})
	if data == nil {
		return make(map[string]map[string]string), nil
	}
	result := make(map[string]map[string]string)
	for i, gid := range productGIDs {
		key := fmt.Sprintf("p%d", i)
		node, _ := data[key].(map[string]interface{})
		if node == nil {
			continue
		}
		transList, _ := node["translations"].([]interface{})
		transMap := make(map[string]string)
		for _, t := range transList {
			tm, _ := t.(map[string]interface{})
			if tm == nil {
				continue
			}
			k, _ := tm["key"].(string)
			v, _ := tm["value"].(string)
			if k != "" {
				transMap[k] = v
			}
		}
		result[gid] = transMap
	}
	return result, nil
}

// applyTranslationsToProductMap overwrites product fields with translated values when present.
// Keys from Shopify: title, body_html, vendor, product_type -> map to title, descriptionHtml, vendor, productType.
func applyTranslationsToProductMap(product map[string]interface{}, trans map[string]string) {
	if len(trans) == 0 {
		return
	}
	if v, ok := trans["title"]; ok && v != "" {
		product["title"] = v
	}
	if v, ok := trans["body_html"]; ok && v != "" {
		product["descriptionHtml"] = v
	}
	if v, ok := trans["vendor"]; ok && v != "" {
		product["vendor"] = v
	}
	if v, ok := trans["product_type"]; ok && v != "" {
		product["productType"] = v
	}
}

// verifyWebhookHMAC verifies Shopify webhook HMAC using X-Shopify-Hmac-Sha256 header
// Webhook HMAC is computed on the raw request body (not query string like OAuth)
// Note: Shopify's webhook HMAC header is base64-encoded, not hex-encoded
func verifyWebhookHMAC(body []byte, hmacHeader, secret string) bool {
	if hmacHeader == "" {
		return false
	}

	// Compute HMAC on raw body
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	// Decode the base64-encoded HMAC from header
	got, err := base64.StdEncoding.DecodeString(hmacHeader)
	if err != nil {
		return false
	}

	return hmac.Equal(expected, got)
}

// handleProductWebhook handles product update/delete webhooks
func handleProductWebhook(w http.ResponseWriter, r *http.Request, clientSecret, shop, token, ver, collHandle, eventType string) {
	// Read raw body for HMAC verification
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", 400)
		return
	}

	// Verify webhook HMAC
	hmacHeader := r.Header.Get("X-Shopify-Hmac-Sha256")
	if !verifyWebhookHMAC(body, hmacHeader, clientSecret) {
		log.Printf("Webhook HMAC verification failed for %s", eventType)
		http.Error(w, "Invalid webhook signature", 401)
		return
	}

	// Parse webhook payload
	var payload struct {
		ID          int64  `json:"id"`
		Title       string `json:"title"`
		Handle      string `json:"handle"`
		Vendor      string `json:"vendor"`
		ProductType string `json:"product_type"`
		Status      string `json:"status"`
		UpdatedAt   string `json:"updated_at"`
		// For delete events, some fields may be missing
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("Failed to parse webhook payload: %v", err)
		http.Error(w, "Invalid payload", 400)
		return
	}

	// Convert numeric ID to GraphQL GID
	productGID := fmt.Sprintf("gid://shopify/Product/%d", payload.ID)

	// Check previous collection membership from cache BEFORE checking current state
	var wasInCollection bool
	var hadPreviousState bool
	if cached, ok := productStateCache.Load(productGID); ok {
		previousState := cached.(*ProductState)
		wasInCollection = previousState.InPartnerCatalog
		hadPreviousState = true
		log.Printf("[WEBHOOK DEBUG] Product %d: Previous state found in cache, wasInCollection=%v", payload.ID, wasInCollection)
	} else {
		log.Printf("[WEBHOOK DEBUG] Product %d: No previous state in cache (first time seeing this product)", payload.ID)
	}

	// Check if product is currently in Partner Catalog collection
	inCollection, err := isProductInCollection(shop, token, ver, productGID, collHandle)
	if err != nil {
		log.Printf("Failed to check collection membership for product %d: %v", payload.ID, err)
		// Don't fail the webhook - just log and return 200 (Shopify expects 200)
		w.WriteHeader(200)
		return
	}

	log.Printf("[WEBHOOK DEBUG] Product %d: Current collection membership check: inCollection=%v, wasInCollection=%v, hadPreviousState=%v",
		payload.ID, inCollection, wasInCollection, hadPreviousState)

	// Handle collection membership changes
	if !inCollection && wasInCollection {
		// Product was REMOVED from collection - notify about removal
		log.Printf("[COLLECTION CHANGE] Product %d (%s) REMOVED from Partner Catalog", payload.ID, payload.Handle)
		notifyPartners(productGID, "collection_removed", payload, []string{"Product removed from Partner Catalog collection"})
		// Remove from cache since it's no longer in collection
		productStateCache.Delete(productGID)
		w.WriteHeader(200)
		w.Write([]byte("OK"))
		return
	}

	if !inCollection && !wasInCollection {
		// Product was never in collection and still isn't - ignore webhook
		log.Printf("Product %d (%s) not in Partner Catalog, ignoring webhook", payload.ID, payload.Handle)
		w.WriteHeader(200)
		w.Write([]byte("OK"))
		return
	}

	// Check if product was just ADDED to collection
	if inCollection && !wasInCollection && hadPreviousState {
		// Product was just ADDED to collection (had previous state but wasn't in collection before)
		log.Printf("[COLLECTION CHANGE] Product %d (%s) ADDED to Partner Catalog", payload.ID, payload.Handle)
		// Still fetch full details to populate cache and get complete info
		changes := detectProductChanges(shop, token, ver, productGID, eventType, payload, collHandle)
		// Prepend the addition message
		changes = append([]string{"Product added to Partner Catalog collection"}, changes...)
		notifyPartners(productGID, "collection_added", payload, changes)
		w.WriteHeader(200)
		w.Write([]byte("OK"))
		return
	}

	// If product is in collection but we don't have previous state, it might be first time or newly added
	// IMPORTANT: Shopify doesn't send webhooks for collection membership changes alone.
	// A webhook only fires when the product itself is updated (title, description, etc.).
	// So if you add/remove a product from a collection, you MUST also edit the product
	// (change title, description, etc.) to trigger a webhook.
	if inCollection && !hadPreviousState {
		log.Printf("[WEBHOOK DEBUG] Product %d: In collection but no previous state - might be newly added or first webhook", payload.ID)
		log.Printf("[WEBHOOK INFO] Note: To detect collection additions/removals, you must edit the product after changing collection membership")
	}

	// Product is in Partner Catalog and was already in collection (or first time seeing it)
	// Fetch full product details to detect changes
	changes := detectProductChanges(shop, token, ver, productGID, eventType, payload, collHandle)

	// Notify partners with detailed change information
	notifyPartners(productGID, eventType, payload, changes)

	w.WriteHeader(200)
	w.Write([]byte("OK"))
}

// handleInventoryWebhook handles inventory level update webhooks
func handleInventoryWebhook(w http.ResponseWriter, r *http.Request, clientSecret, shop, token, ver, collHandle string) {
	// Read raw body for HMAC verification
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", 400)
		return
	}

	// Verify webhook HMAC
	hmacHeader := r.Header.Get("X-Shopify-Hmac-Sha256")
	if !verifyWebhookHMAC(body, hmacHeader, clientSecret) {
		log.Printf("Webhook HMAC verification failed for inventory_levels/update")
		http.Error(w, "Invalid webhook signature", 401)
		return
	}

	// Parse webhook payload
	var payload struct {
		InventoryItemID int64  `json:"inventory_item_id"`
		LocationID      int64  `json:"location_id"`
		Available       int    `json:"available"`
		UpdatedAt       string `json:"updated_at"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("Failed to parse inventory webhook payload: %v", err)
		http.Error(w, "Invalid payload", 400)
		return
	}

	// For inventory updates, we need to find which product(s) use this inventory item
	// Query the inventory item to get associated product
	productGID, err := getProductGIDByInventoryItem(shop, token, ver, payload.InventoryItemID)
	if err != nil {
		log.Printf("Failed to get product for inventory item %d: %v", payload.InventoryItemID, err)
		// Don't fail the webhook - just log and return 200
		w.WriteHeader(200)
		return
	}

	if productGID == "" {
		log.Printf("No product found for inventory item %d", payload.InventoryItemID)
		w.WriteHeader(200)
		return
	}

	// Check if product is in Partner Catalog collection
	inCollection, err := isProductInCollection(shop, token, ver, productGID, collHandle)
	if err != nil {
		log.Printf("Failed to check collection membership for product %s: %v", productGID, err)
		w.WriteHeader(200)
		return
	}

	if !inCollection {
		// Product is not in Partner Catalog - ignore webhook
		log.Printf("Product %s not in Partner Catalog, ignoring inventory webhook", productGID)
		w.WriteHeader(200)
		return
	}

	// Product is in Partner Catalog - handle notification
	// Fetch full product details to detect inventory changes
	changes := detectInventoryChanges(shop, token, ver, productGID, payload, collHandle)

	// Notify partners with detailed change information
	notifyPartners(productGID, "inventory_update", map[string]interface{}{
		"inventory_item_id": payload.InventoryItemID,
		"available":         payload.Available,
		"location_id":       payload.LocationID,
	}, changes)

	w.WriteHeader(200)
	w.Write([]byte("OK"))
}

// getProductGIDByInventoryItem queries Shopify to find the product GID for an inventory item ID
// Note: InventoryItem doesn't have a direct product field in GraphQL, so we use REST API instead
func getProductGIDByInventoryItem(shop, token, ver string, inventoryItemID int64) (string, error) {
	// Use REST API to get variant information from inventory item
	// Query variants by inventory_item_id to find the associated product
	endpoint := fmt.Sprintf("https://%s/admin/api/%s/variants.json?inventory_item_id=%d", shop, ver, inventoryItemID)

	httpReq, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("X-Shopify-Access-Token", token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}

	var result struct {
		Variants []struct {
			ID        int64 `json:"id"`
			ProductID int64 `json:"product_id"`
		} `json:"variants"`
	}

	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Variants) == 0 {
		return "", nil
	}

	// Convert product ID to GID
	productGID := fmt.Sprintf("gid://shopify/Product/%d", result.Variants[0].ProductID)
	return productGID, nil
}

// notifyPartners handles partner notifications for product/inventory changes
// TODO: Replace with actual partner notification (HTTP webhook, DB update, etc.)
func notifyPartners(productGID, eventType string, payload interface{}, changes []string) {
	// Build detailed notification message
	changeMsg := strings.Join(changes, ", ")
	if changeMsg == "" {
		changeMsg = "No specific changes detected"
	}

	log.Printf("[PARTNER NOTIFICATION] Event=%s, Product=%s, Changes=[%s], Payload=%+v",
		eventType, productGID, changeMsg, payload)

	// In production, you would:
	// 1. Store in DB: mark product as "changed" with timestamp and change details
	// 2. Send HTTP webhook to each partner's endpoint with change details
	// 3. Queue for async processing
}

// detectProductChanges fetches current product state and compares with previous state
// Returns a list of human-readable change descriptions
func detectProductChanges(shop, token, ver, productGID, eventType string, webhookPayload interface{}, collHandle string) []string {
	var changes []string

	// Handle delete events
	if eventType == "delete" {
		// Remove from cache
		productStateCache.Delete(productGID)
		return []string{"Product deleted"}
	}

	// Fetch current product state from Shopify
	currentState, err := fetchProductState(shop, token, ver, productGID)
	if err != nil {
		log.Printf("Failed to fetch product state for change detection: %v", err)
		return []string{"Unable to detect changes (fetch failed)"}
	}

	// Verify product is in Partner Catalog collection
	inCollection, err := isProductInCollection(shop, token, ver, productGID, collHandle)
	if err != nil {
		log.Printf("Failed to check collection membership: %v", err)
		// Assume it's in collection since webhook handler already verified
		inCollection = true
	}
	currentState.InPartnerCatalog = inCollection

	// Get previous state from cache
	var previousState *ProductState
	if cached, ok := productStateCache.Load(productGID); ok {
		previousState = cached.(*ProductState)
	}

	// If no previous state, this is a new product or first webhook
	// Note: Collection membership change detection is handled in the webhook handler
	// before calling this function, so we don't need to check it here
	if previousState == nil {
		productStateCache.Store(productGID, currentState)
		tagsInfo := "no tags"
		if len(currentState.Tags) > 0 {
			tagsInfo = fmt.Sprintf("tags: %s", strings.Join(currentState.Tags, ", "))
		}
		// IMPORTANT: This message appears when we first see a product.
		// It doesn't necessarily mean the product was just added to the collection -
		// it could have been in the collection all along, we just haven't seen it before.
		// To detect actual collection additions, the product must have been in cache
		// with InPartnerCatalog=false, then appear with InPartnerCatalog=true.
		return []string{fmt.Sprintf("Product added or first webhook received (%s)", tagsInfo)}
	}

	// Compare states and detect changes
	if previousState.Title != currentState.Title {
		changes = append(changes, fmt.Sprintf("Title changed: '%s' → '%s'", previousState.Title, currentState.Title))
	}

	if previousState.Status != currentState.Status {
		statusMsg := fmt.Sprintf("Status changed: %s → %s", previousState.Status, currentState.Status)
		if currentState.Status == "draft" {
			statusMsg += " (became draft)"
		} else if currentState.Status == "archived" {
			statusMsg += " (archived)"
		} else if currentState.Status == "active" && previousState.Status != "active" {
			statusMsg += " (activated)"
		}
		changes = append(changes, statusMsg)
	}

	if previousState.DescriptionHTML != currentState.DescriptionHTML {
		changes = append(changes, "Description changed")
	}

	if previousState.Vendor != currentState.Vendor {
		changes = append(changes, fmt.Sprintf("Vendor changed: '%s' → '%s'", previousState.Vendor, currentState.Vendor))
	}

	if previousState.ProductType != currentState.ProductType {
		changes = append(changes, fmt.Sprintf("Product type changed: '%s' → '%s'", previousState.ProductType, currentState.ProductType))
	}

	// Compare tags
	tagChanges := detectTagChanges(previousState.Tags, currentState.Tags)
	changes = append(changes, tagChanges...)

	// Check collection membership changes
	currentInCollection := currentState.InPartnerCatalog
	previousInCollection := previousState.InPartnerCatalog

	if !previousInCollection && currentInCollection {
		changes = append(changes, "Product added to Partner Catalog collection")
	} else if previousInCollection && !currentInCollection {
		changes = append(changes, "Product removed from Partner Catalog collection")
	}

	// Compare variants
	variantChanges := detectVariantChanges(previousState.Variants, currentState.Variants)
	changes = append(changes, variantChanges...)

	// Update cache with new state
	productStateCache.Store(productGID, currentState)

	if len(changes) == 0 {
		changes = []string{"Product updated (no specific changes detected)"}
	}

	return changes
}

// detectInventoryChanges detects inventory-specific changes
func detectInventoryChanges(shop, token, ver, productGID string, inventoryPayload interface{}, collHandle string) []string {
	var changes []string

	// Fetch current product state to get inventory info
	currentState, err := fetchProductState(shop, token, ver, productGID)
	if err != nil {
		log.Printf("Failed to fetch product state for inventory change detection: %v", err)
		return []string{"Inventory updated (unable to detect specific changes)"}
	}

	// Verify product is in Partner Catalog collection
	inCollection, err := isProductInCollection(shop, token, ver, productGID, collHandle)
	if err != nil {
		log.Printf("Failed to check collection membership: %v", err)
		inCollection = true // Assume it is since webhook handler verified
	}
	currentState.InPartnerCatalog = inCollection

	// Get previous state
	var previousState *ProductState
	if cached, ok := productStateCache.Load(productGID); ok {
		previousState = cached.(*ProductState)
	}

	if previousState == nil {
		// First time seeing this product
		productStateCache.Store(productGID, currentState)
		if inCollection {
			return []string{"Inventory updated (first webhook for this product - in Partner Catalog)"}
		}
		return []string{"Inventory updated (first webhook for this product - not in Partner Catalog)"}
	}

	// Check collection membership changes
	currentInCollection := currentState.InPartnerCatalog
	previousInCollection := previousState.InPartnerCatalog

	if !previousInCollection && currentInCollection {
		changes = append(changes, "Product added to Partner Catalog collection")
	} else if previousInCollection && !currentInCollection {
		changes = append(changes, "Product removed from Partner Catalog collection")
	}

	// Compare inventory quantities
	prevInventory := make(map[string]int) // variant ID -> quantity
	currInventory := make(map[string]int)

	for _, v := range previousState.Variants {
		prevInventory[v.ID] = v.InventoryQuantity
	}
	for _, v := range currentState.Variants {
		currInventory[v.ID] = v.InventoryQuantity
	}

	// Check for inventory changes
	for variantID, currQty := range currInventory {
		prevQty, exists := prevInventory[variantID]
		if !exists {
			// New variant
			if currQty == 0 {
				changes = append(changes, fmt.Sprintf("New variant added (out of stock)"))
			} else {
				changes = append(changes, fmt.Sprintf("New variant added (inventory: %d)", currQty))
			}
		} else if prevQty != currQty {
			if currQty == 0 {
				changes = append(changes, fmt.Sprintf("Variant out of stock (was %d)", prevQty))
			} else if prevQty == 0 {
				changes = append(changes, fmt.Sprintf("Variant back in stock (now %d)", currQty))
			} else if currQty > prevQty {
				changes = append(changes, fmt.Sprintf("Stock increased: %d → %d (+%d)", prevQty, currQty, currQty-prevQty))
			} else {
				changes = append(changes, fmt.Sprintf("Stock decreased: %d → %d (-%d)", prevQty, currQty, prevQty-currQty))
			}
		} else if currQty == 0 {
			// Quantity hasn't changed but it's 0 - report out of stock status
			changes = append(changes, fmt.Sprintf("Variant out of stock (quantity: 0)"))
		}
	}

	// Check for removed variants
	for variantID, prevQty := range prevInventory {
		if _, exists := currInventory[variantID]; !exists {
			changes = append(changes, fmt.Sprintf("Variant removed (had %d in stock)", prevQty))
		}
	}

	// If no previous state or no changes detected, check current stock status
	if previousState == nil || len(changes) == 0 {
		// Check if any variants are out of stock
		allOutOfStock := true
		someOutOfStock := false
		totalStock := 0

		for _, v := range currentState.Variants {
			if v.InventoryQuantity == 0 {
				someOutOfStock = true
			} else {
				allOutOfStock = false
				totalStock += v.InventoryQuantity
			}
		}

		if len(currentState.Variants) == 0 {
			changes = append(changes, "Product has no variants")
		} else if allOutOfStock {
			changes = append(changes, "All variants out of stock")
		} else if someOutOfStock {
			outOfStockCount := 0
			for _, v := range currentState.Variants {
				if v.InventoryQuantity == 0 {
					outOfStockCount++
				}
			}
			changes = append(changes, fmt.Sprintf("%d variant(s) out of stock (total stock: %d)", outOfStockCount, totalStock))
		} else {
			changes = append(changes, fmt.Sprintf("Inventory updated (total stock: %d)", totalStock))
		}
	}

	// Update cache
	productStateCache.Store(productGID, currentState)

	return changes
}

// fetchProductState fetches the current state of a product from Shopify
func fetchProductState(shop, token, ver, productGID string) (*ProductState, error) {
	q := `query($id:ID!){
		product(id:$id){
			id
			handle
			title
			status
			descriptionHtml
			vendor
			productType
			tags
			updatedAt
			variants(first: 50){
				nodes{
					id
					sku
					barcode
					price
					compareAtPrice
					inventoryQuantity
					inventoryItem {
						id
					}
				}
			}
		}
	}`

	req := gqlReq{
		Query: q,
		Variables: map[string]interface{}{
			"id": productGID,
		},
	}

	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			Product struct {
				ID              string   `json:"id"`
				Handle          string   `json:"handle"`
				Title           string   `json:"title"`
				Status          string   `json:"status"`
				DescriptionHTML string   `json:"descriptionHtml"`
				Vendor          string   `json:"vendor"`
				ProductType     string   `json:"productType"`
				Tags            []string `json:"tags"`
				UpdatedAt       string   `json:"updatedAt"` // Shopify returns RFC3339 string
				Variants        struct {
					Nodes []struct {
						ID                string `json:"id"`
						SKU               string `json:"sku"`
						Barcode           string `json:"barcode"`
						Price             string `json:"price"`
						CompareAtPrice    string `json:"compareAtPrice"`
						InventoryQuantity int    `json:"inventoryQuantity"`
						InventoryItem     struct {
							ID string `json:"id"`
						} `json:"inventoryItem"`
					} `json:"nodes"`
				} `json:"variants"`
			} `json:"product"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", resp.Errors[0].Message)
	}

	// Parse UpdatedAt from RFC3339 string to time.Time
	updatedAt, err := time.Parse(time.RFC3339, resp.Data.Product.UpdatedAt)
	if err != nil {
		// Fallback to current time if parsing fails
		updatedAt = time.Now()
	}

	state := &ProductState{
		ID:               resp.Data.Product.ID,
		Handle:           resp.Data.Product.Handle,
		Title:            resp.Data.Product.Title,
		Status:           resp.Data.Product.Status,
		DescriptionHTML:  resp.Data.Product.DescriptionHTML,
		Vendor:           resp.Data.Product.Vendor,
		ProductType:      resp.Data.Product.ProductType,
		Tags:             resp.Data.Product.Tags,
		UpdatedAt:        updatedAt,
		InPartnerCatalog: false, // Will be set by caller if needed
		LastSeenAt:       time.Now(),
	}

	for _, v := range resp.Data.Product.Variants.Nodes {
		var compareAtPrice *string
		if v.CompareAtPrice != "" {
			compareAtPrice = &v.CompareAtPrice
		}

		state.Variants = append(state.Variants, VariantState{
			ID:                v.ID,
			SKU:               v.SKU,
			Barcode:           v.Barcode,
			Price:             v.Price,
			CompareAtPrice:    compareAtPrice,
			InventoryQuantity: v.InventoryQuantity,
			InventoryItemID:   v.InventoryItem.ID,
		})
	}

	return state, nil
}

// detectTagChanges compares old and new tag lists and reports additions/removals
func detectTagChanges(oldTags, newTags []string) []string {
	var changes []string

	// Create maps for easy lookup
	oldTagMap := make(map[string]bool)
	newTagMap := make(map[string]bool)

	for _, tag := range oldTags {
		oldTagMap[tag] = true
	}
	for _, tag := range newTags {
		newTagMap[tag] = true
	}

	// Find removed tags
	var removedTags []string
	for tag := range oldTagMap {
		if !newTagMap[tag] {
			removedTags = append(removedTags, tag)
		}
	}
	if len(removedTags) > 0 {
		changes = append(changes, fmt.Sprintf("Tag(s) removed: %s", strings.Join(removedTags, ", ")))
	}

	// Find added tags
	var addedTags []string
	for tag := range newTagMap {
		if !oldTagMap[tag] {
			addedTags = append(addedTags, tag)
		}
	}
	if len(addedTags) > 0 {
		changes = append(changes, fmt.Sprintf("Tag(s) added: %s", strings.Join(addedTags, ", ")))
	}

	return changes
}

// detectVariantChanges compares old and new variant lists
func detectVariantChanges(oldVariants, newVariants []VariantState) []string {
	var changes []string

	oldMap := make(map[string]VariantState)
	newMap := make(map[string]VariantState)

	for _, v := range oldVariants {
		oldMap[v.ID] = v
	}
	for _, v := range newVariants {
		newMap[v.ID] = v
	}

	// Check for new variants
	for id, newV := range newMap {
		if _, exists := oldMap[id]; !exists {
			changes = append(changes, fmt.Sprintf("New variant added (SKU: %s, Price: %s)", newV.SKU, newV.Price))
		} else {
			oldV := oldMap[id]
			// Check for price changes
			if oldV.Price != newV.Price {
				changes = append(changes, fmt.Sprintf("Variant price changed: %s → %s (SKU: %s)", oldV.Price, newV.Price, newV.SKU))
			}
			// Compare CompareAtPrice (pointer comparison)
			oldCompare := ""
			if oldV.CompareAtPrice != nil {
				oldCompare = *oldV.CompareAtPrice
			}
			newCompare := ""
			if newV.CompareAtPrice != nil {
				newCompare = *newV.CompareAtPrice
			}
			if oldCompare != newCompare {
				if newCompare == "" {
					changes = append(changes, fmt.Sprintf("Compare-at price removed (was %s, SKU: %s)", oldCompare, newV.SKU))
				} else if oldCompare == "" {
					changes = append(changes, fmt.Sprintf("Compare-at price added: %s (SKU: %s)", newCompare, newV.SKU))
				} else {
					changes = append(changes, fmt.Sprintf("Compare-at price changed: %s → %s (SKU: %s)", oldCompare, newCompare, newV.SKU))
				}
			}
		}
	}

	// Check for removed variants
	for id, oldV := range oldMap {
		if _, exists := newMap[id]; !exists {
			changes = append(changes, fmt.Sprintf("Variant removed (SKU: %s)", oldV.SKU))
		}
	}

	return changes
}

// ensureWebhook creates a webhook subscription in Shopify via GraphQL
// Returns the webhook subscription ID if successful
func ensureWebhook(shop, token, ver, topic, callbackURL string) (string, error) {
	mutation := `mutation WebhookCreate($topic: WebhookSubscriptionTopic!, $callbackUrl: URL!){
		webhookSubscriptionCreate(topic: $topic, webhookSubscription: { callbackUrl: $callbackUrl, format: JSON }) {
			webhookSubscription { id topic endpoint { __typename } }
			userErrors { field message }
		}
	}`

	req := gqlReq{
		Query: mutation,
		Variables: map[string]interface{}{
			"topic":       topic,
			"callbackUrl": callbackURL,
		},
	}

	raw, err := shopifyGraphQL(shop, token, ver, req)
	if err != nil {
		return "", err
	}

	var resp struct {
		Data struct {
			Create struct {
				WebhookSubscription struct {
					ID string `json:"id"`
				} `json:"webhookSubscription"`
				UserErrors []struct {
					Message string `json:"message"`
				} `json:"userErrors"`
			} `json:"webhookSubscriptionCreate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return "", fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}
	if len(resp.Data.Create.UserErrors) > 0 {
		return "", fmt.Errorf("userError: %s", resp.Data.Create.UserErrors[0].Message)
	}
	if resp.Data.Create.WebhookSubscription.ID == "" {
		return "", fmt.Errorf("no webhook id returned")
	}
	return resp.Data.Create.WebhookSubscription.ID, nil
}

// transformPaginatedResponse transforms Shopify GraphQL response to API-friendly format
func transformPaginatedResponse(raw []byte) ([]byte, error) {
	var shopifyResp struct {
		Data struct {
			Collection struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Products struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []interface{} `json:"nodes"`
				} `json:"products"`
			} `json:"collectionByHandle"`
			CollectionByID struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Products struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []interface{} `json:"nodes"`
				} `json:"products"`
			} `json:"collection"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(raw, &shopifyResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(shopifyResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", shopifyResp.Errors[0].Message)
	}

	// Determine which collection structure was used
	var products struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []interface{} `json:"nodes"`
	}

	var collectionTitle string
	if shopifyResp.Data.Collection.ID != "" {
		products = shopifyResp.Data.Collection.Products
		collectionTitle = shopifyResp.Data.Collection.Title
	} else {
		products = shopifyResp.Data.CollectionByID.Products
		collectionTitle = shopifyResp.Data.CollectionByID.Title
	}

	// Build API response
	apiResp := map[string]interface{}{
		"data": products.Nodes,
		"pagination": map[string]interface{}{
			"hasNextPage": products.PageInfo.HasNextPage,
			"nextCursor":  products.PageInfo.EndCursor,
		},
		"meta": map[string]interface{}{
			"collection": collectionTitle,
			"count":      len(products.Nodes),
		},
	}

	return json.Marshal(apiResp)
}
