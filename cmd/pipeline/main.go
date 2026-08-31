// Command pipeline is a miniature, runnable homage to Zerodha's
// "1.5 million PDFs in 25 minutes" nightly contract-note workflow
// (https://zerodha.tech/blog/1-5-million-pdfs-in-25-minutes/).
//
//	pipeline seed  -clients 200            # fabricate exchange trade data (CSV)
//	pipeline run   -gen 8 -sign 4 -mail 4  # run the distributed pipeline
//
// Stages, each with its own worker pool pulling jobs from Redis:
//
//	CSV trades ──▶ [generate] Typst→PDF ──▶ [sign] mock signer ──▶ [email] .eml outbox
//	                    │                        │                      │
//	                    └── object store (S3-style, 0-9 prefix partitioned) ──┘
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example/contract-notes-pipeline/internal/gen"
	"github.com/example/contract-notes-pipeline/internal/mail"
	"github.com/example/contract-notes-pipeline/internal/queue"
	"github.com/example/contract-notes-pipeline/internal/sign"
	"github.com/example/contract-notes-pipeline/internal/store"
)

const (
	dataCSV   = "data/trades.csv"
	storeRoot = "out/objectstore"
	outbox    = "out/outbox"
	template  = "templates/contract_note.typ"
)

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		log.Fatal("usage: pipeline <seed|run> [flags]")
	}
	switch os.Args[1] {
	case "seed":
		fs := flag.NewFlagSet("seed", flag.ExitOnError)
		n := fs.Int("clients", 200, "number of clients to fabricate")
		fs.Parse(os.Args[2:])
		seed(*n)
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		genW := fs.Int("gen", 8, "PDF-generation workers (CPU-heavy pool)")
		signW := fs.Int("sign", 4, "signing workers")
		mailW := fs.Int("mail", 4, "email workers")
		redisAddr := fs.String("redis", "localhost:6379", "redis broker address")
		fs.Parse(os.Args[2:])
		run(*genW, *signW, *mailW, *redisAddr)
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

// ---------------------------------------------------------------- seed ----

var (
	symbols   = []string{"RELIANCE", "TCS", "INFY", "HDFCBANK", "SBIN", "TATAMOTORS", "ITC", "WIPRO", "ICICIBANK", "BHARTIARTL"}
	first     = []string{"Aarav", "Diya", "Vihaan", "Ananya", "Arjun", "Ishaan", "Meera", "Rohan", "Sara", "Kabir"}
	last      = []string{"Sharma", "Patel", "Reddy", "Iyer", "Khan", "Gupta", "Nair", "Singh", "Das", "Mehta"}
	exchanges = []string{"NSE", "BSE"}
)

// seed fabricates the "CSV files from the exchanges" that the real
// pipeline starts from: one row per trade, many trades per client.
func seed(clients int) {
	if err := os.MkdirAll(filepath.Dir(dataCSV), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(dataCSV)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"client_id", "name", "email", "order_no", "time", "exchange", "side", "symbol", "qty", "price"})

	rng := rand.New(rand.NewSource(42))
	rows := 0
	for i := range clients {
		id := fmt.Sprintf("CL%04d", i+1)
		name := first[rng.Intn(len(first))] + " " + last[rng.Intn(len(last))]
		email := fmt.Sprintf("%s@example.com", id)
		for t := 0; t < 3+rng.Intn(12); t++ {
			w.Write([]string{
				id, name, email,
				fmt.Sprintf("%d", 1100000000+rng.Intn(900000000)),
				fmt.Sprintf("%02d:%02d:%02d", 9+rng.Intn(6), rng.Intn(60), rng.Intn(60)),
				exchanges[rng.Intn(2)],
				[]string{"B", "S"}[rng.Intn(2)],
				symbols[rng.Intn(len(symbols))],
				fmt.Sprintf("%d", 1+rng.Intn(500)),
				fmt.Sprintf("%.2f", 100+rng.Float64()*3900),
			})
			rows++
		}
	}
	log.Printf("seeded %s: %d clients, %d trades", dataCSV, clients, rows)
}

