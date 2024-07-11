// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/maruel/sillybot"
	"github.com/maruel/sillybot/huggingface"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// discordBot is the live instance of the bot talking to the Discord API.
//
// Throughout the code, a Discord Server is called a "Guild". See
// https://discord.com/developers/docs/quick-start/overview-of-apps#where-are-apps-installed
type discordBot struct {
	ctx          context.Context
	dg           *discordgo.Session
	l            *sillybot.LLM
	ig           *sillybot.ImageGen
	mem          *sillybot.Memory
	systemPrompt string
	f            *opentype.Font
	chat         chan msgReq
	image        chan intReq
	wg           sync.WaitGroup
}

// newDiscordBot opens a websocket connection to Discord and begin listening.
func newDiscordBot(ctx context.Context, token string, verbose bool, l *sillybot.LLM, ig *sillybot.ImageGen, mem *sillybot.Memory, systPrmpt string) (*discordBot, error) {
	f, err := opentype.Parse(goitalic.TTF)
	if err != nil {
		slog.Error("discord", "message", "failed decoding png", "error", err)
	}
	discordgo.Logger = func(msgL, caller int, format string, a ...interface{}) {
		msg := fmt.Sprintf(format, a...)
		switch msgL {
		case discordgo.LogDebug:
			slog.Debug(msg)
		case discordgo.LogInformational:
			slog.Info(msg)
		case discordgo.LogWarning:
			slog.Warn(msg)
		case discordgo.LogError:
			slog.Error(msg)
		}
	}
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	if verbose {
		dg.LogLevel = discordgo.LogInformational
		// It's very verbose.
		//dg.LogLevel = discordgo.LogDebug
	}
	// We want to receive as few messages as possible.
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentDirectMessages
	d := &discordBot{
		ctx:          ctx,
		dg:           dg,
		l:            l,
		ig:           ig,
		mem:          mem,
		systemPrompt: systPrmpt,
		f:            f,
		chat:         make(chan msgReq, 5),
		image:        make(chan intReq, 3),
	}
	// The events are listed at
	// https://discord.com/developers/docs/topics/gateway-events#receive-events
	// Note that all messages are called asynchronously.
	_ = dg.AddHandler(d.onReady)
	_ = dg.AddHandler(d.onGuildCreate)
	_ = dg.AddHandler(d.onMessageCreate)
	_ = dg.AddHandler(d.onInteractionCreate)
	d.wg.Add(2)
	go d.chatRoutine()
	go d.imageRoutine()
	if err = dg.Open(); err != nil {
		_ = d.dg.Close()
		return nil, err
	}
	slog.Info("discord", "state", "running", "info", "Press CTRL-C to exit.")
	return d, nil
}

func (d *discordBot) Close() error {
	slog.Info("discord", "state", "terminating")
	// TODO: Send a bye bye before closing.
	// TODO: Set presence to "away". It's already the case for channels but not
	// for direct messages.
	err := d.dg.Close()
	d.chat <- msgReq{}
	d.image <- intReq{}
	d.wg.Wait()
	return err
}

// Handlers

