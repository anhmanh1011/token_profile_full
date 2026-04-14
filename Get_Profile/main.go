//go:build !hot
// +build !hot

package main

import (
	"context"
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
	startTimestamp := time.Now().Format("2006-01-02_15-04-05")

	// Parse command line arguments
	emailsFile := flag.String("emails", "", "Path to emails file (default: emails.txt)")
	resultFile := flag.String("result", "", "Path to result file (will use timestamp if not specified)")
	apiAddr := flag.String("api", "", "Python API service address (default: http://localhost:5000)")
	numWorkers := flag.Int("workers", 550, "Number of workers")
	instanceID := flag.String("id", "", "Instance ID for logging (optional)")
	maxCPM := flag.Int("max-cpm", 0, "Max requests per minute (0 = use default 20000)")
	flag.Parse()

	// Generate timestamped filenames
	logFileName := fmt.Sprintf("output_%s.log", startTimestamp)
	resultFileName := fmt.Sprintf("result_%s.txt", startTimestamp)
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
	cfg.NumWorkers = *numWorkers
	cfg.MaxCPM = 20000 // Hard limit: 20K CPM
	if *apiAddr != "" {
		cfg.APIAddr = *apiAddr
	}
	if *emailsFile != "" {
		cfg.EmailsFile = *emailsFile
	}
	cfg.ResultsFile = resultFileName
	if *maxCPM > 0 {
		cfg.MaxCPM = *maxCPM
	}

	log.Printf("[CONFIG] Workers: %d | APITimeout: %ds | EmailBuffer: %d | MaxCPM: %d",
		cfg.NumWorkers, cfg.APITimeout, cfg.EmailBufferSize, cfg.MaxCPM)
	log.Printf("[CONFIG] API: %s", cfg.APIAddr)
	log.Printf("[FILES] Emails: %s | Result: %s", cfg.EmailsFile, cfg.ResultsFile)

	// Check if emails file exists
	if _, err := os.Stat(cfg.EmailsFile); os.IsNotExist(err) {
		log.Fatalf("[ERROR] Emails file not found: %s", cfg.EmailsFile)
	}

	// Create API client for token fetching and user deletion
	apiClient := token.NewAPIClient(cfg.APIAddr)
	log.Printf("[API] Token API client initialized: %s", cfg.APIAddr)

	// Initialize token manager with empty queue
	tokenManager := token.NewManager()
	tokenManager.InitEmptyQueue(2000)
	log.Println("[TOKEN] Queue mode enabled (empty queue, pre-fetching tokens...)")

	// Pre-fetch initial batch of tokens
	const prefetchCount = 50
	fetched := 0
	for fetched < prefetchCount {
		t, err := apiClient.FetchToken()
		if err != nil {
			log.Printf("[TOKEN] Pre-fetch error after %d tokens: %v", fetched, err)
			break
		}
		if t == nil {
			// 202: queue temporarily empty, wait and retry
			log.Printf("[TOKEN] Pre-fetch: API queue empty, waiting 2s... (%d/%d fetched)", fetched, prefetchCount)
			time.Sleep(2 * time.Second)
			continue
		}
		tokenManager.AddToken(t)
		fetched++
	}
	if fetched == 0 {
		log.Fatalf("[ERROR] Failed to pre-fetch any tokens from API")
	}
	log.Printf("[TOKEN] Pre-fetched %d tokens, queue length: %d", fetched, tokenManager.QueueLen())

	// Create dead token channel and set on manager
	deadChan := make(chan string, 1000)
	tokenManager.SetDeadChan(deadChan)

	// Bridge: deadChan → apiClient.QueueDelete
	go func() {
		for email := range deadChan {
			apiClient.QueueDelete(email)
		}
	}()

	// Start delete worker
	deleteCtx, deleteCancel := context.WithCancel(context.Background())
	var deleteWg sync.WaitGroup
	deleteWg.Add(1)
	apiClient.StartDeleteWorker(deleteCtx, &deleteWg)
	log.Println("[API] Delete worker started")

	// Background token fetcher goroutine
	fetchCtx, fetchCancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-fetchCtx.Done():
				return
			default:
			}

			if tokenManager.QueueLen() < 50 {
				t, err := apiClient.FetchToken()
				if err != nil {
					log.Printf("[TOKEN] Background fetch error: %v", err)
					time.Sleep(2 * time.Second)
					continue
				}
				if t == nil {
					// 202: queue empty, wait before retry
					time.Sleep(2 * time.Second)
					continue
				}
				tokenManager.AddToken(t)
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}()
	log.Println("[TOKEN] Background token fetcher started")

	// Initialize email reader
	emailReader := reader.NewEmailReader(cfg.EmailsFile, cfg.FileBufferSize)
	totalEmails := 0
	log.Printf("[READER] Skipping email count for faster startup")

	// Initialize result writer
	resultWriter, err := writer.NewResultWriter(cfg.ResultsFile)
	if err != nil {
		log.Fatalf("[ERROR] Failed to create result writer: %v", err)
	}
	defer resultWriter.Close()
	log.Printf("[WRITER] Output file: %s", cfg.ResultsFile)

	// Initialize Loki API client
	lokiClient := api.NewClient(cfg.APITimeout)
	log.Println("[API] Loki client initialized")

	// Initialize worker pool
	pool := worker.NewPool(cfg.NumWorkers, lokiClient, tokenManager, resultWriter, cfg.EmailBufferSize, cfg.MaxCPM)
	pool.StartProgressReporter(5*time.Second, totalEmails)
	pool.Start()

	// Set up graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start reading emails
	startTime := time.Now()
	emailChan, errChan := emailReader.ReadEmails()

	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() {
		closeOnce.Do(func() {
			close(done)
		})
	}

	go func() {
		for email := range emailChan {
			pool.Submit(email)
		}
		if err := <-errChan; err != nil {
			log.Printf("[ERROR] Failed to read emails: %v", err)
		}
		pool.Close()
		closeDone()
	}()

	go func() {
		<-sigChan
		log.Println("\n[SHUTDOWN] Received interrupt signal, shutting down...")
		pool.Shutdown()
		closeDone()
	}()

	<-done
	time.Sleep(500 * time.Millisecond)

	// Shutdown cleanup
	fetchCancel()
	close(deadChan)
	apiClient.CloseDeleteChan()
	deleteCancel()
	deleteWg.Wait()

	// Print final statistics
	elapsed := time.Since(startTime)
	processed, successful, failed, exactMatch := pool.Stats()

	fmt.Println(repeatString("=", 60))
	log.Println("=== FINAL STATISTICS ===")

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
	if elapsed.Seconds() > 0 {
		log.Printf("Rate:            %.1f emails/second", float64(processed)/elapsed.Seconds())
	}

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

func repeatString(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
