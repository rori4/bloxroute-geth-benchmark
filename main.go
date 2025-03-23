package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
)

// Configuration
type Config struct {
	BloxrouteWSEndpoint string
	BloxrouteAuthHeader string
	GethWSEndpoint      string
	LogFilePath         string
	MonitorDuration     time.Duration
}

// Transaction source
type TxSource int

const (
	SourceBloxroute TxSource = iota
	SourceGeth
	SourceBoth
)

func (s TxSource) String() string {
	switch s {
	case SourceBloxroute:
		return "Bloxroute"
	case SourceGeth:
		return "Geth"
	case SourceBoth:
		return "Both"
	default:
		return "Unknown"
	}
}

// Transaction tracking information
type TxInfo struct {
	Hash            common.Hash
	FirstSeenTime   time.Time
	FirstSeenBy     TxSource
	IncludedInBlock uint64
	IncludedAt      time.Time
	mu              sync.Mutex
}

// Bloxroute subscription request
type BloxrouteSubscribeRequest struct {
	ID     int           `json:"id"`
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
}

// Bloxroute transaction response
type BloxrouteResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Result  string `json:"result,omitempty"`
	Params  struct {
		Subscription string `json:"subscription"`
		Result       struct {
			TxHash string `json:"txHash"`
		} `json:"result"`
	} `json:"params,omitempty"`
}

// Results container
type ComparisonResults struct {
	TxMap                 map[common.Hash]*TxInfo
	TotalTxs              int
	BloxrouteFirstCount   int
	GethFirstCount        int
	BothSimultaneousCount int
	IncludedInBlockCount  int
	mu                    sync.Mutex
}

func main() {
	// Parse command line flags
	bloxrouteWS := flag.String("bloxroute", "wss://api.blxrbdn.com/ws", "Bloxroute WebSocket endpoint")
	bloxrouteAuth := flag.String("auth", "", "Bloxroute authorization header")
	gethWS := flag.String("geth", "ws://localhost:8546", "Geth WebSocket endpoint")
	logFile := flag.String("log", "tx_comparison.log", "Log file path")
	duration := flag.Duration("duration", 60*time.Second, "Monitoring duration")
	flag.Parse()

	if *bloxrouteAuth == "" {
		log.Fatal("Bloxroute authorization header is required")
	}

	config := Config{
		BloxrouteWSEndpoint: *bloxrouteWS,
		BloxrouteAuthHeader: *bloxrouteAuth,
		GethWSEndpoint:      *gethWS,
		LogFilePath:         *logFile,
		MonitorDuration:     *duration,
	}

	// Setup logging
	f, err := os.OpenFile(config.LogFilePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("Error opening log file: %v", err)
	}
	defer f.Close()
	log.SetOutput(f)
	log.Printf("Starting transaction comparison between Bloxroute and Geth for %v", config.MonitorDuration)

	// Initialize results
	results := &ComparisonResults{
		TxMap: make(map[common.Hash]*TxInfo),
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		log.Println("Interrupt received, shutting down...")
		cancel()
	}()

	// Start monitoring
	var wg sync.WaitGroup
	wg.Add(3)

	// Monitor Bloxroute transactions
	log.Println("Starting Bloxroute monitor")
	go monitorBloxroute(ctx, &wg, config, results)

	// Monitor Geth transactions
	go monitorGeth(ctx, &wg, config, results)

	// Monitor block inclusions
	go monitorBlockInclusions(ctx, &wg, config, results)

	// Set timer to end monitoring after specified duration
	go func() {
		select {
		case <-time.After(config.MonitorDuration):
			log.Printf("Monitoring duration of %v completed", config.MonitorDuration)
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	// Wait for all goroutines to complete
	wg.Wait()

	// Print final results
	printResults(results)
}

// Monitor transactions from Bloxroute
func monitorBloxroute(ctx context.Context, wg *sync.WaitGroup, config Config, results *ComparisonResults) {
	defer wg.Done()

	// Connect to Bloxroute WebSocket
	dialer := websocket.DefaultDialer
	header := map[string][]string{
		"Authorization": {config.BloxrouteAuthHeader},
	}

	conn, _, err := dialer.Dial(config.BloxrouteWSEndpoint, header)
	if err != nil {
		log.Fatalf("Failed to connect to Bloxroute: %v", err)
	}
	defer conn.Close()

	log.Println("Connected to Bloxroute")
	// Subscribe to new transactions
	subscribeReq := BloxrouteSubscribeRequest{
		ID:     1,
		Method: "subscribe",
		Params: []interface{}{"newTxs", map[string][]string{"include": {"tx_hash"}}},
	}

	err = conn.WriteJSON(subscribeReq)
	if err != nil {
		log.Fatalf("Failed to subscribe to Bloxroute: %v", err)
	}

	// Process subscription confirmation
	_, _, err = conn.ReadMessage()
	if err != nil {
		log.Fatalf("Failed to read subscription confirmation: %v", err)
	}

	// Process incoming transactions
	for {
		select {
		case <-ctx.Done():
			return
		default:
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Error reading from Bloxroute WebSocket: %v", err)
				return
			}

			var response BloxrouteResponse
			if err := json.Unmarshal(message, &response); err != nil {
				log.Printf("Error parsing Bloxroute response: %v", err)
				continue
			}

			log.Printf("Bloxroute response: %v", response.Params.Result.TxHash)

			// Skip if not a transaction notification
			if response.Params.Result.TxHash == "" {
				continue
			}

			txHash := common.HexToHash(response.Params.Result.TxHash)
			processTransaction(txHash, SourceBloxroute, results)
		}
	}
}

// Monitor transactions from Geth
func monitorGeth(ctx context.Context, wg *sync.WaitGroup, config Config, results *ComparisonResults) {
	defer wg.Done()

	// Connect to Geth WebSocket
	client, err := rpc.Dial(config.GethWSEndpoint)
	if err != nil {
		log.Fatalf("Failed to connect to Geth: %v", err)
	}

	gethClient := gethclient.New(client)

	// Create channel for new pending transactions
	pendingTxs := make(chan *types.Transaction, 1000)
	sub, err := gethClient.SubscribeFullPendingTransactions(ctx, pendingTxs)
	if err != nil {
		log.Fatalf("Failed to subscribe to pending transactions: %v", err)
	}
	defer sub.Unsubscribe()

	// Process incoming transactions
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-sub.Err():
			log.Printf("Geth subscription error: %v", err)
			return
		case tx := <-pendingTxs:
			processTransaction(tx.Hash(), SourceGeth, results)
		}
	}
}

