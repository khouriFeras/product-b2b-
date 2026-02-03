package main

import "time"

// gqlReq represents a GraphQL request
type gqlReq struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

// ProductState stores the last known state of a product for change detection
type ProductState struct {
	ID               string         `json:"id"`
	Handle           string         `json:"handle"`
	Title            string         `json:"title"`
	Status           string         `json:"status"`
	DescriptionHTML  string         `json:"descriptionHtml,omitempty"`
	Vendor           string         `json:"vendor,omitempty"`
	ProductType      string         `json:"productType,omitempty"`
	Tags             []string       `json:"tags,omitempty"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	Variants         []VariantState `json:"variants,omitempty"`
	InPartnerCatalog bool           `json:"inPartnerCatalog"` // Track collection membership
	LastSeenAt       time.Time      `json:"lastSeenAt"`
}

// VariantState stores variant information for change detection
type VariantState struct {
	ID                string  `json:"id"`
	SKU               string  `json:"sku,omitempty"`
	Barcode           string  `json:"barcode,omitempty"`
	Price             string  `json:"price"`
	CompareAtPrice    *string `json:"compareAtPrice,omitempty"`
	InventoryQuantity int     `json:"inventoryQuantity"`
	InventoryItemID   string  `json:"inventoryItemId,omitempty"`
}

// VariantInfo is used for inventory reporting
type VariantInfo struct {
	SKU               string `json:"sku"`
	Price             string `json:"price"`
	InventoryQuantity int    `json:"inventoryQuantity"`
	OutOfStock        bool   `json:"outOfStock"`
}

// InventoryReport represents inventory status for a product
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
