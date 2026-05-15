// test-connect: smoke test and history extractor for the caseyng/whatsmeow fork.
//
// Usage:
//
//	./test-connect [flags] <phone-number>
//
// Flags:
//
//	--db <file>          SQLite file (default: test-connect.db)
//	--readonly           Don't send a test message on connect
//	--chat <jid>         Only store messages from this chat JID (repeatable)
//	--auto-disconnect    Disconnect automatically when history sync completes
//	--unpair             Unpair (remove linked device) instead of just disconnecting
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type chatFilter []string

func (f *chatFilter) String() string { return strings.Join(*f, ",") }
func (f *chatFilter) Set(v string) error {
	*f = append(*f, v)
	return nil
}

var (
	dbFile         = flag.String("db", "test-connect.db", "SQLite file path")
	readonly       = flag.Bool("readonly", false, "Don't send test message on connect")
	autoDisconnect = flag.Bool("auto-disconnect", false, "Disconnect when history sync completes")
	unpair         = flag.Bool("unpair", false, "Unpair linked device on exit")
	chats          chatFilter
)

func main() {
	flag.Var(&chats, "chat", "Only store messages from this chat JID (repeatable)")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: test-connect [flags] <phone-number>")
		flag.PrintDefaults()
		os.Exit(1)
	}
	phone := flag.Arg(0)

	msgDB, err := sql.Open("sqlite3", "file:"+*dbFile+"?_foreign_keys=on")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open message db: %v\n", err)
		os.Exit(1)
	}
	defer msgDB.Close()
	if err := initMessageDB(msgDB); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init message db: %v\n", err)
		os.Exit(1)
	}

	logger := waLog.Stdout("Client", "INFO", true)
	container, err := sqlstore.New(context.Background(), "sqlite3",
		"file:"+*dbFile+"?_foreign_keys=on", waLog.Stdout("DB", "ERROR", true))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open store: %v\n", err)
		os.Exit(1)
	}

	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get device: %v\n", err)
		os.Exit(1)
	}

	client := whatsmeow.NewClient(device, logger)

	// Use Chrome TLS fingerprint
	chrome := whatsmeow.NewChromeHTTPClient()
	client.SetWebsocketHTTPClient(chrome)
	client.SetPreLoginHTTPClient(chrome)

	connected := make(chan struct{}, 1)
	var syncTimer *time.Timer
	var syncMu sync.Mutex

	resetSyncTimer := func() {
		if !*autoDisconnect {
			return
		}
		syncMu.Lock()
		defer syncMu.Unlock()
		if syncTimer != nil {
			syncTimer.Reset(30 * time.Second)
		} else {
			syncTimer = time.AfterFunc(30*time.Second, func() {
				fmt.Println("\nHistory sync idle — auto-disconnecting.")
				client.Disconnect()
				os.Exit(0)
			})
		}
	}

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.PairSuccess:
			fmt.Printf("\nPaired as %s\n", v.ID)

		case *events.Connected:
			fmt.Println("\nConnected.")
			select {
			case connected <- struct{}{}:
			default:
			}

		case *events.LoggedOut:
			fmt.Println("\nLogged out.")

		case *events.Message:
			body, mediaType := extractBody(v.Message)
			if wantChat(v.Info.Chat.String()) {
				storeMessage(msgDB, v.Info.ID, v.Info.Chat.String(),
					v.Info.Sender.String(), v.Info.Timestamp,
					body, mediaType, v.Info.IsFromMe, v.Info.IsGroup)
				fmt.Printf("[%s] MSG %s → %s: %s\n",
					v.Info.Timestamp.Format("15:04:05"),
					v.Info.Sender.User, v.Info.Chat.String(), body)
			}

		case *events.HistorySync:
			data := v.Data
			syncType := data.GetSyncType().String()
			conversations := data.GetConversations()
			fmt.Printf("\nHistorySync [%s]: %d conversations\n", syncType, len(conversations))
			stored := 0
			for _, conv := range conversations {
				chatJID := conv.GetID()
				if !wantChat(chatJID) {
					continue
				}
				for _, msg := range conv.GetMessages() {
					info := msg.GetMessage()
					if info == nil {
						continue
					}
					msgProto := info.GetMessage()
					body, mediaType := extractBody(msgProto)
					ts := time.Unix(int64(info.GetMessageTimestamp()), 0)
					isGroup := strings.HasSuffix(chatJID, "@g.us")
					isFromMe := info.GetKey().GetFromMe()
					sender := info.GetKey().GetParticipant()
					if sender == "" {
						sender = info.GetKey().GetRemoteJID()
					}
					if storeMessage(msgDB, info.GetKey().GetID(), chatJID, sender,
						ts, body, mediaType, isFromMe, isGroup) {
						stored++
					}
				}
			}
			fmt.Printf("Stored %d messages from history sync.\n", stored)
			resetSyncTimer()
		}
	})

	if device.ID == nil {
		fmt.Printf("No saved session — requesting pairing code for %s...\n", phone)
		if err := client.Connect(); err != nil {
			fmt.Fprintf(os.Stderr, "connect error: %v\n", err)
			os.Exit(1)
		}
		code, err := client.PairPhone(context.Background(), phone, true,
			whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			fmt.Fprintf(os.Stderr, "pair phone error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n============================\n  PAIRING CODE: %s\n============================\n", code)
		fmt.Println("Enter in WhatsApp: Settings → Linked Devices → Link a Device → Link with Phone Number")
	} else {
		fmt.Printf("Resuming session for %s\n", device.ID)
		if err := client.Connect(); err != nil {
			fmt.Fprintf(os.Stderr, "connect error: %v\n", err)
			os.Exit(1)
		}
	}

	if !*readonly {
		go func() {
			select {
			case <-connected:
			case <-time.After(5 * time.Minute):
				return
			}
			selfJID := types.NewJID(phone, types.DefaultUserServer)
			_ = client.SendChatPresence(context.Background(), selfJID,
				types.ChatPresenceComposing, types.ChatPresenceMediaText)
			time.Sleep(2 * time.Second)
			msg := &waE2E.Message{Conversation: proto.String("whatsbot fork test ✓")}
			resp, err := client.SendMessage(context.Background(), selfJID, msg)
			if err != nil {
				fmt.Printf("send error: %v\n", err)
			} else {
				fmt.Printf("Test message sent. ID: %s\n", resp.ID)
			}
			_ = client.SendChatPresence(context.Background(), selfJID,
				types.ChatPresencePaused, "")
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig

	if *unpair {
		fmt.Println("\nUnpairing...")
		if err := client.Logout(context.Background()); err != nil {
			fmt.Printf("unpair error: %v\n", err)
		}
	} else {
		fmt.Println("\nDisconnecting (session saved)...")
		client.Disconnect()
	}

	count := 0
	_ = msgDB.QueryRow("SELECT COUNT(*) FROM wa_messages").Scan(&count)
	fmt.Printf("Messages stored in %s: %d\n", *dbFile, count)
}

func initMessageDB(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS wa_messages (
			id          TEXT PRIMARY KEY,
			chat_jid    TEXT NOT NULL,
			sender_jid  TEXT NOT NULL,
			timestamp   INTEGER NOT NULL,
			body        TEXT,
			media_type  TEXT,
			is_from_me  INTEGER NOT NULL DEFAULT 0,
			is_group    INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_wamsg_chat ON wa_messages(chat_jid, timestamp);
	`)
	return err
}

func storeMessage(db *sql.DB, id, chatJID, senderJID string, ts time.Time,
	body, mediaType string, isFromMe, isGroup bool) bool {
	fromMe, grp := 0, 0
	if isFromMe {
		fromMe = 1
	}
	if isGroup {
		grp = 1
	}
	_, err := db.Exec(`
		INSERT OR IGNORE INTO wa_messages
			(id, chat_jid, sender_jid, timestamp, body, media_type, is_from_me, is_group)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, senderJID, ts.Unix(), body, mediaType, fromMe, grp)
	return err == nil
}

func extractBody(msg *waE2E.Message) (body, mediaType string) {
	if msg == nil {
		return "", ""
	}
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation(), ""
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetText(), ""
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetCaption(), "image"
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetCaption(), "video"
	case msg.GetAudioMessage() != nil:
		return "", "audio"
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetFileName(), "document"
	case msg.GetStickerMessage() != nil:
		return "", "sticker"
	case msg.GetLocationMessage() != nil:
		return fmt.Sprintf("%.6f,%.6f", msg.GetLocationMessage().GetDegreesLatitude(),
			msg.GetLocationMessage().GetDegreesLongitude()), "location"
	default:
		return "", "other"
	}
}

func wantChat(jid string) bool {
	if len(chats) == 0 {
		return true
	}
	for _, c := range chats {
		if strings.Contains(jid, c) {
			return true
		}
	}
	return false
}
