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

	prefixedChannelCache          map[string]map[string]string
	prefixedChannelCacheExpiresAt map[string]synchro.Time[tz.AsiaTokyo]

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
		slackClient:   slackClient,
		socketClient:  socketClient,
		cacheDuration: cfg.SlackCacheDuration,

		prefixedChannelCache:          make(map[string]map[string]string, 2),
		prefixedChannelCacheExpiresAt: make(map[string]synchro.Time[tz.AsiaTokyo], 2),

		mcgMemberCache: make(map[string]struct{}, 100),

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

		if warn != "" && warn != "already_in_channel" {
			slog.Warn("Warning joining channel", "channelID", channelID, "warning", warn)
		}

		if _, err := c.slackClient.InviteUsersToConversationContext(ctx, channelID, userIDs...); err != nil && err.Error() != "already_in_channel" {
			slog.Error("Error inviting user to channel", "channelID", channelID, "userIDs", userIDs, "error", err)
			continue
		}
	}

	return nil
}

func (c *Client) GetPrefixedChannels(ctx context.Context, prefix string, mustIncludeChannelIDs ...string) (map[string]string, error) {
	now := synchro.Now[tz.AsiaTokyo]()
	if cache, ok := c.prefixedChannelCache[prefix]; ok && now.Before(c.prefixedChannelCacheExpiresAt[prefix]) {
		useCache := true
		for _, mustIncludeChannelID := range mustIncludeChannelIDs {
			if _, ok := cache[mustIncludeChannelID]; !ok {
				useCache = false
				break
			}
		}

		if useCache {
			return cache, nil
		}
	}

	channels, err := c.GetPublicChannels(ctx)
	if err != nil {
		return nil, err
	}

	prefixedChannels := make(map[string]string, 20)
	for _, channel := range channels {
		if strings.HasPrefix(channel.Name, prefix) {
			prefixedChannels[channel.ID] = channel.Name
		}
	}

	c.prefixedChannelCache[prefix] = prefixedChannels
	c.prefixedChannelCacheExpiresAt[prefix] = now.Add(c.cacheDuration)

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

func (c *Client) GetAllMCGMembers(ctx context.Context, mustIncludeUsers ...string) (map[string]struct{}, error) {
	now := synchro.Now[tz.AsiaTokyo]()

	var include bool
	for _, mustIncludeUser := range mustIncludeUsers {
		_, ok := c.mcgMemberCache[mustIncludeUser]
		include = include || ok
	}

	if c.mcgMemberCache != nil && now.Before(c.mcgMemberCacheExpiresAt) && include {
		return c.mcgMemberCache, nil
	}

	users, err := c.slackClient.GetUsersContext(ctx)
	if err != nil {
		return nil, err
	}

	mcgMembers := make(map[string]struct{}, 100)
	for _, user := range users {
		// IsRestricted: All Guest accounts, IsUltraRestricted: Single-channel guests
		if user.IsRestricted && !user.IsUltraRestricted {
			mcgMembers[user.ID] = struct{}{}
		}
	}

	c.mcgMemberCache = mcgMembers
	c.mcgMemberCacheExpiresAt = now.Add(c.cacheDuration)

	return mcgMembers, nil
}

// 投稿を転送する
func (c *Client) ForwardMessage(ctx context.Context, everythingChannelID string, sourceChannelName string, message slackevents.MessageEvent) error {
	slog.Debug("Forwarding message", "everythingChannelID", everythingChannelID, "sourceChannelName", sourceChannelName, "message", message)

	if message.SubType != "" && message.SubType != "message_deleted" && message.SubType != "message_changed" {
		slog.Debug("Ignored message", "message", message)
		return nil
	}

	// Deleted, Updatedの場合は無視する
	// TODO: 実装する
	if message.SubType == "message_deleted" || message.SubType == "message_changed" {
		return nil
	}

	profile, err := c.slackClient.GetUserProfileContext(ctx, &slack.GetUserProfileParameters{
		UserID:        message.User,
		IncludeLabels: false,
	})
	if err != nil {
		slog.Error("Error getting user profile", "error", err)
	}

	var iconURL string
	if profile == nil {
		iconURL = ""
	} else {
		iconURL = profile.Image512
	}

	permalink, err := c.slackClient.GetPermalinkContext(ctx, &slack.PermalinkParameters{
		Channel: message.Channel,
		Ts:      message.TimeStamp,
	})

	if err != nil {
		slog.Error("Error getting permalink", "error", err)
	}

	blocks := []slack.Block{
		&slack.SectionBlock{
			Type: slack.MBTSection,
			Text: &slack.TextBlockObject{
				Type: slack.MarkdownType,
				Text: fmt.Sprintf("<%s|%s> %s", permalink, sourceChannelName, message.Text),
			},
		},
	}
	if len(message.Files) > 0 {
		for _, file := range message.Files {
			blocks = append(blocks, &slack.ImageBlock{
				Type: slack.MBTImage,
				Title: &slack.TextBlockObject{
					Type: slack.PlainTextType,
					Text: file.Title,
				},
				ImageURL: file.URLPrivate,
				AltText:  file.Title,
			})
		}
	}

	_, _, err = c.slackClient.PostMessageContext(
		ctx,
		everythingChannelID,
		slack.MsgOptionBlocks(
			&slack.SectionBlock{
				Type: slack.MBTSection,
				Text: &slack.TextBlockObject{
					Type: slack.MarkdownType,
					Text: fmt.Sprintf("<%s|`%s`> %s", permalink, sourceChannelName, message.Text),
				},
			},
		),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionIconURL(iconURL),
	)
	return err
}

func (c *Client) HandleSlackEvents(ctx context.Context) {
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

				switch ev := innerEvent.Data.(type) {
				case *slackevents.MessageEvent:
					slog.Info("MessageEvent", "event", ev, "channel", ev.Channel)

					observTarget, err := c.DetermineObservTarget(ctx)
					if err != nil {
						slog.Error("Error determining MCG channel", "error", err)
						continue
					}

					shinkanChannels, err := c.GetPrefixedChannels(ctx, fmt.Sprintf("%s-", observTarget.year))
					if err != nil {
						slog.Error("Error getting prefixed channels", "error", err)
						continue
					}

					shinkanChannelIDs := maps.Keys(shinkanChannels)
					if !slices.Contains(shinkanChannelIDs, ev.Channel) {
						slog.Info(fmt.Sprintf("Message event from %s, not target channel %s", ev.Channel, shinkanChannelIDs))
						continue
					}

					var everythingChannelID string
					for id, name := range shinkanChannels {
						if name == fmt.Sprintf("%s-everything", observTarget.year) {
							everythingChannelID = id
							break
						}
					}

					if everythingChannelID == "" {
						slog.Error("Everything channel not found", "year", observTarget.year)
						continue
					}

					if ev.Channel == everythingChannelID {
						slog.Info("Ignored message event from everything channel", "channel", ev.Channel)
						continue
					}

					sourceChanName, ok := shinkanChannels[ev.Channel]
					if !ok {
						slog.Error("Source channel not found", "channel", ev.Channel)
						continue
					}

					err = c.ForwardMessage(ctx, everythingChannelID, sourceChanName, *ev)
					if err != nil {
						slog.Error("Error forwarding message", "error", err)
						continue
					}
				case *slackevents.MemberJoinedChannelEvent:
					slog.Info("MemberJoinedChannelEvent", "event", ev, "channel", ev.Channel)

					observTarget, err := c.DetermineObservTarget(ctx)
					if err != nil {
						slog.Error("Error determining MCG channel", "error", err)
						continue
					}

					if ev.Channel != observTarget.generalChannelID {
						slog.Info(fmt.Sprintf("Join event from %s, not target channel %s", ev.Channel, observTarget.generalChannelID))
						continue
					}

					mcgMembers, err := c.GetAllMCGMembers(ctx, ev.User)
					if err != nil {
						slog.Error("Error getting MCG members", "error", err)
						continue
					}

					if _, ok := mcgMembers[ev.User]; !ok {
						slog.Info("Join event from non-MCG member, skipping", "user", ev.User)
						continue
					}

					channels, err := c.GetPrefixedChannels(ctx, fmt.Sprintf("%s-", observTarget.year))
					if err != nil {
						slog.Error("Error getting prefixed channels", "error", err)
						continue
					}
					slog.Info("Inviting user to channels", "trigerUser", ev.User, "mcgMembers", mcgMembers, "channels", channels)

					err = c.InviteUsersToChannels(ctx, maps.Keys(channels), maps.Keys(mcgMembers))
					if err != nil {
						slog.Error("Error inviting user to channels", "error", err)
						continue
					}

				case *slackevents.ChannelCreatedEvent:
					slog.Info("ChannelCreatedEvent", "event", ev)

					observTarget, err := c.DetermineObservTarget(ctx)
					if err != nil {
						slog.Error("Error determining MCG channel", "error", err)
						continue
					}

					if !strings.HasPrefix(ev.Channel.Name, observTarget.year) {
						slog.Info("Ignored channel created event", "channelName", ev.Channel.Name)
					}

					mcgMembers, err := c.GetAllMCGMembers(ctx, "")
					if err != nil {
						slog.Error("Error getting MCG members", "error", err)
						continue
					}

					channels, err := c.GetPrefixedChannels(ctx, fmt.Sprintf("%s-", observTarget.year))
					if err != nil {
						slog.Error("Error getting prefixed channels", "error", err)
						continue
					}

					slog.Info("Inviting MCG members to channel", "mcgMembers", mcgMembers, "channels", channels)

					err = c.InviteUsersToChannels(ctx, maps.Keys(channels), maps.Keys(mcgMembers))
					if err != nil {
						slog.Error("Error inviting MCG members to channel", "error", err)
						continue
					}
				default:
					slog.Debug("Ignored event", "event", innerEvent)
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

func (c *Client) DetermineObservTarget(ctx context.Context) (ObservTarget, error) {
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
