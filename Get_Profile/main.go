//go:build !hot
// +build !hot

package main

import (
	"flag"
	"fmt"
	"io"
	"linkedin_fetcher/api"
	"linkedin_fetcher/config"
	"linkedin_fetcher/reader"
	"linkedin_fetcher/token"
	"linkedin_fetcher/worker"
	"linkedin_fetcher/writer"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	// Generate timestamp for this run
	startTimestamp := time.Now().Format("2006-01-02_15-04-05")

	// Parse command line arguments
	emailsFile := flag.String("emails", "", "Path to emails file (default: emails.txt)")
	tokensFile := flag.String("tokens", "", "Path to tokens file (default: tokens.txt)")
	resultFile := flag.String("result", "", "Path to result file (will use timestamp if not specified)")
	refreshedTokensFile := flag.String("refreshed", "tokens_refreshed.txt", "Path to save refreshed tokens")
	instanceID := flag.String("id", "", "Instance ID for logging (optional)")
	maxCPM := flag.Int("max-cpm", 0, "Max requests per minute (0 = unlimited)")
	flag.Parse()

	// Generate timestamped filenames
	logFileName := fmt.Sprintf("output_%s.log", startTimestamp)
	resultFileName := fmt.Sprintf("result_%s.txt", startTimestamp)

	// If result file specified, use it instead
	if *resultFile != "" {
		resultFileName = *resultFile
	}

	// Set up logging to file
	logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	// Log to both stdout and file
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if *instanceID != "" {
		log.SetPrefix(fmt.Sprintf("[%s] ", *instanceID))
	}
	log.Println("=== LinkedIn Profile Fetcher ===")
	log.Printf("Run timestamp: %s", startTimestamp)
	log.Printf("Log file: %s", logFileName)
	log.Printf("Result file: %s", resultFileName)
	log.Println("Starting application...")

	// Load configuration
	cfg := config.NewConfig()

	// Override defaults for protection against rate limiting
	cfg.NumWorkers = 350 // Reduced from 1000 to prevent account restriction
	cfg.MaxCPM = 20000   // Limit to 20K requests/minute

	// Override with command line arguments if provided
	if *emailsFile != "" {
		cfg.EmailsFile = *emailsFile
	}
	if *tokensFile != "" {
		cfg.TokensFile = *tokensFile
	}
	// Use timestamped result file
	cfg.ResultsFile = resultFileName

	// Set max CPM if provided
	if *maxCPM > 0 {
		cfg.MaxCPM = *maxCPM
	}

	log.Printf("[CONFIG] Workers: %d | APITimeout: %ds | EmailBuffer: %d | MaxCPM: %d",
		cfg.NumWorkers, cfg.APITimeout, cfg.EmailBufferSize, cfg.MaxCPM)
	log.Printf("[FILES] Emails: %s | Tokens: %s | Result: %s",
		cfg.EmailsFile, cfg.TokensFile, cfg.ResultsFile)

	// Check if input files exist
	if _, err := os.Stat(cfg.EmailsFile); os.IsNotExist(err) {
		log.Fatalf("[ERROR] Emails file not found: %s", cfg.EmailsFile)
	}
	if _, err := os.Stat(cfg.TokensFile); os.IsNotExist(err) {
		log.Fatalf("[ERROR] Tokens file not found: %s", cfg.TokensFile)
	}

	// Initialize token manager
	tokenManager := token.NewManager()
	if err := tokenManager.LoadFromFile(cfg.TokensFile); err != nil {
		log.Fatalf("[ERROR] Failed to load tokens: %v", err)
	}
	total, alive, _ := tokenManager.Stats()
	log.Printf("[TOKEN] Loaded %d tokens (%d alive)", total, alive)

	// Initialize token queue mode
	tokenManager.InitQueue()
	log.Printf("[TOKEN] Queue mode enabled, %d tokens in queue", tokenManager.QueueLen())

	// Start refresh token saver
	tokenManager.StartRefreshTokenSaver(*refreshedTokensFile)
	log.Printf("[TOKEN] Refreshed tokens will be saved to: %s", *refreshedTokensFile)

	// Initialize email reader
	emailReader := reader.NewEmailReader(cfg.EmailsFile, cfg.FileBufferSize)

	// Skip counting emails - too slow for large files
	totalEmails := 0
	log.Printf("[READER] Skipping email count for faster startup")

	// Initialize result writer
	resultWriter, err := writer.NewResultWriter(cfg.ResultsFile)
	if err != nil {
		log.Fatalf("[ERROR] Failed to create result writer: %v", err)
	}
	defer resultWriter.Close()
	log.Printf("[WRITER] Output file: %s", cfg.ResultsFile)

	// Initialize API client
	apiClient := api.NewClient(cfg.APITimeout)
	log.Println("[API] Client initialized")

	// Initialize worker pool
	pool := worker.NewPool(cfg.NumWorkers, apiClient, tokenManager, resultWriter, cfg.EmailBufferSize, cfg.MaxCPM)

	// Start progress reporter
	pool.StartProgressReporter(5*time.Second, totalEmails)

	// Start workers
	pool.Start()

	// Set up graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start reading emails
	startTime := time.Now()
	emailChan, errChan := emailReader.ReadEmails()

	// Done channel to signal completion
	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() {
		closeOnce.Do(func() {
			close(done)
		})
	}

	// Process emails from channel
	go func() {
		for email := range emailChan {
			pool.Submit(email)
		}
		// Check for errors
		if err := <-errChan; err != nil {
			log.Printf("[ERROR] Failed to read emails: %v", err)
		}
		// Signal that all emails have been submitted
		pool.Close()
		closeDone()
	}()

	// Wait for completion or interrupt
	go func() {
		<-sigChan
		log.Println("\n[SHUTDOWN] Received interrupt signal, shutting down...")
		pool.Shutdown()
		closeDone()
	}()

	// Wait for processing to complete
	<-done
	time.Sleep(500 * time.Millisecond) // Give workers time to finish

	// Stop refresh token saver and flush remaining tokens
	tokenManager.StopRefreshTokenSaver()
	log.Printf("[TOKEN] Refreshed tokens saved to: %s", *refreshedTokensFile)

	// Save alive tokens for next run
	aliveTokensFile := "tokens_alive.txt"
	aliveCount, err := tokenManager.SaveAliveTokens(aliveTokensFile)
	if err != nil {
		log.Printf("[TOKEN] Failed to save alive tokens: %v", err)
	} else {
		log.Printf("[TOKEN] Saved %d alive tokens to: %s", aliveCount, aliveTokensFile)
	}

	// Print final statistics
	elapsed := time.Since(startTime)
	processed, successful, failed, exactMatch := pool.Stats()

	fmt.Println(repeatString("=", 60))
	log.Println("=== FINAL STATISTICS ===")

	// Show stop reason
	if pool.StoppedEarly() {
		log.Printf("Stop Reason:     %s", pool.StopReason())
	} else {
		log.Printf("Stop Reason:     completed (all emails processed)")
	}

	log.Printf("Total Emails:    %d", totalEmails)
	log.Printf("Processed:       %d", processed)
	log.Printf("Successful:      %d", successful)
	log.Printf("Failed:          %d", failed)
	log.Printf("ExactMatch:      %d", exactMatch)
	log.Printf("Results Written: %d", resultWriter.Count())
	log.Printf("Elapsed Time:    %s", elapsed.Round(time.Second))
	log.Printf("Rate:            %.1f emails/second", float64(processed)/elapsed.Seconds())

	// Token stats
	tTotal, tAlive, tDead := tokenManager.Stats()
	log.Printf("Tokens:          %d total, %d alive, %d dead", tTotal, tAlive, tDead)

	fmt.Println(repeatString("=", 60))
	log.Printf("Results saved to: %s", cfg.ResultsFile)

	if pool.StoppedEarly() {
		log.Printf("Application stopped early: %s", pool.StopReason())
	} else {
		log.Println("Application finished successfully.")
	}
}

// repeatString returns a string repeated n times
func repeatString(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
