package bot

import (
	"context"
	"fmt"
	"github.com/tinkoff/invest-api-go-sdk/investgo"
	pb "github.com/tinkoff/invest-api-go-sdk/proto"
	"math"
	"sync"
	"time"
)

type InstrumentState int

const (
	// OUT_OF_STOCK - Нет открытых позиций по этому инструменту
	OUT_OF_STOCK InstrumentState = iota
	// IN_STOCK - Есть открытая позиция по этому инструменту
	IN_STOCK
	// TRY_TO_BUY - Выставлена лимитная заявка на покупку этого инструмента
	TRY_TO_BUY
	// TRY_TO_SELL - Выставлена лимитная заявка на продажу этого инструмента
	TRY_TO_SELL
)

// State - Текущее состояние торгового инструмента
type State struct {
	// instrumentState - Текущее состояние торгового инструмента
	instrumentState InstrumentState
	// orderId - Идентификатор выставленного биржевого поручения. Используется только при
	// state = TRY_TO_BUY или TRY_TO_SELL
	orderId string
}

// States - Состояния инструментов, с которыми работает исполнитель
type States struct {
	mx sync.Mutex
	s  map[string]State
}

func NewStates() *States {
	return &States{
		s: make(map[string]State, 0),
	}
}

// Update - Обновление состояния инструмента
func (s *States) Update(id string, st State) {
	s.mx.Lock()
	s.s[id] = st
	s.mx.Unlock()
}

// Get - Получение состояния инструмента
func (s *States) Get(id string) (State, bool) {
	s.mx.Lock()
	defer s.mx.Unlock()
	state, ok := s.s[id]
	return state, ok
}

type Instrument struct {
	// quantity - Количество лотов, которое покупает/продает исполнитель за 1 поручение
	quantity int64
	// lot - Лотность инструмента
	lot int32
	// currency - Код валюты инструмента
	currency string
	// minPriceInc - Минимальный шаг цены
	minPriceInc *pb.Quotation
	// entryPrice - После открытия позиции, сохраняется цена этой сделки
	entryPrice float64
}

// Executor - Вызывается ботом и исполняет торговые поручения
type Executor struct {
	// instruments - Инструменты, которыми торгует исполнитель
	instruments map[string]Instrument

	wg     *sync.WaitGroup
	cancel context.CancelFunc

	// lastPrices - Текущие позиции на счете, обновляются через стрим сервиса операций
	positions         *Positions
	instrumentsStates *States

	client            *investgo.Client
	ordersService     *investgo.OrdersServiceClient
	operationsService *investgo.OperationsServiceClient
}

func NewExecutor(ctx context.Context, c *investgo.Client, ids map[string]Instrument) *Executor {
	ctxExecutor, cancel := context.WithCancel(ctx)
	wg := &sync.WaitGroup{}

	e := &Executor{
		instruments:       ids,
		positions:         NewPositions(),
		instrumentsStates: NewStates(),
		wg:                wg,
		cancel:            cancel,
		client:            c,
		ordersService:     c.NewOrdersServiceClient(),
		operationsService: c.NewOperationsServiceClient(),
	}
	// Сразу запускаем исполнителя из его же конструктора
	e.start(ctxExecutor)
	return e
}

// BuyLimit - Выставление лимитного торгового поручения на покупку инструмента с uid = id по цене ближайшей к price
func (e *Executor) BuyLimit(id string, price float64) error {
	currentInstrument, ok := e.instruments[id]
	if !ok {
		return fmt.Errorf("instrument %v not found in executor map", id)
	}
	if !e.possibleToBuy(id, price) {
		return nil
	}
	resp, err := e.ordersService.Buy(&investgo.PostOrderRequestShort{
		InstrumentId: id,
		Quantity:     currentInstrument.quantity,
		Price:        floatToQuotation(price, currentInstrument.minPriceInc),
		AccountId:    e.client.Config.AccountId,
		OrderType:    pb.OrderType_ORDER_TYPE_LIMIT,
		OrderId:      investgo.CreateUid(),
	})
	if err != nil {
		return err
	}
	e.instrumentsStates.Update(id, State{
		instrumentState: TRY_TO_BUY,
		orderId:         resp.GetOrderId(),
	})
	return nil
}

// possibleToBuy - Проверка свободного баланса денежных средств на счете, для покупки инструмента c uid = id по цене price
func (e *Executor) possibleToBuy(id string, price float64) bool {
	currentInstrument, ok := e.instruments[id]
	if !ok {
		e.client.Logger.Infof("instrument %v not found in executor map", id)
		return false
	}
	required := price * float64(currentInstrument.quantity) * float64(currentInstrument.lot)
	positionMoney := e.positions.Get().GetMoney()
	var moneyInFloat float64
	for _, pm := range positionMoney {
		m := pm.GetAvailableValue()
		if m.GetCurrency() == currentInstrument.currency {
			moneyInFloat = m.ToFloat()
		}
	}

	if moneyInFloat < required {
		e.client.Logger.Infof("executor: not enough money to buy order with id = %v", id)
	}
	return moneyInFloat > required
}

