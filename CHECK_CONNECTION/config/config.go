package config

type Config struct {
	// File paths
	EmailsFile  string
	TokensFile  string
	ResultsFile string

	// Worker settings
	NumWorkers int
	MaxWorkers int

	// API settings
	APITimeout int // seconds

	// Rate limiting
	MaxCPM int // Max requests per minute (0 = unlimited)

	// Buffer settings
	EmailBufferSize  int
	ResultBufferSize int
	FileBufferSize   int

	// Token API settings (centralized mode)
	TokenAPIURL string // Central Token API URL (e.g. http://central:8080)
	TenantID    string // Tenant ID for this worker
	BatchSize   int    // Tokens per API fetch (default 100)
}

func NewConfig() *Config {
	// Optimized for maximum throughput
	numWorkers := 1000
	maxWorkers := 3000

	return &Config{
		// File paths
		EmailsFile:  "emails.txt",
		TokensFile:  "tokens.txt",
		ResultsFile: "result.txt",

		// Worker settings
		NumWorkers: numWorkers,
		MaxWorkers: maxWorkers,

		// API settings
		APITimeout: 30,

		// Rate limiting (default 35K CPM)
		MaxCPM: 35000,

		// Buffer settings - larger buffers for better IO
		EmailBufferSize:  100000,          // 100K email buffer
		ResultBufferSize: 10000,           // 10K result buffer
		FileBufferSize:   4 * 1024 * 1024, // 4MB file buffer

		// Token API defaults
		TokenAPIURL: "",
		TenantID:    "",
		BatchSize:   100,
	}
}
