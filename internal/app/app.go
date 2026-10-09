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

// Version and Revision name the build. The image build sets them with
// -ldflags; a build from source leaves them as they are here.
var (
	Version  = "dev"
	Revision = "unknown"
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

	for _, provider := range []struct{ name, id, secret string }{
		{"GITHUB", cfg.GitHubID, cfg.GitHubSecret},
		{"GOOGLE", cfg.GoogleID, cfg.GoogleSecret},
	} {
		if (provider.id == "") != (provider.secret == "") {
			fmt.Fprintf(os.Stderr, "%s_CLIENT_ID and %s_CLIENT_SECRET are not both set: that sign-in is off\n", provider.name, provider.name)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.TokenKey, blobs)
	if err != nil {
		return err
	}
	defer db.Close()

	go prune(ctx, db, cfg.LogRetention)

	handler := web.New(web.Config{
		PublicURL:          cfg.PublicURL,
		SessionTTL:         cfg.SessionTTL,
		MaxContent:         cfg.MaxContent,
		GoogleClientID:     cfg.GoogleID,
		GoogleClientSecret: cfg.GoogleSecret,
		TrustedProxies:     cfg.TrustedProxies,
		CloudflareLocation: cfg.VisitorLocation == config.LocationCloudflare,
		RegistrationMode:   cfg.RegistrationMode,
		AnonymousEnabled:   cfg.AnonymousEnabled,
		LogRetention:       cfg.LogRetention,
		Operator:           cfg.Operator,
		ContactEmail:       cfg.ContactEmail,
		SourceURL:          cfg.SourceURL,
		CLIDir:             cfg.CLIDir,
		BlobEndpoint:       cfg.BlobEndpoint,
		AdminKeys:          cfg.AdminKeys,
		AdminOrigins:       cfg.AdminOrigins,
		Version:            Version,
		Revision:           Revision,
		StartedAt:          time.Now().UTC(),
	}, db, upstream.New(cfg.MaxContent), auth.NewGitHub(cfg.GitHubID, cfg.GitHubSecret)).Handler()

	fmt.Fprintf(os.Stderr, "listening on %s\n", cfg.Listen)
	return serve(ctx, cfg.Listen, handler)
}

const pruneInterval = time.Hour

// quickShareSweep is how soon after its end a signed-in quick share is
// deleted. Its link stops working at the end itself; this is the content.
const quickShareSweep = time.Minute

// prune runs on the server's context, so shutdown stops it. A failed pass is
// logged and not retried.
func prune(ctx context.Context, db *store.Store, retention time.Duration) {
	pass := func() {
		started := time.Now()
		result, err := db.Prune(ctx, retention, time.Now().UTC())
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "prune: %v\n", err)
			}
			return
		}
		if result.AccessLogs > 0 || result.Sessions > 0 || result.Pastes > 0 || result.Versions > 0 {
			fmt.Fprintf(os.Stderr, "prune: removed %d access logs, %d expired sessions, %d anonymous pastes and %d old versions in %s\n",
				result.AccessLogs, result.Sessions, result.Pastes, result.Versions, time.Since(started).Round(time.Millisecond))
		}
	}

	// A quick share is gone when its time is up, not within the hour: its
	// own sweep runs every minute and touches only what has ended.
	sweep := func() {
		removed, err := db.PruneQuickShares(ctx, time.Now().UTC())
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "prune quick shares: %v\n", err)
			}
			return
		}
		if removed > 0 {
			fmt.Fprintf(os.Stderr, "prune: removed %d ended quick shares\n", removed)
		}
	}

	pass()
	sweep()
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	quick := time.NewTicker(quickShareSweep)
	defer quick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass()
		case <-quick.C:
			sweep()
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