// onReady is received right after the initial handshake.
//
// It's the very first message. At this point, guilds are not yet available.
// See https://discord.com/developers/docs/topics/gateway-events#ready
func (d *discordBot) onReady(dg *discordgo.Session, r *discordgo.Ready) {
	slog.Debug("discord", "event", "ready", "session", dg, "event", r)
	slog.Info("discord", "event", "ready", "user", r.User.String())

	// TODO: Get list of DMs and tell users "I'm back up!"

	// See https://discord.com/developers/docs/interactions/application-commands
	cmds := []*discordgo.ApplicationCommand{
		// meme_*
		{
			Name:        "meme_auto",
			Type:        discordgo.ChatApplicationCommand,
			Description: "Generate a meme in full automatic mode. Create both the image and labels by leveraging the LLM.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "description",
					Description: "Description used to generate both the meme labels and background image. The LLM will enhance both.",
					Required:    true,
				},
			},
		},
		{
			Name:        "meme_manual",
			Type:        discordgo.ChatApplicationCommand,
			Description: "Generate a meme in full manual mode. Specify both the image and the labels yourself.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "image_prompt",
					Description: "Exact Stable Diffusion style prompt to use to generate the image.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "labels_content",
					Description: "Exact text to overlay on the image. Use comma to split lines.",
					Required:    true,
				},
			},
		},
		{
			Name:        "meme_labels_auto",
			Type:        discordgo.ChatApplicationCommand,
			Description: "Generate meme labels in automatic mode. Create the text by leveraging the LLM.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "description",
					Description: "Description to use to generate the meme labels. The LLM will enhance both.",
					Required:    true,
				},
			},
		},

		// image_*
		{
			Name:        "image_auto",
			Type:        discordgo.ChatApplicationCommand,
			Description: "Generate an image in automatic mode. It automatically uses the LLM to enhance the prompt.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "description",
					Description: "Description to use to generate the image. The LLM will enhance it.",
					Required:    true,
				},
			},
		},
		{
			Name:        "image_manual",
			Type:        discordgo.ChatApplicationCommand,
			Description: "Generate an image in manual mode.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "image_prompt",
					Description: "Exact Stable Diffusion style prompt to use to generate the image.",
					Required:    true,
				},
			},
		},

		// list_models
		{
			Name:        "list_models",
			Type:        discordgo.ChatApplicationCommand,
			Description: "List models available and the one currently used.",
		},

		// forget
		{
			Name:        "forget",
			Type:        discordgo.ChatApplicationCommand,
			Description: "Forget our past conversation. Optionally overrides the system prompt.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "system_prompt",
					Description: "new system prompt to use",
				},
			},
		},
		{
			Name: "forget",
			Type: discordgo.UserApplicationCommand,
		},
	}
	if strings.Contains(dg.State.User.ID, "(dev)") {
		for _, c := range cmds {
			c.Name += "_dev"
		}
	}
	if _, err := dg.ApplicationCommandBulkOverwrite(r.Application.ID, "", cmds); err != nil {
		// TODO: Make this a hard fail.
		slog.Error("discord", "message", "failed to register commands", "error", err)
		return
	}
	slog.Info("discord", "message", "registered commands", "number", len(cmds))
}

// onGuildCreate is received when new guild (server) is joined or becomes
// available right after connecting.
//
// https://discord.com/developers/docs/topics/gateway-events#guild-create
func (d *discordBot) onGuildCreate(dg *discordgo.Session, event *discordgo.GuildCreate) {
	slog.Debug("discord", "event", "guildCreate", "event", event.Guild)
	slog.Info("discord", "event", "guildCreate", "name", event.Guild.Name)
	if event.Guild.Unavailable {
		return
	}
	const welcome = "I'm back up! Check out my commands by typing the '/' slash key. I can generate images and memes."
	for _, channel := range event.Guild.Channels {
		if t := channel.Type; t == discordgo.ChannelTypeGuildVoice || t == discordgo.ChannelTypeGuildCategory {
			continue
		}
		// Don't alert again if the last connection was recent, to not spam the
		// channel.
		msgs, err := dg.ChannelMessages(channel.ID, 5, "", "", "")
		if err != nil {
			slog.Error("discord", "error", err)
		}
		skip := false
		for _, msg := range msgs {
			if msg.Author.ID == dg.State.User.ID && msg.Content == welcome {
				slog.Info("discord", "message", "skipping welcome to not spam", "channel", channel.Name)
				skip = true
				break
			}
		}
		if !skip {
			slog.Info("discord", "message", "welcome", "channel", channel.Name)
			_, _ = dg.ChannelMessageSend(channel.ID, welcome)
		}
	}
}

