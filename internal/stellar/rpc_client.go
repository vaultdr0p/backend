package stellar

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	client "github.com/stellar/go-stellar-sdk/clients/rpcclient"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
)

type RPCClientWrapper struct {
	client *client.Client
}

func NewRPCClientWrapper(rpcURL string, httpClient *http.Client) *RPCClientWrapper {
	innerClient := client.NewClient(rpcURL, httpClient)
	return &RPCClientWrapper{client: innerClient}
}

func (w *RPCClientWrapper) SimulateTransaction(ctx context.Context, request protocol.SimulateTransactionRequest) (*SimulationResult, *SimulationError) {
	if w.client == nil {
		return nil, NewSimulationError(
			errors.New("RPC client not initialized"),
			nil,
		)
	}

	resp, err := w.client.SimulateTransaction(ctx, request)
	if err != nil {
		return nil, NewSimulationError(
			err,
			nil,
		)
	}

	if resp.Error != "" {
		return nil, NewSimulationError(
			errors.New(resp.Error),
			&resp,
		)
	}

	return &SimulationResult{
		Response: resp,
	}, nil
}

func (w *RPCClientWrapper) GetLatestLedgerSequence(ctx context.Context) (uint32, error) {
	if w.client == nil {
		return 0, errors.New("RPC client not initialized")
	}

	resp, err := w.client.GetLatestLedger(ctx)
	if err != nil {
		return 0, fmt.Errorf("getting latest ledger sequence: %w", err)
	}

	return resp.Sequence, nil
}

// GetEvents retrieves contract events from RPC.
//
// Errors are wrapped rather than categorised the way SimulateTransaction's are:
// a getEvents failure is always transport- or range-level (an unreachable
// server, or a startLedger outside the retention window), never a contract
// execution failure, so there is no simulation-style taxonomy to apply.
func (w *RPCClientWrapper) GetEvents(ctx context.Context, request protocol.GetEventsRequest) (*protocol.GetEventsResponse, error) {
	if w.client == nil {
		return nil, errors.New("RPC client not initialized")
	}

	resp, err := w.client.GetEvents(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("getting contract events: %w", err)
	}

	return &resp, nil
}

func NewHTTPClientWithAuth(authHeaderKey, authHeaderValue string) (*http.Client, error) {
	if authHeaderKey == "" && authHeaderValue == "" {
		return http.DefaultClient, nil
	}

	if authHeaderKey == "" || authHeaderValue == "" {
		return nil, fmt.Errorf("both authHeaderKey and authHeaderValue must be provided or both must be empty")
	}

	transport := &headerTransport{
		base:  http.DefaultTransport,
		key:   authHeaderKey,
		value: authHeaderValue,
	}

	return &http.Client{
		Transport: transport,
	}, nil
}

type headerTransport struct {
	base  http.RoundTripper
	key   string
	value string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Add(t.key, t.value)
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("error in RoundTrip: %w", err)
	}
	return res, nil
}

var _ RPCClient = (*RPCClientWrapper)(nil)
