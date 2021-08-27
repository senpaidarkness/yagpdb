package logs

//go:generate sqlboiler --no-hooks psql

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"emperror.dev/errors"
	"github.com/jonas747/discordgo/v2"
	"github.com/jonas747/yagpdb/bot"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/logs/models"
	"github.com/jonas747/yagpdb/web"
	"github.com/volatiletech/null/v8"
	"github.com/volatiletech/sqlboiler/v4/boil"
	"github.com/volatiletech/sqlboiler/v4/queries/qm"
	"golang.org/x/net/context"
)

var (
	ErrChannelBlacklisted = errors.New("Channel blacklisted from creating message logs")

	logger = common.GetPluginLogger(&Plugin{})
)

type Plugin struct{}

func (p *Plugin) PluginInfo() *common.PluginInfo {
	return &common.PluginInfo{
		Name:     "Logging",
		SysName:  "logging",
		Category: common.PluginCategoryMisc,
	}
}

func RegisterPlugin() {
	common.InitSchemas("logs", DBSchemas...)

	p := &Plugin{}
	common.RegisterPlugin(p)
}

// Returns either stored config, err or a default config
func GetConfig(exec boil.ContextExecutor, ctx context.Context, guildID int64) (*models.GuildLoggingConfig, error) {

	config, err := models.FindGuildLoggingConfig(ctx, exec, guildID)
	if err == sql.ErrNoRows {
		// return default config
		return &models.GuildLoggingConfig{
			GuildID:                guildID,
			UsernameLoggingEnabled: null.BoolFrom(true),
			NicknameLoggingEnabled: null.BoolFrom(true),
		}, nil
	}

	return config, err
}

func CreateLink(guildID int64, id int) string {
	return fmt.Sprintf("%s/public/%d/log/%d", web.BaseURL(), guildID, id)
}

func CreateChannelLog(ctx context.Context, config *models.GuildLoggingConfig, guildID, channelID int64, author string, authorID int64, count int) (*models.MessageLogs2, error) {
	if config == nil {
		var err error
		config, err = GetConfig(common.PQ, ctx, guildID)
		if err != nil {
			return nil, err
		}
	}

	// note: since the blacklisted channels column is just a TEXT type with a comma seperator...
	// i was not a smart person back then
	strCID := strconv.FormatInt(channelID, 10)
	split := strings.Split(config.BlacklistedChannels.String, ",")
	if common.ContainsStringSlice(split, strCID) {
		return nil, ErrChannelBlacklisted
	}

	if count > 300 {
		count = 300
	}

	gs := bot.State.GetGuild(guildID)
	if gs == nil {
		return nil, bot.ErrGuildNotFound
	}

	// Make a light copy of the channel
	channel := gs.GetChannel(channelID)
	if channel == nil {
		return nil, errors.New("Unknown channel")
	}

	msgs, err := bot.GetMessages(guildID, channel.ID, count, true)
	if err != nil {
		return nil, err
	}

	logIds := make([]int64, 0, len(msgs))

	tx, err := common.PQ.Begin()
	if err != nil {
		return nil, errors.WrapIf(err, "pq.begin")
	}

	for _, v := range msgs {
		body := v.Content
		for _, attachment := range v.Attachments {
			body += fmt.Sprintf(" (Attachment: %s)", attachment.URL)
		}

		// serialise embeds to their underlying JSON
		for count, embed := range v.Embeds {
			marshalled, err := json.Marshal(embed)
			if err != nil {
				continue
			}

			body += fmt.Sprintf("\nEmbed %d: %s", count, marshalled)
		}

		// Strip out nul characters since postgres dont like them and discord dont filter them out (like they do in a lot of other places)
		body = strings.Replace(body, string(rune(0)), "", -1)

		messageModel := &models.Messages2{
			ID:      v.ID,
			GuildID: guildID,
			Content: body,

			CreatedAt: v.ParsedCreatedAt,
			UpdatedAt: v.ParsedCreatedAt,

			AuthorUsername: v.Author.Username + "#" + v.Author.Discriminator,
			AuthorID:       v.Author.ID,
			Deleted:        v.Deleted,
		}

		err = messageModel.Upsert(ctx, tx, true, []string{"id"}, boil.Blacklist("deleted"), boil.Infer())
		if err != nil {
			tx.Rollback()
			return nil, errors.WrapIf(err, "message.insert")
		}

		logIds = append(logIds, v.ID)
	}

	id, err := common.GenLocalIncrID(guildID, "message_logs")
	if err != nil {
		tx.Rollback()
		return nil, errors.WrapIf(err, "log.gen_id")
	}

	log := &models.MessageLogs2{
		GuildID:  guildID,
		ID:       int(id),
		LegacyID: 0,

		ChannelID:      channel.ID,
		ChannelName:    channel.Name,
		AuthorUsername: author,
		AuthorID:       authorID,
		Messages:       logIds,
	}

	err = log.Insert(ctx, tx, boil.Infer())
	if err != nil {
		tx.Rollback()
		return nil, errors.WrapIf(err, "log.insert")
	}

	err = tx.Commit()
	if err != nil {
		return nil, errors.WrapIf(err, "commit")
	}

	return log, nil
}

