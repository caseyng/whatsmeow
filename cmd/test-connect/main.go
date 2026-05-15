// test-connect: smoke test and history extractor for the caseyng/whatsmeow fork.
//
// Usage:
//
//	./test-connect [flags] <phone-number>
//
// Flags:
//
//	--db <file>          SQLite file (default: <phone>.db). One file per account.
//	--readonly           Don't send a test message on connect
//	--chat <jid>         Only store messages from this chat JID (repeatable)
//	--auto-disconnect    Disconnect automatically 30s after last history sync event
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
	dbFile         = flag.String("db", "", "SQLite file (default: <phone>.db). One file per account.")
	readonly       = flag.Bool("readonly", false, "Don't send test message on connect")
	autoDisconnect = flag.Bool("auto-disconnect", false, "Disconnect 30s after last history sync event")
	unpairOnExit   = flag.Bool("unpair", false, "Unpair linked device on exit")
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

	dbPath := *dbFile
	if dbPath == "" {
		dbPath = phone + ".db"
	}

	// Open the SQLite file once — whatsmeow session tables and wa_messages
	// share the same connection so there is no double-open or locking conflict.
	// WAL mode reduces write contention; busy_timeout makes SQLite retry for
	// up to 5s instead of returning "database is locked" immediately.
	rawDB, err := sql.Open("sqlite3",
		"file:"+dbPath+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open db %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	defer rawDB.Close()

	dbLogger := waLog.Stdout("DB", "ERROR", true)
	container := sqlstore.NewWithDB(rawDB, "sqlite3", dbLogger)
	if err := container.Upgrade(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "failed to upgrade db: %v\n", err)
		os.Exit(1)
	}
	if err := initMessageTable(rawDB); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init message table: %v\n", err)
		os.Exit(1)
	}

	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get device: %v\n", err)
		os.Exit(1)
	}

	// Guard against session/number mismatch — catch the case where the user
	// points at an existing DB that belongs to a different number.
	if device.ID != nil && device.ID.User != phone {
		fmt.Fprintf(os.Stderr,
			"ERROR: %s already has a session for %s, not %s.\n"+
				"Use --db with a different filename for a different number.\n",
			dbPath, device.ID.User, phone)
		os.Exit(1)
	}

	logger := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(device, logger)

	// Chrome TLS fingerprint on both pre-login and post-login connections
	chrome := whatsmeow.NewChromeHTTPClient()
	client.SetWebsocketHTTPClient(chrome)
	client.SetPreLoginHTTPClient(chrome)

	connected := make(chan struct{}, 1)
	var (
		syncTimer *time.Timer
		syncMu    sync.Mutex
	)

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
				fmt.Println("\nNo history sync activity for 30s — disconnecting.")
				client.Disconnect()
				printCount(rawDB, dbPath)
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
			if !wantChat(v.Info.Chat.String()) {
				return
			}
			body, mediaType := extractBody(v.Message)
			storeMessage(rawDB, v.Info.ID, v.Info.Chat.String(),
				v.Info.Sender.String(), v.Info.Timestamp,
				body, mediaType, v.Info.IsFromMe, v.Info.IsGroup)
			fmt.Printf("[%s] MSG %s → %s: %s\n",
				v.Info.Timestamp.Format("15:04:05"),
				v.Info.Sender.User, v.Info.Chat.String(), body)

		case *events.HistorySync:
			syncType := v.Data.GetSyncType().String()
			conversations := v.Data.GetConversations()
			fmt.Printf("\nHistorySync [%s]: %d conversations\n", syncType, len(conversations))
			stored := 0
			for _, conv := range conversations {
				chatJID := conv.GetID()
				if !wantChat(chatJID) {
					continue
				}
				isGroup := strings.HasSuffix(chatJID, "@g.us")
				for _, msg := range conv.GetMessages() {
					info := msg.GetMessage()
					if info == nil {
						continue
					}
					body, mediaType := extractBody(info.GetMessage())
					ts := time.Unix(int64(info.GetMessageTimestamp()), 0)
					sender := info.GetKey().GetParticipant()
					if sender == "" {
						sender = info.GetKey().GetRemoteJID()
					}
					if storeMessage(rawDB, info.GetKey().GetID(), chatJID, sender,
						ts, body, mediaType, info.GetKey().GetFromMe(), isGroup) {
						stored++
					}
				}
			}
			fmt.Printf("Stored %d messages.\n", stored)
			resetSyncTimer()
		}
	})

	if device.ID == nil {
		fmt.Printf("No session in %s — requesting pairing code for %s...\n", dbPath, phone)
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
		fmt.Printf("Resuming session for %s (db: %s)\n", device.ID.User, dbPath)
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

	if *unpairOnExit {
		fmt.Println("\nUnpairing...")
		if err := client.Logout(context.Background()); err != nil {
			fmt.Printf("unpair error: %v\n", err)
		}
	} else {
		fmt.Println("\nDisconnecting (session saved)...")
		client.Disconnect()
	}

	printCount(rawDB, dbPath)
}

func initMessageTable(db *sql.DB) error {
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
		return fmt.Sprintf("%.6f,%.6f",
			msg.GetLocationMessage().GetDegreesLatitude(),
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

func printCount(db *sql.DB, dbFile string) {
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM wa_messages").Scan(&count)
	fmt.Printf("Messages stored in %s: %d\n", dbFile, count)
}
