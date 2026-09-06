package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"plainmote/internal/auth"
	"plainmote/internal/blob"
	"plainmote/internal/config"
	"plainmote/internal/store"
	"plainmote/internal/upstream"
	"plainmote/internal/web"
)

// Run starts the web service from environment configuration and blocks until
// it receives SIGINT or SIGTERM.
func Run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	blobs, err := blob.NewS3(blob.S3Config{
		Endpoint:  cfg.BlobEndpoint,
		Bucket:    cfg.BlobBucket,
		AccessKey: cfg.BlobAccessKey,
		SecretKey: cfg.BlobSecretKey,
		Region:    cfg.BlobRegion,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.TokenKey, blobs, cfg.AllowPrivateUpstream)
	if err != nil {
		return err
	}
	defer db.Close()

	handler := web.New(web.Config{
		PublicURL:      cfg.PublicURL,
		SessionTTL:     cfg.SessionTTL,
		MaxContent:     cfg.MaxContent,
		AllowedIDs:     cfg.AllowedIDs,
		TrustedProxies: cfg.TrustedProxies,
	}, db, upstream.New(cfg.AllowPrivateUpstream, cfg.MaxContent), auth.NewGitHub(cfg.GitHubID, cfg.GitHubSecret)).Handler()

	fmt.Fprintf(os.Stderr, "listening on %s\n", cfg.Listen)
	return serve(ctx, cfg.Listen, handler)
}

func serve(ctx context.Context, address string, handler http.Handler) error {
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-serverErr:
		return fmt.Errorf("serve HTTP: %w", err)
	}
}
