package models

// LinkedInResponse represents the API response structure
type LinkedInResponse struct {
	ResultTemplate string   `json:"resultTemplate"`
	Bound          bool     `json:"bound"`
	Persons        []Person `json:"persons"`
}

// Person represents a LinkedIn profile from the API response
type Person struct {
	ID              string `json:"id"`
	DisplayName     string `json:"displayName"`
	FirstName       string `json:"firstName"`
	LastName        string `json:"lastName"`
	Headline        string `json:"headline"`
	CompanyName     string `json:"companyName"`
	Location        string `json:"location"`
	PhotoURL        string `json:"photoUrl"`
	LinkedInURL     string `json:"linkedInUrl"`
	ConnectionCount int    `json:"connectionCount"`
	IsPublic        bool   `json:"isPublic"`
}

// Result represents the output format
type Result struct {
	Email           string
	DisplayName     string
	ConnectionCount int
	Location        string
	LinkedInURL     string
}

// Job represents a work item for workers
type Job struct {
	Email string
}

// JobResult represents the result of processing a job
type JobResult struct {
	Email   string
	Success bool
	Result  *Result
	Error   error
}
