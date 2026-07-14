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

// TestAdjuntoYaRegistrado verifica que el mensaje "ya registrado" del SP se
// detecte como éxito idempotente, sin importar mayúsculas/minúsculas.
func TestAdjuntoYaRegistrado(t *testing.T) {
	casos := []struct {
		mensaje string
		want    bool
	}{
		{"Adjunto ya registrado.", true}, // mensaje exacto del SP de producción
		{"El adjunto ya registrado", true},
		{"YA REGISTRADO en el sistema", true},
		{"Documento Ya Registrado previamente", true},
		{"Insertado correctamente", false},
		{"", false},
		{"registrado", false},
	}
	for _, k := range casos {
		if got := adjuntoYaRegistrado(k.mensaje); got != k.want {
			t.Errorf("adjuntoYaRegistrado(%q) = %v, want %v", k.mensaje, got, k.want)
		}
	}
}

// TestAdjuntoPersistido verifica la regla de éxito/fallo de la Operacion 2 con el
// SP de producción actualizado: "ya registrado" es éxito para cualquier valor de
// @Resultado (antes venía con 0, ahora con el IdAdjunto>0); el único fallo real
// es @Resultado=0 sin ese mensaje.
func TestAdjuntoPersistido(t *testing.T) {
	casos := []struct {
		nombre    string
		resultado int
		mensaje   string
		want      bool
	}{
		{"inserción nueva (IdAdjunto>0)", 1234, "Adjunto insertado.", true},
		{"ya registrado con IdAdjunto>0 (SP nuevo)", 1234, "Adjunto ya registrado.", true},
		{"ya registrado con Resultado=0 (SP antiguo)", 0, "Adjunto ya registrado.", true},
		{"fallo real: Resultado=0 sin 'ya registrado'", 0, "No existe el radicado.", false},
		{"fallo real: Resultado=0 y mensaje vacío", 0, "", false},
	}
	for _, k := range casos {
		if got := adjuntoPersistido(k.resultado, k.mensaje); got != k.want {
			t.Errorf("%s: adjuntoPersistido(%d,%q) = %v, want %v", k.nombre, k.resultado, k.mensaje, got, k.want)
		}
	}
}

// TestNotasParaAdjunto verifica @NotasAdjunto: el número de documento (recortado)
// para cualquier adjunto, o NULL cuando no está disponible.
func TestNotasParaAdjunto(t *testing.T) {
	deref := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	casos := []struct {
		numDocumento, want string
	}{
		{"FE470", "FE470"},
		{"  FE471 ", "FE471"},
		{"", "<nil>"},
		{"   ", "<nil>"},
	}
	for _, k := range casos {
		if got := deref(notasParaAdjunto(k.numDocumento)); got != k.want {
			t.Errorf("notasParaAdjunto(%q) = %q, want %q", k.numDocumento, got, k.want)
		}
	}
}
