package scheduledevents2

//go:generate sqlboiler psql

import (
	"context"
	"encoding/json"
	"github.com/jonas747/discordgo"
	"github.com/jonas747/yagpdb/bot"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/scheduledevents2/models"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/volatiletech/sqlboiler/boil"
	"github.com/volatiletech/sqlboiler/queries/qm"
	"math/rand"
	"reflect"
	"sync"
	"time"
)

type ScheduledEvents struct {
	stop chan *sync.WaitGroup

	currentlyProcessingMU sync.Mutex
	currentlyProcessing   map[int64]bool
}

func newScheduledEventsPlugin() *ScheduledEvents {
	return &ScheduledEvents{
		stop:                make(chan *sync.WaitGroup),
		currentlyProcessing: make(map[int64]bool),
	}
}

func RegisterPlugin() {
	err := InitSchema()
	if err != nil {
		logrus.WithError(err).Fatal("scheduledevents failed initializing db schema")
	}

	common.RegisterPlugin(newScheduledEventsPlugin())
}

func InitSchema() error {
	_, err := common.PQ.Exec(DBSchema)
	return err
}

func (se *ScheduledEvents) Name() string {
	return "scheduled_events"
}

type HandlerFunc func(evt *models.ScheduledEvent, data interface{}) (retry bool, err error)

type RegisteredHandler struct {
	EvtName    string
	DataFormat interface{}
	Handler    HandlerFunc
}

var (
	registeredHandlers = make(map[string]*RegisteredHandler)
)

// RegisterHandler registers a handler for the scpecified event name
// dataFormat is optional and should not be a pointer, it should match the type you're passing into ScheduleEvent
func RegisterHandler(eventName string, dataFormat interface{}, handler HandlerFunc) {
	registeredHandlers[eventName] = &RegisteredHandler{
		EvtName:    eventName,
		DataFormat: dataFormat,
		Handler:    handler,
	}

	logrus.Debug("[scheduledEvents2] Registered handler for ", eventName)
}

func ScheduleEvent(evtName string, guildID int64, runAt time.Time, data interface{}) error {
	m := &models.ScheduledEvent{
		TriggersAt: runAt,
		EventName:  evtName,
		GuildID:    guildID,
	}

	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return errors.WithMessage(err, "marshal")
		}

		m.Data = b
	} else {
		m.Data = []byte("{}")
	}

	err := m.InsertG(context.Background(), boil.Infer())
	return errors.WithMessage(err, "insert")
}

var _ bot.BotStartedHandler = (*ScheduledEvents)(nil)
var _ bot.BotStopperHandler = (*ScheduledEvents)(nil)

func (se *ScheduledEvents) BotStarted() {
	go se.runCheckLoop()
	go se.MigrateLegacyEvents()
}

func (se *ScheduledEvents) StopBot(wg *sync.WaitGroup) {
	se.stop <- wg
}

func (se *ScheduledEvents) runCheckLoop() {
	t := time.NewTicker(time.Second)
	for {
		select {
		case wg := <-se.stop:
			wg.Done()
			return
		case <-t.C:
			se.check()
		}
	}
}

func (se *ScheduledEvents) check() {
	se.currentlyProcessingMU.Lock()
	defer se.currentlyProcessingMU.Unlock()

	toProcess, err := models.ScheduledEvents(qm.Where("triggers_at < now() AND processed=false")).AllG(context.Background())
	if err != nil {
		logrus.WithError(err).Error("[scheduledevents2] failed checking for events to process")
		return
	}

	numSkipped := 0
	numHandling := 0
	for _, p := range toProcess {
		if !bot.IsGuildOnCurrentProcess(p.GuildID) {
			numSkipped++
			continue
		}

		if v := se.currentlyProcessing[p.ID]; v {
			continue
		}

		numHandling++

		se.currentlyProcessing[p.ID] = true
		go se.processItem(p)
	}

	if numHandling > 0 {
		logrus.Info("[scheduledevents2] triggered ", numHandling, " scheduled events (skipped ", numSkipped, ")")
	}
}

func (se *ScheduledEvents) processItem(item *models.ScheduledEvent) {
	l := logrus.WithField("id", item.ID).WithField("evt", item.EventName)

	handler, ok := registeredHandlers[item.EventName]
	if !ok {
		l.Error("[scheduledevents2] unknown event: ", item.EventName)
		se.markDone(item)
		return
	}

	var decodedData interface{}
	if handler.DataFormat != nil {
		typ := reflect.TypeOf(handler.DataFormat)

		// Decode the form into the destination struct
		decodedData = reflect.New(typ).Interface()
		err := json.Unmarshal(item.Data, decodedData)
		if err != nil {
			l.WithError(err).Error("[scheduledevents2] failed decoding event data")
			se.markDone(item)
			return
		}
	}

	for {
		retry, err := handler.Handler(item, decodedData)
		if err != nil {
			l.WithError(err).Error("[scheduledevents2] handler returned an error")
		}

		if retry {
			l.WithError(err).Warn("[scheduledevents2] retrying handling event")
			time.Sleep(time.Second * time.Duration(rand.Intn(10)+5))
			continue
		}

		se.markDone(item)
		break
	}
}

func (se *ScheduledEvents) markDone(item *models.ScheduledEvent) {
	item.Processed = true
	_, err := item.UpdateG(context.Background(), boil.Whitelist("processed"))

	se.currentlyProcessingMU.Lock()
	delete(se.currentlyProcessing, item.ID)
	se.currentlyProcessingMU.Unlock()

	if err != nil {
		logrus.WithError(err).Error("[scheduledevents2] failed marking item as processed")
	}
}

func CheckDiscordErrRetry(err error) bool {
	if cast, ok := err.(*discordgo.RESTError); ok {
		if cast.Message != nil && cast.Message.Code != 0 {
			// proper discord response, don't retry
			return false
		}
	}

	// an unknown error unrelated to the discord api occured (503's for example) attempt a retry
	return true
}