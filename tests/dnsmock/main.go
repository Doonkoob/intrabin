// Command dnsmock serves the in-memory Cloudflare/ArvanCloud fake used by the UI tests and the
// demo environment. It is a test tool: it never contacts real providers.
//
//	DNSMOCK_LISTEN=127.0.0.1:18096 DNSMOCK_CF_TOKEN=... DNSMOCK_ARVAN_KEY=... go run ./tests/dnsmock
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/intrabin/intrabin/internal/dns/dnsmock"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	addr := env("DNSMOCK_LISTEN", "127.0.0.1:18096")
	m := dnsmock.New(env("DNSMOCK_CF_TOKEN", "mock-cloudflare-token"), env("DNSMOCK_ARVAN_KEY", "mock-arvan-key"))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/", m)
	log.Printf("dnsmock listening on %s (Cloudflare: /cloudflare/client/v4, ArvanCloud: /arvan/cdn/4.0)", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
