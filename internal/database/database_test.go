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

func (l *capturaLog) Infof(f string, a ...any) {
	l.lines = append(l.lines, "INFO "+fmt.Sprintf(f, a...))
}
func (l *capturaLog) Errorf(f string, a ...any) {
	l.lines = append(l.lines, "ERR "+fmt.Sprintf(f, a...))
}
func (l *capturaLog) joined() string { return strings.Join(l.lines, "\n") }

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

// TestPersistSinCUFE verifica el caso de borde de CUFE vacío: sin CUFE no se
// puede invocar la Operacion 0, así que no se llama al SP (db nil no debe usarse)
// y se reporta EstadoNoHallado (correo → Pendientes).
func TestPersistSinCUFE(t *testing.T) {
	log := &capturaLog{}
	c := &Client{log: log, simulation: true} // db nil: no debe usarse

	res, err := c.PersistInvoice(context.Background(), invoice.Data{CUFE: "   "}, time.Now(), nil)
	if err != nil {
		t.Fatalf("se esperaba nil, got %v", err)
	}
	if res.Estado != EstadoNoHallado {
		t.Errorf("Estado = %v, want EstadoNoHallado", res.Estado)
	}
	if !strings.Contains(log.joined(), "sin CUFE") {
		t.Errorf("no se registró el caso 'sin CUFE'; log:\n%s", log.joined())
	}
}

// TestTruncar verifica el recorte por runas a los límites varchar del SP.
func TestTruncar(t *testing.T) {
	casos := []struct {
		in   string
		max  int
		want string
	}{
		{"001", maxMandato, "001"},
		{"1234567", maxMandato, "123456"},
		{"café", 3, "caf"},
		{"", maxNotasAdjunto, ""},
	}
	for _, k := range casos {
		if got := truncar(k.in, k.max); got != k.want {
			t.Errorf("truncar(%q,%d) = %q, want %q", k.in, k.max, got, k.want)
		}
	}
}

// TestNotasParaAdjunto verifica @NotasAdjunto: BL para el PDF (o NULL si vacío) y
// siempre NULL para el XML.
func TestNotasParaAdjunto(t *testing.T) {
	deref := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	casos := []struct {
		ext, bl, want string
	}{
		{"pdf", "BL123", "BL123"},
		{"pdf", "  BL456 ", "BL456"},
		{"pdf", "", "<nil>"},
		{"pdf", "   ", "<nil>"},
		{"PDF", "BL789", "BL789"},
		{"xml", "BL123", "<nil>"},
	}
	for _, k := range casos {
		if got := deref(notasParaAdjunto(k.ext, k.bl)); got != k.want {
			t.Errorf("notasParaAdjunto(%q,%q) = %q, want %q", k.ext, k.bl, got, k.want)
		}
	}
}
