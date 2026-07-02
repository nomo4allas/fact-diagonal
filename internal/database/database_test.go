package database

import (
	"context"
	"database/sql"
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

// TestPersistSinCUFE verifica el caso de borde de CUFE vacío: no se toca la BD
// (dms/adj nil) y se reporta EstadoNoHallado (correo → Pendientes), sin ejecutar
// consulta alguna. Sin CUFE no hay forma de ubicar el registro automático.
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

// TestSetsUpdateRadicado verifica la regla: Mandato/Explicacion solo entran al
// UPDATE cuando el valor extraído no viene vacío; si viene vacío (o solo
// espacios) se omiten del SET para no pisar lo que ya haya en la BD.
func TestSetsUpdateRadicado(t *testing.T) {
	fecha := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	casos := []struct {
		nombre       string
		data         invoice.Data
		wantMandato  bool
		wantExplicac bool
	}{
		{"ambos presentes", invoice.Data{Pedido: "001", Declarac: "IM123"}, true, true},
		{"ambos vacíos", invoice.Data{}, false, false},
		{"solo Pedido", invoice.Data{Pedido: "001"}, true, false},
		{"solo DECLARAC", invoice.Data{Declarac: "IM123"}, false, true},
		{"solo espacios", invoice.Data{Pedido: "   ", Declarac: "\t"}, false, false},
	}
	for _, k := range casos {
		sets, args := setsUpdateRadicado(k.data, fecha, 42)
		joined := strings.Join(sets, ", ")

		// Los 4 campos de recepción van siempre.
		for _, base := range []string{"ViaDeRecepcion", "FechaHoraOriginal", "Usuario", "Pc"} {
			if !strings.Contains(joined, base) {
				t.Errorf("%s: falta el campo de recepción %q en SET=%q", k.nombre, base, joined)
			}
		}
		if got := strings.Contains(joined, "Mandato ="); got != k.wantMandato {
			t.Errorf("%s: Mandato presente=%t, want %t (SET=%q)", k.nombre, got, k.wantMandato, joined)
		}
		if got := strings.Contains(joined, "Explicacion ="); got != k.wantExplicac {
			t.Errorf("%s: Explicacion presente=%t, want %t (SET=%q)", k.nombre, got, k.wantExplicac, joined)
		}
		// El arg @iddoc debe existir siempre; @mandato/@explicacion solo si aplica.
		if !tieneNamed(args, "iddoc") {
			t.Errorf("%s: falta el arg @iddoc", k.nombre)
		}
		if got := tieneNamed(args, "mandato"); got != k.wantMandato {
			t.Errorf("%s: arg @mandato presente=%t, want %t", k.nombre, got, k.wantMandato)
		}
		if got := tieneNamed(args, "explicacion"); got != k.wantExplicac {
			t.Errorf("%s: arg @explicacion presente=%t, want %t", k.nombre, got, k.wantExplicac)
		}
	}
}

// TestNotasAdjunto verifica la regla del BL en el INSERT de Adjuntos: valor si el
// BL tiene contenido, nil (→ NULL) si viene vacío o solo espacios.
func TestNotasAdjunto(t *testing.T) {
	casos := []struct {
		bl      string
		wantSQL any
		wantLog string
	}{
		{"BL123", "BL123", "'BL123'"},
		{"  BL456 ", "BL456", "'BL456'"},
		{"", nil, "NULL"},
		{"   ", nil, "NULL"},
	}
	for _, k := range casos {
		if got := notasAdjuntoSQL(k.bl); got != k.wantSQL {
			t.Errorf("notasAdjuntoSQL(%q) = %v, want %v", k.bl, got, k.wantSQL)
		}
		if got := notasAdjuntoLog(k.bl); got != k.wantLog {
			t.Errorf("notasAdjuntoLog(%q) = %q, want %q", k.bl, got, k.wantLog)
		}
	}
}

// tieneNamed indica si entre los args hay un sql.Named con el nombre dado.
func tieneNamed(args []any, name string) bool {
	for _, a := range args {
		if n, ok := a.(sql.NamedArg); ok && n.Name == name {
			return true
		}
	}
	return false
}
