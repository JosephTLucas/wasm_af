package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

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
// for kv_put — backed by a NATS JetStream KV bucket "wasm-af-memory". The
// bucket handle is created once and captured in the closures. Register each
// provider under its own name in the HostFnRegistry.
func NewMemoryHostFnProviders(nc *nats.Conn, logger *slog.Logger) (getProvider, putProvider HostFnProvider) {
	var (
		kvOnce   sync.Once
		kvBucket natsjetstream.KeyValue
		kvInitOK bool
	)

	resolveKV := func(ctx context.Context) (natsjetstream.KeyValue, error) {
		kvOnce.Do(func() {
			var err error
			kvBucket, err = initMemoryBucket(ctx, nc)
			if err != nil {
				logger.Error("memory host fns: failed to init bucket", "err", err)
				return
			}
			kvInitOK = true
		})
		if kvInitOK {
			return kvBucket, nil
		}
		// First init failed; retry (sync.Once won't re-run, so do it directly).
		bucket, err := initMemoryBucket(ctx, nc)
		if err != nil {
			return nil, err
		}
		kvBucket = bucket
		kvInitOK = true
		return kvBucket, nil
	}

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

				resp := doKvGet(ctx, resolveKV, req.Key, logger)
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

				resp := doKvPut(ctx, resolveKV, req.Key, req.Value, logger)
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

func initMemoryBucket(ctx context.Context, nc *nats.Conn) (natsjetstream.KeyValue, error) {
	js, err := natsjetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return js.CreateOrUpdateKeyValue(ctx, natsjetstream.KeyValueConfig{
		Bucket:      "wasm-af-memory",
		Description: "wasm-af conversation memory",
	})
}

type kvResolver func(ctx context.Context) (natsjetstream.KeyValue, error)

// scopedKey prefixes a key with the agent type from the step context,
// enforcing per-agent-type namespace isolation in the shared KV bucket.
func scopedKey(ctx context.Context, key string) string {
	if meta, ok := stepMetaFrom(ctx); ok && meta.AgentType != "" {
		return meta.AgentType + ":" + key
	}
	return key
}

func doKvGet(ctx context.Context, resolve kvResolver, key string, logger *slog.Logger) kvGetResponse {
	if meta, ok := stepMetaFrom(ctx); ok {
		logger.Info("kv_get", "key", key,
			"task_id", meta.TaskID, "step_id", meta.StepID, "agent_type", meta.AgentType)
	}

	scopedK := scopedKey(ctx, key)
	bucket, err := resolve(ctx)
	if err != nil {
		logger.Error("kv_get: bucket", "err", err)
		return kvGetResponse{}
	}
	entry, err := bucket.Get(ctx, scopedK)
	if err != nil {
		if errors.Is(err, natsjetstream.ErrKeyNotFound) {
			return kvGetResponse{Found: false}
		}
		logger.Error("kv_get: get", "key", scopedK, "err", err)
		return kvGetResponse{}
	}
	return kvGetResponse{Value: string(entry.Value()), Found: true}
}

func doKvPut(ctx context.Context, resolve kvResolver, key, value string, logger *slog.Logger) kvPutResponse {
	if meta, ok := stepMetaFrom(ctx); ok {
		logger.Info("kv_put", "key", key,
			"task_id", meta.TaskID, "step_id", meta.StepID, "agent_type", meta.AgentType)
	}

	scopedK := scopedKey(ctx, key)
	bucket, err := resolve(ctx)
	if err != nil {
		logger.Error("kv_put: bucket", "err", err)
		return kvPutResponse{}
	}
	if _, err := bucket.Put(ctx, scopedK, []byte(value)); err != nil {
		logger.Error("kv_put: put", "key", scopedK, "err", err)
		return kvPutResponse{}
	}
	return kvPutResponse{Success: true}
}
