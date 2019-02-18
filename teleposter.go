package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/net/proxy"
)

/*
 * DB scheme:
 *  table likes:
 *   post_id uint64
 *   reaction_type int8
 *   user_id uint64
 *  table authors
 *   post_id uint64
 *   author_id uint64
 *  table unsupported_messages
 *   forwarded_post_id uint64
 *   keyboard_post_id uint64
 */
const (
	pollTimeout = 600
)

var reactions = [...]string{"🍰", "🤔 ", "[|||]"}

type tBot struct {
	token     string
	chatId    int64
	db        *sql.DB
	proxyAddr string
}

var bot tBot

type tTelegramResponse struct {
	Ok          bool
	Description string
	Result      json.RawMessage
}

type tUser struct{}
type tChat struct{}

func (bot *tBot) request(method string, params map[string]interface{}) (json.RawMessage, error) {
	var client *http.Client = nil
	if bot.proxyAddr != "" {
		dialer, err := proxy.SOCKS5("tcp", bot.proxyAddr, nil, proxy.Direct)
		if err != nil {
			return nil, err
		}
		transport := &http.Transport{
			Dial: dialer.Dial,
		}
		client = &http.Client{
			Transport: transport,
		}
	} else {
		client = &http.Client{}
	}
	query_params := url.Values{}
	for k, v := range params {
		query_params.Add(k, (fmt.Sprintf("%v", v)))
	}
	req_url := url.URL{
		Scheme:   "https",
		Host:     "api.telegram.org",
		Path:     "bot" + bot.token + "/" + method,
		RawQuery: query_params.Encode(),
	}
	req := req_url.String()
	resp, err := client.Get(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	answer := tTelegramResponse{Ok: false}
	err = json.Unmarshal(body, &answer)
	if err != nil {
		return nil, err
	}
	if answer.Ok == false {
		return nil, errors.New(answer.Description)
	}
	return answer.Result, nil
}

type tUpdate struct {
	Update_id            *int64           // required
	Message              *json.RawMessage // optional
	Edited_message       *json.RawMessage // optional
	Channel_post         *json.RawMessage // optional
	Edited_channel_post  *json.RawMessage // optional
	Inline_query         interface{}      // optional TODO
	Chosen_inline_result interface{}      // optional TODO
	Callback_query       *json.RawMessage // optional TODO
	Shipping_query       interface{}      // optional TODO
	Pre_checkout_query   interface{}      // optional TODO
}

func getUpdates(offset *int64) []tUpdate {
	params := make(map[string]interface{})
	params["timeout"] = pollTimeout
	if offset != nil {
		params["offset"] = *offset
	}
	answer, err := bot.request("getUpdates", params)
	if err != nil {
		log.Panic(err)
	}
	var updates []tUpdate
	err = json.Unmarshal(answer, &updates)
	if err != nil {
		log.Panic(err)
	}
	return updates
}

func getLikeKeyboard(postId *int64) string {
	var reactions_cnt [len(reactions)]int
	if postId != nil {
		rows, err := bot.db.Query(`
            SELECT reaction_type
            FROM likes
            WHERE post_id = ?`, *postId)
		if err != nil {
			log.Panic(err)
		}
		defer rows.Close()
		for rows.Next() {
			var reaction_type int
			err := rows.Scan(&reaction_type)
			if err != nil {
				log.Panic(err)
			}
			if reaction_type < 0 || reaction_type >= len(reactions) {
				log.Panic(errors.New("Bad reaction type"))
			}
			reactions_cnt[reaction_type] += 1
		}
	}
	var keyboard strings.Builder
	keyboard.WriteString(`{"inline_keyboard":[[`)
	for i, cnt := range reactions_cnt {
		if i != 0 {
			keyboard.WriteString(",")
		}
		fmt.Fprintf(&keyboard, `{"text":"%s %d","callback_data":"%d"}`,
			reactions[i], cnt, i)
	}
	keyboard.WriteString(`]]}`)
	return keyboard.String()
}

func handleMessage(messageJson json.RawMessage) {
	log.Println("Input message")
	type tChat struct {
		Id *int64
	}
	type tAnimation struct {
		File_id  string
		Width    *int64
		Height   *int64
		Duration *int64
	}
	type tPhoto struct {
		File_id string
	}
	type tMessage struct {
		Message_id *int64
		Chat       *tChat
		Text       *string
		Audio      *interface{} // TODO
		Document   *interface{} // TODO
		Animation  *tAnimation
		Photo      []tPhoto
		Sticker    *interface{} // TODO
		Video      *interface{} // TODO
		Voice      *interface{} // TODO
		Video_note *interface{} // TODO
		Caption    *interface{} // TODO
		Contact    *interface{} // TODO
		Location   *interface{} // TODO
	}
	var message tMessage
	err := json.Unmarshal(messageJson, &message)
	if err != nil {
		log.Panic(err)
	}

	_, err = bot.db.Exec(`
        INSERT INTO authors (post_id, author_id)
        VALUES(?, ?)`,
		*message.Message_id, *message.Chat.Id)
	if err != nil {
		log.Panic(err)
	}

	supported := true
	var forwardedMessageId int64
	var replyMethod string
	params := make(map[string]interface{})
	params["chat_id"] = bot.chatId
	if message.Text != nil {
		replyMethod = "sendMessage"
		params["text"] = *message.Text
	} else if message.Photo != nil {
		replyMethod = "sendPhoto"
		params["photo"] = message.Photo[0].File_id
		/* TODO send media group if have field media_group_id
			sendMediaGroup does not support inline keyboard
			replyMethod = "sendMediaGroup"
			media := "["
			for i, photo := range uniquePhotos {
				if i != 0 {
					media += ","
				}
				media += fmt.Sprintf(`{"type":"photo", "media":"%s"}`,
					photo.File_id)
			}
			media += "]"
			params["media"] = media
			supported = false
		}*/
	} else if message.Animation != nil {
		replyMethod = "sendAnimation"
		params["animation"] = message.Animation.File_id
		params["width"] = *message.Animation.Width
		params["height"] = *message.Animation.Height
		params["duration"] = *message.Animation.Duration
		if message.Caption != nil {
			params["caption"] = *message.Caption
		}
	} else {
		supported = false
	}
	if !supported {
		log.Println("Unsupported message type")
		answer, err := bot.request("forwardMessage", map[string]interface{}{
			"chat_id":      bot.chatId,
			"from_chat_id": *message.Chat.Id,
			"message_id":   *message.Message_id,
		})
		if err != nil {
			log.Panic(err)
		}
		var forwardedMessage tMessage
		err = json.Unmarshal(answer, &forwardedMessage)
		if err != nil {
			log.Panic(err)
		}
		forwardedMessageId = *forwardedMessage.Message_id
		replyMethod = "sendMessage"
		params["text"] = "^^Нраица?"
	}
	params["reply_markup"] = getLikeKeyboard(nil)
	answer, err := bot.request(replyMethod, params)
	if err != nil {
		log.Panic(err)
	}
	if !supported {
		var sentMessage tMessage
		err = json.Unmarshal(answer, &sentMessage)
		if err != nil {
			log.Panic(err)
		}
		_, err = bot.db.Exec(`
            INSERT INTO unsupported_messages (forwarded_post_id, keyboard_post_id)
            VALUES(?, ?)`,
			forwardedMessageId, *sentMessage.Message_id)
	}
}

func like(postId int64, reactionType int, userId int64, name string) {
	res, err := bot.db.Exec(`
        DELETE FROM likes
        WHERE post_id = ? AND reaction_type = ? AND user_id = ?`,
		postId, reactionType, userId)
	if err != nil {
		log.Panic(err)
	}
	likes_cnt, err := res.RowsAffected()
	if err != nil {
		log.Panic(err)
	}
	res, err = bot.db.Exec(`
        DELETE FROM likes
        WHERE post_id = ? AND user_id = ?`,
		postId, userId)
	if err != nil {
		log.Panic(err)
	}
	if likes_cnt%2 == 0 {
		_, err = bot.db.Exec(`
            INSERT INTO likes (post_id, reaction_type, user_id)
            VALUES(?, ?, ?)`,
			postId, reactionType, userId)
		log.Printf("Reaction of <%v> to %v: %v\n", name, postId, reactionType)
		if err != nil {
			log.Panic(err)
		}
	} else {
		log.Printf("Reaction of <%v> to %v: not %v\n", name, postId, reactionType)
	}
}

func handleCallback(callbackQueryJson json.RawMessage) {
	type tChat struct {
		Id *int64
	}
	type tUser struct {
		Id         *int64
		First_name string
	}
	type tMessage struct {
		Message_id *int64
		Chat       *tChat
	}
	type tCallbackQuery struct {
		Message *tMessage
		From    *tUser
		Data    *string
	}
	var callbackQuery tCallbackQuery
	err := json.Unmarshal(callbackQueryJson, &callbackQuery)
	if err != nil {
		log.Panic(err)
	}
	// TODO check fields presence
	num, err := strconv.Atoi(*callbackQuery.Data)
	if num < 0 || num >= len(reactions) {
		log.Panic(errors.New("Bad reaction type"))
	}
	like(*callbackQuery.Message.Message_id, num,
		*callbackQuery.From.Id,
		callbackQuery.From.First_name)
	params := map[string]interface{}{
		"chat_id":      *callbackQuery.Message.Chat.Id,
		"message_id":   *callbackQuery.Message.Message_id,
		"reply_markup": getLikeKeyboard(callbackQuery.Message.Message_id),
	}
	bot.request("editMessageReplyMarkup", params) // TODO check answer and err
}

func handleUpdate(update tUpdate) int64 {
	if update.Update_id == nil {
		log.Panic("nil update id")
	}
	if update.Message != nil {
		handleMessage(*update.Message)
	}
	if update.Callback_query != nil {
		handleCallback(*update.Callback_query)
	}
	if update.Edited_message != nil ||
		update.Inline_query != nil ||
		update.Chosen_inline_result != nil ||
		update.Shipping_query != nil ||
		update.Pre_checkout_query != nil {
		log.Println("Some features of this messages are not supported by the bot yet")
	}
	return *update.Update_id
}

type IntFlag struct {
	set   bool
	value int64
}

func (intf *IntFlag) Set(x string) error {
	var err error
	intf.value, err = strconv.ParseInt(x, 10, 64)
	if err != nil {
		return err
	}
	intf.set = true
	return nil
}

func (intf *IntFlag) String() string {
	if intf.set {
		return strconv.FormatInt(intf.value, 10)
	}
	return "nil"
}

func main() {
	var chatId IntFlag
	var dbname string
	flag.StringVar(&dbname, "dbname", "", "Database filename")
	flag.StringVar(&bot.token, "token", "", "Bot token")
	flag.Var(&chatId, "chat", "ChatId")
	flag.StringVar(&bot.proxyAddr, "proxy", "", "SOCKS5 proxy address")
	flag.Parse()
	if dbname == "" || bot.token == "" || !chatId.set {
		flag.Usage()
		return
	}
	bot.chatId = chatId.value

	var err error
	bot.db, err = sql.Open("sqlite3", dbname)
	if err != nil {
		log.Panic(err)
	}
	defer bot.db.Close()
	_, err = bot.db.Exec(`
        CREATE TABLE IF NOT EXISTS likes (
            post_id       INTEGER,
            reaction_type INTEGER,
            user_id       INTEGER
        );
        CREATE TABLE IF NOT EXISTS authors (
            post_id   INTEGER,
            author_id INTEGER
        );
        CREATE TABLE IF NOT EXISTS unsupported_messages (
            forwarded_post_id INTEGER,
            keyboard_post_id  INTEGER
        );
    `)
	if err != nil {
		log.Panic(err)
	}

	var poffset *int64
	var offset int64
	for {
		updates := getUpdates(poffset)
		for _, update := range updates {
			offset = handleUpdate(update) + 1
			poffset = &offset
		}
		if len(updates) == 0 {
			log.Println("No updates")
		}
	}
}
