package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Transaction struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Date        string `json:"date"`
}

type Balance struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Net         int    `json:"net"`
}

type LedgerResponse struct {
	Transactions []Transaction `json:"transactions"`
	Balances     []Balance     `json:"balances"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

var db *pgxpool.Pool

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func computeBalances(txns []Transaction) []Balance {
	type pairKey struct{ a, b string }
	nets := map[pairKey]int{}

	for _, t := range txns {
		a, b := t.Source, t.Destination
		if a > b {
			a, b = b, a
			nets[pairKey{a, b}]--
		} else {
			nets[pairKey{a, b}]++
		}
	}

	balances := []Balance{}
	for k, net := range nets {
		if net == 0 {
			continue
		}
		if net > 0 {
			balances = append(balances, Balance{Source: k.a, Destination: k.b, Net: net})
		} else {
			balances = append(balances, Balance{Source: k.b, Destination: k.a, Net: -net})
		}
	}

	sort.Slice(balances, func(i, j int) bool {
		return balances[i].Net > balances[j].Net
	})

	return balances
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleData(w http.ResponseWriter, r *http.Request) {
	source      := r.URL.Query().Get("source")
	destination := r.URL.Query().Get("destination")

	rows, err := db.Query(r.Context(), `
		SELECT id, source, destination, date::text
		FROM transactions
		WHERE ($1 = '' OR source = $1)
		  AND ($2 = '' OR destination = $2)
		ORDER BY date
	`, source, destination)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{err.Error()})
		return
	}
	defer rows.Close()

	transactions := []Transaction{}
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.Source, &t.Destination, &t.Date); err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{err.Error()})
			return
		}
		transactions = append(transactions, t)
	}

	writeJSON(w, http.StatusOK, LedgerResponse{
		Transactions: transactions,
		Balances:     computeBalances(transactions),
	})
}

func handlePostTransaction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
		Date        string `json:"date"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{"invalid JSON"})
		return
	}

	switch {
	case body.Source == "" || body.Destination == "" || body.Date == "":
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{"source, destination, and date are required"})
		return
	case body.Source == body.Destination:
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{"source and destination must be different"})
		return
	}

	t := Transaction{
		ID:          newID(),
		Source:      body.Source,
		Destination: body.Destination,
		Date:        body.Date,
	}

	_, err := db.Exec(r.Context(),
		`INSERT INTO transactions (id, source, destination, date) VALUES ($1, $2, $3, $4)`,
		t.ID, t.Source, t.Destination, t.Date,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, t)
}

func main() {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot connect to database: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	db = pool

	if _, err = db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS transactions (
			id          TEXT PRIMARY KEY,
			source      TEXT NOT NULL,
			destination TEXT NOT NULL,
			date        DATE NOT NULL
		)
	`); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create table: %v\n", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /data", handleData)
	mux.HandleFunc("POST /transactions", handlePostTransaction)
	mux.HandleFunc("OPTIONS /data", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("OPTIONS /transactions", func(w http.ResponseWriter, r *http.Request) {})

	fmt.Println("listening on :8080")
	if err := http.ListenAndServe(":8080", withCORS(mux)); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
