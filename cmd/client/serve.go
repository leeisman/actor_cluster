package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/frankieli/actor_cluster/pkg/persistence"
	"github.com/frankieli/actor_cluster/pkg/remote"
	"github.com/frankieli/actor_cluster/pkg/remote/pb"
)

var errAlreadyRunning = errors.New("stress test is already running")

type serveConfig struct {
	clientConfig
	httpAddr       string
	cassandraHosts string
	keyspace       string
}

type executeRequest struct {
	UID    int64 `json:"uid"`
	Amount int64 `json:"amount"`
}

type stressRequest struct {
	TPSTarget   int `json:"tps_target"`
	Concurrency int `json:"concurrency"`
	DurationSec int `json:"duration_sec"`
}

type gatewayServer struct {
	router *Router
	engine *StressEngine
	store  *persistence.CassandraStore
}

func runServeCommand(args []string) error {
	var cfg serveConfig
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	registerSharedFlags(fs, &cfg.clientConfig)
	fs.StringVar(&cfg.httpAddr, "http", ":8080", "HTTP listen address")
	fs.StringVar(&cfg.cassandraHosts, "cassandra", "127.0.0.1:9042", "comma-separated Cassandra hosts")
	fs.StringVar(&cfg.keyspace, "keyspace", "wallet", "Cassandra keyspace")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	router, cleanup, err := newRouterFromConfig(ctx, cfg.clientConfig)
	if err != nil {
		return err
	}
	defer cleanup()

	store, err := persistence.NewCassandraStore(strings.Split(cfg.cassandraHosts, ","), cfg.keyspace)
	if err != nil {
		return fmt.Errorf("cassandra connection failed: %w", err)
	}
	defer store.Close()

	srv := &gatewayServer{
		router: router,
		engine: NewStressEngine(router),
		store:  store,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/stress/start", srv.handleStressStart)
	mux.HandleFunc("/api/stress/stop", srv.handleStressStop)
	mux.HandleFunc("/api/stress/status", srv.handleStressStatus)
	mux.HandleFunc("/api/stress/reset", srv.handleStressReset)
	mux.HandleFunc("/api/wallet/execute", srv.handleExecute)
	mux.HandleFunc("/api/wallet/balance/", srv.handleBalance)
	mux.HandleFunc("/healthz", handleHealth)

	httpServer := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		srv.engine.Stop()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown failed: %v", err)
		}
	}()

	log.Printf("test web gateway listening on %s", cfg.httpAddr)
	err = httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *gatewayServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeHTML(w, gatewayHTML)
}

func (s *gatewayServer) handleStressStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req stressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.TPSTarget <= 0 || req.Concurrency <= 0 || req.DurationSec <= 0 {
		http.Error(w, "tps_target, concurrency, duration_sec must be > 0", http.StatusBadRequest)
		return
	}

	cfg := stressConfig{
		tpsTarget:   req.TPSTarget,
		concurrency: req.Concurrency,
		duration:    time.Duration(req.DurationSec) * time.Second,
	}
	if err := s.engine.Start(cfg); err != nil {
		if errors.Is(err, errAlreadyRunning) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"started": true,
		"status":  s.engine.Status(),
	})
}

func (s *gatewayServer) handleStressStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.engine.Stop()
	writeJSON(w, http.StatusOK, map[string]any{
		"stopped": true,
		"status":  s.engine.Status(),
	})
}

func (s *gatewayServer) handleStressStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.engine.Status())
}

func (s *gatewayServer) handleStressReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.engine.ResetMetrics()
	writeJSON(w, http.StatusOK, map[string]any{
		"reset":  true,
		"status": s.engine.Status(),
	})
}

func (s *gatewayServer) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req executeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.UID <= 0 {
		http.Error(w, "uid must be > 0", http.StatusBadRequest)
		return
	}

	reqID := nextRequestID()
	payloadBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(payloadBuf, uint64(req.Amount))

	env := &pb.RemoteEnvelope{
		TenantId:  1,
		Uid:       req.UID,
		RequestId: reqID,
		TxId:      strconv.FormatUint(reqID, 10),
		OpCode:    1,
		Payload:   payloadBuf,
	}
	res := s.router.ExecuteAndWait(env, 5*time.Second)
	status := http.StatusOK
	if !res.Success {
		status = mapErrorToHTTPStatus(res.ErrorCode)
	}

	response := map[string]any{
		"success":    res.Success,
		"request_id": strconv.FormatUint(reqID, 10),
		"uid":        req.UID,
		"amount":     req.Amount,
		"error_code": res.ErrorCode,
		"error_msg":  res.ErrorMsg,
	}
	if len(res.Payload) >= 8 {
		response["balance"] = int64(binary.BigEndian.Uint64(res.Payload))
	}

	writeJSON(w, status, response)
}

func (s *gatewayServer) handleBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uidText := strings.TrimPrefix(r.URL.Path, "/api/wallet/balance/")
	uid, err := strconv.ParseInt(uidText, 10, 64)
	if err != nil || uid <= 0 {
		http.Error(w, "invalid uid", http.StatusBadRequest)
		return
	}

	balance, lastVersion, err := s.computeBalance(r.Context(), 1, uid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uid":           uid,
		"exact_balance": balance,
		"last_version":  lastVersion,
	})
}