// SellLimit - Выставление лимитного торгового поручения на продажу инструмента с uid = id по цене ближайшей к price
func (e *Executor) SellLimit(id string, price float64) error {
	currentInstrument, ok := e.instruments[id]
	if !ok {
		return fmt.Errorf("instrument %v not found in executor map", id)
	}
	st, ok := e.instrumentsStates.Get(id)
	if !ok {
		e.client.Logger.Infof("%v not found in instrumentStates", id)
		return nil
	}
	if st.instrumentState != IN_STOCK {
		e.client.Logger.Infof("sell limit fail %v not in stock", id)
		return nil
	}
	resp, err := e.ordersService.Sell(&investgo.PostOrderRequestShort{
		InstrumentId: id,
		Quantity:     currentInstrument.quantity,
		Price:        floatToQuotation(price, currentInstrument.minPriceInc),
		AccountId:    e.client.Config.AccountId,
		OrderType:    pb.OrderType_ORDER_TYPE_LIMIT,
		OrderId:      investgo.CreateUid(),
	})
	if err != nil {
		return err
	}
	e.instrumentsStates.Update(id, State{
		instrumentState: TRY_TO_SELL,
		orderId:         resp.GetOrderId(),
	})
	return nil
}

// CancelLimit - Отмена текущего лимитного поручения, если оно есть, для инструмента с uid = id
func (e *Executor) CancelLimit(id string) error {
	state, ok := e.instrumentsStates.Get(id)
	if !ok {
		return fmt.Errorf("%v not found in instruments states", id)
	}
	if state.instrumentState == IN_STOCK || state.instrumentState == OUT_OF_STOCK {
		return fmt.Errorf("invalid instrument state")
	}
	_, err := e.ordersService.CancelOrder(e.client.Config.AccountId, state.orderId)
	if err != nil {
		return err
	}
	var newState InstrumentState
	switch state.instrumentState {
	case TRY_TO_SELL:
		newState = IN_STOCK
	case TRY_TO_BUY:
		newState = OUT_OF_STOCK
	}
	e.instrumentsStates.Update(id, State{
		instrumentState: newState,
	})
	return nil
}

// ReplaceLimit - Изменение цены лимитного торгового поручения, если оно есть, для инструмента с uid = id
func (e *Executor) ReplaceLimit(id string, price float64) error {
	currentInstrument, ok := e.instruments[id]
	if !ok {
		return fmt.Errorf("instrument %v not found in executor map", id)
	}
	state, ok := e.instrumentsStates.Get(id)
	if !ok {
		return fmt.Errorf("%v not found in instruments states", id)
	}
	if state.instrumentState == IN_STOCK || state.instrumentState == OUT_OF_STOCK {
		return fmt.Errorf("invalid instrument state")
	}
	resp, err := e.ordersService.ReplaceOrder(&investgo.ReplaceOrderRequest{
		AccountId:  e.client.Config.AccountId,
		OrderId:    state.orderId,
		NewOrderId: investgo.CreateUid(),
		Quantity:   currentInstrument.quantity,
		Price:      floatToQuotation(price, currentInstrument.minPriceInc),
		PriceType:  pb.PriceType_PRICE_TYPE_CURRENCY,
	})
	if err != nil {
		return err
	}
	// обновляем orderId в статусе инструмента
	e.instrumentsStates.Update(id, State{
		instrumentState: state.instrumentState,
		orderId:         resp.GetOrderId(),
	})
	return nil
}

// floatToQuotation - Перевод float в Quotation
func floatToQuotation(number float64, step *pb.Quotation) *pb.Quotation {
	// делим дробь на дробь и округляем до ближайшего целого
	k := math.Round(number / step.ToFloat())
	// целое умножаем на дробный шаг и получаем готовое дробное значение
	roundedNumber := step.ToFloat() * k
	// разделяем дробную и целую части
	unit, nano := math.Modf(roundedNumber)
	return &pb.Quotation{
		Units: int64(unit),
		Nano:  int32(nano * math.Pow(10, 9)),
	}
}

// Positions - Данные о позициях счета
type Positions struct {
	mx sync.Mutex
	pd *pb.PositionData
}

func NewPositions() *Positions {
	return &Positions{
		pd: &pb.PositionData{},
	}
}

