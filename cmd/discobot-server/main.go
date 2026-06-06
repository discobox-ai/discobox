package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/obot-platform/disco2/internal/app"
	"github.com/obot-platform/disco2/internal/database"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx := context.Background()
	dsn := os.Getenv("DATABASE_DSN")
	if dsn == "" {
		dsn = "sqlite3://disco2.db"
	}

	db, err := database.New(database.Config{DSN: dsn})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "migrate database: %v\n", err)
		os.Exit(1)
	}

	router, _, err := app.NewDatabaseRouter(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize app: %v\n", err)
		os.Exit(1)
	}

	addr := ":" + port
	log.Printf("listening on http://localhost%s", addr)
	log.Printf("openapi spec available at http://localhost%s/openapi.json", addr)
	if err := http.ListenAndServe(addr, router); err != nil {
		fmt.Fprintf(os.Stderr, "server failed: %v\n", err)
		os.Exit(1)
	}
}
