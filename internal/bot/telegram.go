package bot

import (
	"bytes"
	"context"
	"log/slog"
	"strings"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Handler receives incoming events from the messenger.
type Handler interface {
	HandleText(ctx context.Context, text string, replyTo int)
	HandleCallback(ctx context.Context, callbackID string, msgID int, data string)
}

// Telegram is a Messenger on top of the Bot API with long polling: no public address needed.
// It works with one chat and silently ignores all other messages.
type Telegram struct {
	b      *tg.Bot
	chatID int64
	log    *slog.Logger
	h      Handler
}

func NewTelegram(token, serverURL string, chatID int64, log *slog.Logger) (*Telegram, error) {
	t := &Telegram{chatID: chatID, log: log}
	opts := []tg.Option{
		tg.WithDefaultHandler(t.dispatch),
		tg.WithErrorsHandler(func(err error) { log.Warn("telegram", "err", err) }),
		tg.WithAllowedUpdates(tg.AllowedUpdates{"message", "callback_query"}),
		tg.WithNotAsyncHandlers(),
		// No getMe at startup: if Telegram is unreachable, payments are still received and
		// questions go out once the connection is back.
		tg.WithSkipGetMe(),
	}
	if serverURL != "" {
		opts = append(opts, tg.WithServerURL(serverURL))
	}
	b, err := tg.New(token, opts...)
	if err != nil {
		return nil, err
	}
	t.b = b
	return t, nil
}

// Start blocks until ctx is cancelled.
func (t *Telegram) Start(ctx context.Context, h Handler) {
	t.h = h
	t.b.Start(ctx)
}

func (t *Telegram) dispatch(ctx context.Context, _ *tg.Bot, u *models.Update) {
	switch {
	case u.Message != nil:
		m := u.Message
		if m.Chat.ID != t.chatID {
			// the chat_id in the log helps fill in TELEGRAM_ALLOWED_CHAT_ID during setup
			t.log.Info("telegram: ignored message from other chat", "chat_id", m.Chat.ID)
			return
		}
		if m.Text == "" {
			return
		}
		replyTo := 0
		if m.ReplyToMessage != nil {
			replyTo = m.ReplyToMessage.ID
		}
		t.h.HandleText(ctx, m.Text, replyTo)
	case u.CallbackQuery != nil:
		cq := u.CallbackQuery
		var chatID int64
		var msgID int
		switch {
		case cq.Message.Message != nil:
			chatID, msgID = cq.Message.Message.Chat.ID, cq.Message.Message.ID
		case cq.Message.InaccessibleMessage != nil:
			chatID, msgID = cq.Message.InaccessibleMessage.Chat.ID, cq.Message.InaccessibleMessage.MessageID
		}
		if chatID != t.chatID {
			t.log.Info("telegram: ignored callback from other chat", "chat_id", chatID)
			return
		}
		t.h.HandleCallback(ctx, cq.ID, msgID, cq.Data)
	}
}

func markup(buttons [][]Button) models.ReplyMarkup {
	if buttons == nil {
		// an empty keyboard removes buttons when editing
		return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
	}
	rows := make([][]models.InlineKeyboardButton, len(buttons))
	for i, row := range buttons {
		for _, b := range row {
			rows[i] = append(rows[i], models.InlineKeyboardButton{Text: b.Text, CallbackData: b.Data})
		}
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (t *Telegram) Send(ctx context.Context, m Message) (int, error) {
	p := &tg.SendMessageParams{ChatID: t.chatID, Text: m.Text, DisableNotification: m.Silent}
	if m.Buttons != nil {
		p.ReplyMarkup = markup(m.Buttons)
	}
	msg, err := t.b.SendMessage(ctx, p)
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

func (t *Telegram) Edit(ctx context.Context, msgID int, m Message) error {
	_, err := t.b.EditMessageText(ctx, &tg.EditMessageTextParams{
		ChatID: t.chatID, MessageID: msgID, Text: m.Text, ReplyMarkup: markup(m.Buttons),
	})
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

func (t *Telegram) AnswerCallback(ctx context.Context, id, text string) error {
	_, err := t.b.AnswerCallbackQuery(ctx, &tg.AnswerCallbackQueryParams{CallbackQueryID: id, Text: text})
	return err
}

func (t *Telegram) SendFile(ctx context.Context, name string, data []byte, caption string) error {
	_, err := t.b.SendDocument(ctx, &tg.SendDocumentParams{
		ChatID:   t.chatID,
		Document: &models.InputFileUpload{Filename: name, Data: bytes.NewReader(data)},
		Caption:  caption,
	})
	return err
}
