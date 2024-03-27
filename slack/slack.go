package slack

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Code-Hex/synchro"
	"github.com/Code-Hex/synchro/tz"
	"github.com/kmc-jp/inviteallmcg/config"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"golang.org/x/exp/maps"
)

type Client struct {
	slackClient  *slack.Client
	socketClient *socketmode.Client

	cacheDuration time.Duration

	prefixedChannelCache          []string
	prefixedChannelCacheExpiresAt synchro.Time[tz.AsiaTokyo]

	mcgMemberCache          map[string]struct{}
	mcgMemberCacheExpiresAt synchro.Time[tz.AsiaTokyo]

	determineYearRegex         *regexp.Regexp
	determineYearCacheDuration time.Duration
	determineYearCache         ObservTarget
	determineYearExpiresAt     synchro.Time[tz.AsiaTokyo]
}

type SlackLogger struct {
	logger *slog.Logger
}

func (s SlackLogger) Output(calldepth int, str string) error {
	s.logger.Debug(str)
	return nil
}

func NewSlackClient(cfg config.Config) Client {
	useDebug := cfg.LogLevel == slog.LevelDebug
	logger := slog.Default()

	slackClient := slack.New(cfg.SlackUserToken, slack.OptionDebug(useDebug), slack.OptionLog(SlackLogger{logger}), slack.OptionAppLevelToken(cfg.SlackAppToken))

	socketClient := socketmode.New(
		slackClient,
		socketmode.OptionDebug(useDebug),
		socketmode.OptionLog(SlackLogger{logger}),
	)

	return Client{
		slackClient:                slackClient,
		socketClient:               socketClient,
		cacheDuration:              cfg.SlackCacheDuration,
		determineYearRegex:         regexp.MustCompile(cfg.MCGJoinChannelRegex),
		determineYearCacheDuration: cfg.SlackDetermineYearCacheDuration,
	}
}

func (c *Client) InviteUsersToChannels(ctx context.Context, channelIDs []string, userIDs []string) error {
	for _, channelID := range channelIDs {
		//joinしないと招待できない
		_, warn, _, err := c.slackClient.JoinConversationContext(ctx, channelID)
		if err != nil {
			slog.Error("Error joining channel", "channelID", channelID, "error", err)
			continue
		}

		if warn != "" {
			slog.Warn("Warning joining channel", "channelID", channelID, "warning", warn)
		}

		if _, err := c.slackClient.InviteUsersToConversationContext(ctx, channelID, userIDs...); err != nil {
			slog.Error("Error inviting user to channel", "channelID", channelID, "userIDs", userIDs, "error", err)
			continue
		}
	}

	return nil
}

func (c *Client) GetPrefixedChannels(ctx context.Context, prefix string) ([]string, error) {
	now := synchro.Now[tz.AsiaTokyo]()
	if c.prefixedChannelCache != nil && now.Before(c.prefixedChannelCacheExpiresAt) {
		return c.prefixedChannelCache, nil
	}

	channels, err := c.GetPublicChannels(ctx)
	if err != nil {
		return nil, err
	}

	prefixedChannels := make([]string, 0, 20)
	for _, channel := range channels {
		if strings.HasPrefix(channel.Name, prefix) {
			prefixedChannels = append(prefixedChannels, channel.ID)
		}
	}

	c.prefixedChannelCache = prefixedChannels
	c.prefixedChannelCacheExpiresAt = now.Add(c.cacheDuration)

	return prefixedChannels, nil
}

func (c *Client) GetPublicChannels(ctx context.Context) ([]slack.Channel, error) {
	channels := make([]slack.Channel, 0)
	cursor := "dGVhbTpDMDRRV0ZGTkREMQ==" // 2023-general

	for {
		cs, nextCursor, err := c.slackClient.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types:           []string{"public_channel"},
			ExcludeArchived: true,
			Cursor:          cursor,
		})
		if err != nil {
			return nil, err
		}
		channels = append(channels, cs...)

		if nextCursor == "" {
			slog.Debug("No more cursor")
			break
		}

		cursor = nextCursor
		slog.Debug("Get public channels", "len", len(cs), "nextCursor", cursor)
	}
	slog.Debug("Get public channels", "len", len(channels))

	return channels, nil
}

func (c *Client) GetAllMCGMembers(ctx context.Context, mustIncludeUser string) (map[string]struct{}, error) {
	now := synchro.Now[tz.AsiaTokyo]()
	_, include := c.mcgMemberCache[mustIncludeUser]
	if c.mcgMemberCache != nil && now.Before(c.mcgMemberCacheExpiresAt) && include {
		return c.mcgMemberCache, nil
	}

	users, err := c.slackClient.GetUsersContext(ctx)
	if err != nil {
		return nil, err
	}

	mcgMembers := make(map[string]struct{}, 100)
	for _, user := range users {
		if user.IsRestricted {
			mcgMembers[user.ID] = struct{}{}
		}
	}

	c.mcgMemberCache = mcgMembers
	c.mcgMemberCacheExpiresAt = now.Add(c.cacheDuration)

	return mcgMembers, nil
}

