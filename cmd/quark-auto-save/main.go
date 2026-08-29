package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xinghaix/quark-auto-save/internal/qas"
)

var (
	BuildSHA string
	BuildTag string
)

func main() {
	stdio := flag.Bool("mcp-stdio", false, "run the MCP stdio transport")
	flag.Parse()

	app, err := qas.NewApp(qas.Options{
		BuildSHA: BuildSHA,
		BuildTag: BuildTag,
	})
	if err != nil {
		log.Fatal(err)
	}
	if *stdio {
		if err := app.RunStdio(os.Stdin, os.Stdout, os.Getenv("QAS_MCP_API_KEY")); err != nil {
			log.Fatal(err)
		}
		return
	}

	server := &http.Server{
		Addr:              app.Address(),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("quark-auto-save Go backend %s listening on %s", app.Version(), app.Address())
		serverErr <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server stopped: %v", err)
		}
	case <-stop:
		app.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
}
