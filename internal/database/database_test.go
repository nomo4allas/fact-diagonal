package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// capturaLog es un Logger de prueba que acumula los mensajes.
type capturaLog struct{ lines []string }

func (l *capturaLog) Infof(f string, a ...any)  { l.lines = append(l.lines, "INFO "+fmt.Sprintf(f, a...)) }
func (l *capturaLog) Errorf(f string, a ...any) { l.lines = append(l.lines, "ERR "+fmt.Sprintf(f, a...)) }
func (l *capturaLog) joined() string            { return strings.Join(l.lines, "\n") }

func TestDSN(t *testing.T) {
	cfg := Config{Server: "localhost", Port: "1433", User: "sa", Password: "p@ss w/ord", NameDMS: "DMSDiagonal"}
	got := cfg.dsn(cfg.NameDMS)

	for _, want := range []string{"sqlserver://", "sa:", "@localhost:1433", "database=DMSDiagonal", "encrypt=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN %q no contiene %q", got, want)
		}
	}
}

func TestDSNPuertoPorDefecto(t *testing.T) {
	cfg := Config{Server: "db", User: "sa", NameDMS: "X"}
	if got := cfg.dsn("X"); !strings.Contains(got, "@db:1433") {
		t.Errorf("se esperaba puerto por defecto 1433, DSN=%q", got)
	}
}

// TestPersistSinCUFE verifica el caso de borde de CUFE vacío: no se toca la BD
// (dms/adj nil) y se registra el mensaje correspondiente.
func TestPersistSinCUFE(t *testing.T) {
	log := &capturaLog{}
	c := &Client{log: log, simulation: true} // dms/adj nil: no deben usarse

	err := c.PersistInvoice(context.Background(), invoice.Data{CUFE: "   "}, time.Now(), nil)
	if err != nil {
		t.Fatalf("se esperaba nil, got %v", err)
	}
	if !strings.Contains(log.joined(), "sin CUFE") {
		t.Errorf("no se registró el caso 'sin CUFE'; log:\n%s", log.joined())
	}
}
