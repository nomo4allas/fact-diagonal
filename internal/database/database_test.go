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

// TestPersistSinCUFE verifica el caso de borde de CUFE vacío y sin datos para el
// match manual: no se toca la BD (dms/adj nil) y se reporta EstadoNoHallado
// (correo → Pendientes), sin ejecutar consulta alguna.
func TestPersistSinCUFE(t *testing.T) {
	log := &capturaLog{}
	c := &Client{log: log, simulation: true} // dms/adj nil: no deben usarse

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

// TestNormalizeNITNumeric cubre la normalización del NIT del XML al entero base
// que almacena la BD (sin puntos ni dígito de verificación).
func TestNormalizeNITNumeric(t *testing.T) {
	casos := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"900.123.456-7", 900123456, true},
		{"901791583", 901791583, true},
		{" 78031842 ", 78031842, true},
		{"78024287-1", 78024287, true},
		{"", 0, false},
		{"N/A", 0, false},
	}
	for _, k := range casos {
		got, ok := normalizeNITNumeric(k.in)
		if got != k.want || ok != k.ok {
			t.Errorf("normalizeNITNumeric(%q) = (%d,%t), want (%d,%t)", k.in, got, ok, k.want, k.ok)
		}
	}
}

// TestSplitNumero cubre la separación consecutivo/prefijo usada en el match manual.
func TestSplitNumero(t *testing.T) {
	casos := []struct{ numero, prefijo, wantNum, wantPref string }{
		{"FE470", "FE", "470", "FE"},
		{"470", "", "470", ""},
		{"SETP980", "SETP", "980", "SETP"},
	}
	for _, k := range casos {
		gotNum, gotPref := splitNumero(k.numero, k.prefijo)
		if gotNum != k.wantNum || gotPref != k.wantPref {
			t.Errorf("splitNumero(%q,%q) = (%q,%q), want (%q,%q)", k.numero, k.prefijo, gotNum, gotPref, k.wantNum, k.wantPref)
		}
	}
}
