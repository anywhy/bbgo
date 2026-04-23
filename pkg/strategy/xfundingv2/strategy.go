package xfundingv2

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/datasource/coinmarketcap"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/sirupsen/logrus"
)

const ID = "xfundingv2"

func init() {
	bbgo.RegisterStrategy(ID, &Strategy{})
}

type Strategy struct {
	Environment *bbgo.Environment

	// Session configuration
	SpotSession    string `json:"spotSession"`
	FuturesSession string `json:"futuresSession"`

	// CandidateSymbols is the list of symbols to consider for selection
	// IMPORTANT: xfundingv2 is now assuming trading on U-major pairs
	CandidateSymbols []string `json:"candidateSymbols"`
	TickSymbol       string   `json:"tickSymbol"` // symbol used for ticking the strategy, default to the first candidate symbol
	// Market selection criteria
	MarketSelectionConfig *MarketSelectionConfig `json:"marketSelection,omitempty"`

	CheckInterval       time.Duration `json:"checkInterval"`
	ClosePositionOnExit bool          `json:"closePositionOnExit"`

	futuresOrderBooks, spotOrderBooks map[string]*types.StreamOrderBook
	spotMarkets, futuresMarkets       types.MarketMap
	spotSession, futuresSession       *bbgo.ExchangeSession

	coinmarketcapClient *coinmarketcap.DataSource
	mu                  sync.Mutex
	checkRoundStartTime time.Time
	currentTime         time.Time

	logger logrus.FieldLogger
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	symbols := strings.Join(s.CandidateSymbols, "_")
	return fmt.Sprintf("%s-%s", ID, symbols)
}

// Defaults() -> Initialize() -> Validate() -> CrossSubscribe() -> CrossRun()
func (s *Strategy) Defaults() error {
	if len(s.CandidateSymbols) == 0 {
		return errors.New("empty candidateSymbols")
	}
	if s.TickSymbol == "" {
		s.TickSymbol = s.CandidateSymbols[0]
	}
	return nil
}

func (s *Strategy) Initialize() error {
	s.logger = logrus.WithFields(logrus.Fields{
		"strategy":    ID,
		"strategy_id": s.InstanceID(),
	})

	if apiKey := os.Getenv("COINMARKETCAP_API_KEY"); apiKey == "" {
		s.logger.Warn("CoinMarketCap API key not set, top cap market filtering will be disabled")
	} else {
		s.coinmarketcapClient = coinmarketcap.New(apiKey)
	}
	s.futuresOrderBooks = make(map[string]*types.StreamOrderBook)
	s.spotOrderBooks = make(map[string]*types.StreamOrderBook)
	return nil
}

func (s *Strategy) Validate() error {
	if s.MarketSelectionConfig == nil {
		return errors.New("marketSelection config is required")
	}
	if len(s.CandidateSymbols) == 0 {
		return errors.New("candidateSymbols is required")
	}

	return nil
}

func (s *Strategy) CrossSubscribe(sessions map[string]*bbgo.ExchangeSession) {
	spotSession := sessions[s.SpotSession]
	futuresSession := sessions[s.FuturesSession]

	for _, symbol := range s.CandidateSymbols {
		spotSession.Subscribe(types.KLineChannel, symbol, types.SubscribeOptions{Interval: types.Interval1m})
		spotSession.Subscribe(types.BookChannel, symbol, types.SubscribeOptions{})
		futuresSession.Subscribe(types.KLineChannel, symbol, types.SubscribeOptions{Interval: types.Interval1m})
		futuresSession.Subscribe(types.BookChannel, symbol, types.SubscribeOptions{})
	}
	spotSession.Subscribe(types.MarketTradeChannel, s.TickSymbol, types.SubscribeOptions{})
	futuresSession.Subscribe(types.MarketTradeChannel, s.TickSymbol, types.SubscribeOptions{})
}