// Monitor block inclusions
func monitorBlockInclusions(ctx context.Context, wg *sync.WaitGroup, config Config, results *ComparisonResults) {
	defer wg.Done()

	// Connect to Geth WebSocket for block headers
	client, err := ethclient.Dial(config.GethWSEndpoint)
	if err != nil {
		log.Fatalf("Failed to connect to Geth for block monitoring: %v", err)
	}

	// Subscribe to new block headers
	headers := make(chan *types.Header)
	sub, err := client.SubscribeNewHead(ctx, headers)
	if err != nil {
		log.Fatalf("Failed to subscribe to new headers: %v", err)
	}
	defer sub.Unsubscribe()

	// Process new blocks
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-sub.Err():
			log.Printf("Block header subscription error: %v", err)
			return
		case header := <-headers:
			processBlock(ctx, client, header, results)
		}
	}
}

// Process a new transaction
func processTransaction(txHash common.Hash, source TxSource, results *ComparisonResults) {
	results.mu.Lock()
	defer results.mu.Unlock()

	now := time.Now()
	info, exists := results.TxMap[txHash]

	if !exists {
		// First time seeing this transaction
		results.TxMap[txHash] = &TxInfo{
			Hash:          txHash,
			FirstSeenTime: now,
			FirstSeenBy:   source,
		}

		if source == SourceBloxroute {
			results.BloxrouteFirstCount++
		} else {
			results.GethFirstCount++
		}

		results.TotalTxs++
		log.Printf("New transaction %s first seen by %s", txHash.Hex(), source)
	} else if info.FirstSeenBy != source && info.FirstSeenBy != SourceBoth {
		// Seen by the other source within a very small time window (10ms)
		if now.Sub(info.FirstSeenTime) < 10*time.Millisecond {
			info.FirstSeenBy = SourceBoth
			results.BloxrouteFirstCount--
			results.GethFirstCount--
			results.BothSimultaneousCount++
			log.Printf("Transaction %s seen simultaneously by both sources", txHash.Hex())
		} else {
			log.Printf("Transaction %s seen by %s after %v", txHash.Hex(), source, now.Sub(info.FirstSeenTime))
		}
	}
}

