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
	"linkedin_fetcher/progress"
	"linkedin_fetcher/reader"
	"linkedin_fetcher/token"
	"linkedin_fetcher/worker"
	"linkedin_fetcher/writer"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tokenFetchBatchSize        = 300
	tokenQueueBaseCapacity     = 2000
	tokenQueueBaseLowWatermark = 100
)

func main() {
	startTimestamp := time.Now().Format("2006-01-02_15-04-05")

	// Parse command line arguments
	emailsFile := flag.String("emails", "", "Path to emails file (default: emails.txt)")
	resultFile := flag.String("result", "", "Path to result file (will use timestamp if not specified)")
	apiAddr := flag.String("api", "", "Python API service address (default: http://localhost:5000)")
	globalConfig := flag.String("config", "", "Path to admin_token_config_global.json")
	poolName := flag.String("pool", "", "Pool ID from admin_token_config_global.json")
	instanceName := flag.String("instance", "", "Alias for --pool")
	numWorkers := flag.Int("workers", 400, "Number of workers")
	instanceID := flag.String("id", "", "Instance ID for logging (optional)")
	maxCPM := flag.Int("max-cpm", 0, "Max requests per minute (0 = use default 20000)")
	checkpointFile := flag.String("checkpoint", "", "Progress bitmap file (default: <emails>.<pool>.ckpt)")
	proxyOverride := flag.String("proxy", "", "SOCKS5 proxy override; empty = fetch from Python /proxy endpoint")
	flag.Parse()

	configPath := strings.TrimSpace(*globalConfig)
	if configPath == "" {
		var err error
		configPath, err = config.DefaultGlobalConfigPath()
		if err != nil {
			fmt.Printf("[ERROR] %v\n", err)
			os.Exit(1)
		}
	}

	requestedPool := strings.TrimSpace(*poolName)
	requestedInstance := strings.TrimSpace(*instanceName)
	if requestedPool != "" && requestedInstance != "" && requestedPool != requestedInstance {
		fmt.Println("[ERROR] --pool and --instance refer to different values")
		os.Exit(1)
	}
	if requestedPool == "" {
		requestedPool = requestedInstance
	}

	globalInstances, err := config.LoadGlobalInstances(configPath)
	if err != nil {
		fmt.Printf("[ERROR] %v\n", err)
		os.Exit(1)
	}
	selectedInstance, err := config.SelectGlobalInstance(globalInstances, requestedPool)
	if err != nil {
		fmt.Printf("[ERROR] %v\n", err)
		os.Exit(1)
	}
	poolID := selectedInstance.ResolvedPoolID()
	runID := strings.TrimSpace(*instanceID)
	if runID == "" {
		runID = poolID
	}

	// Generate timestamped filenames
	logFileName := fmt.Sprintf("output_%s_%s.log", poolID, startTimestamp)
	resultFileName := config.DefaultResultPath(poolID, startTimestamp)
	if selectedInstance.ResultFile != "" {
		resultFileName = selectedInstance.ResultFile
	}
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
	log.SetPrefix(fmt.Sprintf("[%s] ", runID))
	log.Println("=== LinkedIn Profile Fetcher ===")
	log.Printf("Run timestamp: %s", startTimestamp)
	log.Printf("Log file: %s", logFileName)
	log.Printf("Result file: %s", resultFileName)
	log.Printf("Global config: %s", configPath)
	log.Printf("Pool: %s | Domain: %s | EmailFile: %s | BotPrefix: %s",
		poolID, selectedInstance.Domain, selectedInstance.EmailFile, selectedInstance.BotPrefix)
	log.Println("Starting application...")

	// Load configuration
	cfg := config.NewConfig()
	cfg.NumWorkers = *numWorkers
	cfg.MaxCPM = 20000 // Hard limit: 20K CPM
	if selectedInstance.EmailFile != "" {
		cfg.EmailsFile = selectedInstance.EmailFile
	}
	if *apiAddr != "" {
		cfg.APIAddr = *apiAddr
	}
	if *emailsFile != "" {
		cfg.EmailsFile = *emailsFile
	}
	if selectedInstance.Workers > 0 && !flagWasSet("workers") {
		cfg.NumWorkers = selectedInstance.Workers
	}
	cfg.ResultsFile = resultFileName
	if selectedInstance.MaxCPM > 0 && !flagWasSet("max-cpm") {
		cfg.MaxCPM = selectedInstance.MaxCPM
	}
	if *maxCPM > 0 {
		cfg.MaxCPM = *maxCPM
	}

	ckptPath := *checkpointFile
	if ckptPath == "" {
		if selectedInstance.CheckpointFile != "" {
			ckptPath = selectedInstance.CheckpointFile
		} else {
			ckptPath = config.DefaultCheckpointPath(cfg.EmailsFile, poolID)
		}
	}

	log.Printf("[CONFIG] Workers: %d | APITimeout: %ds | EmailBuffer: %d | MaxCPM: %d",
		cfg.NumWorkers, cfg.APITimeout, cfg.EmailBufferSize, cfg.MaxCPM)
	log.Printf("[CONFIG] API: %s", cfg.APIAddr)
	log.Printf("[FILES] Emails: %s | Result: %s | Checkpoint: %s", cfg.EmailsFile, cfg.ResultsFile, ckptPath)

	// Check if emails file exists
	if _, err := os.Stat(cfg.EmailsFile); os.IsNotExist(err) {
		log.Fatalf("[ERROR] Emails file not found: %s", cfg.EmailsFile)
	}

	// Load or create progress bitmap (skip lines already processed in prior runs).
	bitmap, err := progress.LoadOrCreate(ckptPath, cfg.EmailsFile)
	if err != nil {
		log.Fatalf("[CHECKPOINT] %v", err)
	}
	stopAutoSave := bitmap.StartAutoSaver(10 * time.Second)
	defer stopAutoSave()
	totalEmails := int(bitmap.TotalLines())
	log.Printf("[CHECKPOINT] Total lines: %d | Already done: %d", totalEmails, bitmap.Done())

	// Create API client for token fetching and user deletion (always direct,
	// since the Python service is on localhost — must NOT go through SOCKS5).
	apiClient := token.NewAPIClient(cfg.APIAddr)
	apiClient.SetPoolID(poolID)
	log.Printf("[API] Token API client initialized: %s (pool=%s)", cfg.APIAddr, poolID)

	// Resolve SOCKS5 proxy: --proxy flag overrides; otherwise ask the Python
	// service which proxy is bound to the selected global config pool.
	cfg.Proxy = *proxyOverride
	if cfg.Proxy == "" {
		if p, err := apiClient.FetchProxy(); err != nil {
			if selectedInstance.Proxy != "" {
				cfg.Proxy = selectedInstance.Proxy
				log.Printf("[PROXY] Failed to fetch pool proxy from Python service: %v (using global config proxy)", err)
			} else {
				log.Printf("[PROXY] Failed to fetch pool proxy from Python service: %v (continuing direct)", err)
			}
		} else {
			cfg.Proxy = p
		}
	}
	if cfg.Proxy != "" {
		log.Printf("[PROXY] SOCKS5 enabled for Loki + token-exchange traffic")
	} else {
		log.Println("[PROXY] No proxy configured — direct dialing")
	}

	// Initialize token manager with empty queue
	tokenQueueCapacity, tokenLowWatermark, tokenHighWatermark, initialTokenTarget := tokenQueueSettings(cfg.NumWorkers)
	tokenManager := token.NewManagerWithProxy(cfg.Proxy)
	tokenManager.InitEmptyQueue(tokenQueueCapacity)
	log.Printf("[TOKEN] Queue mode enabled (capacity=%d, low=%d, high=%d, initial=%d)",
		tokenQueueCapacity, tokenLowWatermark, tokenHighWatermark, initialTokenTarget)

	// Pre-fetch enough tokens for the configured worker count.
	log.Println("[TOKEN] Fetching initial tokens from API...")
	var fetched int
	for tokenManager.QueueLen() < initialTokenTarget {
		tokens, err := apiClient.FetchTokens(tokenFetchBatchSize)
		if err != nil {
			if fetched == 0 {
				log.Fatalf("[ERROR] Failed to pre-fetch tokens from API: %v", err)
			}
			log.Printf("[TOKEN] Pre-fetch stopped after %d tokens: %v", fetched, err)
			break
		}
		if tokens == nil {
			log.Println("[TOKEN] Pre-fetch: API queue empty, waiting 2s...")
			time.Sleep(2 * time.Second)
			continue
		}
		for _, t := range tokens {
			tokenManager.AddToken(t)
		}
		fetched += len(tokens)
	}
	if fetched == 0 {
		log.Fatalf("[ERROR] Failed to pre-fetch any tokens from API")
	}
	log.Printf("[TOKEN] Pre-fetched %d tokens, queue length: %d", fetched, tokenManager.QueueLen())

	// Create dead token channel and set on manager
	deadChan := make(chan string, 1000)
	tokenManager.SetDeadChan(deadChan)

	// Bridge: deadChan → apiClient.QueueDelete
	var bridgeWg sync.WaitGroup
	bridgeWg.Add(1)
	go func() {
		defer bridgeWg.Done()
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

	// Background token fetcher goroutine (batch 300 per API call)
	fetchCtx, fetchCancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-fetchCtx.Done():
				return
			default:
			}

			if tokenManager.QueueLen() < tokenLowWatermark {
				fetched := 0
				for tokenManager.QueueLen() < tokenHighWatermark {
					select {
					case <-fetchCtx.Done():
						return
					default:
					}

					tokens, err := apiClient.FetchTokens(tokenFetchBatchSize)
					if err != nil {
						log.Printf("[TOKEN] Background fetch error: %v", err)
						time.Sleep(2 * time.Second)
						break
					}
					if tokens == nil {
						time.Sleep(2 * time.Second)
						break
					}
					for _, t := range tokens {
						tokenManager.AddToken(t)
					}
					fetched += len(tokens)
					if len(tokens) < tokenFetchBatchSize {
						break
					}
				}
				if fetched > 0 {
					log.Printf("[TOKEN] Background fetched %d tokens, queue: %d", fetched, tokenManager.QueueLen())
				}
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}()
	log.Println("[TOKEN] Background token fetcher started")

	// Initialize email reader (with bitmap for skip).
	emailReader := reader.NewEmailReader(cfg.EmailsFile, cfg.FileBufferSize, bitmap)

	// Initialize result writer
	resultWriter, err := writer.NewResultWriter(cfg.ResultsFile)
	if err != nil {
		log.Fatalf("[ERROR] Failed to create result writer: %v", err)
	}
	defer resultWriter.Close()
	log.Printf("[WRITER] Output file: %s", cfg.ResultsFile)

	// Initialize Loki API client (Loki traffic flows through SOCKS5 if configured).
	lokiClient := api.NewClientWithProxy(cfg.APITimeout, cfg.Proxy)
	log.Println("[API] Loki client initialized")

	// Initialize worker pool
	pool := worker.NewPool(cfg.NumWorkers, lokiClient, tokenManager, resultWriter, cfg.EmailBufferSize, cfg.MaxCPM, bitmap)
	pool.StartProgressReporter(5*time.Second, totalEmails)
	pool.Start()

	// Set up graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start reading emails. Reader pushes directly into pool's channel via
	// pool.Submit — no intermediate channel, no pump goroutine.
	startTime := time.Now()

	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() {
		closeOnce.Do(func() {
			close(done)
		})
	}

	go func() {
		if err := emailReader.ReadJobsInto(pool.Submit); err != nil {
			log.Printf("[ERROR] Failed to read emails: %v", err)
		}
		pool.Close()
		closeDone()
	}()

	go func() {
		<-sigChan
		log.Println("\n[SHUTDOWN] Received interrupt signal, shutting down...")
		fetchCancel()
		pool.Shutdown()
		closeDone()
	}()

	<-done
	time.Sleep(500 * time.Millisecond)

	// Shutdown cleanup
	fetchCancel()
	close(deadChan)
	bridgeWg.Wait()
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
	log.Printf("Skipped (done):  %d", emailReader.GetSkipCount())
	log.Printf("Submitted:       %d", emailReader.GetTotalCount())
	log.Printf("Processed:       %d", processed)
	log.Printf("Successful:      %d", successful)
	log.Printf("Failed:          %d", failed)
	log.Printf("ExactMatch:      %d", exactMatch)
	log.Printf("Bitmap Done:     %d / %d", bitmap.Done(), bitmap.TotalLines())
	log.Printf("Results Written: %d", resultWriter.Count())
	log.Printf("Elapsed Time:    %s", elapsed.Round(time.Second))
	if elapsed.Seconds() > 0 {
		log.Printf("Rate:            %.1f emails/second", float64(processed)/elapsed.Seconds())
	}

	tTotal, tAlive, tDead := tokenManager.Stats()
	log.Printf("Tokens:          %d total, %d alive, %d dead", tTotal, tAlive, tDead)

	fmt.Println(repeatString("=", 60))
	log.Printf("Results saved to: %s", cfg.ResultsFile)
	log.Printf("Checkpoint saved to: %s", ckptPath)

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

func tokenQueueSettings(workers int) (capacity, lowWatermark, highWatermark, initialTarget int) {
	capacity = maxInt(tokenQueueBaseCapacity, workers*4)
	lowWatermark = maxInt(tokenQueueBaseLowWatermark, workers)
	highWatermark = minInt(capacity, maxInt(lowWatermark+tokenFetchBatchSize, workers*2))
	initialTarget = minInt(highWatermark, maxInt(tokenFetchBatchSize, workers))
	return
}

func flagWasSet(name string) bool {
	wasSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
