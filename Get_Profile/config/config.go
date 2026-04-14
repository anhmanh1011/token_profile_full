package config

type Config struct {
	// File paths
	EmailsFile  string
	ResultsFile string

	// API settings
	APIAddr string // Python API service address

	// Worker settings
	NumWorkers int
	MaxWorkers int

	// Worker API settings
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

		// API settings
		APIAddr: "http://localhost:5000",

		// Worker settings
		NumWorkers: numWorkers,
		MaxWorkers: maxWorkers,

		// Worker API settings
		APITimeout: 30,

		// Rate limiting (default 35K CPM, overridden in main.go to 20K)
		MaxCPM: 35000,

		// Buffer settings
		EmailBufferSize:  100000,
		ResultBufferSize: 10000,
		FileBufferSize:   4 * 1024 * 1024,
	}
}
