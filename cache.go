package main

import "sync"

// productStateCache stores the last known state of products (keyed by product GID)
var productStateCache = sync.Map{} // map[string]*ProductState