// 2024-generalにユーザーが追加されるイベントを監視する
func (c *Client) HandleChannelJoinEvent(ctx context.Context) {
	slog.Info("Start handling channel join event")

	for event := range c.socketClient.Events {
		slog.Debug("Event", "event", event)
		switch event.Type {
		case socketmode.EventTypeConnecting:
			slog.Info("Connecting to Slack with Socket Mode...")
		case socketmode.EventTypeConnectionError:
			slog.Error("Connection error", "error", event.Data)
		case socketmode.EventTypeConnected:
			slog.Info("Connected to Slack with Socket Mode")
		case socketmode.EventTypeEventsAPI:
			eventsAPIEvent, ok := event.Data.(slackevents.EventsAPIEvent)
			if !ok {
				slog.Debug("Ignored event", "event", event)
				continue
			}
			c.socketClient.Ack(*event.Request)

			slog.Debug("EventsAPIEvent", "eventsAPIEvent", eventsAPIEvent)
			if eventsAPIEvent.Type == slackevents.CallbackEvent {
				innerEvent := eventsAPIEvent.InnerEvent
				slog.Debug("InnerEvent", "innerEvent", innerEvent)
				if ev, ok := innerEvent.Data.(*slackevents.MemberJoinedChannelEvent); ok {
					observTarget, err := c.DetermineGeneralChannel(ctx)
					if err != nil {
						slog.Error("Error determining MCG channel", "error", err)
						continue
					}

					if ev.Channel == observTarget.generalChannelID {
						slog.Debug("MemberJoinedChannelEvent", "event", ev)

						mcgMembers, err := c.GetAllMCGMembers(ctx, ev.User)
						if err != nil {
							slog.Error("Error getting MCG members", "error", err)
							continue
						}

						if _, ok := mcgMembers[ev.User]; !ok {
							slog.Info("Join event from non-MCG member, skipping", "user", ev.User)
							continue
						}

						channelIDs, err := c.GetPrefixedChannels(ctx, fmt.Sprintf("%s-", observTarget.year))
						if err != nil {
							slog.Error("Error getting prefixed channels", "error", err)
							continue
						}

						err = c.InviteUsersToChannels(ctx, channelIDs, maps.Keys(mcgMembers))
						if err != nil {
							slog.Error("Error inviting user to channels", "error", err)
							continue
						}
					} else {
						slog.Debug(fmt.Sprintf("Join event from %s, not target channel %s", ev.Channel, observTarget.generalChannelID))
					}
				}
			}
		}
	}

	slog.Info("End handling channel join event")
}

func (c *Client) Listen(ctx context.Context) error {
	return c.socketClient.Run()
}

type ObservTarget struct {
	year             string
	generalChannelID string
}

func (c *Client) DetermineGeneralChannel(ctx context.Context) (ObservTarget, error) {
	now := synchro.Now[tz.AsiaTokyo]()

	if c.determineYearCache != (ObservTarget{}) && now.Before(c.determineYearExpiresAt) {
		return c.determineYearCache, nil
	}

	year := now.Year()

	channels, err := c.GetPublicChannels(ctx)
	if err != nil {
		return ObservTarget{}, err
	}

	generalChannels := make(map[string]string, (year - 2014))
	for _, channel := range channels {
		slog.Debug("Channel", "name", channel.Name, "id", channel.ID)
		matches := c.determineYearRegex.FindStringSubmatch(channel.Name)
		if len(matches) == 0 {
			continue
		}
		generalChannels[matches[1]] = channel.ID
		slog.Debug("Found general channel", "year", matches[1], "channelID", channel.ID)
	}

	if len(generalChannels) == 0 {
		return ObservTarget{}, fmt.Errorf("general channels not found")
	}
	slog.Debug("Found general channels", "ids", generalChannels, "len", len(generalChannels))

	var target ObservTarget

	if id, exist := generalChannels[fmt.Sprintf("%d", year+1)]; exist {
		target = ObservTarget{
			year:             fmt.Sprintf("%d", year+1),
			generalChannelID: id,
		}
	} else if id, exist := generalChannels[fmt.Sprintf("%d", year)]; exist {
		target = ObservTarget{
			year:             fmt.Sprintf("%d", year),
			generalChannelID: id,
		}
	} else {
		keys := make([]string, 0, len(generalChannels))
		for key := range generalChannels {
			keys = append(keys, key)
		}

		slices.Sort(keys)
		latest := keys[len(keys)-1]
		target = ObservTarget{
			year:             latest,
			generalChannelID: generalChannels[latest],
		}
	}

	slog.Debug("Determined general channel", "target", target)

	c.determineYearCache = target
	c.determineYearExpiresAt = now.Add(c.determineYearCacheDuration)

	return target, nil
}