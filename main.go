package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/lmittmann/tint"
	"github.com/walnuts1018/inviteallmcg/config"
	"github.com/walnuts1018/inviteallmcg/slack"
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

	go func() {
		slackClient.HandleChannelJoinEvent(ctx)
	}()

	if err := slackClient.Listen(ctx); err != nil {
		slog.Error("Error listening", "error", err)
		os.Exit(1)
	}
}
