package config

type Config struct {
	// File paths
	EmailsFile  string
	ResultsFile string

	// API settings
	APIAddr string // Python API service address

	// SOCKS5 proxy URL applied to Loki + token-exchange clients.
	// Empty = no proxy. Localhost calls to APIAddr never go through it.
	Proxy string

	// Worker settings
	NumWorkers int

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
	return &Config{
		EmailsFile:  "emails.txt",
		ResultsFile: "result.txt",

		APIAddr: "http://localhost:5000",

		NumWorkers: 400,

		APITimeout: 30,

		MaxCPM: 20000,

		EmailBufferSize:  100000,
		ResultBufferSize: 10000,
		FileBufferSize:   4 * 1024 * 1024,
	}
}