func (s *gatewayServer) computeBalance(ctx context.Context, tenantID int32, uid int64) (int64, int64, error) {
	var snapshotBalance int64
	var lastVersion int64

	snapCQL := `SELECT balance, last_version FROM wallet_snapshots WHERE tenant_id = ? AND uid = ?`
	err := s.store.Query(ctx, snapCQL, []any{tenantID, uid}, func(scanner persistence.Scanner) error {
		for scanner.Scan(&snapshotBalance, &lastVersion) {
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("load snapshot failed: %w", err)
	}

	balance := snapshotBalance
	finalVersion := lastVersion
	eventCQL := `SELECT version, delta_amount FROM wallet_events WHERE tenant_id = ? AND uid = ? AND version > ? ORDER BY version ASC`
	err = s.store.Query(ctx, eventCQL, []any{tenantID, uid, lastVersion}, func(scanner persistence.Scanner) error {
		var version int64
		var delta int64
		for scanner.Scan(&version, &delta) {
			balance += delta
			finalVersion = version
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("load events failed: %w", err)
	}

	return balance, finalVersion, nil
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func mapErrorToHTTPStatus(errCode string) int {
	switch errCode {
	case remote.ErrDeadlineExceeded:
		return http.StatusGatewayTimeout
	case remote.ErrInsufficientFunds, remote.ErrInvalidPayload:
		return http.StatusBadRequest
	case remote.ErrWrongNode, remote.ErrDiscovery, remote.ErrConnection, remote.ErrTransportClosed, remote.ErrStreamInterrupted:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("JSON encode failed: %v", err)
	}
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := fmt.Fprint(w, body); err != nil {
		log.Printf("HTML write failed: %v", err)
	}
}

const gatewayHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Actor Cluster Test Gateway</title>
  <style>
    :root {
      --bg: #08111f;
      --panel: #0f1b2d;
      --panel-border: #1f314d;
      --text: #e5eef9;
      --muted: #8ea3bd;
      --green: #10b981;
      --blue: #38bdf8;
      --red: #ef4444;
      --amber: #f59e0b;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "SFMono-Regular", "Consolas", monospace;
      background:
        radial-gradient(circle at top left, rgba(56, 189, 248, 0.15), transparent 28%),
        radial-gradient(circle at bottom right, rgba(16, 185, 129, 0.12), transparent 24%),
        var(--bg);
      color: var(--text);
    }
    .shell {
      max-width: 1180px;
      margin: 0 auto;
      padding: 32px 20px 48px;
    }
    h1, h2 { margin: 0 0 12px; }
    h1 { font-size: 34px; }
    h2 { font-size: 18px; color: var(--blue); }
    p { color: var(--muted); line-height: 1.5; }
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
      gap: 18px;
      margin-top: 20px;
    }
    .panel {
      background: rgba(15, 27, 45, 0.92);
      border: 1px solid var(--panel-border);
      border-radius: 18px;
      padding: 20px;
      box-shadow: 0 16px 40px rgba(0, 0, 0, 0.25);
    }
    .metrics {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
      gap: 14px;
    }
    .metric {
      background: rgba(8, 17, 31, 0.9);
      border-radius: 14px;
      padding: 14px;
      border: 1px solid rgba(56, 189, 248, 0.18);
    }
    .metric .label {
      color: var(--muted);
      font-size: 12px;
      text-transform: uppercase;
      letter-spacing: 0.08em;
    }
    .metric .value {
      margin-top: 8px;
      font-size: 28px;
      font-weight: 700;
    }
    form { display: grid; gap: 10px; }
    label {
      display: grid;
      gap: 6px;
      font-size: 12px;
      color: var(--muted);
      text-transform: uppercase;
      letter-spacing: 0.06em;
    }
    input, button {
      width: 100%;
      border-radius: 10px;
      border: 1px solid var(--panel-border);
      padding: 12px 14px;
      font: inherit;
    }
    input {
      background: #07101d;
      color: var(--text);
    }
    button {
      background: linear-gradient(135deg, rgba(56, 189, 248, 0.22), rgba(16, 185, 129, 0.24));
      color: var(--text);
      cursor: pointer;
    }
    button.stop {
      background: linear-gradient(135deg, rgba(239, 68, 68, 0.28), rgba(245, 158, 11, 0.22));
    }
    pre {
      margin: 0;
      background: #050b14;
      border: 1px solid rgba(143, 169, 199, 0.12);
      border-radius: 12px;
      padding: 14px;
      min-height: 180px;
      overflow: auto;
      color: #d8e7f6;
    }
    .status {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      padding: 6px 10px;
      border-radius: 999px;
      background: rgba(56, 189, 248, 0.08);
      color: var(--blue);
      font-size: 12px;
    }
    .status.running {
      color: var(--green);
      background: rgba(16, 185, 129, 0.1);
    }
  </style>
</head>
<body>
  <div class="shell">
    <div class="panel">
      <h1>Actor Cluster Test Gateway</h1>
      <p>同一個 <code>cmd/client</code> binary 現在同時能做壓測控制與手動交易注入。這個頁面會每秒輪詢一次狀態。</p>
      <div id="runState" class="status">idle</div>
    </div>

    <div class="grid">
      <section class="panel">
        <h2>Manual Execute</h2>
        <form id="executeForm">
          <label>UID <input type="number" name="uid" min="1" value="1"></label>
          <label>Amount <input type="number" name="amount" value="1"></label>
          <button type="submit">Send Transaction</button>
        </form>
        <form id="balanceForm" style="margin-top: 14px;">
          <label>Balance UID <input type="number" name="uid" min="1" value="1"></label>
          <button type="submit">Query Balance</button>
        </form>
      </section>

      <section class="panel">
        <h2>Stress Control</h2>
        <form id="stressForm">
          <label>TPS Target <input type="number" name="tps_target" min="1" value="50000"></label>
          <label>Concurrency <input type="number" name="concurrency" min="1" value="5000"></label>
          <label>Duration (sec) <input type="number" name="duration_sec" min="1" value="30"></label>
          <button type="submit">Launch Load Test</button>
          <button type="button" class="stop" id="stopBtn">Stop</button>
          <button type="button" id="resetBtn">Clear Metrics</button>
        </form>
      </section>
    </div>

    <section class="panel" style="margin-top: 18px;">
      <h2>Live Metrics</h2>
      <div class="metrics">
        <div class="metric"><div class="label">Current TPS</div><div class="value" id="mTps">0</div></div>
        <div class="metric"><div class="label">Total Sent</div><div class="value" id="mSent">0</div></div>
        <div class="metric"><div class="label">Total Success</div><div class="value" id="mSuccess">0</div></div>
        <div class="metric"><div class="label">Avg Latency</div><div class="value" id="mLatency">0.00 ms</div></div>
      </div>
    </section>

    <section class="panel" style="margin-top: 18px;">
      <h2>Event Log</h2>
      <pre id="log"></pre>
    </section>
  </div>

  <script>
    const logNode = document.getElementById("log");
    const stateNode = document.getElementById("runState");
    const executeUIDInput = document.querySelector('#executeForm input[name="uid"]');
    const balanceUIDInput = document.querySelector('#balanceForm input[name="uid"]');

    function appendLog(title, payload) {
      const line = "[" + new Date().toLocaleTimeString() + "] " + title + "\n" + JSON.stringify(payload, null, 2) + "\n\n";
      logNode.textContent = line + logNode.textContent;
    }

    function syncBalanceUID(force = false) {
      if (force || balanceUIDInput.value === "" || balanceUIDInput.value === "1") {
        balanceUIDInput.value = executeUIDInput.value;
      }
    }

    async function refreshStatus() {
      const res = await fetch("/api/stress/status");
      const data = await res.json();
      document.getElementById("mTps").textContent = data.current_tps;
      document.getElementById("mSent").textContent = data.total_sent;
      document.getElementById("mSuccess").textContent = data.total_success;
      document.getElementById("mLatency").textContent = Number(data.avg_latency_ms).toFixed(2) + " ms";
      stateNode.textContent = data.is_running ? "running" : "idle";
      stateNode.className = data.is_running ? "status running" : "status";
    }

    document.getElementById("executeForm").addEventListener("submit", async (event) => {
      event.preventDefault();
      const body = Object.fromEntries(new FormData(event.target).entries());
      body.uid = Number(body.uid);
      body.amount = Number(body.amount);
      const res = await fetch("/api/wallet/execute", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify(body),
      });
      const data = await res.json();
      appendLog("execute", data);
      balanceUIDInput.value = String(body.uid);
      refreshStatus();
    });

    document.getElementById("balanceForm").addEventListener("submit", async (event) => {
      event.preventDefault();
      const body = Object.fromEntries(new FormData(event.target).entries());
      const uid = Number(body.uid);
      const res = await fetch("/api/wallet/balance/" + uid);
      appendLog("balance.query", await res.json());
    });

    document.getElementById("stressForm").addEventListener("submit", async (event) => {
      event.preventDefault();
      const body = Object.fromEntries(new FormData(event.target).entries());
      body.tps_target = Number(body.tps_target);
      body.concurrency = Number(body.concurrency);
      body.duration_sec = Number(body.duration_sec);
      const res = await fetch("/api/stress/start", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify(body),
      });
      const data = await res.json();
      appendLog("stress.start", data);
      refreshStatus();
    });

    document.getElementById("stopBtn").addEventListener("click", async () => {
      const res = await fetch("/api/stress/stop", {method: "POST"});
      appendLog("stress.stop", await res.json());
      refreshStatus();
    });

    document.getElementById("resetBtn").addEventListener("click", async () => {
      const res = await fetch("/api/stress/reset", {method: "POST"});
      appendLog("metrics.reset", await res.json());
      refreshStatus();
    });

    executeUIDInput.addEventListener("input", () => syncBalanceUID());
    syncBalanceUID(true);
    refreshStatus();
    setInterval(refreshStatus, 1000);
  </script>
</body>
</html>`