// onMessageCreate is received when new message is created on any channel that
// the authenticated bot has access to.
//
// See https://discord.com/developers/docs/topics/gateway-events#message-create
func (d *discordBot) onMessageCreate(dg *discordgo.Session, m *discordgo.MessageCreate) {
	slog.Debug("discord", "event", "messageCreate", "message", m.Message, "state", dg.State)
	botid := dg.State.User.ID
	if m.Author.ID == botid {
		return
	}
	user := fmt.Sprintf("<@%s>", botid)
	if m.GuildID != "" && !strings.Contains(m.Content, user) {
		// Ignore if the bot is not explicitly referenced to.
		return
	}
	msg := strings.TrimSpace(strings.ReplaceAll(m.Content, user, ""))
	slog.Info("discord", "event", "messageCreate", "author", m.Author.Username, "message", msg)
	if d.l == nil {
		if _, err := dg.ChannelMessageSend(m.ChannelID, "LLM is not enabled."); err != nil {
			slog.Error("discord", "message", "failed posting message", "error", err)
		}
		return
	}
	// Immediately signal the user that the bot is preparing a reply.
	if err := dg.ChannelTyping(m.ChannelID); err != nil {
		slog.Error("discord", "message", "failed posting 'user typing'", "error", err)
		// Continue anyway.
	}
	req := msgReq{
		msg:       msg,
		authorID:  m.Author.ID,
		channelID: m.ChannelID,
		guildID:   m.GuildID,
		replyToID: m.ID,
	}
	select {
	case d.chat <- req:
	default:
		_, err := dg.ChannelMessageSendComplex(req.channelID, &discordgo.MessageSend{
			Content:   "Sorry! I have too many pending chat requests. Please retry in a moment.",
			Reference: &discordgo.MessageReference{MessageID: req.replyToID, ChannelID: req.channelID, GuildID: req.guildID},
		})
		if err != nil {
			slog.Error("discord", "message", "failed posting message", "error", err)
		}
	}
}

func (d *discordBot) onInteractionCreate(dg *discordgo.Session, event *discordgo.InteractionCreate) {
	slog.Info("discord", "event", "interactionCreate", "name", event.Data)
	if t := event.Data.Type(); t != discordgo.InteractionApplicationCommand {
		slog.Warn("discord", "message", "surprising interaction", "type", t.String())
		return
	}
	data, ok := event.Data.(discordgo.ApplicationCommandInteractionData)
	if !ok {
		slog.Warn("discord", "message", "invalid type", "type", event.Data)
		return
	}
	switch data.Name {
	case "forget":
		d.onForget(event, data)
	case "list_models":
		d.onListModels(event, data)
	case "meme_auto", "meme_manual", "meme_labels_auto", "image_auto", "image_manual":
		d.onImage(event, data)
	default:
		slog.Warn("discord", "unexpected command", data.Name, "data", event.Interaction)
	}
}

func (d *discordBot) onForget(event *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	u := event.User
	if event.Member != nil {
		u = event.Member.User
	}
	c := d.mem.Get(u.ID, event.ChannelID)
	opts := struct {
		SystemPrompt string `json:"system_prompt"`
	}{SystemPrompt: d.systemPrompt}
	if err := optionsToStruct(data.Options, &opts); err != nil {
		slog.Error("discord", "command", data.Name, "message", "failed decoding command options", "error", err)
		return
	}
	reply := "I don't know you. I can't wait to start our discussion so I can get to know you better!"
	if len(c.Messages) > 1 {
		reply = "The memory of our past conversations just got zapped."
	}
	reply += "\n*System prompt*: " + escapeMarkdown(opts.SystemPrompt)
	c.Messages = []sillybot.Message{{Role: sillybot.System, Content: opts.SystemPrompt}}
	if err := d.interactionRespond(event.Interaction, reply); err != nil {
		slog.Error("discord", "command", data.Name, "message", "failed reply", "error", err)
	}
}

