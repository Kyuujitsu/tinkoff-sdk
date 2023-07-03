package main

import (
	"context"
	"github.com/tinkoff/invest-api-go-sdk/examples/interval_bot/internal/bot"
	"github.com/tinkoff/invest-api-go-sdk/investgo"
	pb "github.com/tinkoff/invest-api-go-sdk/proto"
	"go.uber.org/zap"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	// SHARES_NUM - Количество акций для торгов
	SHARES_NUM = 30
	// EXCHANGE - Биржа на которой будет работать бот
	EXCHANGE = "MOEX"
)

func main() {
	// загружаем конфигурацию для сдк из .yaml файла
	sdkConfig, err := investgo.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("config loading error %v", err.Error())
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// сдк использует для внутреннего логирования investgo.Logger
	// для примера передадим uber.zap
	prod := zap.NewExample()
	defer func() {
		err := prod.Sync()
		if err != nil {
			log.Printf("Prod.Sync %v", err.Error())
		}
	}()
	if err != nil {
		log.Fatalf("logger creating error %v", err)
	}
	logger := prod.Sugar()
	// создаем клиента для investAPI, он позволяет создавать нужные сервисы и уже
	// через них вызывать нужные методы
	client, err := investgo.NewClient(ctx, sdkConfig, logger)
	if err != nil {
		logger.Fatalf("client creating error %v", err.Error())
	}
	defer func() {
		logger.Infof("closing client connection")
		err := client.Stop()
		if err != nil {
			logger.Errorf("client shutdown error %v", err.Error())
		}
	}()

	// для создания стратеги нужно ее сконфигурировать, для этого получим список идентификаторов инструментов,
	// которыми предстоит торговать
	insrtumentsService := client.NewInstrumentsServiceClient()
	// получаем список акций доступных для торговли через investAPI
	instrumentsResp, err := insrtumentsService.Shares(pb.InstrumentStatus_INSTRUMENT_STATUS_BASE)
	if err != nil {
		logger.Errorf(err.Error())
	}
	// слайс идентификаторов торговых инструментов instrument_uid
	// акции с московской биржи
	instrumentIds := make([]string, 0, 300)
	shares := instrumentsResp.GetInstruments()
	for _, share := range shares {
		if len(instrumentIds) > SHARES_NUM-1 {
			break
		}
		if share.GetExchange() == EXCHANGE {
			instrumentIds = append(instrumentIds, share.GetUid())
		}
	}
	logger.Infof("got %v instruments\n", len(instrumentIds))

	intervalConfig := bot.IntervalStrategyConfig{}
	// создание бота на стакане
	intervalBot, err := bot.NewBot(ctx, client, intervalConfig)
	if err != nil {
		logger.Fatalf("interval bot creating fail %v", err.Error())
	}

	wg := &sync.WaitGroup{}
	// Таймер для Московской биржи, отслеживает расписание и дает сигналы, на остановку/запуск бота
	// cancelAhead - Событие STOP будет отправлено в канал за cancelAhead до конца торгов
	cancelAhead := time.Minute * 5
	t := investgo.NewTimer(client, "MOEX", cancelAhead)

	// запуск таймера
	wg.Add(1)
	go func(ctx context.Context) {
		defer wg.Done()
		err := t.Start(ctx)
		if err != nil {
			logger.Errorf(err.Error())
		}
	}(ctx)

	// по сигналам останавливаем таймер
	go func() {
		<-sigs
		t.Stop()
	}()

	// чтение событий от таймера и управление ботом
	events := t.Events()
	wg.Add(1)
	go func(ctx context.Context) {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				logger.Infof("got event = %v", ev)
				switch ev {
				case investgo.START:
					// запуск бота
					wg.Add(1)
					go func() {
						defer wg.Done()
						err = intervalBot.Run()
						if err != nil {
							logger.Errorf(err.Error())
						}
					}()
				case investgo.STOP:
					// остановка бота
					intervalBot.Stop()
				}
			}
		}
	}(ctx)

	wg.Wait()
}
