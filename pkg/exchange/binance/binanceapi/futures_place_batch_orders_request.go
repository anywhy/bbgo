package binanceapi

import (
	"encoding/json"
	"fmt"

	"github.com/c9s/requestgen"
)

//go:generate requestgen -method POST -url "/fapi/v1/batchOrders" -type FuturesPlaceBatchOrdersRequest -responseType .FuturesBatchOrdersResponse
type FuturesPlaceBatchOrdersRequest struct {
	client requestgen.AuthenticatedAPIClient

	orders []*FuturesPlaceOrderRequest `param:"orders"`
}

func (c *FuturesRestClient) NewFuturesPlaceBatchOrdersRequest() *FuturesPlaceBatchOrdersRequest {
	return &FuturesPlaceBatchOrdersRequest{client: c}
}

type FuturesBatchOrdersResponse struct {
	// List of orders which were placed successfully which can have a length between 0 and N
	Orders []FuturesOrderResponse `json:"orders"`

	// List of errors of length N, where each item corresponds to a nil value if
	// the order at that index
	Errors []error `json:"errors"`
}

func (f *FuturesBatchOrdersResponse) Unmarshal(data []byte) error {
	var rawMsgs []json.RawMessage
	if err := json.Unmarshal(data, &rawMsgs); err != nil {
		return err
	}

	f.Errors = make([]error, len(rawMsgs))
	f.Orders = make([]FuturesOrderResponse, len(rawMsgs))

	for i, msg := range rawMsgs {
		var apiErr struct {
			Code    int64  `json:"code"`
			Message string `json:"msg"`
		}
		if err := json.Unmarshal(msg, &apiErr); err == nil && (apiErr.Code != 0 || apiErr.Message != "") {
			f.Errors[i] = fmt.Errorf("<APIError> code=%d, msg=%s", apiErr.Code, apiErr.Message)
			continue
		}

		var order FuturesOrderResponse
		if err := json.Unmarshal(msg, &order); err != nil {
			return fmt.Errorf("unmarshal success order item[%d]: %w", i, err)
		}
		f.Orders = append(f.Orders, order)
	}

	return nil
}