func (d *discordBot) onListModels(event *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	var replies []string
	reply := "Known models:\n"
	for _, k := range d.l.KnownLLMs {
		reply += "- [`" + k.Basename + "`](" + k.URL() + ") "
		parts := strings.SplitN(k.RepoID, "/", 2)
		info := huggingface.Model{ModelRef: huggingface.ModelRef{Author: parts[0], Repo: parts[1]}}
		if err := d.l.HF.GetModelInfo(d.ctx, &info); err != nil {
			reply += " Oh no, we failed to query: " + err.Error()
			slog.Error("discord", "command", data.Name, "error", err)
		} else {
			reply += " Quantizations: "
			for _, f := range info.Files {
				if !strings.HasPrefix(f, k.Basename) {
					continue
				}
				if strings.HasPrefix(filepath.Ext(f), ".cat") {
					// TODO: Support split files. For now just hide them. They are large
					// anyway so it's only for power users.
					continue
				}
				f = f[len(k.Basename):]
				f = strings.TrimSuffix(f, ".gguf")
				f = strings.TrimSuffix(f, ".llamafile")
				reply += f + ", "
			}
			if info.Upstream.Author != "" && info.Upstream.Repo != "" {
				infoUpstream := huggingface.Model{ModelRef: info.Upstream}
				if err = d.l.HF.GetModelInfo(d.ctx, &infoUpstream); err != nil {
					reply += " Oh no, we failed to query: " + err.Error()
					slog.Error("discord", "command", data.Name, "error", err)
				} else {
					if infoUpstream.NumWeights != 0 {
						reply += fmt.Sprintf(" Tensors: %s in %.fB", infoUpstream.TensorType, float64(infoUpstream.NumWeights)*0.000000001)
					}
					if infoUpstream.LicenseURL != "" {
						reply += " License: [" + infoUpstream.License + "](" + infoUpstream.LicenseURL + ")"
					} else {
						reply += " License: " + infoUpstream.License
					}
				}
			}
		}
		reply += "\n"
		if len(reply) > 1000 {
			// Don't hit the 2000 characters limit.
			replies = append(replies, reply)
			reply = ""
		}
	}
	if reply != "" {
		replies = append(replies, reply)
	}
	if err := d.interactionRespond(event.Interaction, replies[0]); err != nil {
		slog.Error("discord", "command", data.Name, "message", "failed reply", "error", err)
	}
	for _, r := range replies[1:] {
		// TODO: use MessageID so it becomes a set of replies.
		if _, err := d.dg.ChannelMessageSend(event.Interaction.ChannelID, r); err != nil {
			slog.Error("discord", "command", data.Name, "message", "failed reply", "error", err)
		}
	}
}

func (d *discordBot) onImage(event *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	opts := struct {
		// meme_auto, meme_labels_auto, image_auto
		Description string `json:"description"`
		// meme_manual, image_manual
		ImagePrompt string `json:"image_prompt"`
		// meme_manual
		LabelsContent string `json:"labels_content"`
	}{}
	if err := optionsToStruct(data.Options, &opts); err != nil {
		slog.Error("discord", "command", data.Name, "message", "failed decoding command options", "error", err)
		return
	}
	if d.ig == nil && strings.HasSuffix(data.Name, "_auto") && data.Name != "meme_labels_auto" {
		if err := d.interactionRespond(event.Interaction, "Image generation is not enabled. Restart with bot.image_gen.model set in config.yml."); err != nil {
			slog.Error("discord", "command", data.Name, "message", "failed reply to enable", "error", err)
		}
		return
	}
	req := intReq{
		description:   opts.Description,
		imagePrompt:   opts.ImagePrompt,
		labelsContent: opts.LabelsContent,
		cmdName:       data.Name,
		int:           event.Interaction,
	}
	select {
	case d.image <- req:
	default:
		if err := d.interactionRespond(event.Interaction, "Sorry! I have too many pending image requests. Please retry in a moment."); err != nil {
			slog.Error("discord", "command", data.Name, "message", "failed reply rate limit", "error", err)
		}
		return
	}
	r := &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource}
	if err := d.dg.InteractionRespond(req.int, r); err != nil {
		slog.Error("discord", "command", data.Name, "message", "failed reply update", "error", err)
	}
}

func (d *discordBot) interactionRespond(int *discordgo.Interaction, s string) error {
	r := &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Content: s}}
	return d.dg.InteractionRespond(int, r)
}

// Internal

// chatRoutine serializes the chat requests.
func (d *discordBot) chatRoutine() {
	for req := range d.chat {
		if req.authorID == "" {
			d.wg.Done()
			return
		}
		d.handlePrompt(req)
	}
}

// imageRoutine serializes the chat requests.
func (d *discordBot) imageRoutine() {
	for req := range d.image {
		if req.int == nil {
			d.wg.Done()
			return
		}
		d.handleImage(req)
	}
}

