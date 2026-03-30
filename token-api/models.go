package main

// TokenData represents a token stored in Redis
type TokenData struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	RefreshToken string `json:"refresh_token"`
	TenantID     string `json:"tenant_id"`
	CreatedAt    int64  `json:"created_at"`
	ExhaustedAt  int64  `json:"exhausted_at,omitempty"`
}

// TokenResponse is the API response for GET /tokens/{tenant_id}
type TokenResponse struct {
	Tokens []TokenData `json:"tokens"`
	Count  int         `json:"count"`
}

// ExhaustedRequest is the request body for POST /tokens/{tenant_id}/exhausted
type ExhaustedRequest struct {
	Email string `json:"email"`
}

// StatsResponse is the API response for GET /stats/{tenant_id}
type StatsResponse struct {
	TenantID string `json:"tenant_id,omitempty"`
	Fresh    int64  `json:"fresh"`
	InUse    int64  `json:"in_use"`
	Used     int64  `json:"used"`
}

// OKResponse is a simple success response
type OKResponse struct {
	OK bool `json:"ok"`
}
