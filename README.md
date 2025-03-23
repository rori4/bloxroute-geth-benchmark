# bloxroute-geth-benchmark

## How to Run

To run the `bloxroute-geth-benchmark` application, use the following command:

```bash
go run main.go -bloxroute "wss://api.blxrbdn.com/ws" -auth "your_auth_token" -geth "ws://localhost:8546" -log "tx_comparison.log" -duration 60s
```