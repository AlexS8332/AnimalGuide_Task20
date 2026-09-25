package main

import (
	"fmt"
	"log/slog"
	"os"
)

// runNotes — stdio-сервер «блокнот натуралиста» (-role notes): инструменты
// internal/notes над каталогом <data>/notes. Сеть не нужна, модель тоже.
// Возвращает код выхода.
func runNotes(dataDir string, log *slog.Logger) int {
	_, _ = dataDir, log
	fmt.Fprintln(os.Stderr, "блокнот: ещё не реализован")
	return 1
}
