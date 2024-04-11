package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/kmc-jp/inviteallmcg/config"
	"github.com/kmc-jp/inviteallmcg/slack"
	"github.com/lmittmann/tint"
	"golang.org/x/sync/errgroup"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("Error loading config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(tint.NewHandler(os.Stdout, &tint.Options{
		TimeFormat: time.RFC3339,
		Level:      cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	ctx := context.Background()

	slackClient := slack.NewSlackClient(cfg)

	eg := new(errgroup.Group)

	eg.Go(func() error {
		err := slackClient.HandleSlackEvents(ctx)
		if err != nil {
			return fmt.Errorf("error handling slack events: %w", err)
		}
		return nil
	})

	eg.Go(func() error {
		if err := slackClient.Listen(ctx); err != nil {
			return fmt.Errorf("error listening to slack: %w", err)
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		slog.Error("Error running", "error", err)
		os.Exit(1)
	}
}