// Process a new block
func processBlock(ctx context.Context, client *ethclient.Client, header *types.Header, results *ComparisonResults) {
	// Get full block with transactions
	block, err := client.BlockByHash(ctx, header.Hash())
	if err != nil {
		log.Printf("Failed to get block %s: %v", header.Number.String(), err)
		return
	}

	blockTime := time.Now() // Approximate block time
	log.Printf("Processing block #%s with %d transactions", block.Number().String(), len(block.Transactions()))

	// Check transactions in this block
	for _, tx := range block.Transactions() {
		txHash := tx.Hash()

		results.mu.Lock()
		info, exists := results.TxMap[txHash]
		if exists && info.IncludedInBlock == 0 {
			// This is a transaction we've seen and it's now included in a block
			info.IncludedInBlock = block.NumberU64()
			info.IncludedAt = blockTime
			results.IncludedInBlockCount++

			log.Printf("Transaction %s included in block #%d after %v (first seen by %s)",
				txHash.Hex(), block.NumberU64(), blockTime.Sub(info.FirstSeenTime), info.FirstSeenBy)
		}
		results.mu.Unlock()
	}
}

// Print final comparison results
func printResults(results *ComparisonResults) {
	results.mu.Lock()
	defer results.mu.Unlock()

	fmt.Println("\n=== Transaction Comparison Results ===")
	fmt.Printf("Total transactions observed: %d\n", results.TotalTxs)
	fmt.Printf("Transactions first seen by Bloxroute: %d (%.2f%%)\n",
		results.BloxrouteFirstCount, float64(results.BloxrouteFirstCount)/float64(results.TotalTxs)*100)
	fmt.Printf("Transactions first seen by Geth: %d (%.2f%%)\n",
		results.GethFirstCount, float64(results.GethFirstCount)/float64(results.TotalTxs)*100)
	fmt.Printf("Transactions seen simultaneously: %d (%.2f%%)\n",
		results.BothSimultaneousCount, float64(results.BothSimultaneousCount)/float64(results.TotalTxs)*100)
	fmt.Printf("Transactions included in blocks: %d (%.2f%%)\n",
		results.IncludedInBlockCount, float64(results.IncludedInBlockCount)/float64(results.TotalTxs)*100)

	// Calculate average time to block inclusion
	var totalBloxrouteTime, totalGethTime time.Duration
	var bloxrouteCount, gethCount int

	for _, info := range results.TxMap {
		if info.IncludedInBlock > 0 {
			if info.FirstSeenBy == SourceBloxroute {
				totalBloxrouteTime += info.IncludedAt.Sub(info.FirstSeenTime)
				bloxrouteCount++
			} else if info.FirstSeenBy == SourceGeth {
				totalGethTime += info.IncludedAt.Sub(info.FirstSeenTime)
				gethCount++
			}
		}
	}

	if bloxrouteCount > 0 {
		fmt.Printf("Average time to block inclusion for Bloxroute-first transactions: %v\n",
			totalBloxrouteTime/time.Duration(bloxrouteCount))
	}

	if gethCount > 0 {
		fmt.Printf("Average time to block inclusion for Geth-first transactions: %v\n",
			totalGethTime/time.Duration(gethCount))
	}

	// Log the same information
	log.Println("\n=== Transaction Comparison Results ===")
	log.Printf("Total transactions observed: %d", results.TotalTxs)
	log.Printf("Transactions first seen by Bloxroute: %d (%.2f%%)",
		results.BloxrouteFirstCount, float64(results.BloxrouteFirstCount)/float64(results.TotalTxs)*100)
	log.Printf("Transactions first seen by Geth: %d (%.2f%%)",
		results.GethFirstCount, float64(results.GethFirstCount)/float64(results.TotalTxs)*100)
	log.Printf("Transactions seen simultaneously: %d (%.2f%%)",
		results.BothSimultaneousCount, float64(results.BothSimultaneousCount)/float64(results.TotalTxs)*100)
	log.Printf("Transactions included in blocks: %d (%.2f%%)",
		results.IncludedInBlockCount, float64(results.IncludedInBlockCount)/float64(results.TotalTxs)*100)

	if bloxrouteCount > 0 {
		log.Printf("Average time to block inclusion for Bloxroute-first transactions: %v",
			totalBloxrouteTime/time.Duration(bloxrouteCount))
	}

	if gethCount > 0 {
		log.Printf("Average time to block inclusion for Geth-first transactions: %v",
			totalGethTime/time.Duration(gethCount))
	}
}
