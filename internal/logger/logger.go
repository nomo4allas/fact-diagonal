// Package logger configura un logging básico que escribe simultáneamente
// a consola y a un archivo dentro del directorio /logs.
package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Logger envuelve el log estándar y mantiene abierto el archivo de log
// para poder cerrarlo ordenadamente al finalizar.
type Logger struct {
	*log.Logger
	file *os.File
}

// New crea un Logger que escribe en consola y en logs/fact-diagonal-YYYY-MM-DD.log.
// El directorio se crea si no existe.
func New(dir string) (*Logger, error) {
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("no se pudo crear el directorio de logs %q: %w", dir, err)
	}

	name := fmt.Sprintf("fact-diagonal-%s.log", time.Now().Format("2006-01-02"))
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("no se pudo abrir el archivo de log %q: %w", path, err)
	}

	out := io.MultiWriter(os.Stdout, f)
	std := log.New(out, "", log.LstdFlags)

	return &Logger{Logger: std, file: f}, nil
}

// Infof registra un mensaje informativo.
func (l *Logger) Infof(format string, args ...any) {
	l.Printf("[INFO] "+format, args...)
}

// Errorf registra un mensaje de error.
func (l *Logger) Errorf(format string, args ...any) {
	l.Printf("[ERROR] "+format, args...)
}

// Close cierra el archivo de log subyacente.
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
