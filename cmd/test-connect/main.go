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
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
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
	backfillChat   = flag.String("backfill-chat", "", "Fetch all available history for this chat JID")
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

	connected   := make(chan struct{}, 1)
	onDemandCh  := make(chan int, 1) // backfill loop: messages returned per ON_DEMAND batch
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
			if r := v.Message.GetReactionMessage(); r != nil {
				if wantChat(v.Info.Chat.String()) {
					storeReaction(rawDB,
						r.GetKey().GetID(), v.Info.Chat.String(),
						v.Info.Sender.String(), r.GetText(),
						r.GetSenderTimestampMS())
				}
				return
			}
			if p := v.Message.GetProtocolMessage(); p != nil &&
				p.GetType() == waE2E.ProtocolMessage_REVOKE {
				markMessageDeleted(rawDB, p.GetKey().GetID())
				return
			}
			if !wantChat(v.Info.Chat.String()) {
				return
			}
			body, mediaType := extractBody(v.Message)
			storeMessage(rawDB, v.Info.ID, v.Info.Chat.String(),
				v.Info.Sender.String(), v.Info.Timestamp,
				body, mediaType, "", v.Info.PushName,
				v.Info.IsFromMe, v.Info.IsGroup, false)
			fmt.Printf("[%s] MSG %s → %s: %s\n",
				v.Info.Timestamp.Format("15:04:05"),
				v.Info.Sender.User, v.Info.Chat.String(), body)

		case *events.HistorySync:
			isOnDemand := v.Data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND
			syncType := v.Data.GetSyncType().String()
			conversations := v.Data.GetConversations()
			fmt.Printf("\nHistorySync [%s]: %d conversations\n", syncType, len(conversations))
			stored, returned := 0, 0
			for _, conv := range conversations {
				chatJID := conv.GetID()
				isGroup := strings.HasSuffix(chatJID, "@g.us")
				name := conv.GetName()
				if name == "" {
					name = conv.GetDisplayName()
				}
				upsertChat(rawDB, chatJID, name, conv.GetDescription(),
					isGroup, conv.GetArchived(), conv.GetPinned() > 0,
					conv.GetLastMsgTimestamp(), conv.GetCreatedAt(),
					conv.GetCreatedBy(), conv.GetEphemeralExpiration())
				for _, p := range conv.GetParticipant() {
					upsertGroupMember(rawDB, chatJID, p.GetUserJID(), p.GetRank().String())
				}
				for _, msg := range conv.GetMessages() {
					info := msg.GetMessage()
					if info == nil {
						continue
					}
					returned++
					// ON_DEMAND responses bypass the --chat filter: we asked for this chat explicitly.
					if !isOnDemand && !wantChat(chatJID) {
						continue
					}
					msgID := info.GetKey().GetID()
					body, mediaType := extractBody(info.GetMessage())
					ts := time.Unix(int64(info.GetMessageTimestamp()), 0)
					sender := info.GetKey().GetParticipant()
					if sender == "" {
						sender = info.GetKey().GetRemoteJID()
					}
					isDeleted := info.GetRevokeMessageTimestamp() > 0
					if storeMessage(rawDB, msgID, chatJID, sender, ts,
						body, mediaType,
						info.GetStatus().String(), info.GetPushName(),
						info.GetKey().GetFromMe(), isGroup, isDeleted) {
						stored++
					}
					for _, r := range info.GetReactions() {
						reactorJID := r.GetKey().GetParticipant()
						if reactorJID == "" {
							reactorJID = r.GetKey().GetRemoteJID()
						}
						storeReaction(rawDB, msgID, chatJID, reactorJID, r.GetText(), r.GetSenderTimestampMS())
					}
				}
			}
			fmt.Printf("Stored %d messages.\n", stored)
			resetSyncTimer()
			if isOnDemand {
				select {
				case onDemandCh <- returned:
				default:
				}
			}
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

	if *backfillChat != "" {
		go func() {
			select {
			case <-connected:
			case <-time.After(5 * time.Minute):
				fmt.Println("Backfill: timed out waiting for connection")
				return
			}
			// Give the initial reconnect sync a moment to settle.
			time.Sleep(3 * time.Second)
			fmt.Printf("Backfill: starting history fetch for %s\n", *backfillChat)
			total := 0
			for {
				info, err := oldestMessageInfo(rawDB, *backfillChat)
				if err == sql.ErrNoRows {
					fmt.Println("Backfill: no messages in DB for this chat yet — waiting for initial sync")
					time.Sleep(5 * time.Second)
					continue
				}
				if err != nil {
					fmt.Printf("Backfill: DB error: %v\n", err)
					return
				}
				fmt.Printf("Backfill: requesting 50 messages before [%s] @ %s\n",
					info.ID, info.Timestamp.Format("2006-01-02 15:04:05"))
				req := client.BuildHistorySyncRequest(&info, 50)
				if _, err := client.SendPeerMessage(context.Background(), req); err != nil {
					fmt.Printf("Backfill: send failed: %v\n", err)
					return
				}
				select {
				case n := <-onDemandCh:
					total += n
					if n == 0 {
						fmt.Printf("Backfill: complete — %d total messages fetched for %s\n", total, *backfillChat)
						printCount(rawDB, dbPath)
						return
					}
					fmt.Printf("Backfill: +%d messages (running total: %d)\n", n, total)
				case <-time.After(30 * time.Second):
					fmt.Println("Backfill: timed out waiting for ON_DEMAND response")
					return
				}
			}
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
			id         TEXT PRIMARY KEY,
			chat_jid   TEXT NOT NULL,
			sender_jid TEXT NOT NULL,
			timestamp  INTEGER NOT NULL,
			body       TEXT,
			media_type TEXT,
			status     TEXT,
			push_name  TEXT,
			is_from_me INTEGER NOT NULL DEFAULT 0,
			is_group   INTEGER NOT NULL DEFAULT 0,
			is_deleted INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_wamsg_chat ON wa_messages(chat_jid, timestamp);

		CREATE TABLE IF NOT EXISTS wa_chats (
			jid                  TEXT PRIMARY KEY,
			name                 TEXT,
			description          TEXT,
			is_group             INTEGER NOT NULL DEFAULT 0,
			archived             INTEGER NOT NULL DEFAULT 0,
			pinned               INTEGER NOT NULL DEFAULT 0,
			last_msg_ts          INTEGER,
			ephemeral_expiration INTEGER NOT NULL DEFAULT 0,
			created_at           INTEGER,
			created_by           TEXT
		);

		CREATE TABLE IF NOT EXISTS wa_group_members (
			chat_jid   TEXT NOT NULL,
			member_jid TEXT NOT NULL,
			rank       TEXT NOT NULL DEFAULT 'regular',
			PRIMARY KEY (chat_jid, member_jid)
		);

		CREATE TABLE IF NOT EXISTS wa_reactions (
			message_id  TEXT NOT NULL,
			chat_jid    TEXT NOT NULL,
			reactor_jid TEXT NOT NULL,
			emoji       TEXT NOT NULL,
			timestamp   INTEGER NOT NULL,
			PRIMARY KEY (message_id, reactor_jid)
		);
	`)
	if err != nil {
		return err
	}
	migrations := []string{
		`ALTER TABLE wa_messages ADD COLUMN status TEXT`,
		`ALTER TABLE wa_messages ADD COLUMN push_name TEXT`,
		`ALTER TABLE wa_messages ADD COLUMN is_deleted INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE wa_chats ADD COLUMN description TEXT`,
		`ALTER TABLE wa_chats ADD COLUMN archived INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE wa_chats ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE wa_chats ADD COLUMN last_msg_ts INTEGER`,
		`ALTER TABLE wa_chats ADD COLUMN ephemeral_expiration INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE wa_chats ADD COLUMN created_at INTEGER`,
		`ALTER TABLE wa_chats ADD COLUMN created_by TEXT`,
	}
	for _, m := range migrations {
		db.Exec(m)
	}
	return nil
}

func upsertChat(db *sql.DB,
	jid, name, description string,
	isGroup, archived, pinned bool,
	lastMsgTS, createdAt uint64,
	createdBy string,
	ephemeralExpiration uint32) {

	grp, arch, pin := 0, 0, 0
	if isGroup {
		grp = 1
	}
	if archived {
		arch = 1
	}
	if pinned {
		pin = 1
	}
	db.Exec(`
		INSERT INTO wa_chats
			(jid, name, description, is_group, archived, pinned,
			 last_msg_ts, ephemeral_expiration, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			name                 = excluded.name,
			description          = excluded.description,
			archived             = excluded.archived,
			pinned               = excluded.pinned,
			last_msg_ts          = excluded.last_msg_ts,
			ephemeral_expiration = excluded.ephemeral_expiration,
			created_at           = excluded.created_at,
			created_by           = excluded.created_by`,
		jid, name, description, grp, arch, pin,
		lastMsgTS, ephemeralExpiration, createdAt, createdBy)
}

func upsertGroupMember(db *sql.DB, chatJID, memberJID, rank string) {
	db.Exec(`
		INSERT INTO wa_group_members (chat_jid, member_jid, rank) VALUES (?, ?, ?)
		ON CONFLICT(chat_jid, member_jid) DO UPDATE SET rank = excluded.rank`,
		chatJID, memberJID, rank)
}

func storeReaction(db *sql.DB, messageID, chatJID, reactorJID, emoji string, timestampMS int64) {
	if emoji == "" {
		db.Exec(`DELETE FROM wa_reactions WHERE message_id = ? AND reactor_jid = ?`,
			messageID, reactorJID)
		return
	}
	db.Exec(`
		INSERT INTO wa_reactions (message_id, chat_jid, reactor_jid, emoji, timestamp)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(message_id, reactor_jid) DO UPDATE SET
			emoji     = excluded.emoji,
			timestamp = excluded.timestamp`,
		messageID, chatJID, reactorJID, emoji, timestampMS/1000)
}

func markMessageDeleted(db *sql.DB, messageID string) {
	db.Exec(`UPDATE wa_messages SET is_deleted = 1 WHERE id = ?`, messageID)
}

func storeMessage(db *sql.DB, id, chatJID, senderJID string, ts time.Time,
	body, mediaType, status, pushName string,
	isFromMe, isGroup, isDeleted bool) bool {

	fromMe, grp, del := 0, 0, 0
	if isFromMe {
		fromMe = 1
	}
	if isGroup {
		grp = 1
	}
	if isDeleted {
		del = 1
	}
	_, err := db.Exec(`
		INSERT OR IGNORE INTO wa_messages
			(id, chat_jid, sender_jid, timestamp, body, media_type,
			 status, push_name, is_from_me, is_group, is_deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, senderJID, ts.Unix(),
		body, mediaType, status, pushName,
		fromMe, grp, del)
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

// oldestMessageInfo returns a MessageInfo for the oldest message stored for a
// given chat JID. Used by the backfill loop to request older history.
func oldestMessageInfo(db *sql.DB, chatJID string) (types.MessageInfo, error) {
	var id string
	var ts int64
	var isFromMe int
	err := db.QueryRow(`
		SELECT id, timestamp, is_from_me FROM wa_messages
		WHERE chat_jid = ? ORDER BY timestamp ASC LIMIT 1`, chatJID).
		Scan(&id, &ts, &isFromMe)
	if err != nil {
		return types.MessageInfo{}, err
	}
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return types.MessageInfo{}, err
	}
	return types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     jid,
			IsFromMe: isFromMe == 1,
		},
		ID:        id,
		Timestamp: time.Unix(ts, 0),
	}, nil
}
