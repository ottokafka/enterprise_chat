package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/joho/godotenv"
)

func main() {
	// First check system prerequisites
	CheckPrerequisites()

	// 5. require('dotenv').config({ path: "./.env" });
	if err := godotenv.Load("./.env"); err != nil {
		log.Println("No .env file found or error loading it")
	}

	// Initialize global dependencies
	InitClickhouse()
	InitSessionStore()
	InitMSAL()

	// Start email cron jobs (weekly + monthly usage reports)
	InitEmailCronJobs()

	port := os.Getenv("PORT")
	if port == "" {
		port = "4445" // default fallback
	}

	host := os.Getenv("HOST")
	if host == "" {
		host = "localhost" // default fallback
	}

	// Retrieve mux with route mappings
	mux := InitRoutes()

	// Apply global middleware (logger, equivalent to app.use)
	handler := GlobalMiddleware(mux)

	addr := fmt.Sprintf("%s:%s", host, port)

	fmt.Printf("\n------------------------------------------------------------\n")
	fmt.Printf("AliceGPT api started\n")
	fmt.Printf("listening on: http://%s\n", addr)
	fmt.Printf("------------------------------------------------------------\n")

	// Spin up server
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