// ----------------------------------------------------------------- run ----

type trade struct {
	OrderNo  string  `json:"order_no"`
	Time     string  `json:"time"`
	Exchange string  `json:"exchange"`
	Side     string  `json:"side"`
	Symbol   string  `json:"symbol"`
	Qty      int     `json:"qty"`
	Price    string  `json:"price"`
	Value    string  `json:"value"`
	PriceF   float64 `json:"-"`
	ValueF   float64 `json:"-"`
}

type client struct {
	ID, Name, Email string
	Trades          []trade
}

func run(genW, signW, mailW int, redisAddr string) {
	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.New(storeRoot)
	if err != nil {
		log.Fatal(err)
	}
	q, err := queue.New(redisAddr)
	if err != nil {
		log.Fatal(err)
	}
	if err := q.Reset(ctx); err != nil {
		log.Fatal(err)
	}

	clients := loadClients()
	total := int64(len(clients))
	log.Printf("run: %d clients | workers: %d generate, %d sign, %d email", total, genW, signW, mailW)

	// Producer: enqueue one generate job per client (the real system's
	// data-processing stage, which turns exchange CSVs into job units).
	meta := map[string]client{}
	for _, c := range clients {
		meta[c.ID] = c
		payload, _ := json.Marshal(buildNoteData(c))
		if err := q.Enqueue(ctx, "generate", queue.Job{ID: c.ID, DataJSON: string(payload)}); err != nil {
			log.Fatal(err)
		}
	}

	g := &gen.Generator{TemplatePath: template, WorkDir: os.TempDir(), Store: st}
	sg := &sign.Signer{Identity: "CN=Sample Broking Ltd, O=Demo Pipeline", Store: st}
	ml := &mail.Mailer{From: "Contract Notes <noreply@samplebroking.example>", Outbox: outbox, Store: st}

	var done, failed int64
	var wg sync.WaitGroup

	// pool spins up n workers that drain one stage's queue. In
	// production these pools live on separate Nomad-scheduled machines
	// sized for the stage (big instances for typst, small for the rest).
	pool := func(stage string, n int, handle func(*queue.Job) error) {
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					j, err := q.Dequeue(ctx, stage, time.Second)
					if err != nil || j == nil {
						if ctx.Err() != nil || j == nil && atomic.LoadInt64(&done)+atomic.LoadInt64(&failed) >= total {
							return
						}
						continue
					}
					if err := handle(j); err != nil {
						log.Printf("[%s] %s FAILED: %v", stage, j.ID, err)
						q.Fail(ctx, j, err)
						atomic.AddInt64(&failed, 1)
						continue
					}
					q.Done(ctx, j)
				}
			}()
		}
	}

	pool("generate", genW, func(j *queue.Job) error {
		key, err := g.Render(j.ID, j.DataJSON)
		if err != nil {
			return err
		}
		return q.Enqueue(ctx, "sign", queue.Job{ID: j.ID, ObjKey: key})
	})
	pool("sign", signW, func(j *queue.Job) error {
		key, err := sg.Sign(j.ID, j.ObjKey)
		if err != nil {
			return err
		}
		return q.Enqueue(ctx, "email", queue.Job{ID: j.ID, ObjKey: key})
	})
	pool("email", mailW, func(j *queue.Job) error {
		c := meta[j.ID]
		if _, err := ml.Send(c.ID, c.Name, c.Email, j.ObjKey); err != nil {
			return err
		}
		n := atomic.AddInt64(&done, 1)
		if n%50 == 0 || n == total {
			log.Printf("progress: %d/%d notes delivered (%.1f/sec)", n, total, float64(n)/time.Since(start).Seconds())
		}
		return nil
	})

	// Wait for the run to drain, then stop the pools.
	for atomic.LoadInt64(&done)+atomic.LoadInt64(&failed) < total {
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	elapsed := time.Since(start)
	log.Printf("──────────────────────────────────────────────")
	log.Printf("finished: %d delivered, %d failed in %s (%.1f notes/sec)",
		done, failed, elapsed.Round(time.Millisecond), float64(done)/elapsed.Seconds())
	log.Printf("signed PDFs in %s, emails in %s", storeRoot, outbox)
}

