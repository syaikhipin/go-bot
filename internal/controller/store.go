package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
	"go-harness/internal/modelsettings"
	"go-harness/internal/providersettings"
)

var ErrAccountNotFound = errors.New("account not found")
var ErrAccountExists = errors.New("account already exists")

type Account struct {
	DesiredState    string                     `json:"desiredState"`
	HostID          string                     `json:"hostId,omitempty"`
	HostToken       string                     `json:"hostToken,omitempty"`
	HostEndpoint    string                     `json:"hostEndpoint,omitempty"`
	WebSearch       *providersettings.Settings `json:"webSearch,omitempty"`
	ImageGeneration *providersettings.Settings `json:"imageGeneration,omitempty"`
}

type AccountStore interface {
	Create(context.Context, string, Account) error
	Get(context.Context, string) (Account, error)
	Save(context.Context, string, Account) error // Save only Host lifecycle fields.
	SaveProviderSettings(context.Context, string, string, providersettings.Settings) error
	LoadModels(context.Context, string) (modelsettings.Snapshot, error)
	SaveModelCatalog(context.Context, string, modelsettings.Catalog) (int64, error)
	SaveSessionModel(context.Context, string, string, modelsettings.Selection) (int64, error)
}

const (
	accountPrefix        = "user:"
	webSearchField       = "webSearch"
	imageGenerationField = "imageGeneration"
)

type RedisStore struct{ client *redis.Client }

func OpenRedis(ctx context.Context, address string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{Addr: address, Protocol: 2})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &RedisStore{client: client}, nil
}

func (s *RedisStore) Close() error { return s.client.Close() }

func (a Account) hostFields() []any {
	return []any{"desiredState", a.DesiredState, "hostId", a.HostID, "hostToken", a.HostToken, "hostEndpoint", a.HostEndpoint}
}

func (a Account) fields() ([]any, error) {
	fields := a.hostFields()
	for _, item := range []struct {
		name     string
		settings *providersettings.Settings
	}{{webSearchField, a.WebSearch}, {imageGenerationField, a.ImageGeneration}} {
		if item.settings != nil {
			data, err := json.Marshal(item.settings)
			if err != nil {
				return nil, err
			}
			fields = append(fields, item.name, data)
		}
	}
	return fields, nil
}

func (s *RedisStore) Create(ctx context.Context, id string, record Account) error {
	fields, err := record.fields()
	if err != nil {
		return err
	}
	key := accountPrefix + id
	return s.watch(ctx, []string{key}, func(tx *redis.Tx) error {
		exists, err := tx.Exists(ctx, key).Result()
		if err != nil {
			return err
		}
		if exists != 0 {
			return ErrAccountExists
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, key, fields...)
			return nil
		})
		return err
	})
}

func (s *RedisStore) Get(ctx context.Context, id string) (Account, error) {
	fields, err := s.client.HMGet(ctx, accountPrefix+id, "desiredState", "hostId", "hostToken", "hostEndpoint", webSearchField, imageGenerationField).Result()
	if err != nil {
		return Account{}, err
	}
	if fields[0] == nil {
		return Account{}, ErrAccountNotFound
	}
	record := Account{
		DesiredState: redisString(fields[0]), HostID: redisString(fields[1]),
		HostToken: redisString(fields[2]), HostEndpoint: redisString(fields[3]),
	}
	if !decodeField(fields[4], &record.WebSearch) || !decodeField(fields[5], &record.ImageGeneration) {
		return Account{}, errors.New("invalid Redis provider settings")
	}
	return record, nil
}

func (s *RedisStore) Save(ctx context.Context, id string, record Account) error {
	return s.client.HSet(ctx, accountPrefix+id, record.hostFields()...).Err()
}

func (s *RedisStore) SaveProviderSettings(ctx context.Context, id, field string, settings providersettings.Settings) error {
	if field != webSearchField && field != imageGenerationField {
		return errors.New("invalid provider settings field")
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return s.client.HSet(ctx, accountPrefix+id, field, data).Err()
}

func redisString(value any) string {
	text, _ := value.(string)
	return text
}

func decodeField(value any, output any) bool {
	return value == nil || json.Unmarshal([]byte(redisString(value)), output) == nil
}

func (s *RedisStore) watch(ctx context.Context, keys []string, change func(*redis.Tx) error) error {
	for attempt := 0; attempt < 16; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.client.Watch(ctx, change, keys...)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return fmt.Errorf("Redis account changed too often: %w", redis.TxFailedErr)
}