// Update - Обновление позиций
func (p *Positions) Update(data *pb.PositionData) {
	p.mx.Lock()
	p.pd = data
	p.mx.Unlock()
}

// Get - получение позиций
func (p *Positions) Get() *pb.PositionData {
	p.mx.Lock()
	defer p.mx.Unlock()
	return p.pd
}

// start - Запуск чтения стримов позиций и сделок
func (e *Executor) start(ctx context.Context) {
	e.wg.Add(1)
	go func(ctx context.Context) {
		defer e.wg.Done()
		err := e.listenPositions(ctx)
		if err != nil {
			e.client.Logger.Errorf(err.Error())
		}
	}(ctx)

	e.wg.Add(1)
	go func(ctx context.Context) {
		defer e.wg.Done()
		err := e.listenTrades(ctx)
		if err != nil {
			e.client.Logger.Errorf(err.Error())
		}
	}(ctx)
}

// listenPositions - Метод слушает стрим позиций и обновляет их
func (e *Executor) listenPositions(ctx context.Context) error {
	err := e.updatePositionsUnary()
	if err != nil {
		return err
	}
	operationsStreamService := e.client.NewOperationsStreamClient()
	stream, err := operationsStreamService.PositionsStream([]string{e.client.Config.AccountId})
	if err != nil {
		return err
	}
	positionsChan := stream.Positions()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		err := stream.Listen()
		if err != nil {
			e.client.Logger.Errorf(err.Error())
		}
	}()

	e.wg.Add(1)
	go func(ctx context.Context) {
		defer e.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case p, ok := <-positionsChan:
				if !ok {
					return
				}
				// e.client.Logger.Infof("update from positions stream %v\n", p.GetMoney())
				e.positions.Update(p)
			}
		}
	}(ctx)

	<-ctx.Done()
	e.client.Logger.Infof("stop updating positions in executor")
	stream.Stop()
	return nil
}

// updatePositionsUnary - Unary метод обновления позиций
func (e *Executor) updatePositionsUnary() error {
	resp, err := e.operationsService.GetPositions(e.client.Config.AccountId)
	if err != nil {
		return err
	}
	// два слайса *MoneyValue
	available := resp.GetMoney()
	blocked := resp.GetBlocked()

	// слайс *PositionMoney
	positionMoney := make([]*pb.PositionsMoney, 0)
	// ключ - код валюты, значение - *PositionMoney
	moneyByCurrency := make(map[string]*pb.PositionsMoney, 0)

	for _, avail := range available {
		moneyByCurrency[avail.GetCurrency()] = &pb.PositionsMoney{
			AvailableValue: avail,
			BlockedValue:   nil,
		}
	}

	for _, block := range blocked {
		m := moneyByCurrency[block.GetCurrency()]
		moneyByCurrency[block.GetCurrency()] = &pb.PositionsMoney{
			AvailableValue: m.GetAvailableValue(),
			BlockedValue:   block,
		}
	}

	for _, money := range moneyByCurrency {
		positionMoney = append(positionMoney, money)
	}

	// обновляем позиции для исполнителя
	e.positions.Update(&pb.PositionData{
		AccountId:  e.client.Config.AccountId,
		Money:      positionMoney,
		Securities: resp.GetSecurities(),
		Futures:    resp.GetFutures(),
		Options:    resp.GetOptions(),
		Date:       investgo.TimeToTimestamp(time.Now()),
	})

	return nil
}

// listenTrades - Метод слушает стрим сделок и обновляет состояние инструмента
func (e *Executor) listenTrades(ctx context.Context) error {
	ordersStreamClient := e.client.NewOrdersStreamClient()
	stream, err := ordersStreamClient.TradesStream([]string{e.client.Config.AccountId})
	if err != nil {
		return err
	}

	trades := stream.Trades()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		err := stream.Listen()
		if err != nil {
			e.client.Logger.Errorf(err.Error())
		}
	}()

	e.wg.Add(1)
	go func(ctx context.Context) {
		defer e.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case t, ok := <-trades:
				if !ok {
					return
				}
				// обновление статуса инструмента после исполнения лимитной заявки
				var is InstrumentState
				if t.GetAccountId() == e.client.Config.AccountId {
					switch {
					case t.GetDirection() == pb.OrderDirection_ORDER_DIRECTION_BUY:
						is = IN_STOCK
						// TODO add entry price
					case t.GetDirection() == pb.OrderDirection_ORDER_DIRECTION_SELL:
						is = OUT_OF_STOCK
					}
				}
				uid := t.GetInstrumentUid()
				e.instrumentsStates.Update(uid, State{instrumentState: is})
			}
		}
	}(ctx)

	<-ctx.Done()
	e.client.Logger.Infof("stop listening trades in executor")
	stream.Stop()
	return nil
}