// loadClients parses the seeded CSV back into per-client job units.
func loadClients() []client {
	f, err := os.Open(dataCSV)
	if err != nil {
		log.Fatalf("no trade data — run `pipeline seed` first: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		log.Fatal(err)
	}
	byID := map[string]*client{}
	var order []string
	for _, r := range rows[1:] {
		c, ok := byID[r[0]]
		if !ok {
			c = &client{ID: r[0], Name: r[1], Email: r[2]}
			byID[r[0]] = c
			order = append(order, r[0])
		}
		var qty int
		var price float64
		fmt.Sscan(r[8], &qty)
		fmt.Sscan(r[9], &price)
		value := float64(qty) * price
		c.Trades = append(c.Trades, trade{
			OrderNo: r[3], Time: r[4], Exchange: r[5], Side: r[6], Symbol: r[7],
			Qty: qty, Price: money(price), Value: money(value),
			PriceF: price, ValueF: value,
		})
	}
	out := make([]client, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// buildNoteData assembles the JSON document the Typst template consumes,
// including the charges summary computed pipeline-side.
func buildNoteData(c client) map[string]any {
	var gross, net float64
	for _, t := range c.Trades {
		gross += t.ValueF
		if t.Side == "B" {
			net -= t.ValueF
		} else {
			net += t.ValueF
		}
	}
	brokerage := min(round2(gross*0.0003), 20*float64(len(c.Trades)))
	txn := round2(gross * 0.0000345)
	stt := round2(gross * 0.00025)
	gst := round2((brokerage + txn) * 0.18)
	other := round2(gross*0.000001 + 15)
	charges := brokerage + txn + stt + gst + other
	netAmt := net - charges
	side := "receivable"
	if netAmt < 0 {
		side, netAmt = "payable", -netAmt
	}
	return map[string]any{
		"broker": map[string]string{
			"name":      "Sample Broking Ltd",
			"address":   "42 Demo Street, Bengaluru 560001",
			"sebi_regn": "INZ000000000 (sample)",
		},
		"note_no":      "CN-" + time.Now().Format("20060102") + "-" + c.ID,
		"trade_date":   time.Now().Format("02 Jan 2006"),
		"settlement":   "T+1",
		"generated_at": time.Now().Format("02 Jan 2006 15:04 MST"),
		"client":       map[string]string{"id": c.ID, "name": c.Name, "email": c.Email},
		"trades":       c.Trades,
		"summary": map[string]any{
			"gross": money(gross), "brokerage": money(brokerage), "txn_charges": money(txn),
			"stt": money(stt), "gst": money(gst), "other": money(other),
			"net": money(netAmt), "net_side": side,
		},
	}
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// money formats a value with two decimals and Indian digit grouping
// (12,34,567.89) for the Typst template.
func money(v float64) string {
	s := fmt.Sprintf("%.2f", round2(v))
	intPart, frac := s[:len(s)-3], s[len(s)-3:]
	if len(intPart) > 3 {
		head, tail := intPart[:len(intPart)-3], intPart[len(intPart)-3:]
		var groups []string
		for len(head) > 2 {
			groups = append([]string{head[len(head)-2:]}, groups...)
			head = head[:len(head)-2]
		}
		if head != "" {
			groups = append([]string{head}, groups...)
		}
		intPart = ""
		for _, g := range groups {
			intPart += g + ","
		}
		intPart += tail
	}
	return intPart + frac
}
