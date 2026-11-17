package controller

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/redis/go-redis/v9"
	"go-bot/internal/modelsettings"
)

const (
	llmField           = "llm"
	sessionModelsField = "sessionModels"
	modelRevisionField = "modelRevision"
)

func emptyModelSnapshot() modelsettings.Snapshot {
	return modelsettings.Snapshot{
		Catalog:  modelsettings.Catalog{Endpoints: make(map[string]modelsettings.Endpoint)},
		Sessions: make(map[string]modelsettings.Selection),
	}
}

func (s *RedisStore) LoadModels(ctx context.Context, id string) (modelsettings.Snapshot, error) {
	state := emptyModelSnapshot()
	values, err := s.client.HMGet(ctx, accountPrefix+id, "desiredState", llmField, sessionModelsField, modelRevisionField).Result()
	if err != nil {
		return state, err
	}
	if values[0] == nil {
		return state, ErrAccountNotFound
	}
	if !decodeField(values[1], &state.Catalog) || !decodeField(values[2], &state.Sessions) {
		return emptyModelSnapshot(), errors.New("invalid Redis model settings")
	}
	if state.Catalog.Endpoints == nil {
		state.Catalog.Endpoints = make(map[string]modelsettings.Endpoint)
	}
	if state.Sessions == nil {
		state.Sessions = make(map[string]modelsettings.Selection)
	}
	state.Revision, err = modelRevision(values[3])
	if err != nil {
		return emptyModelSnapshot(), err
	}
	return state, nil
}

func (s *RedisStore) SaveModelCatalog(ctx context.Context, id string, catalog modelsettings.Catalog) (int64, error) {
	return s.saveModels(ctx, id, llmField, func(any) (any, error) { return catalog, nil })
}

func (s *RedisStore) SaveSessionModel(ctx context.Context, id, sessionID string, choice modelsettings.Selection) (int64, error) {
	return s.saveModels(ctx, id, sessionModelsField, func(raw any) (any, error) {
		var sessions map[string]modelsettings.Selection
		if !decodeField(raw, &sessions) {
			return nil, errors.New("invalid Redis session models")
		}
		if sessions == nil {
			sessions = make(map[string]modelsettings.Selection)
		}
		sessions[sessionID] = choice
		return sessions, nil
	})
}

func (s *RedisStore) saveModels(ctx context.Context, id, field string, update func(any) (any, error)) (int64, error) {
	key := accountPrefix + id
	var revision int64
	err := s.watch(ctx, []string{key}, func(tx *redis.Tx) error {
		values, err := tx.HMGet(ctx, key, "desiredState", field, modelRevisionField).Result()
		if err != nil {
			return err
		}
		if values[0] == nil {
			return ErrAccountNotFound
		}
		current, err := modelRevision(values[2])
		if err != nil {
			return err
		}
		if current == math.MaxInt64 {
			return errors.New("Redis model revision exhausted")
		}
		value, err := update(values[1])
		if err != nil {
			return err
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		revision = current + 1
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, key, field, data, modelRevisionField, revision)
			return nil
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	return revision, nil
}

func modelRevision(raw any) (int64, error) {
	if raw == nil {
		return 0, nil
	}
	revision, err := strconv.ParseInt(redisString(raw), 10, 64)
	if err != nil || revision < 0 {
		return 0, errors.New("invalid Redis model revision")
	}
	return revision, nil
}
