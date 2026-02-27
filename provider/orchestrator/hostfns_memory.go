package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	nats "github.com/nats-io/nats.go"
	natsjetstream "github.com/nats-io/nats.go/jetstream"

	extism "github.com/extism/go-sdk"
)

type kvGetRequest struct {
	Key string `json:"key"`
}

type kvGetResponse struct {
	Value string `json:"value"`
	Found bool   `json:"found"`
}

type kvPutRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type kvPutResponse struct {
	Success bool `json:"success"`
}

// NewMemoryHostFnProviders returns two HostFnProviders — one for kv_get and one
// for kv_put — backed by a NATS JetStream KV bucket "wasm-af-memory". Register
// each under its own name in the HostFnRegistry.
func NewMemoryHostFnProviders(nc *nats.Conn, logger *slog.Logger) (getProvider, putProvider HostFnProvider) {
	getProvider = func(_ *Orchestrator) []extism.HostFunction {
		fn := extism.NewHostFunctionWithStack(
			"kv_get",
			func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
				inputBytes, err := p.ReadBytes(stack[0])
				if err != nil {
					logger.Error("kv_get: read input", "err", err)
					stack[0] = 0
					return
				}

				var req kvGetRequest
				if err := json.Unmarshal(inputBytes, &req); err != nil {
					logger.Error("kv_get: unmarshal", "err", err)
					stack[0] = 0
					return
				}

				resp := doKvGet(ctx, nc, req.Key, logger)
				outputBytes, _ := json.Marshal(resp)
				offset, err := p.WriteBytes(outputBytes)
				if err != nil {
					logger.Error("kv_get: write output", "err", err)
					stack[0] = 0
					return
				}
				stack[0] = offset
			},
			[]extism.ValueType{extism.ValueTypePTR},
			[]extism.ValueType{extism.ValueTypePTR},
		)
		fn.SetNamespace("extism:host/user")
		return []extism.HostFunction{fn}
	}

	putProvider = func(_ *Orchestrator) []extism.HostFunction {
		fn := extism.NewHostFunctionWithStack(
			"kv_put",
			func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
				inputBytes, err := p.ReadBytes(stack[0])
				if err != nil {
					logger.Error("kv_put: read input", "err", err)
					stack[0] = 0
					return
				}

				var req kvPutRequest
				if err := json.Unmarshal(inputBytes, &req); err != nil {
					logger.Error("kv_put: unmarshal", "err", err)
					stack[0] = 0
					return
				}

				resp := doKvPut(ctx, nc, req.Key, req.Value, logger)
				outputBytes, _ := json.Marshal(resp)
				offset, err := p.WriteBytes(outputBytes)
				if err != nil {
					logger.Error("kv_put: write output", "err", err)
					stack[0] = 0
					return
				}
				stack[0] = offset
			},
			[]extism.ValueType{extism.ValueTypePTR},
			[]extism.ValueType{extism.ValueTypePTR},
		)
		fn.SetNamespace("extism:host/user")
		return []extism.HostFunction{fn}
	}

	return getProvider, putProvider
}

func memoryBucket(ctx context.Context, nc *nats.Conn) (natsjetstream.KeyValue, error) {
	js, err := natsjetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return js.CreateOrUpdateKeyValue(ctx, natsjetstream.KeyValueConfig{
		Bucket:      "wasm-af-memory",
		Description: "wasm-af conversation memory",
	})
}

func doKvGet(ctx context.Context, nc *nats.Conn, key string, logger *slog.Logger) kvGetResponse {
	kv, err := memoryBucket(ctx, nc)
	if err != nil {
		logger.Error("kv_get: bucket", "err", err)
		return kvGetResponse{}
	}
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, natsjetstream.ErrKeyNotFound) {
			return kvGetResponse{Found: false}
		}
		logger.Error("kv_get: get", "key", key, "err", err)
		return kvGetResponse{}
	}
	return kvGetResponse{Value: string(entry.Value()), Found: true}
}

func doKvPut(ctx context.Context, nc *nats.Conn, key, value string, logger *slog.Logger) kvPutResponse {
	kv, err := memoryBucket(ctx, nc)
	if err != nil {
		logger.Error("kv_put: bucket", "err", err)
		return kvPutResponse{}
	}
	if _, err := kv.Put(ctx, key, []byte(value)); err != nil {
		logger.Error("kv_put: put", "key", key, "err", err)
		return kvPutResponse{}
	}
	return kvPutResponse{Success: true}
}