// handlePrompt uses the LLM to generate a response.
func (d *discordBot) handlePrompt(req msgReq) {
	c := d.mem.Get(req.authorID, req.channelID)
	if len(c.Messages) == 0 {
		c.Messages = []sillybot.Message{{Role: sillybot.System, Content: d.systemPrompt}}
	}
	c.Messages = append(c.Messages, sillybot.Message{Role: sillybot.User, Content: req.msg})
	words := make(chan string, 10)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		t := time.NewTicker(3 * time.Second)
		replyToID := req.replyToID
		text := ""
		pending := ""
		for {
			select {
			case w, ok := <-words:
				if !ok {
					if pending != "" {
						text += pending
						_, err := d.dg.ChannelMessageSendComplex(req.channelID, &discordgo.MessageSend{
							Content:   pending,
							Reference: &discordgo.MessageReference{MessageID: replyToID, ChannelID: req.channelID, GuildID: req.guildID},
						})
						if err != nil {
							slog.Error("discord", "message", "failed posting message", "error", err)
						}
					}
					// Remember our own answer.
					c.Messages = append(c.Messages, sillybot.Message{Role: sillybot.Assistant, Content: text})
					t.Stop()
					wg.Done()
					return
				}
				pending += w
			case <-t.C:
				i := splitResponse(pending)
				if i == 0 {
					continue
				}
				msg, err := d.dg.ChannelMessageSendComplex(req.channelID, &discordgo.MessageSend{
					Content:   pending[:i],
					Reference: &discordgo.MessageReference{MessageID: replyToID, ChannelID: req.channelID, GuildID: req.guildID},
				})
				text += pending[:i]
				pending = pending[i:]
				if err != nil {
					slog.Error("discord", "message", "failed posting message", "error", err)
				} else {
					replyToID = msg.ID
				}
				if err := d.dg.ChannelTyping(req.channelID); err != nil {
					slog.Error("discord", "message", "failed posting 'user typing'", "error", err)
				}
			}
		}
	}()
	err := d.l.PromptStreaming(d.ctx, c.Messages, 0, 1.0, words)
	close(words)
	wg.Wait()

	if err != nil {
		if _, err = d.dg.ChannelMessageSend(req.channelID, "Prompt generation failed: "+err.Error()); err != nil {
			slog.Error("discord", "message", "failed posting message", "error", err)
		}
	}
}

// splitResponse take pending reply from the LLM and returns the amount of
// bytes to send back. Returns 0 if nothing should be sent.
//
// The basic assumption here is that most LLMs are trained to generate
// markdown, for the better or worst.
//
// If you spot a case where it doesn't work right in the wild, please fix and
// add a test case! Make sure it's 100% test coverage (except the panic).
func splitResponse(t string) int {
	// First priority is 3 backquotes. We must never break that in the middle.
	if backquotes := strings.Count(t, "```"); backquotes == 1 {
		i := strings.Index(t, "```")
		t = t[:i]
	} else if backquotes >= 2 {
		// Cut right after the second.
		start := strings.Index(t, "```") + 3
		return strings.Index(t[start:], "```") + start + 3
	}
	if t == "" {
		return 0
	}

	// If there's a EOL, use it.
	if i := strings.LastIndexByte(t, '\n'); i >= 10 {
		return i + 1
	}

	// Now look for enumerations. The only thing we want to break on for
	// enumerations is '\n'.
	isEnum := strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")
	if !isEnum {
		var err error
		isEnum, err = regexp.MatchString(t, `^\d+\. .*`)
		if err != nil {
			panic(err)
		}
	}
	if isEnum {
		return 0
	}

	if len(t) < 10 {
		// Not enough characters to send, ignore.
		return 0
	}

	// If there's backquotes, e.g. `foo.bar`, they mess up punctuation search. So
	// only start the search after the last backquote.
	start := 0
	if backquotes := strings.Count(t, "`"); (backquotes & 1) == 1 {
		// Impair number of backquotes. Limit ourselves up to the last one.
		t = t[:strings.LastIndexByte(t, '`')]
	}

	// TODO: Highlighting pairs: '*' and '_'
	i := strings.LastIndexAny(t[start:], ".?!")
	if i == -1 {
		return 0
	}
	return start + i + 1
}

