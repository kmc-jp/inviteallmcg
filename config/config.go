package config

import (
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v10"
	_ "github.com/joho/godotenv/autoload"
)

type Config struct {
	SlackUserToken                  string        `env:"SLACK_USER_TOKEN,required"`
	SlackAppToken                   string        `env:"SLACK_APP_TOKEN,required"`
	SlackCacheDuration              time.Duration `env:"SLACK_CACHE_DURATION" envDefault:"5m"`
	SlackDetermineYearCacheDuration time.Duration `env:"SLACK_DETERMINE_YEAR_CACHE_DURATION" envDefault:"72h"`

	// MCG Joinを監視するチャンネル
	MCGJoinChannelRegex string `env:"MCG_JOIN_CHANNEL_REGEX" envDefault:"^([0-9]{4})-general$"`

	LogLevelString string `env:"LOG_LEVEL" envDefault:"info"`
	LogLevel       slog.Level
}

func Load() (Config, error) {
	cfg := Config{}
	if err := env.Parse(&cfg); err != nil {
		return cfg, err
	}

	// parse log level
	switch strings.ToLower(cfg.LogLevelString) {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "info":
		cfg.LogLevel = slog.LevelInfo
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		slog.Warn("Invalid log level, use default level: info")
		cfg.LogLevel = slog.LevelInfo
	}

	return cfg, nil
}
