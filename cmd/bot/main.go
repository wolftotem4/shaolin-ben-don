package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
	"github.com/wolftotem4/shaolin-ben-don/internal/action"
	"github.com/wolftotem4/shaolin-ben-don/internal/app"
	"github.com/wolftotem4/shaolin-ben-don/internal/conducts"
	database "github.com/wolftotem4/shaolin-ben-don/internal/db"
	"github.com/wolftotem4/shaolin-ben-don/internal/transformers"
	"github.com/wolftotem4/shaolin-ben-don/internal/types/ctrl"
	typesjson "github.com/wolftotem4/shaolin-ben-don/internal/types/json"
)

const (
	helpMsg = `可用指令:
/subscribe 訂閱訊息，有可用項目將發佈至此頻道/私人訊息
/unsubscribe 取消訂閱`
)

type Subscription map[int64]bool

var (
	flagDrain = flag.Bool("drain", false, "drop first item report.")
	flagMock  = flag.String("mock", "", "read items from JSON file.")

	subscribe   = make(chan int64)
	unsubscribe = make(chan int64)
	updateCmd   = make(chan bool)

	// record item IDs in order to avoid duplicate reports
	reported = ctrl.ReportedItems{Items: make(map[string]bool)}
)

func main() {
	ctx := context.Background()

	flag.Parse()

	app, err := app.Register()
	if err != nil {
		log.Fatal(err)
	}

	if err := boot(ctx, app); err != nil {
		log.Fatal(err)
	}

	go func() {
		var (
			subscription Subscription

			// A value generated by session on webpage.
			// Some APIs needs the interface value to work.
			interfaceValue = 0

			// How often to call heartbeat to prevent session from expiring
			heartbeat = time.Tick(3 * time.Minute)

			// How often to trigger update
			updateInterval = time.Tick(app.Config.App.UpdateInterval)

			// Store items in the memory in order to get next expiring items
			pendingItems = ctrl.NewPendingItems(app.Config.App.PriorTime)
		)

		log.Printf("Restoring subscriptions...")
		subscription, err = restoreSubscriptions(ctx, app.DB)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Subscriptions restored.")

		if *flagDrain {
			// prevent spam users by keeping rebooting the program
			items, _ := fetchItems(ctx, app, &interfaceValue)
			pendingItems.Update(items)
		}

		updateFunc := func() {
			var (
				// items for broadcast
				reports []*typesjson.ProgressItem

				// all fetched items
				all []*typesjson.ProgressItem
			)

			if len(subscription) == 0 {
				return
			}

			reports, all = fetchItems(ctx, app, &interfaceValue)
			pendingItems.Update(all)

			if len(reports) > 0 {
				broadcast(subscription, reports, app)

				// delete expiring items in order to prevent duplication of report items.
				pendingItems.ExtractExpiringItems()
			}
		}

		for {
			// update remaining time from ExpiresDate
			pendingItems.UpdateRemainSecondBeforeExpireValues()

			select {
			case chatId := <-subscribe:
				subscription[chatId] = true

				handler := database.Handler{DB: app.DB}
				if err := handler.AddSubscription(ctx, chatId); err != nil {
					log.Fatal(err)
				}

				log.Printf("Chat [%d] subscribed.", chatId)
			case chatId := <-unsubscribe:
				delete(subscription, chatId)

				handler := database.Handler{DB: app.DB}
				if err := handler.DeleteSubscription(ctx, chatId); err != nil {
					log.Fatal(err)
				}

				log.Printf("Chat [%d] unsubscribed.", chatId)
			case <-updateCmd:
				updateFunc()
			case <-updateInterval:
				updateFunc()

				// save cookies to database
				if err := app.RealClient.Store.Save(ctx); err != nil {
					log.Fatal(err)
				}
			case <-pendingItems.Chan():
				// update remaining time from ExpiresDate
				pendingItems.UpdateRemainSecondBeforeExpireValues()

				broadcast(subscription, pendingItems.ExtractExpiringItems(), app)
			case <-heartbeat:
				// call heartbeat to prevent session from expiring
				act := action.HeartbeatAction{Client: app.Client}

				log.Println("Calling heartbeat...")
				if err != act.Call(ctx, interfaceValue) {
					log.Fatal(err)
				}
				log.Println("Heartbeat called.")
			}
		}
	}()

	log.Printf("Authorized on account %s", app.Bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := app.Bot.GetUpdatesChan(u)

	for update := range updates {
		if update.MyChatMember != nil && update.MyChatMember.NewChatMember.User.ID == app.Bot.Self.ID && update.MyChatMember.NewChatMember.HasLeft() {
			unsubscribe <- update.Message.Chat.ID
		}

		if update.Message != nil {
			switch update.Message.Text {
			case "/subscribe":
				subscribe <- update.Message.Chat.ID

				msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("[%s] 已訂閱", getRoomTitleFromMessage(update.Message)))
				app.Bot.Send(msg)
			case "/unsubscribe":
				unsubscribe <- update.Message.Chat.ID

				msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("[%s] 已取消訂閱", getRoomTitleFromMessage(update.Message)))
				app.Bot.Send(msg)
			case "/update":
				updateCmd <- true

				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "立即抓取最新資料.")
				app.Bot.Send(msg)
			case "/start":
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, helpMsg)
				app.Bot.Send(msg)
			}
		}
	}
}

func broadcast(subscription Subscription, items []*typesjson.ProgressItem, app *app.App) {
	var msgText string

	{
		t := transformers.LinkItems{Items: items, Client: app.Client}
		msgText = t.String()
	}

	log.Printf("Broadcasting (%d items)...\n", len(items))
	for chatId := range subscription {
		msg := tgbotapi.NewMessage(chatId, msgText)
		msg.ParseMode = tgbotapi.ModeHTML
		if _, err := app.Bot.Send(msg); err != nil {
			log.Println(err)
		}
	}
	log.Println("Broadcasted.")
}

func fetchItems(ctx context.Context, app *app.App, interfaceValue *int) ([]*typesjson.ProgressItem, []*typesjson.ProgressItem) {
	var (
		items []*typesjson.ProgressItem
	)

	log.Println("Fetching Items...")
	if *flagMock == "" {
		info, err := conducts.GetUnexpiredItems(ctx, app, *interfaceValue)
		if err != nil {
			log.Fatal(err)
		}
		*interfaceValue = info.Interface

		items = info.Items
	} else {
		var err error
		if items, err = readMockFile(); err != nil {
			log.Fatal(err)
		}
	}
	log.Println("Items fetched.")

	// filter out reported items to prevent duplications
	reports := reported.ExtractUnreported(items)
	reported.MarkReported(items)

	return reports, items
}

func boot(ctx context.Context, app *app.App) error {
	if err := createTable(ctx, app.DB.DB); err != nil {
		return errors.WithStack(err)
	}

	if err := app.RealClient.Load(ctx); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func createTable(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	s, err := os.ReadFile("data/create_tables.sql")
	if err != nil {
		return errors.WithStack(err)
	}

	_, err = db.ExecContext(ctx, string(s))
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func restoreSubscriptions(ctx context.Context, db *sqlx.DB) (Subscription, error) {
	handler := database.Handler{DB: db}
	keys, err := handler.GetSubscriptions(ctx)
	if err != nil {
		return Subscription{}, errors.WithStack(err)
	}

	subscription := make(Subscription)
	for _, key := range keys {
		subscription[key] = true
	}

	return subscription, nil
}

func getRoomTitleFromMessage(msg *tgbotapi.Message) string {
	if msg.Chat.Title != "" {
		return msg.Chat.Title
	}
	return msg.From.UserName
}
