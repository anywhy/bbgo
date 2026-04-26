package xfundingv2

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/datasource/coinmarketcap"
	"github.com/c9s/bbgo/pkg/exchange/binance"
	"github.com/c9s/bbgo/pkg/fixedpoint"
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

	// TickSymbol is the symbol used for ticking the strategy, default to the first candidate symbol
	TickSymbol string `json:"tickSymbol"`

	// Market selection criteria
	MarketSelectionConfig *MarketSelectionConfig `json:"marketSelection,omitempty"`

	CheckInterval       time.Duration `json:"checkInterval"`
	ClosePositionOnExit bool          `json:"closePositionOnExit"`

	futuresOrderBooks, spotOrderBooks map[string]*types.StreamOrderBook
	spotMarkets, futuresMarkets       types.MarketMap
	spotSession, futuresSession       *bbgo.ExchangeSession
	candidateSymbols                  []string
	costEstimators                    map[string]*CostEstimator
	preliminaryMarketSelector         *MarketSelector

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

	if s.MarketSelectionConfig == nil {
		s.MarketSelectionConfig = &MarketSelectionConfig{}
	}
	s.MarketSelectionConfig.Defaults()

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
	s.costEstimators = make(map[string]*CostEstimator)
	return nil
}

func (s *Strategy) Validate() error {
	if len(s.CandidateSymbols) == 0 {
		return errors.New("candidateSymbols is required")
	}

	return nil
}

func (s *Strategy) CrossSubscribe(sessions map[string]*bbgo.ExchangeSession) {
	spotSession, ok := sessions[s.SpotSession]
	if !ok {
		s.logger.Warnf("spot session %s not found, skip subscription", s.SpotSession)
		return
	}
	futuresSession, ok := sessions[s.FuturesSession]
	if !ok {
		s.logger.Warnf("futures session %s not found, skip subscription", s.FuturesSession)
		return
	}

	for _, sess := range []*bbgo.ExchangeSession{spotSession, futuresSession} {
		sess.Subscribe(types.KLineChannel, s.TickSymbol, types.SubscribeOptions{Interval: types.Interval1m})
		sess.Subscribe(types.MarketTradeChannel, s.TickSymbol, types.SubscribeOptions{})
	}
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

	s.candidateSymbols = candidateSymbols

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

		market, ok := s.futuresMarkets[symbol]
		if !ok {
			return fmt.Errorf("market %s not found in futures markets", symbol)
		}
		costEstimator := NewCostEstimator(
			market, futuresBook, spotBook,
		)
		costEstimator.
			SetFuturesFeeRate(types.ExchangeFee{
				MakerFeeRate: s.futuresSession.MakerFeeRate,
				TakerFeeRate: s.futuresSession.TakerFeeRate,
			}).
			SetSpotFeeRate(types.ExchangeFee{
				MakerFeeRate: s.spotSession.MakerFeeRate,
				TakerFeeRate: s.spotSession.TakerFeeRate,
			})
		s.costEstimators[symbol] = costEstimator
	}
	if err := futureStream.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect future stream books: %w", err)
	}
	if err := spotStream.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect spot stream books: %w", err)
	}

	binanceEx, ok := s.futuresSession.Exchange.(*binance.Exchange)
	s.preliminaryMarketSelector = NewMarketSelector(*s.MarketSelectionConfig, binanceEx, s.logger)

	for _, sess := range []*bbgo.ExchangeSession{s.spotSession, s.futuresSession} {
		sess.MarketDataStream.OnMarketTrade(types.TradeWith(s.TickSymbol, func(trade types.Trade) {
			s.tick(ctx, trade.Time.Time())
		}))
		sess.MarketDataStream.OnKLineClosed(types.KLineWith(s.TickSymbol, types.Interval1m, func(kline types.KLine) {
			s.tick(ctx, kline.EndTime.Time())
		}))
	}

	return nil
}

