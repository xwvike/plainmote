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

	// Housekeeping runs on the same context as the server, so shutdown stops
	// it too. It is in-process on purpose: the deployment is one compose file
	// and one set of environment variables, and a host crontab or a second
	// container would put a piece of it somewhere neither of those describes.
	go prune(ctx, db, cfg.LogRetention)

	handler := web.New(web.Config{
		PublicURL:        cfg.PublicURL,
		SessionTTL:       cfg.SessionTTL,
		MaxContent:       cfg.MaxContent,
		AllowedIDs:       cfg.AllowedIDs,
		TrustedProxies:   cfg.TrustedProxies,
		RegistrationMode: cfg.RegistrationMode,
	}, db, upstream.New(cfg.AllowPrivateUpstream, cfg.MaxContent), auth.NewGitHub(cfg.GitHubID, cfg.GitHubSecret)).Handler()

	fmt.Fprintf(os.Stderr, "listening on %s\n", cfg.Listen)
	return serve(ctx, cfg.Listen, handler)
}

// pruneInterval is how often records are checked for having aged out. The
// window is measured in days, so the exact cadence does not matter; an hour is
// often enough that a backlog never builds, and rare enough to be invisible.
const pruneInterval = time.Hour

func prune(ctx context.Context, db *store.Store, retention time.Duration) {
	pass := func() {
		started := time.Now()
		result, err := db.Prune(ctx, retention, time.Now().UTC())
		if err != nil {
			// Housekeeping failing is not a reason to take the service down:
			// it serves resources perfectly well with a table that is larger
			// than it should be.
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "prune: %v\n", err)
			}
			return
		}
		if result.AccessLogs > 0 || result.Sessions > 0 {
			fmt.Fprintf(os.Stderr, "prune: removed %d access logs and %d expired sessions in %s\n",
				result.AccessLogs, result.Sessions, time.Since(started).Round(time.Millisecond))
		}
	}

	// Once at startup, so a deployment that has been down - or has just had
	// retention turned on - does not wait an hour to catch up.
	pass()

	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass()
		}
	}
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
