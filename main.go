package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var client *whatsmeow.Client

type Event struct {
	Tanggal    string
	Notifikasi string
	Keterangan string
}

var loc *time.Location

func TimeNowLocal() time.Time {
	return time.Now().In(loc)
}

func main() {
	loc, _ = time.LoadLocation("Asia/Jakarta")
	dbLog := waLog.Stdout("Database", "DEBUG", true)
	container, err := sqlstore.New("sqlite3", "file:sqlite3.db?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}
	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		panic(err)
	}
	clientLog := waLog.Stdout("Client", "DEBUG", true)
	client = whatsmeow.NewClient(deviceStore, clientLog)
	client.AddEventHandler(eventHandler)

	if client.Store.ID == nil {
		// No ID stored, new login
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else {
				fmt.Println("Login event:", evt.Event)
			}
		}
	} else {
		err = client.Connect()
		if err != nil {
			panic(err)
		}
	}

	go startScheduler()

	// Listen to Ctrl+C (you can also do something else that prevents the program from exiting)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	client.Disconnect()
}

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		handlerMassage(v)
	}
}

func handlerMassage(v *events.Message) {
	jid := types.NewJID(v.Info.Sender.User, "s.whatsapp.net")
	var message string

	if v.Message.GetConversation() != "" {
		message = v.Message.GetConversation()
	} else if v.Message.GetExtendedTextMessage() != nil {
		message = v.Message.GetExtendedTextMessage().GetText()
	}
	message = strings.TrimSpace(message)

	if message == "/set" {
		sendMessage := "Silahkan masukan format seperti berikut:\nTanggal: DD-MM-YYYY\nNotifikasi: HH:MM\nKeterangan: isi"
		HandleSendMessage(jid, sendMessage)
		return
	}

	if strings.HasPrefix(message, "Tanggal: ") {
		event, err := parseEventMessage(message)
		if err != nil {
			HandleSendMessage(jid, "Format tidak sesuai, silahkan cek kembali '/set'")
			return
		}

		tanggalAcara, err := time.Parse("02-01-2006", event.Tanggal)
		if err != nil {
			sendMessage := "Tanggal tidak valid: " + err.Error()
			HandleSendMessage(jid, sendMessage)
			return
		}
		if tanggalAcara.Before(TimeNowLocal().AddDate(0, 0, -1)) {
			sendMessage := "Tanggal acara tidak boleh sebelum hari ini"
			HandleSendMessage(jid, sendMessage)
			return
		}

		err = HandleSaveToDB(event, jid)
		if err != nil {
			sendMessage := "Gagal menyimpan acara: " + err.Error()
			HandleSendMessage(jid, sendMessage)
			return
		}

		sendMessage := "Acara berhasil disimpan"
		HandleSendMessage(jid, sendMessage)
	}
}

func parseEventMessage(message string) (Event, error) {
	lines := strings.Split(message, "\n")
	if len(lines) < 3 {
		return Event{}, fmt.Errorf("format pesan tidak sesuai, kurang data")
	}

	event := Event{}
	for _, line := range lines {
		if strings.HasPrefix(line, "Tanggal: ") {
			event.Tanggal = strings.TrimSpace(strings.TrimPrefix(line, "Tanggal: "))
		} else if strings.HasPrefix(line, "Notifikasi: ") {
			event.Notifikasi = strings.TrimSpace(strings.TrimPrefix(line, "Notifikasi: "))
		} else if strings.HasPrefix(line, "Keterangan: ") {
			event.Keterangan = strings.TrimSpace(strings.TrimPrefix(line, "Keterangan: "))
		}
	}

	if event.Tanggal == "" || event.Notifikasi == "" || event.Keterangan == "" {
		return Event{}, fmt.Errorf("format pesan tidak sesuai, data tidak lengkap")
	}

	return event, nil
}

func HandleSendMessage(jid types.JID, sendMessage string) {
	_, err := client.SendMessage(context.Background(), jid, &waProto.Message{
		Conversation: proto.String(sendMessage),
	})
	if err != nil {
		fmt.Println(err)
	}
}

func HandleSaveToDB(event Event, JID types.JID) error {
	db, err := sql.Open("sqlite3", "sqlite3.db")

	if err != nil {
		panic(err)
	}

	defer db.Close()
	createTableSQL := `CREATE TABLE IF NOT EXISTS events (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_jid TEXT NOT NULL,
        tanggal TEXT NOT NULL,
        notifikasi TEXT NOT NULL,
        keterangan TEXT NOT NULL
    );`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Table created successfully")

	print(event.Tanggal + " " + event.Notifikasi)

	_, err = db.Exec("INSERT INTO events (user_jid, tanggal, notifikasi, keterangan) VALUES (?, ?, ?, ?)",
		JID, event.Tanggal, event.Notifikasi, event.Keterangan)
	if err != nil {
		return err
	}

	return nil
}

func startScheduler() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			HandlerCheckAndSendReminder()
		}
	}
}

func HandlerCheckAndSendReminder() {
	db, err := sql.Open("sqlite3", "sqlite3.db")
	if err != nil {
		log.Println("Error opening database:", err)
		return
	}
	defer db.Close()

	// Query events for the current time in the correct format
	print(TimeNowLocal().Format("02-01-2006 15:04"))
	rows, err := db.Query("SELECT user_jid, keterangan FROM events WHERE tanggal = ? AND notifikasi = ?", TimeNowLocal().Format("02-01-2006"), TimeNowLocal().Format("15:04"))
	if err != nil {
		log.Println("Error querying database:", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var userJID string
		var keterangan string

		if err := rows.Scan(&userJID, &keterangan); err != nil {
			log.Println("Error scanning row:", err)
			continue
		}

		sendMessage := fmt.Sprintf("Pengingat Acara:\nKeterangan: %s", keterangan)
		HandleSendMessage(types.NewJID(userJID, "s.whatsapp.net"), sendMessage)
	}

	if err := rows.Err(); err != nil {
		log.Println("Error iterating rows:", err)
	}
}
