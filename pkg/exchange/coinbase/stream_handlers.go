package coinbase

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
)

const (
	rfqMatchChannel    types.Channel = "rfq_matches"
	statusChannel      types.Channel = "status"
	auctionChannel     types.Channel = "auctionfeed"
	matchesChannel     types.Channel = "matches"
	tickerChannel      types.Channel = "ticker"
	tickerBatchChannel types.Channel = "ticker_batch"
	level2Channel      types.Channel = "level2"
	level2BatchChannel types.Channel = "level2_batch"
	fullChannel        types.Channel = "full"
	userChannel        types.Channel = "user"
	balanceChannel     types.Channel = "balance"
)

type channelType struct {
	Name       types.Channel `json:"name"`
	ProductIDs []string      `json:"product_ids,omitempty"`
}

func (c channelType) String() string {
	return fmt.Sprintf("(channelType Name:%s, ProductIDs: %s)", c.Name, c.ProductIDs)
}

type authMsg struct {
	Signature  string `json:"signature,omitempty"`
	Key        string `json:"key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
}

func (msg authMsg) String() string {
	return fmt.Sprintf("(authMsg Signature:%s, Timestamp:%s)", msg.Signature, msg.Timestamp)
}

type subscribeMsgType1 struct {
	Type     string        `json:"type"`
	Channels []channelType `json:"channels"`

	authMsg
}

func (msg subscribeMsgType1) String() string {
	return fmt.Sprintf("(subscribeMsg Type:%s, Channels: %s, %s)", msg.Type, msg.Channels, msg.authMsg.String())
}

type subscribeMsgType2 struct {
	Type       string          `json:"type"`
	Channels   []types.Channel `json:"channels"`
	ProductIDs []string        `json:"product_ids,omitempty"`
	AccountIDs []string        `json:"account_ids,omitempty"` // for balance channel

	authMsg
}

func (msg subscribeMsgType2) String() string {
	return fmt.Sprintf(
		"(Type:%s, Channels:%s, ProductIDs:%s, AccountIDs:%s, %s",
		msg.Type,
		msg.Channels,
		msg.ProductIDs,
		msg.AccountIDs,
		msg.authMsg.String(),
	)
}

func (s *Stream) handleConnect() {
	// subscribe to channels
	if len(s.Subscriptions) == 0 {
		return
	}

	subProductsMap := make(map[types.Channel][]string)
	for _, sub := range s.Subscriptions {
		// rfqMatchChannel allow empty symbol
		if sub.Channel != rfqMatchChannel && sub.Channel != statusChannel && len(sub.Symbol) == 0 {
			continue
		}
		subProductsMap[sub.Channel] = append(subProductsMap[sub.Channel], toLocalSymbol(sub.Symbol))
	}

	_, subscribeFull := subProductsMap[fullChannel]
	s.userOrderOnly = !subscribeFull

	var subCmds []any
	signature, ts := s.generateSignature()
	for channel, productIDs := range subProductsMap {
		var subType string
		switch types.Channel(channel) {
		case rfqMatchChannel:
			subType = "subscriptions"
		default:
			subType = "subscribe"
		}
		var subCmd any
		switch types.Channel(channel) {
		case statusChannel:
			subCmd = subscribeMsgType1{
				Type: subType,
				Channels: []channelType{
					{
						Name: channel,
					},
				},
			}
		case auctionChannel, rfqMatchChannel:
			subCmd = subscribeMsgType1{
				Type: subType,
				Channels: []channelType{
					{
						Name:       channel,
						ProductIDs: productIDs,
					},
				},
			}
		case matchesChannel:
			subCmd = subscribeMsgType1{
				Type: subType,
				Channels: []channelType{
					{
						Name:       channel,
						ProductIDs: productIDs,
					},
				},
			}
			if v, _ := subCmd.(subscribeMsgType1); s.authEnabled {
				v.authMsg = authMsg{
					Signature:  signature,
					Key:        s.apiKey,
					Passphrase: s.passphrase,
					Timestamp:  ts,
				}
				subCmd = v
			}
		case tickerChannel, tickerBatchChannel:
			subCmd = subscribeMsgType2{
				Type:       subType,
				Channels:   []types.Channel{channel},
				ProductIDs: productIDs,
			}
		case fullChannel, userChannel:
			subCmd = subscribeMsgType2{
				Type:       subType,
				Channels:   []types.Channel{channel},
				ProductIDs: productIDs,
				authMsg: authMsg{
					Signature:  signature,
					Key:        s.apiKey,
					Passphrase: s.passphrase,
					Timestamp:  ts,
				},
			}
		case level2Channel, level2BatchChannel:
			subCmd = subscribeMsgType2{
				Type:       subType,
				Channels:   []types.Channel{channel},
				ProductIDs: productIDs,

				authMsg: authMsg{
					Signature:  signature,
					Key:        s.apiKey,
					Passphrase: s.passphrase,
					Timestamp:  ts,
				},
			}
		case balanceChannel:
			subCmd = subscribeMsgType2{
				Type:       subType,
				Channels:   []types.Channel{channel},
				AccountIDs: productIDs,

				authMsg: authMsg{
					Signature:  signature,
					Key:        s.apiKey,
					Passphrase: s.passphrase,
					Timestamp:  ts,
				},
			}
		default:
			subCmd = subscribeMsgType1{
				Type: subType,
				Channels: []channelType{
					{
						Name:       channel,
						ProductIDs: productIDs,
					},
				},

				authMsg: authMsg{
					Signature:  signature,
					Key:        s.apiKey,
					Passphrase: s.passphrase,
					Timestamp:  ts,
				},
			}
		}
		subCmds = append(subCmds, subCmd)
	}
	for _, subCmd := range subCmds {
		err := s.Conn.WriteJSON(subCmd)
		if err != nil {
			logStream.WithError(err).Errorf("subscription error: %s", subCmd)
		} else {
			logStream.Infof("subscribed to %s", subCmd)
		}
	}

	s.clearSequenceNumber()
	s.clearWorkingOrders()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()

		// emit balance snapshot on connection
		// query account balances
		balances, err := s.exchange.QueryAccountBalances(ctx)
		if err != nil {
			logStream.WithError(err).Warn("failed to query account balances, the balance snapshot is initialized with empty balances")
			balances = make(types.BalanceMap)
		}
		s.EmitBalanceSnapshot(balances)

		// query working orders and cache them
		// the cache is required since we do note receive the original order
		// size in the order update messages. Hence we need the cache to find
		// the original order size to calculate the executed quantity.
		// empty symbol -> all symbols
		// empty status array -> all orders that are open or un-settled
		workingRawOrders, err := s.exchange.queryOrdersByPagination(ctx, "", []string{})
		if err != nil {
			logStream.WithError(err).Warn("failed to query open orders, the orders snapshot is initialized with empty orders")
		} else {
			openOrders := make([]types.Order, 0, len(workingRawOrders))
			for _, rawOrder := range workingRawOrders {
				openOrders = append(openOrders, toGlobalOrder(&rawOrder))
			}
			s.updateWorkingOrders(openOrders...)
		}
	}()
}

func (s *Stream) handleDisconnect() {
	s.lockWorkingOrderMap.Lock()
	defer s.lockWorkingOrderMap.Unlock()

	// clear sequence numbers
	s.clearSequenceNumber()
	s.clearWorkingOrders()
}

// order book, trade
func (s *Stream) handleTickerMessage(msg *TickerMessage) {
	// ignore outdated messages
	if !s.checkAndUpdateSequenceNumber(msg.Type, msg.ProductID, msg.Sequence) {
		return
	}
	trade := msg.Trade()
	s.EmitTradeUpdate(trade)
}

func (s *Stream) handleMatchMessage(msg *MatchMessage) {
	// ignore outdated messages
	if !s.checkAndUpdateSequenceNumber(msg.Type, msg.ProductID, msg.Sequence) {
		return
	}
	trade := msg.Trade()
	s.EmitTradeUpdate(trade)
}

func (s *Stream) handleOrderBookSnapshotMessage(msg *OrderBookSnapshotMessage) {
	symbol := toGlobalSymbol(msg.ProductID)
	var bids types.PriceVolumeSlice
	for _, b := range msg.Bids {
		if len(b) != 2 {
			continue
		}
		bids = append(bids, types.PriceVolume{
			Price:  b[0],
			Volume: b[1],
		})
	}
	var asks types.PriceVolumeSlice
	for _, a := range msg.Asks {
		if len(a) != 2 {
			continue
		}
		asks = append(asks, types.PriceVolume{
			Price:  a[0],
			Volume: a[1],
		})
	}
	book := types.SliceOrderBook{
		Symbol: symbol,
		Bids:   bids,
		Asks:   asks,
		Time:   time.Now(),
	}
	s.EmitBookSnapshot(book)
}

func (s *Stream) handleOrderbookUpdateMessage(msg *OrderBookUpdateMessage) {
	var bids types.PriceVolumeSlice
	var asks types.PriceVolumeSlice
	for _, c := range msg.Changes {
		if len(c) != 3 {
			continue
		}
		side := c[0]
		price := fixedpoint.MustNewFromString(c[1])
		volume := fixedpoint.MustNewFromString(c[2])
		if volume.IsZero() {
			// 0 volume means the price can be removed.
			continue
		}
		switch side {
		case "buy":
			bids = append(bids, types.PriceVolume{
				Price:  price,
				Volume: volume,
			})
		case "sell":
			asks = append(asks, types.PriceVolume{
				Price:  price,
				Volume: volume,
			})
		default:
			logStream.Warnf("unknown order book update side: %s", side)
		}
	}
	if len(bids) > 0 || len(asks) > 0 {
		book := types.SliceOrderBook{
			Symbol: toGlobalSymbol(msg.ProductID),
			Bids:   bids,
			Asks:   asks,
			Time:   msg.Time,
		}
		s.EmitBookUpdate(book)
	}

}

// order update
func (s *Stream) handleReceivedMessage(msg *ReceivedMessage) {
	if !s.checkAndUpdateSequenceNumber(msg.Type, msg.ProductID, msg.Sequence) {
		return
	}
	if msg.OrderType == "stop" {
		// stop order is not supported
		logStream.Warnf("should not receive stop order via received channel: %s", msg.OrderID)
		return
	}
	// A valid order has been received and is now active.
	orderUpdate := types.Order{
		SubmitOrder: types.SubmitOrder{
			Symbol: toGlobalSymbol(msg.ProductID),
			Side:   toGlobalSide(msg.Side),
		},
		Status:     types.OrderStatusNew,
		UUID:       msg.OrderID,
		Exchange:   types.ExchangeCoinBase,
		UpdateTime: types.Time(msg.Time),
		IsWorking:  true,
	}
	switch msg.OrderType {
	case "limit":
		orderUpdate.SubmitOrder.Type = types.OrderTypeLimit
		orderUpdate.SubmitOrder.Price = msg.Price
		orderUpdate.SubmitOrder.Quantity = msg.Size
	case "market":
		// NOTE: the Exchange.SubmitOrder method garantees that the market order does not support funds.
		// So we simply check the size for market order here.
		if msg.Size.IsZero() {
			logStream.Warnf("received empty order size, dropped: %s", msg.OrderID)
			return
		}
		orderUpdate.SubmitOrder.Type = types.OrderTypeMarket
		orderUpdate.SubmitOrder.Quantity = msg.Size
	default:
		logStream.Warnf("unknown order type, dropped: %s", msg.OrderType)
		return
	}
	s.updateWorkingOrders(orderUpdate)
	s.EmitOrderUpdate(orderUpdate)
}

func (s *Stream) handleOpenMessage(msg *OpenMessage) {
	if !s.checkAndUpdateSequenceNumber(msg.Type, msg.ProductID, msg.Sequence) {
		return
	}
	// The order is now open on the order book.
	// Getting an open message means that it's not fully filled immediately at receipt of the order or not canceled.
	// We need to consider the case of partially filled orders here.
	lastOrder, found := s.getOrderById(msg.OrderID)
	if !found {
		// the order is not in the working orders map, retrieve it via the API
		order, err := s.retrieveOrderById(msg.OrderID)
		if err != nil {
			logStream.Warnf(
				"the order is not found in the cache and cannot be retrieved via API: %s. Skipped",
				msg.OrderID,
			)
			return
		}
		lastOrder = *order
	}
	amountExecuted := lastOrder.SubmitOrder.Quantity.Sub(msg.RemainingSize)
	orderUpdate := types.Order{
		SubmitOrder: types.SubmitOrder{
			Symbol:   toGlobalSymbol(msg.ProductID),
			Side:     toGlobalSide(msg.Side),
			Price:    msg.Price,
			Quantity: lastOrder.Quantity,
		},
		ExecutedQuantity: amountExecuted,
		Status:           types.OrderStatusNew,
		UUID:             msg.OrderID,
		Exchange:         types.ExchangeCoinBase,
		IsWorking:        true,
		UpdateTime:       types.Time(msg.Time),
	}
	s.EmitOrderUpdate(orderUpdate)
}

func (s *Stream) handleDoneMessage(msg *DoneMessage) {
	if !s.checkAndUpdateSequenceNumber(msg.Type, msg.ProductID, msg.Sequence) {
		return
	}
	// The lastOrder is no longer on the lastOrder book.
	lastOrder, found := s.getOrderById(msg.OrderID)
	if !found {
		// the order is not in the working orders map, retrieve it via the API
		order, err := s.retrieveOrderById(msg.OrderID)
		if err != nil {
			logStream.Warnf(
				"the order is not found in the cache and cannot be retrieved via API: %s. Skipped",
				msg.OrderID,
			)
			return
		}
		lastOrder = *order
	}
	quantityExecuted := lastOrder.SubmitOrder.Quantity.Sub(msg.RemainingSize)
	status := types.OrderStatusFilled
	if msg.Reason == "canceled" {
		status = types.OrderStatusCanceled
	} else {
		status = types.OrderStatusFilled
	}

	orderUpdate := types.Order{
		SubmitOrder: types.SubmitOrder{
			Symbol:   toGlobalSymbol(msg.ProductID),
			Side:     toGlobalSide(msg.Side),
			Price:    msg.Price,
			Quantity: lastOrder.Quantity,
		},
		Status:           status,
		UUID:             msg.OrderID,
		Exchange:         types.ExchangeCoinBase,
		ExecutedQuantity: quantityExecuted,
		IsWorking:        false,
		UpdateTime:       types.Time(msg.Time),
	}
	s.updateWorkingOrders(orderUpdate)
	s.EmitOrderUpdate(orderUpdate)
}

func (s *Stream) handleChangeMessage(msg *ChangeMessage) {
	if !s.checkAndUpdateSequenceNumber(msg.Type, msg.ProductID, msg.Sequence) {
		return
	}
	// An order has changed.
	_, found := s.getOrderById(msg.OrderID)
	if !found {
		// We assume the change should only apply to the orders that are cached in the working orders map.
		// The rationale is that
		// 1. If the order is submitted after the connection is established, there should be a received/open message
		//    before the change message. So we should have the order in the working orders map.
		// 2. If the order is submitted before the connection is established, it should be cached in the working orders map
		//    since the map is created on connection.
		// Skip the message if not found since it should be orders that are not belong to the user.
		logStream.Warnf("the order is not found in the cache: %s. Skipped the change", msg.OrderID)
		return
	}
	orderUpdate := types.Order{
		SubmitOrder: types.SubmitOrder{
			Symbol:   toGlobalSymbol(msg.ProductID),
			Side:     toGlobalSide(msg.Side),
			Price:    msg.NewPrice,
			Quantity: msg.NewSize,
		},
		UUID:       msg.OrderID,
		Exchange:   types.ExchangeCoinBase,
		IsWorking:  true,
		UpdateTime: types.Time(msg.Time),
	}
	s.updateWorkingOrders(orderUpdate)
	s.EmitOrderUpdate(orderUpdate)
}

func (s *Stream) handleActivateMessage(msg *ActivateMessage) {
	// An activate message is sent when a stop order is placed.
	// the stop order now becomes a limit order.
	lastOrder, found := s.getOrderById(msg.OrderID)
	if !found {
		// the order is not in the working orders map, retrieve it via the API
		order, err := s.retrieveOrderById(msg.OrderID)
		if err != nil {
			logStream.Warnf(
				"the order is not found in the cache and cannot be retrieved via API: %s. Skipped",
				msg.OrderID,
			)
			return
		}
		lastOrder = *order
	}
	var updateTime types.Time
	timestamp, err := strconv.ParseFloat(msg.Timestamp, 64)
	if err != nil {
		updateTime = types.Time(time.Now())
	} else {
		updateTime = types.Time(time.UnixMilli(int64(timestamp * 1000)))
	}
	// do no consider limit order with funds
	order := types.Order{
		SubmitOrder: types.SubmitOrder{
			Type:      types.OrderTypeStopLimit,
			Symbol:    toGlobalSymbol(msg.ProductID),
			Side:      toGlobalSide(msg.Side),
			Price:     lastOrder.Price,
			StopPrice: msg.StopPrice,
			Quantity:  msg.Size,
		},
		Status:     types.OrderStatusNew,
		UUID:       msg.OrderID,
		Exchange:   types.ExchangeCoinBase,
		IsWorking:  true,
		UpdateTime: updateTime,
	}
	s.updateWorkingOrders(order)
	s.EmitOrderUpdate(order)
}

// balance update
func (s *Stream) handleBalanceMessage(msg *BalanceMessage) {
	balanceUpdte := make(types.BalanceMap)
	balanceUpdte[msg.Currency] = types.Balance{
		Currency:  msg.Currency,
		Available: msg.Available,
		Locked:    msg.Holds,
		NetAsset:  msg.Available.Add(msg.Holds),
	}
	s.EmitBalanceUpdate(balanceUpdte)
}

// helpers
func (s *Stream) checkAndUpdateSequenceNumber(msgType MessageType, productId string, currentSeqNum SequenceNumberType) bool {
	s.lockSeqNumMap.Lock()
	defer s.lockSeqNumMap.Unlock()

	key := fmt.Sprintf("%s-%s", msgType, productId)
	latestSeq := s.lastSequenceMsgMap[key]
	if latestSeq >= currentSeqNum {
		return false
	}
	s.lastSequenceMsgMap[key] = currentSeqNum
	return true
}

func (s *Stream) clearSequenceNumber() {
	s.lockSeqNumMap.Lock()
	defer s.lockSeqNumMap.Unlock()

	s.lastSequenceMsgMap = make(map[string]SequenceNumberType)
}

func (s *Stream) updateWorkingOrders(orders ...types.Order) {
	s.lockWorkingOrderMap.Lock()
	defer s.lockWorkingOrderMap.Unlock()

	for _, order := range orders {
		exisiting, ok := s.workingOrdersMap[order.UUID]
		if ok {
			if !order.IsWorking {
				// order is already in the map and not working, remove it
				delete(s.workingOrdersMap, order.UUID)
				continue
			} else {
				// order is already in the map and working, update it
				exisiting.Update(order)
				s.workingOrdersMap[order.UUID] = exisiting
			}
		} else {
			// order is not in the map, add it
			s.workingOrdersMap[order.UUID] = order
		}
	}
}

func (s *Stream) getOrderById(orderId string) (types.Order, bool) {
	s.lockWorkingOrderMap.Lock()
	defer s.lockWorkingOrderMap.Unlock()

	order, found := s.workingOrdersMap[orderId]
	return order, found
}

func (s *Stream) clearWorkingOrders() {
	s.lockWorkingOrderMap.Lock()
	defer s.lockWorkingOrderMap.Unlock()

	s.workingOrdersMap = make(map[string]types.Order)
}

func (s *Stream) retrieveOrderById(orderId string) (*types.Order, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	if !s.userOrderOnly {
		msg := fmt.Sprintf("retrieve order by id disabled on a stream feeds including non-user orders: %s", orderId)
		logStream.Warn(msg)
		return nil, fmt.Errorf(msg)
	}
	order, err := s.exchange.QueryOrder(ctx, types.OrderQuery{OrderID: orderId})
	if err != nil {
		return nil, err
	}
	s.updateWorkingOrders(*order)
	return order, nil
}