// handleImage generates an image based on the user prompt.
func (d *discordBot) handleImage(req intReq) {
	content := ""
	if req.description != "" {
		content += "*Description*: " + escapeMarkdown(req.description) + "\n"
	}
	// Optionally use the LLM to generate the prompt(s) based on the description.
	// TODO: We could do the two requests in parallel to reduce latency. Only
	// important in "meme_auto".

	// Image
	if req.cmdName == "meme_auto" || req.cmdName == "image_auto" {
		// TODO: fine-tune.
		const imagePrompt = "You are autoregressive language model that specializes in creating perfect, outstanding prompts for generative art models like Stable Diffusion. Your job is to take user ideas, capture ALL main parts, and turn into amazing prompts. You have to capture everything from the user's prompt and then use your talent to make it amazing. You are a master of art styles, terminology, pop culture, and photography across the globe. Respond only with the new prompt. Exclude article words."
		msgs := []sillybot.Message{
			{Role: sillybot.System, Content: imagePrompt},
			{Role: sillybot.User, Content: req.description},
		}
		if reply, err := d.l.Prompt(d.ctx, msgs, 0, 1.0); err != nil {
			slog.Error("discord", "command", req.cmdName, "message", "failed to enhance prompt", "error", err)
			content += "*LLM Error*: " + escapeMarkdown(err.Error()) + "\n"
		} else {
			req.imagePrompt = reply
		}
	}
	if req.imagePrompt != "" {
		content += "*Image prompt*: " + escapeMarkdown(req.imagePrompt) + "\n"
	}

	// Labels
	if req.cmdName == "meme_auto" || req.cmdName == "meme_labels_auto" {
		// TODO: fine-tune.
		const memePrompt = "You are autoregressive language model that specializes in creating perfect, outstanding meme text. Your job is to take user ideas, capture ALL main parts, and turn into amazing meme labels. You have to capture everything from the user's prompt and then use your talent to make it amazing filled with sarcasm. Respond only with the new meme text. Make it as succinct as possible. Use few words. Use exactly one comma. Exclude article words."
		msgs := []sillybot.Message{
			{Role: sillybot.System, Content: memePrompt},
			{Role: sillybot.User, Content: req.description},
		}
		if meme, err := d.l.Prompt(d.ctx, msgs, 0, 1.0); err != nil {
			slog.Error("discord", "command", req.cmdName, "message", "failed to make meme prompt", "error", err)
			content += "*LLM Error*: " + escapeMarkdown(err.Error()) + "\n"
		} else {
			req.labelsContent = strings.Trim(meme, "\",.")
		}
		if req.cmdName == "meme_labels_auto" {
			// Special case since we don't actually need an image.
			content += "*Labels*: " + escapeMarkdown(req.labelsContent)
			if _, err := d.dg.InteractionResponseEdit(req.int, &discordgo.WebhookEdit{Content: &content}); err != nil {
				slog.Error("discord", "command", req.cmdName, "message", "failed posting interaction", "error", err)
			}
			return
		}
	}
	if req.labelsContent != "" {
		content += "*Labels*: " + escapeMarkdown(req.labelsContent) + "\n"
	}

	// TODO: Generate multiple images when the queue is empty?
	img, err := d.ig.GenImage(d.ctx, req.imagePrompt, 1)
	if err != nil {
		content += "*ImageGen Error*: " + escapeMarkdown(err.Error())
		if _, err = d.dg.InteractionResponseEdit(req.int, &discordgo.WebhookEdit{Content: &content}); err != nil {
			slog.Error("discord", "command", req.cmdName, "message", "failed posting interaction", "error", err)
		}
		return
	}

	if req.labelsContent != "" {
		drawMemeOnImage(img, d.f, req.labelsContent)
	}
	w := bytes.Buffer{}
	if err = png.Encode(&w, img); err != nil {
		slog.Error("discord", "command", req.cmdName, "message", "failed encoding PNG", "error", err)
		return
	}
	_, err = d.dg.InteractionResponseEdit(req.int, &discordgo.WebhookEdit{
		Content: &content,
		Files: []*discordgo.File{
			{
				Name:        "prompt.png",
				ContentType: "image/png",
				Reader:      &w,
			},
		},
	})
	if err != nil {
		slog.Error("discord", "command", req.cmdName, "message", "failed posting interaction", "error", err)
	}
}