type SearchMode int

const (
	SearchModeNew SearchMode = iota
	SearchModeLegacy
)

func logsSearchNew(ctx context.Context, guildID, id int64) (*models.MessageLogs2, error) {
	return models.MessageLogs2s(
		models.MessageLogs2Where.ID.EQ(int(id)),
		models.MessageLogs2Where.GuildID.EQ(guildID)).OneG(ctx)
}

func logsSearchLegacy(ctx context.Context, guildID, id int64) (*models.MessageLogs2, error) {
	return models.MessageLogs2s(
		models.MessageLogs2Where.LegacyID.EQ(int(id)),
		models.MessageLogs2Where.GuildID.EQ(guildID)).OneG(ctx)
}

func GetChannelLogs(ctx context.Context, id, guildID int64, sm SearchMode) (*models.MessageLogs2, []*models.Messages2, error) {
	var logs *models.MessageLogs2
	var err error

	if sm == SearchModeNew {
		// try with new ID system first
		logs, err = logsSearchNew(ctx, guildID, id)
		if err == sql.ErrNoRows {
			// fallback to legacy ids
			logs, err = logsSearchLegacy(ctx, guildID, id)
			if err != nil {
				return nil, nil, err
			}
		}

		if err != nil {
			return nil, nil, errors.WrapIf(err, "messagelogs2")
		}
	} else {
		// try with legacy id system first
		logs, err = logsSearchLegacy(ctx, guildID, id)
		if err == sql.ErrNoRows {
			// fallback to new ids
			logs, err = logsSearchNew(ctx, guildID, id)
			if err != nil {
				return nil, nil, err
			}
		}

		if err != nil {
			return nil, nil, errors.WrapIf(err, "messagelogs2")
		}

	}

	args := []interface{}{}
	for _, v := range logs.Messages {
		args = append(args, v)
	}

	messages, err := models.Messages2s(qm.WhereIn("id in ?", args...), qm.OrderBy("id desc")).AllG(ctx)
	if err != nil {
		return nil, nil, errors.WrapIf(err, "messages2")
	}

	return logs, messages, err
}

func GetGuilLogs(ctx context.Context, guildID int64, before, after, limit int) ([]*models.MessageLogs2, error) {

	qms := []qm.QueryMod{
		qm.OrderBy("id desc"),
		qm.Limit(limit),
		models.MessageLogs2Where.GuildID.EQ(guildID),
	}

	if before != 0 {
		qms = append(qms, models.MessageLogs2Where.ID.LT(before))
	} else if after != 0 {
		qms = append(qms, models.MessageLogs2Where.ID.GT(after))
	}

	logs, err := models.MessageLogs2s(qms...).AllG(ctx)
	return logs, err
}

func GetUsernames(ctx context.Context, userID int64, limit, offset int) ([]*models.UsernameListing, error) {
	result, err := models.UsernameListings(models.UsernameListingWhere.UserID.EQ(null.Int64From(userID)), qm.OrderBy("id desc"), qm.Limit(limit), qm.Offset(offset)).AllG(ctx)
	return result, err
}

func GetNicknames(ctx context.Context, userID, guildID int64, limit, offset int) ([]*models.NicknameListing, error) {

	return models.NicknameListings(
		models.NicknameListingWhere.GuildID.EQ(null.StringFrom(discordgo.StrID(guildID))),
		models.NicknameListingWhere.UserID.EQ(null.Int64From(userID)),
		qm.OrderBy("id desc"),
		qm.Limit(limit),
		qm.Offset(offset)).AllG(ctx)
}

const (
	AccessModeMembers  = 0
	AccessModeEveryone = 1
)
