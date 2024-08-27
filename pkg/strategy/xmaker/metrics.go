package xmaker

import "github.com/prometheus/client_golang/prometheus"

var openOrderBidExposureInUsdMetrics = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "xmaker_open_order_bid_exposure_in_usd",
		Help: "",
	}, []string{"strategy_type", "strategy_id", "exchange", "symbol"})

var openOrderAskExposureInUsdMetrics = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "xmaker_open_order_ask_exposure_in_usd",
		Help: "",
	}, []string{"strategy_type", "strategy_id", "exchange", "symbol"})

var makerBestBidPriceMetrics = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "xmaker_maker_best_bid_price",
		Help: "",
	}, []string{"strategy_type", "strategy_id", "exchange", "symbol"})

var makerBestAskPriceMetrics = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "xmaker_maker_best_ask_price",
		Help: "",
	}, []string{"strategy_type", "strategy_id", "exchange", "symbol"})

var configNumOfLayersMetrics = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "xmaker_config_num_of_layers",
		Help: "",
	}, []string{"strategy_type", "strategy_id", "symbol"})

var configMaxExposureMetrics = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "xmaker_config_max_exposure",
		Help: "",
	}, []string{"strategy_type", "strategy_id", "symbol"})

func init() {
	prometheus.MustRegister(
		openOrderBidExposureInUsdMetrics,
		openOrderAskExposureInUsdMetrics,
		makerBestBidPriceMetrics,
		makerBestAskPriceMetrics,
		configNumOfLayersMetrics,
		configMaxExposureMetrics,
	)
}
