package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func createServer(port string) *http.Server {
	mux := http.NewServeMux()

	// Endpoint principal
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // Simula processamento
		fmt.Fprintf(w, "Resposta do servidor na porta %s\n", port)
	})

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "healthy")
	})

	return &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Por favor, especifique a porta como argumento")
	}

	port := os.Args[1]
	server := createServer(port)

	log.Printf("Servidor iniciado na porta %s", port)
	log.Fatal(server.ListenAndServe())
}
