package config

type Config struct {
	// File paths
	EmailsFile  string
	ResultsFile string

	// Redis settings
	RedisAddr  string
	RedisQueue string // Queue suffix, e.g. "1" -> redis-tokens-1

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
}

func NewConfig() *Config {
	numWorkers := 1000
	maxWorkers := 3000

	return &Config{
		// File paths
		EmailsFile:  "emails.txt",
		ResultsFile: "result.txt",

		// Redis settings
		RedisAddr:  "localhost:6379",
		RedisQueue: "",

		// Worker settings
		NumWorkers: numWorkers,
		MaxWorkers: maxWorkers,

		// API settings
		APITimeout: 30,

		// Rate limiting (default 35K CPM, overridden in main.go to 20K)
		MaxCPM: 35000,

		// Buffer settings
		EmailBufferSize:  100000,
		ResultBufferSize: 10000,
		FileBufferSize:   4 * 1024 * 1024,
	}
}
