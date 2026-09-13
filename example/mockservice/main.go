package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// A small mock service used to demo sievegate.
//
// The same binary is deployed twice:
//   - MOCK_ROLE=original  -> the service that currently serves traffic
//   - MOCK_ROLE=migrated  -> the candidate service with minor response diffs
//
// Differences on the migrated side (intentional, for the demo):
//   /api/users/{id}:  name value differs, email renamed to contact_email,
//                     extra "preferences" field.
//   /api/orders:      each item gains a "currency" field, X-Api-Version
//                     header differs, response is slower.
//   /api/products/{id}: extra "warranty" field.
//   /api/users/missing: both services return 404 (should still match).
//   /api/health:      identical, and excluded from mirroring in config.
func main() {
	role := os.Getenv("MOCK_ROLE")
	if role == "" {
		role = "original"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	apiVersion := "v1"

	mux.HandleFunc("GET /api/users/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/api/users/"):]
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		if id == "missing" {
			http.NotFound(w, r)
			return
		}
		sleep(3, 8, role)

		resp := map[string]any{
			"name":  "Jane Doe",
			"email": "jane@example.com",
			"server_time": time.Now().UnixMilli(),
			"address": map[string]any{"city": "Bengaluru", "zip": "560001"},
		}
		if role == "migrated" {
			resp["name"] = "Janet Doe"
			resp["contact_email"] = resp["email"]
			delete(resp, "email")
			resp["preferences"] = map[string]any{"theme": "dark"}
		}
		setHeaders(w, "application/json", apiVersion)
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /api/orders", func(w http.ResponseWriter, r *http.Request) {
		sleep(5, 25, role)

		order := map[string]any{"id": "o1", "total": 100, "items": 2}
		if role == "migrated" {
			order["currency"] = "USD"
		}
		resp := map[string]any{
			"orders": []any{order},
			"count":  1,
		}
		if role == "migrated" {
			setHeaders(w, "application/json", "v2")
		} else {
			setHeaders(w, "application/json", apiVersion)
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /api/products/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/api/products/"):]
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		sleep(2, 4, role)

		product := map[string]any{
			"id": id, "name": "Widget", "price": 9.99, "in_stock": true,
		}
		if role == "migrated" {
			product["warranty"] = "1y"
		}
		setHeaders(w, "application/json", apiVersion)
		writeJSON(w, http.StatusOK, map[string]any{"product": product})
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w, "application/json", apiVersion)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	addr := "0.0.0.0:" + port
	log.Printf("mock service role=%s listening on %s", role, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func setHeaders(w http.ResponseWriter, contentType, apiVersion string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Api-Version", apiVersion)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func sleep(origMS, migMS int, role string) {
	ms := origMS
	if role == "migrated" {
		ms = migMS
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}