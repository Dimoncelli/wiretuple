// Command wiretuple runs the tuple-compressing reverse proxy.
//
// Point an SDK at it and the rest is transparent:
//
//	OPENAI_BASE_URL=http://127.0.0.1:8787/v1
//	ANTHROPIC_BASE_URL=http://127.0.0.1:8787
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dimoncelli/wiretuple/internal/proxy"
	"github.com/Dimoncelli/wiretuple/internal/shadow"
)

func main() {
	upstream := flag.String("upstream", "", "upstream base URL, for example https://api.openai.com")
	listen := flag.String("listen", "127.0.0.1:8787", "listen address")
	breakEven := flag.Float64("break-even", 5.0, "minimum net savings percent to activate")
	legendTokens := flag.Int64("legend-tokens", 0, "fixed legend token cost (0 = estimate from the legend)")
	activate := flag.Bool("activate", true, "master switch for schema rewriting")
	shadowPath := flag.String("shadow", "", "enable passive audit and write the SQLite file at this path")
	maxBody := flag.Int64("max-body", proxy.DefaultMaxBodyBytes, "maximum request body in bytes")
	flag.Parse()

	if *upstream == "" {
		log.Fatal("wiretuple: -upstream is required")
	}

	logger := log.New(os.Stdout, "wiretuple ", log.LstdFlags|log.LUTC)

	cfg := proxy.Config{
		Upstream:     *upstream,
		BreakEvenPct: *breakEven,
		LegendTokens: *legendTokens,
		Activate:     *activate,
		MaxBodyBytes: *maxBody,
		Logger:       logger,
	}

	var recorder *shadow.Recorder
	if *shadowPath != "" {
		var err error
		recorder, err = shadow.NewRecorderWithLogger(*shadowPath, logger)
		if err != nil {
			log.Fatalf("wiretuple: cannot open shadow database: %v", err)
		}
		cfg.Shadow = recorder
	}

	handler, err := proxy.New(cfg)
	if err != nil {
		log.Fatalf("wiretuple: %v", err)
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		logger.Print("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Printf("shutdown: %v", err)
		}
	}()

	logger.Printf("listening on %s → %s (break-even %.1f%%, activate=%v)",
		*listen, *upstream, *breakEven, *activate)

	err = server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("wiretuple: %v", err)
	}

	if recorder != nil {
		if err := recorder.Close(); err != nil {
			logger.Printf("shadow close: %v", err)
		}
	}
}
