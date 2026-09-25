package hub

import (
	"context"
	"errors"
	"log/slog"
)

// Заглушки контракта: тела пишутся на этапе реестра.

var errTODO = errors.New("hub: не реализовано")

// Hub — реестр серверов. Реализует Router. Безопасен для одновременных
// вызовов.
type Hub struct {
	cfg Config
	log *slog.Logger
}

// DefaultConfig — встроенная конфигурация (default.json): sources, daemon,
// notes.
func DefaultConfig() Config { return Config{} }

// LoadConfig читает конфигурацию из файла; пустой путь — встроенная.
// Подставляет ${DATA} и ${DAEMON} и проверяет её (Validate).
func LoadConfig(path string, d Defaults) (Config, error) { return Config{}, errTODO }

// Validate — имена серверов уникальны и непусты, транспорт известен, у
// stdio нет URL, у http есть URL, одно имя инструмента не выдано двум
// серверам. Ошибка оборачивает ErrConfig.
func (c Config) Validate() error { return errTODO }

// Open собирает реестр по конфигурации; не подключается (это Connect или
// первый Tools/Routes).
func Open(cfg Config, log *slog.Logger) (*Hub, error) { return nil, errTODO }

// Close закрывает соединения и останавливает stdio-процессы.
func (h *Hub) Close() {}

func (h *Hub) Servers(ctx context.Context) []ServerView       { return nil }
func (h *Hub) Connect(ctx context.Context) error              { return errTODO }
func (h *Hub) Routes(ctx context.Context) ([]Route, error)    { return nil, errTODO }
func (h *Hub) Tools(ctx context.Context) ([]Bound, error)     { return nil, errTODO }
func (h *Hub) Snapshot(ctx context.Context) (Snapshot, error) { return nil, errTODO }