func (s *Strategy) CrossRun(
	ctx context.Context, orderExecutionRouter bbgo.OrderExecutionRouter, sessions map[string]*bbgo.ExchangeSession,
) error {
	s.spotSession = sessions[s.SpotSession]
	s.futuresSession = sessions[s.FuturesSession]

	if s.futuresSession == nil {
		return fmt.Errorf("futures session %s not found", s.FuturesSession)
	}
	if s.spotSession == nil {
		return fmt.Errorf("spot session %s not found", s.SpotSession)
	}
	futuresEx, ok := s.futuresSession.Exchange.(types.FuturesExchange)
	if !ok {
		return fmt.Errorf("sessioin %s does not support futures", s.futuresSession.Name)
	}
	if !futuresEx.GetFuturesSettings().IsFutures {
		return fmt.Errorf("session %s is not configured for futures trading", s.futuresSession.Name)
	}

	spotMarkets, err := s.spotSession.Exchange.QueryMarkets(ctx)
	if err != nil {
		return fmt.Errorf("failed to query spot markets: %w", err)
	}
	s.spotMarkets = spotMarkets
	futuresMarkets, err := s.futuresSession.Exchange.QueryMarkets(ctx)
	if err != nil {
		return fmt.Errorf("failed to query futures markets: %w", err)
	}
	s.futuresMarkets = futuresMarkets

	// static filters
	var candidateSymbols []string
	// 1. should be listed on both spot and futures
	candidateSymbols = s.filterMarketBothListed(s.CandidateSymbols)
	// 2. filter by collateral rate
	candidateSymbols = s.filterMarketCollateralRate(ctx, candidateSymbols)
	// 3. filter by top N market cap
	candidateSymbols = s.filterMarketByCapSize(ctx, candidateSymbols)

	if len(candidateSymbols) == 0 {
		return errors.New("no candidate symbols after filtering")
	}

	// subscribe BNB pairs for trading fee calculation
	quoteCurrencies := make(map[string]struct{})
	for _, symbol := range candidateSymbols {
		market := s.futuresMarkets[symbol]
		quoteCurrencies[market.QuoteCurrency] = struct{}{}
	}
	for quoteCurrency := range quoteCurrencies {
		bnbSymbol := fmt.Sprintf("BNB%s", quoteCurrency)
		s.spotSession.Subscribe(types.KLineChannel, bnbSymbol, types.SubscribeOptions{Interval: types.Interval1m})
		s.futuresSession.Subscribe(types.KLineChannel, bnbSymbol, types.SubscribeOptions{Interval: types.Interval1m})
	}

	// initialize depth books for model selection
	futureStream := s.futuresSession.Exchange.NewStream()
	futureStream.SetPublicOnly()
	spotStream := s.spotSession.Exchange.NewStream()
	spotStream.SetPublicOnly()
	for _, symbol := range candidateSymbols {
		futuresBook := types.NewStreamBook(symbol, s.futuresSession.ExchangeName)
		futuresBook.BindStream(futureStream)
		futureStream.Subscribe(types.BookChannel, symbol, types.SubscribeOptions{})
		s.futuresOrderBooks[symbol] = futuresBook

		spotBook := types.NewStreamBook(symbol, s.spotSession.ExchangeName)
		spotBook.BindStream(spotStream)
		spotStream.Subscribe(types.BookChannel, symbol, types.SubscribeOptions{})
		s.spotOrderBooks[symbol] = spotBook
	}
	if err := futureStream.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect future stream books: %w", err)
	}
	if err := spotStream.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect spot stream books: %w", err)
	}

	s.spotSession.MarketDataStream.OnMarketTrade(types.TradeWith(s.TickSymbol, func(trade types.Trade) {
		s.tick(trade.Time.Time())
	}))
	s.futuresSession.MarketDataStream.OnMarketTrade(types.TradeWith(s.TickSymbol, func(trade types.Trade) {
		s.tick(trade.Time.Time())
	}))

	return nil
}

func (s *Strategy) tick(tickTime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.checkRoundStartTime.IsZero() {
		s.checkRoundStartTime = tickTime
		return
	}

	if !s.currentTime.IsZero() && tickTime.Before(s.currentTime) {
		return
	}
	s.currentTime = tickTime

	if s.currentTime.Sub(s.checkRoundStartTime) < s.CheckInterval {
		return
	}

	// 1. check if any open round needs to be closed
	// 2. check if new round can be opened

	// check interval done, reset and start new check interval
	s.checkRoundStartTime = s.currentTime
}

// static market filters
func (s *Strategy) filterMarketByCapSize(ctx context.Context, symbols []string) []string {
	if s.coinmarketcapClient == nil {
		return symbols
	}
	topAssets, err := s.queryTopCapAssets(ctx)
	if err != nil {
		return symbols
	}
	var candidateSymbols []string
	for _, symbol := range symbols {
		market, ok := s.futuresMarkets[symbol]
		if !ok {
			continue
		}
		if _, found := topAssets[market.BaseCurrency]; found {
			candidateSymbols = append(candidateSymbols, symbol)
		}
	}
	return candidateSymbols
}

func (s *Strategy) filterMarketBothListed(symbols []string) []string {
	var candidateSymbols []string
	for _, symbol := range symbols {
		_, spotOk := s.spotMarkets[symbol]
		_, futuresOk := s.futuresMarkets[symbol]
		if spotOk && futuresOk {
			candidateSymbols = append(candidateSymbols, symbol)
		} else {
			s.logger.Infof("skipping %s as it's not listed on both spot and futures", symbol)
		}
	}
	return candidateSymbols
}

func (s *Strategy) filterMarketCollateralRate(ctx context.Context, symbols []string) []string {
	var markets []types.Market
	for _, symbol := range symbols {
		market, ok := s.futuresMarkets[symbol]
		if !ok {
			continue
		}
		markets = append(markets, market)
	}
	var baseAssets []string
	for _, market := range markets {
		baseAssets = append(baseAssets, market.BaseCurrency)
	}
	collateralRates, err := queryPortfolioModeCollateralRates(ctx, baseAssets)
	if err != nil {
		s.logger.WithError(err).Warn("failed to query collateral rates, skipping collateral rate filter")
		return symbols
	}
	var candidateSymbols []string
	for _, market := range markets {
		rate, ok := collateralRates[market.BaseCurrency]
		if !ok {
			continue
		}
		if rate.Compare(s.MarketSelectionConfig.MinCollateralRate) >= 0 {
			candidateSymbols = append(candidateSymbols, market.Symbol)
		} else {
			s.logger.Infof("skipping %s due to low collateral rate: %s", market.Symbol, rate.String())
		}
	}
	return candidateSymbols
}