func drawMemeOnImage(img *image.NRGBA, f *opentype.Font, meme string) {
	lines := strings.Split(meme, ",")
	switch len(lines) {
	case 1:
		drawTextOnImage(img, f, 0, lines[0])
	case 2:
		drawTextOnImage(img, f, 0, lines[0])
		drawTextOnImage(img, f, 100, lines[1])
	case 3:
		drawTextOnImage(img, f, 0, lines[0])
		drawTextOnImage(img, f, 50, lines[1])
		drawTextOnImage(img, f, 100, lines[2])
	case 4:
		drawTextOnImage(img, f, 0, lines[0])
		drawTextOnImage(img, f, 30, lines[1])
		drawTextOnImage(img, f, 60, lines[2])
		drawTextOnImage(img, f, 100, lines[3])
	default:
		drawTextOnImage(img, f, 0, lines[0])
		drawTextOnImage(img, f, 20, lines[1])
		drawTextOnImage(img, f, 50, lines[2])
		drawTextOnImage(img, f, 80, lines[3])
		drawTextOnImage(img, f, 100, lines[4])
	}
}

func drawTextOnImage(img *image.NRGBA, f *opentype.Font, top int, text string) {
	bounds := img.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	d := font.Drawer{Dst: img, Src: image.Black}

	// Do once with a size way too large, then adjust the size.
	var err error
	if d.Face, err = opentype.NewFace(f, &opentype.FaceOptions{Size: 1000, DPI: 72}); err != nil {
		slog.Error("discord", "message", "failed loading typeface", "error", err)
		return
	}
	textWidth := d.MeasureString(text).Round()
	// TODO: This code needs proper fine tuning.
	if d.Face, err = opentype.NewFace(f, &opentype.FaceOptions{Size: 1000. * float64(w) / (250. + float64(textWidth)), DPI: 72}); err != nil {
		slog.Error("discord", "message", "failed loading typeface", "error", err)
		return
	}
	textWidth = d.MeasureString(text).Round()
	textHeight := d.Face.Metrics().Height.Ceil()
	// The text tends to offshoot on the right so offset it on the left, divide
	// by 4 instead of 2.
	x := (w - textWidth) / 4
	y := top * h / 100
	if y < textHeight {
		y = textHeight
	} else if y > h-40 {
		y = h - 40
	}
	// Draw a crude outline.
	// TODO: It's not super efficient to draw this many (36) times! Make it
	// faster unless it's good enough.
	// TODO: Rasterize at 8x then downsize to reduce aliasing and not have to
	// render so many times.
	radius := 5.
	for i := 0; i < 360; i += 10 {
		a := math.Pi / 180. * float64(i)
		dx := math.Cos(a) * radius
		dy := math.Sin(a) * radius
		dot := fixed.Point26_6{X: fixed.Int26_6((float64(x) + dx) * 64), Y: fixed.Int26_6((float64(y) + dy) * 64)}
		if dot != d.Dot {
			d.Dot = dot
			d.DrawString(text)
		}
	}
	// Draw the final text.
	d.Src = image.White
	d.Dot = fixed.P(x, y)
	d.DrawString(text)
}

// msgReq is an incoming message pending to be processed.
type msgReq struct {
	// msg is the message received.
	// See
	// https://discord.com/developers/docs/reference#message-formatting-formats
	// for the formatting of references.
	msg       string
	authorID  string
	channelID string
	guildID   string
	replyToID string
}

// intReq is an interaction request to generate an image.
type intReq struct {
	description   string
	imagePrompt   string
	labelsContent string
	cmdName       string
	// Only there for ID and Token.
	int *discordgo.Interaction
}

func optionsToStruct(opts []*discordgo.ApplicationCommandInteractionDataOption, out interface{}) error {
	// The world's slowest implementation.
	// TODO: Use something faster, e.g. use reflect directly. PR appreciated. ❤
	t := map[string]interface{}{}
	for _, o := range opts {
		t[o.Name] = o.Value
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func escapeMarkdown(s string) string {
	const _MARKDOWN_ESCAPE_COMMON = `^>(?:>>)?\s|\[.+\]\(.+\)|^#{1,3}|^\s*-`
	const _MARKDOWN_STOCK_REGEX = `(?P<markdown>[_\\~|\*` + "`" + `]|` + _MARKDOWN_ESCAPE_COMMON + `)`
	re := regexp.MustCompile(_MARKDOWN_STOCK_REGEX)
	return re.ReplaceAllStringFunc(s, func(m string) string { return "\\" + m })
}
