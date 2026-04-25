package binance

import (
	"testing"

	"github.com/c9s/bbgo/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestIsFuturesPublicChannel(t *testing.T) {
	assert.True(t, isFuturesPublicChannel(types.BookChannel))
	assert.True(t, isFuturesPublicChannel(types.BookTickerChannel))
	assert.False(t, isFuturesPublicChannel(types.KLineChannel))
	assert.False(t, isFuturesPublicChannel(types.AggTradeChannel))
	assert.False(t, isFuturesPublicChannel(types.MarketTradeChannel))
	assert.False(t, isFuturesPublicChannel(types.MarkPriceChannel))
	assert.False(t, isFuturesPublicChannel(types.ForceOrderChannel))
}

func TestClassifyFuturesSubscriptions(t *testing.T) {
	subs := []types.Subscription{
		{Channel: types.BookChannel, Symbol: "BTCUSDT"},
		{Channel: types.BookTickerChannel, Symbol: "ETHUSDT"},
		{Channel: types.KLineChannel, Symbol: "BTCUSDT"},
		{Channel: types.AggTradeChannel, Symbol: "BTCUSDT"},
		{Channel: types.MarkPriceChannel, Symbol: "BTCUSDT"},
	}
	public, market := classifyFuturesSubscriptions(subs)
	assert.Len(t, public, 2)
	assert.Len(t, market, 3)
	assert.Equal(t, types.BookChannel, public[0].Channel)
	assert.Equal(t, types.BookTickerChannel, public[1].Channel)
	assert.Equal(t, types.KLineChannel, market[0].Channel)
}

func TestClassifyFuturesSubscriptions_OnlyPublic(t *testing.T) {
	subs := []types.Subscription{{Channel: types.BookChannel, Symbol: "BTCUSDT"}}
	public, market := classifyFuturesSubscriptions(subs)
	assert.Len(t, public, 1)
	assert.Empty(t, market)
}

func TestClassifyFuturesSubscriptions_OnlyMarket(t *testing.T) {
	subs := []types.Subscription{{Channel: types.KLineChannel, Symbol: "BTCUSDT"}}
	public, market := classifyFuturesSubscriptions(subs)
	assert.Empty(t, public)
	assert.Len(t, market, 1)
}

func TestGetFuturesPublicEndpointUrl(t *testing.T) {
	s := &Stream{exchange: &Exchange{}}
	testNet = false
	assert.Equal(t, FuturesPublicWebSocketURL+"/ws", s.getFuturesPublicEndpointUrl())
}

func TestGetFuturesMarketEndpointUrl(t *testing.T) {
	s := &Stream{exchange: &Exchange{}}
	testNet = false
	assert.Equal(t, FuturesMarketWebSocketURL+"/ws", s.getFuturesMarketEndpointUrl())
}

func TestGetFuturesPrivateEndpointUrl(t *testing.T) {
	s := &Stream{exchange: &Exchange{}}
	testNet = false
	url := s.getFuturesPrivateEndpointUrl("mykey123")
	assert.Equal(t, FuturesPrivateWebSocketURL+"/ws?listenKey=mykey123&events="+futuresPrivateStreamEvents, url)
}

func TestWriteSpecificSubscriptions_Empty(t *testing.T) {
	s := &Stream{exchange: &Exchange{}}
	// nil conn — should return nil without panicking when subs is empty
	err := s.writeSpecificSubscriptions(nil, nil)
	assert.NoError(t, err)
}

func TestGetPublicEndpointUrl_FuturesOnlyMarket(t *testing.T) {
	testNet = false
	ex := &Exchange{}
	ex.FuturesSettings.IsFutures = true
	s := &Stream{
		StandardStream: types.NewStandardStream(),
		exchange:       ex,
	}
	s.Subscriptions = []types.Subscription{
		{Channel: types.KLineChannel, Symbol: "BTCUSDT"},
	}
	url := s.getPublicEndpointUrl()
	assert.Equal(t, FuturesMarketWebSocketURL+"/ws", url)
	assert.Nil(t, s.futuresAuxStream)
}

func TestGetPublicEndpointUrl_FuturesOnlyPublic(t *testing.T) {
	testNet = false
	ex := &Exchange{}
	ex.FuturesSettings.IsFutures = true
	s := &Stream{
		StandardStream: types.NewStandardStream(),
		exchange:       ex,
	}
	s.Subscriptions = []types.Subscription{
		{Channel: types.BookChannel, Symbol: "BTCUSDT"},
	}
	url := s.getPublicEndpointUrl()
	assert.Equal(t, FuturesPublicWebSocketURL+"/ws", url)
	assert.Nil(t, s.futuresAuxStream)
}

func TestGetPublicEndpointUrl_FuturesMixed(t *testing.T) {
	testNet = false
	ex := &Exchange{}
	ex.FuturesSettings.IsFutures = true
	s := &Stream{
		StandardStream: types.NewStandardStream(),
		exchange:       ex,
	}
	s.Subscriptions = []types.Subscription{
		{Channel: types.BookChannel, Symbol: "BTCUSDT"},
		{Channel: types.KLineChannel, Symbol: "BTCUSDT"},
	}
	url := s.getPublicEndpointUrl()
	assert.Equal(t, FuturesMarketWebSocketURL+"/ws", url)
	assert.NotNil(t, s.futuresAuxStream)
}

func TestGetUserDataStreamEndpointUrl_Futures(t *testing.T) {
	testNet = false
	ex := &Exchange{}
	ex.FuturesSettings.IsFutures = true
	s := &Stream{
		StandardStream: types.NewStandardStream(),
		exchange:       ex,
	}
	url := s.getUserDataStreamEndpointUrl("listenkey123")
	assert.Equal(t, FuturesPrivateWebSocketURL+"/ws?listenKey=listenkey123&events="+futuresPrivateStreamEvents, url)
}