func (s *Strategy) tick(ctx context.Context, tickTime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.checkRoundStartTime.IsZero() || s.currentTime.IsZero() {
		s.checkRoundStartTime = tickTime
		s.currentTime = tickTime
		return
	}
	// from now on, s.checkRoundStartTime and s.currentTime are not zero

	// skip tick that's before current time
	if tickTime.Before(s.currentTime) {
		return
	}
	s.currentTime = tickTime

	// check if it's time to check for new round
	if s.currentTime.Sub(s.checkRoundStartTime) < s.CheckInterval {
		return
	}

	// start processing
	s.process(ctx)

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

func (s *Strategy) process(ctx context.Context) {
	// 1. check if there is any active round needs to be closed
	// TODO: implement round management and closing logic

	// 2. check if new round can be opened or existing round needs to be adjusted
	candidates, err := s.preliminaryMarketSelector.SelectMarkets(ctx, s.candidateSymbols)
	if err != nil {
		s.logger.WithError(err).Error("failed to select market candidates")
		return
	}
	if len(candidates) == 0 {
		// no candidates, nothing to do in this round
		return
	}

	selectedCandidate, _ := s.selectMostPorfitableMarket(candidates)
	if selectedCandidate == nil {
		// no profitable candidate found, nothing to do in this round
		return
	}
	// TODO: implement round opening logic with the selected candidate
}

// selectMostProfitableMarket selects the most profitable market among the candidates based on the estimated break-even holding intervals
// it will also return the target position for the futures trade
// the most profitable market is the one with the shortest break-even holding intervals
func (s *Strategy) selectMostPorfitableMarket(candidates []MarketCandidate) (*MarketCandidate, fixedpoint.Value) {
	if len(candidates) == 0 {
		return nil, fixedpoint.Zero
	}
	spotAccount := s.spotSession.GetAccount()
	breakevenIntervals := make(map[string]fixedpoint.Value)
	targetPositions := make(map[string]fixedpoint.Value)
	for _, candidate := range candidates {
		spotMarket := s.spotMarkets[candidate.Symbol]
		if s.MarketSelectionConfig.FuturesDirection == types.PositionShort {
			// long spot -> find the amount for the quote currency
			quoteBalance, ok := spotAccount.Balance(spotMarket.QuoteCurrency)
			if !ok {
				continue
			}
			// long spot -> trade on the sell side of the order book
			sellBook := s.spotOrderBooks[candidate.Symbol].SideBook(types.SideTypeSell)
			spotPrice := sellBook.AverageDepthPriceByQuote(quoteBalance.Available, 0)
			targetSize := quoteBalance.Available.Div(spotPrice)
			// short futures -> trade on the buy side of the order book
			buyBook := s.futuresOrderBooks[candidate.Symbol].SideBook(types.SideTypeBuy)
			futuresPrice := buyBook.AverageDepthPrice(targetSize)
			// short futures -> target future position should be negative
			breakEvenIntervals, err := s.calculateMinHoldingIntervals(candidate, futuresPrice, targetSize.Neg())
			if err != nil {
				continue
			}
			breakevenIntervals[candidate.Symbol] = breakEvenIntervals
			targetPositions[candidate.Symbol] = targetSize.Neg()
		} else if s.MarketSelectionConfig.FuturesDirection == types.PositionLong {
			baseBalance, ok := spotAccount.Balance(spotMarket.BaseCurrency)
			if !ok {
				continue
			}
			targetSize := baseBalance.Available
			// long futures -> trade on the sell side of the order book
			sellBook := s.futuresOrderBooks[candidate.Symbol].SideBook(types.SideTypeSell)
			futuresPrice := sellBook.AverageDepthPrice(targetSize)
			// long futures -> target future position should be positive
			breakEvenIntervals, err := s.calculateMinHoldingIntervals(candidate, futuresPrice, targetSize)
			if err != nil {
				continue
			}
			breakevenIntervals[candidate.Symbol] = breakEvenIntervals
			targetPositions[candidate.Symbol] = targetSize
		} else {
			return nil, fixedpoint.Zero
		}
	}
	if len(breakevenIntervals) == 0 {
		return nil, fixedpoint.Zero
	}
	sortedCandidates := candidates
	sort.Slice(sortedCandidates, func(i, j int) bool {
		candidate1 := sortedCandidates[i]
		candidate2 := sortedCandidates[j]
		return breakevenIntervals[candidate1.Symbol].Compare(breakevenIntervals[candidate2.Symbol]) <= 0
	})
	bestCandidate := &sortedCandidates[0]
	targetPosition := targetPositions[bestCandidate.Symbol]
	// set the estimated min holding interval for the selected candidate
	numHoldingHours := breakevenIntervals[bestCandidate.Symbol].Int() * bestCandidate.FundingIntervalHours
	bestCandidate.MinHoldingDuration = time.Duration(numHoldingHours) * time.Hour
	return bestCandidate, targetPosition
}

func (s *Strategy) calculateMinHoldingIntervals(candidate MarketCandidate, bestPrice, targetPosition fixedpoint.Value) (fixedpoint.Value, error) {
	costEstimator := s.costEstimators[candidate.Symbol]
	costEstimator.SetTargetPosition(targetPosition)
	estimateEntryCost, err := costEstimator.EstimateEntryCost(true)
	if err != nil {
		return fixedpoint.Zero, err
	}
	estimateExitCost, err := costEstimator.EstimateExitCost(true)
	if err != nil {
		return fixedpoint.Zero, err
	}
	totalCost := estimateEntryCost.
		TotalFeeCost().
		Add(estimateEntryCost.SpreadPnL).
		Add(estimateExitCost.TotalFeeCost()).
		Add(estimateExitCost.SpreadPnL)
	amount := targetPosition.Abs().Mul(bestPrice)
	estimateFundingFeePerInterval := amount.Mul(candidate.LastFundingRate.Abs())
	if estimateFundingFeePerInterval.IsZero() {
		return fixedpoint.Zero, nil
	}
	breakEvenIntervals := totalCost.Div(estimateFundingFeePerInterval).Round(0, fixedpoint.Up)
	return breakEvenIntervals, nil
}
